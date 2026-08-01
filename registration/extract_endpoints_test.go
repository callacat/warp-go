package registration

import "testing"

// TestExtractPeerEndpointsV4 验证带端口 IPv4 端点提取为主机名（V4 有值时
// 不受 Host 字段影响）。
func TestExtractPeerEndpointsV4(t *testing.T) {
	v4, v6, ports := extractPeerEndpoints("162.159.198.2:4500", "", nil, "host.example.com")
	if v4 != "162.159.198.2" {
		t.Errorf("V4 应剥掉端口得到主机：%q", v4)
	}
	if v6 != "" {
		t.Errorf("V6 应为空：%q", v6)
	}
	if ports != nil {
		t.Errorf("ports 应为 nil：%v", ports)
	}
}

// TestExtractPeerEndpointsV4EmptyUsesHost 验证 V4 为空时回退到 Endpoint.Host
// （API 只返回 host 而不返回 v4/v6 的场景——修复前 endpoint_v4 恒为空，
// 导致扫描回退候选 ":443"）。
func TestExtractPeerEndpointsV4EmptyUsesHost(t *testing.T) {
	v4, _, _ := extractPeerEndpoints("", "", nil, "edge.example.com")
	if v4 != "edge.example.com" {
		t.Errorf("V4 为空时应回退 Host：%q", v4)
	}
}

// TestExtractPeerEndpointsV6 验证带方括号 IPv6 端点剥括号。
func TestExtractPeerEndpointsV6(t *testing.T) {
	v4, v6, _ := extractPeerEndpoints("", "[2606:4700:103::2]:443", nil, "")
	if v6 != "2606:4700:103::2" {
		t.Errorf("V6 应剥掉方括号：%q", v6)
	}
	if v4 != "" {
		t.Errorf("V4 应为空：%q", v4)
	}
}

// TestExtractPeerEndpointsAllEmpty 验证全部为空时返回空串（调用方负责报错）。
func TestExtractPeerEndpointsAllEmpty(t *testing.T) {
	v4, v6, ports := extractPeerEndpoints("", "", nil, "")
	if v4 != "" || v6 != "" || ports != nil {
		t.Errorf("全空输入应返回空：v4=%q v6=%q ports=%v", v4, v6, ports)
	}
}

// TestExtractPeerEndpointsPorts 验证端口透传。
func TestExtractPeerEndpointsPorts(t *testing.T) {
	ports := []int{443, 500}
	_, _, got := extractPeerEndpoints("1.2.3.4:443", "", ports, "")
	if len(got) != 2 || got[0] != 443 || got[1] != 500 {
		t.Errorf("端口未透传：%v", got)
	}
}
