package proxy

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// blackholeTunnelDial 模拟黑洞隧道：像 tunnel 侧 reconnect() 等重连航班那样
// 一直阻塞到 ctx 取消——真实路径在 DoH 解析、openRequestStream、reconnect
// 航班汇合点全都 select ctx.Done()，所以「带截止时间的 ctx」就是它的逃生门。
// 旧实现传的是服务器级 ctx（只在 Close 时取消），这道门永远不响。
//
// sawDeadline 记录收到的 ctx 是否带截止时间：直接钉住「看门狗装在 dial()」。
func blackholeTunnelDial(t *testing.T, sawDeadline *bool) func(context.Context, string) (net.Conn, error) {
	t.Helper()
	return func(ctx context.Context, target string) (net.Conn, error) {
		if _, ok := ctx.Deadline(); ok {
			*sawDeadline = true
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
}

// TestSOCKS5RequestCannotHangForever 锁定代理接单路径的看门狗（t_f0a0102b）：
// 黑洞连接下单次 SOCKS5 请求不得无限挂起，必须在 dialTimeout 内以错误收口。
//
// 现场形态（CT103 00:45→02:40，1h55m 无自愈）：warp-go 进程活着、照常监听
// 40000、照常打 "SOCKS5 CONNECT" 日志，但所有走 proxy 规则的请求永久挂在
// CONNECT 交换里。根因是 handleSOCKS5 用「服务器级 ctx」拨号——它只在
// Close 时取消，于是隧道侧 reconnect() 会一直等那个要等满长退避周期才起下一轮
// 探测的重连航班，永远等不到。
func TestSOCKS5RequestCannotHangForever(t *testing.T) {
	// 上界调到 300ms：与真实 35s 同语义（给 TunnelDial 一个有界的等待），
	// 但让单测在秒级跑完。
	prev := dialTimeout
	dialTimeout = 300 * time.Millisecond
	t.Cleanup(func() { dialTimeout = prev })

	var sawDeadline bool
	srv := NewServer(Config{TunnelDial: blackholeTunnelDial(t, &sawDeadline)})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()

	start := time.Now()
	// 客户端侧上界：dialTimeout 的 5 倍。再长就该由看门狗收口了。
	cc.SetDeadline(time.Now().Add(1500 * time.Millisecond))
	code := socks5Handshake(t, cc, 0x01, "127.0.0.1:9")
	elapsed := time.Since(start)

	if code == 0x00 {
		t.Fatal("黑洞连接不应握手成功")
	}
	if elapsed > dialTimeout+time.Second {
		t.Fatalf("SOCKS5 请求挂起 %s，超过看门狗上界 %s", elapsed, dialTimeout)
	}
	if !sawDeadline {
		t.Fatal("TunnelDial 收到的 ctx 必须带截止时间：没有它，隧道侧会永远等重连航班")
	}
}

// TestHTTPConnectCannotHangForever 锁定 HTTP CONNECT 走同一个看门狗：三个
// handler（SOCKS5 CONNECT / HTTP CONNECT / HTTP 转发）共用 dial() 这一个
// 漏斗，所以只需证明另外两条路径确实经由它，而不必各写一遍黑洞场景。
func TestHTTPConnectCannotHangForever(t *testing.T) {
	prev := dialTimeout
	dialTimeout = 300 * time.Millisecond
	t.Cleanup(func() { dialTimeout = prev })

	var sawDeadline bool
	srv := NewServer(Config{TunnelDial: blackholeTunnelDial(t, &sawDeadline)})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听失败：%v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	cc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("连接代理失败：%v", err)
	}
	defer cc.Close()

	start := time.Now()
	cc.SetDeadline(time.Now().Add(1500 * time.Millisecond))
	if _, err := cc.Write([]byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")); err != nil {
		t.Fatalf("发送 CONNECT 失败：%v", err)
	}
	buf := make([]byte, 128)
	n, err := cc.Read(buf)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("HTTP CONNECT 在 %s 后未收到任何响应（应回 5xx）：%v", elapsed, err)
	}
	if !strings.Contains(string(buf[:n]), " 502 ") {
		t.Fatalf("期望 502 Bad Gateway，实际 %q", buf[:n])
	}
	if elapsed > dialTimeout+time.Second {
		t.Fatalf("HTTP CONNECT 挂起 %s，超过看门狗上界 %s", elapsed, dialTimeout)
	}
}

// TestDialTimeoutBudgetCoversTunnelSetup 锁定看门狗上界与隧道自身的建连预算
// 相称：太短会在健康但偏慢的链路上把正常请求误杀成失败（用户看到的是
// 「代理时好时坏」）；太长则失去意义。隧道侧 socksSetupTimeout 同样是 35s
// （含 DNS ~5s 重试一次 + 两次 10s CONNECT 尝试），两边保持一致。
func TestDialTimeoutBudgetCoversTunnelSetup(t *testing.T) {
	if dialTimeout <= 0 {
		t.Fatal("dialTimeout 必须为正")
	}
	if dialTimeout < 20*time.Second {
		t.Fatalf("dialTimeout=%s 过短：不足以覆盖隧道建连（DNS + 两次 CONNECT 尝试），会误杀健康慢链路", dialTimeout)
	}
	if dialTimeout > 90*time.Second {
		t.Fatalf("dialTimeout=%s 过长：黑洞请求会挂太久，失去看门狗意义", dialTimeout)
	}
}
