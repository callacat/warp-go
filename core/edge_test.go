package core

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"fmt"
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

// TestResolveEdgeAddrsBothEmptyAutoCrossFamily 验证 cfg.EdgeAddr 与 optsEdgeIP
// 均为空时进入 auto 模式：候选 = v4:443 → v6:443 → v4 其余 → v6 其余（Android
// 桥的常态；v0.5.9 曾把双空当显式端点报错，v0.5.9~本版前回落 IPv4——CT103 上
// IPv4 被 QoS 时该默认把隧道一起带死，recvu4IV207cHy 改为跨族 auto）。
func TestResolveEdgeAddrsBothEmptyAutoCrossFamily(t *testing.T) {
	reg := &registration.Registration{
		EndpointV4:    "162.159.198.2",
		EndpointV6:    "2606:4700:103::2",
		EndpointPorts: []int{443, 500, 1701, 4500},
	}
	got, err := ResolveEdgeAddrs(DefaultConfig(), "", "", reg)
	if err != nil {
		t.Fatalf("双空 auto 失败：%v", err)
	}
	want := []string{
		"162.159.198.2:443", "[2606:4700:103::2]:443", // 两族 443 优先
		"162.159.198.2:500", "[2606:4700:103::2]:500",
		"162.159.198.2:1701", "[2606:4700:103::2]:1701",
		"162.159.198.2:4500", "[2606:4700:103::2]:4500",
	}
	if diff := cmpEdges(got, want); diff != "" {
		t.Errorf("双空 auto 候选不符：%s", diff)
	}
}

// TestResolveEdgeAddrsExplicitAutoKeyword 验证显式 -ip auto 与双空同义。
func TestResolveEdgeAddrsExplicitAutoKeyword(t *testing.T) {
	reg := &registration.Registration{
		EndpointV4: "162.159.198.2",
		EndpointV6: "2606:4700:103::2",
	}
	got, err := ResolveEdgeAddrs(DefaultConfig(), EdgeIPAuto, "", reg)
	if err != nil {
		t.Fatalf("-ip auto 失败：%v", err)
	}
	want := []string{"162.159.198.2:443", "[2606:4700:103::2]:443"}
	if diff := cmpEdges(got, want); diff != "" {
		t.Errorf("-ip auto 候选不符（默认端口 443）：%s", diff)
	}
}

// TestResolveEdgeAddrsExplicitV4Unchanged 锁定显式 -ip 4 在 auto 引入后行为
// 不变：只展开 IPv4 端口列表，绝不混入 v6 候选（向后兼容）。
func TestResolveEdgeAddrsExplicitV4Unchanged(t *testing.T) {
	reg := &registration.Registration{
		EndpointV4:    "162.159.198.2",
		EndpointV6:    "2606:4700:103::2",
		EndpointPorts: []int{443, 500},
	}
	got, err := ResolveEdgeAddrs(DefaultConfig(), "4", "", reg)
	if err != nil {
		t.Fatalf("-ip 4 失败：%v", err)
	}
	want := []string{"162.159.198.2:443", "162.159.198.2:500"}
	if diff := cmpEdges(got, want); diff != "" {
		t.Errorf("-ip 4 候选不符：%s", diff)
	}
}

// TestAutoEdgeAddrsPort443Promoted 锁定端口排序规则：注册端口列表不含 443 时
// 用首个端口充当两族首测端口，其余按原序尾随；443 在列表中但非首位时提前。
func TestAutoEdgeAddrsPort443Promoted(t *testing.T) {
	reg := &registration.Registration{
		EndpointV4:    "162.159.198.2",
		EndpointV6:    "2606:4700:103::2",
		EndpointPorts: []int{500, 443, 4500}, // 443 在第 2 位
	}
	got, err := ResolveEdgeAddrs(DefaultConfig(), EdgeIPAuto, "", reg)
	if err != nil {
		t.Fatalf("auto 失败：%v", err)
	}
	want := []string{
		"162.159.198.2:443", "[2606:4700:103::2]:443",
		"162.159.198.2:500", "[2606:4700:103::2]:500",
		"162.159.198.2:4500", "[2606:4700:103::2]:4500",
	}
	if diff := cmpEdges(got, want); diff != "" {
		t.Errorf("443 提前后的候选不符：%s", diff)
	}

	// 无 443：首端口 500 充当首测端口。
	reg.EndpointPorts = []int{500, 4500}
	got, err = ResolveEdgeAddrs(DefaultConfig(), EdgeIPAuto, "", reg)
	if err != nil {
		t.Fatalf("auto（无 443）失败：%v", err)
	}
	want = []string{
		"162.159.198.2:500", "[2606:4700:103::2]:500",
		"162.159.198.2:4500", "[2606:4700:103::2]:4500",
	}
	if diff := cmpEdges(got, want); diff != "" {
		t.Errorf("无 443 时的候选不符：%s", diff)
	}
}

// TestAutoEdgeAddrsSingleFamilyDegradation 锁定单族退化：注册信息只有单一
// 地址族时 auto 等价该族顺序展开（不会产出空 host 的垃圾候选）。
func TestAutoEdgeAddrsSingleFamilyDegradation(t *testing.T) {
	reg := &registration.Registration{
		EndpointV6:    "2606:4700:103::2",
		EndpointPorts: []int{443, 500},
	}
	got, err := ResolveEdgeAddrs(DefaultConfig(), "", "", reg)
	if err != nil {
		t.Fatalf("单族 auto 失败：%v", err)
	}
	want := []string{"[2606:4700:103::2]:443", "[2606:4700:103::2]:500"}
	if diff := cmpEdges(got, want); diff != "" {
		t.Errorf("单族退化候选不符：%s", diff)
	}
}

// TestResolveEdgeAddrsBothEmptyNoEndpoints 验证双空 auto 且注册信息两族端点
// 全缺时报清晰错误（提示重新注册），而非产出垃圾候选或空表。
func TestResolveEdgeAddrsBothEmptyNoEndpoints(t *testing.T) {
	_, err := ResolveEdgeAddrs(DefaultConfig(), "", "", &registration.Registration{})
	if err == nil {
		t.Fatal("两族端点全缺时应报错")
	}
	if !strings.Contains(err.Error(), "重新注册") {
		t.Errorf("错误信息应提示重新注册，得到：%v", err)
	}
}

// cmpEdges 比较候选列表，不一致时返回差异描述（一致返回空串）。
func cmpEdges(got, want []string) string {
	if len(got) != len(want) {
		return fmt.Sprintf("结果 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Sprintf("结果[%d] = %q，期望 %q（全部=%v）", i, got[i], want[i], got)
		}
	}
	return ""
}
