package main

// 日志环形缓冲：GUI 日志页的数据源。service.go 的 logWriter 把 log.Printf
// 的输出同时写入这里；GetLogs 供前端轮询最近 N 条。

import (
	"log"
	"strings"
	"sync"
	"time"
)

// init 在包加载时（c-shared 库加载、任何 JNI 导出可被调用之前）就把标准库
// log 路由到环形缓冲。Android 上 main() 经 Wails 在 goroutine 中异步执行
// （application_android.go go mainFunc()），WarpVpnService 的 nativeStartVpn
// 可能先于 newService()→initLogging() 触发——若等 newService 才设
// log.SetOutput，早期内核日志只会进 logcat 而不到 GUI 日志页（用户看到
// "无日志"）。包 init 保证时序：日志路由先于一切 JNI 调用就绪。
func init() {
	log.SetOutput(logWriter{})
	log.SetFlags(0)
}

// LogEntry 是日志页展示的单条记录。
type LogEntry struct {
	Time  string `json:"time"`
	Level string `json:"level"` // info | warn | error | debug
	Msg   string `json:"msg"`
}

// ringLogger 是有界环形缓冲，容量固定，新日志覆盖最旧。
type ringLogger struct {
	mu   sync.Mutex
	buf  []LogEntry
	next int
	full bool
}

const ringCap = 500

var ringLog = &ringLogger{buf: make([]LogEntry, ringCap)}

// Append 追加一条日志；自动按前缀推断级别。
func (r *ringLogger) Append(line string) {
	level := "info"
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "error"), strings.Contains(l, "失败"), strings.Contains(l, "无法"):
		level = "error"
	case strings.Contains(l, "warn"), strings.Contains(l, "⚠"), strings.Contains(l, "警告"):
		level = "warn"
	case strings.Contains(l, "debug"):
		level = "debug"
	}

	entry := LogEntry{
		Time:  time.Now().Format("15:04:05"),
		Level: level,
		Msg:   line,
	}

	r.mu.Lock()
	r.buf[r.next] = entry
	r.next = (r.next + 1) % ringCap
	if r.next == 0 {
		r.full = true
	}
	r.mu.Unlock()
}

// Snapshot 返回最近 n 条（按时间正序）。
func (r *ringLogger) Snapshot(n int) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	size := r.next
	if r.full {
		size = ringCap
	}
	if n > size {
		n = size
	}
	out := make([]LogEntry, n)
	for i := 0; i < n; i++ {
		idx := (r.next - n + i) % ringCap
		if idx < 0 {
			idx += ringCap
		}
		out[i] = r.buf[idx]
	}
	return out
}
