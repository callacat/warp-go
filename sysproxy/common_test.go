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
		})
	}
}
