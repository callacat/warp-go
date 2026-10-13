package tunnel

import (
	"context"
	"strings"
	"testing"
	"time"
)

// probeWatchdogWindow 是本文件里判定「看门狗是否在有界时间内收口」的观察窗。
// 取 3s：远大于测试里调小后的 probeWatchdogTimeout(150ms)，又远小于任何真实
// 退避（最快 100ms 起步的指数退避也只覆盖前几轮），因此「cur 被替换」只可能
// 由看门狗触发，不会被重连节奏偶然凑出来。
const probeWatchdogWindow = 3 * time.Second

// shrinkProbeWatchdog 把看门狗上界调到秒级，返回还原函数。测试串行运行
// （与 captureLog 同前提），临时改包级 var 无竞争。
func shrinkProbeWatchdog(t *testing.T, d time.Duration) {
	t.Helper()
	prev := probeWatchdogTimeout
	probeWatchdogTimeout = d
	t.Cleanup(func() { probeWatchdogTimeout = prev })
}

// TestProbeWatchdogRetiresWedgedProbe 锁定探针硬存活上界（t_f0a0102b）：
// 探测自身卡死不返回时（现场 goroutine dump 停在 resolveDNS——黑洞路径下
// 内部超时同样失效），必须由外部看门狗在有界时间内强制 retire + 重连。
//
// 场景刻意构造「连接已死但判据全部失效」：dead 未置位、quicConn 为 nil
// （currentConnection 的 Context().Done() 检查无从触发）。这正是现场
// 00:45→02:40 零自愈的形态——旧实现里这一轮探针永远不返回，串行的
// egressProbeLoop 从此再也不会跑第二轮。
func TestProbeWatchdogRetiresWedgedProbe(t *testing.T) {
	c, b := deadBundle(t)
	shrinkProbeWatchdog(t, 150*time.Millisecond)

	// 探针永不返回：模拟卡死在 resolveDNS / 流打开 / CONNECT 交换。
	release := make(chan struct{})
	defer close(release)
	c.probeFn = func(context.Context, *connBundle) error {
		<-release // 阻塞直到测试结束，模拟僵死探针
		return nil
	}
	newB := newTestBundle()
	c.dialFn = func(context.Context) (*connBundle, error) { return newB, nil }

	// 前提锁定：这条连接此刻「看起来健康」——dead 未置位，Context 也无从检查。
	if b.dead.Load() {
		t.Fatal("前提不成立：测试连接的 dead 不应已置位")
	}

	c.probeEgressWatchdog(b)

	deadline := time.Now().Add(probeWatchdogWindow)
	for time.Now().Before(deadline) {
		c.connMu.RLock()
		cur := c.cur
		c.connMu.RUnlock()
		if cur == newB {
			if !b.dead.Load() {
				t.Fatal("看门狗收口后旧连接应已置 dead（后续请求不得再复用它）")
			}
			return // retire + 重连在有界时间内完成
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("探针僵死超过硬存活上界后应强制 retire + 重连：cur 未被替换")
}

// TestProbeWatchdogNormalProbeUnaffected 锁定看门狗不误伤：探测在上界内正常
// 返回（无论成功还是判失败）时，看门狗必须原样放行，绝不代替探测结论。
// 少了这条，一次慢但正常的探测就会被拆掉共享连接，把它上面所有在途流
// 一起连坐——正是 probeFailureThreshold 当初调高要避开的回归。
func TestProbeWatchdogNormalProbeUnaffected(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"成功", nil},
		// 失败但未达 probeFailureThreshold：按既有语义只记数，不拆连接。
		{"瞬时失败", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, b := deadBundle(t)
			// 上界远大于探测耗时：走的一定是 done 分支。
			shrinkProbeWatchdog(t, probeWatchdogWindow)
			c.probeFn = func(context.Context, *connBundle) error { return tc.err }
			c.dialFn = func(context.Context) (*connBundle, error) { return newTestBundle(), nil }

			c.probeEgressWatchdog(b)

			c.connMu.RLock()
			cur := c.cur
			c.connMu.RUnlock()
			if cur != b {
				t.Fatal("探测在上界内正常返回时看门狗不应动连接")
			}
			if b.dead.Load() {
				t.Fatal("探测正常返回时不应置 dead")
			}
		})
	}
}

// TestProbeWatchdogTimeoutDocumented 锁定探针相关参数都有默认值且可配：
// 硬存活上界必须严格大于单次探测自身的超时，否则看门狗会在探测还有希望
// 完成时先一步拆连接——那就退化成「每次探测必拆」，比不修更糟。
func TestProbeWatchdogTimeoutDocumented(t *testing.T) {
	if probeWatchdogTimeout <= probeEgressTimeout {
		t.Errorf("探针硬存活上界 %s 必须大于单次探测超时 %s，否则看门狗会抢在探测完成前拆连接",
			probeWatchdogTimeout, probeEgressTimeout)
	}
	if !strings.Contains(errProbeStalled.Error(), "僵死") {
		t.Error("探针僵死的原因串应点明「僵死」，便于日志归因")
	}
}
