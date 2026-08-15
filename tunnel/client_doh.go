package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
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
)

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
	failureTargets map[string]int
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
		d.failureTargets = make(map[string]int)
	}
	d.failureTargets[target]++
	var total int
	for _, n := range d.failureTargets {
		total += n
	}
	return total >= connectFailureTargets
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
