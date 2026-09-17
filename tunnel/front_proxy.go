package tunnel

// front_proxy：百度中转 HTTP CONNECT 隧道拨号器（recvvkPP7whiIL）。
//
// 定位：作为 MasqueClient 的 TCP fallback——QUIC/UDP 被 ISP 干扰时的保命
// 备用通道。实现 dialer 接口（DialTunnel/ResolveDNS/Close），语义移植自
// x-tunnel internal/app/front_proxy.go（不抄实现）。
//
// 协议形态（2026-09-17 真实端点复测修正，P0）：cloudnproxy.baidu.com:443 是
// **明文 HTTP** 端点——443 只是端口号，squid 并不终止 TLS，对它做 TLS 握手
// 只会拿到 `tls: first record does not look like a TLS handshake`。原实现先
// TLS 握手再发 CONNECT，真实端点根本连不上（开启后所有请求 502）；蓝本
// x-tunnel 一直是明文 CONNECT。现按蓝本语义明文 CONNECT，隧道内的 TLS 由
// 上层流量自行完成（CONNECT 只是字节管道）。既有单测用 tls.NewListener 做
// mock，与真实端点协议不一致，把这个缺陷掩盖了——mock 已同步改为明文 TCP
// listener，并另补一条 env 门控的真实端点集成测试（front_proxy_live_test.go）。
//
// 健壮性（C4）：单条 TCP 无池/无重连在保命场景（QUIC 被干扰才启用，5 分钟
// 连不上即整体失败）不够。本实现补三件事：
//  ① 预热连接池：后台预拨一条到 front proxy 的 TCP 连接，省掉每次新建隧道的
//     握手往返（实测一次 TCP+CONNECT 往返 ~200ms）；
//  ② DialTunnel 失败短退避（frontProxyRetryWait）后重试一次（失败的那条连接
//     直接丢弃——代理端可能已半关）；
//  ③ 连续 3 轮全失败转长退避静默：复用 dial_retry.go 的 dialRetryPolicy，
//     停止密集重试与预热，调用方每次仍只发起一次尝试（不能像装配循环那样
//     把用户请求挂 30 分钟）。目标级拒绝（代理可达但对本次目标回 4xx/5xx）
//     不计入该计数——见 DialTunnel 注释。
//
// DNS（C3）：本模式 DNS 走系统解析器、不经隧道（有泄漏），原因与实测证据见
// ResolveDNS 注释；数据面 CONNECT 一律用域名形态（由代理侧解析）。

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// FrontProxyDialerConfig 从 core.FrontProxyConfig 映射的本地配置
// （避免 core→tunnel 反向依赖）。
type FrontProxyDialerConfig struct {
	Server      string // host:port（如 cloudnproxy.baidu.com:443）
	ConnectHost string // Host 头值（兜底 sptest.baidu.com）
	Token       string // X-T5-Auth token（空=不发该头；百度端点对缺失/错误 token 回 403）
	UserAgent   string // UA 头（空=默认 okhttp）
}

const (
	// frontProxyDialTimeout 单次 TCP 拨号到 front proxy 的超时。
	frontProxyDialTimeout = 10 * time.Second

	// frontProxyHandshakeTimeout 是 CONNECT 请求→响应往返的总超时（蓝本
	// WSHandshakeTimeout 同义）。只约束握手期，成功后清除 deadline——残留
	// deadline 会杀掉数据面的长连接。
	frontProxyHandshakeTimeout = 10 * time.Second

	// frontProxyPoolSize 是预热连接数，只预热 1 条：预热只值「省一次握手
	// 往返」，多条会平白占着故障网络里的半开连接。保命通道的可用性不依赖
	// 预热（池空即现拨）。
	frontProxyPoolSize = 1

	// frontProxyRetryWait 是 DialTunnel 失败后重试一次的短退避（契约 300ms–1s）。
	// 取区间下沿：单次 CONNECT 往返远快于 QUIC 拨号，退避久了只会拖慢恢复。
	frontProxyRetryWait = 300 * time.Millisecond

	// frontProxyDefaultConnectHost / frontProxyDefaultUA 是配置缺省值。
	// connect_host 兜底 sptest.baidu.com（实测放行值；Host=服务域名回 403）。
	frontProxyDefaultConnectHost = "sptest.baidu.com"
	frontProxyDefaultUA          = "okhttp/3.11.0"
)

// FrontProxyDialer 通过 HTTP CONNECT 隧道实现 dialer 接口。
type FrontProxyDialer struct {
	cfg    FrontProxyDialerConfig
	dialer net.Dialer

	// dialContext 是拨号缝：nil = 生产用 dialer.DialContext（测试注入假实现
	// 以免真实 socket）。
	dialContext func(ctx context.Context, network, addr string) (net.Conn, error)

	mu     sync.Mutex
	closed bool
	warm   []net.Conn // 已拨通、尚未 CONNECT 的代理连接（预热池）
	refill bool       // 后台补池进行中（防重复补）
	done   chan struct{}

	// policyMu 保护 policy/lastBackoff（与 mu 分开，避免锁序耦合：policy 的
	// 方法不会回调 mu）。
	policyMu    sync.Mutex
	policy      dialRetryPolicy
	lastBackoff time.Duration
}

// NewFrontProxyDialer 构造 TCP fallback 拨号器。构造期不发起任何网络动作
// （保命通道可能在装配期连通性很差，拨号推迟到首次 DialTunnel）。
func NewFrontProxyDialer(cfg FrontProxyDialerConfig) *FrontProxyDialer {
	return &FrontProxyDialer{
		cfg:    cfg,
		dialer: net.Dialer{Timeout: frontProxyDialTimeout},
		done:   make(chan struct{}),
	}
}

// DialTunnel 通过 front proxy 的明文 HTTP CONNECT 隧道建立到 target 的 TCP
// 连接。target 格式 host:port（主机名按原样写进 CONNECT 请求行，由百度代理
// 侧解析——本机不做 DNS，避免把域名解析成本地视图）。实现 dialer.DialTunnel。
func (d *FrontProxyDialer) DialTunnel(ctx context.Context, target string) (net.Conn, error) {
	if strings.TrimSpace(target) == "" {
		return nil, fmt.Errorf("front proxy CONNECT 目标为空")
	}
	if d.isClosed() {
		return nil, fmt.Errorf("front proxy dialer 已关闭")
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 && !d.sleep(ctx, frontProxyRetryWait) {
			return nil, lastErr // ctx 取消 / dialer 已关闭
		}
		conn, dialErr := d.takeConn(ctx)
		if dialErr != nil {
			lastErr = dialErr
		} else if tunnel, connErr := d.connectTunnel(ctx, conn, target); connErr == nil {
			d.noteSuccess()
			d.warmUp()
			return tunnel, nil
		} else {
			lastErr = connErr
		}
		// 代理可达、但对**本次目标**回 4xx/5xx（百度白名单/瞬时过载）：这是一次
		// 目标级拒绝，不是「front proxy 不可达」。若也计入连续失败，一次目标被拒
		// （实测 DoH 裸 IP 一律 503）就会把整条保命通道推进长退避、停掉预热 30
		// 分钟——故障从一个域名扩散到整条通道。故只按短退避重试一次，不推进计数。
		if isFrontProxyRejected(lastErr) {
			continue
		}
		// 记一轮失败；连续失败达阈值即转长退避——此后不再密集重试（对齐
		// dial_retry.go 的 dialRetryPolicy：3 轮全失败就停密集重试 + 静默）。
		if d.noteFailure() {
			break
		}
	}
	return nil, lastErr
}

// frontProxyRejectedError 表示「代理可达但拒绝了本次 CONNECT」——与拨号失败
// （TCP 不可达）区分开：前者不该推进长退避计数（见 DialTunnel 注释）。
type frontProxyRejectedError struct {
	status int
	target string
}

func (e *frontProxyRejectedError) Error() string {
	return fmt.Sprintf("CONNECT 被拒：HTTP %d（目标 %s）", e.status, e.target)
}

// isFrontProxyRejected 报告错误是否为「代理可达但拒绝本次 CONNECT」。
func isFrontProxyRejected(err error) bool {
	var rej *frontProxyRejectedError
	return errors.As(err, &rej)
}

// connectTunnel 在已拨通的 TCP 连接上发一次明文 HTTP CONNECT，成功返回隧道
// 连接。任何失败路径都会关闭该连接（不回收进池：代理端可能已半关，复用会
// 把一次故障放大成后续多次故障）。
func (d *FrontProxyDialer) connectTunnel(ctx context.Context, conn net.Conn, target string) (net.Conn, error) {
	connectHost := d.cfg.ConnectHost
	if connectHost == "" {
		connectHost = frontProxyDefaultConnectHost
	}
	ua := d.cfg.UserAgent
	if ua == "" {
		ua = frontProxyDefaultUA
	}

	// 只约束握手期，成功后清除（见 frontProxyHandshakeTimeout 注释）。
	deadline := time.Now().Add(frontProxyHandshakeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	if err := conn.SetDeadline(deadline); err != nil {
		conn.Close()
		return nil, fmt.Errorf("设置 CONNECT 超时失败：%w", err)
	}

	// Host 头必须设到 req.Host 而非 Header["Host"]：Go 标准库 Request.Write
	// 只用 req.Host（Header map 里的 "Host" 键会被丢弃），而百度代理以
	// connect_host 为放行依据（Host=服务域名会 403，CHANGELOG 已记录）。
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   connectHost, // 百度放行值（默认 sptest.baidu.com）
		Header: make(http.Header),
	}
	if d.cfg.Token != "" {
		// 空 token 不发该头：百度端点对缺失/错误 X-T5-Auth 回 403
		// （2026-09-17 实测：带 token 200 / 不带 403），配置校验在
		// core.ValidateFrontProxy 里已拦 enabled+空 token。
		req.Header.Set("X-T5-Auth", d.cfg.Token)
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("Connection", "keep-alive")

	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("写 CONNECT 请求失败：%w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("读 CONNECT 响应失败：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, &frontProxyRejectedError{status: resp.StatusCode, target: target}
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("清除 CONNECT 超时失败：%w", err)
	}

	log.Printf("✓ front proxy CONNECT 隧道建立 → %s", target)
	return &bufferedConn{Conn: conn, reader: br}, nil
}

// takeConn 取一条到 front proxy 的 TCP 连接：优先用预热池，池空则现拨。
func (d *FrontProxyDialer) takeConn(ctx context.Context) (net.Conn, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, fmt.Errorf("front proxy dialer 已关闭")
	}
	if n := len(d.warm); n > 0 {
		conn := d.warm[n-1]
		d.warm = d.warm[:n-1]
		d.mu.Unlock()
		return conn, nil
	}
	d.mu.Unlock()

	conn, err := d.dialProxy(ctx)
	if err != nil {
		return nil, fmt.Errorf("拨号 front proxy %s 失败：%w", d.cfg.Server, err)
	}
	return conn, nil
}

// dialProxy 拨一条到 front proxy 的裸 TCP 连接。
func (d *FrontProxyDialer) dialProxy(ctx context.Context) (net.Conn, error) {
	if d.dialContext != nil {
		return d.dialContext(ctx, "tcp", d.cfg.Server)
	}
	return d.dialer.DialContext(ctx, "tcp", d.cfg.Server)
}

// warmUp 后台补一条预热连接。长退避期不补（对齐 dial_retry.go：连续失败达
// 阈值即停止密集重试，不再对故障网络施压）；补池失败只静默丢弃——下一次
// DialTunnel 仍会现拨，可用性不依赖预热。
func (d *FrontProxyDialer) warmUp() {
	d.mu.Lock()
	if d.closed || d.refill || len(d.warm) >= frontProxyPoolSize || d.backing() {
		d.mu.Unlock()
		return
	}
	d.refill = true
	d.mu.Unlock()

	go func() {
		defer func() {
			d.mu.Lock()
			d.refill = false
			d.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), frontProxyDialTimeout)
		defer cancel()
		conn, err := d.dialProxy(ctx)
		if err != nil {
			return
		}
		d.mu.Lock()
		if d.closed || len(d.warm) >= frontProxyPoolSize {
			d.mu.Unlock()
			_ = conn.Close()
			return
		}
		d.warm = append(d.warm, conn)
		d.mu.Unlock()
	}()
}

// noteFailure 记一轮拨号失败，返回是否已进入长退避期。切入长退避只播报一次
// （对齐 dial_retry.go：边界日志一条，退避期内静默）。
func (d *FrontProxyDialer) noteFailure() bool {
	d.policyMu.Lock()
	defer d.policyMu.Unlock()
	wait, first := d.policy.afterFailure(d.lastBackoff)
	d.lastBackoff = wait
	if first {
		log.Printf("⚠ front proxy 连续 %d 轮不可达 → 转长退避（%s 后低频探测，期间静默）",
			dialRoundFailureLimit, retryLongBackoff)
	}
	return d.policy.backing()
}

// noteSuccess 一轮拨号成功：清零连续失败计数并回到快速节奏。
func (d *FrontProxyDialer) noteSuccess() {
	d.policyMu.Lock()
	defer d.policyMu.Unlock()
	d.policy.reset()
	d.lastBackoff = 0
}

// backing 报告当前是否处于长退避期。
func (d *FrontProxyDialer) backing() bool {
	d.policyMu.Lock()
	defer d.policyMu.Unlock()
	return d.policy.backing()
}

// ResolveDNS 用系统解析器解析 host。实现 dialer.ResolveDNS。
//
// ⚠ 本模式的 DNS **不走隧道（有泄漏）**，这是实测后的取舍（C3）：
// MasqueClient 的隧道内 DoH 在本端点上不可实现——① 百度代理按 SNI 过滤，
// CONNECT cloudflare-dns.com:443 回 200 后隧道内 TLS 一律 EOF（2026-09-17
// 实测 6/6 失败）；② DoH 裸 IP（1.1.1.1 / 1.0.0.1 / 162.159.36.1 /
// 162.159.46.1 / 8.8.8.8）在 CONNECT 阶段就被 503 拒。故退回系统 DNS，
// 并打一条日志明示泄漏（保命通道下可用性优先于 DNS 隐私）。
//
// 数据面不受影响：DialTunnel 把主机名原样写进 CONNECT 请求行、由百度代理侧
// 解析，本机不做数据面 DNS；日志只在真实发生解析时提示一次性质说明。
func (d *FrontProxyDialer) ResolveDNS(_ context.Context, host string) (net.IP, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("DNS 解析 %s 返回空", host)
	}
	// 与 MasqueClient 的 A 优先一致：IPv4 优先，无 IPv4 才用 IPv6。
	for _, cand := range ips {
		if cand.To4() != nil {
			return cand, nil
		}
	}
	return ips[0], nil
}

// sleep 等待 wait，ctx 取消或 dialer 关闭时提前返回 false。
func (d *FrontProxyDialer) sleep(ctx context.Context, wait time.Duration) bool {
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	case <-d.done:
		return false
	}
}

func (d *FrontProxyDialer) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// Close 关闭拨号器（幂等）：关闭信号唤醒等待中的退避，并关掉预热池里的连接。
func (d *FrontProxyDialer) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	warm := d.warm
	d.warm = nil
	close(d.done)
	d.mu.Unlock()

	for _, conn := range warm {
		_ = conn.Close()
	}
	return nil
}

// bufferedConn 包裹已读部分数据的连接。
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
