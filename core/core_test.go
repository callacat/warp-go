package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewResolvesPathsToExecDir 验证 New() 把默认相对路径锚定到可执行文件目录
// （GUI 双击启动时工作目录可能漂移，文件必须跟着可执行文件走）。
func TestNewResolvesPathsToExecDir(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable 不可用：%v", err)
	}
	exeDir := filepath.Dir(exe)

	s := New(Options{})
	if !filepath.IsAbs(s.opts.ConfigPath) {
		t.Errorf("ConfigPath 未锚定：%q", s.opts.ConfigPath)
	}
	if !strings.HasPrefix(s.opts.ConfigPath, exeDir) {
		t.Errorf("ConfigPath 不在可执行目录：%q（期望前缀 %q）", s.opts.ConfigPath, exeDir)
	}
	if !filepath.IsAbs(s.opts.StateFile) {
		t.Errorf("StateFile 未锚定：%q", s.opts.StateFile)
	}
	if !strings.HasPrefix(s.opts.StateFile, exeDir) {
		t.Errorf("StateFile 不在可执行目录：%q", s.opts.StateFile)
	}
}

// TestNewKeepsAbsolutePaths 验证显式绝对路径不被改写。
func TestNewKeepsAbsolutePaths(t *testing.T) {
	s := New(Options{
		ConfigPath: "/tmp/warp-config.json",
		StateFile:  "/tmp/warp-reg.json",
	})
	if s.opts.ConfigPath != "/tmp/warp-config.json" {
		t.Errorf("绝对 ConfigPath 被改写：%q", s.opts.ConfigPath)
	}
	if s.opts.StateFile != "/tmp/warp-reg.json" {
		t.Errorf("绝对 StateFile 被改写：%q", s.opts.StateFile)
	}
}

// TestResolveExecPathEmpty 验证空串原样返回（不 panic）。
func TestResolveExecPathEmpty(t *testing.T) {
	if got := resolveExecPath(""); got != "" {
		t.Errorf("空串应原样返回，得到 %q", got)
	}
}

// TestNewResolvesToDataDir 验证 DataDir 非空时，相对路径锚定到 DataDir
// （Android 沙箱场景：可执行文件在 /system/bin 只读，运行时文件必须进沙箱）。
func TestNewResolvesToDataDir(t *testing.T) {
	s := New(Options{
		ConfigPath: "config.json",
		StateFile:  "reg.json",
		DataDir:    "/data/data/com.wails.app/files",
	})
	if s.opts.ConfigPath != "/data/data/com.wails.app/files/config.json" {
		t.Errorf("ConfigPath 未锚定到 DataDir：%q", s.opts.ConfigPath)
	}
	if s.opts.StateFile != "/data/data/com.wails.app/files/reg.json" {
		t.Errorf("StateFile 未锚定到 DataDir：%q", s.opts.StateFile)
	}
}

// TestNewDataDirKeepsAbsolute 验证 DataDir 存在时显式绝对路径仍不被改写。
func TestNewDataDirKeepsAbsolute(t *testing.T) {
	s := New(Options{
		ConfigPath: "/tmp/warp-config.json",
		StateFile:  "/tmp/warp-reg.json",
		DataDir:    "/data/data/com.wails.app/files",
	})
	if s.opts.ConfigPath != "/tmp/warp-config.json" {
		t.Errorf("绝对 ConfigPath 被改写：%q", s.opts.ConfigPath)
	}
	if s.opts.StateFile != "/tmp/warp-reg.json" {
		t.Errorf("绝对 StateFile 被改写：%q", s.opts.StateFile)
	}
}

// TestNewEmptyDataDirStillExecAnchors 验证 DataDir 为空时行为与默认一致
// （桌面/CLI 回归锁：仍锚定到可执行文件目录）。
func TestNewEmptyDataDirStillExecAnchors(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable 不可用：%v", err)
	}
	exeDir := filepath.Dir(exe)
	s := New(Options{})
	if !strings.HasPrefix(s.opts.ConfigPath, exeDir) {
		t.Errorf("空 DataDir 下 ConfigPath 未走默认锚定：%q（期望前缀 %q）", s.opts.ConfigPath, exeDir)
	}
	if !strings.HasPrefix(s.opts.StateFile, exeDir) {
		t.Errorf("空 DataDir 下 StateFile 未走默认锚定：%q（期望前缀 %q）", s.opts.StateFile, exeDir)
	}
}

// TestEnsureConfigAnchorsToDataDir 验证 ensureConfig 把 config.json 内的
// 相对 rules_path / geo_dir 也锚定到 DataDir（Android 沙箱完整一致性）。
func TestEnsureConfigAnchorsToDataDir(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfgJSON := `{"rules_path":"rules.txt","geo_dir":"geo"}`
	if err := os.WriteFile(cfgPath, []byte(cfgJSON), 0o600); err != nil {
		t.Fatalf("写入测试配置失败：%v", err)
	}

	s := New(Options{ConfigPath: cfgPath, DataDir: dir})
	cfg, err := s.ensureConfig()
	if err != nil {
		t.Fatalf("ensureConfig 失败：%v", err)
	}
	if cfg.RulesPath != filepath.Join(dir, "rules.txt") {
		t.Errorf("RulesPath 未锚定到 DataDir：%q（期望 %q）", cfg.RulesPath, filepath.Join(dir, "rules.txt"))
	}
	if cfg.GeoDir != filepath.Join(dir, "geo") {
		t.Errorf("GeoDir 未锚定到 DataDir：%q（期望 %q）", cfg.GeoDir, filepath.Join(dir, "geo"))
	}
}
