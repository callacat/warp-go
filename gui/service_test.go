package main

import (
	"testing"
	"time"
)

// TestStartNoDeadlock 回归测试：Service.Start() 桌面路径曾持有 s.mu 调用
// serverInstance()（内部再次加锁，sync.Mutex 不可重入 → 自死锁），导致
// "点击启动卡死 + 其他页全部无法显示"。修复后 serverInstance() 在锁外调用。
// 用 goroutine + 超时验证 Start 不阻塞（若死锁，2s 后测试失败）。
func TestStartNoDeadlock(t *testing.T) {
	svc := newService()
	if svc == nil {
		t.Fatal("newService 返回 nil")
	}

	done := make(chan struct{})
	go func() {
		// 桌面路径（非 android）：Start 走 serverInstance + 异步 goroutine。
		_ = svc.Start()
		close(done)
	}()

	select {
	case <-done:
		// Start 正常返回（serverInstance 成功创建 Server，goroutine 异步启动）。
	case <-time.After(2 * time.Second):
		t.Fatal("Start() 死锁：2s 未返回（s.mu 自死锁）")
	}

	// Start 后 GetStatus 也必须立即可用（死锁时这里同样会卡住）。
	stDone := make(chan struct{})
	go func() {
		_ = svc.GetStatus()
		close(stDone)
	}()
	select {
	case <-stDone:
	case <-time.After(2 * time.Second):
		t.Fatal("GetStatus() 死锁：2s 未返回")
	}

	// 清理：Stop 幂等（未真正启动内核，started=false 时 no-op）。
	_ = svc.Stop()
}
