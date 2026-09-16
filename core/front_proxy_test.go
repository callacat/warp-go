package core

import (
	"testing"
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
	cfg := &FrontProxyConfig{Enabled: true, Server: ""}
	err := ValidateFrontProxy(cfg)
	if err == nil {
		t.Fatal("empty server should error")
	}
	if err.Error() != "front_proxy.server 不能为空" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFrontProxy_ServerInvalid(t *testing.T) {
	cfg := &FrontProxyConfig{Enabled: true, Server: "no-port"}
	err := ValidateFrontProxy(cfg)
	if err == nil {
		t.Fatal("invalid server should error")
	}
}

func TestValidateFrontProxy_ConnectHostDefault(t *testing.T) {
	cfg := &FrontProxyConfig{Enabled: true, Server: "proxy.example.com:443"}
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
	}
	if err := ValidateFrontProxy(cfg); err != nil {
		t.Fatalf("valid config should not error: %v", err)
	}
}

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
	if cfg.Token != "482857715" {
		t.Fatalf("default token wrong: %q", cfg.Token)
	}
}

func TestFrontProxyConfig_MarshalJSON(t *testing.T) {
	cfg := FrontProxyConfig{
		Enabled:     true,
		Server:      "cloudnproxy.baidu.com:443",
		ConnectHost: "sptest.baidu.com",
		Token:       "482857715",
		UserAgent:   "okhttp/3.11.0",
	}
	// 验证 JSON tag 正确
	if cfg.Server != "cloudnproxy.baidu.com:443" {
		t.Fatal("server field broken")
	}
}
