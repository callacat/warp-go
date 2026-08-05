package tunnel

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// newTestBundle 构造一个无真实 quic.Conn 的 connBundle——health 判定
// （noteProgressingCONNECTFailure）只碰 healthMu/failureSince/failureTargets，
// 均同包可构造；receivedPackets() 对 nil quicConn 返回 0（等价"交换期间无新包"）。
func newTestBundle() *connBundle {
	return &connBundle{
		healthMu:       sync.Mutex{},
		failureTargets: make(map[string]int),
	}
}

// timeoutErr 模拟 CONNECT 交换超时（deadline exceeded）——Android 真机对
// 不可达目标（如 IPv6 [2001::1]:443，物理网络无 IPv6 路由）的典型失败。
// 必须用 context.DeadlineExceeded：isTimeout 的第一分支 errors.Is 命中它。
func timeoutErr() error { return context.DeadlineExceeded }

// closedErr 模拟连接级错误（socket 被关）——应触发立即重连。
func closedErr() error { return net.ErrClosed }

// TestConnectFailureSingleTargetTimeoutKeepsSharedConnection 是 v0.5.21 回归：
// 单个目标 CONNECT 超时 + 交换期间无新包，不得判定"路径黑洞"而淘汰共享连接
// （Android 真机：IPv6 不可达目标超时 → retireConnection → 其他并发流撞上
// 关闭的 socket → use of closed network connection）。v0.5.23 计数语义下
// 首次失败记入窗口（1/2），仍不触发重连。
func TestConnectFailureSingleTargetTimeoutKeepsSharedConnection(t *testing.T) {
	b := newTestBundle()
	if got := b.connectFailureRequiresReconnect(timeoutErr(), nil, "2001::1:443", 0); got {
		t.Fatal("单个目标首次超时不应立即重连（会污染共享连接）：期望 false，得到 true")
	}
}

// TestConnectFailureSameTargetTwiceTriggersReconnect 是 v0.5.23 核心回归：
// 浏览器对同一站点并发重试时，同一目标反复 CONNECT 超时也必须触发重连。
// v0.5.21 的 distinct 去重让同目标失败永不累计（用户日志：Facebook/IPv6
// 两目标 × 各 2 次超时 = distinct 2 < 3 → 隧道黑洞后外网永久不通）。
func TestConnectFailureSameTargetTwiceTriggersReconnect(t *testing.T) {
	b := newTestBundle()
	if b.connectFailureRequiresReconnect(timeoutErr(), nil, "69.171.235.22:443", 0) {
		t.Fatal("同目标第 1 次超时不应触发重连")
	}
	if !b.connectFailureRequiresReconnect(timeoutErr(), nil, "69.171.235.22:443", 0) {
		t.Fatal("同目标窗口内第 2 次超时应触发重连（v0.5.21 distinct 去重导致永不触发）")
	}
}

// TestConnectFailureDistinctTargetsWindow 验证路径黑洞仍能在更早阈值被检出：
// 窗口内 2 个不同目标超时 → 触发重连。第 1 个目标记入窗口返回 false，第 2 个
// 不同目标达到阈值返回 true（阈值从 3 收紧到 2，恢复更及时）。
func TestConnectFailureDistinctTargetsWindow(t *testing.T) {
	b := newTestBundle()
	targets := []string{"a:443", "b:443", "c:443"}
	for i, target := range targets {
		got := b.connectFailureRequiresReconnect(timeoutErr(), nil, target, 0)
		want := i >= 1 // 第 2 个起（窗口内累计 ≥2 次失败）均触发重连
		if got != want {
			t.Fatalf("目标 %d (%s)：期望 %v，得到 %v", i+1, target, want, got)
		}
	}
}

// TestConnectFailureNonTimeoutErrorReconnects 验证非超时连接级错误仍立即重连
// （socket 被关 = 连接级故障，必须重建）。
func TestConnectFailureNonTimeoutErrorReconnects(t *testing.T) {
	b := newTestBundle()
	if !b.connectFailureRequiresReconnect(closedErr(), nil, "x:443", 0) {
		t.Fatal("非超时连接级错误应立即重连：期望 true")
	}
}

// TestConnectFailureWindowExpiry 验证窗口超时（30s）后重置计数——旧的单目标
// 失败不累积到新窗口（否则一次短暂 IPv6 故障会永久污染判定）。
func TestConnectFailureWindowExpiry(t *testing.T) {
	b := newTestBundle()
	now := time.Now()
	if b.noteProgressingCONNECTFailure("a:443", now) {
		t.Fatal("第 1 个目标不应触发重连")
	}
	if !b.noteProgressingCONNECTFailure("a:443", now.Add(1*time.Second)) {
		t.Fatal("窗口内同目标第 2 次失败应触发重连")
	}
	// 窗口过期（31s 后）：计数重置，从 0 开始。
	if b.noteProgressingCONNECTFailure("c:443", now.Add(31*time.Second)) {
		t.Fatal("窗口过期后第 1 个目标应重置窗口，不触发重连")
	}
	if !b.noteProgressingCONNECTFailure("d:443", now.Add(32*time.Second)) {
		t.Fatal("重置后第 2 个不同目标应触发重连")
	}
}

// TestConnectFailureSuccessResetsWindow 验证一次成功 CONNECT 清空失败窗口
// （noteCONNECTSuccess）——隧道恢复后旧失败不残留，避免累积误判。
func TestConnectFailureSuccessResetsWindow(t *testing.T) {
	b := newTestBundle()
	if b.noteProgressingCONNECTFailure("a:443", time.Now()) {
		t.Fatal("窗口内第 1 次失败不应触发重连（计数 1/2）")
	}
	if b.failureSince.IsZero() || len(b.failureTargets) == 0 {
		t.Fatal("失败窗口未记录")
	}
	b.noteCONNECTSuccess()
	if !b.failureSince.IsZero() || b.failureTargets != nil {
		t.Fatal("成功 CONNECT 后失败窗口应清空")
	}
}
