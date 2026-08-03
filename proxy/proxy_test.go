package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"warp/route"
)

// startProxy 在随机端口启动 mixed HTTP+SOCKS5 服务，返回监听地址。
// 测试结束时自动 Close（取消 context、关闭监听器、等待 handler 退出）。
func startProxy(t *testing.T, cfg Config) string {
	t.Helper()
	srv := NewServer(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

// startEchoServer 启动 127.0.0.1 上的 TCP echo 服务。
func startEchoServer(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 echo 服务失败：%v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// newTunnelEcho 构造一个模拟 WARP 隧道的 TunnelDial：每次调用返回 net.Pipe 的
// 一端，另一端由 goroutine 回显——真实隧道是字节流，net.Pipe 是等价的最小替身，
// 不需要真实监听/拨号。同时记录收到的目标地址。
func newTunnelEcho(t *testing.T) (dial func(ctx context.Context, targetAddr string) (net.Conn, error), targets *[]string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	return func(ctx context.Context, targetAddr string) (net.Conn, error) {
		cc, sc := net.Pipe()
		mu.Lock()
		got = append(got, targetAddr)
		mu.Unlock()
		go func() {
			defer sc.Close()
			_, _ = io.Copy(sc, sc)
		}()
		return cc, nil
	}, &got
}

// socks5ConnectReq 构造 SOCKS5 请求（无认证方法，IPv4 目标）。
func socks5ConnectReq(cmd byte, target string) []byte {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		panic(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		panic(err)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		panic("测试目标必须是 IPv4")
	}
	return append([]byte{0x05, cmd, 0x00, 0x01}, append(ip, byte(port>>8), byte(port))...)
}

// socks5Handshake 完成无认证握手并发起请求，返回响应状态码。
func socks5Handshake(t *testing.T, cc net.Conn, cmd byte, target string) byte {
	t.Helper()
	if _, err := cc.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("发送问候失败：%v", err)
	}
	greet := make([]byte, 2)
	if _, err := io.ReadFull(cc, greet); err != nil {
		t.Fatalf("读取握手响应失败：%v", err)
	}
	if greet[0] != 0x05 || greet[1] != 0x00 {
		t.Fatalf("握手响应异常：%v", greet)
	}
	if _, err := cc.Write(socks5ConnectReq(cmd, target)); err != nil {
		t.Fatalf("发送请求失败：%v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(cc, reply); err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	if reply[0] != 0x05 {
		t.Fatalf("非 SOCKS5 响应：%v", reply)
	}
	return reply[1]
}

// roundTrip 在已建立的隧道上写一条消息并读回回显。
func roundTrip(t *testing.T, cc net.Conn, msg string) {
	t.Helper()
	if _, err := cc.Write([]byte(msg)); err != nil {
		t.Fatalf("发送数据失败：%v", err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(cc, buf); err != nil {
		t.Fatalf("读取回显失败：%v", err)
	}
	if string(buf) != msg {
		t.Fatalf("回显内容为 %q，期望 %q", buf, msg)
	}
}

// TestSOCKS5RouteDirect 验证命中 direct 时本地直连 echo 服务并双向转发，
// Router 收到去掉端口的主机名与未解析的 IP。
func TestSOCKS5RouteDirect(t *testing.T) {
	echo := startEchoServer(t)
	var routerCalls int
	addr := startProxy(t, Config{
		Router: func(host string, ip netip.Addr) (string, bool) {
			routerCalls++
			if host != "127.0.0.1" {
				t.Errorf("Router 收到主机名 %q，期望 127.0.0.1", host)
			}
			if ip.IsValid() {
				t.Errorf("Router 收到已解析 IP %v，期望 netip.Addr{}", ip)
			}
			return route.ActionDirect, true
		},
	})

	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	if code := socks5Handshake(t, cc, 0x01, echo.Addr().String()); code != 0x00 {
		t.Fatalf("期望 CONNECT 成功（0x00），收到 0x%02x", code)
	}
	if routerCalls != 1 {
		t.Fatalf("Router 调用次数为 %d，期望 1", routerCalls)
	}
	roundTrip(t, cc, "hello")
}

// TestSOCKS5ProxyActionUsesTunnel 验证命中 proxy 时走 TunnelDial 而不是本地
// 直连——目标监听器绝不能收到连接。
func TestSOCKS5ProxyActionUsesTunnel(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动目标监听失败：%v", err)
	}
	defer target.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := target.Accept(); err == nil {
			accepted <- c
		}
	}()

	dial, targets := newTunnelEcho(t)
	addr := startProxy(t, Config{
		Router: func(host string, ip netip.Addr) (string, bool) {
			return route.ActionProxy, true
		},
		TunnelDial: dial,
	})

	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	if code := socks5Handshake(t, cc, 0x01, target.Addr().String()); code != 0x00 {
		t.Fatalf("期望 CONNECT 成功（0x00），收到 0x%02x", code)
	}
	roundTrip(t, cc, "via-tunnel")

	if len(*targets) != 1 || (*targets)[0] != target.Addr().String() {
		t.Fatalf("TunnelDial 收到的目标为 %v，期望 [%s]", *targets, target.Addr().String())
	}
	select {
	case c := <-accepted:
		c.Close()
		t.Fatal("命中 proxy 时不应向目标建立本地直连")
	default:
	}
}

// TestSOCKS5UnmatchedFallsBackDirect 验证未命中规则时按引擎的隐式 direct 兜底
// 本地直连，与 tunnel 的 RouteFunc 语义一致。
func TestSOCKS5UnmatchedFallsBackDirect(t *testing.T) {
	echo := startEchoServer(t)
	addr := startProxy(t, Config{
		Router: func(host string, ip netip.Addr) (string, bool) {
			return "", false // 未命中任何规则
		},
	})

	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	if code := socks5Handshake(t, cc, 0x01, echo.Addr().String()); code != 0x00 {
		t.Fatalf("期望 CONNECT 成功（0x00，隐式 direct 兜底），收到 0x%02x", code)
	}
	roundTrip(t, cc, "ping")
}

// TestSOCKS5RejectAction 验证命中 reject 规则时返回 SOCKS5 0x02
// （connection not allowed by ruleset），且绝不建立任何连接（隧道或直连）。
func TestSOCKS5RejectAction(t *testing.T) {
	echo := startEchoServer(t)
	var routerCalls int
	addr := startProxy(t, Config{
		Router: func(host string, ip netip.Addr) (string, bool) {
			routerCalls++
			return route.ActionReject, true
		},
	})

	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	if code := socks5Handshake(t, cc, 0x01, echo.Addr().String()); code != 0x02 {
		t.Fatalf("期望 CONNECT 被拒（0x02），收到 0x%02x", code)
	}
	if routerCalls != 1 {
		t.Fatalf("Router 调用次数为 %d，期望 1", routerCalls)
	}
	// 目标 echo 服务绝不能收到任何连接（reject 不建立隧道也不直连）。
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := echo.Accept(); err == nil {
			accepted <- c
		}
	}()
	select {
	case c := <-accepted:
		c.Close()
		t.Fatal("命中 reject 时目标不应收到连接")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestHTTPConnectReject 验证命中 reject 规则时 HTTP CONNECT 返回 403。
func TestHTTPConnectReject(t *testing.T) {
	addr := startProxy(t, Config{
		Router: func(host string, ip netip.Addr) (string, bool) {
			return route.ActionReject, true
		},
	})

	status := httpConnect(t, addr, "ads.example.com:443")
	if !strings.Contains(status, "403") {
		t.Fatalf("期望 HTTP 403 Forbidden，收到 %q", status)
	}
}

// TestSOCKS5NilRouterUsesTunnel 验证 Router 为 nil 时行为与上游一致：全部走
// 隧道，绝不本地直连。
func TestSOCKS5NilRouterUsesTunnel(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动目标监听失败：%v", err)
	}
	defer target.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := target.Accept(); err == nil {
			accepted <- c
		}
	}()

	dial, targets := newTunnelEcho(t)
	addr := startProxy(t, Config{TunnelDial: dial}) // Router == nil

	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	if code := socks5Handshake(t, cc, 0x01, target.Addr().String()); code != 0x00 {
		t.Fatalf("期望 CONNECT 成功（0x00），收到 0x%02x", code)
	}
	roundTrip(t, cc, "tunnel-only")

	if len(*targets) != 1 {
		t.Fatalf("TunnelDial 调用次数为 %d，期望 1", len(*targets))
	}
	select {
	case c := <-accepted:
		c.Close()
		t.Fatal("Router 为 nil 时不应本地直连")
	default:
	}
}

// httpConnect 通过代理建立 HTTP CONNECT 隧道，返回状态行内容（如 "HTTP/1.1 200"）。
func httpConnect(t *testing.T, addr, target string) string {
	t.Helper()
	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	t.Cleanup(func() { cc.Close() })
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	if _, err := fmt.Fprintf(cc, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		t.Fatalf("发送 CONNECT 失败：%v", err)
	}
	status, err := bufio.NewReader(cc).ReadString('\n')
	if err != nil {
		t.Fatalf("读取状态行失败：%v", err)
	}
	return strings.TrimSpace(status)
}

// TestHTTPConnect 验证同一端口的 HTTP CONNECT 走 TunnelDial（命中 proxy 规则）。
func TestHTTPConnect(t *testing.T) {
	dial, targets := newTunnelEcho(t)
	addr := startProxy(t, Config{
		Router: func(host string, ip netip.Addr) (string, bool) {
			return route.ActionProxy, true
		},
		TunnelDial: dial,
	})

	status := httpConnect(t, addr, "example.com:443")
	if !strings.Contains(status, "200") {
		t.Fatalf("期望 HTTP/1.1 200，收到 %q", status)
	}
	if len(*targets) != 1 || (*targets)[0] != "example.com:443" {
		t.Fatalf("TunnelDial 收到的目标为 %v，期望 [example.com:443]", *targets)
	}
}

// TestHTTPForward 验证同一端口的非 CONNECT 请求被改写为 origin-form 并转发到
// 本地直连的 HTTP 服务。
func TestHTTPForward(t *testing.T) {
	httpSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动 HTTP 服务失败：%v", err)
	}
	go func() { _ = httpSrv.Serve(ln) }()
	t.Cleanup(func() { _ = httpSrv.Close() })

	addr := startProxy(t, Config{
		Router: func(host string, ip netip.Addr) (string, bool) {
			return route.ActionDirect, true
		},
	})

	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	// 代理形式：absolute-URI。
	if _, err := fmt.Fprintf(cc, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr(), ln.Addr()); err != nil {
		t.Fatalf("发送 GET 失败：%v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(cc), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，收到 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败：%v", err)
	}
	if string(body) != "ok" {
		t.Fatalf("响应体为 %q，期望 ok", body)
	}
}

// TestMixedPortSniffing 验证首字节嗅探：同一监听端口同时服务 SOCKS5、
// HTTP CONNECT 与 HTTP 转发，互不干扰。
func TestMixedPortSniffing(t *testing.T) {
	echo := startEchoServer(t)
	dial, _ := newTunnelEcho(t)
	addr := startProxy(t, Config{
		Router: func(host string, ip netip.Addr) (string, bool) {
			return route.ActionDirect, true
		},
		TunnelDial: dial,
	})

	// SOCKS5（首字节 0x05）。
	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	cc.SetDeadline(time.Now().Add(10 * time.Second))
	if code := socks5Handshake(t, cc, 0x01, echo.Addr().String()); code != 0x00 {
		t.Fatalf("SOCKS5 期望 0x00，收到 0x%02x", code)
	}
	roundTrip(t, cc, "socks5")
	cc.Close()

	// HTTP CONNECT（首字节 'C'）。
	if status := httpConnect(t, addr, "example.com:443"); !strings.Contains(status, "200") {
		t.Fatalf("HTTP CONNECT 期望 200，收到 %q", status)
	}
}

// TestHTTPForwardWebSocketUpgrade 验证 WebSocket 升级请求的逐跳头被保留：
// handleHTTPForward 不能剥掉 Connection: Upgrade 与 Upgrade: websocket，
// 否则上游收到普通 GET 无法完成 101 握手（netbirdio/netbird #6190 同款坑）。
// 用裸 TCP 上游捕获代理转发的原始请求字节，回 101 后验证客户端能收到
// 握手成功响应——证明升级路径是长连接双向流而非被拆掉的普通转发。
func TestHTTPForwardWebSocketUpgrade(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动上游监听失败：%v", err)
	}
	defer upstream.Close()

	gotReq := make(chan string, 1)
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		gotReq <- string(buf[:n])
		// 回 101 升级成功响应，之后保持连接等待客户端数据。
		_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\n"+
			"Upgrade: websocket\r\n"+
			"Connection: Upgrade\r\n"+
			"Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\n")
		_, _ = io.Copy(io.Discard, c)
	}()

	addr := startProxy(t, Config{
		Router: func(host string, ip netip.Addr) (string, bool) {
			return route.ActionDirect, true
		},
	})

	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	// 代理形式 absolute-URI 的 WS 升级请求。
	host := upstream.Addr().String()
	req := "GET http://" + host + "/ws/terminal/test HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Connection: Upgrade\r\n" +
		"Upgrade: websocket\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(cc, req); err != nil {
		t.Fatalf("发送 WS 升级请求失败：%v", err)
	}

	// 上游必须收到升级头（Connection/Upgrade/Sec-WebSocket-* 原样透传）。
	// 注：Go 的 http.Request.Write 会把头名规范化为标准形式（如
	// Sec-WebSocket-Key → Sec-Websocket-Key），HTTP 头大小写不敏感，
	// 头名统一小写比较；base64 值大小写敏感，原样比对。
	select {
	case raw := <-gotReq:
		lower := strings.ToLower(raw)
		for _, want := range []string{
			"connection: upgrade",
			"upgrade: websocket",
			"sec-websocket-version: 13",
		} {
			if !strings.Contains(lower, want) {
				t.Errorf("上游收到的请求缺少 %q：\n%s", want, raw)
			}
		}
		if !strings.Contains(raw, "Sec-Websocket-Key: dGhlIHNhbXBsZSBub25jZQ==") {
			t.Errorf("上游收到的请求缺少 Sec-Websocket-Key 头：\n%s", raw)
		}
		if strings.Contains(lower, "connection: close") {
			t.Errorf("升级请求不应携带 Connection: close：\n%s", raw)
		}
		// 请求行必须已是 origin-form（代理已改写）。
		if !strings.Contains(raw, "GET /ws/terminal/test HTTP/1.1") {
			t.Errorf("上游请求行应为 origin-form：\n%s", raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("上游未收到升级请求")
	}

	// 客户端必须收到 101 升级成功（relay 把上游响应原样送回）。
	resp, err := http.ReadResponse(bufio.NewReader(cc), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("读取 101 响应失败：%v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("期望 101 Switching Protocols，收到 %d", resp.StatusCode)
	}
}

// TestHTTPForwardRejectsInvalidUpgrade 验证普通请求不受影响：无 Upgrade 头的
// GET 仍按原路径转发（不保留 Connection、设 close），行为与修复前一致。
func TestHTTPForwardRejectsInvalidUpgrade(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动上游监听失败：%v", err)
	}
	defer upstream.Close()

	gotReq := make(chan string, 1)
	go func() {
		c, err := upstream.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		if err != nil {
			return
		}
		gotReq <- string(buf[:n])
		_, _ = io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
	}()

	addr := startProxy(t, Config{
		Router: func(host string, ip netip.Addr) (string, bool) {
			return route.ActionDirect, true
		},
	})

	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	host := upstream.Addr().String()
	if _, err := fmt.Fprintf(cc, "GET http://%s/ HTTP/1.1\r\nHost: %s\r\n\r\n", host, host); err != nil {
		t.Fatalf("发送 GET 失败：%v", err)
	}

	select {
	case raw := <-gotReq:
		if strings.Contains(raw, "Connection: Upgrade") || strings.Contains(raw, "Upgrade:") {
			t.Errorf("普通请求不应透传升级头：\n%s", raw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("上游未收到请求")
	}

	resp, err := http.ReadResponse(bufio.NewReader(cc), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("读取响应失败：%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，收到 %d", resp.StatusCode)
	}
}

func TestUDPAssociateRelay(t *testing.T) {
	// 本地 UDP echo 服务。
	echoAddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("解析 UDP 地址失败：%v", err)
	}
	echo, err := net.ListenUDP("udp", echoAddr)
	if err != nil {
		t.Fatalf("启动 UDP echo 失败：%v", err)
	}
	defer echo.Close()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, err := echo.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = echo.WriteToUDP(buf[:n], src)
		}
	}()

	addr := startProxy(t, Config{AllowUDP: true})

	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	// 握手（问候）——不能复用 socks5Handshake：它把 10 字节的绑定响应一并读走，
	// UDP ASSOCIATE 的绑定地址就丢了。
	if _, err := cc.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("发送问候失败：%v", err)
	}
	greet := make([]byte, 2)
	if _, err := io.ReadFull(cc, greet); err != nil {
		t.Fatalf("读取握手响应失败：%v", err)
	}
	if greet[0] != 0x05 || greet[1] != 0x00 {
		t.Fatalf("握手响应异常：%v", greet)
	}
	// UDP ASSOCIATE（目标填 0.0.0.0:0，客户端尚不知道中继地址）。
	if _, err := cc.Write(socks5ConnectReq(0x03, "0.0.0.0:0")); err != nil {
		t.Fatalf("发送 UDP ASSOCIATE 请求失败：%v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(cc, reply); err != nil {
		t.Fatalf("读取绑定响应失败：%v", err)
	}
	if reply[0] != 0x05 || reply[1] != 0x00 || reply[3] != 0x01 {
		t.Fatalf("绑定响应异常：%v", reply)
	}
	bound := &net.UDPAddr{
		IP:   net.IP(reply[4:8]),
		Port: int(reply[8])<<8 | int(reply[9]),
	}

	client, err := net.DialUDP("udp", nil, bound)
	if err != nil {
		t.Fatalf("连接绑定地址失败：%v", err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(10 * time.Second))

	// 组装 SOCKS5 UDP 请求帧：RSV2+FRAG+ATYP+IPv4+端口+数据。
	echoIP := echo.LocalAddr().(*net.UDPAddr).IP.To4()
	echoPort := echo.LocalAddr().(*net.UDPAddr).Port
	frame := []byte{0x00, 0x00, 0x00, 0x01, echoIP[0], echoIP[1], echoIP[2], echoIP[3], byte(echoPort >> 8), byte(echoPort)}
	frame = append(frame, []byte("udp-echo")...)
	if _, err := client.Write(frame); err != nil {
		t.Fatalf("发送 UDP 帧失败：%v", err)
	}

	buf := make([]byte, 2048)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("读取回包失败：%v", err)
	}
	// 回包帧：RSV2+FRAG+ATYP+IPv4+端口+数据，数据从偏移 10 开始。
	if n < 10 || string(buf[10:n]) != "udp-echo" {
		t.Fatalf("回包内容异常：%q", buf[:n])
	}
}

// TestUDPAssociateDisabled 验证 AllowUDP=false 时 UDP ASSOCIATE 返回 0x07。
func TestUDPAssociateDisabled(t *testing.T) {
	addr := startProxy(t, Config{AllowUDP: false})

	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	if code := socks5Handshake(t, cc, 0x03, "0.0.0.0:0"); code != 0x07 {
		t.Fatalf("期望 0x07（命令不支持），收到 0x%02x", code)
	}
}

// TestSOCKS5Auth 验证配置了用户名/密码后，错误凭据被拒绝、正确凭据放行。
// Router 为 nil 时全部走隧道，故需提供 TunnelDial 让 CONNECT 成功。
func TestSOCKS5Auth(t *testing.T) {
	echo := startEchoServer(t)
	dial, _ := newTunnelEcho(t)
	addr := startProxy(t, Config{Username: "u", Password: "p", TunnelDial: dial})

	cc, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	// 错误密码。
	if _, err := cc.Write([]byte{0x05, 0x01, 0x02}); err != nil {
		t.Fatalf("发送问候失败：%v", err)
	}
	var greet [2]byte
	if _, err := io.ReadFull(cc, greet[:]); err != nil {
		t.Fatalf("读取握手响应失败：%v", err)
	}
	if greet[1] != 0x02 {
		t.Fatalf("期望要求认证（0x02），收到 0x%02x", greet[1])
	}
	if _, err := cc.Write([]byte{0x01, 0x01, 'u', 0x01, 'x'}); err != nil {
		t.Fatalf("发送凭据失败：%v", err)
	}
	var authReply [2]byte
	if _, err := io.ReadFull(cc, authReply[:]); err != nil {
		t.Fatalf("读取认证结果失败：%v", err)
	}
	if authReply[1] != 0x01 {
		t.Fatalf("错误密码期望认证失败（0x01），收到 0x%02x", authReply[1])
	}
	cc.Close()

	// 正确凭据。
	cc, err = net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()
	cc.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := cc.Write([]byte{0x05, 0x01, 0x02}); err != nil {
		t.Fatalf("发送问候失败：%v", err)
	}
	if _, err := io.ReadFull(cc, greet[:]); err != nil {
		t.Fatalf("读取握手响应失败：%v", err)
	}
	if _, err := cc.Write([]byte{0x01, 0x01, 'u', 0x01, 'p'}); err != nil {
		t.Fatalf("发送凭据失败：%v", err)
	}
	if _, err := io.ReadFull(cc, authReply[:]); err != nil {
		t.Fatalf("读取认证结果失败：%v", err)
	}
	if authReply[1] != 0x00 {
		t.Fatalf("正确密码期望认证成功（0x00），收到 0x%02x", authReply[1])
	}
	// 认证通过后服务器已越过问候阶段，直接发 CONNECT 请求（socks5Handshake
	// 会重复问候，服务器会把它误读为请求头而卡死）。
	if _, err := cc.Write(socks5ConnectReq(0x01, echo.Addr().String())); err != nil {
		t.Fatalf("发送 CONNECT 请求失败：%v", err)
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(cc, reply); err != nil {
		t.Fatalf("读取 CONNECT 响应失败：%v", err)
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		t.Fatalf("认证后 CONNECT 期望 0x00，收到 0x%02x", reply[1])
	}
	roundTrip(t, cc, "authed")
}
