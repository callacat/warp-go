package tunnel

import (
	"testing"
)

// TestQuicConfigPacketSizeClamp 锁定 v0.5.31 的包大小钳制：本端报文必须
// 恒小于真机实测的路径 MTU 上限（≈1478，ICMP 1450 负载通过 / 1460 全丢），
// 且禁止 PMTUD 探到 1452 危险区（1452+28=1480 越过 1478，触发 DF 静默丢包）。
func TestQuicConfigPacketSizeClamp(t *testing.T) {
	cfg := newQUICConfig()

	// InitialPacketSize 1200（quic-go 合法下限）：报文恒 ≤ 1200+8+20=1228，
	// 对任何 ≥1232 的路径 MTU 都安全，且关闭 PMTUD 后是本端终生包上限。
	if cfg.InitialPacketSize != 1200 {
		t.Fatalf("InitialPacketSize = %d，期望 1200", cfg.InitialPacketSize)
	}
	// 关闭 PMTUD：杜绝上行包探到 quic-go 通告上限 1452。
	if !cfg.DisablePathMTUDiscovery {
		t.Fatal("DisablePathMTUDiscovery = false，期望 true（禁止探到 1452 危险区）")
	}
	if cfg.InitialPacketSize >= 1452 {
		t.Fatalf("InitialPacketSize %d 已触及 quic-go MaxPacketBufferSize(1452) 危险区", cfg.InitialPacketSize)
	}

	// 包尺寸必须在 quic-go validateConfig 可接受区间 [1200, 1452] 内
	// （构造后经 quic.Transport.Dial 交给 validateConfig，超界会被静默钳制，
	// 导致上面的断言与实际生效值脱节）。这里直接复跑一遍相同的钳制逻辑校验。
	if cfg.InitialPacketSize < 1200 || cfg.InitialPacketSize > 1452 {
		t.Fatalf("InitialPacketSize %d 超出 quic-go 合法区间 [1200,1452]", cfg.InitialPacketSize)
	}
}

// TestQuicConfigWindows 锁定流控窗口：连接 10MB / 单流 1MB，与 warp-svc
// tokio-quiche 对齐，防止回归把流控又压小压死大下载。
func TestQuicConfigWindows(t *testing.T) {
	cfg := newQUICConfig()
	if cfg.MaxConnectionReceiveWindow != 10_000_000 {
		t.Fatalf("MaxConnectionReceiveWindow = %d，期望 10000000", cfg.MaxConnectionReceiveWindow)
	}
	if cfg.MaxStreamReceiveWindow != 1_000_000 {
		t.Fatalf("MaxStreamReceiveWindow = %d，期望 1000000", cfg.MaxStreamReceiveWindow)
	}
	if cfg.MaxIncomingStreams != 100 {
		t.Fatalf("MaxIncomingStreams = %d，期望 100", cfg.MaxIncomingStreams)
	}
}

// TestQuicConfigMaxPacketSizeStaysWithinMeasuredPathMTU 缓存 1478 路径 MTU
// 的算术，防止未来有人把 InitialPacketSize 提到触发黑洞的区间。
func TestQuicConfigMaxPacketSizeStaysWithinMeasuredPathMTU(t *testing.T) {
	cfg := newQUICConfig()
	// 报文总长 = QUIC 载荷 + UDP 头(8) + IPv4 头(20)。实测路径 MTU ≈1478，
	// 安全上限即 1450 字节 QUIC 载荷；1200 留足余量。
	wire := int(cfg.InitialPacketSize) + 8 + 20
	if wire >= 1478 {
		t.Fatalf("报文总长 %d ≥ 实测路径 MTU 1478，将触发 DF 静默丢包", wire)
	}
}