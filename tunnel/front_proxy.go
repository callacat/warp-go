package tunnel

// front_proxy：百度中转 HTTP CONNECT 隧道拨号器（recvvkPP7whiIL）。
// 实现 dialer 接口（DialTunnel/ResolveDNS/Close），作为 MasqueClient 的
// TCP fallback——QUIC/UDP 被 ISP 干扰时的保命备用通道。
// 语义移植自 x-tunnel internal/app/front_proxy.go，不抄实现。

import (
	"bufio"
	"context"
	"crypto/tls"
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
	Token       string // X-T5-Auth token
	UserAgent   string // UA 头（空=默认）
}

// FrontProxyDialer 通过 HTTP CONNECT 隧道实现 dialer 接口。
type FrontProxyDialer struct {
	cfg    FrontProxyDialerConfig
	dialer net.Dialer
	mu     sync.Mutex
	closed bool
}

// NewFrontProxyDialer 构造 TCP fallback 拨号器。
func NewFrontProxyDialer(cfg FrontProxyDialerConfig) *FrontProxyDialer {
	return &FrontProxyDialer{
		cfg:    cfg,
		dialer: net.Dialer{Timeout: 10 * time.Second},
	}
}

// DialTunnel 通过 front proxy 的 HTTP CONNECT 隧道建立到 target 的 TCP 连接。
// target 格式 host:port（WARP 边缘地址）。实现 dialer.DialTunnel。
func (d *FrontProxyDialer) DialTunnel(ctx context.Context, target string) (net.Conn, error) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, fmt.Errorf("front proxy dialer 已关闭")
	}
	d.mu.Unlock()

	server := d.cfg.Server
	connectHost := d.cfg.ConnectHost
	if connectHost == "" {
		connectHost = "sptest.baidu.com"
	}
	ua := d.cfg.UserAgent
	if ua == "" {
		ua = "okhttp/3.11.0"
	}

	// ① TCP 拨号到 front proxy
	conn, err := d.dialer.DialContext(ctx, "tcp", server)
	if err != nil {
		return nil, fmt.Errorf("拨号 front proxy %s 失败：%w", server, err)
	}

	// ② TLS 握手（百度代理是 HTTPS）
	host := hostWithoutPort(server)
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("front proxy TLS 握手失败：%w", err)
	}

	// ③ HTTP CONNECT 请求
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: make(http.Header),
	}
	req.Header.Set("Host", connectHost)
	req.Header.Set("X-T5-Auth", d.cfg.Token)
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("Connection", "keep-alive")

	if err := req.Write(tlsConn); err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("写 CONNECT 请求失败：%w", err)
	}

	// ④ 读 CONNECT 响应
	br := bufio.NewReader(tlsConn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("读 CONNECT 响应失败：%w", err)
	}
	if resp.StatusCode != http.StatusOK {
		tlsConn.Close()
		return nil, fmt.Errorf("CONNECT 被拒：HTTP %d", resp.StatusCode)
	}

	log.Printf("✓ front proxy CONNECT 隧道建立 → %s", target)
	return &bufferedConn{Conn: tlsConn, reader: br}, nil
}

// ResolveDNS 通过 front proxy 走系统 DNS 解析。
func (d *FrontProxyDialer) ResolveDNS(_ context.Context, host string) (net.IP, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("DNS 解析 %s 返回空", host)
	}
	return ips[0], nil
}

// Close 关闭拨号器（幂等）。
func (d *FrontProxyDialer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return nil
}

// hostWithoutPort 从 host:port 中提取 host 部分。
func hostWithoutPort(s string) string {
	if i := strings.LastIndex(s, ":"); i != -1 {
		return s[:i]
	}
	return s
}

// bufferedConn 包裹已读部分数据的连接。
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
