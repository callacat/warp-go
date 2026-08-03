//go:build windows

package sysproxy

import (
	"fmt"
	"net"

	"golang.org/x/sys/windows/registry"
)

// set 写入 HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings：
// 启用时先写 ProxyServer、ProxyOverride（本机/局域网旁路）再置
// ProxyEnable=1；禁用时只清 ProxyEnable（保留 ProxyServer 便于用户手动
// 恢复）。只改当前用户，不需要管理员权限。
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
	// <local> 是 Windows 专用 token：本机与局域网地址（含回环）一律不经
	// 代理。旁路本地服务（如 WebSSH 网关），避免其流量被转回代理端口。
	if err := k.SetStringValue("ProxyOverride", "<local>;localhost"); err != nil {
		return fmt.Errorf("写入 ProxyOverride 失败：%w", err)
	}
	if err := k.SetDWordValue("ProxyEnable", 1); err != nil {
		return fmt.Errorf("写入 ProxyEnable=1 失败：%w", err)
	}
	return nil
}

// enabled 读 HKCU Internet Settings 的 ProxyEnable/ProxyServer，报告系统
// 代理是否启用且指向 (host, port)。其它软件（VPN/代理工具）关闭系统代理
// 时 ProxyEnable=0 → 返回 false，GUI 据此同步开关。
func enabled(host, port string) (bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`,
		registry.QUERY_VALUE)
	if err != nil {
		return false, fmt.Errorf("打开 Internet Settings 注册表键失败：%w", err)
	}
	defer k.Close()

	enable, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil {
		return false, fmt.Errorf("读取 ProxyEnable 失败：%w", err)
	}
	if enable == 0 {
		return false, nil
	}
	proxyServer, _, err := k.GetStringValue("ProxyServer")
	if err != nil {
		return false, fmt.Errorf("读取 ProxyServer 失败：%w", err)
	}
	// ProxyServer 形如 "http=host:port;https=host:port;socks=host:port"。
	// 任一协议段指向目标地址即视为"本程序设置的代理仍在"。
	ep := net.JoinHostPort(host, port)
	return containsTarget(proxyServer, ep), nil
}
