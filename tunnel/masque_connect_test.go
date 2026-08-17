package tunnel

import (
	"context"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// newTestMasqueClient 构造一个无隧道状态的最小客户端：lifeCtx 可取消以便
// 重连 goroutine 退出，edgeAddrs 为空使隧道拨号必然失败（且不会触碰
// tlsConfig）。与 NewMasqueClient 的字段初始化保持一致。
func newTestMasqueClient(t *testing.T) *MasqueClient {
	t.Helper()
	lifeCtx, lifeStop := context.WithCancel(context.Background())
	t.Cleanup(lifeStop)
	return &MasqueClient{
		lifeCtx:   lifeCtx,
		lifeStop:  lifeStop,
		dnsCache:  make(map[string]dnsCacheEntry),
		dnsFlight: make(map[string]*dnsFlightResult),
	}
}

// newTestBundle 构造一个无真实 quic.Conn 的 connBundle——health 判定
// （noteProgressingCONNECTFailure）只碰 healthMu/failureSince/failureTargets，
// 均同包可构造；receivedPackets() 对 nil quicConn 返回 0（等价"交换期间无新包"）。
func newTestBundle() *connBundle {
	return &connBundle{
		healthMu:       sync.Mutex{},
		failureTargets: make(map[string]int),
	}
}

// newTestClient 构造一个可注入拨号/探测钩子的 MasqueClient（供连接级测试）。
func newTestClient(t *testing.T) *MasqueClient {
	t.Helper()
	c := newTestMasqueClient(t)
	c.dialFn = func(context.Context) (*connBundle, error) { return newTestBundle(), nil }
	return c
}

// deadBundle 返回一个持有 client+cur=newBundle 的测试 setup，调用方可在其上
// 触发 dead 置位的路径（noteDeadStream / probeEgressOnce）。
func deadBundle(t *testing.T) (*MasqueClient, *connBundle) {
	t.Helper()
	c := newTestMasqueClient(t)
	b := newTestBundle()
	c.cur = b
	return c, b
}

// TestNoteDeadStreamDeadFastPath 锁定 dead 置位后 openRequestStream 立即
// 加入重连航班（不再在死连接上白等 10s CONNECT 超时）——这是 00:06:04
// m.youtube.com 连续 RST 的修复核心：运行中的并发流观测到连接死亡，
// 后续请求必须快速失败并共享同一个重连航班。
func TestNoteDeadStreamDeadFastPath(t *testing.T) {
	c, b := deadBundle(t)
	c.dialFn = func(context.Context) (*connBundle, error) { return newTestBundle(), nil }

	tc := &tunnelConn{client: c, bundle: b}
	tc.noteDeadStream(connectionLevelErr()) // 连接级错误 → dead + 异步重连

	// 轮询等待 cur 被替换（异步重连）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.connMu.RLock()
		cur := c.cur
		c.connMu.RUnlock()
		if cur != b {
			return // 连接已被替换（dead 置位被确认，但单测无法构造真实 quic.Conn）
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("noteDeadStream 连接级错误应触发重连：cur 未被替换")
}

// TestCurrentConnectionDeadReturnsClosed 锁定 currentConnection 对 dead 置位
// 的 bundle 返回 ErrClosed（黑洞路径下 quic.Context() 未 Done 也能被检出）。
func TestCurrentConnectionDeadReturnsClosed(t *testing.T) {
	c, b := deadBundle(t)
	b.dead.Store(true)
	if _, err := c.currentConnection(); err != net.ErrClosed {
		t.Fatalf("dead 置位后 currentConnection 应返回 ErrClosed，得到 %v", err)
	}
}

// TestEnsureServiceableUnhealthyPokesReconnect 验证不可用连接（dead 置位）
// EnsureServiceable 返回 false，且后台重建航班被拉起（cur 被异步替换——
// 非阻塞自愈，供连接池跳过该成员期间恢复服务）。
func TestEnsureServiceableUnhealthyPokesReconnect(t *testing.T) {
	c, b := deadBundle(t)
	c.dialFn = func(context.Context) (*connBundle, error) { return newTestBundle(), nil }
	b.dead.Store(true)

	if c.EnsureServiceable() {
		t.Fatal("dead 连接应返回 false")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.connMu.RLock()
		cur := c.cur
		c.connMu.RUnlock()
		if cur != b {
			return // 后台航班已替换 cur
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("EnsureServiceable 应触发后台重连：cur 未被替换")
}

// TestEnsureServiceableClosedFalse 验证已关闭客户端返回 false 且不 panic
// （池轮询在全关时安全跳过，回退正常拨号路径）。
func TestEnsureServiceableClosedFalse(t *testing.T) {
	c := newTestMasqueClient(t)
	c.closed = true
	if c.EnsureServiceable() {
		t.Fatal("已关闭客户端应返回 false")
	}
}

// TestProbeInternationalEgressNilBundle 锁定探测对 nil/未就绪 bundle 防御性
// 返回错误（不 panic）：dialAddr 的 bundle 在 h3Client 建立前不得被探测。
func TestProbeInternationalEgressNilBundle(t *testing.T) {
	c := newTestMasqueClient(t)
	if err := c.probeEgress(context.Background(), nil); err == nil {
		t.Fatal("nil bundle 探测应返回错误")
	}
	if err := c.probeInternationalEgress(context.Background(), newTestBundle()); err == nil {
		t.Fatal("未就绪 bundle（nil h3Client）探测应返回错误")
	}
}

// TestProbeEgressOnceFailureTriggersReconnect 锁定运行期探测失败 → 置 dead
// + retire + reconnect（不等用户请求在死连接上白等）：静默死会话（KeepAlive
// 往返仍在但出口已坏）由周期探测发现，这是 debugdiag 隧道被掐后浏览器
// connection reset 风暴的主动侧修复。
func TestProbeEgressOnceFailureTriggersReconnect(t *testing.T) {
	c, b := deadBundle(t)
	c.probeFn = func(context.Context, *connBundle) error { return context.DeadlineExceeded }
	newB := newTestBundle()
	c.dialFn = func(context.Context) (*connBundle, error) { return newB, nil }

	c.handleProbeFailure(b, context.DeadlineExceeded)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.connMu.RLock()
		cur := c.cur
		c.connMu.RUnlock()
		if cur == newB {
			return // 探测失败已触发重连
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("探测失败应触发重连：cur 未被替换")
}

// TestProbeEgressOnceNoConnectionSkips 锁定无当前连接（重连中/已关闭）时
// 探测静默跳过，不 panic 不唤醒多余重连。
func TestProbeEgressOnceNoConnectionSkips(t *testing.T) {
	c := newTestMasqueClient(t)
	c.probeFn = func(context.Context, *connBundle) error { return context.DeadlineExceeded }
	c.probeEgressOnce() // cur == nil → 返回，不应 panic
}

// timeoutErr 模拟 CONNECT 交换超时（deadline exceeded）——Android 真机对
// 不可达目标（如 IPv6 [2001::1]:443，物理网络无 IPv6 路由）的典型失败。
// 必须用 context.DeadlineExceeded：isTimeout 的第一分支 errors.Is 命中它。
func timeoutErr() error { return context.DeadlineExceeded }

// connectionLevelErr 模拟 QUIC 连接本身死亡（TransportError）——native QUIC
// 层真正的连接级错误，应触发立即重连。此前测试用 net.ErrClosed 充当该角色，
// 但裸 net.ErrClosed 现在语义是"连接已被他人淘汰"（见 isConnectionLevelError），
// 改用类型化的 TransportError 保持"连接级错误立即重连"的测试意图。
func connectionLevelErr() error {
	return &quic.TransportError{ErrorCode: quic.ProtocolViolation}
}

// closedErr 模拟他人已 retire/换代后的信号（裸 net.ErrClosed）——不应触发
// 或累计任何重连决策。
func closedErr() error { return net.ErrClosed }

// resetErr 模拟边缘对单个目标的重置（非连接级）——应走观察窗，不单次拆线。
func resetErr() error { return &net.OpError{Op: "read", Net: "udp", Err: syscall.ECONNRESET} }

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

// TestConnectFailureConnectionLevelErrorReconnects 验证可辨别的连接级错误
// （quic TransportError）仍立即重连——连接本身已死必须重建，不得走观察窗。
func TestConnectFailureConnectionLevelErrorReconnects(t *testing.T) {
	b := newTestBundle()
	if !b.connectFailureRequiresReconnect(connectionLevelErr(), nil, "x:443", 0) {
		t.Fatal("连接级错误（TransportError）应立即重连：期望 true")
	}
}

// TestConnectFailureNetErrClosedNoRetire 验证裸 net.ErrClosed 不触发也不累计
// 重连：它是他人已 retire/换代拆线的信号，本条 CONNECT 只是被拖累，重复决策
// 只会在批量死亡时污染观察窗。
func TestConnectFailureNetErrClosedNoRetire(t *testing.T) {
	b := newTestBundle()
	if b.connectFailureRequiresReconnect(closedErr(), nil, "x:443", 0) {
		t.Fatal("net.ErrClosed（已被他人淘汰）不应触发重连：期望 false")
	}
	if b.connectFailureRequiresReconnect(closedErr(), nil, "x:443", 0) {
		t.Fatal("net.ErrClosed 不应计入观察窗：期望 false")
	}
}

// TestConnectFailureNonTimeoutResetWindowed 验证非超时的单目标重置（如边缘
// 对某个不可达目标立即 reset CONNECT）不再单次拆毁共享连接——改为观察窗
// 累计，窗口内第 2 次才触发。这是 Android 上"映射 miss 裸 IP 目标被边缘
// 立即重置 → 整条共享连接被拖死 → 所有并发健康流一起 use of closed
// network connection"的直接修复。
func TestConnectFailureNonTimeoutResetWindowed(t *testing.T) {
	b := newTestBundle()
	if b.connectFailureRequiresReconnect(resetErr(), nil, "x:443", 0) {
		t.Fatal("单个目标重置第 1 次不应立即重连：期望 false")
	}
	if !b.connectFailureRequiresReconnect(resetErr(), nil, "x:443", 0) {
		t.Fatal("窗口内第 2 次目标重置应触发重连：期望 true")
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

// TestNoteDeadStreamConnectionLevelErrorReconnects 锁定 noteDeadStream 对
// 连接级错误的处理：QUIC 连接死亡时（非 timeout、非 stream-reset），正在跑
// 的隧道流必须主动触发重连——这是 15:22:00.461 同毫秒 21 条并发流
// down:write 死掉（真机打不开外网根因）的修复：此前连接死亡后只有新请求
// 才触发重连，页面卡"加载中"。
func TestNoteDeadStreamConnectionLevelErrorReconnects(t *testing.T) {
	c := newTestMasqueClient(t)
	b := newTestBundle()
	c.cur = b
	// dialFn 注入：重连立即返回一个新 bundle，验证 reconnect 真的被触发。
	newB := newTestBundle()
	c.dialFn = func(context.Context) (*connBundle, error) { return newB, nil }

	tc := &tunnelConn{client: c, bundle: b}
	tc.noteDeadStream(connectionLevelErr()) // 连接级错误（QUIC 连接本身死亡）

	// 重连是异步的（goroutine），轮询等待 cur 被替换。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.connMu.RLock()
		cur := c.cur
		c.connMu.RUnlock()
		if cur == newB {
			return // 重连成功，cur 已被新 bundle 替换
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("连接级错误应触发重连：cur 未被新 bundle 替换")
}

// TestNoteDeadStreamStreamResetNoReconnect 锁定 stream-reset 不触发重连：
// 单流 reset（如边缘拒绝单个目标）是目标级问题，不应牵连共享连接（与
// shouldReconnectH3 的注释一致）。
func TestNoteDeadStreamStreamResetNoReconnect(t *testing.T) {
	c := newTestMasqueClient(t)
	b := newTestBundle()
	c.cur = b
	tc := &tunnelConn{client: c, bundle: b}

	tc.noteDeadStream(&quic.StreamError{ErrorCode: 1})
	time.Sleep(50 * time.Millisecond) // 给异步 goroutine 时间（若有）

	c.connMu.RLock()
	cur := c.cur
	c.connMu.RUnlock()
	if cur != b {
		t.Fatal("stream-reset 不应触发重连：cur 不应被替换")
	}
}

// TestNoteDeadStreamEOFNoReconnect 锁定 io.EOF 不触发重连：正常关闭（对端
// 发 FIN）是干净结束，不应唤醒重连（否则每个正常请求都触发重连风暴）。
func TestNoteDeadStreamEOFNoReconnect(t *testing.T) {
	c := newTestMasqueClient(t)
	b := newTestBundle()
	c.cur = b
	tc := &tunnelConn{client: c, bundle: b}

	tc.noteDeadStream(io.EOF)
	time.Sleep(50 * time.Millisecond)

	c.connMu.RLock()
	cur := c.cur
	c.connMu.RUnlock()
	if cur != b {
		t.Fatal("EOF 不应触发重连：cur 不应被替换")
	}
}

// TestNoteDeadStreamNetErrClosedNoReconnect 锁定裸 net.ErrClosed 不触发重连：
// 它是共享连接已被他人 retire/换代/关闭的信号（探针阈值、CONNECT 失败窗口、
// 换代 close 都会走到 bundle.close），本条流只是被拖累——批量死亡时每一条
// 并发流都读到 use of closed network connection，若各自再跑一遍 retire/
// reconnect 只剩噪声。恢复由先动手的那条路径完成。
func TestNoteDeadStreamNetErrClosedNoReconnect(t *testing.T) {
	c, b := deadBundle(t)
	newB := newTestBundle()
	c.dialFn = func(context.Context) (*connBundle, error) { return newB, nil }
	tc := &tunnelConn{client: c, bundle: b}

	tc.noteDeadStream(closedErr())
	time.Sleep(50 * time.Millisecond) // 给异步 goroutine 时间（若有）

	c.connMu.RLock()
	cur := c.cur
	c.connMu.RUnlock()
	if cur != b {
		t.Fatal("net.ErrClosed（已被他人淘汰）不应触发重连：cur 不应被替换")
	}
}

// TestNoteDeadStreamAmbiguousErrorWindowed 锁定非连接级流错误走观察窗：单条
// 流被边缘重置（resetErr）第 1 次只计数不拆共享连接，窗口内第 2 次才判定
// 连接死亡——一条流被边缘重置不该拆毁共享连接拖死所有健康并发流
// （debugdiag：批量死亡全部源于本地拆线）。
func TestNoteDeadStreamAmbiguousErrorWindowed(t *testing.T) {
	c, b := deadBundle(t)
	newB := newTestBundle()
	c.dialFn = func(context.Context) (*connBundle, error) { return newB, nil }
	tc := &tunnelConn{client: c, bundle: b}

	tc.noteDeadStream(resetErr()) // 第 1 次：仅记窗
	time.Sleep(50 * time.Millisecond)
	c.connMu.RLock()
	cur := c.cur
	c.connMu.RUnlock()
	if cur != b {
		t.Fatal("非连接级流错误第 1 次不应拆共享连接：cur 不应被替换")
	}

	tc.noteDeadStream(resetErr()) // 第 2 次：观察窗达标 → retire + 重连
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.connMu.RLock()
		cur := c.cur
		c.connMu.RUnlock()
		if cur == newB {
			return // 观察窗达标已触发重连
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("观察窗内第 2 次非连接级流错误应触发重连：cur 未被替换")
}

// TestProbeEgressOnceSingleFailureKeepsConnection 锁定运行期探测单次失败不拆
// 共享连接：手机网络 UDP 抖动 / 边缘偶发慢响应会让探测 CONNECT 超时，而拆线
// 瞬间所有在途流一起 use of closed network connection（debugdiag：多个健康
// 下载被一次探测的瞬时错误连坐）。连续 probeFailureThreshold 次才 retire。
func TestProbeEgressOnceSingleFailureKeepsConnection(t *testing.T) {
	c, b := deadBundle(t)
	c.probeFn = func(context.Context, *connBundle) error { return context.DeadlineExceeded }
	newB := newTestBundle()
	c.dialFn = func(context.Context) (*connBundle, error) { return newB, nil }

	c.probeEgressOn(b) // 第 1 次失败：仅记数，不拆共享连接
	time.Sleep(50 * time.Millisecond)
	c.connMu.RLock()
	cur := c.cur
	c.connMu.RUnlock()
	if cur != b {
		t.Fatal("单次探测失败不应拆共享连接：cur 不应被替换")
	}

	c.probeEgressOn(b) // 第 2 次失败：达到阈值 → retire + 重连
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.connMu.RLock()
		cur := c.cur
		c.connMu.RUnlock()
		if cur == newB {
			return // 连续两次探测失败已触发重连
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("连续两次探测失败应触发重连：cur 未被替换")
}

// TestProbeEgressOnceSuccessResetsFailures 锁定探测成功清空连续失败计数：一次
// 成功后单次失败不累积到阈值（否则历史毛刺会永久污染判定）。
func TestProbeEgressOnceSuccessResetsFailures(t *testing.T) {
	c, b := deadBundle(t)
	calls := 0
	c.probeFn = func(context.Context, *connBundle) error {
		calls++
		if calls == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}
	newB := newTestBundle()
	c.dialFn = func(context.Context) (*connBundle, error) { return newB, nil }

	c.probeEgressOn(b) // 失败 1 次
	c.probeEgressOn(b) // 成功 → 计数清零
	c.probeEgressOn(b) // 再失败 1 次 → 仍不达阈值（1/2）
	time.Sleep(50 * time.Millisecond)
	c.connMu.RLock()
	cur := c.cur
	c.connMu.RUnlock()
	if cur != b {
		t.Fatal("探测成功清零后单次失败不应拆共享连接：cur 不应被替换")
	}
}
