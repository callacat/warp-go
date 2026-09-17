package tunnel

// front proxy 的真实端点集成测试（env 门控）。
//
// 存在的理由（P0 教训）：单测用 mock 服务器，mock 的协议形态一旦与真实端点
// 不一致（原 mock 是 tls.NewListener，真实 cloudnproxy.baidu.com:443 是明文），
// 单测全绿也照样掩盖「生产实现连不上真实端点」。这条测试走真实网络，因此
// 默认跳过，只在显式开启时跑：
//
//	WARP_FRONT_PROXY_LIVE=1 \
//	WARP_FRONT_PROXY_TOKEN=<X-T5-Auth> \
//	  go test ./tunnel/ -run TestFrontProxyLive -v
//
// token 只从环境读，绝不写进源码/测试（C6）：百度端点对缺失 token 回 403
// （2026-09-17 实测：带 token 200 / 不带 403）。

import (
	"context"
	"crypto/tls"
	"os"
	"testing"
	"time"
)

// liveFrontProxyDialer 构造真实端点的拨号器；未开启门控或缺少 token 时跳过。
func liveFrontProxyDialer(t *testing.T) *FrontProxyDialer {
	t.Helper()
	if os.Getenv("WARP_FRONT_PROXY_LIVE") != "1" {
		t.Skip("真实端点集成测试默认跳过（置 WARP_FRONT_PROXY_LIVE=1 启用）")
	}
	token := os.Getenv("WARP_FRONT_PROXY_TOKEN")
	if token == "" {
		t.Skip("缺少 WARP_FRONT_PROXY_TOKEN（token 不入源码，见 C6）")
	}
	server := os.Getenv("WARP_FRONT_PROXY_SERVER")
	if server == "" {
		server = "cloudnproxy.baidu.com:443"
	}
	d := NewFrontProxyDialer(FrontProxyDialerConfig{
		Server:      server,
		ConnectHost: "sptest.baidu.com", // 实测放行值
		Token:       token,
	})
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// TestFrontProxyLive_DialTunnel 走真实百度端点建 CONNECT 隧道，并在隧道内对
// 目标做一次 TLS 握手——证明数据面双向可用（明文 CONNECT 的协议形态正确）。
func TestFrontProxyLive_DialTunnel(t *testing.T) {
	d := liveFrontProxyDialer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	target := "162.159.198.2:443" // CF 边缘（warp-go 的典型 CONNECT 目标）
	conn, err := d.DialTunnel(ctx, target)
	if err != nil {
		t.Fatalf("真实端点 DialTunnel 失败：%v", err)
	}
	defer conn.Close()

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName:         "cloudflare.com",
		InsecureSkipVerify: true, // 只验证隧道能承载完整 TLS 握手（数据面双向）
	})
	if err := tlsConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		t.Fatalf("隧道内 TLS 握手失败（数据面不通）：%v", err)
	}
	t.Logf("隧道内 TLS 握手成功：version=%#x cipher=%#x", tlsConn.ConnectionState().Version, tlsConn.ConnectionState().CipherSuite)
}

// TestFrontProxyLive_DialTunnelDomainTarget 走真实端点验证**域名形态**的
// CONNECT（数据面主路径）：百度代理按域名放行并按 SNI 过滤，裸 IP 目标部分
// 被 503（1.1.1.1 / 8.8.8.8 / 162.159.36.1），因此生产路径必须传域名。
func TestFrontProxyLive_DialTunnelDomainTarget(t *testing.T) {
	d := liveFrontProxyDialer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	conn, err := d.DialTunnel(ctx, "example.com:443")
	if err != nil {
		t.Fatalf("域名目标 DialTunnel 失败：%v", err)
	}
	defer conn.Close()

	tlsConn := tls.Client(conn, &tls.Config{ServerName: "example.com", MinVersion: tls.VersionTLS12})
	if err := tlsConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		t.Fatalf("隧道内 TLS 握手失败（数据面不通）：%v", err)
	}
	t.Logf("域名目标隧道内 TLS 握手成功：version=%#x", tlsConn.ConnectionState().Version)
}

// TestFrontProxyLive_ResolveDNS 验证 ResolveDNS 在真实环境可用。注意它走的是
// **系统解析器**（C3 取舍：隧道内 DoH 被该端点的 SNI 过滤挡住，实测 6/6 失败，
// 见 ResolveDNS 注释），这里只断言能拿到公网地址。
func TestFrontProxyLive_ResolveDNS(t *testing.T) {
	d := liveFrontProxyDialer(t)

	ip, err := d.ResolveDNS(context.Background(), "cloudflare.com")
	if err != nil {
		t.Fatalf("ResolveDNS 失败：%v", err)
	}
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLoopback() {
		t.Fatalf("解析结果 = %v，期望公网单播地址", ip)
	}
	t.Logf("系统解析 cloudflare.com → %s（本模式 DNS 不经隧道）", ip)
}
