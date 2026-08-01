//go:build android || linux

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// writeRegJSON 写一份可被 registration.Load 解析的最小 reg.json：含 P-256
// 私钥与可选的分配地址 / 边缘公钥，返回文件路径。
func writeRegJSON(t *testing.T, dir string, assignedV4, assignedV6 string) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("编码私钥失败：%v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"id":             "test-device",
		"token":          "tok",
		"private_key":    base64.StdEncoding.EncodeToString(der),
		"assigned_ipv4":  assignedV4,
		"assigned_ipv6":  assignedV6,
		"endpoint_v4":    "162.159.198.2",
		"endpoint_ports": []int{443, 500},
	})
	path := filepath.Join(dir, "reg.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("写 reg.json 失败：%v", err)
	}
	return path
}

// T1：有效 config.json + reg.json（带分配地址）→ fd 透传、MTU 1500、
// Inet4Address [172.16.0.2/32]、Inet6Address [/128]、DNSServers [1.1.1.1]、
// 相对 RulesPath 锚定到沙箱。
func TestBuildAndroidConfigFull(t *testing.T) {
	sandbox := t.TempDir()
	if err := os.WriteFile(filepath.Join(sandbox, "config.json"), []byte(`{
		"listen_addr": "127.0.0.1:40000",
		"rules_path": "rules.txt",
		"geo_dir": "geo"
	}`), 0o644); err != nil {
		t.Fatalf("写 config.json 失败：%v", err)
	}
	writeRegJSON(t, sandbox, "172.16.0.2", "2606:4700:110:8a2e:fb70:7a34:2f7e:1")

	built, err := buildAndroidConfig(sandbox, 42)
	if err != nil {
		t.Fatalf("buildAndroidConfig 失败：%v", err)
	}
	if built.vpnCfg.FileDescriptor != 42 {
		t.Errorf("fd = %d，期望 42（透传）", built.vpnCfg.FileDescriptor)
	}
	if built.vpnCfg.MTU != 1500 {
		t.Errorf("MTU = %d，期望 1500", built.vpnCfg.MTU)
	}
	if len(built.vpnCfg.Inet4Address) != 1 || built.vpnCfg.Inet4Address[0].String() != "172.16.0.2/32" {
		t.Errorf("Inet4Address = %v，期望 [172.16.0.2/32]", built.vpnCfg.Inet4Address)
	}
	if len(built.vpnCfg.Inet6Address) != 1 || built.vpnCfg.Inet6Address[0].String() != "2606:4700:110:8a2e:fb70:7a34:2f7e:1/128" {
		t.Errorf("Inet6Address = %v，期望 [/128]", built.vpnCfg.Inet6Address)
	}
	if len(built.vpnCfg.DNSServers) != 1 || built.vpnCfg.DNSServers[0] != netip.MustParseAddr("1.1.1.1") {
		t.Errorf("DNSServers = %v，期望 [1.1.1.1]", built.vpnCfg.DNSServers)
	}
	if got := built.cfg.RulesPath; got != filepath.Join(sandbox, "rules.txt") {
		t.Errorf("RulesPath = %q，期望锚定到沙箱 %q", got, filepath.Join(sandbox, "rules.txt"))
	}
	if got := built.cfg.GeoDir; got != filepath.Join(sandbox, "geo") {
		t.Errorf("GeoDir = %q，期望锚定到沙箱 %q", got, filepath.Join(sandbox, "geo"))
	}
	if built.regData == nil {
		t.Error("regData 应为非 nil")
	}
}

// T2：缺失 reg.json → 错误（错误信息含"注册"，且可解包到 fs.ErrNotExist）。
func TestBuildAndroidConfigMissingReg(t *testing.T) {
	sandbox := t.TempDir()
	if err := os.WriteFile(filepath.Join(sandbox, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatalf("写 config.json 失败：%v", err)
	}
	_, err := buildAndroidConfig(sandbox, 1)
	if err == nil {
		t.Fatal("缺失 reg.json 应返回错误")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("错误应可解包到 fs.ErrNotExist，得到：%v", err)
	}
}

// T3：缺失 config.json → 使用默认配置（ListenAddr 等默认值），仍需要 reg.json。
func TestBuildAndroidConfigDefaultConfig(t *testing.T) {
	sandbox := t.TempDir()
	writeRegJSON(t, sandbox, "172.16.0.2", "")

	built, err := buildAndroidConfig(sandbox, 7)
	if err != nil {
		t.Fatalf("buildAndroidConfig 失败：%v", err)
	}
	if built.cfg.ListenAddr != "127.0.0.1:40000" {
		t.Errorf("默认 ListenAddr = %q，期望 127.0.0.1:40000", built.cfg.ListenAddr)
	}
	if built.cfg.GeoAutoUpdateDays != 7 {
		t.Errorf("默认 GeoAutoUpdateDays = %d，期望 7", built.cfg.GeoAutoUpdateDays)
	}
	if _, err := os.Stat(filepath.Join(sandbox, "config.json")); !os.IsNotExist(err) {
		t.Error("缺失 config.json 时不应在沙箱生成模板文件")
	}
}

// T4：非法 AssignedIPv4 → Inet4Address 为空（零值地址跳过），IPv6 不受影响。
func TestBuildAndroidConfigInvalidIPv4(t *testing.T) {
	sandbox := t.TempDir()
	writeRegJSON(t, sandbox, "not-an-ip", "2606:4700:110::2")

	built, err := buildAndroidConfig(sandbox, 3)
	if err != nil {
		t.Fatalf("buildAndroidConfig 失败：%v", err)
	}
	if len(built.vpnCfg.Inet4Address) != 0 {
		t.Errorf("非法 IPv4 时应跳过，Inet4Address = %v", built.vpnCfg.Inet4Address)
	}
	if len(built.vpnCfg.Inet6Address) != 1 || built.vpnCfg.Inet6Address[0].String() != "2606:4700:110::2/128" {
		t.Errorf("IPv6 应保留，Inet6Address = %v", built.vpnCfg.Inet6Address)
	}
}

// T5：config.json 相对 rules_path → 锚定 filepath.Join(sandbox, "rules.txt")。
func TestBuildAndroidConfigRelativeRulesPath(t *testing.T) {
	sandbox := t.TempDir()
	if err := os.WriteFile(filepath.Join(sandbox, "config.json"), []byte(`{"rules_path":"rules.txt"}`), 0o644); err != nil {
		t.Fatalf("写 config.json 失败：%v", err)
	}
	writeRegJSON(t, sandbox, "172.16.0.2", "")

	built, err := buildAndroidConfig(sandbox, 9)
	if err != nil {
		t.Fatalf("buildAndroidConfig 失败：%v", err)
	}
	want := filepath.Join(sandbox, "rules.txt")
	if built.cfg.RulesPath != want {
		t.Errorf("RulesPath = %q，期望 %q", built.cfg.RulesPath, want)
	}
}
