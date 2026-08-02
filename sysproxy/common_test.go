package sysproxy

import (
	"testing"
)

// TestSetInvalidAddr 验证非法地址在触碰系统之前就被拒绝（net.SplitHostPort +
// 主机/端口非空检查），各平台通用。注意：端口是否数字不由本层校验——Linux 的
// gsettings 与 Windows 注册表都把端口当字符串写入。
func TestSetInvalidAddr(t *testing.T) {
	cases := []struct {
		name string
		addr string
	}{
		{"空地址", ""},
		{"缺端口", "127.0.0.1"},
		{"缺主机名", ":8080"},
		{"多个冒号", "127.0.0.1:8080:extra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Set(tc.addr, true); err == nil {
				t.Errorf("Set(%q, true) 应返回错误", tc.addr)
			}
			if err := Set(tc.addr, false); err == nil {
				t.Errorf("Set(%q, false) 应返回错误", tc.addr)
			}
			if _, err := Enabled(tc.addr); err == nil {
				t.Errorf("Enabled(%q) 应返回错误", tc.addr)
			}
		})
	}
}

// TestContainsTarget 验证 ProxyServer 字符串解析：命中任一协议段即 true，
// 前缀 "proto=" 被忽略，; / , 都是分隔符。
func TestContainsTarget(t *testing.T) {
	ep := "127.0.0.1:40000"
	cases := []struct {
		name string
		cfg  string
		want bool
	}{
		{"命中 http", "http=127.0.0.1:40000;https=127.0.0.1:40000;socks=127.0.0.1:40000", true},
		{"命中 https", "http=10.0.0.1:8080;https=127.0.0.1:40000", true},
		{"只命中 socks", "http=10.0.0.1:8080;socks=127.0.0.1:40000", true},
		{"未命中", "http=10.0.0.1:8080;https=10.0.0.1:8080", false},
		{"逗号分隔", "127.0.0.1:40000,10.0.0.1:8080", true},
		{"无前缀单值", "127.0.0.1:40000", true},
		{"空串", "", false},
		{"IPv6 方括号（不同地址）", "http=[::1]:40000;https=[::1]:40000", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := containsTarget(tc.cfg, ep); got != tc.want {
				t.Errorf("containsTarget(%q) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}
