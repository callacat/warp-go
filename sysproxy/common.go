// Package sysproxy 设置/清除操作系统级代理，供 CLI（-sysproxy）与 GUI 共用。
package sysproxy

import (
	"fmt"
	"net"
	"strings"
)

// Set 将系统代理指向 addr（形如 "host:port"，host 可为域名或 IP，IPv6 需方括号）。
// enabled=false 时清除系统代理（还原到禁用状态）。
//
// 各平台实现：
//   - windows：写入 HKCU Internet Settings 的 ProxyEnable/ProxyServer
//   - darwin：对每个网络服务执行 networksetup（web/secure 代理）
//   - linux：gsettings（GNOME）org.gnome.system.proxy
func Set(addr string, enabled bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("系统代理地址 %q 非法：%w", addr, err)
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("系统代理地址 %q 缺少主机名", addr)
	}
	if strings.TrimSpace(port) == "" {
		return fmt.Errorf("系统代理地址 %q 缺少端口", addr)
	}
	return set(host, port, enabled)
}
