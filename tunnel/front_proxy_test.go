package tunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"
)

// mockFrontProxy 启动一个模拟 front proxy（TLS + HTTP CONNECT）。
// 返回 addr、cleanup 与自签证书 CA 池（dialer 注入信任锚用）。
func mockFrontProxy(t *testing.T) (addr string, cleanup func(), rootCAs *x509.CertPool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	// 自签证书
	cert := generateSelfSignedCert(t)
	tlsLn := tls.NewListener(ln, &tls.Config{
		Certificates: []tls.Certificate{cert},
	})

	caPool := x509.NewCertPool()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf cert failed: %v", err)
	}
	caPool.AddCert(leaf)

	go func() {
		for {
			conn, err := tlsLn.Accept()
			if err != nil {
				return
			}
			go handleMockCONNECT(conn)
		}
	}()

	return ln.Addr().String(), func() { tlsLn.Close() }, caPool
}

func handleMockCONNECT(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		// net.Conn 不是 http.ResponseWriter，手写 405 响应行。
		fmt.Fprintf(conn, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
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
	// 连接保持（模拟隧道建立后的双向数据）
	buf := make([]byte, 1024)
	for {
		_, err := conn.Read(buf)
		if err != nil {
			return
		}
	}
}

func TestFrontProxyDialer_DialTunnel_OK(t *testing.T) {
	addr, cleanup, rootCAs := mockFrontProxy(t)
	defer cleanup()

	d := NewFrontProxyDialer(FrontProxyDialerConfig{
		Server:      addr,
		ConnectHost: "sptest.baidu.com",
		Token:       "482857715",
		UserAgent:   "test/1.0",
		RootCAs:     rootCAs,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := d.DialTunnel(ctx, "162.159.198.2:443")
	if err != nil {
		t.Fatalf("DialTunnel failed: %v", err)
	}
	defer conn.Close()

	// 验证连接可读写
	_, err = conn.Write([]byte("test"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
}

func TestFrontProxyDialer_DialTunnel_ConnectRejected(t *testing.T) {
	// 启动一个返回 403 的 mock
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert := generateSelfSignedCert(t)
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
	defer tlsLn.Close()

	caPool := x509.NewCertPool()
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf cert failed: %v", err)
	}
	caPool.AddCert(leaf)

	go func() {
		conn, err := tlsLn.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		http.ReadRequest(br)
		resp := &http.Response{StatusCode: 403, ProtoMajor: 1, ProtoMinor: 1, Header: make(http.Header)}
		resp.Write(conn)
	}()

	d := NewFrontProxyDialer(FrontProxyDialerConfig{
		Server:      ln.Addr().String(),
		ConnectHost: "sptest.baidu.com",
		Token:       "482857715",
		RootCAs:     caPool,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err = d.DialTunnel(ctx, "target:443")
	if err == nil {
		t.Fatal("403 should cause error")
	}
}

func TestFrontProxyDialer_Close(t *testing.T) {
	d := NewFrontProxyDialer(FrontProxyDialerConfig{Server: "127.0.0.1:443"})
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	// Closed dialer should error
	_, err := d.DialTunnel(context.Background(), "target:443")
	if err == nil {
		t.Fatal("closed dialer should error")
	}
}

func TestFrontProxyDialer_ResolveDNS(t *testing.T) {
	d := NewFrontProxyDialer(FrontProxyDialerConfig{})
	ip, err := d.ResolveDNS(context.Background(), "localhost")
	if err != nil {
		t.Fatalf("ResolveDNS failed: %v", err)
	}
	if ip == nil {
		t.Fatal("localhost should resolve")
	}
}

func generateSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	// Go crypto 自签证书（20 行，无外部依赖；CI/本地都可跑）。
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key failed: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		// dialer 用 ServerName=127.0.0.1 握手；Go 1.15+ 不回落 CN，必须带 IP SAN。
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate failed: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// 注：自签证书生成太复杂，跳过。集成测试在 CI 真机跑。
func TestFrontProxyDialer_IntegrationSkipped(t *testing.T) {
	t.Skip("HTTP CONNECT 集成测试在 CI 真机跑（需百度代理可达）")
}
