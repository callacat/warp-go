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
		failureTargets: make(map[string]struct{}),
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
// 关闭的 socket → use of closed network connection）。当前实现（红）对
// "无新包超时"立即返回 true；修复后应记入 failure-window 返回 false。
func TestConnectFailureSingleTargetTimeoutKeepsSharedConnection(t *testing.T) {
	b := newTestBundle()
	// 第一个超时目标：记入窗口（1/3），不触发重连。
	got := b.connectFailureRequiresReconnect(timeoutErr(), nil, "2001::1:443", 0)
	if got {
		t.Fatal("单个目标超时无新包不应立即重连（会污染共享连接）：期望 false，得到 true")
	}
	// 同目标再次超时：仍只算 1 个 distinct target，不重连。
	if got := b.connectFailureRequiresReconnect(timeoutErr(), nil, "2001::1:443", 0); got {
		t.Fatal("同一目标重复超时不应累计到窗口阈值：期望 false")
	}
}

// TestConnectFailureDistinctTargetsWindow 验证真正的路径黑洞仍能被检出：
// 窗口内 3 个不同目标超时 → 触发重连（恢复能力不退化）。第 1、2 个目标
// 记入窗口返回 false，第 3 个不同目标达到阈值返回 true。
func TestConnectFailureDistinctTargetsWindow(t *testing.T) {
	b := newTestBundle()
	targets := []string{"a:443", "b:443", "c:443"}
	for i, target := range targets {
		got := b.connectFailureRequiresReconnect(timeoutErr(), nil, target, 0)
		want := i == 2 // 第 3 个不同目标才触发重连
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
	if b.noteProgressingCONNECTFailure("b:443", now.Add(1*time.Second)) {
		t.Fatal("第 2 个目标（窗口内）不应触发重连")
	}
	// 窗口过期（31s 后）：计数重置，从 0 开始。
	if b.noteProgressingCONNECTFailure("c:443", now.Add(31*time.Second)) {
		t.Fatal("窗口过期后第 1 个目标应重置窗口，不触发重连")
	}
	if b.noteProgressingCONNECTFailure("d:443", now.Add(32*time.Second)) {
		t.Fatal("重置后第 2 个目标不应触发")
	}
	if !b.noteProgressingCONNECTFailure("e:443", now.Add(33*time.Second)) {
		t.Fatal("重置后第 3 个不同目标应触发重连")
	}
}
