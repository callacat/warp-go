//go:build android || linux

// Package androidvpn 提供 Android VPNService（TUN）集成。
//
// decision.go 只包含纯路由判定逻辑（host/ip → proxy/direct/reject）与
// 无 Android 依赖的类型定义，因此在宿主（linux）与 Android 都能编译，
// 可被单元测试直接覆盖（androidvpn.go 的 tun-stack 代码仅在 Android 构建）。
package androidvpn

import (
	"context"
	"net"
	"net/netip"
)

// RouteFunc 判定 (host, ip) 的转发行为，返回 ("proxy"|"direct"|"reject", matched)。
// 与 core 的 route.Engine.Match 语义一致；nil 时全部走 proxy（隧道）。
type RouteFunc func(host string, ip netip.Addr) (action string, matched bool)

// TunnelDial 建立到目标的 WARP 隧道字节流（core.MasqueClient.DialTunnel）。
type TunnelDial func(ctx context.Context, targetAddr string) (net.Conn, error)

// DirectDial 建立到目标的本地直连（nil 时用 net.Dialer）。
type DirectDial func(ctx context.Context, targetAddr string) (net.Conn, error)

// Config 配置 TUN 服务。
type Config struct {
	// FileDescriptor 是 Java VpnService.Builder.establish() 的 TUN fd。
	FileDescriptor int
	// MTU 默认 1500；由 Java 侧传入（与 VpnService.Builder.setMtu 一致）。
	MTU uint32
	// Inet4Address / Inet6Address 是分配给 TUN 网卡的隧道内地址
	// （来自注册信息 assigned_ipv4/assigned_ipv6）。
	Inet4Address []netip.Prefix
	Inet6Address []netip.Prefix
	// DNSServers 是 TUN 内 DNS 服务器（默认 1.1.1.1）。
	DNSServers []netip.Addr

	Route      RouteFunc
	TunnelDial TunnelDial
	DirectDial DirectDial
}

// decideAction 把 (host, ip) 判定为 proxy / direct / reject。
//
// 语义（与桌面端 route.Engine 一致）：
//   - route == nil          → ("proxy", true)   全流量走隧道
//   - route 命中            → 透传其 action（"proxy"/"direct"/"reject"），matched=true
//   - route 未命中          → ("direct", false) 隐式 direct 兜底
//
// reject 命中时调用方必须关闭连接（绝不建连），与 M6 桌面 REJECT 行为一致
// （SOCKS5 0x02 / HTTP 403）。
func decideAction(route RouteFunc, host string, ip netip.Addr) (action string, matched bool) {
	if route == nil {
		return "proxy", true
	}
	action, matched = route(host, ip)
	if !matched {
		return "direct", false
	}
	return action, true
}
