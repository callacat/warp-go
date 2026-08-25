package tunnel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// streamConn wraps http3.RequestStream to implement net.Conn for TLS
type streamConn struct {
	*http3.RequestStream
	localAddr  net.Addr
	remoteAddr net.Addr
}

func (s *streamConn) LocalAddr() net.Addr  { return s.localAddr }
func (s *streamConn) RemoteAddr() net.Addr { return s.remoteAddr }

// releaseStream fully retires an H3 stream. Closing only the send side leaves the
// receive side open, and the edge keeps its half of a CONNECT tunnel open until
// the target closes — so a stream abandoned that way is never returned to the
// edge's finite concurrent-stream grant.
func releaseStream(s *http3.RequestStream) {
	if s == nil {
		return
	}
	s.CancelRead(quic.StreamErrorCode(http3.ErrCodeNoError))
	s.CancelWrite(quic.StreamErrorCode(http3.ErrCodeNoError))
	_ = s.Close()
}

// tunnelConn adapts an established H3 CONNECT stream to net.Conn for callers
// outside the SOCKS5 path（mixed 代理的 TunnelDial）。Read/Write/LocalAddr/
// RemoteAddr 由内嵌的 streamConn（→ http3.RequestStream）提供。
type tunnelConn struct {
	*streamConn
	releaseOnce sync.Once
	// closing 标记本端已主动 Close。relay 是双向 io.Copy：一侧结束后 Close
	// （CancelRead/CancelWrite）会解除另一方向仍阻塞在 Read 上的 Copy，那条
	// Read 返回的错误是本端收尾的尾流而非连接故障——closing 置位后一律不再
	// 进 noteDeadStream（2026-08-26 qlog 实验：缺这层防护时每个请求结束都会
	// 贡献一次可疑流失败，观察窗 2 次即满，健康共享连接被按请求节奏拆除）。
	closing atomic.Bool
	client  *MasqueClient // 触发重连用（连接死亡时唤醒恢复，不等新请求）
	bundle  *connBundle   // retire 用（幂等：current!=bundle 时 no-op）
}

// Close 完整释放 H3 流。只关发送侧会让读方向永久阻塞（边缘保持隧道另一侧
// 直到目标关闭），同时泄漏边缘的并发流配额——与 releaseStream 同一套约束。
func (t *tunnelConn) Close() error {
	t.releaseOnce.Do(func() {
		t.closing.Store(true) // 先置位再取消：解除阻塞的 Read 前尾流抑制必须就绪
		reqStream := t.streamConn.RequestStream
		reqStream.CancelRead(quic.StreamErrorCode(http3.ErrCodeNoError))
		reqStream.CancelWrite(quic.StreamErrorCode(http3.ErrCodeNoError))
		_ = reqStream.Close()
	})
	return nil
}

// Read 委托 streamConn，但在连接级错误（QUIC 连接死亡，非 EOF / 单流 reset）
// 时异步唤醒重连：正在跑的流在连接被掐后立即触发恢复，而不是干等下一个
// 新请求（Android TUN 复用流，连接死亡到新请求之间页面卡"加载中"——
// 15:22:00.461 同毫秒 21 条并发流一起 down:write 的真机证据）。
func (t *tunnelConn) Read(b []byte) (int, error) {
	n, err := t.streamConn.Read(b)
	if err != nil {
		t.noteDeadStream(err)
	}
	return n, err
}

// Write 委托 streamConn，连接级错误同样唤醒重连（见 Read 注释）。
func (t *tunnelConn) Write(b []byte) (int, error) {
	n, err := t.streamConn.Write(b)
	if err != nil {
		t.noteDeadStream(err)
	}
	return n, err
}

// noteDeadStream 在连接级错误时触发重连：复用 shouldReconnectH3 的判定
// （非 timeout、非 stream-reset = 连接级状态坏），异步 retire + reconnect。
// io.EOF（对端正常 FIN 关闭）与单流 reset 一样不触发——否则每个正常请求
// 关闭都会唤醒重连风暴。幂等安全：reconnect 的 singleflight 保证并发调用
// 合并为一次；current != bundle 时 retire/reconnect 均为 no-op（另一
// goroutine 已替换）。
func (t *tunnelConn) noteDeadStream(err error) {
	if t == nil || t.client == nil || t.bundle == nil {
		return
	}
	if t.closing.Load() {
		return // 本端已 Close：I/O 错误是收尾尾流（另一方向被解除阻塞的 Copy），非连接故障
	}
	if err == io.EOF {
		return // 正常关闭（对端 FIN），非连接死亡
	}
	if !shouldReconnectH3(err, nil) {
		return
	}
	switch {
	case isConnectionLevelError(err):
		// QUIC 连接本身已死（TransportError/IdleTimeout/...）：立即置 dead
		// 并重连（v0.5.27 快速恢复，不等新请求）。
	case errors.Is(err, net.ErrClosed):
		// 共享连接已被他人 retire/换代/关闭：本条流只是被拖累。批量死亡时
		// 每一条并发流都读到 use of closed network connection，若各自再触发
		// 一轮 retire/reconnect 只剩噪声——retire 单飞且幂等，但没必要也不该
		// 由垂死流来唤醒恢复（恢复已由先动手的那条路径完成）。
		return
	case isLocalStreamTeardown(err):
		// 本端主动取消单条流的尾流：http3 的 RequestStream.Read 路径经
		// body.Read 冒泡时，maybeReplaceError 已把 *quic.StreamError 替换为
		// *http3.Error（无 Unwrap），上面的 shouldReconnectH3 单流豁免
		// （errors.As(*quic.StreamError)）因此失效。不拦住的话，每条流正常
		// 收尾都被误计为可疑失败，观察窗两次即满 → 健康共享连接按请求节奏
		// 被反复拆除，其上全部活跃流（Telegram 长轮询等）周期性中断。
		return
	default:
		// 其余（对端 reset、未知 net 错误等）可能是单目标/单流问题：走观察
		// 窗，窗口内累计 streamFailureTargets 次才判定连接死亡——一条流被
		// 边缘重置不该拆毁共享连接、拖死所有健康并发流（debugdiag：批量
		// 死亡全部源于本地拆线）。
		if !t.bundle.noteStreamFailure() {
			return
		}
	}
	// 先置 dead：即使 QUIC 连接在黑洞下 Context() 未 Done，后续并发请求
	// 也会经 currentConnection 立即加入重连航班，不再在死连接上重试
	// （v0.5.26 只 retire 本 bundle，重连期间新请求仍会叠在死连接上超时）。
	log.Printf("HTTP/3 流观测到连接级错误，淘汰当前连接并重连：%T: %v", err, err)
	t.bundle.dead.Store(true)
	go func() {
		t.client.retireConnection(t.bundle)
		ctx, cancel := context.WithTimeout(context.Background(), reconnectRetryMax*4)
		defer cancel()
		_ = t.client.reconnect(ctx, t.bundle)
	}()
}

// 隧道是长命字节流：残留 deadline 会在传输中途掐断它（见 connectThroughEdge
// 在成功交换后必须清除 deadline 的注释）。上层不应依赖 deadline 终止 I/O——
// 统一走 Close（CancelRead/CancelWrite 立即解除阻塞）或 ctx 取消。故按 no-op
// 处理，避免调用方（如 http.Transport）顺手设置的超时误杀隧道。
func (t *tunnelConn) SetDeadline(time.Time) error      { return nil }
func (t *tunnelConn) SetReadDeadline(time.Time) error  { return nil }
func (t *tunnelConn) SetWriteDeadline(time.Time) error { return nil }

// DialTunnel 建立一条到 targetAddr 的 WARP 隧道字节流并返回 net.Conn。
//
// 与 HandleSOCKS5 的 CONNECT 分支共享 resolveTarget + establishCONNECT，
// 但跳过 SOCKS5 握手：调用方（mixed 代理等）已自行完成协议协商，只接管
// 隧道字节流。目标一律经隧道内 DoH 解析（避免 DNS 以真实源地址泄漏），
// direct 分流由调用方决定，本方法不参与。
func (c *MasqueClient) DialTunnel(ctx context.Context, targetAddr string) (net.Conn, error) {
	connectTarget, hostHeader, err := c.resolveTarget(ctx, targetAddr)
	if err != nil {
		return nil, err
	}

	req := &http.Request{
		Method: "CONNECT",
		Host:   connectTarget,
		URL:    &url.URL{Scheme: "https", Host: connectTarget},
		Header: make(http.Header),
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if hostHeader != "" {
		// 目标经域名解析时，Host 头保留原始域名:端口，让边缘能做虚拟主机路由。
		_, hostPort, _ := net.SplitHostPort(connectTarget)
		req.Header.Set("Host", hostHeader+":"+hostPort)
	}

	reqStream, bundle, resp, err := c.establishCONNECT(ctx, req, connectExchangeTimeout)
	if err != nil {
		return nil, fmt.Errorf("H3 CONNECT %s 失败：%w", connectTarget, err)
	}
	if resp.StatusCode != 200 {
		log.Printf("H3 CONNECT %s -> %d", connectTarget, resp.StatusCode)
		releaseStream(reqStream)
		return nil, fmt.Errorf("H3 CONNECT %s 返回 %d", connectTarget, resp.StatusCode)
	}
	log.Printf("隧道已建立：%s（colo=%s）", targetAddr, resp.Header.Get("Cf-Warp-Colo"))

	// resolveTarget 保证 connectTarget 的主机部分是 IP 字面量；地址仅供
	// LocalAddr/RemoteAddr 使用，不参与 I/O。
	host, portStr, _ := net.SplitHostPort(connectTarget)
	port, _ := strconv.Atoi(portStr)
	return &tunnelConn{
		streamConn: &streamConn{
			RequestStream: reqStream,
			localAddr:     &net.TCPAddr{IP: net.IPv4zero},
			remoteAddr:    &net.TCPAddr{IP: net.ParseIP(host), Port: port},
		},
		client: c,
		bundle: bundle,
	}, nil
}

// establishCONNECT performs a CONNECT exchange and retries once on a fresh H3
// connection when the error indicates that the shared connection, rather than
// the requested target, became unusable. The returned bundle owns stream.
func (c *MasqueClient) establishCONNECT(ctx context.Context, req *http.Request, timeout time.Duration) (*http3.RequestStream, *connBundle, *http.Response, error) {
	var firstErr error
	for attempt := 0; attempt < 2; attempt++ {
		stream, bundle, err := c.openRequestStream(ctx)
		if err != nil {
			if firstErr != nil {
				return nil, nil, nil, fmt.Errorf("首次 CONNECT 失败：%v；重连后打开流失败：%w", firstErr, err)
			}
			return nil, nil, nil, err
		}

		packetsBefore := bundle.receivedPackets()
		resp, err := connectThroughEdge(stream, req, connectDeadline(ctx, timeout))
		if err == nil {
			bundle.noteCONNECTSuccess()
			return stream, bundle, resp, nil
		}
		releaseStream(stream)
		if firstErr == nil {
			firstErr = err
		}
		// dead 置位（noteDeadStream / 运行期探测已观测到连接级故障）→ 不再
		// 在死连接上重试 CONNECT（10s×2），直接 retire + 加入重连航班。
		// 否则隧道被掐后每个并发请求各白等一次超时才恢复，浏览器在等待
		// 窗口内全看到 connection reset（debugdiag：dn=0 并发 RST 风暴）。
		if bundle.dead.Load() {
			_ = c.retireConnection(bundle)
			if reconnectErr := c.reconnect(ctx, bundle); reconnectErr != nil {
				return nil, nil, nil, fmt.Errorf("%v；恢复 HTTP/3 连接失败：%w", err, reconnectErr)
			}
			continue
		}
		if !bundle.connectFailureRequiresReconnect(err, ctx.Err(), req.Host, packetsBefore) {
			return nil, nil, nil, err
		}

		// The threshold (connectFailureTargets failures in the window) has been
		// crossed, so the shared session is likely blackholed. retireConnection
		// removes it from service and aborts it — new requests then join the
		// reconnect flight instead of burning another CONNECT timeout on the
		// dead session. Retiring only happens here, after the failure threshold,
		// so a single unreachable target never kills the connection under
		// healthy flows (v0.5.20 collateral-teardown regression).
		retired := c.retireConnection(bundle)
		if attempt != 0 {
			// The retry budget is exhausted, but leave no connection that this
			// attempt proved unhealthy in service. The next request starts a fresh
			// singleflight reconnect instead of repeating the timeout on it.
			if retired {
				log.Printf("重试的 HTTP/3 CONNECT 仍失败（%v），淘汰连接", err)
			}
			return nil, nil, nil, err
		}
		if retired {
			log.Printf("HTTP/3 CONNECT 交换失败（%v），淘汰当前连接并重连 ...", err)
		}
		if reconnectErr := c.reconnect(ctx, bundle); reconnectErr != nil {
			return nil, nil, nil, fmt.Errorf("%v；恢复 HTTP/3 连接失败：%w", err, reconnectErr)
		}
	}
	return nil, nil, nil, firstErr
}

// isConnectionLevelError 判定错误是否代表共享 QUIC 连接本身已死，而非单个
// 目标/单条流的问题。可辨别的连接级错误（quic TransportError/IdleTimeout/
// ApplicationError/StatelessReset）直接判定整连接死亡。native QUIC 层把这些
// 错误都 Unwrap 成 net.ErrClosed 的一部分，所以必须先按具体类型匹配——否则
// 会把真正的连接级死亡误判成"已被他人淘汰"而跳过重连。剩余裸 net.ErrClosed
// 才是"连接已被他人 retire/换代/关闭"的信号，不需要（也不应该）重复触发
// retire/reconnect。
func isConnectionLevelError(err error) bool {
	if err == nil {
		return false
	}
	var te *quic.TransportError
	if errors.As(err, &te) {
		return true
	}
	var app *quic.ApplicationError
	if errors.As(err, &app) {
		return true
	}
	var idle *quic.IdleTimeoutError
	if errors.As(err, &idle) {
		return true
	}
	var sr *quic.StatelessResetError
	if errors.As(err, &sr) {
		return true
	}
	return false
}

// isLocalStreamTeardown 判定错误是否为本端主动取消单条流的尾流。quic-go 的
// http3 body.Read 在返回前用 maybeReplaceError 把 *quic.StreamError 替换为
// *http3.Error（后者无 Unwrap），shouldReconnectH3 的单流 reset 豁免
// （errors.As(*quic.StreamError)）对这条路径失效。Remote=false 且
// H3_NO_ERROR 是本地 CancelRead/CancelWrite(ErrCodeNoError) 的唯一特征；
// 对端拒绝目标产生的是 Remote=true，不匹配、仍走观察窗。
func isLocalStreamTeardown(err error) bool {
	var h3e *http3.Error
	return errors.As(err, &h3e) && !h3e.Remote && h3e.ErrorCode == http3.ErrCodeNoError
}

// shouldReconnectH3 distinguishes a dead shared transport from a target-level
// stream rejection. CONNECT response deadlines are the important case: QUIC
// can remain locally "alive" during a path blackhole and still allow new stream
// objects, while no request bytes or response frames reach the edge.
func shouldReconnectH3(err, callerErr error) bool {
	if err == nil || callerErr != nil {
		return false
	}
	if isTimeout(err) {
		return true
	}

	// A stream reset is scoped to one CONNECT (for example, the edge rejected a
	// target). Other H3/QUIC I/O failures imply connection-level state and are
	// safe to recover by generation: if another goroutine already reconnected,
	// reconnect observes bundle != current and becomes a no-op.
	var streamErr *quic.StreamError
	return !errors.As(err, &streamErr)
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// connectThroughEdge runs the H3 CONNECT exchange on stream and returns the edge's
// response.
//
// Neither SendRequestHeader nor ReadResponse takes a context or has any timeout of
// its own, so a stream the edge accepts but never answers would park the caller
// forever with nothing logged. The exchange therefore runs under a deadline, which
// is cleared on success: the stream becomes a long-lived tunnel afterwards and a
// leftover deadline would kill it mid-transfer.
func connectThroughEdge(stream *http3.RequestStream, req *http.Request, deadline time.Time) (*http.Response, error) {
	if err := stream.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("设置 CONNECT 超时失败：%w", err)
	}
	if err := stream.SendRequestHeader(req); err != nil {
		return nil, fmt.Errorf("发送 CONNECT 失败：%w", err)
	}
	resp, err := stream.ReadResponse()
	if err != nil {
		return nil, fmt.Errorf("读取 CONNECT 响应失败：%w", err)
	}
	if err := stream.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("清除 CONNECT 超时失败：%w", err)
	}
	return resp, nil
}

// connectDeadline returns the deadline to bound a CONNECT exchange with: the
// caller's remaining budget when it has one, otherwise a fixed fallback so the
// exchange can never be unbounded.
func connectDeadline(ctx context.Context, fallback time.Duration) time.Time {
	deadline := time.Now().Add(fallback)
	if dl, ok := ctx.Deadline(); ok {
		if dl.Before(deadline) {
			return dl
		}
	}
	return deadline
}
