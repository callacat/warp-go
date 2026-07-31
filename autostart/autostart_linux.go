//go:build linux

package autostart

import (
	"fmt"
	"os"
	"path/filepath"
)

const desktopName = "warp-go.desktop"

func desktopPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户主目录失败：%w", err)
	}
	return filepath.Join(home, ".config", "autostart", desktopName), nil
}

func enable(execPath string) error {
	path, err := desktopPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("创建 autostart 目录失败：%w", err)
	}
	content := fmt.Sprintf(`[Desktop Entry]
Type=Application
Name=warp-go
Comment=Cloudflare WARP 客户端
Exec=%s
Terminal=false
X-GNOME-Autostart-enabled=true
`, execPath)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("写入 autostart .desktop 失败：%w", err)
	}
	return nil
}

func disable() error {
	path, err := desktopPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 autostart .desktop 失败：%w", err)
	}
	return nil
}

func enabled() bool {
	path, err := desktopPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}
