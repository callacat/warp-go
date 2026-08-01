package core

import (
	"strings"
	"testing"

	"warp/registration"
)

// TestScanFallbackValidV4 验证注册信息含 IPv4 边缘地址时，回退候选正确生成。
func TestScanFallbackValidV4(t *testing.T) {
	reg := &registration.Registration{EndpointV4: "162.159.198.2"}
	got, err := scanFallback(reg, "4")
	if err != nil {
		t.Fatalf("scanFallback 返回错误：%v", err)
	}
	if len(got) != 1 || got[0] != "162.159.198.2:443" {
		t.Errorf("IPv4 回退候选错误：%v", got)
	}
}

// TestScanFallbackMissingV4 验证 IPv4 边缘地址缺失时返回清晰错误（而非
// 生成 ":443" 垃圾候选——修复前 net.JoinHostPort("","443") 的行为）。
func TestScanFallbackMissingV4(t *testing.T) {
	reg := &registration.Registration{}
	got, err := scanFallback(reg, "4")
	if err == nil {
		t.Fatalf("IPv4 缺失时应报错，得到 %v", got)
	}
	if !strings.Contains(err.Error(), "缺少 IPv4 边缘地址") {
		t.Errorf("错误信息不含可操作提示：%v", err)
	}
}

// TestScanFallbackValidV6 验证注册信息含 IPv6 边缘地址时，回退候选正确生成。
func TestScanFallbackValidV6(t *testing.T) {
	reg := &registration.Registration{EndpointV6: "2606:4700:103::2"}
	got, err := scanFallback(reg, "6")
	if err != nil {
		t.Fatalf("scanFallback 返回错误：%v", err)
	}
	if len(got) != 1 || got[0] != "[2606:4700:103::2]:443" {
		t.Errorf("IPv6 回退候选错误：%v", got)
	}
}

// TestScanFallbackMissingV6 验证 IPv6 边缘地址缺失时返回清晰错误。
func TestScanFallbackMissingV6(t *testing.T) {
	reg := &registration.Registration{}
	got, err := scanFallback(reg, "6")
	if err == nil {
		t.Fatalf("IPv6 缺失时应报错，得到 %v", got)
	}
	if !strings.Contains(err.Error(), "缺少 IPv6 边缘地址") {
		t.Errorf("错误信息不含可操作提示：%v", err)
	}
}

// TestScanFallbackExplicitSpec 验证显式端点原样返回（不扫描语义不变）。
func TestScanFallbackExplicitSpec(t *testing.T) {
	reg := &registration.Registration{}
	got, err := scanFallback(reg, "1.2.3.4:443")
	if err != nil {
		t.Fatalf("显式端点不应报错：%v", err)
	}
	if len(got) != 1 || got[0] != "1.2.3.4:443" {
		t.Errorf("显式端点未原样返回：%v", got)
	}
}
