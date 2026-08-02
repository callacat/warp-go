package core

import "testing"

// TestCompareVersions 覆盖语义版本比较的边界：主/次/补丁、缺段、非数字。
func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"0.5.3", "0.5.3", 0},
		{"0.5.4", "0.5.3", 1},
		{"0.5.2", "0.5.3", -1},
		{"0.6.0", "0.5.99", 1},
		{"1.0.0", "0.9.9", 1},
		{"0.5", "0.5.0", 0},       // 缺段视为 0
		{"0.5.10", "0.5.9", 1},    // 逐段比较，非字符串比较
		{"x.y.z", "0.0.0", 0},     // 非数字段按 0
		{"", "", 0},               // 空串
		{"0.5.3-rc1", "0.5.3", 0}, // 预发布后缀忽略（parse 只取前 3 段数字）
	}
	for _, tt := range tests {
		if got := compareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// TestParseVersion 验证拆段：缺段补 0，非法段归 0。
func TestParseVersion(t *testing.T) {
	if got := parseVersion("0.5.3"); got != [3]int{0, 5, 3} {
		t.Errorf("parseVersion(0.5.3) = %v", got)
	}
	if got := parseVersion("1.2"); got != [3]int{1, 2, 0} {
		t.Errorf("parseVersion(1.2) = %v", got)
	}
	if got := parseVersion("abc"); got != [3]int{0, 0, 0} {
		t.Errorf("parseVersion(abc) = %v", got)
	}
}
