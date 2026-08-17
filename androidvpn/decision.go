//go:build android || linux

// Package androidvpn 提供 Android VPNService（TUN）集成。
//
// decision.go 只包含纯路由判定逻辑（host/ip → proxy/direct/reject）与
// 无 Android 依赖的类型定义，因此在宿主（linux）与 Android 都能编译，
// 可被单元测试直接覆盖（androidvpn.go 的 tun-stack 代码仅在 Android 构建）。
package androidvpn

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"syscall"
)

// socketProtector, when non-nil, is invoked with the raw fd of every direct
// (non-tunnel) socket the TUN stack dials, right after creation and before any
// packet is sent. Android injects VpnService.protect via SetSocketProtector so
// direct sockets egress over the physical network instead of looping back into
// the TUN (v0.5.14 protected only the QUIC dial socket; direct sockets without
// protect caused a loop storm — see relayUDP/NewConnectionEx). Desktop/CLI
// leave this nil (no-op).
var socketProtector func(fd int) error

// SetSocketProtector 注册包级 socket 保护器，供 Android 桥注入
// VpnService.protect 实现。桌面/CLI 不调用，保持 nil。
func SetSocketProtector(fn func(fd int) error) {
	socketProtector = fn
}

// protectConn 对已建连接的底层 socket 调用 socketProtector（UDP 路径）。
func protectConn(conn syscall.Conn) {
	if socketProtector == nil {
		return
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		log.Printf("⚠ 获取直连 socket 原始 fd 失败：%v", err)
		return
	}
	_ = raw.Control(func(fd uintptr) {
		if perr := socketProtector(int(fd)); perr != nil {
			log.Printf("⚠ 保护直连 socket（fd=%d）失败：%v", int(fd), perr)
		}
	})
}

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
	// TunnelDNS 是隧道内域名解析函数（Android 桥注入
	// *core.Kernel.ResolveDNS → 隧道内 DoH）。非 nil 时启用 TUN DNS 拦截
	// （androidvpn.go）：Java 侧把系统 DNS 指向 TUN 内拦截服务器
	// （198.18.0.1），UDP:53 查询经隧道 DoH 解析——只有这种 IP 才是 WARP
	// 边缘网络视图可达的（v0.5.24 Android 外网根因）；解析结果同时记录
	// IP→域名映射，NewConnectionEx 对 TCP 目标 IP 查表还原域名后走
	// DialTunnel，保证 CONNECT 目标永远边缘可达。nil 时不拦截（桌面/CLI
	// 无 TUN，恒 nil）。
	TunnelDNS ResolveFunc
}

// decideTunnelTarget 决定拨号目标字符串（v0.5.25 修复）。
//
// IP→域名还原**只用于 proxy 分支**：隧道 DialTunnel 收到域名时内部再次
// 隧道 DoH 解析，CONNECT 目标永远边缘可达（v0.5.24 Android 外网根因修复的
// 正确路径）。direct 分支必须保留原始 IP——v0.5.24 回归根因：无条件还原
// 域名让 direct 也走 net.Dialer 物理解析，系统 DNS 又进 TUN → 环路 canceled
// （真机日志 `拨号失败 49.7.252.24:443：lookup obus-cn.dc.heytapmobi.com:
// canceled`）。该 IP 是隧道 DoH 解析出的真实 IP，物理网络同样可达。
func decideTunnelTarget(action, origAddr string, mappedHost string) string {
	if action != "proxy" || mappedHost == "" {
		return origAddr
	}
	_, port, err := net.SplitHostPort(origAddr)
	if err != nil {
		return origAddr
	}
	return net.JoinHostPort(mappedHost, port)
}

// quicBlockPort 是触发 QUIC 拦截的 UDP 端口。HTTP/3（QUIC）标准端口 443，
// 浏览器在该端口做 QUIC 探测；拦截后丢弃包，浏览器回退 HTTP/2 over TCP
// （Chrome/Firefox 标准行为），TCP 经 NewConnectionEx 走 WARP 隧道。
const quicBlockPort uint16 = 443

// shouldBlockUDP 判定 TUN 内 UDP 流是否应拦截（丢弃而非直连）。
// 返回 true 时 NewPacketConnectionEx 丢弃包并关闭连接，强制上层回退 TCP。
//
// 当前只拦截 QUIC:443：上游 warp-svc 只有 ConnectTcpProxy（不支持
// CONNECT-UDP / RFC 9298），UDP 无法走隧道；运营商封 UDP/QUIC 直连 →
// 浏览器外网打不开。拦截后浏览器回退 TCP:443 → WARP 隧道 → 通。
//
// 纯函数（host-compilable），可单测；NewPacketConnectionEx 调用它。
func shouldBlockUDP(port uint16) bool {
	return port == quicBlockPort
}

// udpKind 把 UDP 直连流端口分类为 debugdiag 遥测类别（host-compilable
// 纯函数）：53 → dns（非拦截 DNS 泄漏），443 → quic（浏览器 HTTP/3 直接
// 泄漏），其余 → udp。两类泄漏在真机日志可见：
// `[tun] UDP ...:443（直连）` 与 `[tun] UDP ...:53（直连）`。
func udpKind(port uint16) string {
	switch port {
	case 53:
		return "dns"
	case 443:
		return "quic"
	default:
		return "udp"
	}
}

// rejectErr 是命中 reject 规则时返回给调用方的错误（与 M6 桌面端
// proxy.errRejected 语义一致：连接被规则拒绝，绝不建连）。
var rejectErr = errors.New("rejected by route")

// errBareV6Proxy 标记 proxy 分支的裸 IPv6 目标被本地快速拒绝：IP→域名映射
// miss（该 IP 从未经隧道 DNS 解析，只有本地视图可见），CONNECT 到 WARP
// 边缘不可达——A15 双栈 hang 到 deadline（firstByteMs=-1），A14 边缘快拒。
// 本地拒绝让客户端立即收到 connection refused → Happy Eyeballs 回退 v4。
var errBareV6Proxy = errors.New("bare IPv6 target not reachable via tunnel")

// shouldRejectBareV6 判定 proxy 分支的裸 IPv6 字面量目标是否应本地快速拒绝
// （v0.5.29 洞 B 兜底）。Dns 拦截已让 AAAA 查询不再泄漏到物理 DNS
// （dns.go noData 空应答），但已缓存的污染 v6 / 应用自带解析器 / 硬编码
// v6 IP 仍会产生裸 v6 流：隧道 DNS 解析出的 v6（映射命中，mappedHost 非空）
// 边缘可达、放行；其余裸 v6 在边缘不可达，本地立即拒连而不是挂到 deadline，
// 让客户端快速回退 v4。direct/reject 分支不在此判定内（reject 由路由规则
// 决定、direct 走本地直连）；v4 与域名目标（Addr 零值）一律放行。
func shouldRejectBareV6(action string, ip netip.Addr, mappedHost string) bool {
	return action == "proxy" && mappedHost == "" && ip.Is6() && !ip.Is4In6()
}

// resolveAction 把判定结果解析为上游连接：proxy → TunnelDial；
// direct → DirectDial（nil 时 net.Dialer）；reject → (nil, rejectErr, true)
// 绝不拨号。second=true 表示 reject 命中，调用方必须立即关闭连接。
// 与 decideAction 同属 host-compilable 逻辑，reject 分支契约由此可单测。
func resolveAction(action string, ctx context.Context, targetAddr string, tunnelDial TunnelDial, directDial DirectDial) (net.Conn, error, bool) {
	switch action {
	case "reject":
		return nil, rejectErr, true
	case "proxy":
		if tunnelDial == nil {
			return nil, errors.New("tunnel not configured"), false
		}
		conn, err := tunnelDial(ctx, targetAddr)
		return conn, err, false
	default: // direct
		if directDial != nil {
			conn, err := directDial(ctx, targetAddr)
			return conn, err, false
		}
		// TCP direct：用 Dialer.Control 在建立连接前 protect 底层 fd——
		// 否则 Android 上该 socket 重新进入 TUN 造成环路风暴（v0.5.14
		// 只 protect 了 QUIC 拨号 socket，direct 未豁免）。
		dialer := &net.Dialer{}
		if socketProtector != nil {
			dialer.Control = func(network, address string, c syscall.RawConn) error {
				return c.Control(func(fd uintptr) {
					if perr := socketProtector(int(fd)); perr != nil {
						log.Printf("⚠ 保护直连 socket（fd=%d）失败：%v", int(fd), perr)
					}
				})
			}
		}
		conn, err := dialer.DialContext(ctx, "tcp", targetAddr)
		return conn, err, false
	}
}

// decideAction 把 (host, ip) 判定为 proxy / direct / reject。
//
// 语义（与桌面端 route.Engine 在"命中规则"上一致，但**未命中兜底不同**）：
//   - route == nil          → ("proxy", true)  全流量走隧道
//   - route 命中            → 透传其 action（"proxy"/"direct"/"reject"），matched=true
//   - route 未命中          → ("proxy", true)  VPN 隧道兜底
//
// 未命中兜底必须是 **proxy 而非 direct**（v0.5.18 无法互联网根因）：TUN 收到
// 的是已解析的 IP 字面量，route.Engine.Match 对 IP 只走 geoip 规则（geosite/
// domain 的域名语义对 IP 无意义）→ 国外目标（如 Google 的 172.217.x）不命中
// 默认规则里唯一一条 geoip-proxy（telegram）→ miss。若兜底 direct，国外流量
// 全落本地直连 → 被墙 i/o timeout、浏览器不通。VPN 语义是"除显式 direct/reject
// （私有段、中国大陆）外全部走隧道"，故兜底 proxy。桌面 SOCKS 拿域名走
// route.Engine（其 miss→direct），不经过本函数，不受影响。
//
// reject 命中时调用方必须关闭连接（绝不建连），与 M6 桌面 REJECT 行为一致
// （SOCKS5 0x02 / HTTP 403）。
func decideAction(route RouteFunc, host string, ip netip.Addr) (action string, matched bool) {
	if route == nil {
		return "proxy", true
	}
	action, matched = route(host, ip)
	if !matched {
		return "proxy", true
	}
	return action, true
}
