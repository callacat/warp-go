//go:build windows

package sysproxy

import (
	"fmt"
	"net"

	"golang.org/x/sys/windows/registry"
)

// set 写入 HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings：
// 启用时先写 ProxyServer 再置 ProxyEnable=1；禁用时只清 ProxyEnable（保留
// ProxyServer 便于用户手动恢复）。只改当前用户，不需要管理员权限。
func set(host, port string, enabled bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`,
		registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("打开 Internet Settings 注册表键失败：%w", err)
	}
	defer k.Close()

	if !enabled {
		if err := k.SetDWordValue("ProxyEnable", 0); err != nil {
			return fmt.Errorf("写入 ProxyEnable=0 失败：%w", err)
		}
		return nil
	}

	// Windows 的 ProxyServer 值对每种协议单独给出地址；混合代理同端口服务
	// HTTP/HTTPS/SOCKS5，三种都指向同一地址。JoinHostPort 保证 IPv6 字面量
	// 带方括号，否则 [::1]:40000 会被解析成非法的 host:port。
	ep := net.JoinHostPort(host, port)
	proxyServer := fmt.Sprintf("http=%s;https=%s;socks=%s", ep, ep, ep)
	if err := k.SetStringValue("ProxyServer", proxyServer); err != nil {
		return fmt.Errorf("写入 ProxyServer 失败：%w", err)
	}
	if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
		return fmt.Errorf("写入 ProxyEnable=1 失败：%w", err)
	}
	return nil
}
