package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	h3qlog "github.com/quic-go/quic-go/http3/qlog"
)

// connectionIDLength matches warp-svc: tokio_quiche's SimpleConnectionIdGenerator
// emits 20-byte source connection IDs. With quic-go's 4-byte default the WARP
// edge intermittently answers with PROTOCOL_VIOLATION and drops the connection.
const connectionIDLength = 20

// socketProtector, when non-nil, is invoked with the raw fd of every UDP socket
// the tunnel dials with, right after creation and before any packet is sent.
// Android uses it to call VpnService.protect(fd): once VpnService.establish()
// installs catch-all routes, the VPN app's OWN new sockets also route through
// the TUN — and the TUN isn't read until after the dial succeeds, so the QUIC
// ClientHello would sit unprocessed in the tun and every edge handshake times
// out ("所有边缘地址均失败"). protect() exempts the dial socket and sends it
// over the physical network instead. Desktop/CLI leave this nil (no-op).
var socketProtector func(fd int) error

// SetSocketProtector 注册包级 socket 保护器，供 Android 桥（gui/androidbridge
// 包）注入 VpnService.protect 实现。桌面/CLI 不调用，保持 nil。
func SetSocketProtector(fn func(fd int) error) {
	socketProtector = fn
}

// perAddrDialTimeout bounds a single edge address attempt so an unreachable
// port falls through to the next candidate quickly.
const perAddrDialTimeout = 2 * time.Second

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
	// connectFailureTargets 是窗口内的失败次数阈值（v0.5.23 改为计数而非
	// distinct 目标数）：浏览器对少数站点并发重试时，同一目标反复失败在
	// distinct 去重下永不累计（v0.5.21 的 "3 个不同目标" 判定 → 隧道黑洞后
	// 外网永久不通，用户日志 2 个目标 × 各 2 次 = distinct 2 < 3）。计数语义
	// 下：单目标失败 1 次不重连（保护共享连接，保留 v0.5.21 场景），同/异
	// 目标累计 2 次即触发 retire + 重连恢复。
	connectFailureTargets = 2
	reconnectRetryInitial = 100 * time.Millisecond
	reconnectRetryMax     = 5 * time.Second

	// 国际出口探测配置：验证边缘节点是否真的能连通境外目标。
	// 使用 Google 公共 DNS IP (8.8.8.8:443) 作为探测目标——它在边缘网络内必达，
	// 且不依赖 DNS 解析（避免循环依赖）。
	probeEgressTarget  = "8.8.8.8:443"
	probeEgressTimeout = 5 * time.Second

	// egressProbeInterval 是运行期国际出口活性探测周期：每 20s 在共享 QUIC
	// 连接上做一次到 probeEgressTarget 的 CONNECT。把静默死会话（KeepAlive
	// 往返仍在、但国际出口已坏或路径被掐）的发现从"下一次用户 CONNECT 超时
	// （10s×2）"提前到 20s 内——真机 debugdiag：隧道被掐瞬间同一连接上所有
	// 并发流一起 read ... connection reset by peer（dn=0），浏览器看到
	// "打不开外网"（v0.5.26 之后的新数据：00:06:04 m.youtube.com 连续 2 条
	// RST 0 下行）。
	egressProbeInterval = 20 * time.Second

	// probeFailureThreshold 是运行期探测连续失败后判定连接死亡的阈值：手机
	// 网络 UDP 抖动 / 边缘偶发慢响应会让单次探测 CONNECT 超时，而拆线瞬间
	// 所有在途流一起 use of closed network connection（debugdiag：多个健康
	// 下载被一次探测的瞬时错误连坐）。真实黑洞仍有 CONNECT 失败窗口兜底，
	// 恢复延迟从 20s 增到约 40s，但不再为每次毛刺殉葬整连接。
	probeFailureThreshold = 2
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

	// dead 标记连接级故障已被观测（noteDeadStream / 运行期探测）：即使
	// quicConn.Context() 尚未 Done（黑洞路径下 socket 仍在、KeepAlive 往返
	// 没超时），后续请求也立即加入重连航班而不是在死连接上重试。重建后新
	// bundle 零值开始；currentConnection 与 openRequestStream 在置位后直接
	// 返回 net.ErrClosed，消除"死连接上 10s×2 CONNECT 超时"（debugdiag：
	// 隧道被掐后 m.youtube.com 连续 2 条 read tcp ... connection reset by
	// peer 且 dn=0——浏览器等满重试窗口才放弃）。
	dead atomic.Bool

	// A live QUIC path can coexist with a wedged H3 session. Track CONNECT
	// timeouts in one short window so a single unreachable target doesn't cause
	// collateral reconnects, while repeated failures — same or distinct target
	// — still detect a session-wide blackhole (v0.5.23: distinct-only counting
	// never reached the threshold for browser-style retries on one host).
	failureSince   time.Time
	failureTargets map[string]int

	// probeFailures 是连续的国际出口探测失败次数（healthMu 保护）。单次探测
	// 失败可能是手机网络 UDP 抖动 / 边缘偶发慢响应，立即拆共享连接会把所有
	// 在途流一起拖死（debugdiag：批量死亡均为本地拆线的 use of closed
	// network connection）。连续 probeFailureThreshold 次才判定连接死亡。
	probeFailures int

	// streamFailureSince / streamFailureCount 是非连接级流错误的观察窗（与
	// CONNECT 失败窗口同语义，独立计数避免互相污染）：单条流的抖动（如边缘
	// 对单个目标 reset）不立即 retire，窗口内累计 connectFailureTargets 次
	// 才判定共享连接死亡。
	streamFailureSince time.Time
	streamFailureCount int
}

// close abortively tears down the bundle. Safe on a nil receiver, concurrent
// calls, and partially-constructed bundles.
func (b *connBundle) close(reason string) {
	if b == nil {
		return
	}
	b.closeOnce.Do(func() {
		// 记录拆线原因：这是判定"谁先动手"的第一证据（探针阈值 / 失败窗口 /
		// 换代 / 用户关闭各有独立 reason 字符串），此前全部静默丢弃——批量
		// 死亡时只能看到 use of closed network connection，无法归因是运营商
		// 掐线还是本地误拆。
		log.Printf("QUIC 隧道连接关闭：%s", reason)
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

	// probeFn overrides the egress probe in tests. Nil in production (meaning
	// probeInternationalEgress).
	probeFn func(context.Context, *connBundle) error

	// DNS cache to avoid redundant DoH queries for the same host
	dnsCache   map[string]dnsCacheEntry
	dnsCacheMu sync.RWMutex

	// singleflight for DNS: coalesce concurrent queries for the same host
	dnsFlight   map[string]*dnsFlightResult
	dnsFlightMu sync.Mutex
}

type reconnectFlight struct {
	done chan struct{}
	err  error
}

// NewMasqueClient establishes a QUIC/H3 connection to the WARP edge.
// edgeAddrs are candidate host:port addresses tried in order. Dial retries
// indefinitely (exponential backoff) and is not externally cancellable —
// used by the desktop/CLI path where the process owns the tunnel lifetime.
func NewMasqueClient(edgeAddrs []string, tlsConfig *tls.Config, token string) (*MasqueClient, error) {
	return NewMasqueClientContext(context.Background(), edgeAddrs, tlsConfig, token)
}

// NewMasqueClientContext is NewMasqueClient with an externally-cancellable
// initial dial. ctx cancels the bootstrapping dial loop (so a host that wants
// to abort startup — e.g. the Android bridge when the user hits Stop while the
// edge is unreachable and the client would otherwise retry forever — can
// return immediately). The established connection's lifetime is still owned
// by the returned client's own lifecycle ctx; ctx only gates construction.
func NewMasqueClientContext(ctx context.Context, edgeAddrs []string, tlsConfig *tls.Config, token string) (*MasqueClient, error) {
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

	// Initial dial loop: same exponential-backoff pattern as runReconnect so that
	// a transient network outage at start — e.g. routing not ready on boot — does
	// not kill the process. The loop runs until dial succeeds or the process
	// receives a signal (SIGINT/SIGTERM kills the process by default before the
	// signal handler is installed below).
	backoff := reconnectRetryInitial
	for {
		bundle, err := c.dial(ctx)
		if err == nil {
			c.cur = bundle
			go c.egressProbeLoop()
			return c, nil
		}
		log.Printf("MASQUE 连接失败（%v），%s 后重试 ...", err, backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-timer.C:
		case <-lifeCtx.Done():
			timer.Stop()
			lifeStop()
			return nil, net.ErrClosed
		case <-ctx.Done():
			// 外部（Android 桥）取消装配：立即中止无限重试，不再等下一个
			// 退避周期——否则用户点停止后拨号仍一直重连（v0.5.10 反馈）。
			timer.Stop()
			lifeStop()
			return nil, context.Cause(ctx)
		}
		if backoff < reconnectRetryMax {
			backoff *= 2
			if backoff > reconnectRetryMax {
				backoff = reconnectRetryMax
			}
		}
	}
}

// dialAddr tries each candidate edge address in turn, starting from the one that
// last worked, and returns the first connection that reaches H3 SETTINGS AND
// passes the international egress probe.
func (c *MasqueClient) dial(ctx context.Context) (*connBundle, error) {
	var errs []string
	n := len(c.edgeAddrs)
	for i := 0; i < n; i++ {
		idx := (c.addrIdx + i) % n
		addr := c.edgeAddrs[idx]

		bundle, err := c.dialAddr(ctx, addr)
		if err == nil {
			// 国际出口探测：验证该边缘能否连通境外目标。
			// 避免国内边缘节点国际出口受限/故障（握手成功但境外流量被重置）。
			if err := c.probeEgress(ctx, bundle); err != nil {
				log.Printf("边缘 %s 国际出口探测失败（%v），尝试下一个 ...", addr, err)
				bundle.close("egress probe failed")
				errs = append(errs, fmt.Sprintf("%s: egress probe failed: %v", addr, err))
				continue
			}
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

// probeInternationalEgress 在指定 bundle 上做一次到 probeEgressTarget
// （8.8.8.8:443，WARP 边缘网内必达的境外目标）的 H3 CONNECT 探测，验证该
// 边缘的国际出口真的可用——握手成功但境外流量被掐（国内边缘节点国际出口
// 受限/故障）会让后续所有用户 CONNECT 在边缘侧被重置，拨号时探测一次就能
// 在选边缘阶段排除它们。成功返回 nil（探测流已释放），失败返回错误。
//
// 直接在传入 bundle 上开流并做 CONNECT 交换，绝不触碰 openRequestStream /
// reconnect：初始拨号时 c.cur 尚未安装（currentConnection 返回 ErrClosed），
// 走 establishCONNECT 会触发 reconnect → runReconnect → dial →
// probeInternationalEgress 的无限递归。探测只关心"CONNECT 能否建立"，成功
// 立即 releaseStream 归还边缘并发流配额；失败由调用方决定丢弃 bundle 或
// 标记 dead。
func (c *MasqueClient) probeInternationalEgress(ctx context.Context, bundle *connBundle) error {
	if bundle == nil || bundle.h3Client == nil {
		return errors.New("国际出口探测：bundle 未就绪")
	}
	req := &http.Request{
		Method: "CONNECT",
		Host:   probeEgressTarget,
		URL:    &url.URL{Scheme: "https", Host: probeEgressTarget},
		Header: make(http.Header),
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	probeCtx, cancel := context.WithTimeout(ctx, probeEgressTimeout)
	defer cancel()
	stream, err := bundle.h3Client.OpenRequestStream(probeCtx)
	if err != nil {
		return fmt.Errorf("国际出口探测开流失败：%w", err)
	}
	defer releaseStream(stream)
	resp, err := connectThroughEdge(stream, req, connectDeadline(probeCtx, probeEgressTimeout))
	if err != nil {
		return fmt.Errorf("国际出口探测 %s 失败：%w", probeEgressTarget, err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("国际出口探测 %s 返回 %d", probeEgressTarget, resp.StatusCode)
	}
	return nil
}

// probeEgress 是国际出口探测的统一入口：生产走 probeInternationalEgress，
// 测试可注入 probeFn 假实现（egressProbeLoop 与 dial 共用）。
func (c *MasqueClient) probeEgress(ctx context.Context, bundle *connBundle) error {
	if c.probeFn != nil {
		return c.probeFn(ctx, bundle)
	}
	return c.probeInternationalEgress(ctx, bundle)
}

// egressProbeLoop 运行期活性探测：每 egressProbeInterval 做一次
// probeEgressOnce，把静默死会话（KeepAlive 往返仍在但出口已坏或路径被掐）
// 的发现从"下一次用户 CONNECT 超时"提前到探测周期内。lifeCtx 取消
// （Close）即退出。
func (c *MasqueClient) egressProbeLoop() {
	ticker := time.NewTicker(egressProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.probeEgressOnce()
		case <-c.lifeCtx.Done():
			return
		}
	}
}

// probeEgressOnce 对当前连接做一次国际出口探测；失败即置 dead 并唤醒重连
// 航班——不等用户请求在死连接上白等（debugdiag：隧道被掐后 m.youtube.com
// 连续 2 条 connection reset by peer、dn=0）。无当前连接（重连中/已关闭）
// 时跳过。
func (c *MasqueClient) probeEgressOnce() {
	bundle, err := c.currentConnection()
	if err != nil {
		return // 无当前连接（重连中/已关闭），下轮再探
	}
	c.probeEgressOn(bundle)
}

// probeEgressOn 对指定 bundle 做一次国际出口探测，并按连续失败阈值决定是否
// retire：单次失败（手机网络 UDP 抖动 / 边缘偶发慢响应）只记数不拆共享连接
// （见 probeFailureThreshold 注释——探针与用户流共用同一 QUIC 连接，一次瞬时
// 毛刺拆线会把所有在途下载一起拖死），连续 probeFailureThreshold 次失败才
// retire+reconnect，不等用户请求在死连接上白等（v0.5.27 的恢复目的保留）。
// 独立成方法便于单测：probeEgressOnce 的 currentConnection 需要真实
// quic.Conn，无法在单测中构造（与 handleProbeFailure 独立成方法的理由相同）。
func (c *MasqueClient) probeEgressOn(bundle *connBundle) {
	perr := c.probeEgress(c.lifeCtx, bundle)
	if perr != nil {
		if bundle.noteProbeFailure() < probeFailureThreshold {
			log.Printf("运行期出口探测瞬时失败（%v），连续 %d 次失败后判定连接死亡并重连",
				perr, probeFailureThreshold)
			return
		}
		c.handleProbeFailure(bundle, perr)
		return
	}
	bundle.noteProbeSuccess()
}

// noteProbeFailure 记录一次探测失败，返回当前连续失败次数。
func (b *connBundle) noteProbeFailure() int {
	b.healthMu.Lock()
	defer b.healthMu.Unlock()
	b.probeFailures++
	return b.probeFailures
}

// noteProbeSuccess 清空连续探测失败计数。
func (b *connBundle) noteProbeSuccess() {
	b.healthMu.Lock()
	b.probeFailures = 0
	b.healthMu.Unlock()
}

// handleProbeFailure 处理探测失败：置 dead（黑洞路径下 quic.Context() 未
// Done 时后继续复用会白等 10s）→ retire → 唤醒重连航班，不等用户请求在
// 死连接上触发失败。独立成方法便于单测（currentConnection 需要真实
// quic.Conn，无法在单元测试中构造）。
func (c *MasqueClient) handleProbeFailure(bundle *connBundle, err error) {
	log.Printf("运行期出口探测失败（%v），标记连接死亡并重连", err)
	bundle.dead.Store(true)
	_ = c.retireConnection(bundle)
	ctx, cancel := context.WithTimeout(context.Background(), reconnectRetryMax*4)
	defer cancel()
	_ = c.reconnect(ctx, bundle)
}

func (c *MasqueClient) dialAddr(ctx context.Context, edgeAddr string) (*connBundle, error) {
	log.Printf("QUIC 拨号 %s（SNI=%s）...", edgeAddr, c.tlsConfig.ServerName)

	udpAddr, err := net.ResolveUDPAddr("udp", edgeAddr)
	if err != nil {
		return nil, fmt.Errorf("解析边缘地址 %s 失败：%w", edgeAddr, err)
	}

	// Bind the local socket in the same address family as the edge. Use an
	// explicit udp4/udp6 so the socket is not a dual-stack one: "udp" + an
	// IPv4-mapped address routes IPv4 targets through the IPv6 socket and table,
	// and on hosts without working IPv6 the kernel answers ENETUNREACH — while
	// a dedicated udp4 socket succeeds. debugdiag: 33 × "write udp [::]:X->
	// 162.159.198.2:4443: sendmsg: network is unreachable" killed the whole
	// shared QUIC connection at once (each H3 stream shares one UDP socket).
	listenFamily := "udp4"
	listenAddr := &net.UDPAddr{IP: net.IPv4zero}
	if udpAddr.IP.To4() == nil {
		listenFamily = "udp6"
		listenAddr = &net.UDPAddr{IP: net.IPv6zero}
	}
	udpConn, err := net.ListenUDP(listenFamily, listenAddr)
	if err != nil {
		return nil, fmt.Errorf("监听 UDP 失败：%w", err)
	}

	// Android：VpnService.establish() 后应用自身的新 socket 也走 TUN（未
	// protect 时），而 TUN 在拨号成功后才被读取——ClientHello 会滞留 tun 里
	// 导致所有边缘握手超时。protect() 把拨号 socket 豁免出 VPN 路由，走物理
	// 网络。桌面/CLI 无此问题（socketProtector 为 nil）。
	if socketProtector != nil {
		rawConn, cerr := udpConn.SyscallConn()
		if cerr == nil {
			_ = rawConn.Control(func(fd uintptr) {
				if perr := socketProtector(int(fd)); perr != nil {
					log.Printf("⚠ 保护拨号 socket（fd=%d）失败：%v", int(fd), perr)
				}
			})
		} else {
			log.Printf("⚠ 获取拨号 socket 原始 fd 失败：%v", cerr)
		}
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
	// dead 置位 = 连接级故障已被观测（noteDeadStream / 运行期探测）：黑洞
	// 路径下 quic.Context() 可能仍未 Done，但继续复用只会让每个请求在死
	// 连接上白等 10s CONNECT 超时。立即判失效，让 openRequestStream 加入
	// 重连航班。
	if c.cur.dead.Load() {
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
// to a CONNECT exchange failure. A non-timeout error (e.g. the socket was
// closed) is connection-level and recovers immediately. For a timeout, require
// several failures in one short window before declaring the shared H3 session
// bad: a single unreachable target (e.g. an IPv6 address on an IPv4-only
// physical network) times out with no new QUIC packets during the exchange —
// that is target-level failure, not a path blackhole, and must not tear down
// the shared connection (v0.5.21 real-device: one IPv6 target's CONNECT timeout
// retired the bundle and every concurrent flow died with "use of closed network
// connection"). Distinct targets were originally required, but browser retries
// hammer one host, so counting failures (v0.5.23) detects the blackhole without
// resurrecting the collateral teardown (see noteProgressingCONNECTFailure).
func (b *connBundle) connectFailureRequiresReconnect(err, callerErr error, target string, packetsBefore uint64) bool {
	if !shouldReconnectH3(err, callerErr) {
		return false
	}
	// 可辨别的连接级错误（quic TransportError/IdleTimeout/ApplicationError
	// /StatelessReset）→ 连接本身已死，立即重连（v0.5.27 快速恢复语义）。
	if isConnectionLevelError(err) {
		return true
	}
	// 裸 net.ErrClosed：共享连接已被他人 retire/换代/关闭，本条 CONNECT 只是
	// 被拖累——不重复决策，也不计入观察窗（批量死亡时每一条并发流都读到它，
	// 逐条计数会污染窗口）。
	if errors.Is(err, net.ErrClosed) {
		return false
	}
	// 其余（超时、对端 reset 等）可能只是单个目标不可达：单次不拆共享连接
	// （v0.5.21 教训），窗口内累计 connectFailureTargets 次才判定路径黑洞
	// （v0.5.23 语义——浏览器对同一站点的并发重试不会被 distinct 去重抹掉）。
	return b.noteProgressingCONNECTFailure(target, time.Now())
}

// noteProgressingCONNECTFailure counts a CONNECT timeout towards the
// failure-window threshold (connectFailureTargets failures in
// connectFailureWindow). Counting — not distinct-target de-duplication —
// matters because a browser retries the same few hosts concurrently: with
// distinct-only accounting, repeated failures of one host never reach the
// threshold and a blackholed shared session is never recovered (v0.5.23
// real-device: 2 targets × 2 failures each = distinct 2 < 3, foreign traffic
// dead until app restart). A single failure still stays scoped to its stream;
// the second failure in the window (same or different target) crosses the
// threshold, after which establishCONNECT retires the dead bundle and
// reconnects.
func (b *connBundle) noteProgressingCONNECTFailure(target string, now time.Time) bool {
	b.healthMu.Lock()
	defer b.healthMu.Unlock()
	if b.failureSince.IsZero() || now.Sub(b.failureSince) > connectFailureWindow {
		b.failureSince = now
		b.failureTargets = make(map[string]int)
	}
	b.failureTargets[target]++
	var total int
	for _, n := range b.failureTargets {
		total += n
	}
	return total >= connectFailureTargets
}

// noteStreamFailure 记录一条非连接级流错误；窗口内（connectFailureWindow）
// 累计 connectFailureTargets 次返回 true（调用方据此判定共享连接死亡并
// retire）。与 CONNECT 失败窗口分开计数：流错误与 CONNECT 失败是独立信号，
// 混在一起会互相污染阈值。窗口靠超时自然衰减（无成功路径重置——读路径每
// 包触发的重置会把锁打进热路径）。
func (b *connBundle) noteStreamFailure() bool {
	b.healthMu.Lock()
	defer b.healthMu.Unlock()
	if b.streamFailureSince.IsZero() || time.Since(b.streamFailureSince) > connectFailureWindow {
		b.streamFailureSince = time.Now()
		b.streamFailureCount = 0
	}
	b.streamFailureCount++
	return b.streamFailureCount >= connectFailureTargets
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

// isClosed reports whether Close has run. closed is written under connMu, so it
// must be read under connMu too.
func (c *MasqueClient) isClosed() bool {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.closed
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
