//go:build linux || android

package androidvpn

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

// TestDecideAction 覆盖 host/ip → action 的纯路由判定（T3 里程碑：Android
// 路径在宿主机可测）。语义与桌面 route.Engine 一致：未命中隐式 direct 兜底；
// reject 命中必须原样返回，由调用方关闭连接。
func TestDecideAction(t *testing.T) {
	const host = "example.com"

	// T6 记录 route 实际收到的参数（geoip 匹配零值 Addr 的场景）。
	var gotHost string
	var gotIP netip.Addr

	tests := []struct {
		name        string
		route       RouteFunc
		ip          netip.Addr
		wantAction  string
		wantMatched bool
		check       func(*testing.T)
	}{
		{
			// T1: 无 RouteFunc → 全流量走隧道。
			name:        "nil route → proxy all",
			route:       nil,
			wantAction:  "proxy",
			wantMatched: true,
		},
		{
			// T2: 命中 proxy → 走隧道。
			name: "matched proxy → proxy",
			route: func(host string, ip netip.Addr) (string, bool) {
				return "proxy", true
			},
			wantAction:  "proxy",
			wantMatched: true,
		},
		{
			// T3: 命中 direct → 本地直连。
			name: "matched direct → direct",
			route: func(host string, ip netip.Addr) (string, bool) {
				return "direct", true
			},
			wantAction:  "direct",
			wantMatched: true,
		},
		{
			// T4: 未命中 → VPN 隧道兜底 proxy（v0.5.18 无法互联网根因修复）。
			// TUN 收到 IP 字面量 → route.Match 只走 geoip → 国外 IP 不命中
			// → 若非 proxy 兜底则直连被墙。VPN 语义：除显式 direct/reject
			// （私有段、中国大陆）外全部走隧道。
			name: "no match → proxy fallback (VPN tunnel default)",
			route: func(host string, ip netip.Addr) (string, bool) {
				return "", false
			},
			wantAction:  "proxy",
			wantMatched: true,
		},
		{
			// T5: 命中 reject → 原样返回，调用方必须关闭连接（新路径）。
			name: "matched reject → reject",
			route: func(host string, ip netip.Addr) (string, bool) {
				return "reject", true
			},
			wantAction:  "reject",
			wantMatched: true,
		},
		{
			// T6: Android 调用方传 netip.Addr{} 零值 → route 收到零值；
			// geoip 匹配零值 Addr 查不到 → 未命中 → 隧道兜底 proxy。
			name: "geoip zero addr → route receives netip.Addr{}, proxy fallback",
			route: func(host string, ip netip.Addr) (string, bool) {
				gotHost, gotIP = host, ip
				return "", false
			},
			ip:          netip.Addr{},
			wantAction:  "proxy",
			wantMatched: true,
			check: func(t *testing.T) {
				if gotHost != host {
					t.Errorf("route received host %q, want %q", gotHost, host)
				}
				if gotIP != (netip.Addr{}) {
					t.Errorf("route received ip %v, want zero netip.Addr{}", gotIP)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, matched := decideAction(tt.route, host, tt.ip)
			if action != tt.wantAction {
				t.Errorf("decideAction() action = %q, want %q", action, tt.wantAction)
			}
			if matched != tt.wantMatched {
				t.Errorf("decideAction() matched = %v, want %v", matched, tt.wantMatched)
			}
			if tt.check != nil {
				tt.check(t)
			}
		})
	}
}

// TestDecideTunnelTarget 锁定 v0.5.25 的拨号目标契约：IP→域名还原**只用于
// proxy 分支**；direct 分支必须保留原始 IP 拨号（v0.5.24 回归根因：无条件
// 还原域名让 direct 也走 net.Dialer 物理解析 → 系统 DNS 又进 TUN → 环路
// canceled——真机日志 `拨号失败 49.7.252.24:443：lookup
// obus-cn.dc.heytapmobi.com: canceled`）。
func TestDecideTunnelTarget(t *testing.T) {
	origAddr := "49.7.252.24:443"
	domain := "obus-cn.dc.heytapmobi.com"

	tests := []struct {
		name       string
		action     string
		mappedHost string
		wantTarget string
	}{
		{
			// proxy + 映射命中 → 用域名（隧道内再次 DoH 解析，CONNECT 目标
			// 永远边缘可达——v0.5.24 根因修复的正确路径）。
			name:       "proxy with domain mapping → tunnel dials domain",
			action:     "proxy",
			mappedHost: domain,
			wantTarget: domain + ":443",
		},
		{
			// direct → 保留原始 IP（v0.5.24 回归：direct 还原域名会触发
			// net.Dialer 物理解析 → 系统 DNS 又进 TUN → 环路 canceled）。
			// 该 IP 本身是隧道 DoH 解析出的真实 IP，物理网络同样可达。
			name:       "direct with domain mapping → keeps original IP",
			action:     "direct",
			mappedHost: domain,
			wantTarget: origAddr,
		},
		{
			// 映射 miss（IP 直连/未拦截查询）→ 无条件用原始 IP；proxy 走
			// 隧道也尽力（边缘可达与否由上层 DialTunnel 处理）。
			name:       "proxy without mapping → keeps original IP",
			action:     "proxy",
			mappedHost: "",
			wantTarget: origAddr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := decideTunnelTarget(tt.action, origAddr, tt.mappedHost)
			if target != tt.wantTarget {
				t.Errorf("decideTunnelTarget(%q) = %q, want %q", tt.action, target, tt.wantTarget)
			}
		})
	}
}

// TestDecideActionForwardsIP 锁定 NewConnectionEx 的调用契约（T8）：调用方
// 把真实目标 IP 传给 decideAction 后，必须原样透传给 RouteFunc——geoip:
// 规则正是靠这个 ip 参数命中 IP 字面量目标。域名目标（ip 为 netip.Addr{}
// 零值）同样原样透传，host/geosite 规则照常生效。
func TestDecideActionForwardsIP(t *testing.T) {
	// Given: 记录 route 收到的 host 与 ip 的录制型 RouteFunc。
	var gotHost string
	var gotIP netip.Addr
	record := func(host string, ip netip.Addr) (string, bool) {
		gotHost, gotIP = host, ip
		return "direct", true
	}

	tests := []struct {
		name     string
		host     string
		ip       netip.Addr
		wantHost string
		wantIP   netip.Addr
	}{
		{
			// IP 字面量目标：sing Socksaddr.Addr 为真实 IP，route 必须收到
			// 同一个 IP（geoip:private / geoip:cn 等规则因此可命中）。
			name:     "IP literal destination → route receives real IP",
			host:     "1.2.3.4",
			ip:       netip.MustParseAddr("1.2.3.4"),
			wantHost: "1.2.3.4",
			wantIP:   netip.MustParseAddr("1.2.3.4"),
		},
		{
			// 域名目标：sing Socksaddr.Addr 恒为零值，route 收到零值
			// （退化与修复前一致，仅 host/geosite 规则生效）。
			name:     "domain destination → route receives zero Addr",
			host:     "example.com",
			ip:       netip.Addr{},
			wantHost: "example.com",
			wantIP:   netip.Addr{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// When: 调用方调用序列（NewConnectionEx 的实际判定调用）。
			action, matched := decideAction(record, tt.host, tt.ip)

			// Then: action 命中透传，route 原样收到 host 与 ip。
			if action != "direct" || !matched {
				t.Errorf("decideAction() = (%q, %v)，期望 (\"direct\", true)", action, matched)
			}
			if gotHost != tt.wantHost {
				t.Errorf("route received host %q, want %q", gotHost, tt.wantHost)
			}
			if gotIP != tt.wantIP {
				t.Errorf("route received ip %v, want %v", gotIP, tt.wantIP)
			}
		})
	}
}

// TestDecideActionRejectNeverDialed 锁定 NewConnectionEx 的调用契约：路由命中
// reject 时 decideAction 返回 ("reject", true)，随后 resolveAction 必须返回
// (nil, error, true) —— 隧道拨号与本地直连函数都绝不能被调用（与桌面端
// proxy 包"reject 不建立连接"行为一致）。
func TestDecideActionRejectNeverDialed(t *testing.T) {
	const host = "blocked.example.com"
	routeReject := func(string, netip.Addr) (string, bool) { return "reject", true }

	// Given: 记录拨号是否被触碰的隧道/直连拨号函数。
	tunnelDialed := false
	directDialed := false
	tunnelDial := func(context.Context, string) (net.Conn, error) {
		tunnelDialed = true
		return nil, errors.New("should not be called")
	}
	directDial := func(context.Context, string) (net.Conn, error) {
		directDialed = true
		return nil, errors.New("should not be called")
	}

	// When: 完整判定 + 解析流程（NewConnectionEx 的实际调用序列）。
	action, matched := decideAction(routeReject, host, netip.Addr{})
	upstream, err, rejected := resolveAction(action, context.Background(), host+":443", tunnelDial, directDial)

	// Then: reject 原样透传，连接被拒绝，两个拨号函数零调用。
	if action != "reject" || !matched {
		t.Errorf("decideAction() = (%q, %v)，期望 (\"reject\", true)", action, matched)
	}
	if !rejected {
		t.Error("resolveAction() rejected = false，期望 true（reject 必须拒连）")
	}
	if upstream != nil {
		t.Errorf("resolveAction() upstream = %v，期望 nil（reject 绝不建连）", upstream)
	}
	if err == nil {
		t.Error("resolveAction() err = nil，期望 reject 错误")
	} else if !errors.Is(err, rejectErr) {
		t.Errorf("resolveAction() err = %v，期望 errors.Is(err, rejectErr)", err)
	}
	if tunnelDialed {
		t.Error("命中 reject 时 TunnelDial 被调用，期望零调用")
	}
	if directDialed {
		t.Error("命中 reject 时 DirectDial 被调用，期望零调用")
	}
}

// TestResolveAction 覆盖 resolveAction 全部分支的拨号委托契约：
// proxy → TunnelDial；direct → DirectDial / 默认 net.Dialer；reject → 拒连。
func TestResolveAction(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name         string
		action       string
		target       string
		tunnelDial   TunnelDial
		directDial   DirectDial
		wantRejected bool
		wantErr      bool
		wantTunneled bool
		wantDirect   bool
	}{
		{
			name:         "reject → 拒连，绝不拨号",
			action:       "reject",
			tunnelDial:   func(context.Context, string) (net.Conn, error) { return nil, errors.New("unexpected") },
			directDial:   func(context.Context, string) (net.Conn, error) { return nil, errors.New("unexpected") },
			wantRejected: true,
			wantErr:      true,
		},
		{
			name:         "proxy → TunnelDial",
			action:       "proxy",
			tunnelDial:   func(context.Context, string) (net.Conn, error) { a, _ := net.Pipe(); return a, nil },
			wantTunneled: true,
		},
		{
			name:       "proxy 但 TunnelDial 未配置 → 错误",
			action:     "proxy",
			wantErr:    true,
			wantDirect: false,
		},
		{
			name:       "direct → DirectDial",
			action:     "direct",
			directDial: func(context.Context, string) (net.Conn, error) { a, _ := net.Pipe(); return a, nil },
			wantDirect: true,
		},
		{
			name:    "direct 但 DirectDial 未配置 → 默认 net.Dialer（回环端口拒绝，无网络依赖）",
			action:  "direct",
			target:  "127.0.0.1:1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := tt.target
			if target == "" {
				target = "example.com:443"
			}
			upstream, err, rejected := resolveAction(tt.action, ctx, target, tt.tunnelDial, tt.directDial)
			if rejected != tt.wantRejected {
				t.Errorf("rejected = %v，期望 %v", rejected, tt.wantRejected)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v，期望 err != nil: %v", err, tt.wantErr)
			}
			if tt.wantRejected && upstream != nil {
				t.Errorf("reject 时 upstream = %v，期望 nil", upstream)
			}
			if tt.wantTunneled && tt.tunnelDial == nil {
				t.Errorf("proxy 分支应使用 TunnelDial")
			}
			if tt.wantDirect && tt.directDial == nil {
				t.Errorf("direct 分支应使用 DirectDial")
			}
			if upstream != nil {
				_ = upstream.Close()
			}
		})
	}
}

// TestUDPKind 锁定 debugdiag UDP 直连流类别：53 → dns（非拦截 DNS 泄漏），
// 443 → quic（浏览器 HTTP/3 直接泄漏），其余 → udp。
func TestUDPKind(t *testing.T) {
	tests := []struct {
		port uint16
		want string
	}{
		{53, "dns"},
		{443, "quic"},
		{123, "udp"},
		{0, "udp"},
		{8080, "udp"},
		{65535, "udp"},
	}
	for _, tt := range tests {
		if got := udpKind(tt.port); got != tt.want {
			t.Errorf("udpKind(%d) = %q，期望 %q", tt.port, got, tt.want)
		}
	}
}

// TestShouldBlockUDP 锁定 QUIC:443 拦截判定（v0.5.28 阶段5）：只有 443 端口
// 返回 true（浏览器 HTTP/3 探测），其余 UDP 放行直连。拦截后丢弃包，浏览器
// 回退 TCP:443 → WARP 隧道。
func TestShouldBlockUDP(t *testing.T) {
	tests := []struct {
		port uint16
		want bool
	}{
		{443, true},   // QUIC:443 → 拦截
		{53, false},   // DNS → DNS 拦截路径处理，不到这里
		{80, false},   // HTTP/3 非标准端口 → 放行（极少见）
		{123, false},  // NTP → 放行
		{8080, false}, // 其他 → 放行
		{0, false},    // 非法端口 → 放行
	}
	for _, tt := range tests {
		if got := shouldBlockUDP(tt.port); got != tt.want {
			t.Errorf("shouldBlockUDP(%d) = %v，期望 %v", tt.port, got, tt.want)
		}
	}
}
