package tunnel

import (
	"testing"
	"time"
)

// TestDialRetryPolicyEscalateRhythm 锁定拨号重试的「快速节奏 → 长退避」切换韵律
// （recvu2HKHM5zIj，2026-09-02 CT103 事故修复）：连续失败前 2 轮按 100ms→200ms
// 指数退避立即重试，第 3 轮起停止密集重试、切入 retryLongBackoff(30min) 低频
// 探测，且仅第一次切入时报 firstBackoff=true（调用方借此打「进入长退避」日志），
// 此后每一轮 wait 恒为 30min、firstBackoff 恒 false（静默）。事故背景：边缘全
// 端口不可达时旧逻辑永不停止、每 5s 一轮 7+ 条日志紧循环刷 journal 4 天——本
// 测试锁死「3 轮即停 + 只播报一次」这两个防刷屏关键点。
func TestDialRetryPolicyEscalateRhythm(t *testing.T) {
	p := &dialRetryPolicy{}
	prev := time.Duration(0) // 上一轮返回的 wait 作为下一轮 prev（与真实调用链一致）

	tests := []struct {
		wantWait         time.Duration
		wantFirstBackoff bool
	}{
		{100 * time.Millisecond, false}, // 第 1 轮失败：快速节奏起步
		{200 * time.Millisecond, false}, // 第 2 轮失败：指数翻倍
		{retryLongBackoff, true},        // 第 3 轮失败：达阈值切入长退避，首次播报
		{retryLongBackoff, false},       // 第 4 轮：退避期静默，不重复播报
		{retryLongBackoff, false},       // 第 5 轮：退避期恒 30min、恒静默
	}
	for i, tt := range tests {
		gotWait, gotFirst := p.afterFailure(prev)
		if gotWait != tt.wantWait || gotFirst != tt.wantFirstBackoff {
			t.Fatalf("第 %d 轮失败后：期望 wait=%v firstBackoff=%v，得到 wait=%v firstBackoff=%v",
				i+1, tt.wantWait, tt.wantFirstBackoff, gotWait, gotFirst)
		}
		prev = gotWait
	}
}

// TestDialRetryPolicyBacking 锁定 backing() 的进入/退出阈值：rounds 未达
// dialRoundFailureLimit(3) 时返回 false（仍处快速重试），达到 3 即返回 true。
// 调用方（拨号循环）据此在退避期静默逐端口日志——阈值一旦漂移，退避期日志就会
// 死灰复燃，重演 CT103 边缘全端口不可达刷爆 journal 的事故。
func TestDialRetryPolicyBacking(t *testing.T) {
	p := &dialRetryPolicy{}
	if p.backing() {
		t.Fatal("0 轮失败后不应进入长退避：期望 false，得到 true")
	}

	p.afterFailure(0)                      // 第 1 轮失败
	p.afterFailure(100 * time.Millisecond) // 第 2 轮失败
	if p.backing() {
		t.Fatal("2 轮失败后（未达阈值 3）不应进入长退避：期望 false，得到 true")
	}

	p.afterFailure(200 * time.Millisecond) // 第 3 轮失败：达阈值
	if !p.backing() {
		t.Fatal("3 轮连续失败后应进入长退避：期望 true，得到 false")
	}
}

// TestDialRetryPolicyReset 锁定 reset() 对重连航班跨故障期恢复的支持：跑满
// 3+ 轮进入长退避（backing()=true、announced=true）后 reset() 必须把
// rounds/announced 归零——下一故障期从 100ms 快速节奏重新开始，且 firstBackoff
// 重新可报（连续 3 轮后再次 firstBackoff=true）。这是「拆线瞬间尽快恢复」的
// 语义：重建成功即视为故障期结束，不应残留上期的退避计数与已播报标记。
func TestDialRetryPolicyReset(t *testing.T) {
	p := &dialRetryPolicy{}
	p.afterFailure(0)
	p.afterFailure(100 * time.Millisecond)
	p.afterFailure(200 * time.Millisecond) // 第 3 轮：达阈值进入长退避并已播报
	if !p.backing() {
		t.Fatal("进入长退避前置检查失败：期望 backing()=true，得到 false")
	}

	p.reset()
	if p.rounds != 0 || p.announced {
		t.Fatalf("reset 应清零计数与播报标记：期望 rounds=0/announced=false，得到 rounds=%d/announced=%v",
			p.rounds, p.announced)
	}
	if p.backing() {
		t.Fatal("reset 后不应处于长退避：期望 false，得到 true")
	}

	// 重置后重新走一遍快速节奏 → 再次切入长退避，验证 firstBackoff 可重新播报。
	if wait, first := p.afterFailure(0); wait != 100*time.Millisecond || first {
		t.Fatalf("reset 后第 1 轮失败应回到快速节奏 100ms 且不播报：期望 wait=100ms first=false，得到 wait=%v first=%v", wait, first)
	}
	if wait, first := p.afterFailure(100 * time.Millisecond); wait != 200*time.Millisecond || first {
		t.Fatalf("reset 后第 2 轮失败应回到 200ms 且不播报：期望 wait=200ms first=false，得到 wait=%v first=%v", wait, first)
	}
	if wait, first := p.afterFailure(200 * time.Millisecond); wait != retryLongBackoff || !first {
		t.Fatalf("reset 后第 3 轮失败应重新切入长退避并播报：期望 wait=30min first=true，得到 wait=%v first=%v", wait, first)
	}
	if _, first := p.afterFailure(retryLongBackoff); first {
		t.Fatal("同一故障期第二次长退避不应重复播报：期望 firstBackoff=false，得到 true")
	}
}

// TestEscalateBackoff 表驱动锁定指数退避 escalateBackoff 的全部边界：0（拆线
// 后首次立即重试）→ reconnectRetryInitial(100ms) 起步；低于 reconnectRetryMax
// (5s) 逐轮翻倍；翻倍恰好等于 5s 封顶；翻倍超过 5s 钳回 5s；已达 5s 后保持 5s
// 不再增长。这是快速重试期的防风暴节奏——封顶若失效，故障期重试间隔会无限膨胀
// 拖慢恢复，或日志刷屏节奏失控（CT103 教训）。
func TestEscalateBackoff(t *testing.T) {
	tests := []struct {
		prev time.Duration // 传入的上一轮 wait
		want time.Duration // 期望返回的 wait
	}{
		{0, 100 * time.Millisecond},                      // 0 → 100ms 起步
		{100 * time.Millisecond, 200 * time.Millisecond}, // 未触顶翻倍
		{2 * time.Second, 4 * time.Second},               // 继续翻倍
		{2500 * time.Millisecond, 5 * time.Second},       // 翻倍恰好 5s 封顶
		{3 * time.Second, 5 * time.Second},               // 翻倍 6s 超限 → 钳到 5s
		{5 * time.Second, 5 * time.Second},               // 已达封顶保持 5s
	}
	for i, tt := range tests {
		if got := escalateBackoff(tt.prev); got != tt.want {
			t.Fatalf("escalateBackoff(%v) 用例 %d：期望 %v，得到 %v", tt.prev, i, tt.want, got)
		}
	}
}
