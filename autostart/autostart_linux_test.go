//go:build linux

package autostart

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnableDisableLinux 用临时 HOME 验证 .desktop 生成/删除（不碰真实用户配置）。
func TestEnableDisableLinux(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	exe := "/usr/local/bin/warp-gui"
	if err := Enable(exe); err != nil {
		t.Fatalf("Enable 失败：%v", err)
	}
	if !Enabled() {
		t.Fatal("Enable 后 Enabled() 应为 true")
	}

	path := filepath.Join(tmp, ".config", "autostart", desktopName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 .desktop 失败：%v", err)
	}
	want := "Exec=" + exe + "\n"
	if got := string(data); !contains(got, want) {
		t.Errorf(".desktop 缺少 Exec 行：\n%s", got)
	}

	if err := Disable(); err != nil {
		t.Fatalf("Disable 失败：%v", err)
	}
	if Enabled() {
		t.Fatal("Disable 后 Enabled() 应为 false")
	}
}

// TestDisableIdempotent 验证重复 Disable 不报错。
func TestDisableIdempotent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := Disable(); err != nil {
		t.Fatalf("未启用时 Disable 应返回 nil：%v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
