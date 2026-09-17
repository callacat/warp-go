package tunnel

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockFrontProxyServer 是一个模拟 front proxy：**明文** TCP + HTTP CONNECT。
// 真实端点 cloudnproxy.baidu.com:443 就是明文（squid 不终止 TLS）——mock 用
// tls.NewListener 曾把「生产实现对明文端点多包一层 TLS」的缺陷掩盖掉（P0），
// 因此这里刻意用裸 TCP，并单独记录每条连接的第一行供「线上无 TLS」断言。
//
// wantHost 是 CONNECT 请求必须携带的 Host 头值（百度放行值 sptest.baidu.com）；
// 不一致时回 403（模拟百度对错误 Host 的拒绝，防 req.Host 未正确赋值的回归）。
// 注：不能用 http.ReadRequest 校验 Host——CONNECT 请求的 Host 头值会被
// ReadRequest 并入 req.Host 为请求行目标，Header map 里不留 "Host" 键。
// mock 端按手写 Header 行原始解析以拿到真实 Host 头。
type mockFrontProxyServer struct {
	addr string
	ln   net.Listener

	mu         sync.Mutex
	conns      int
	firstLines []string
	dials      int // 客户端拨号次数（由 countingDial 记录）
}

func newMockFrontProxy(t *testing.T, wantHost string) *mockFrontProxyServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	m := &mockFrontProxyServer{addr: ln.Addr().String(), ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			m.mu.Lock()
			m.conns++
			m.mu.Unlock()
			go m.handle(conn, wantHost)
		}
	}()
	t.Cleanup(m.Close)
	return m
}

func (m *mockFrontProxyServer) Close() { _ = m.ln.Close() }

func (m *mockFrontProxyServer) connCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conns
}

func (m *mockFrontProxyServer) firstLine(i int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= len(m.firstLines) {
		return ""
	}
	return m.firstLines[i]
}

// dialContext 返回把拨号重定向到 mock 的拨号缝，同时计数（预热连接也走它）。
func (m *mockFrontProxyServer) dialContext() func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		m.mu.Lock()
		m.dials++
		m.mu.Unlock()
		return d.DialContext(ctx, network, m.addr)
	}
}

func (m *mockFrontProxyServer) dialCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dials
}

func (m *mockFrontProxyServer) handle(conn net.Conn, wantHost string) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	// 手动解析请求行 + 头部块（不用 http.ReadRequest：对 CONNECT 请求，
	// 它会把 Host 头值并入 req.Host 为请求行目标，Header map 里不留
	// "Host" 键——无法从 http.Request 中拿到线上原始 Host 头）。
	reqLine, err := br.ReadString('\n')
	if err != nil {
		return
	}
	m.mu.Lock()
	m.firstLines = append(m.firstLines, strings.TrimRight(reqLine, "\r\n"))
	m.mu.Unlock()

	fields := strings.Fields(reqLine)
	if len(fields) < 1 || fields[0] != http.MethodConnect {
		fmt.Fprintf(conn, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	// 逐行读取头部，按 Host 头值校验
	host := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, rest, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Host") {
			host = strings.TrimSpace(rest)
		}
	}
	if host != wantHost {
		fmt.Fprintf(conn, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		return
	}
	// 模拟 CONNECT 成功（返回 200）
	resp := &http.Response{
		StatusCode: 200,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	resp.Write(conn)
	// 连接保持（模拟隧道建立后的双向数据）：把收到的字节原样回显，
	// 便于断言隧道数据面真的可用。
	buf := make([]byte, 1024)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		if _, err := conn.Write(buf[:n]); err != nil {
			return
		}
	}
}

func newTestDialer(t *testing.T, m *mockFrontProxyServer) *FrontProxyDialer {
	t.Helper()
	d := NewFrontProxyDialer(FrontProxyDialerConfig{
		Server:      "front.proxy.test:443", // 真实连接由 dialContext 重定向到 mock
		ConnectHost: "sptest.baidu.com",     // 百度实测放行 Host 值
		Token:       "test-token",
		UserAgent:   "test/1.0",
	})
	d.dialContext = m.dialContext()
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func TestFrontProxyDialer_DialTunnel_OK(t *testing.T) {
	m := newMockFrontProxy(t, "sptest.baidu.com")
	d := newTestDialer(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := d.DialTunnel(ctx, "162.159.198.2:443")
	if err != nil {
		t.Fatalf("DialTunnel failed: %v", err)
	}
	defer conn.Close()

	// 隧道数据面双向可用（mock 回显）
	if _, err := conn.Write([]byte("test")); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := conn.Read(echo); err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if string(echo) != "test" {
		t.Fatalf("隧道回显 = %q，期望 test", echo)
	}
}

// TestFrontProxyDialer_NoTLSOnWire 是 P0 的回归防线：对上线的字节流做断言，
// 第一个字节必须是明文 HTTP 请求行，绝不是 TLS record（0x16 0x03）。
// 原实现先 tls.Client 握手，真实端点直接回 `first record does not look like
// a TLS handshake`——开启 front proxy 后所有请求 502。
func TestFrontProxyDialer_NoTLSOnWire(t *testing.T) {
	m := newMockFrontProxy(t, "sptest.baidu.com")
	d := newTestDialer(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialTunnel(ctx, "162.159.198.2:443")
	if err != nil {
		t.Fatalf("DialTunnel failed: %v", err)
	}
	defer conn.Close()

	line := m.firstLine(0)
	if !strings.HasPrefix(line, "CONNECT 162.159.198.2:443 HTTP/1.1") {
		t.Fatalf("线上首行 = %q，期望明文 CONNECT 请求行（先做 TLS 握手会得到 TLS record）", line)
	}
	if strings.ContainsRune(line, 0x16) {
		t.Fatalf("线上首个消息像 TLS record，明文端点会拒：%q", line)
	}
}

func TestFrontProxyDialer_DialTunnel_ConnectRejected(t *testing.T) {
	// Host 不匹配 → mock 回 403
	m := newMockFrontProxy(t, "other.baidu.com")
	d := newTestDialer(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := d.DialTunnel(ctx, "target:443"); err == nil {
		t.Fatal("403 should cause error")
	}
}

// TestFrontProxyDialer_RetryOnceAfterDialFailure 验证失败短退避后重试一次
// （C4）：首次拨号失败、第二次成功时，隧道仍能建立。
func TestFrontProxyDialer_RetryOnceAfterDialFailure(t *testing.T) {
	m := newMockFrontProxy(t, "sptest.baidu.com")
	d := newTestDialer(t, m)

	var calls int
	var mu sync.Mutex
	inner := d.dialContext
	d.dialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			return nil, fmt.Errorf("模拟首次拨号失败")
		}
		return inner(ctx, network, addr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialTunnel(ctx, "162.159.198.2:443")
	if err != nil {
		t.Fatalf("重试后应成功，实际失败：%v", err)
	}
	defer conn.Close()

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 2 {
		t.Fatalf("拨号次数 = %d，期望 2（首次失败 + 重试一次）", got)
	}
}

// TestFrontProxyDialer_ConnectRejectionDoesNotBackOff 验证「代理可达但拒绝本次
// 目标」不推进长退避计数：百度代理按域名放行（实测 DoH 裸 IP 一律 503），若把
// 目标级 4xx/5xx 也算成「不可达」，一次目标被拒就会让整条保命通道停掉预热
// 30 分钟——故障从一个域名扩散到整条通道。
func TestFrontProxyDialer_ConnectRejectionDoesNotBackOff(t *testing.T) {
	m := newMockFrontProxy(t, "other.baidu.com") // Host 不匹配 → mock 回 403
	d := newTestDialer(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < 4; i++ {
		if _, err := d.DialTunnel(ctx, "162.159.198.2:443"); err == nil {
			t.Fatal("403 应返回错误")
		} else if !isFrontProxyRejected(err) {
			t.Fatalf("403 应被识别为目标级拒绝，实际 %v", err)
		}
	}

	if d.backing() {
		t.Fatal("目标级拒绝不应转入长退避")
	}
	d.policyMu.Lock()
	rounds := d.policy.rounds
	d.policyMu.Unlock()
	if rounds != 0 {
		t.Fatalf("连续失败轮数 = %d，期望 0（目标级拒绝不计入）", rounds)
	}
}

// TestFrontProxyDialer_LongBackoffStopsDenseRetry 验证连续 3 轮全失败后转长
// 退避并停止密集重试（对齐 dial_retry.go 的 dialRetryPolicy）：进入长退避后
// 每次 DialTunnel 只发起一次尝试，且不再后台预热连接。
func TestFrontProxyDialer_LongBackoffStopsDenseRetry(t *testing.T) {
	d := NewFrontProxyDialer(FrontProxyDialerConfig{
		Server:      "127.0.0.1:1", // 不可达
		ConnectHost: "sptest.baidu.com",
		Token:       "t",
	})
	t.Cleanup(func() { _ = d.Close() })

	var mu sync.Mutex
	attempts := 0
	d.dialContext = func(context.Context, string, string) (net.Conn, error) {
		mu.Lock()
		attempts++
		mu.Unlock()
		return nil, fmt.Errorf("模拟不可达")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 第 1 轮：2 次尝试（含重试一次）→ 连续失败 2 轮，未达阈值
	if _, err := d.DialTunnel(ctx, "162.159.198.2:443"); err == nil {
		t.Fatal("不可达时 DialTunnel 应失败")
	}
	// 第 2 轮：第 1 次尝试即达 3 轮阈值 → 不再重试
	if _, err := d.DialTunnel(ctx, "162.159.198.2:443"); err == nil {
		t.Fatal("不可达时 DialTunnel 应失败")
	}
	if !d.backing() {
		t.Fatal("连续 3 轮全失败后应进入长退避")
	}
	// 长退避期：每次调用只发起一次尝试，且不预热
	before := attempts
	if _, err := d.DialTunnel(ctx, "162.159.198.2:443"); err == nil {
		t.Fatal("不可达时 DialTunnel 应失败")
	}
	mu.Lock()
	after := attempts
	mu.Unlock()
	if after-before != 1 {
		t.Fatalf("长退避期拨号尝试 = %d，期望 1（不再密集重试）", after-before)
	}

	d.mu.Lock()
	warm := len(d.warm)
	d.mu.Unlock()
	if warm != 0 {
		t.Fatalf("长退避期预热连接数 = %d，期望 0", warm)
	}
}

// TestFrontProxyDialer_WarmPoolReusesPreconnected 验证预热池（C4）：一次成功
// 拨号后后台预拨一条连接，下一次 DialTunnel 直接复用它（不再拨号）。
func TestFrontProxyDialer_WarmPoolReusesPreconnected(t *testing.T) {
	m := newMockFrontProxy(t, "sptest.baidu.com")
	d := newTestDialer(t, m)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn1, err := d.DialTunnel(ctx, "162.159.198.2:443")
	if err != nil {
		t.Fatalf("首次 DialTunnel 失败：%v", err)
	}
	defer conn1.Close()

	// 等后台预热补池（异步）
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.mu.Lock()
		warm := len(d.warm)
		d.mu.Unlock()
		if warm > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if dialsAfterFirst := m.dialCount(); dialsAfterFirst != 2 {
		t.Fatalf("首次拨号 + 预热 = %d 次，期望 2", dialsAfterFirst)
	}

	// 封掉「现拨」路径：第二次 DialTunnel 只有复用预热连接才可能成功。
	connsBefore := m.connCount()
	d.dialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("测试禁止现拨")
	}
	conn2, err := d.DialTunnel(ctx, "162.159.198.2:443")
	if err != nil {
		t.Fatalf("第二次 DialTunnel 应从预热池取连接，实际失败：%v", err)
	}
	defer conn2.Close()
	// mock 端连接数不变 = 复用了预热连接（未新建 TCP）
	if got := m.connCount(); got != connsBefore {
		t.Fatalf("mock 端新建了连接（%d → %d），期望复用预热连接", connsBefore, got)
	}
}

func TestFrontProxyDialer_Close(t *testing.T) {
	d := NewFrontProxyDialer(FrontProxyDialerConfig{Server: "127.0.0.1:443"})
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close 应幂等：%v", err)
	}
	if _, err := d.DialTunnel(context.Background(), "target:443"); err == nil {
		t.Fatal("closed dialer should error")
	}
}

func TestFrontProxyDialer_EmptyTarget(t *testing.T) {
	d := NewFrontProxyDialer(FrontProxyDialerConfig{Server: "127.0.0.1:443"})
	t.Cleanup(func() { _ = d.Close() })
	if _, err := d.DialTunnel(context.Background(), "  "); err == nil {
		t.Fatal("空 CONNECT 目标应报错（蓝本同样校验）")
	}
}

// TestFrontProxyDialer_ResolveDNS 验证 ResolveDNS 走系统解析器（C3 取舍：本
// 模式 DNS 不经隧道、有泄漏，见 ResolveDNS 注释里的实测证据）且不依赖 front
// proxy 连接——DNS 不可用不该让解析跟着整条通道一起挂。
func TestFrontProxyDialer_ResolveDNS(t *testing.T) {
	d := NewFrontProxyDialer(FrontProxyDialerConfig{Server: "127.0.0.1:1"})
	t.Cleanup(func() { _ = d.Close() })
	d.dialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, fmt.Errorf("模拟 front proxy 不可达")
	}

	ip, err := d.ResolveDNS(context.Background(), "localhost")
	if err != nil {
		t.Fatalf("ResolveDNS 失败：%v", err)
	}
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("localhost 解析 = %v，期望回环地址", ip)
	}
}
