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

// Enabled 报告系统代理当前是否启用且指向 addr（本程序设置的地址）。
// 用于检测"外部软件关闭了系统代理"——GUI 轮询它来同步开关状态。
// addr 形如 "host:port"，与 Set 一致。
func Enabled(addr string) (bool, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false, fmt.Errorf("系统代理地址 %q 非法：%w", addr, err)
	}
	if strings.TrimSpace(host) == "" {
		return false, fmt.Errorf("系统代理地址 %q 缺少主机名", addr)
	}
	if strings.TrimSpace(port) == "" {
		return false, fmt.Errorf("系统代理地址 %q 缺少端口", addr)
	}
	return enabled(host, port)
}

// containsTarget 检查代理配置字符串中是否含 "host:port" 段（按 ; 与 ,
// 分隔的协议段，忽略 "proto=" 前缀）。命中任一即 true。Windows 的
// ProxyServer（"http=host:port;https=host:port;socks=host:port"）用它判断
// 系统代理是否仍指向目标地址。
func containsTarget(proxyConfig, ep string) bool {
	for _, seg := range strings.FieldsFunc(proxyConfig, func(r rune) bool {
		return r == ';' || r == ','
	}) {
		seg = strings.TrimSpace(seg)
		if i := strings.IndexByte(seg, '='); i >= 0 {
			seg = seg[i+1:]
		}
		if seg == ep {
			return true
		}
	}
	return false
}
