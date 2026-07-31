//go:build linux

package sysproxy

import (
	"fmt"
	"os/exec"
)

// set 通过 gsettings（GNOME）配置系统代理：启用时先写各协议的 host/port 再
// 切 mode=manual；禁用时切回 mode=none（保留已填地址便于恢复）。非 GNOME
// 桌面没有 gsettings 或没有 org.gnome.system.proxy schema，此时返回明确的
// 错误并提示替代方案，而不是静默失败。
func set(host, port string, enabled bool) error {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return fmt.Errorf("Linux 桌面代理需 gsettings（GNOME）；未找到 gsettings，" +
			"可改用环境变量 http_proxy/https_proxy/all_proxy 或手动配置系统代理")
	}

	if !enabled {
		return gsettings("org.gnome.system.proxy", "mode", "none")
	}

	// 混合代理同端口服务 HTTP/HTTPS/SOCKS5，三种 schema 都指向同一地址。
	for _, proto := range []string{"http", "https", "socks"} {
		base := "org.gnome.system.proxy." + proto
		if err := gsettings(base, "host", host); err != nil {
			return err
		}
		if err := gsettings(base, "port", port); err != nil {
			return err
		}
	}
	return gsettings("org.gnome.system.proxy", "mode", "manual")
}

func gsettings(schema, key, value string) error {
	if err := exec.Command("gsettings", "set", schema, key, value).Run(); err != nil {
		return fmt.Errorf("gsettings set %s %s %q 失败：%w", schema, key, value, err)
	}
	return nil
}
