package scanner

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// probeResult 是对单个 WARP 边缘候选探针一次的结论。OK 为真表示真实 QUIC 握手
// 在 perProbeTimeout 内完成，RTT 即从拨号开始到握手成功所测得的往返时间。
type probeResult struct {
	// Addr 是被探边缘地址的字符串形式（host:port），无论成败都记录，便于上层
	// 把多候选的失败原因聚合成一条可读的报告。
	Addr string
	// RTT 仅在 OK 为真时有意义：从 dial 启动到 QUIC 握手完成所经过的时间。
	// 失败时为 0。
	RTT time.Duration
	// OK 为真当且仅当 QUIC 握手成功并已干净释放连接。
	OK bool
	// ErrReason 在 OK 为假时给出人类可读的失败原因（无换行）。OK 为真时为空串。
	ErrReason string
}

// probeFunc 是一次真实 QUIC 拨号的执行体。把它抽成函数变量是为了可测性
// （依赖注入 seam）：probeEdge 的控制流——超时子 ctx、结果组装、父 ctx 取消
// 传导——可被单测直接验证，而"真拨一次 QUIC"无法在单元测试中触达。测试把
// probeDialer 替换为返回固定 (probeResult, error) 的假实现即可覆盖成功 / 失败 /
// ctx 取消三条路径，无需真连任何边缘。
//
// dialer 收到 addr（probeEdge 解析后的 *net.UDPAddr）完成 qtr.Dial、测 RTT、并
// 在内部完成握手后干净释放连接；资源（UDP socket、Transport、QUIC 连接）由
// dialer 自管自关，使这个 seam 在真实与测试两种路径下都自洽。
type probeFunc func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error)

// probeDialer 是 probeEdge 实际调用的拨号入口。默认指向真实实现；
// 测试可临时替换它为假实现（替换后须在 t.Cleanup 中恢复以避免污染同包其它测试）。
var probeDialer probeFunc = defaultProbeDialer

// probeConnectionIDLength 与 tunnel/masque.go 中的 connectionIDLength 同源：
// warp-svc 的 SimpleConnectionIdGenerator 发出 20 字节源连接 ID。若用 quic-go
// 默认的 4 字节，WARP 边缘会间歇性地以 PROTOCOL_VIOLATION 关闭连接（项目逆向文档
// §6）。探针必须复用同一长度，否则会被边缘拒绝，从而得到误导性的"不可达"结论。
// scanner 包不依赖 tunnel，故独立定义一份。
const probeConnectionIDLength = 20

// unroutableFamily 判断 err 是否表示"本机对整个地址族都不可达"，而非某个端口被
// 阻塞。在 IPv4-only 主机上选中了 IPv6 边缘是常见场景：这类错误对同一主机的不同
// 端口都会重复出现，继续逐端口尝试只会逐个白耗费 perAddrDialTimeout，因此上层应
// 据此提前终止该家族的后续候选。
//
// 这里复制 tunnel/masque.go:255 的同义判断（scanner 不 import tunnel），仅识别
// 三个最常见的不可达错误码。
func unroutableFamily(err error) bool {
	return errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EAFNOSUPPORT) ||
		errors.Is(err, syscall.EHOSTUNREACH)
}

// probeEdge 对单个 WARP 边缘候选做一次探针：解析地址、起 perProbeTimeout 子 ctx、
// 把真正 QUIC 拨号委托给 probeDialer，依据其返回组装 probeResult。它不直接触网——
// 所有真实 QUIC 行为都在 probeDialer 里，便于测试注入假实现。
//
// 设计要点：
//   - 资源由 dialer 自管：defaultProbeDialer 内部完成 socket 绑定、Transport 构造、
//     Dial、握手后 CloseWithError、defer 关闭 socket+Transport，使真实与测试路径
//     在 probeEdge 视角下完全一致——probeEdge 不持有任何需手工关闭的句柄。
//   - 子 ctx：context.WithTimeout(ctx, perProbeTimeout) 同时受 perProbeTimeout 上限
//     与父 ctx 取消约束，上层可批量取消整个扫描，单探针的取消会立即传导给 dialer。
//   - 结果组装：成功路径透传 dialer 给出的 RTT；失败路径区分父 ctx 已取消、子 ctx
//     超时/取消、以及 dialer 自身错误，给出可读的 ErrReason。
func probeEdge(ctx context.Context, edgeAddr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config, perProbeTimeout time.Duration) probeResult {
	addrStr := edgeAddr.String()

	// ResolveUDPAddr 顺手把 host:port 规整为 *net.UDPAddr，传给 dialer，并保证即使
	// 调用方传入 DNS 主机名也能在拨号前落地。解析失败直接判失败：探针不负责 DNS 预筛。
	udpAddr, err := net.ResolveUDPAddr("udp", addrStr)
	if err != nil {
		return probeResult{Addr: addrStr, ErrReason: fmt.Sprintf("解析边缘地址失败：%v", err)}
	}

	// perProbeTimeout 子 ctx：父 ctx 取消会传播进来。dialer 若尊重 ctx（真实
	// 实现的 qtr.Dial 会），取消时立即返回 ctx.Err。
	dialCtx, cancel := context.WithTimeout(ctx, perProbeTimeout)
	defer cancel()

	res, err := probeDialer(dialCtx, udpAddr, tlsCfg, quicCfg)
	res.Addr = addrStr
	switch {
	case err == nil && res.OK:
		return res
	case err == nil:
		// dialer 异常防御：既没成功也没报错，按失败处理而非揣测一个不存在的 RTT。
		return probeResult{Addr: addrStr, ErrReason: "探测无结果且无错误"}
	default:
		// 失败：先区分父 ctx 取消（上层主动中止），再区分子 ctx 超时/取消，最后才是
		// dialer 自身错误——三者文案不同，便于上层据此决定是否继续后续候选。
		switch {
		case ctx.Err() != nil:
			return probeResult{Addr: addrStr, ErrReason: fmt.Sprintf("探测被取消：%v（%v）", ctx.Err(), err)}
		case dialCtx.Err() != nil:
			return probeResult{Addr: addrStr, ErrReason: fmt.Sprintf("探测超时或取消：%v（%v）", dialCtx.Err(), err)}
		default:
			return probeResult{Addr: addrStr, ErrReason: err.Error()}
		}
	}
}

// defaultProbeDialer 是真实实现：对单个候选做一次真实 QUIC 握手、测 RTT，握手一
// 完成（不等 HTTP/3 SETTINGS）立即 CloseWithError 干净释放连接，再关闭
// Transport 与 UDP socket。
//
// 为何不等 H3 SETTINGS：WARP 边缘先完成 QUIC 握手，再由 H3 层发 SETTINGS；而"这个
// 候选是否可达、RTT 是多少"在 QUIC 握手完成时就已经确定——握手本身需要对端响应
// Initial+握手往返，等 SETTINGS 只会额外引入一次 H3 往返，既无收益也拖慢整体扫
// 描。探针的目标是测路径可达性与基本往返延迟而非建立长连接，故握手一成立即关闭,
// 符合项目方案 §3.1 的论证。
//
// 为何每次 Clone tlsConfig：与 tunnel/masque.go:288 同理——quic-go 拨号过程中会按需
// 修改传入的 tls.Config，多候选并发探针若共享同一非克隆配置会互相覆盖；Clone 确保
// 每个探测的副本独立。
//
// 为何 SCID 必为 20 字节：见 probeConnectionIDLength 注释。
func defaultProbeDialer(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
	udpAddr, ok := addr.(*net.UDPAddr)
	if !ok {
		return probeResult{}, fmt.Errorf("探测地址不是 UDP 地址：%T", addr)
	}
	addrStr := udpAddr.String()

	// 按地址族绑本地 socket，与 tunnel/masque.go:270 一致：IPv4 候选绑 IPv4zero、
	// IPv6 候选绑 IPv6zero，确保发包的源地址族正确，避免双栈环境下的选源歧义。
	listenAddr := &net.UDPAddr{IP: net.IPv4zero}
	if udpAddr.IP.To4() == nil {
		listenAddr = &net.UDPAddr{IP: net.IPv6zero}
	}
	udpConn, err := net.ListenUDP("udp", listenAddr)
	if err != nil {
		return probeResult{Addr: addrStr}, fmt.Errorf("监听 UDP 失败：%w", err)
	}
	defer udpConn.Close()

	// 显式 Transport 才能把源连接 ID 设为 20 字节（见 probeConnectionIDLength）。
	qtr := &quic.Transport{Conn: udpConn, ConnectionIDLength: probeConnectionIDLength}
	defer qtr.Close()

	log.Printf("探测 QUIC 拨号 %s ...", addrStr)
	start := time.Now()
	quicConn, err := qtr.Dial(ctx, udpAddr, tlsCfg.Clone(), quicCfg)
	if err != nil {
		return probeResult{Addr: addrStr}, fmt.Errorf("QUIC 拨号 %s 失败：%w", addrStr, err)
	}
	rtt := time.Since(start)

	// 握手一完成立即干净释放：用 H3 NoError 应用错误码主动关闭，不等 SETTINGS。
	// quic.ApplicationErrorCode(http3.ErrCodeNoError) 与 tunnel/masque.go 中
	// releaseStream 的 quic.StreamErrorCode(http3.ErrCodeNoError) 同源，均表示
	// "主动干净关闭"，对端据此回收资源而无需任何错误恢复逻辑。
	_ = quicConn.CloseWithError(quic.ApplicationErrorCode(http3.ErrCodeNoError), "probe done")

	log.Printf("✓ 探测 %s：RTT %s", addrStr, rtt)
	return probeResult{Addr: addrStr, RTT: rtt, OK: true}, nil
}
