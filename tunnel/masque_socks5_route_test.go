package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"warp/route"
)

// newTestMasqueClient 构造一个无隧道状态的最小客户端：lifeCtx 可取消以便
// 重连 goroutine 退出，edgeAddrs 为空使隧道拨号必然失败（且不会触碰
// tlsConfig）。与 NewMasqueClient 的字段初始化保持一致。
func newTestMasqueClient(t *testing.T) *MasqueClient {
	t.Helper()
	lifeCtx, lifeStop := context.WithCancel(context.Background())
	t.Cleanup(lifeStop)
	return &MasqueClient{
		lifeCtx:   lifeCtx,
		lifeStop:  lifeStop,
		dnsCache:  make(map[string]dnsCacheEntry),
		dnsFlight: make(map[string]*dnsFlightResult),
	}
}

// startEchoServer 启动 127.0.0.1 上的 TCP echo 服务，测试结束时自动关闭。
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
				io.Copy(c, c)
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

// serveSOCKS5 在 net.Pipe 上运行一次 HandleSOCKS5，返回客户端侧连接。
// 测试结束时关闭客户端侧连接并等待 handler 退出，避免 goroutine 泄漏。
func serveSOCKS5(t *testing.T, client *MasqueClient, cfg SOCKS5Config) net.Conn {
	t.Helper()
	cc, sc := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.HandleSOCKS5(ctx, sc, cfg)
	}()
	t.Cleanup(func() {
		cc.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Log("HandleSOCKS5 未在 5 秒内退出")
		}
	})
	cc.SetDeadline(time.Now().Add(10 * time.Second))
	return cc
}

// socks5ConnectReq 构造 SOCKS5 CONNECT 请求（无认证，IPv4/IPv6/域名地址类型）。
func socks5ConnectReq(target string) ([]byte, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return nil, fmt.Errorf("拆解目标 %q 失败：%w", target, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 0 || port > 65535 {
		return nil, fmt.Errorf("非法端口 %q", portStr)
	}
	req := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, 0x01)
			req = append(req, v4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	req = append(req, byte(port>>8), byte(port))
	return req, nil
}

// socks5Handshake 完成无认证握手并发出 CONNECT 请求，返回响应状态码
// （0x00 成功，其余为 SOCKS5 错误码）。
func socks5Handshake(t *testing.T, cc net.Conn, target string) (byte, error) {
	t.Helper()
	if _, err := cc.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return 0, err
	}
	greet := make([]byte, 2)
	if _, err := io.ReadFull(cc, greet); err != nil {
		return 0, fmt.Errorf("读取握手响应失败：%w", err)
	}
	if greet[0] != 0x05 || greet[1] != 0x00 {
		return 0, fmt.Errorf("握手响应异常：%v", greet)
	}
	req, err := socks5ConnectReq(target)
	if err != nil {
		return 0, err
	}
	if _, err := cc.Write(req); err != nil {
		return 0, err
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(cc, reply); err != nil {
		return 0, fmt.Errorf("读取 CONNECT 响应失败：%w", err)
	}
	if reply[0] != 0x05 {
		return 0, fmt.Errorf("非 SOCKS5 响应：%v", reply)
	}
	return reply[1], nil
}

// TestSOCKS5RouteDirect 验证显式命中 direct 时本地直连 echo 服务并完成双向转发。
func TestSOCKS5RouteDirect(t *testing.T) {
	echo := startEchoServer(t)
	var called bool
	cfg := SOCKS5Config{
		RouteFunc: func(host string, ip netip.Addr) (string, bool) {
			called = true
			if host != "127.0.0.1" {
				t.Errorf("RouteFunc 收到主机名 %q，期望 127.0.0.1", host)
			}
			if ip.IsValid() {
				t.Errorf("RouteFunc 收到已解析 IP %v，期望 netip.Addr{}", ip)
			}
			return route.ActionDirect, true
		},
	}
	cc := serveSOCKS5(t, newTestMasqueClient(t), cfg)

	code, err := socks5Handshake(t, cc, echo.Addr().String())
	if err != nil {
		t.Fatalf("SOCKS5 握手失败：%v", err)
	}
	if code != 0x00 {
		t.Fatalf("期望 CONNECT 成功（0x00），收到 0x%02x", code)
	}
	if !called {
		t.Fatal("RouteFunc 未被调用")
	}

	if _, err := cc.Write([]byte("hello")); err != nil {
		t.Fatalf("发送数据失败：%v", err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(cc, buf); err != nil {
		t.Fatalf("读取回显失败：%v", err)
	}
	if string(buf) != "hello" {
		t.Fatalf("回显内容为 %q，期望 hello", buf)
	}
}

// TestSOCKS5RouteUnmatchedFallbackDirect 验证未命中规则（matched=false）时按
// 引擎的隐式 direct 兜底语义本地直连，而不是走隧道。
func TestSOCKS5RouteUnmatchedFallbackDirect(t *testing.T) {
	echo := startEchoServer(t)
	cfg := SOCKS5Config{
		RouteFunc: func(host string, ip netip.Addr) (string, bool) {
			return "", false // 未命中任何规则 → 隐式 direct 兜底
		},
	}
	cc := serveSOCKS5(t, newTestMasqueClient(t), cfg)

	code, err := socks5Handshake(t, cc, echo.Addr().String())
	if err != nil {
		t.Fatalf("SOCKS5 握手失败：%v", err)
	}
	if code != 0x00 {
		t.Fatalf("期望 CONNECT 成功（0x00，隐式 direct 兜底），收到 0x%02x", code)
	}

	if _, err := cc.Write([]byte("ping")); err != nil {
		t.Fatalf("发送数据失败：%v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(cc, buf); err != nil {
		t.Fatalf("读取回显失败：%v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("回显内容为 %q，期望 ping", buf)
	}
}

// TestSOCKS5NilRouteFuncUsesTunnel 验证 RouteFunc 为 nil 时行为与原来一致：
// 走隧道路径，且绝不会向目标发起本地直连。客户端预先 Close（无隧道状态）使
// 隧道路径立即失败返回 0x04，无需等待 35s 的 setup 预算。
func TestSOCKS5NilRouteFuncUsesTunnel(t *testing.T) {
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("启动目标监听失败：%v", err)
	}
	defer target.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := target.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	client := newTestMasqueClient(t)
	client.Close() // 无隧道状态：establishCONNECT 必然立即失败

	cc, sc := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.HandleSOCKS5(ctx, sc, SOCKS5Config{}) // RouteFunc == nil
	}()
	defer func() {
		cc.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Log("HandleSOCKS5 未在 5 秒内退出")
		}
	}()
	cc.SetDeadline(time.Now().Add(10 * time.Second))

	code, err := socks5Handshake(t, cc, target.Addr().String())
	if err != nil {
		t.Fatalf("SOCKS5 握手失败：%v", err)
	}
	if code != 0x04 {
		t.Fatalf("期望隧道路径失败返回 0x04，收到 0x%02x", code)
	}

	select {
	case conn := <-accepted:
		conn.Close()
		t.Fatal("隧道路径不应向目标建立本地直连")
	default:
	}
}
