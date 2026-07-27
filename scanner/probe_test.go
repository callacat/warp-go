package scanner

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// TestProbeResult_unroutableFamily_PureFunction 验证 unroutableFamily 是一个
// 纯函数：只看错误的地址族类别，不触网。其判断与 tunnel/masque.go:255 的
// unroutableFamily 字面同义，但 scanner 包不 import tunnel，故两侧各自手抄一份、
// 语义靠本测试与 masque 端用法共同约束保持一致。
func TestProbeResult_unroutableFamily_PureFunction(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"ENETUNREACH", syscall.ENETUNREACH, true},
		{"EAFNOSUPPORT", syscall.EAFNOSUPPORT, true},
		{"EHOSTUNREACH", syscall.EHOSTUNREACH, true},
		{"ioEOF", io.EOF, false},
		{"nilError", nil, false},
		{"randomError", errors.New("random"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unroutableFamily(tc.err); got != tc.want {
				t.Fatalf("unroutableFamily(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestProbeEdge_InjectedDialerSuccess 替换 probeDialer 为返回成功的假实现，
// 验证 probeEdge 把拨号行为委托给 probeDialer，并能正确组装成功结果。
// 关键断言：fake 收到的 edgeAddr 参数正确、返回 OK==true、RTT 透传、ErrReason 为空。
func TestProbeEdge_InjectedDialerSuccess(t *testing.T) {
	// 记录 fake 收到的参数。
	var gotAddr net.Addr
	var gotTLS *tls.Config
	var gotQUIC *quic.Config
	var callCount int32

	orig := probeDialer
	t.Cleanup(func() {
		probeDialer = orig // 恢复真实实现，避免污染同包其它测试。
	})
	probeDialer = func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		atomic.AddInt32(&callCount, 1)
		gotAddr = addr
		gotTLS = tlsCfg
		gotQUIC = quicCfg
		return probeResult{OK: true, RTT: 50 * time.Millisecond}, nil
	}

	edgeAddr := &net.UDPAddr{IP: net.ParseIP("162.159.36.1"), Port: 443}
	tlsCfg := &tls.Config{ServerName: "test"}
	quicCfg := &quic.Config{}

	res := probeEdge(context.Background(), edgeAddr, tlsCfg, quicCfg, 5*time.Second)

	if atomic.LoadInt32(&callCount) != 1 {
		t.Fatalf("probeDialer 被调用 %d 次，期望 1 次", atomic.LoadInt32(&callCount))
	}
	if gotAddr.String() != edgeAddr.String() {
		t.Fatalf("fake 收到 addr=%v，期望 %v", gotAddr, edgeAddr)
	}
	if gotTLS == nil {
		t.Fatalf("fake 未收到 tls.Config")
	}
	if gotQUIC == nil {
		t.Fatalf("fake 未收到 quic.Config")
	}
	if !res.OK {
		t.Fatalf("期望 OK==true，实际 OK==false")
	}
	if res.RTT != 50*time.Millisecond {
		t.Fatalf("期望 RTT=%v，实际 RTT=%v", 50*time.Millisecond, res.RTT)
	}
	if res.ErrReason != "" {
		t.Fatalf("成功结果 ErrReason 应为空，实际 %q", res.ErrReason)
	}
	if res.Addr != edgeAddr.String() {
		t.Fatalf("期望 Addr=%q，实际 Addr=%q", edgeAddr.String(), res.Addr)
	}
}

// TestProbeEdge_InjectedDialerFailure 替换 probeDialer 为返回错误的假实现，
// 验证 probeEdge 组装失败结果：OK==false、RTT==0、ErrReason 非空。
func TestProbeEdge_InjectedDialerFailure(t *testing.T) {
	orig := probeDialer
	t.Cleanup(func() {
		probeDialer = orig
	})
	dialErr := errors.New("QUIC 拨号失败：握手超时")
	probeDialer = func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		return probeResult{}, dialErr
	}

	edgeAddr := &net.UDPAddr{IP: net.ParseIP("162.159.36.1"), Port: 443}
	res := probeEdge(context.Background(), edgeAddr, &tls.Config{}, &quic.Config{}, 5*time.Second)

	if res.OK {
		t.Fatalf("期望 OK==false，实际 OK==true")
	}
	if res.RTT != 0 {
		t.Fatalf("失败结果 RTT 应为 0，实际 %v", res.RTT)
	}
	if res.ErrReason == "" {
		t.Fatalf("失败结果 ErrReason 不应为空")
	}
	if res.Addr != edgeAddr.String() {
		t.Fatalf("失败结果应仍记录 Addr，期望 %q，实际 %q", edgeAddr.String(), res.Addr)
	}
}

// TestProbeEdge_RealDialerPathUsesTimeout 不直接触网，但验证 probeEdge 在 ctx
// 已取消时，dialer 返回的 ctx.Err 会被记录为失败（用注入的 fake 返回 ctx.Err）。
// 这验证了 perProbeTimeout 子 ctx 的取消传导。
func TestProbeEdge_RealDialerPathUsesTimeout(t *testing.T) {
	orig := probeDialer
	t.Cleanup(func() {
		probeDialer = orig
	})

	var sawCtx context.Context
	probeDialer = func(ctx context.Context, addr net.Addr, tlsCfg *tls.Config, quicCfg *quic.Config) (probeResult, error) {
		sawCtx = ctx
		// 模拟 dialer 观察到 ctx 已取消并返回 ctx.Err。
		return probeResult{}, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 先取消，再用取消后的 ctx 调 probeEdge。

	edgeAddr := &net.UDPAddr{IP: net.ParseIP("162.159.36.1"), Port: 443}
	res := probeEdge(ctx, edgeAddr, &tls.Config{}, &quic.Config{}, 5*time.Second)

	if res.OK {
		t.Fatalf("ctx 取消后期望 OK==false，实际 OK==true")
	}
	if res.RTT != 0 {
		t.Fatalf("ctx 取消后 RTT 应为 0，实际 %v", res.RTT)
	}
	if res.ErrReason == "" {
		t.Fatalf("ctx 取消后 ErrReason 不应为空")
	}
	if sawCtx == nil {
		t.Fatalf("dialer 未被调用，sawCtx 为 nil")
	}
	// dialer 收到的 ctx 应该是 perProbeTimeout 子 ctx，它已被父 ctx 取消传播。
	if sawCtx.Err() == nil {
		t.Fatalf("dialer 收到的 ctx 应已被父 ctx 取消传播，实际未取消")
	}
}
