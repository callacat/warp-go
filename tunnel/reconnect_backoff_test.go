package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"log"
	"strings"
	"testing"
	"time"
)

// retryTestWindow 覆盖 3 轮快速失败（0ms+100ms+200ms ≈ 300ms）外加约 2.7s
// 静默观察：窗口内长退避（30min）绝不可能到期，之后的任何日志/拨号都是
// 回归信号。
const retryTestWindow = 3 * time.Second

// captureLog 把标准日志重定向到缓冲区，返回读取函数；测试结束自动还原。
// 与 route/rules_test.go 的 log.SetOutput 同模式（包内测试串行，无竞争）。
func captureLog(t *testing.T) func() string {
	t.Helper()
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(log.Writer()) })
	return buf.String
}

// TestReconnectLongBackoffStopsDialing 锁定重连航班的「重试 3 次不通即停」
// （recvu2HKHM5zIj，CT103 事故）：边缘 QUIC 全端口不可达时，旧逻辑每 5s 一轮
// 紧循环重试、每轮 7+ 条日志连刷 journal 4 天。新行为：3 轮全失败后停止密集
// 重试进入 30min 长退避，且整个故障期只打 1 条「进入长退避」边界日志。
func TestReconnectLongBackoffStopsDialing(t *testing.T) {
	c := newTestMasqueClient(t)
	var calls int
	c.dialFn = func(context.Context) (*connBundle, error) {
		calls++
		return nil, context.DeadlineExceeded
	}
	readLog := captureLog(t)

	flight := &reconnectFlight{done: make(chan struct{})}
	c.reconnectMu.Lock()
	c.reconnectFlight = flight
	c.reconnectMu.Unlock()
	go c.runReconnect(flight)

	time.Sleep(retryTestWindow)
	c.lifeStop()

	out := readLog()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if got := strings.Count(out, "进入长退避"); got != 1 {
		t.Fatalf("「进入长退避」边界日志应恰好 1 条，得到 %d（全文：%s）", got, out)
	}
	if len(lines) != 3 {
		t.Fatalf("静默观察窗口内应恰 3 行（2 轮快速失败+1 条边界），得到 %d（全文：%s）", len(lines), out)
	}
	if calls != 3 {
		t.Fatalf("窗口内 dial 应恰 3 次（3 轮失败即停，等 30min 长退避），得到 %d", calls)
	}
}

// TestBootstrapLongBackoffStopsDialing 锁定初始装配循环的对称行为：装配期
// 边缘不可达（用不可解析地址使每轮快速失败）同样 3 轮后切长退避、静默。装配
// 循环在长退避下不会返回（语义上持续持有到拨通/取消），用 goroutine 跑、
// 观察窗后取消 ctx 收尾。
func TestBootstrapLongBackoffStopsDialing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	readLog := captureLog(t)

	done := make(chan error, 1)
	go func() {
		// 不可解析主机名：ResolveUDPAddr 立即失败（不占 2s 拨号超时），每轮极快。
		// tlsConfig 需非 nil：dialAddr 的过程日志要读 ServerName（生产恒非 nil，
		// 由 registration 构建；测试给最小配置）。
		_, err := NewMasqueClientContext(ctx, []string{"invalid.invalid:443"}, &tls.Config{ServerName: "test"}, "test-token")
		done <- err
	}()

	time.Sleep(retryTestWindow)
	cancel() // 中止装配（v0.5.10 语义：外部取消立即退出拨号循环）
	if err := <-done; err == nil {
		t.Fatal("取消后装配应返回错误")
	}

	out := readLog()
	if got := strings.Count(out, "进入长退避"); got != 1 {
		t.Fatalf("「进入长退避」边界日志应恰好 1 条，得到 %d（全文：%s）", got, out)
	}
	if got := strings.Count(out, "QUIC 拨号"); got != 3 {
		t.Fatalf("静默期前逐端口日志应只出现在 3 轮内，QUIC 拨号共 %d 条（全文：%s）", got, out)
	}
}
