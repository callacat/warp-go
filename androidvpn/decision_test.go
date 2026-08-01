//go:build linux || android

package androidvpn

import (
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
			// T4: 未命中 → 隐式 direct 兜底（与桌面一致）。
			name: "no match → direct fallback",
			route: func(host string, ip netip.Addr) (string, bool) {
				return "", false
			},
			wantAction:  "direct",
			wantMatched: false,
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
			// geoip 匹配零值 Addr 查不到 → 未命中（matched=false → direct）。
			name: "geoip zero addr → route receives netip.Addr{}",
			route: func(host string, ip netip.Addr) (string, bool) {
				gotHost, gotIP = host, ip
				return "", false
			},
			ip:          netip.Addr{},
			wantAction:  "direct",
			wantMatched: false,
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
