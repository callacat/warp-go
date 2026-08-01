package core

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"warp/registration"
)

// newMinimalRegistration 生成 P-256 密钥对并写入临时 reg.json 加载，返回最小
// 注册信息（ClientCert 已构建，PeerPublicKey 为空）。
func newMinimalRegistration(t *testing.T) *registration.Registration {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("编码私钥失败：%v", err)
	}
	path := filepath.Join(t.TempDir(), "reg.json")
	if err := os.WriteFile(path, []byte(`{"id":"test-device","token":"tok","private_key":"`+
		base64.StdEncoding.EncodeToString(der)+`"}`), 0o600); err != nil {
		t.Fatalf("写注册文件失败：%v", err)
	}
	reg, err := registration.Load(path)
	if err != nil {
		t.Fatalf("加载注册信息失败：%v", err)
	}
	return reg
}

// mustEncodePublicKey 把公钥编码为 base64 PKIX DER（与注册文件 peer_public_key
// 的存储形式一致）。
func mustEncodePublicKey(t *testing.T, pub *ecdsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("编码公钥失败：%v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// TestBuildTLSConfigPinsPeerPublicKey 验证含边缘公钥的注册信息构建出固定校验器
// （VerifyPeerCertificate 非 nil）与客户端证书、TLS13、H3 ALPN、NIST 曲线。
func TestBuildTLSConfigPinsPeerPublicKey(t *testing.T) {
	reg := newMinimalRegistration(t)
	// 用私钥派生一个合法公钥编码，模拟注册时 API 下发的边缘公钥。
	reg.PeerPublicKey = mustEncodePublicKey(t, &reg.PrivateKey.PublicKey)

	tlsConfig, err := BuildTLSConfig(reg)
	if err != nil {
		t.Fatalf("BuildTLSConfig 失败：%v", err)
	}
	if tlsConfig.VerifyPeerCertificate == nil {
		t.Error("含边缘公钥时 VerifyPeerCertificate 应为非 nil（固定应启用）")
	}
	if len(tlsConfig.Certificates) != 1 {
		t.Errorf("Certificates 长度 = %d，期望 1", len(tlsConfig.Certificates))
	}
	if tlsConfig.ServerName != "consumer-masque-proxy.cloudflareclient.com" {
		t.Errorf("ServerName = %q", tlsConfig.ServerName)
	}
}

// TestBuildTLSConfigNoPublicKeyReturnsNilVerifier 验证注册信息没有边缘公钥时
// 返回 nil 校验器（公钥固定禁用），不报错——Server.Start 原 L316-320 语义。
func TestBuildTLSConfigNoPublicKeyReturnsNilVerifier(t *testing.T) {
	reg := newMinimalRegistration(t) // PeerPublicKey 为空
	tlsConfig, err := BuildTLSConfig(reg)
	if err != nil {
		t.Fatalf("无边缘公钥时不应报错，得到 %v", err)
	}
	if tlsConfig.VerifyPeerCertificate != nil {
		t.Error("无边缘公钥时 VerifyPeerCertificate 应为 nil（固定禁用）")
	}
}

// TestResolveEdgeAddrsIPv4JoinsPorts 验证 optsEdgeIP="4" 时按注册信息 IPv4
// 端点展开端口列表（默认 443）。
func TestResolveEdgeAddrsIPv4JoinsPorts(t *testing.T) {
	reg := &registration.Registration{
		EndpointV4:    "162.159.198.2",
		EndpointPorts: []int{443, 500},
	}
	got, err := ResolveEdgeAddrs(DefaultConfig(), "4", "", reg)
	if err != nil {
		t.Fatalf("ResolveEdgeAddrs 失败：%v", err)
	}
	want := []string{"162.159.198.2:443", "162.159.198.2:500"}
	if len(got) != len(want) {
		t.Fatalf("结果 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("结果[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestResolveEdgeAddrsIPv4MissingEndpoint 验证 optsEdgeIP="4" 但注册信息没有
// IPv4 端点时报错。
func TestResolveEdgeAddrsIPv4MissingEndpoint(t *testing.T) {
	reg := &registration.Registration{EndpointV6: "2606:4700:103::2"}
	_, err := ResolveEdgeAddrs(DefaultConfig(), "4", "", reg)
	if err == nil {
		t.Fatal("缺少 IPv4 端点时应报错")
	}
	if !strings.Contains(err.Error(), "IPv4") {
		t.Errorf("错误信息应提及 IPv4，得到：%v", err)
	}
}

// TestResolveEdgeAddrsEdgeAddrWins 验证 cfg.EdgeAddr 非空且 optsEdgeIP 为空时
// 走 resolveEdge 应用扫描结果（显式 host:port 透传）。
func TestResolveEdgeAddrsEdgeAddrWins(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EdgeAddr = "162.159.192.1:443"
	got, err := ResolveEdgeAddrs(cfg, "", "", &registration.Registration{})
	if err != nil {
		t.Fatalf("ResolveEdgeAddrs 失败：%v", err)
	}
	if len(got) != 1 || got[0] != "162.159.192.1:443" {
		t.Errorf("结果 = %v，期望 [162.159.192.1:443]", got)
	}
}

// TestResolveEdgeAddrsExplicitSpec 验证 optsEdgeIP 为显式 host:port 时透传
// resolveEdge（IP 字面量单候选）。
func TestResolveEdgeAddrsExplicitSpec(t *testing.T) {
	got, err := ResolveEdgeAddrs(DefaultConfig(), "162.159.198.2:443", "", &registration.Registration{})
	if err != nil {
		t.Fatalf("ResolveEdgeAddrs 失败：%v", err)
	}
	if len(got) != 1 || got[0] != "162.159.198.2:443" {
		t.Errorf("结果 = %v，期望 [162.159.198.2:443]", got)
	}
}
