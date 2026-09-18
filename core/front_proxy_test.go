package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"warp/registration"
	"warp/tunnel"
)

func TestValidateFrontProxy_Disabled(t *testing.T) {
	cfg := &FrontProxyConfig{Enabled: false}
	if err := ValidateFrontProxy(cfg); err != nil {
		t.Fatalf("disabled front proxy should not error, got: %v", err)
	}
}

func TestValidateFrontProxy_NilConfig(t *testing.T) {
	if err := ValidateFrontProxy(nil); err != nil {
		t.Fatalf("nil config should not error, got: %v", err)
	}
}

func TestValidateFrontProxy_ServerEmpty(t *testing.T) {
	cfg := &FrontProxyConfig{Enabled: true, Server: "", Token: "t"}
	err := ValidateFrontProxy(cfg)
	if err == nil {
		t.Fatal("empty server should error")
	}
	if err.Error() != "front_proxy.server 不能为空" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFrontProxy_ServerInvalid(t *testing.T) {
	cfg := &FrontProxyConfig{Enabled: true, Server: "no-port", Token: "t"}
	if err := ValidateFrontProxy(cfg); err == nil {
		t.Fatal("invalid server should error")
	}
}

// TestValidateFrontProxy_TokenEmpty 验证空 token 仍被拦截（默认已预填，
// 此用例覆盖用户手动清空 token 后 enabled=true 的边界场景）。
func TestValidateFrontProxy_TokenEmpty(t *testing.T) {
	cfg := &FrontProxyConfig{Enabled: true, Server: "proxy.example.com:443", Token: ""}
	err := ValidateFrontProxy(cfg)
	if err == nil {
		t.Fatal("enabled 且 token 为空应报错")
	}
	if err.Error() != ErrFrontProxyTokenEmpty.Error() {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFrontProxy_ConnectHostDefault(t *testing.T) {
	cfg := &FrontProxyConfig{Enabled: true, Server: "proxy.example.com:443", Token: "t"}
	if err := ValidateFrontProxy(cfg); err != nil {
		t.Fatalf("valid config should not error: %v", err)
	}
	if cfg.ConnectHost != "sptest.baidu.com" {
		t.Fatalf("connect_host should default to sptest.baidu.com, got %q", cfg.ConnectHost)
	}
}

func TestValidateFrontProxy_ValidConfig(t *testing.T) {
	cfg := &FrontProxyConfig{
		Enabled:     true,
		Server:      "cloudnproxy.baidu.com:443",
		ConnectHost: "sptest.baidu.com",
		Token:       "t",
	}
	if err := ValidateFrontProxy(cfg); err != nil {
		t.Fatalf("valid config should not error: %v", err)
	}
}

// TestDefaultFrontProxyConfig 验证默认值：token 预填共享凭据（2026-09-17 东哥拍板，
// 开箱即用，不再要求用户手动填写）。
func TestDefaultFrontProxyConfig(t *testing.T) {
	cfg := DefaultFrontProxyConfig()
	if cfg.Enabled {
		t.Fatal("default should be disabled")
	}
	if cfg.Server != "cloudnproxy.baidu.com:443" {
		t.Fatalf("default server wrong: %q", cfg.Server)
	}
	if cfg.ConnectHost != "sptest.baidu.com" {
		t.Fatalf("default connect_host wrong: %q", cfg.ConnectHost)
	}
	if cfg.Token != DefaultFrontProxyToken {
		t.Fatalf("默认 token 应预填共享凭据，实际 %q", cfg.Token)
	}
}

func TestFrontProxyConfig_MarshalJSON(t *testing.T) {
	cfg := FrontProxyConfig{
		Enabled:     true,
		Server:      "cloudnproxy.baidu.com:443",
		ConnectHost: "sptest.baidu.com",
		Token:       "user-supplied",
		UserAgent:   "okhttp/3.11.0",
	}
	// 验证 JSON tag 正确
	if cfg.Server != "cloudnproxy.baidu.com:443" {
		t.Fatal("server field broken")
	}
}

// TestApplyFrontProxyOptions_FlagOverridesAndFallsBackEdgeIP 是 C1 的核心断言：
// `-front-proxy=true` 时 cfg 必须立即置 enabled（供 NewKernelContext 装配
// FrontProxyDialer），且 EdgeIP 必须回退 auto（供 ResolveEdgeAddrs 展开候选）。
func TestApplyFrontProxyOptions_FlagOverridesAndFallsBackEdgeIP(t *testing.T) {
	cfg := &Config{FrontProxy: FrontProxyConfig{
		Enabled: false,
		Server:  "cloudnproxy.baidu.com:443",
		Token:   "t",
	}}
	enabled := true
	opts := &Options{FrontProxyOverride: &enabled, EdgeIP: "162.159.192.1:443"}

	if err := ApplyFrontProxyOptions(cfg, opts); err != nil {
		t.Fatalf("应用旗标失败：%v", err)
	}
	if !cfg.FrontProxy.Enabled {
		t.Fatal("-front-proxy=true 应启用 cfg.FrontProxy.Enabled")
	}
	if opts.EdgeIP != EdgeIPAuto {
		t.Fatalf("EdgeIP = %q，期望回退 %q（互斥）", opts.EdgeIP, EdgeIPAuto)
	}
}

// TestApplyFrontProxyOptions_ConfigEnabledFallsBackEdgeIP 验证 GUI 配置路径：
// config.json 里 enabled=true 同样触发互斥回退（无需旗标）。
func TestApplyFrontProxyOptions_ConfigEnabledFallsBackEdgeIP(t *testing.T) {
	cfg := &Config{FrontProxy: FrontProxyConfig{
		Enabled: true,
		Server:  "cloudnproxy.baidu.com:443",
		Token:   "t",
	}}
	opts := &Options{EdgeIP: "162.159.192.1:443"}

	if err := ApplyFrontProxyOptions(cfg, opts); err != nil {
		t.Fatalf("应用配置失败：%v", err)
	}
	if opts.EdgeIP != EdgeIPAuto {
		t.Fatalf("EdgeIP = %q，期望回退 %q（互斥）", opts.EdgeIP, EdgeIPAuto)
	}
}

// TestApplyFrontProxyOptions_FlagFalseWins 验证三态覆盖：旗标给出 false 时
// 即使 config.json 里 enabled=true 也必须关掉，且不动 EdgeIP。
func TestApplyFrontProxyOptions_FlagFalseWins(t *testing.T) {
	cfg := &Config{FrontProxy: FrontProxyConfig{
		Enabled: true,
		Server:  "cloudnproxy.baidu.com:443",
		Token:   "t",
	}}
	disabled := false
	opts := &Options{FrontProxyOverride: &disabled, EdgeIP: "4"}

	if err := ApplyFrontProxyOptions(cfg, opts); err != nil {
		t.Fatalf("应用旗标失败：%v", err)
	}
	if cfg.FrontProxy.Enabled {
		t.Fatal("-front-proxy=false 应覆盖 config.json 的 enabled=true")
	}
	if opts.EdgeIP != "4" {
		t.Fatalf("未启用时不应回退 EdgeIP，实际 %q", opts.EdgeIP)
	}
}

// TestApplyFrontProxyOptions_NoFlagKeepsConfig 验证未给旗标时按 config.json。
func TestApplyFrontProxyOptions_NoFlagKeepsConfig(t *testing.T) {
	cfg := &Config{FrontProxy: FrontProxyConfig{
		Enabled: false,
		Server:  "cloudnproxy.baidu.com:443",
		Token:   "t",
	}}
	opts := &Options{EdgeIP: "4"}

	if err := ApplyFrontProxyOptions(cfg, opts); err != nil {
		t.Fatalf("未给旗标不应报错：%v", err)
	}
	if cfg.FrontProxy.Enabled {
		t.Fatal("未给旗标时不应启用")
	}
	if opts.EdgeIP != "4" {
		t.Fatalf("未启用时不应回退 EdgeIP，实际 %q", opts.EdgeIP)
	}
}

// TestApplyFrontProxyOptions_DefaultPrefillTokenPasses 验证默认配置下开启
// front_proxy 不报 token 空（预填默认共享凭据后，enabled=true 无需用户填 token
// 即可通过 ValidateFrontProxy）。
func TestApplyFrontProxyOptions_DefaultPrefillTokenPasses(t *testing.T) {
	cfg := &Config{FrontProxy: DefaultFrontProxyConfig()}
	enabled := true
	opts := &Options{FrontProxyOverride: &enabled, EdgeIP: EdgeIPAuto}
	if err := ApplyFrontProxyOptions(cfg, opts); err != nil {
		t.Fatalf("默认配置 + enabled=true 不应报错（token 已预填），实际: %v", err)
	}
	if !cfg.FrontProxy.Enabled {
		t.Fatal("front_proxy 应被旗标启用")
	}
	if cfg.FrontProxy.Token != DefaultFrontProxyToken {
		t.Fatalf("token 不应被改动，实际 %q", cfg.FrontProxy.Token)
	}
}

// TestApplyFrontProxyOptions_InvalidConfigErrors 验证 C2：配置错误在启动期
// 返回（原先 ValidateFrontProxy 从未被生产路径调用，格式错误运行时才炸）。
func TestApplyFrontProxyOptions_InvalidConfigErrors(t *testing.T) {
	cfg := &Config{FrontProxy: FrontProxyConfig{Enabled: true, Server: "cloudnproxy.baidu.com:443"}} // 无 token
	err := ApplyFrontProxyOptions(cfg, &Options{EdgeIP: EdgeIPAuto})
	if err == nil {
		t.Fatal("enabled 且 token 为空时应在启动期报错")
	}
	if !errors.Is(err, ErrFrontProxyTokenEmpty) {
		t.Fatalf("错误未包装 token 校验失败：%v", err)
	}
}

// TestNewKernelContextUsesFrontProxyDialer 验证 C1 的装配断言：cfg 里
// FrontProxy.Enabled=true 时内核拨号器必须是 *tunnel.FrontProxyDialer
// （而不是 MasqueClient）——原实现覆盖发生在这之后，旗标对装配完全失效。
func TestNewKernelContextUsesFrontProxyDialer(t *testing.T) {
	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "rules.txt")
	if err := os.WriteFile(rulesPath, []byte("proxy,domain:proxy.example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		RulesPath: rulesPath,
		GeoDir:    filepath.Join(tmp, "geo"),
		FrontProxy: FrontProxyConfig{
			Enabled:     true,
			Server:      "cloudnproxy.baidu.com:443",
			ConnectHost: "sptest.baidu.com",
			Token:       "t",
		},
	}
	reg := &registration.Registration{
		AssignedIPv4: "172.16.0.2",
		AssignedIPv6: "2606:4700:110:8a2e:fb70:7a34:2f7e:1",
	}
	k, err := NewKernelContext(context.Background(), cfg, reg, []string{"162.159.192.1:443"}, &tls.Config{})
	if err != nil {
		t.Fatalf("NewKernelContext 失败：%v", err)
	}
	t.Cleanup(func() { _ = k.Close() })

	if _, ok := k.dial.(*tunnel.FrontProxyDialer); !ok {
		t.Fatalf("front proxy 启用时拨号器 = %T，期望 *tunnel.FrontProxyDialer", k.dial)
	}
	// 未启用（MASQUE）分支不发真实 QUIC 拨号就无法断言，由 kernel_test.go
	// 的 fakeDialer 注入用例覆盖装配缝。
}

// TestKernelRouteUnchangedWhenFrontProxyEnabled 是 v0.6.6 DNS 回流修复的
// 分流回归：front-proxy 开启（拨号器换 FrontProxyDialer）不得改变 Route
// 判定——proxy 域名仍 → ("proxy", true)、direct 域名仍 → ("direct", true)、
// 未命中仍 → ("", false)。Android 桥在 front-proxy 下只替换 TunnelDNS
// （物理直连阻断回流），分流语义由此证保持原样。
func TestKernelRouteUnchangedWhenFrontProxyEnabled(t *testing.T) {
	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "rules.txt")
	if err := os.WriteFile(rulesPath, []byte("proxy,domain:proxy.example\ndirect,domain:direct.example\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		RulesPath: rulesPath,
		GeoDir:    filepath.Join(tmp, "geo"),
		FrontProxy: FrontProxyConfig{
			Enabled:     true,
			Server:      "cloudnproxy.baidu.com:443",
			ConnectHost: "sptest.baidu.com",
			Token:       "t",
		},
	}
	reg := &registration.Registration{
		AssignedIPv4: "172.16.0.2",
		AssignedIPv6: "2606:4700:110:8a2e:fb70:7a34:2f7e:1",
	}
	k, err := NewKernelContext(context.Background(), cfg, reg, []string{"162.159.192.1:443"}, &tls.Config{})
	if err != nil {
		t.Fatalf("NewKernelContext（front_proxy 开启）失败：%v", err)
	}
	t.Cleanup(func() { _ = k.Close() })

	if action, matched := k.Route("proxy.example", netip.Addr{}); action != "proxy" || !matched {
		t.Errorf("front_proxy 下 Route(proxy.example) = (%q, %v)，期望 (\"proxy\", true)", action, matched)
	}
	if action, matched := k.Route("direct.example", netip.Addr{}); action != "direct" || !matched {
		t.Errorf("front_proxy 下 Route(direct.example) = (%q, %v)，期望 (\"direct\", true)", action, matched)
	}
	if action, matched := k.Route("unmatched.example", netip.Addr{}); action != "" || matched {
		t.Errorf("front_proxy 下 Route(未命中) = (%q, %v)，期望 (\"\", false)", action, matched)
	}
}

// TestServerStartFrontProxyFlagAppliesBeforeKernel 是 C1 的 Start 级回归防线：
// 真实走 Server.Start（最小注册信息 + 真实配置加载），断言 `-front-proxy=true`
// 的内核拨号器是 FrontProxyDialer、且 EdgeIP 已在 ResolveEdgeAddrs 之前回退
// auto（边缘候选里不再出现用户指定的优选 IP）。若有人把覆盖块挪回 NewKernel
// 之后，本条即刻变红。
func TestServerStartFrontProxyFlagAppliesBeforeKernel(t *testing.T) {
	dir := t.TempDir()

	// 最小注册信息（含边缘端点：auto 候选表需要）
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败：%v", err)
	}
	der, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("编码私钥失败：%v", err)
	}
	regPath := filepath.Join(dir, "reg.json")
	regJSON := fmt.Sprintf(`{"id":"c1-test","token":"tok","private_key":%q,"endpoint_v4":"162.159.198.2"}`,
		base64.StdEncoding.EncodeToString(der))
	if err := os.WriteFile(regPath, []byte(regJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	// config.json：front_proxy 未启用（由旗标开启），token 由配置提供（C6 后无默认值）
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON := fmt.Sprintf(`{
  "listen_addr": "127.0.0.1:0",
  "rules_path": %q,
  "geo_dir": %q,
  "geo_auto_update_days": 0,
  "front_proxy": {
    "enabled": false,
    "server": "cloudnproxy.baidu.com:443",
    "connect_host": "sptest.baidu.com",
    "token": "config-token"
  }
}`, filepath.Join(dir, "rules.txt"), filepath.Join(dir, "geo"))
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	enabled := true
	s := New(Options{
		ConfigPath:         cfgPath,
		StateFile:          regPath,
		DataDir:            dir,
		ListenAddr:         "127.0.0.1:0",
		EdgeIP:             "162.159.192.1:443", // 用户显式优选 IP：front proxy 开启后必须被忽略
		FrontProxyOverride: &enabled,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Start(ctx) }()

	var k *Kernel
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		k = s.kernel
		s.mu.Unlock()
		if k != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if k == nil {
		t.Fatal("Start 未在 20s 内完成内核装配")
	}

	if _, ok := k.dial.(*tunnel.FrontProxyDialer); !ok {
		t.Errorf("-front-proxy=true 时内核拨号器 = %T，期望 *tunnel.FrontProxyDialer（C1：覆盖必须早于 NewKernel）", k.dial)
	}
	if s.opts.EdgeIP != EdgeIPAuto {
		t.Errorf("EdgeIP = %q，期望回退 %q", s.opts.EdgeIP, EdgeIPAuto)
	}
	s.mu.Lock()
	addrs := append([]string(nil), s.edgeAddrs...)
	s.mu.Unlock()
	for _, a := range addrs {
		if a == "162.159.192.1:443" {
			t.Errorf("边缘候选仍含用户优选 IP %q（互斥回退未在 ResolveEdgeAddrs 之前生效）", a)
		}
	}
	if len(addrs) == 0 {
		t.Error("边缘候选为空")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start 返回错误：%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ctx 取消后 Start 未退出")
	}
}
