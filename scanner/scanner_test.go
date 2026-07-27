package scanner

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// numGoroutines 读取当前进程的 goroutine 数。放在一个名字下避免测试里多处裸
// runtime.NumGoroutine() 的语义模糊。
func numGoroutines() int { return runtime.NumGoroutine() }

// ---- Test seams agreement ---------------------------------------------------
//
// Scan is tested purely at the orchestrator seam: every test injects probeDialer
// with a deterministic fake returning fixed (probeResult, error) tuples, so no
// real QUIC handshake ever runs. Each test is one vertical slice describing a
// single observable behavior of Scan.
//
// A default small config is shared because every slice needs the same scaffolding:
// a tiny CIDR, a single port, real-ish TLS/QUIC configs (Scan only stores them,
// never inspects their internals). The fakes do the observing.

func baseScanConfig(t *testing.T, candidates []string, concurrency int, total, perProbe time.Duration, topN int) Config {
	t.Helper()
	return Config{
		Candidates:      candidates,
		Concurrency:     concurrency,
		TotalTimeout:    total,
		PerProbeTimeout: perProbe,
		TopN:            topN,
		TLSConfig:       &tls.Config{ServerName: "test"},
		QUICConfig:      &quic.Config{},
		PerIPLimit:      DefaultPerIPLimit,
		// CIDRs/Ports left zero; Candidates is the direct injection point so
		// tests don't re-derive the cartesian product they intend to assert on.
	}
}

// injectDialer swaps probeDialer for fake and restores it on cleanup. Tests MUST
// go through this so a panic mid-test can't leak a fake into sibling tests.
func injectDialer(t *testing.T, fake probeFunc) {
	t.Helper()
	orig := probeDialer
	t.Cleanup(func() { probeDialer = orig })
	probeDialer = fake
}

// TestScan_RTTAscendingSuccessFilteredSortsByRTT 验证 Scan 把成功的候选按 RTT
// 升序返回，失败的候选被完全丢弃。这是 Scan 最核心的契约：产出一份"最优在前"
// 的可用端点列表。
//
// 注入三个候选：B 最快、A 次之、C 失败。期望返回恰好 [B, A]，顺序就是 RTT 升序，
// 失败的 C 不出现在结果里。
func TestScan_RTTAscendingSuccessFilteredSortsByRTT(t *testing.T) {
	injectDialer(t, func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		switch addr.String() {
		case "1.1.1.1:443":
			return probeResult{OK: true, RTT: 40 * time.Millisecond}, nil
		case "2.2.2.2:443":
			return probeResult{OK: true, RTT: 10 * time.Millisecond}, nil
		case "3.3.3.3:443":
			return probeResult{}, errors.New("timeout")
		}
		return probeResult{}, fmt.Errorf("unexpected addr %s", addr)
	})

	cfg := baseScanConfig(t, []string{"1.1.1.1:443", "2.2.2.2:443", "3.3.3.3:443"}, 4, 5*time.Second, 1*time.Second, 0)
	got, err := Scan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Scan 返回错误：%v", err)
	}
	if len(got) != 2 {
		t.Fatalf("结果数 = %d，期望 2（失败的候选被丢弃）", len(got))
	}
	wantOrder := []string{"2.2.2.2:443", "1.1.1.1:443"}
	for i, want := range wantOrder {
		if got[i].Addr != want {
			t.Errorf("got[%d].Addr = %q，期望 %q（按 RTT 升序）", i, got[i].Addr, want)
		}
	}
	if got[0].RTT >= got[1].RTT {
		t.Errorf("结果未按 RTT 升序：[0]=%v > [1]=%v", got[0].RTT, got[1].RTT)
	}
}

// TestScan_TopNTruncates 验证 TopN>0 时结果被截断为最优的 N 个，多余的丢弃。
func TestScan_TopNTruncates(t *testing.T) {
	injectDialer(t, func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		// 用端口编码 RTT，保证排序可预测。
		return probeResult{OK: true, RTT: time.Duration(addr.(*net.UDPAddr).Port) * time.Millisecond}, nil
	})
	// 5 个候选，端口即 RTT（毫秒），升序为 [10,20,30,40,50]。
	cfg := baseScanConfig(t,
		[]string{"9.9.9.9:50", "9.9.9.9:40", "9.9.9.9:30", "9.9.9.9:20", "9.9.9.9:10"},
		4, 5*time.Second, 1*time.Second, 3)
	got, err := Scan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Scan 返回错误：%v", err)
	}
	if len(got) != 3 {
		t.Fatalf("TopN=3 截断后结果数 = %d，期望 3", len(got))
	}
	wantPorts := []int{10, 20, 30}
	for i, wp := range wantPorts {
		if got[i].Addr != fmt.Sprintf("9.9.9.9:%d", wp) {
			t.Errorf("got[%d].Addr = %q，期望 9.9.9.9:%d", i, got[i].Addr, wp)
		}
	}
}

// TestScan_AllFailReturnsErrNoSuccess 验证全部候选失败时返回 errNoSuccess，
// 而不是空切片加 nil 错误 —— 上层据此决定是否回退到注册端点。
func TestScan_AllFailReturnsErrNoSuccess(t *testing.T) {
	injectDialer(t, func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		return probeResult{}, errors.New("unreachable")
	})
	cfg := baseScanConfig(t, []string{"1.1.1.1:443", "2.2.2.2:443"}, 2, 2*time.Second, 1*time.Second, 0)
	got, err := Scan(context.Background(), cfg)
	if !errors.Is(err, errNoSuccess) {
		t.Fatalf("期望 errNoSuccess，实际 err=%v（got len=%d）", err, len(got))
	}
	if len(got) != 0 {
		t.Errorf("全失败时结果应为空，实际 %d 条", len(got))
	}
}

// TestScan_ConcurrencySemaphoreBoundsConcurrentDialers 验证 Concurrency 信号量
// 真正约束了同时进行的 probeDialer 调用数 —— 这是 goroutine 泄漏/资源失控的关键
// 不变量。fake 在调用瞬间对 inFlight 计数取 max，退出时减一；扫描结束后 maxInFlight
// 必须 ≤ cfg.Concurrency。
func TestScan_ConcurrencySemaphoreBoundsConcurrentDialers(t *testing.T) {
	var inFlight int32
	var maxInFlight int32
	injectDialer(t, func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			m := atomic.LoadInt32(&maxInFlight)
			if cur <= m || atomic.CompareAndSwapInt32(&maxInFlight, m, cur) {
				break
			}
		}
		// 短暂占用一个 slot，制造并发窗口。
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
		}
		atomic.AddInt32(&inFlight, -1)
		return probeResult{OK: true, RTT: 1 * time.Millisecond}, nil
	})

	// 8 个候选，并发 3，TotalTimeout 足够宽，保证不会因超时提前收。
	cfg := baseScanConfig(t,
		[]string{"1.1.1.1:1", "1.1.1.1:2", "1.1.1.1:3", "1.1.1.1:4", "1.1.1.1:5", "1.1.1.1:6", "1.1.1.1:7", "1.1.1.1:8"},
		3, 5*time.Second, 1*time.Second, 0)
	if _, err := Scan(context.Background(), cfg); err != nil {
		t.Fatalf("Scan 返回错误：%v", err)
	}
	if max := atomic.LoadInt32(&maxInFlight); max > 3 {
		t.Fatalf("最大并发 = %d，超过 Concurrency=3", max)
	}
}

// TestScan_TotalTimeoutCancelsPending 验证 TotalTimeout 到期会通过 ctx 取消
// 传播，把尚未完成的探针从阻塞中解锁 —— 不靠 fake 自觉超时，而靠 ctx.Done()。
//
// fake 在 ctx.Done() 上阻塞，故意不返回；TotalTimeout 极短（30ms）。若 Scan 不
// 把 TotalTimeout 绑进 ctx，fake 会永远阻塞、wg.Wait 永远不返回、测试超时失败。
func TestScan_TotalTimeoutCancelsPending(t *testing.T) {
	// 信号量下发给每个 fake，让测试在所有 fake 都看到 ctx 取消后再放行主流程，
	// 避免还没等到取消断言就结束。
	var seenCancel int32
	injectDialer(t, func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		<-ctx.Done()
		atomic.AddInt32(&seenCancel, 1)
		return probeResult{}, ctx.Err()
	})

	// 4 个候选，并发 2，TotalTimeout 30ms，PerProbeTimeout 必须远大于它，
	// 否则 PerProbeTimeout 会先于 TotalTimeout 触发而无法隔离这个不变量。
	cfg := baseScanConfig(t,
		[]string{"1.1.1.1:1", "1.1.1.1:2", "1.1.1.1:3", "1.1.1.1:4"},
		2, 30*time.Millisecond, 10*time.Second, 0)

	start := time.Now()
	got, err := Scan(context.Background(), cfg)
	elapsed := time.Since(start)
	// TotalTimeout 生效意味着 Scan 在总超时附近返回，而不是跑去等 PerProbeTimeout。
	if elapsed > 2*time.Second {
		t.Fatalf("Scan 耗时 %v，远超 TotalTimeout=30ms —— TotalTimeout 未生效或未取消 pending", elapsed)
	}
	if !errors.Is(err, errNoSuccess) {
		t.Fatalf("全被取消后期望 errNoSuccess，实际 err=%v", err)
	}
	if len(got) != 0 {
		t.Errorf("取消后结果应为空，实际 %d 条", len(got))
	}
	// 不强求所有 fake 都被取消（并发 slot 在 TotalTimeout 后可能尚未全派发），
	// 但只要至少一个看到了取消，就证明 ctx 取消确实传到了 dialer。
	if atomic.LoadInt32(&seenCancel) == 0 {
		t.Fatalf("没有任何 dialer 在 ctx.Done 上看到取消 —— TotalTimeout 未传给 probeDialer")
	}
}

// TestScan_DispatchCtxCancelDoesNotDeadlock 是 Critical deadlock 的回归测试。
//
// 原 bug：runProbes 的分发循环里 `select { case sem<-struct{}{}: case <-ctx.Done(): break }`
// 的 break 只退 select 不退 for；随后无差别执行 `<-sem`，在"ctx.Done 先到、本帧根本没占槽"
// 的路径上会从某个在途 worker 即将释放的槽里偷出一格。后果：worker 的 defer <-sem
// 永远拿不到它应得的释放（槽已被分发线程提前取走），wg.Wait() 永久挂死，Scan 不归。
//
// 本测试固定触发该窗口：
//   - 8 候选、并发 3：前 3 个 worker 各占 1 槽后，第 4 个候选分发时 sem<-struct{}{} 阻塞，
//     与 ctx.Done 竞速。TotalTimeout 40ms 到期必走 ctx.Done 分支。
//   - 所有 worker dialer 在 <-ctx.Done 上阻塞（不自觉退出），与真实 QUIC 握手被取消
//     的行为一致。
//
// 断言：Scan 必须在 2s 内返回（修复后约 40ms；原 bug 永久挂死，被 watchdog 当 caught）。
// 用 watchdog goroutine 而非 testing.T 的 deadline，是为了在挂死时打出明确诊断而非
// 笼统的 "panic: test timed out"。
func TestScan_DispatchCtxCancelDoesNotDeadlock(t *testing.T) {
	injectDialer(t, func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		<-ctx.Done()
		return probeResult{}, ctx.Err()
	})

	// 8 候选、并发 3：buffer=3，第 4 个候选分发即撞上 ctx 取消窗口。
	cfg := baseScanConfig(t,
		[]string{"1.1.1.1:1", "1.1.1.1:2", "1.1.1.1:3", "1.1.1.1:4", "1.1.1.1:5", "1.1.1.1:6", "1.1.1.1:7", "1.1.1.1:8"},
		3, 40*time.Millisecond, 10*time.Second, 0)

	type result struct {
		res []ProbeResult
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := Scan(context.Background(), cfg)
		done <- result{res, err}
	}()

	select {
	case <-done:
		// Scan 返回即未死锁 —— 通过。err 应为 errNoSuccess（全被取消）。
		// 拿到结果即可，不强断 err 形态，免得对"取消传播时序"过度敏感。
	case <-time.After(2 * time.Second):
		t.Fatalf("Scan 挂死：分发循环 ctx 取消路径偷了在途 worker 的槽，wg.Wait 永不返回" +
			"（这正是 Critical deadlock 的复现；runProbes 的 ctx.Done 分支不应执行 <-sem）")
	}
}

// TestScan_GoroutinesReconcileAfterScan 验证 worker pool 不会泄漏 goroutine：
// Scan 返回后进程的 goroutine 数不应比调用前明显增多。这是 §7.3 的回归保障。
func TestScan_GoroutinesReconcileAfterScan(t *testing.T) {
	// 先做一次 Scan 让运行时把 scanner 相关缓存预热，避免首次分配干扰基线。
	injectDialer(t, func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		return probeResult{OK: true}, nil
	})
	cfg := baseScanConfig(t, []string{"1.1.1.1:443"}, 2, 2*time.Second, 1*time.Second, 0)
	if _, err := Scan(context.Background(), cfg); err != nil {
		t.Fatalf("预热 Scan 失败：%v", err)
	}

	// 给运行时一点时间回收预热阶段可能残留的 goroutine。
	time.Sleep(50 * time.Millisecond)
	baseline := numGoroutines()

	// 用会并发 8 路、每路过一会才返回的 fake 制造 goroutine 活跃窗口。
	injectDialer(t, func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		select {
		case <-time.After(30 * time.Millisecond):
		case <-ctx.Done():
		}
		return probeResult{OK: true}, nil
	})
	cfg2 := baseScanConfig(t,
		[]string{"1.1.1.1:1", "1.1.1.1:2", "1.1.1.1:3", "1.1.1.1:4", "1.1.1.1:5", "1.1.1.1:6", "1.1.1.1:7", "1.1.1.1:8"},
		8, 5*time.Second, 1*time.Second, 0)
	if _, err := Scan(context.Background(), cfg2); err != nil {
		t.Fatalf("Scan 返回错误：%v", err)
	}

	// 等待 worker goroutine 完全回收。轮询最多 1s。
	deadline := time.Now().Add(1 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		after = numGoroutines()
		if after <= baseline+1 { // 容忍 +1 的运行时抖动。
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("扫描后 goroutine 数 = %d，基线 = %d，存在泄漏（差值 %d）", after, baseline, after-baseline)
}

// TestScan_RejectsZeroConcurrency 验证 Concurrency<=0 被钳到合理默认而非
// 退化成无界并发（会让信号量变成 nil-channel 死锁）或为零（永远不拨号）。
func TestScan_RejectsZeroConcurrency(t *testing.T) {
	injectDialer(t, func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		return probeResult{OK: true, RTT: 1 * time.Millisecond}, nil
	})
	cfg := baseScanConfig(t, []string{"1.1.1.1:443", "2.2.2.2:443"}, 0, 2*time.Second, 1*time.Second, 0)
	got, err := Scan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Concurrency=0 不应致命，实际 err=%v", err)
	}
	if len(got) != 2 {
		t.Errorf("Concurrency=0 钳默认后应拨完所有候选，结果数 = %d，期望 2", len(got))
	}
}

// TestScan_PreservesAddrInResult 验证返回的 ProbeResult.Addr 与候选地址字符串完全
// 一致 —— main.go 据此拼接 edgeAddrs，任何改写都破坏集成。
func TestScan_PreservesAddrInResult(t *testing.T) {
	injectDialer(t, func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		return probeResult{OK: true, RTT: 1 * time.Millisecond}, nil
	})
	cands := []string{"[2606:4700:d0::1]:443", "162.159.198.2:4500"}
	cfg := baseScanConfig(t, cands, 2, 2*time.Second, 1*time.Second, 0)
	got, err := Scan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Scan 返回错误：%v", err)
	}
	gotSorted := make([]string, len(got))
	for i, r := range got {
		gotSorted[i] = r.Addr
	}
	sort.Strings(gotSorted)
	want := append([]string(nil), cands...)
	sort.Strings(want)
	for i := range want {
		if gotSorted[i] != want[i] {
			t.Errorf("结果 Addr 被改写：got %q 期望 %q", gotSorted[i], want[i])
		}
	}
}

// TestProbeQuicConfig_NonNilAndProbesFriendly 验证 ProbeQuicConfig 返回非 nil 且
// 字段是"探针友好"的：握手超时短（更快判失败）、EnableDatagrams 开（MASQUE 要求）、
// InitialPacketSize 1350（与官方一致）、接收窗口远小于 MasqueClient 的大窗口。
func TestProbeQuicConfig_NonNilAndProbesFriendly(t *testing.T) {
	qc := ProbeQuicConfig()
	if qc == nil {
		t.Fatal("ProbeQuicConfig 返回 nil")
	}
	if !qc.EnableDatagrams {
		t.Error("EnableDatagrams 应为 true（MASQUE 要求）")
	}
	if qc.InitialPacketSize != 1350 {
		t.Errorf("InitialPacketSize = %d，期望 1350", qc.InitialPacketSize)
	}
	if qc.HandshakeIdleTimeout <= 0 || qc.HandshakeIdleTimeout > 5*time.Second {
		t.Errorf("HandshakeIdleTimeout = %v，期望在 (0, 5s] 内（探针应快速判失败）", qc.HandshakeIdleTimeout)
	}
	if qc.MaxIdleTimeout <= 0 {
		t.Errorf("MaxIdleTimeout = %v，应 > 0", qc.MaxIdleTimeout)
	}
	// 探针接收窗口应显著小于 MasqueClient 的 10MB/1MB —— 这是省内存的取舍点。
	if qc.InitialConnectionReceiveWindow >= 10_000_000 {
		t.Errorf("InitialConnectionReceiveWindow = %d，探针应远小于 10MB", qc.InitialConnectionReceiveWindow)
	}
}

// TestScan_FamilyPreProbeSkipsUnreachableFamily 验证族级预探：当某个地址族的代表
// 候选返回 unroutableFamily 错误时，整族候选被跳过（不逐个白耗费时间）。
//
// 这是 §7.2 的契约：IPv4-only 主机选 IPv6 边缘常见，逐端口 ENETUNREACH 会刷爆总超时。
// Scan 应先对每个出现的地址族发一个代表探针，族不可达则整族剔除。
func TestScan_FamilyPreProbeSkipsUnreachableFamily(t *testing.T) {
	// 统计每个候选是否真正进入了 worker 拨号。
	var dialed sync.Map
	injectDialer(t, func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		ua := addr.(*net.UDPAddr)
		isV6 := ua.IP.To4() == nil
		dialed.Store(addr.String(), true)
		if isV6 {
			// IPv6 族：模拟本机不可达，返回 unroutableFamily 错误。
			return probeResult{}, syscall.ENETUNREACH
		}
		// IPv4 族：成功。
		return probeResult{OK: true, RTT: 5 * time.Millisecond}, nil
	})

	cands := []string{
		"162.159.198.2:443",     // v4
		"162.159.198.3:443",     // v4
		"[2606:4700:d0::1]:443", // v6
		"[2606:4700:d0::2]:443", // v6
	}
	cfg := baseScanConfig(t, cands, 4, 5*time.Second, 1*time.Second, 0)
	got, err := Scan(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Scan 返回错误：%v", err)
	}
	// 仅有 IPv4 候选成功。
	if len(got) != 2 {
		t.Fatalf("结果数 = %d，期望 2（仅 IPv4 成功，IPv6 整族跳过）", len(got))
	}
	for _, r := range got {
		ua, _ := net.ResolveUDPAddr("udp", r.Addr)
		if ua == nil || ua.IP.To4() == nil {
			t.Errorf("结果应全是 IPv4，出现 %q", r.Addr)
		}
	}
	// IPv6 候选不应被逐个拨号 —— 只有族预探那一个被拨过。
	v6Dialed := 0
	dialed.Range(func(k, v any) bool {
		if s, ok := k.(string); ok && len(s) > 0 && s[0] == '[' {
			v6Dialed++
		}
		return true
	})
	if v6Dialed > 1 {
		t.Errorf("IPv6 候选被拨号 %d 次，族预探应使整族跳过，最多只拨代表 1 次", v6Dialed)
	}
}
