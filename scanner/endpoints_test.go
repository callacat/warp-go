package scanner

import (
	"net"
	"strings"
	"testing"
)

// TestDefaultV4CIDRs 验证默认 IPv4 CIDR 段正好 5 段且内容固定。
func TestDefaultV4CIDRs(t *testing.T) {
	got := DefaultV4CIDRs()
	want := []string{
		"162.159.192.0/24",
		"162.159.193.0/24",
		"162.159.195.0/24",
		"162.159.197.0/24",
		"162.159.198.0/24",
	}
	if len(got) != len(want) {
		t.Fatalf("DefaultV4CIDRs 段数 = %d，期望 %d（got=%v）", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DefaultV4CIDRs[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestDefaultV6CIDRs 验证默认 IPv6 CIDR 段正好 2 段。
func TestDefaultV6CIDRs(t *testing.T) {
	got := DefaultV6CIDRs()
	want := []string{
		"2606:4700:d0::/48",
		"2606:4700:d1::/48",
	}
	if len(got) != len(want) {
		t.Fatalf("DefaultV6CIDRs 段数 = %d，期望 %d（got=%v）", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("DefaultV6CIDRs[%d] = %q，期望 %q", i, got[i], want[i])
		}
	}
}

// TestDefaultPorts_UsesRegWhenNonEmpty 验证非空 reg 原样返回。
func TestDefaultPorts_UsesRegWhenNonEmpty(t *testing.T) {
	reg := []int{443, 500, 1701, 4500, 4443, 8443, 8095}
	got := DefaultPorts(reg)
	if len(got) != len(reg) {
		t.Fatalf("DefaultPorts 长度 = %d，期望 %d", len(got), len(reg))
	}
	for i := range reg {
		if got[i] != reg[i] {
			t.Errorf("DefaultPorts[%d] = %d，期望 %d", i, got[i], reg[i])
		}
	}
}

// TestDefaultPorts_EmptyRegFallsBack443 验证空或 nil 入参兜底 [443]。
func TestDefaultPorts_EmptyRegFallsBack443(t *testing.T) {
	for _, reg := range [][]int{nil, {}} {
		got := DefaultPorts(reg)
		if len(got) != 1 || got[0] != 443 {
			t.Fatalf("DefaultPorts(%v) = %v，期望 [443]", reg, got)
		}
	}
}

// TestBuildCandidates_5CIDRs_7Ports_16PerIP 验证 5 段 / 7 端口 / 每段 16 IP 的总候选数与格式。
func TestBuildCandidates_5CIDRs_7Ports_16PerIP(t *testing.T) {
	cidrs := DefaultV4CIDRs()
	ports := []int{443, 500, 1701, 4500, 4443, 8443, 8095}
	got := BuildCandidates(cidrs, ports, DefaultPerIPLimit)

	wantCount := 5 * 16 * 7 // 560
	if len(got) != wantCount {
		t.Fatalf("候选总数 = %d，期望 %d", len(got), wantCount)
	}

	// 每个候选必须是 IPv4 无方括号形式，且不含 .0/.255，端口顺序保持。
	for _, hpu := range got {
		host, _, err := net.SplitHostPort(hpu)
		if err != nil {
			t.Errorf("候选 %q 无法 SplitHostPort：%v", hpu, err)
			continue
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() == nil {
			t.Errorf("候选 %q 的 host=%q 不是 IPv4", hpu, host)
			continue
		}
		// IPv4 不应出现方括号（即原始字符串不应以 '[' 开头）。
		if strings.HasPrefix(hpu, "[") {
			t.Errorf("候选 %q 的 IPv4 不应带方括号", hpu)
		}
	}
}

// TestBuildCandidates_IPv6Brackets 验证 IPv6 用 net.JoinHostPort 自动加方括号。
func TestBuildCandidates_IPv6Brackets(t *testing.T) {
	got := BuildCandidates([]string{"2606:4700:d0::/48"}, []int{443}, 4)
	if len(got) != 4 {
		t.Fatalf("候选总数 = %d，期望 4（got=%v）", len(got), got)
	}
	for i, hpu := range got {
		host, port, err := net.SplitHostPort(hpu)
		if err != nil {
			t.Fatalf("候选 %q 无法 SplitHostPort：%v", hpu, err)
		}
		if port != "443" {
			t.Errorf("候选[%d] 端口 = %q，期望 443", i, port)
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.To4() != nil {
			t.Errorf("候选 %q 的 host=%q 不是 IPv6", hpu, host)
		}
		// IPv6 应以 '[' 开头。
		if !strings.HasPrefix(hpu, "[") {
			t.Errorf("候选 %q 的 IPv6 应带方括号", hpu)
		}
	}
}

// TestBuildCandidates_PerIPLimit0 全展开 /24 应得到 254 个去 .0/.255 后的候选。
func TestBuildCandidates_PerIPLimit0(t *testing.T) {
	got := BuildCandidates([]string{"162.159.198.0/24"}, []int{443}, 0)
	if len(got) != 254 {
		t.Fatalf("候选总数 = %d，期望 254", len(got))
	}
	for _, hpu := range got {
		host, _, _ := net.SplitHostPort(hpu)
		ip := net.ParseIP(host).To4()
		if ip == nil {
			t.Errorf("候选 %q 不是 IPv4", hpu)
			continue
		}
		last := ip[3]
		if last == 0 || last == 255 {
			t.Errorf("候选 %q 命中网络/广播地址", hpu)
		}
	}
}

// TestBuildCandidates_MalformedCIDRSkipped 验证非法 CIDR 被跳过不 panic。
func TestBuildCandidates_MalformedCIDRSkipped(t *testing.T) {
	got := BuildCandidates([]string{"not-a-cidr", "162.159.198.0/24"}, []int{443}, 16)
	if len(got) != 16 {
		t.Fatalf("候选总数 = %d，期望 16（只有后一段贡献，非法段应被跳过）", len(got))
	}
	for _, hpu := range got {
		host, _, _ := net.SplitHostPort(hpu)
		if !strings.HasPrefix(host, "162.159.198.") {
			t.Errorf("候选 %q 不应来自非法段", hpu)
		}
	}
}

// TestBuildCandidates_PortsDedup 验证端口去重后保持首次出现顺序。
func TestBuildCandidates_PortsDedup(t *testing.T) {
	got := BuildCandidates([]string{"162.159.198.0/24"}, []int{443, 443, 500}, 1)
	// 单 IP，去重后 2 个端口 → 2 个候选，且端口依次为 443、500。
	if len(got) != 2 {
		t.Fatalf("候选总数 = %d，期望 2（端口去重后 443、500）", len(got))
	}
	for i, want := range []string{"443", "500"} {
		_, port, _ := net.SplitHostPort(got[i])
		if port != want {
			t.Errorf("候选[%d] 端口 = %q，期望 %q", i, port, want)
		}
	}
}
