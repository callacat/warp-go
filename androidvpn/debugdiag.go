//go:build debugdiag

// Package androidvpn 的调试数据收集（仅 -tags debugdiag 编译；release 构建
// 编译 debugdiag_stub.go 的 no-op 版本，零 IO 零内存）。
//
// 目的：解决"打不开外网"多轮修复无法观察的 payload 层盲区——CONNECT
// 建立成功 ≠ 字节流动。本文件把每条 TCP 隧道关闭时的双向字节数/首字节
// 耗时、每个 UDP 直连流（含 QUIC:443 与非拦截 DNS:53 泄漏）、tun0 计数
// 采样落盘到 <沙箱>/debugdiag/，供离线分析。
//
// 数据文件：
//   tunnels.tsv  一条 TCP 隧道一行：time seq host upBytes downBytes firstByteMs lifeMs err
//   udp.tsv      一个 UDP 直连流一行：time host kind bytes err（kind=dns|quic|udp）
//   tun0.tsv     tun0 计数采样（2s 间隔）：time txBytes deltaTx rxBytes deltaRx
package androidvpn

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// debugDiag 是调试收集器单例。dir 为空时所有方法 no-op（未注入目录时行为
// 与 release 完全一致）。所有字段由 mu 保护。
type debugDiag struct {
	mu     sync.Mutex
	dir    string
	tsv    *os.File // tunnels.tsv
	usv    *os.File // udp.tsv
	nsv    *os.File // tun0.tsv
	start  time.Time
	seq    int
	stopCh chan struct{}
	stop   bool
	lastTx uint64
	lastRx uint64
}

var dbg debugDiag

// DebugSetDir 设置调试输出目录（沙箱根，数据落在 <root>/debugdiag/）。
// 空串或调用失败 = 禁用（与 release 行为一致）。由 Android 桥在
// startVpnKernel 时调用。重复调用会先停旧采样再开新会话。
func DebugSetDir(root string) {
	dbg.mu.Lock()
	stopLocked()
	dbg.mu.Unlock()

	if strings.TrimSpace(root) == "" {
		return
	}
	dir := filepath.Join(root, "debugdiag")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	dbg.mu.Lock()
	dbg.dir = dir
	dbg.start = time.Now()
	dbg.tsv = openAppend(dir, "tunnels.tsv",
		"time seq host upBytes downBytes firstByteMs lifeMs err\n")
	dbg.usv = openAppend(dir, "udp.tsv",
		"time host kind bytes err\n")
	dbg.nsv = openAppend(dir, "tun0.tsv",
		"time txBytes deltaTx rxBytes deltaRx\n")
	if tx, rx, ok := readTun0(); ok {
		dbg.lastTx, dbg.lastRx = tx, rx
		writeLine(dbg.nsv, "%s %d 0 %d 0\n", time.Now().Format("15:04:05.000"), tx, rx)
	}
	dbg.stop = false
	dbg.stopCh = make(chan struct{})
	dbg.mu.Unlock()
	go sampleLoop()
}

// DebugStop 结束收集（写会话时长尾行、关文件、停采样）。Android 桥在
// VPN 停止时调用；之后可再次 DebugSetDir 重启（新会话新文件）。
func DebugStop() {
	dbg.mu.Lock()
	stopLocked()
	dbg.mu.Unlock()
}

// stopLocked 停采样 goroutine 并关闭全部文件。调用方必须持有 dbg.mu。
func stopLocked() {
	if dbg.stopCh != nil && !dbg.stop {
		close(dbg.stopCh)
		dbg.stop = true
	}
	if dbg.tsv != nil {
		writeLine(dbg.tsv, "session_ended_after=%dms\n",
			time.Since(dbg.start).Milliseconds())
	}
	closeFile(&dbg.tsv)
	closeFile(&dbg.usv)
	closeFile(&dbg.nsv)
	dbg.lastTx, dbg.lastRx = 0, 0
}

// sampleLoop 每 2s 采样一次 tun0 计数（增量），直到 stopCh 关闭。
func sampleLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			logTun0()
		case <-dbg.stopCh:
			return
		}
	}
}

// logTun0 采样一次 tun0 计数（相对上次的增量）写入文件。tun0 不存在
// （未建 TUN / 宿主无 /proc/net/dev）时静默跳过。
func logTun0() {
	tx, rx, ok := readTun0()
	if !ok {
		return
	}
	dbg.mu.Lock()
	defer dbg.mu.Unlock()
	if dbg.nsv == nil {
		return
	}
	dTx, dRx := tx-dbg.lastTx, rx-dbg.lastRx
	dbg.lastTx, dbg.lastRx = tx, rx
	writeLine(dbg.nsv, "%s %d %d %d %d\n",
		time.Now().Format("15:04:05.000"), tx, dTx, rx, dRx)
}

// logTunnelClosed 记录一条已关闭的 TCP 隧道。host 是目标（域名或 IP），
// upBytes/downBytes 是 io.Copy 实际字节数，firstByteMs 是下行首字节相对
// 会话开始的毫秒数（-1 = 从未收到任何字节），lifeMs 是连接存续毫秒。
// err 非空表示复制提前结束。
func logTunnelClosed(host string, upBytes, downBytes int64, firstByteMs int, lifeMs int64, err error) {
	dbg.mu.Lock()
	defer dbg.mu.Unlock()
	if dbg.tsv == nil {
		return
	}
	dbg.seq++
	errStr := "ok"
	if err != nil {
		errStr = err.Error()
	}
	writeLine(dbg.tsv, "%s %d %s %d %d %d %d %s\n",
		time.Now().Format("15:04:05.000"), dbg.seq, host,
		upBytes, downBytes, firstByteMs, lifeMs,
		strings.ReplaceAll(errStr, "\t", " "))
}

// logUDPClosed 记录一个已关闭的 UDP 直连流。kind 由端口判定：53 → dns
// （非拦截 DNS 泄漏），443 → quic（浏览器 HTTP/3 直连），其余 → udp。
func logUDPClosed(host, kind string, bytes int64, err error) {
	dbg.mu.Lock()
	defer dbg.mu.Unlock()
	if dbg.usv == nil {
		return
	}
	errStr := "ok"
	if err != nil {
		errStr = err.Error()
	}
	writeLine(dbg.usv, "%s %s %s %d %s\n",
		time.Now().Format("15:04:05.000"), host, kind, bytes,
		strings.ReplaceAll(errStr, "\n", " "))
}

// openAppend 以追加模式打开（或创建）数据文件，可选写表头。失败返回 nil。
func openAppend(dir, name, header string) *os.File {
	f, err := os.OpenFile(filepath.Join(dir, name),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	if header != "" {
		_, _ = f.WriteString(header)
	}
	return f
}

func closeFile(f **os.File) {
	if *f != nil {
		_ = (*f).Close()
		*f = nil
	}
}

// readTun0 读 /proc/net/dev 的 tun0 行，返回（txBytes, rxBytes, ok）。
func readTun0() (tx, rx uint64, ok bool) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "tun0:") {
			continue
		}
		fields := strings.Fields(l)
		if len(fields) < 10 {
			continue
		}
		rx, err1 := strconv.ParseUint(fields[1], 10, 64)
		rtx, err2 := strconv.ParseUint(fields[9], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		return rtx, rx, true
	}
	return 0, 0, false
}

func writeLine(f *os.File, format string, args ...any) {
	if f == nil {
		return
	}
	_, _ = fmt.Fprintf(f, format, args...)
}