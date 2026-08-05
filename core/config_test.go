package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"warp/route"
)

// TestDefaultConfig 验证内置默认值，与 config.go 的 DefaultConfig 一致。
func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ListenAddr != "127.0.0.1:40000" {
		t.Errorf("ListenAddr = %q，期望 127.0.0.1:40000", cfg.ListenAddr)
	}
	if cfg.RulesPath != "rules.txt" {
		t.Errorf("RulesPath = %q，期望 rules.txt", cfg.RulesPath)
	}
	if cfg.GeoDir != "geo" {
		t.Errorf("GeoDir = %q，期望 geo", cfg.GeoDir)
	}
	if cfg.GeoRepo != "https://github.com/MetaCubeX/meta-rules-dat" {
		t.Errorf("GeoRepo = %q，与期望不符", cfg.GeoRepo)
	}
	if cfg.GeoAutoUpdateDays != 7 {
		t.Errorf("GeoAutoUpdateDays = %d，期望 7", cfg.GeoAutoUpdateDays)
	}
	if cfg.EnableSystemProxy || cfg.AllowUDP {
		t.Error("EnableSystemProxy/AllowUDP 默认应为 false")
	}
}

// TestLoadConfigMissingCreatesFile 验证文件缺失时自动以默认值原子生成模板。
func TestLoadConfigMissingCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig 失败：%v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:40000" {
		t.Errorf("缺失文件应返回默认配置，ListenAddr = %q", cfg.ListenAddr)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("默认配置文件未生成：%v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("生成的配置文件不是合法 JSON：%v", err)
	}
	if parsed["listen_addr"] != "127.0.0.1:40000" {
		t.Errorf("生成的 listen_addr = %v", parsed["listen_addr"])
	}
}

// TestLoadConfigOverrides 验证 JSON 字段覆盖默认值、未出现字段保持默认。
func TestLoadConfigOverrides(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{
  "listen_addr": "0.0.0.0:1080",
  "rules_path": "my-rules.txt",
  "geo_auto_update_days": 3
}`), 0o600); err != nil {
		t.Fatalf("写入配置失败：%v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig 失败：%v", err)
	}
	if cfg.ListenAddr != "0.0.0.0:1080" {
		t.Errorf("ListenAddr = %q，期望覆盖值 0.0.0.0:1080", cfg.ListenAddr)
	}
	if cfg.RulesPath != "my-rules.txt" {
		t.Errorf("RulesPath = %q，期望 my-rules.txt", cfg.RulesPath)
	}
	if cfg.GeoAutoUpdateDays != 3 {
		t.Errorf("GeoAutoUpdateDays = %d，期望 3", cfg.GeoAutoUpdateDays)
	}
	// 未出现的字段保持默认。
	if cfg.GeoDir != "geo" {
		t.Errorf("GeoDir = %q，期望默认 geo", cfg.GeoDir)
	}
	if cfg.AllowUDP {
		t.Error("AllowUDP 应为默认 false")
	}
}

// TestLoadConfigExplicitZero 验证文件中显式 0 能关闭自动更新（JSON 覆盖发生在
// 默认值之上，"0" 是真实值而非缺省）。
func TestLoadConfigExplicitZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"geo_auto_update_days": 0}`), 0o600); err != nil {
		t.Fatalf("写入配置失败：%v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig 失败：%v", err)
	}
	if cfg.GeoAutoUpdateDays != 0 {
		t.Errorf("GeoAutoUpdateDays = %d，显式 0 应关闭自动更新", cfg.GeoAutoUpdateDays)
	}
}

// TestLoadConfigInvalidJSON 验证解析失败报错且不改动原文件。
func TestLoadConfigInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	garbage := []byte("{ not valid json !!!")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatalf("写入配置失败：%v", err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Fatal("非法 JSON 应返回错误")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取原文件失败：%v", err)
	}
	if string(data) != string(garbage) {
		t.Fatal("解析失败不应改动原文件")
	}
}

// TestWriteConfigPerm 验证写入的配置文件权限为 0600。
func TestWriteConfigPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := WriteConfig(path, DefaultConfig()); err != nil {
		t.Fatalf("WriteConfig 失败：%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat 失败：%v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("配置文件权限为 %o，期望 600", perm)
	}
}

// TestGeoURLs 验证 GeoRepo 推导下载地址、空仓库回退到 route 内置默认，
// 以及默认下载加速前缀（DownloadProxy）的拼接。
func TestGeoURLs(t *testing.T) {
	cfg := DefaultConfig()
	// 默认 DownloadProxy = https://gh-proxy.org/ → GitHub 官方 URL 前拼接加速。
	if got, want := cfg.GeoSiteURL(), "https://gh-proxy.org/https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geosite.dat"; got != want {
		t.Errorf("GeoSiteURL = %q，期望 %q", got, want)
	}
	if got, want := cfg.GeoIPURL(), "https://gh-proxy.org/https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip-lite.dat"; got != want {
		t.Errorf("GeoIPURL = %q，期望 %q", got, want)
	}

	// 仓库尾斜杠不应产生双斜杠。
	cfg.GeoRepo = "https://github.com/owner/repo/"
	if got := cfg.GeoSiteURL(); got != "https://gh-proxy.org/https://github.com/owner/repo/releases/download/latest/geosite.dat" {
		t.Errorf("带尾斜杠 GeoSiteURL = %q", got)
	}

	// 空仓库回退内置默认（同样加速）。
	cfg.GeoRepo = ""
	if got := cfg.GeoSiteURL(); got != "https://gh-proxy.org/"+route.DefaultGeoSiteURL {
		t.Errorf("空仓库 GeoSiteURL = %q，期望加速内置 %q", got, "https://gh-proxy.org/"+route.DefaultGeoSiteURL)
	}
	if got := cfg.GeoIPURL(); got != "https://gh-proxy.org/"+route.DefaultGeoIPURL {
		t.Errorf("空仓库 GeoIPURL = %q，期望加速内置 %q", got, "https://gh-proxy.org/"+route.DefaultGeoIPURL)
	}
}

// TestDownloadProxy 验证加速前缀的行为：
//   - 非 GitHub 域名（镜像仓库、本地测试）不加速
//   - DownloadProxy 置空可完全关闭加速
//   - 自定义加速前缀生效
func TestDownloadProxy(t *testing.T) {
	cfg := DefaultConfig()

	// 非 github.com 的 GeoRepo（如 gitee 镜像）不加加速前缀。
	cfg.GeoRepo = "https://gitee.com/mirrors/meta-rules-dat"
	if got := cfg.GeoSiteURL(); got != "https://gitee.com/mirrors/meta-rules-dat/releases/download/latest/geosite.dat" {
		t.Errorf("非 GitHub 仓库不应加速，得到 %q", got)
	}

	// DownloadProxy 置空 → 完全关闭加速，GitHub URL 原样。
	cfg = DefaultConfig()
	cfg.DownloadProxy = ""
	if got := cfg.GeoSiteURL(); got != "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geosite.dat" {
		t.Errorf("DownloadProxy 空时不应加速，得到 %q", got)
	}

	// 自定义加速前缀生效（形如 https://gh-proxy.org/ 的任意前缀）。
	cfg = DefaultConfig()
	cfg.DownloadProxy = "https://mirror.example.com/"
	if got := cfg.GeoSiteURL(); !strings.HasPrefix(got, "https://mirror.example.com/https://github.com/") {
		t.Errorf("自定义加速前缀未生效：%q", got)
	}

	// AccelerateURL 对 raw.githubusercontent.com 同样生效（规则下载用）。
	cfg = DefaultConfig()
	if got := cfg.AccelerateURL(route.DefaultRulesURL); got != "https://gh-proxy.org/"+route.DefaultRulesURL {
		t.Errorf("AccelerateURL(raw) = %q，期望 %q", got, "https://gh-proxy.org/"+route.DefaultRulesURL)
	}
	// 非 GitHub URL 不加速。
	if got := cfg.AccelerateURL("https://example.com/x.txt"); got != "https://example.com/x.txt" {
		t.Errorf("AccelerateURL(非GitHub) = %q，应原样", got)
	}
}
