package tunnel

import (
	"errors"
	"net/http"
	"testing"
)

// TestProbeVerdictStatusCodeLocked 锁定探测判据：收到 HTTP 响应即出口可达，
// 只有传输层失败才算失败。v0.6.7 把 www.cloudflare.com 的 403 判失败，导致
// 全部边缘被跳过 → CT103 数据面 0/30（2026-10-04 02:13 回滚）。
func TestProbeVerdictStatusCodeLocked(t *testing.T) {
	for _, code := range []int{200, 301, 403, 500, 503} {
		if err := probeVerdict(&http.Response{StatusCode: code}, nil); err != nil {
			t.Errorf("收到 %d 响应应判出口可达，得到错误 %v", code, err)
		}
	}
	// 出口真被掐的表现是 CONNECT 超时 / 流错误（压根读不回状态行），仍判失败。
	if err := probeVerdict(nil, errors.New("读取 CONNECT 响应失败：http3 deadline")); err == nil {
		t.Error("CONNECT 超时应判探测失败")
	}
}
