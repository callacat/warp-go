//go:build linux

package sysproxy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGsettings 在临时目录生成一个记录调用参数、可按需失败的假 gsettings 脚本，
// 并把 PATH 前置到该目录，使 set() 的 exec.LookPath 命中假脚本而非真实桌面工具。
// 测试结束自动还原 PATH。返回调用日志路径。
func fakeGsettings(t *testing.T, fail bool) string {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$@\" >> \"" + logPath + "\"\n"
	if fail {
		script += "exit 1\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "gsettings"), []byte(script), 0o755); err != nil {
		t.Fatalf("写入假 gsettings 失败：%v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func readCalls(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取调用日志失败：%v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// TestLinuxSetEnable 验证启用系统代理时按序写入 http/https/socks 的 host 与
// port，最后切换 mode=manual——混合代理同端口服务三种协议。
func TestLinuxSetEnable(t *testing.T) {
	logPath := fakeGsettings(t, false)
	if err := Set("127.0.0.1:8080", true); err != nil {
		t.Fatalf("Set 失败：%v", err)
	}

	want := []string{
		"set org.gnome.system.proxy.http host 127.0.0.1",
		"set org.gnome.system.proxy.http port 8080",
		"set org.gnome.system.proxy.https host 127.0.0.1",
		"set org.gnome.system.proxy.https port 8080",
		"set org.gnome.system.proxy.socks host 127.0.0.1",
		"set org.gnome.system.proxy.socks port 8080",
		"set org.gnome.system.proxy mode manual",
	}
	got := readCalls(t, logPath)
	if len(got) != len(want) {
		t.Fatalf("gsettings 调用数为 %d，期望 %d：%v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 次调用 = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestLinuxSetDisable 验证禁用时只切回 mode=none（保留已填地址便于恢复）。
func TestLinuxSetDisable(t *testing.T) {
	logPath := fakeGsettings(t, false)
	if err := Set("127.0.0.1:8080", false); err != nil {
		t.Fatalf("Set 失败：%v", err)
	}

	got := readCalls(t, logPath)
	if len(got) != 1 || got[0] != "set org.gnome.system.proxy mode none" {
		t.Fatalf("禁用时调用异常：%v", got)
	}
}

// TestLinuxGsettingsFailure 验证 gsettings 失败时错误向上传播而非静默。
func TestLinuxGsettingsFailure(t *testing.T) {
	fakeGsettings(t, true)
	if err := Set("127.0.0.1:8080", true); err == nil {
		t.Fatal("gsettings 失败时应返回错误")
	}
}
