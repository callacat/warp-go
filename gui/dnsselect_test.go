//go:build android || linux

package main

import (
	"context"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"warp/androidvpn"
)

// sentinelDNS 是 kernel.ResolveDNS 的测试替身：任何非 nil 方法值都可用指针
// 区分，断言「front-proxy 下 TunnelDNS ≠ kernel.ResolveDNS」只用身份比较。
func sentinelDNS(_ context.Context, host string) (net.IP, error) {
	return net.ParseIP("198.18.0.1"), nil
}

func resolverPtr(f androidvpn.ResolveFunc) uintptr {
	return reflect.ValueOf(f).Pointer()
}

// TestSelectAndroidTunnelDNS 是防回流装配回归（v0.6.6 DNS 回流修复）：
//   - front-proxy（百度中转）开启 → TunnelDNS 换物理直连解析器（≠ kernel.
//     ResolveDNS），阻断 198.18.0.1→TUN→HandleQuery 递归回流；
//   - front-proxy 关闭 → TunnelDNS 保持 kernel.ResolveDNS（v0.5.24 隧道内
//     DoH 语义，桌面/真实隧道路径不变）。
//
// 若有人删除条件分支恒返回 kernelDNS，本条 true 分支即红。
func TestSelectAndroidTunnelDNS(t *testing.T) {
	physicalDNS := []netip.Addr{netip.MustParseAddr("223.5.5.5")}
	kernelDNS := androidvpn.ResolveFunc(sentinelDNS)

	got := selectAndroidTunnelDNS(true, physicalDNS, kernelDNS)
	if got == nil {
		t.Fatal("front-proxy 开启时 TunnelDNS 不应为 nil")
	}
	if resolverPtr(got) == resolverPtr(kernelDNS) {
		t.Fatal("front-proxy 开启时 TunnelDNS 应换物理直连解析器（≠ kernel.ResolveDNS），否则回流自锁（198.18.0.1→TUN→HandleQuery）")
	}

	got = selectAndroidTunnelDNS(false, physicalDNS, kernelDNS)
	if resolverPtr(got) != resolverPtr(kernelDNS) {
		t.Fatal("front-proxy 关闭时 TunnelDNS 应保持 kernel.ResolveDNS（隧道内 DoH），不能替换")
	}
}