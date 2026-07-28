package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	h3qlog "github.com/quic-go/quic-go/http3/qlog"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/http2"
)

// DoH endpoints, tried in order. These are the addresses warp-svc compiles in as
// its consumer defaults (a 4-entry IpAddr blob read out of dns_proxy) — not the
// public 1.1.1.1/1.0.0.1 resolvers, which the official client never uses for its
// own DoH. The daemon also lets CF_WARP_DNS_IP and the policy's doh_ips field
// override them; here they are fixed because we always tunnel to the same anycast
// front doors.
var dnsServers = []string{
	"162.159.36.1:443",
	"162.159.46.1:443",
}

const (
	// dohServerName is the TLS SNI / :authority for DoH. It is never resolved —
	// the connection is made to one of dnsServers above.
	dohServerName  = "cloudflare-dns.com"
	dohURL         = "https://" + dohServerName + "/dns-query"
	dohContentType = "application/dns-message" // RFC 8484 wire format

	dohHandshakeTimeout = 10 * time.Second
	dohQueryTimeout     = 5 * time.Second
	dohMaxResponseSize  = 64 << 10

	// TTL bounds for the resolution cache. The floor keeps very short TTLs from
	// making the cache useless; the ceiling keeps us from pinning a stale address.
	dohMinTTL = 5 * time.Second
	dohMaxTTL = 5 * time.Minute

	// Cache sizing. Sweeping starts once the map is large enough that the scan
	// cost is worth paying; the cap bounds memory even if every entry is live.
	dnsCacheSweepAt    = 1024
	dnsCacheMaxEntries = 8192
)

// streamConn wraps http3.RequestStream to implement net.Conn for TLS
type streamConn struct {
	*http3.RequestStream
	localAddr  net.Addr
	remoteAddr net.Addr
}

func (s *streamConn) LocalAddr() net.Addr  { return s.localAddr }
func (s *streamConn) RemoteAddr() net.Addr { return s.remoteAddr }

// connectionIDLength matches warp-svc: tokio_quiche's SimpleConnectionIdGenerator
// emits 20-byte source connection IDs. With quic-go's 4-byte default the WARP
// edge intermittently answers with PROTOCOL_VIOLATION and drops the connection.
const connectionIDLength = 20

// perAddrDialTimeout bounds a single edge address attempt so an unreachable
// port falls through to the next candidate quickly.
const perAddrDialTimeout = 8 * time.Second

// relayDrainGrace bounds how long a tunnel waits for the response direction
// after the client has half-closed. Generous enough for the legitimate
// send-then-read pattern, short enough that a stream abandoned by a dead client
// is returned to the edge's concurrent-stream grant promptly.
const relayDrainGrace = 30 * time.Second

const (
	// connectExchangeTimeout bounds each H3 CONNECT attempt. A second attempt is
	// made on a fresh QUIC connection when the first one exposes a dead path, so
	// this must leave enough of socksSetupTimeout for redial and retry.
	connectExchangeTimeout = 10 * time.Second
	socksSetupTimeout      = 35 * time.Second
	streamOpenTimeout      = 10 * time.Second
	connectFailureWindow   = 30 * time.Second
	connectFailureTargets  = 3
	reconnectRetryInitial  = 100 * time.Millisecond
	reconnectRetryMax      = 5 * time.Second
)

// connBundle groups everything owned by a single QUIC connection attempt so the
// whole set can be torn down together on reconnect.
type connBundle struct {
	udpConn   *net.UDPConn
	qtr       *quic.Transport
	quicConn  *quic.Conn
	h3Client  *http3.ClientConn
	h3Trans   *http3.Transport
	closeOnce sync.Once
	healthMu  sync.Mutex

	// A live QUIC path can coexist with a wedged H3 session. Track distinct
	// targets that timed out in one short window so a single unreachable target
	// doesn't cause collateral reconnects, while session-wide failures still do.
	failureSince   time.Time
	failureTargets map[string]struct{}
}

// close abortively tears down the bundle. Safe on a nil receiver, concurrent
// calls, and partially-constructed bundles.
func (b *connBundle) close(reason string) {
	if b == nil {
		return
	}
	b.closeOnce.Do(func() {
		// Close the socket and Transport before touching higher layers. This is
		// intentionally an abortive close: quic.Conn.CloseWithError waits for the
		// connection run loop, which is exactly the component that may be wedged
		// when a path has gone black. Transport.Close destroys its connections and
		// guarantees all stream reads and writes are unblocked.
		if b.udpConn != nil {
			_ = b.udpConn.Close()
		}
		if b.qtr != nil {
			_ = b.qtr.Close()
		} else if b.quicConn != nil {
			_ = b.quicConn.CloseWithError(0, reason)
		}
		if b.h3Trans != nil {
			_ = b.h3Trans.Close()
		}
	})
}

// MasqueClient manages a QUIC/H3 connection to WARP edge
type MasqueClient struct {
	// edgeAddrs holds every host:port the edge advertised at registration, in
	// preference order. Ports are tried in turn until one completes a QUIC
	// handshake — 443 in particular is blocked or blackholed on some networks.
	// addrIdx remembers the last address that worked so reconnects start there.
	// Both are only touched during dial, which runs either at construction or as
	// the sole reconnect flight.
	edgeAddrs  []string
	addrIdx    int
	tlsConfig  *tls.Config
	quicConfig *quic.Config
	token      string

	connMu    sync.RWMutex
	cur       *connBundle
	closed    bool
	closeOnce sync.Once
	lifeCtx   context.Context
	lifeStop  context.CancelFunc

	// Reconnects are singleflight. The dial itself runs outside the requesting
	// handler, so a canceled SOCKS request can stop waiting without abandoning a
	// half-built shared connection. lifeCtx still cancels it immediately on
	// client shutdown.
	reconnectMu     sync.Mutex
	reconnectFlight *reconnectFlight

	// Shared HTTP/2 DoH connection; created on first lookup, replaced when a
	// query finds it unusable. dohDial coalesces cold-start dials so a burst of
	// concurrent lookups establishes one connection rather than one each.
	dohMu   sync.Mutex
	doh     *dohConn
	dohDial *dohDialFlight

	// dialDoHFn overrides how a DoH connection is established. Nil in production
	// (meaning dialAnyDoH); set by tests that need to exercise the coalescing
	// logic without a live tunnel.
	dialDoHFn func(context.Context) (*dohConn, error)

	// dialFn overrides the edge dial in lifecycle tests. Nil in production.
	dialFn func(context.Context) (*connBundle, error)

	// DNS cache to avoid redundant DoH queries for the same host
	dnsCache   map[string]dnsCacheEntry
	dnsCacheMu sync.RWMutex

	// singleflight for DNS: coalesce concurrent queries for the same host
	dnsFlight   map[string]*dnsFlightResult
	dnsFlightMu sync.Mutex
}

type dnsCacheEntry struct {
	ip        net.IP
	expiresAt time.Time
}

type dnsFlightResult struct {
	ip   net.IP
	err  error
	done chan struct{}
}

type reconnectFlight struct {
	done chan struct{}
	err  error
}

// NewMasqueClient establishes a QUIC/H3 connection to the WARP edge.
// edgeAddrs are candidate host:port addresses tried in order.
func NewMasqueClient(edgeAddrs []string, tlsConfig *tls.Config, token string) (*MasqueClient, error) {
	if len(edgeAddrs) == 0 {
		return nil, errors.New("未提供任何边缘地址")
	}
	quicConfig := &quic.Config{
		KeepAlivePeriod:      10 * time.Second,
		MaxIdleTimeout:       60 * time.Second,
		HandshakeIdleTimeout: 30 * time.Second,
		EnableDatagrams:      true,
		Tracer:               h3qlog.DefaultConnectionTracer,

		// Flow-control and stream limits below are warp-svc's tokio-quiche
		// QuicSettings defaults, read out of the official binary:
		//   +0xd0 = 0x989680 (10,000,000) connection receive window
		//   +0xd8 = 0x0f4240 ( 1,000,000) stream receive window
		//   +0xf0 = 0x64     (       100) max concurrent streams
		//   +0x108 = 0x546   (      1350) max send UDP payload size
		// quic-go's defaults are far smaller and throttle bulk transfers.
		InitialConnectionReceiveWindow: 10_000_000,
		MaxConnectionReceiveWindow:     10_000_000,
		InitialStreamReceiveWindow:     1_000_000,
		MaxStreamReceiveWindow:         1_000_000,
		MaxIncomingStreams:             100,
		MaxIncomingUniStreams:          100,
		InitialPacketSize:              1350,
	}

	lifeCtx, lifeStop := context.WithCancel(context.Background())
	c := &MasqueClient{
		edgeAddrs:  edgeAddrs,
		tlsConfig:  tlsConfig.Clone(),
		quicConfig: quicConfig,
		token:      token,
		lifeCtx:    lifeCtx,
		lifeStop:   lifeStop,
		dnsCache:   make(map[string]dnsCacheEntry),
		dnsFlight:  make(map[string]*dnsFlightResult),
	}
	bundle, err := c.dial(context.Background())
	if err != nil {
		lifeStop()
		return nil, err
	}
	c.cur = bundle
	return c, nil
}

// dial tries each candidate edge address in turn, starting from the one that
// last worked, and returns the first connection that reaches H3 SETTINGS.
func (c *MasqueClient) dial(ctx context.Context) (*connBundle, error) {
	var errs []string
	n := len(c.edgeAddrs)
	for i := 0; i < n; i++ {
		idx := (c.addrIdx + i) % n
		addr := c.edgeAddrs[idx]

		bundle, err := c.dialAddr(ctx, addr)
		if err == nil {
			c.addrIdx = idx
			return bundle, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", addr, err))

		// A canceled caller means give up entirely, not try the next port.
		if ctx.Err() != nil {
			break
		}
		// The remaining candidates differ only by port, so a failure that comes
		// from this host having no route for the address family at all will
		// repeat identically. Stop instead of burning the per-address timeout
		// once per port — seven ports would otherwise take a minute to report a
		// condition already known after the first.
		if unroutableFamily(err) {
			break
		}
		log.Printf("边缘 %s 不可达（%v），尝试下一个端口 ...", addr, err)
	}
	return nil, fmt.Errorf("所有边缘地址均失败：%s", strings.Join(errs, "; "))
}

// unroutableFamily reports whether err means this host cannot reach the address
// family at all, as opposed to the particular port being blocked. Selecting an
// IPv6 edge on an IPv4-only host is the common case.
func unroutableFamily(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EHOSTUNREACH)
}

func (c *MasqueClient) dialAddr(ctx context.Context, edgeAddr string) (*connBundle, error) {
	log.Printf("QUIC 拨号 %s（SNI=%s）...", edgeAddr, c.tlsConfig.ServerName)

	udpAddr, err := net.ResolveUDPAddr("udp", edgeAddr)
	if err != nil {
		return nil, fmt.Errorf("解析边缘地址 %s 失败：%w", edgeAddr, err)
	}

	// Bind the local socket in the same address family as the edge.
	listenAddr := &net.UDPAddr{IP: net.IPv4zero}
	if udpAddr.IP.To4() == nil {
		listenAddr = &net.UDPAddr{IP: net.IPv6zero}
	}
	udpConn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("监听 UDP 失败：%w", err)
	}

	// Dial through an explicit Transport so the source connection ID length can
	// be set to 20 bytes, matching the official client (see connectionIDLength).
	qtr := &quic.Transport{Conn: udpConn, ConnectionIDLength: connectionIDLength}

	// Bound each attempt so a blackholed port fails fast and the next candidate
	// gets tried, instead of burning the full handshake idle timeout.
	dialCtx, cancelDial := context.WithTimeout(ctx, perAddrDialTimeout)
	defer cancelDial()

	quicConn, err := qtr.Dial(dialCtx, udpAddr, c.tlsConfig.Clone(), c.quicConfig)
	if err != nil {
		qtr.Close()
		udpConn.Close()
		return nil, fmt.Errorf("QUIC 拨号 %s 失败：%w", edgeAddr, err)
	}

	h3Trans := &http3.Transport{
		TLSClientConfig: c.tlsConfig,
		QUICConfig:      c.quicConfig,
		EnableDatagrams: true,
	}
	b := &connBundle{
		udpConn:  udpConn,
		qtr:      qtr,
		quicConn: quicConn,
		h3Trans:  h3Trans,
	}
	b.h3Client = h3Trans.NewClientConn(quicConn)

	setupTimer := time.NewTimer(10 * time.Second)
	defer setupTimer.Stop()
	select {
	case <-b.h3Client.ReceivedSettings():
		settings := b.h3Client.Settings()
		state := quicConn.ConnectionState()
		log.Printf("HTTP/3 就绪（ALPN=%s，datagram=%t，extended-connect=%t，scid=%d 字节）",
			state.TLS.NegotiatedProtocol, settings.EnableDatagrams,
			settings.EnableExtendedConnect, connectionIDLength)
		return b, nil
	case <-quicConn.Context().Done():
		err := context.Cause(quicConn.Context())
		b.close("setup failed")
		return nil, fmt.Errorf("HTTP/3 初始化失败：%w", err)
	case <-ctx.Done():
		b.close("setup canceled")
		return nil, context.Cause(ctx)
	case <-setupTimer.C:
		b.close("SETTINGS timeout")
		return nil, errors.New("HTTP/3 初始化超时：未等到服务端 SETTINGS")
	}
}

func (c *MasqueClient) currentConnection() (*connBundle, error) {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	if c.closed || c.cur == nil {
		return nil, net.ErrClosed
	}
	if c.cur.quicConn == nil || c.cur.h3Client == nil {
		return nil, net.ErrClosed
	}
	// Check if the QUIC connection is still alive — the context is canceled
	// when the connection is closed by either side or reaches idle timeout.
	select {
	case <-c.cur.quicConn.Context().Done():
		return nil, net.ErrClosed
	default:
	}
	return c.cur, nil
}

// openRequestStream opens an H3 request stream and returns the bundle that owns
// it. Keeping that identity is important: a failure on an old stream must never
// retire a connection another goroutine has already installed.
func (c *MasqueClient) openRequestStream(ctx context.Context) (*http3.RequestStream, *connBundle, error) {
	var firstErr error
	for attempt := 0; attempt < 2; attempt++ {
		bundle, err := c.currentConnection()
		if err != nil {
			log.Printf("HTTP/3 连接已失效，正在重连 ...")
			if reconnectErr := c.reconnect(ctx, nil); reconnectErr != nil {
				return nil, nil, fmt.Errorf("连接已失效，重连失败：%w", reconnectErr)
			}
			bundle, err = c.currentConnection()
			if err != nil {
				return nil, nil, err
			}
		}

		openCtx, cancel := context.WithTimeout(ctx, streamOpenTimeout)
		stream, err := bundle.h3Client.OpenRequestStream(openCtx)
		cancel()
		if err == nil {
			return stream, bundle, nil
		}
		if ctx.Err() != nil {
			return nil, nil, context.Cause(ctx)
		}
		if firstErr == nil {
			firstErr = err
		}

		// OpenStreamSync also waits when the peer's concurrent-stream grant is
		// exhausted. Its timeout is therefore not evidence that the shared
		// connection is bad: fail only this request and preserve every existing
		// tunnel. A stream that actually opens still gets the CONNECT exchange
		// health check below, where a path blackhole is unambiguous.
		if !bundle.streamOpenRequiresReconnect(err) {
			return nil, nil, fmt.Errorf("等待 HTTP/3 流容量失败：%w", err)
		}
		retired := c.retireConnection(bundle)
		if attempt != 0 {
			if retired {
				log.Printf("重连后 HTTP/3 流仍无法打开（%v），淘汰连接", err)
			}
			return nil, nil, fmt.Errorf("首次打开流失败：%v；重连后仍失败：%w", firstErr, err)
		}
		if retired {
			log.Printf("HTTP/3 流打开失败（%v），淘汰当前连接并重连 ...", err)
		}
		if reconnectErr := c.reconnect(ctx, bundle); reconnectErr != nil {
			return nil, nil, fmt.Errorf("打开请求流失败：%v；重连失败：%w", err, reconnectErr)
		}
	}
	return nil, nil, firstErr
}

func (b *connBundle) streamOpenRequiresReconnect(err error) bool {
	if err == nil {
		return false
	}
	return !isTimeout(err)
}

// reconnect coalesces every concurrent recovery attempt and lets each caller
// stop waiting independently. A caller whose context has expired can therefore
// return its SOCKS error promptly while the one shared recovery continues for
// future requests.
func (c *MasqueClient) reconnect(ctx context.Context, stale *connBundle) error {
	current, err := c.currentConnection()
	if err == nil && current != stale {
		return nil // another goroutine already replaced stale
	}
	if c.isClosed() {
		return net.ErrClosed
	}

	c.reconnectMu.Lock()
	// Recheck after taking reconnectMu: a flight may have completed between the
	// optimistic check above and this critical section.
	current, err = c.currentConnection()
	if err == nil && current != stale {
		c.reconnectMu.Unlock()
		return nil
	}
	if c.isClosed() {
		c.reconnectMu.Unlock()
		return net.ErrClosed
	}

	flight := c.reconnectFlight
	if flight == nil {
		flight = &reconnectFlight{done: make(chan struct{})}
		c.reconnectFlight = flight
		go c.runReconnect(flight)
	}
	c.reconnectMu.Unlock()

	select {
	case <-flight.done:
		return flight.err
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-c.lifeCtx.Done():
		return net.ErrClosed
	}
}

func (c *MasqueClient) runReconnect(flight *reconnectFlight) {
	dial := c.dial
	if c.dialFn != nil {
		dial = c.dialFn
	}

	var err error
	backoff := reconnectRetryInitial
	for {
		var bundle *connBundle
		bundle, err = dial(c.lifeCtx)
		if err == nil && bundle == nil {
			err = errors.New("边缘拨号返回了空连接")
		}
		if err == nil {
			c.connMu.Lock()
			if c.closed {
				c.connMu.Unlock()
				bundle.close("client closed")
				err = net.ErrClosed
			} else {
				old := c.cur
				c.cur = bundle
				c.connMu.Unlock()

				// Abort the old QUIC transport before closing protocol layers nested in
				// its streams. This makes TLS close_notify writes fail immediately rather
				// than blocking forever on a blackholed connection.
				old.close("replaced")
				c.invalidateDoHBundle(old)
				log.Println("HTTP/3 连接已重建")
			}
			break
		}
		if c.lifeCtx.Err() != nil || c.isClosed() {
			err = net.ErrClosed
			break
		}

		// Keep one recovery flight alive across a network outage. This both avoids a
		// dial storm from concurrent SOCKS requests and means the client heals as
		// soon as connectivity returns, even if the request that noticed the outage
		// has already timed out.
		log.Printf("HTTP/3 重连失败（%v），%s 后重试", err, backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-c.lifeCtx.Done():
			timer.Stop()
			err = net.ErrClosed
		}
		if c.lifeCtx.Err() != nil {
			break
		}
		if backoff < reconnectRetryMax {
			backoff *= 2
			if backoff > reconnectRetryMax {
				backoff = reconnectRetryMax
			}
		}
	}

	c.reconnectMu.Lock()
	flight.err = err
	if c.reconnectFlight == flight {
		c.reconnectFlight = nil
	}
	close(flight.done)
	c.reconnectMu.Unlock()
}

// retireConnection atomically removes stale from service and then aborts it.
// New requests will join the reconnect flight instead of opening more streams
// on a connection already proven unable to complete CONNECT exchanges.
func (c *MasqueClient) retireConnection(stale *connBundle) bool {
	if stale == nil {
		return false
	}
	c.connMu.Lock()
	if c.closed || c.cur != stale {
		c.connMu.Unlock()
		return false
	}
	c.cur = nil
	c.connMu.Unlock()

	stale.close("unresponsive")
	c.invalidateDoHBundle(stale)
	return true
}

func (b *connBundle) receivedPackets() uint64 {
	if b == nil || b.quicConn == nil {
		return 0
	}
	return b.quicConn.ConnectionStats().PacketsReceived
}

// connectFailureRequiresReconnect applies the transport-vs-target distinction
// to a CONNECT exchange failure. No received QUIC packet during the whole
// exchange is strong evidence of a path blackhole and recovers immediately. If
// the path made progress, require several distinct target failures in one short
// window before declaring the shared H3 session bad.
func (b *connBundle) connectFailureRequiresReconnect(err, callerErr error, target string, packetsBefore uint64) bool {
	if !shouldReconnectH3(err, callerErr) {
		return false
	}
	if !isTimeout(err) {
		return true
	}
	if b.receivedPackets() <= packetsBefore {
		return true
	}
	return b.noteProgressingCONNECTFailure(target, time.Now())
}

func (b *connBundle) noteProgressingCONNECTFailure(target string, now time.Time) bool {
	b.healthMu.Lock()
	defer b.healthMu.Unlock()
	if b.failureSince.IsZero() || now.Sub(b.failureSince) > connectFailureWindow {
		b.failureSince = now
		b.failureTargets = make(map[string]struct{})
	}
	b.failureTargets[target] = struct{}{}
	return len(b.failureTargets) >= connectFailureTargets
}

func (b *connBundle) noteCONNECTSuccess() {
	if b == nil {
		return
	}
	b.healthMu.Lock()
	b.failureSince = time.Time{}
	b.failureTargets = nil
	b.healthMu.Unlock()
}

// SOCKS5 request commands (RFC 1928 §4).
const (
	socks5CmdConnect      = 0x01
	socks5CmdBind         = 0x02
	socks5CmdUDPAssociate = 0x03
)

// SOCKS5Config controls how a SOCKS5 connection is served.
type SOCKS5Config struct {
	// Username and Password, when both non-empty, make username/password auth
	// (RFC 1929) mandatory.
	Username string
	Password string

	// AllowUDP enables UDP ASSOCIATE. Those datagrams do not traverse the WARP
	// tunnel — see tunnel/udp.go.
	AllowUDP bool
}

// HandleSOCKS5 processes a single SOCKS5 connection.
// When ctx is canceled (e.g. SIGINT), the connection is forcefully closed
// so io.Copy in the relay goroutines unblocks and the handler exits.
func (c *MasqueClient) HandleSOCKS5(ctx context.Context, conn net.Conn, cfg SOCKS5Config) {
	username, password := cfg.Username, cfg.Password
	defer conn.Close()

	// Monitor context cancellation: closing conn unblocks any in-progress
	// io.Copy relay goroutines, letting the handler return promptly. The monitor
	// must also stop when this handler returns — ctx lives as long as the
	// process, so waiting on it alone parks one goroutine per connection handled
	// for the rest of the run.
	handlerDone := make(chan struct{})
	defer close(handlerDone)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-handlerDone:
		}
	}()

	conn.SetDeadline(time.Now().Add(15 * time.Second))

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return
	}
	data := buf[:n]

	if len(data) < 2 || data[0] != 0x05 {
		return
	}

	requireAuth := username != "" && password != ""

	if requireAuth {
		// Offer username/password auth (0x02) only
		if _, err := conn.Write([]byte{0x05, 0x02}); err != nil {
			return
		}

		// Read auth request: [0x01, ulen, username, plen, password]
		n, err = conn.Read(buf)
		if err != nil {
			return
		}
		authData := buf[:n]
		if len(authData) < 2 || authData[0] != 0x01 {
			return
		}
		ulen := int(authData[1])
		if len(authData) < 2+ulen+1 {
			return
		}
		recvUser := string(authData[2 : 2+ulen])
		plen := int(authData[2+ulen])
		if len(authData) < 2+ulen+1+plen {
			return
		}
		recvPass := string(authData[2+ulen+1 : 2+ulen+1+plen])

		if recvUser != username || recvPass != password {
			conn.Write([]byte{0x01, 0x01}) // auth failure
			return
		}
		conn.Write([]byte{0x01, 0x00}) // auth success
	} else {
		// No auth required
		if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
			return
		}
	}

	n, err = conn.Read(buf)
	if err != nil {
		return
	}
	data = buf[:n]

	if len(data) < 4 {
		sendSocks5Err(conn, 0x07)
		return
	}

	switch data[1] {
	case socks5CmdConnect:
		// handled below
	case socks5CmdUDPAssociate:
		if !cfg.AllowUDP {
			sendSocks5Err(conn, 0x07)
			return
		}
		// The association outlives this exchange, so drop the handshake deadline.
		conn.SetDeadline(time.Time{})
		c.handleUDPAssociate(ctx, conn)
		return
	default:
		// BIND (0x02) and anything else.
		sendSocks5Err(conn, 0x07)
		return
	}

	targetAddr, err := parseSOCKS5Addr(data)
	if err != nil {
		sendSocks5Err(conn, 0x08)
		return
	}

	log.Printf("SOCKS5 CONNECT %s", targetAddr)
	conn.SetDeadline(time.Time{})

	// Bound DNS resolution + tunnel setup. Derived from the caller's
	// context so a shutdown cancels setup that is still in flight instead of
	// letting it run to the full timeout. The budget includes one reconnect and
	// retry when a stale H3 session is detected.
	setupCtx, setupCancel := context.WithTimeout(ctx, socksSetupTimeout)
	defer setupCancel()

	connectTarget, hostHeader, err := c.resolveTarget(setupCtx, targetAddr)
	if err != nil {
		log.Printf("解析失败：%v", err)
		sendSocks5Err(conn, 0x04)
		return
	}
	log.Printf("目标已解析：%s（主机名：%s）", connectTarget, hostHeader)

	req := &http.Request{
		Method: "CONNECT",
		Host:   connectTarget,
		URL:    &url.URL{Scheme: "https", Host: connectTarget},
		Header: make(http.Header),
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if hostHeader != "" {
		// When the target was resolved from a domain, send the original
		// domain:port as the Host header so the edge can do virtual hosting.
		_, hostPort, _ := net.SplitHostPort(connectTarget)
		req.Header.Set("Host", hostHeader+":"+hostPort)
	}

	// H3 CONNECT. If a locally-opened stream gets no response, the QUIC path can
	// be dead even while quicConn.Context still says it is alive. Retire that
	// exact connection generation, redial once, and retry the exchange.
	reqStream, _, resp, err := c.establishCONNECT(setupCtx, req, connectExchangeTimeout)
	if err != nil {
		log.Printf("H3 CONNECT %s 失败：%v", connectTarget, err)
		sendSocks5Err(conn, 0x04)
		return
	}
	if resp.StatusCode != 200 {
		log.Printf("H3 CONNECT %s -> %d", connectTarget, resp.StatusCode)

		releaseStream(reqStream)
		sendSocks5Err(conn, 0x04)
		return
	}

	log.Printf("H3 CONNECT 200 %s（colo=%s）",
		connectTarget, resp.Header.Get("Cf-Warp-Colo"))

	sendSocks5Success(conn)
	log.Printf("隧道已建立：%s", targetAddr)

	// Releasing the H3 stream is not optional. Closing only the send side leaves
	// the receive side open, and the edge keeps its half of a CONNECT tunnel open
	// until the target closes — so a client that vanishes mid-transfer (a killed
	// download, Ctrl+C) strands this handler blocked on the read forever. That
	// leaks the goroutine *and* the stream, and the edge grants only a finite
	// number of concurrent streams: a few abandoned transfers exhaust the grant
	// and every later OpenRequestStream blocks with nothing logged.
	var once sync.Once
	release := func() {
		once.Do(func() {
			// Both directions must be canceled. Close only sends FIN and does not
			// unblock a Write parked on QUIC flow control after the path dies.
			reqStream.CancelRead(quic.StreamErrorCode(http3.ErrCodeNoError))
			reqStream.CancelWrite(quic.StreamErrorCode(http3.ErrCodeNoError))
			_ = reqStream.Close()
			_ = conn.Close()
		})
	}
	defer release()

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		io.Copy(conn, reqStream)
		// The edge finished sending: tear the pair down so the other copy exits.
		release()
	}()

	_, clientErr := io.Copy(reqStream, conn)

	if clientErr != nil {
		// The client connection broke rather than closing cleanly, so nothing can
		// be delivered to it. Release immediately instead of waiting on a response
		// that now has nowhere to go.
		release()
	} else {
		// A clean EOF may be a half-close: the client is done sending but still
		// reading. Let the response drain, bounded so an abandoned tunnel cannot
		// hold a stream indefinitely.
		_ = reqStream.Close()
		select {
		case <-drained:
		case <-time.After(relayDrainGrace):
			log.Printf("隧道 %s：客户端半关闭后 %s 无响应，释放流",
				targetAddr, relayDrainGrace)
			release()
		}
	}

	<-drained
	log.Printf("隧道已关闭：%s", targetAddr)
}

// releaseStream fully retires an H3 stream. Closing only the send side leaves the
// receive side open, and the edge keeps its half of a CONNECT tunnel open until
// the target closes — so a stream abandoned that way is never returned to the
// edge's finite concurrent-stream grant.
func releaseStream(s *http3.RequestStream) {
	if s == nil {
		return
	}
	s.CancelRead(quic.StreamErrorCode(http3.ErrCodeNoError))
	s.CancelWrite(quic.StreamErrorCode(http3.ErrCodeNoError))
	_ = s.Close()
}

// establishCONNECT performs a CONNECT exchange and retries once on a fresh H3
// connection when the error indicates that the shared connection, rather than
// the requested target, became unusable. The returned bundle owns stream.
func (c *MasqueClient) establishCONNECT(ctx context.Context, req *http.Request, timeout time.Duration) (*http3.RequestStream, *connBundle, *http.Response, error) {
	var firstErr error
	for attempt := 0; attempt < 2; attempt++ {
		stream, bundle, err := c.openRequestStream(ctx)
		if err != nil {
			if firstErr != nil {
				return nil, nil, nil, fmt.Errorf("首次 CONNECT 失败：%v；重连后打开流失败：%w", firstErr, err)
			}
			return nil, nil, nil, err
		}

		packetsBefore := bundle.receivedPackets()
		resp, err := connectThroughEdge(stream, req, connectDeadline(ctx, timeout))
		if err == nil {
			bundle.noteCONNECTSuccess()
			return stream, bundle, resp, nil
		}
		releaseStream(stream)
		if firstErr == nil {
			firstErr = err
		}
		if !bundle.connectFailureRequiresReconnect(err, ctx.Err(), req.Host, packetsBefore) {
			return nil, nil, nil, err
		}

		retired := c.retireConnection(bundle)
		if attempt != 0 {
			// The retry budget is exhausted, but leave no connection that this
			// attempt proved unhealthy in service. The next request starts a fresh
			// singleflight reconnect instead of repeating the timeout on it.
			if retired {
				log.Printf("重试的 HTTP/3 CONNECT 仍失败（%v），淘汰连接", err)
			}
			return nil, nil, nil, err
		}
		if retired {
			log.Printf("HTTP/3 CONNECT 交换失败（%v），淘汰当前连接并重连 ...", err)
		}
		if reconnectErr := c.reconnect(ctx, bundle); reconnectErr != nil {
			return nil, nil, nil, fmt.Errorf("%v；恢复 HTTP/3 连接失败：%w", err, reconnectErr)
		}
	}
	return nil, nil, nil, firstErr
}

// shouldReconnectH3 distinguishes a dead shared transport from a target-level
// stream rejection. CONNECT response deadlines are the important case: QUIC
// can remain locally "alive" during a path blackhole and still allow new stream
// objects, while no request bytes or response frames reach the edge.
func shouldReconnectH3(err, callerErr error) bool {
	if err == nil || callerErr != nil {
		return false
	}
	if isTimeout(err) {
		return true
	}

	// A stream reset is scoped to one CONNECT (for example, the edge rejected a
	// target). Other H3/QUIC I/O failures imply connection-level state and are
	// safe to recover by generation: if another goroutine already reconnected,
	// reconnect observes bundle != current and becomes a no-op.
	var streamErr *quic.StreamError
	return !errors.As(err, &streamErr)
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// connectThroughEdge runs the H3 CONNECT exchange on stream and returns the edge's
// response.
//
// Neither SendRequestHeader nor ReadResponse takes a context or has any timeout of
// its own, so a stream the edge accepts but never answers would park the caller
// forever with nothing logged. The exchange therefore runs under a deadline, which
// is cleared on success: the stream becomes a long-lived tunnel afterwards and a
// leftover deadline would kill it mid-transfer.
func connectThroughEdge(stream *http3.RequestStream, req *http.Request, deadline time.Time) (*http.Response, error) {
	if err := stream.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("设置 CONNECT 超时失败：%w", err)
	}
	if err := stream.SendRequestHeader(req); err != nil {
		return nil, fmt.Errorf("发送 CONNECT 失败：%w", err)
	}
	resp, err := stream.ReadResponse()
	if err != nil {
		return nil, fmt.Errorf("读取 CONNECT 响应失败：%w", err)
	}
	if err := stream.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("清除 CONNECT 超时失败：%w", err)
	}
	return resp, nil
}

// connectDeadline returns the deadline to bound a CONNECT exchange with: the
// caller's remaining budget when it has one, otherwise a fixed fallback so the
// exchange can never be unbounded.
func connectDeadline(ctx context.Context, fallback time.Duration) time.Time {
	deadline := time.Now().Add(fallback)
	if dl, ok := ctx.Deadline(); ok {
		if dl.Before(deadline) {
			return dl
		}
	}
	return deadline
}

// resolveTarget resolves through WARP tunnel DNS
func (c *MasqueClient) resolveTarget(ctx context.Context, targetAddr string) (connectTarget string, hostHeader string, err error) {
	host, port, splitErr := net.SplitHostPort(targetAddr)
	if splitErr != nil {
		// Passing an unparseable target through unchanged used to hide a
		// malformed address until the edge cancelled the stream, with nothing in
		// the log pointing at the cause. Fail here, where the address is named.
		return "", "", fmt.Errorf("目标地址 %q 无法解析为 host:port：%w", targetAddr, splitErr)
	}
	if net.ParseIP(host) != nil {
		// Re-join rather than echoing the input: this normalises an IPv6 literal
		// to exactly one level of brackets.
		return net.JoinHostPort(host, port), "", nil
	}

	ip, dnsErr := c.resolveDNS(ctx, host)
	if dnsErr != nil {
		// resolveDNS already names the host; don't wrap it a second time.
		return "", "", dnsErr
	}

	// JoinHostPort brackets an IPv6 literal itself. Bracketing here as well
	// produced "[[2606:...]]:443", which the edge answered by cancelling the
	// stream — a latent bug that only surfaced once AAAA answers were used.
	return net.JoinHostPort(ip.String(), port), host, nil
}

// cacheResolution stores a resolution and keeps the cache from growing without
// bound. Entries were previously only ever added: expired ones were skipped on
// read but never removed, so a long run that resolves many distinct names (any
// browser workload) grew the map forever.
//
// Sweeping expired entries once the map crosses a threshold bounds it by the
// number of names resolved within one TTL window rather than by uptime. The hard
// cap is a backstop for the pathological case where that window alone is huge;
// dropping the whole map is acceptable because the cache is an optimisation, not
// a source of truth.
func (c *MasqueClient) cacheResolution(host string, ip net.IP, ttl time.Duration) {
	c.dnsCacheMu.Lock()
	defer c.dnsCacheMu.Unlock()

	if len(c.dnsCache) >= dnsCacheSweepAt {
		now := time.Now()
		for name, entry := range c.dnsCache {
			if !now.Before(entry.expiresAt) {
				delete(c.dnsCache, name)
			}
		}
		if len(c.dnsCache) >= dnsCacheMaxEntries {
			log.Printf("DNS 缓存中 %d 条仍在有效期内，整体清空", len(c.dnsCache))
			clear(c.dnsCache)
		}
	}
	c.dnsCache[host] = dnsCacheEntry{ip: ip, expiresAt: time.Now().Add(ttl)}
}

func (c *MasqueClient) resolveDNS(ctx context.Context, host string) (net.IP, error) {
	// Check cache first
	c.dnsCacheMu.RLock()
	if entry, ok := c.dnsCache[host]; ok && time.Now().Before(entry.expiresAt) {
		c.dnsCacheMu.RUnlock()
		log.Printf("WARP DNS（缓存）✓ %s -> %s", host, entry.ip)
		return entry.ip, nil
	}
	c.dnsCacheMu.RUnlock()

	// Singleflight: if another goroutine is already resolving this host, wait for it.
	c.dnsFlightMu.Lock()
	if flight, ok := c.dnsFlight[host]; ok {
		c.dnsFlightMu.Unlock()
		select {
		case <-flight.done:
			return flight.ip, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &dnsFlightResult{done: make(chan struct{})}
	c.dnsFlight[host] = flight
	c.dnsFlightMu.Unlock()

	// Do the actual resolution — singleflight means only one goroutine runs this
	// for a given host at a time.
	//
	// Retry exactly once, and only when the shared connection itself was at fault:
	// dnsQuery has already retired that connection (by identity), so the second
	// attempt dials a fresh one. A DNS-level answer — NXDOMAIN, no A record, a
	// non-200 status — says nothing about the connection, and neither does this
	// query's own deadline expiring; retrying or tearing the connection down for
	// those would abort every other lookup multiplexed on it.
	var ip net.IP
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resolved, ttl, err := c.dnsQuery(ctx, host)
		if err == nil {
			ip = resolved
			lastErr = nil
			log.Printf("WARP DNS ✓ %s -> %s（TTL %s）", host, ip, ttl)
			c.cacheResolution(host, ip, ttl)
			break
		}
		lastErr = err
		if !shouldRetryDoH(err, ctx.Err()) {
			break
		}
	}
	if ip == nil {
		lastErr = fmt.Errorf("%s 的 DNS 解析失败：%w", host, lastErr)
	}

	// Store result and wake waiters
	flight.ip = ip
	flight.err = lastErr
	close(flight.done)

	// Clean up flight entry
	c.dnsFlightMu.Lock()
	delete(c.dnsFlight, host)
	c.dnsFlightMu.Unlock()

	return ip, lastErr
}

// dnsQuery opens a fresh H3 CONNECT stream to the DoH server, performs
// a single DNS-over-HTTPS A-record query, and closes the stream. Each
// call is fully self-contained so there is no shared connection state
// between concurrent DNS lookups — they can proceed in parallel on
// independent H3 streams without serialisation or stale-connection bugs.
// errDoHTransport marks a failure that means the shared DoH connection is no
// longer usable, as distinct from a DNS-level answer (NXDOMAIN, no A record, a
// non-200 status) or this query's own deadline expiring — both of which leave the
// connection perfectly healthy. Only a transport failure may retire the shared
// connection or justify a retry, because retiring it aborts every other query
// multiplexed on it.
var errDoHTransport = errors.New("DoH 连接不可用")

// shouldRetryDoH reports whether a failed lookup deserves one more attempt.
// Only a transport failure qualifies: dnsQuery has already retired that
// connection, so a second attempt gets a fresh one. DNS-level answers and
// expired deadlines are final — retrying them just doubles the latency, and
// treating them as connection failures would abort sibling lookups.
func shouldRetryDoH(err, ctxErr error) bool {
	return errors.Is(err, errDoHTransport) && ctxErr == nil
}

// dohConn is a long-lived DNS-over-HTTPS connection: one H3 CONNECT stream to a
// DoH server, TLS inside it, and an HTTP/2 client connection on top of that.
//
// This mirrors warp-svc's dns_proxy::resolver::MultiplexedDohProvider, which
// keeps a single HTTP/2 connection per name server and gives every query its own
// H2 stream (hickory clones the h2 SendRequest handle per query while the
// connection driver runs in its own task). HTTP/1.1 cannot do this — a keep-alive
// connection is strictly serial, so concurrent lookups queue behind each other.
// x/net/http2's ClientConn provides the same property here: RoundTrip is safe to
// call concurrently and each call gets its own H2 stream.
type dohConn struct {
	addr   string
	stream *http3.RequestStream
	tls    *tls.Conn
	h2     *http2.ClientConn
	bundle *connBundle
	once   sync.Once

	healthMu       sync.Mutex
	failureSince   time.Time
	failureTargets map[string]struct{}
}

func (d *dohConn) close() {
	if d == nil {
		return
	}
	d.once.Do(func() {
		// Abort the carrier first. Closing H2/TLS first can make tls.Close block
		// writing close_notify to a flow-controlled H3 stream on a dead path.
		releaseStream(d.stream)
		if d.h2 != nil {
			_ = d.h2.Close()
		}
		if d.tls != nil {
			_ = d.tls.Close()
		}
	})
}

func (d *dohConn) queryTimeoutRequiresReconnect(callerErr error, target string, packetsBefore uint64) bool {
	if d == nil || callerErr != nil {
		return false
	}
	if d.bundle == nil || d.bundle.receivedPackets() <= packetsBefore {
		return true
	}
	return d.noteProgressingQueryTimeout(target, time.Now())
}

func (d *dohConn) noteProgressingQueryTimeout(target string, now time.Time) bool {
	d.healthMu.Lock()
	defer d.healthMu.Unlock()
	if d.failureSince.IsZero() || now.Sub(d.failureSince) > connectFailureWindow {
		d.failureSince = now
		d.failureTargets = make(map[string]struct{})
	}
	d.failureTargets[target] = struct{}{}
	return len(d.failureTargets) >= connectFailureTargets
}

func (d *dohConn) noteQuerySuccess() {
	if d == nil {
		return
	}
	d.healthMu.Lock()
	d.failureSince = time.Time{}
	d.failureTargets = nil
	d.healthMu.Unlock()
}

// dohDialFlight is one in-progress DoH dial that other callers wait on instead
// of starting their own.
type dohDialFlight struct {
	done chan struct{}
	conn *dohConn
	err  error
}

// dohConnection returns the shared DoH connection, establishing it on first use.
// Like the official client it is created lazily, never re-dialled in place, and
// replaced only once a query finds it unusable.
//
// dohMu is deliberately NOT held across the dial. Doing so deadlocks: dialDoH ->
// openRequestStream -> reconnect -> invalidateDoH also wants dohMu, and Go
// mutexes are not reentrant. Instead exactly one caller dials while the others
// wait on dohDial, so a cold-start burst still yields a single connection —
// matching warp-svc, where hickory holds the slot in an async mutex it *can*
// keep across the dial. Without this, N concurrent first lookups each paid a
// full CONNECT + TLS + H2 handshake and then discarded all but one connection.
func (c *MasqueClient) dohConnection(ctx context.Context) (*dohConn, error) {
	for {
		if d := c.liveDoH(); d != nil {
			return d, nil
		}

		c.dohMu.Lock()
		if flight := c.dohDial; flight != nil {
			c.dohMu.Unlock()
			select {
			case <-flight.done:
				if flight.err != nil {
					return nil, flight.err
				}
				// The winner's connection may already have been retired by a
				// failing query; re-check rather than handing back a dead one.
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		flight := &dohDialFlight{done: make(chan struct{})}
		c.dohDial = flight
		c.dohMu.Unlock()

		dial := c.dialAnyDoH
		if c.dialDoHFn != nil {
			dial = c.dialDoHFn
		}
		d, err := dial(ctx)
		if err == nil && d == nil {
			err = errors.New("DoH 拨号返回了空连接")
		}

		// Validate and install while holding connMu for reading. Reconnect and Close
		// both take it for writing before invalidating DoH, so neither can swap the
		// carrier between this generation check and the install. Without this, a
		// dial that completed alongside reconnect could orphan a dead DoH connection
		// after old-generation cleanup had already run.
		if err == nil {
			c.connMu.RLock()
			switch {
			case c.closed:
				c.connMu.RUnlock()
				d.close()
				d, err = nil, net.ErrClosed
			case d.bundle != nil && c.cur != d.bundle:
				c.connMu.RUnlock()
				d.close()
				d, err = nil, fmt.Errorf("%w：DoH 所属的 HTTP/3 连接已被替换", errDoHTransport)
			default:
				c.dohMu.Lock()
				c.doh = d
				c.dohDial = nil
				flight.conn, flight.err = d, nil
				c.dohMu.Unlock()
				c.connMu.RUnlock()
				close(flight.done)
				return d, nil
			}
		}

		// Error publication does not need connMu: no connection was installed.
		c.dohMu.Lock()
		c.dohDial = nil
		flight.conn, flight.err = d, err
		c.dohMu.Unlock()
		close(flight.done)
		return nil, err
	}
}

// isClosed reports whether Close has run. closed is written under connMu, so it
// must be read under connMu too.
func (c *MasqueClient) isClosed() bool {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.closed
}

// liveDoH returns the shared connection if it is still usable, and retires it
// otherwise. Never blocks on I/O while holding dohMu.
//
// Usability is judged with State() rather than CanTakeNewRequest(): the latter is
// also false when the connection is merely saturated (at the server's
// MAX_CONCURRENT_STREAMS), and a saturated connection is healthy — RoundTrip will
// wait for a stream slot, honouring the caller's context. Only a closed or
// closing (GOAWAY, DoNotReuse) connection is actually spent.
func (c *MasqueClient) liveDoH() *dohConn {
	c.dohMu.Lock()
	d := c.doh
	if d == nil {
		c.dohMu.Unlock()
		return nil
	}
	if st := d.h2.State(); !st.Closed && !st.Closing {
		c.dohMu.Unlock()
		return d
	}
	c.doh = nil
	c.dohMu.Unlock()

	log.Printf("到 %s 的 DoH 连接已失效，重新拨号", d.addr)
	d.close()
	return nil
}

// dialAnyDoH tries each DoH endpoint in turn. Runs with no lock held.
func (c *MasqueClient) dialAnyDoH(ctx context.Context) (*dohConn, error) {
	var errs []string
	for _, addr := range dnsServers {
		d, err := c.dialDoH(ctx, addr)
		if err == nil {
			return d, nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v", addr, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("没有可用的 DoH 服务器：%s", strings.Join(errs, "; "))
}

// invalidateDoH drops the shared DoH connection. When stale is non-nil the
// connection is only dropped if it is still the current one, so a query that
// failed on an already-replaced connection does not discard the new one.
func (c *MasqueClient) invalidateDoH(stale *dohConn) {
	c.dohMu.Lock()
	victim := c.doh
	if victim == nil || (stale != nil && victim != stale) {
		c.dohMu.Unlock()
		return
	}
	c.doh = nil
	c.dohMu.Unlock()
	victim.close()
}

// invalidateDoHBundle retires only a DoH connection carried by stale. A new
// DoH connection may already have been installed on the replacement bundle by
// the time old cleanup runs, and must not be torn down with it.
func (c *MasqueClient) invalidateDoHBundle(stale *connBundle) {
	if stale == nil {
		return
	}
	c.dohMu.Lock()
	victim := c.doh
	if victim == nil || victim.bundle != stale {
		c.dohMu.Unlock()
		return
	}
	c.doh = nil
	c.dohMu.Unlock()
	victim.close()
}

func (c *MasqueClient) dialDoH(ctx context.Context, addr string) (*dohConn, error) {
	req := &http.Request{
		Method: "CONNECT",
		Host:   addr,
		URL:    &url.URL{Scheme: "https", Host: addr},
		Header: make(http.Header),
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	reqStream, bundle, resp, err := c.establishCONNECT(ctx, req, dohHandshakeTimeout)
	if err != nil {
		return nil, fmt.Errorf("到 %s 的 DoH CONNECT 失败：%w", addr, err)
	}
	if resp.StatusCode != 200 {
		releaseStream(reqStream)
		return nil, fmt.Errorf("到 %s 的 DoH CONNECT 返回 %d", addr, resp.StatusCode)
	}

	host, _, _ := net.SplitHostPort(addr)
	sc := &streamConn{
		RequestStream: reqStream,
		localAddr:     &net.TCPAddr{IP: net.IPv4zero},
		remoteAddr:    &net.TCPAddr{IP: net.ParseIP(host), Port: 443},
	}

	// ALPN must offer h2 — without it the server negotiates HTTP/1.1 and the
	// HTTP/2 client below would talk into a connection that cannot frame it.
	tlsConn := tls.Client(sc, &tls.Config{
		ServerName: dohServerName,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"h2"},
	})

	// Bound the handshake only. The deadline must be cleared afterwards: this
	// connection is long-lived, and a leftover deadline would kill it later.
	if err := tlsConn.SetDeadline(time.Now().Add(dohHandshakeTimeout)); err != nil {
		releaseStream(reqStream)
		return nil, fmt.Errorf("设置 DoH 超时失败：%w", err)
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		tlsConn.Close()
		releaseStream(reqStream)
		return nil, fmt.Errorf("DoH TLS 握手失败：%w", err)
	}
	if err := tlsConn.SetDeadline(time.Time{}); err != nil {
		tlsConn.Close()
		releaseStream(reqStream)
		return nil, fmt.Errorf("清除 DoH 超时失败：%w", err)
	}
	if proto := tlsConn.ConnectionState().NegotiatedProtocol; proto != "h2" {
		tlsConn.Close()
		releaseStream(reqStream)
		return nil, fmt.Errorf("DoH 服务端协商出 %q，期望 h2", proto)
	}

	// DisableCompression stops x/net/http2 from adding "accept-encoding: gzip",
	// which warp-svc does not send; DNS wire-format responses are tiny anyway.
	h2Conn, err := (&http2.Transport{DisableCompression: true}).NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		releaseStream(reqStream)
		return nil, fmt.Errorf("DoH HTTP/2 握手失败：%w", err)
	}

	log.Printf("DoH 连接就绪：%s（h2 多路复用）", addr)
	return &dohConn{addr: addr, stream: reqStream, tls: tlsConn, h2: h2Conn, bundle: bundle}, nil
}

// dnsQuery resolves host over the shared DoH connection, asking for A and AAAA
// at the same time and preferring the A answer.
//
// WARP is dual-stack — the edge reaches the target, and it has IPv6 egress — so
// an A-only client cannot reach IPv6-only destinations at all. Both questions go
// out concurrently rather than sequentially because that is exactly what the
// multiplexed H2 connection buys: two questions, two H2 streams, one round trip.
// Preferring A matches hickory's default Ipv4thenIpv6 strategy, which warp-svc
// does not override.
func (c *MasqueClient) dnsQuery(ctx context.Context, host string) (net.IP, time.Duration, error) {
	d, err := c.dohConnection(ctx)
	if err != nil {
		return nil, 0, err
	}

	type answer struct {
		ip  net.IP
		ttl time.Duration
		err error
	}
	results := make(chan answer, 2)
	for _, qtype := range [...]dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA} {
		go func(qtype dnsmessage.Type) {
			ip, ttl, err := c.dnsQueryType(ctx, d, host, qtype)
			results <- answer{ip, ttl, err}
		}(qtype)
	}

	// An IPv4 answer is preferred, so return the moment one arrives instead of
	// waiting for the other question. Waiting for both would make every lookup
	// cost the slower of the two — a host whose AAAA query runs to the query
	// timeout would take that long even though its A answer came back at once.
	// The result channel is buffered, so the abandoned goroutine still finishes.
	var v6 answer
	var transportErr error
	for i := 0; i < 2; i++ {
		got := <-results
		switch {
		case got.err != nil:
			// A transport failure applies to both questions: remember it so the
			// caller can retry on a fresh connection if neither family answered.
			// It outranks a DNS-level error, which says nothing about the link.
			if transportErr == nil || errors.Is(got.err, errDoHTransport) {
				transportErr = got.err
			}
		case got.ip.To4() != nil:
			return got.ip, got.ttl, nil
		default:
			v6 = got
		}
	}

	if v6.ip != nil {
		return v6.ip, v6.ttl, nil
	}
	if transportErr != nil {
		return nil, 0, transportErr
	}
	return nil, 0, fmt.Errorf("%s 没有 A 或 AAAA 记录", host)
}

// dnsQueryType issues one wire-format question on the shared connection.
// warp-svc uses POST with application/dns-message and has no DoH-JSON code path
// at all, so this matches it; the JSON API also costs an extra parse and a
// larger response.
func (c *MasqueClient) dnsQueryType(ctx context.Context, d *dohConn, host string, qtype dnsmessage.Type) (net.IP, time.Duration, error) {
	name, err := dnsmessage.NewName(fqdn(host))
	if err != nil {
		return nil, 0, fmt.Errorf("非法的 DNS 名称 %q：%w", host, err)
	}
	query := dnsmessage.Message{
		Header: dnsmessage.Header{RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name:  name,
			Type:  qtype,
			Class: dnsmessage.ClassINET,
		}},
	}
	wire, err := query.Pack()
	if err != nil {
		return nil, 0, fmt.Errorf("封装 DNS 查询失败：%w", err)
	}

	queryCtx, cancel := context.WithTimeout(ctx, dohQueryTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(queryCtx, http.MethodPost, dohURL, bytes.NewReader(wire))
	if err != nil {
		return nil, 0, fmt.Errorf("构造 DoH 请求失败：%w", err)
	}
	req.Header.Set("content-type", dohContentType)
	req.Header.Set("accept", dohContentType)
	req.ContentLength = int64(len(wire))
	// x/net/http2 injects "user-agent: Go-http-client/2.0" when the caller sets
	// none. warp-svc sends exactly content-type, accept and content-length, so
	// set the header to empty to suppress the default rather than advertise Go.
	req.Header.Set("user-agent", "")

	// Concurrent RoundTrips are multiplexed onto separate H2 streams.
	packetsBefore := d.bundle.receivedPackets()
	resp, err := d.h2.RoundTrip(req)
	if err != nil {
		if queryCtx.Err() != nil {
			// A caller cancellation says nothing about connection health. A query's
			// own timeout is different: no QUIC progress, or timeouts for several
			// distinct names, means this long-lived H2 carrier is half-dead. Retire it
			// so resolveDNS retries on a fresh DoH connection.
			if d.queryTimeoutRequiresReconnect(ctx.Err(), host, packetsBefore) {
				c.invalidateDoH(d)
				return nil, 0, fmt.Errorf("%w：%s 的查询超时：%v", errDoHTransport, host, err)
			}
			return nil, 0, fmt.Errorf("%s 的 DoH 查询失败：%w", host, err)
		}
		c.invalidateDoH(d)
		return nil, 0, fmt.Errorf("%w：往返请求失败：%v", errDoHTransport, err)
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, dohMaxResponseSize))
	resp.Body.Close()
	if readErr != nil {
		if queryCtx.Err() != nil {
			if d.queryTimeoutRequiresReconnect(ctx.Err(), host, packetsBefore) {
				c.invalidateDoH(d)
				return nil, 0, fmt.Errorf("%w：读取 %s 的响应超时：%v", errDoHTransport, host, readErr)
			}
			return nil, 0, fmt.Errorf("读取 %s 的 DoH 响应失败：%w", host, readErr)
		}
		c.invalidateDoH(d)
		return nil, 0, fmt.Errorf("%w：读取响应失败：%v", errDoHTransport, readErr)
	}
	if resp.StatusCode != http.StatusOK {
		d.noteQuerySuccess()
		return nil, 0, fmt.Errorf("%s 的 DoH 请求返回状态 %d", host, resp.StatusCode)
	}

	d.noteQuerySuccess()
	return parseDoHAnswer(body, host, qtype)
}

// fqdn appends the root label that dnsmessage.NewName requires.
func fqdn(host string) string {
	if strings.HasSuffix(host, ".") {
		return host
	}
	return host + "."
}

// parseDoHAnswer extracts the first address of the requested family and its TTL
// from a wire-format response, following CNAME chains by simply taking the first
// matching address record present.
func parseDoHAnswer(body []byte, host string, qtype dnsmessage.Type) (net.IP, time.Duration, error) {
	var msg dnsmessage.Message
	if err := msg.Unpack(body); err != nil {
		return nil, 0, fmt.Errorf("解析 DNS 响应失败：%w", err)
	}
	if msg.RCode != dnsmessage.RCodeSuccess {
		return nil, 0, fmt.Errorf("%s 的 DNS 响应码为 %s", host, msg.RCode)
	}
	for _, ans := range msg.Answers {
		var ip net.IP
		switch body := ans.Body.(type) {
		case *dnsmessage.AResource:
			if qtype != dnsmessage.TypeA {
				continue
			}
			ip = net.IP(body.A[:])
		case *dnsmessage.AAAAResource:
			if qtype != dnsmessage.TypeAAAA {
				continue
			}
			ip = net.IP(body.AAAA[:])
		default:
			continue
		}
		ttl := time.Duration(ans.Header.TTL) * time.Second
		if ttl < dohMinTTL {
			ttl = dohMinTTL
		}
		if ttl > dohMaxTTL {
			ttl = dohMaxTTL
		}
		return ip, ttl, nil
	}
	return nil, 0, fmt.Errorf("%s 没有 %s 记录", host, qtype)
}

func (c *MasqueClient) Close() error {
	c.closeOnce.Do(func() {
		c.connMu.Lock()
		c.closed = true
		bundle := c.cur
		c.cur = nil
		c.connMu.Unlock()

		// Cancel reconnect dials first, then abort QUIC so every nested H3 stream
		// becomes non-writable before TLS/H2 cleanup attempts close_notify.
		if c.lifeStop != nil {
			c.lifeStop()
		}
		bundle.close("bye")
		c.invalidateDoH(nil)
	})
	return nil
}

func parseSOCKS5Addr(data []byte) (string, error) {
	if len(data) < 4 {
		return "", fmt.Errorf("数据过短")
	}
	addrType := data[3]
	var host string
	var port int
	switch addrType {
	case 0x01:
		if len(data) < 10 {
			return "", fmt.Errorf("IPv4 地址数据过短")
		}
		host = net.IP(data[4:8]).String()
		port = int(data[8])<<8 | int(data[9])
	case 0x03:
		addrLen := int(data[4])
		if len(data) < 5+addrLen+2 {
			return "", fmt.Errorf("域名数据过短")
		}
		host = string(data[5 : 5+addrLen])
		port = int(data[5+addrLen])<<8 | int(data[5+addrLen+1])
	case 0x04:
		if len(data) < 22 {
			return "", fmt.Errorf("IPv6 地址数据过短")
		}
		host = net.IP(data[4:20]).String()
		port = int(data[20])<<8 | int(data[21])
	default:
		return "", fmt.Errorf("未知的地址类型：%d", addrType)
	}
	if port == 0 {
		return "", fmt.Errorf("目标端口为 0")
	}
	// JoinHostPort, not Sprintf("%s:%d"): an IPv6 literal needs brackets. Without
	// them the result ("2606:4700::1:443") is not a parseable host:port at all,
	// and the edge answers such a CONNECT by cancelling the stream.
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func sendSocks5Err(conn net.Conn, code byte) {
	conn.Write([]byte{0x05, code, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
}

func sendSocks5Success(conn net.Conn) {
	conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
}

// sendSocks5Bound replies with a real BND.ADDR/BND.PORT. UDP ASSOCIATE needs
// this: the client sends its datagrams to exactly this address, so the all-zero
// reply used for CONNECT would leave it with nowhere to send.
func sendSocks5Bound(conn net.Conn, ip net.IP, port int) error {
	reply := []byte{0x05, 0x00, 0x00}
	if v4 := ip.To4(); v4 != nil {
		reply = append(reply, 0x01)
		reply = append(reply, v4...)
	} else if v6 := ip.To16(); v6 != nil {
		reply = append(reply, 0x04)
		reply = append(reply, v6...)
	} else {
		return fmt.Errorf("绑定地址 %v 不可用", ip)
	}
	reply = binary.BigEndian.AppendUint16(reply, uint16(port))
	_, err := conn.Write(reply)
	return err
}
