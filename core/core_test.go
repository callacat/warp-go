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
