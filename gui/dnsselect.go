//go:build android || linux

package main

import (
	"net/netip"

	"warp/androidvpn"
)

// selectAndroidTunnelDNS 选择 Android TUN DNS 拦截的解析器（v0.6.6 DNS 回流
// 修复，t_b1f681d1 根因闭环）。front-proxy（百度中转）开启时 kernel 的
// ResolveDNS 委托 FrontProxyDialer.ResolveDNS = net.LookupIP（系统解析器），
// 系统 DNS 又指向 TUN 内 198.18.0.1（WarpVpnService addDnsServer）→ UDP:53
// 再进 HandleQuery → 递归回流自锁至超时。此时换 protect() 物理 DNS 直连
// 解析器阻断回流；否则保持 kernel.ResolveDNS（隧道内 DoH，v0.5.24 语义）。
//
// 不破坏 direct/proxy 分流：HandleQuery 仍先 usePhysical 判 direct，本选择
// 只替换「proxy/未命中」分支的解析器。桌面/CLI 不经过这里（无 TUN DNS 拦截，
// TunnelDNS 恒 nil）。
func selectAndroidTunnelDNS(frontProxyEnabled bool, physicalDNS []netip.Addr, kernelDNS androidvpn.ResolveFunc) androidvpn.ResolveFunc {
	if frontProxyEnabled {
		return androidvpn.NewPhysicalDNSResolver(physicalDNS)
	}
	return kernelDNS
}