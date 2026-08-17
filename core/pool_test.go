package core

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
)

// stubDialer 实现 dialer 接口：记录 DialTunnel 调用次数，返回注入的 conn/err。
type stubDialer struct {
	name     string
	dials    atomic.Int64
	resolves atomic.Int64
	closed   atomic.Bool
	conn     net.Conn
	err      error
	dnsIP    net.IP
}

func (f *stubDialer) DialTunnel(_ context.Context, _ string) (net.Conn, error) {
	f.dials.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	if f.conn != nil {
		return f.conn, nil
	}
	return &net.TCPConn{}, nil
}

func (f *stubDialer) ResolveDNS(_ context.Context, _ string) (net.IP, error) {
	f.resolves.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	if f.dnsIP != nil {
		return f.dnsIP, nil
	}
	return net.IPv4(1, 1, 1, 1), nil
}

func (f *stubDialer) Close() error {
	f.closed.Store(true)
	return nil
}

func (f *stubDialer) dialCount() int64 { return f.dials.Load() }

// TestPoolRoundRobin 验证轮询分发：N 个拨号器、2N 次调用，每个恰好 N 次。
func TestPoolRoundRobin(t *testing.T) {
	var fs [3]stubDialer
	dials := make([]dialer, len(fs))
	for i := range fs {
		dials[i] = &fs[i]
	}
	p := newPoolDialer(dials)

	for i := 0; i < 6; i++ {
		if _, err := p.DialTunnel(context.Background(), "example.com:443"); err != nil {
			t.Fatalf("DialTunnel %d 失败：%v", i, err)
		}
	}
	for i := range fs {
		if got := fs[i].dialCount(); got != 2 {
			t.Fatalf("拨号器 %d 被调用 %d 次，期望 2（轮询均分）", i, got)
		}
	}
}

// TestPoolErrorFallthrough 验证失败换下一条：拨号器 0 报错时，调用落到拨号
// 器 1 并成功，且后续轮询从拨号器 1 继续（不重复撞坏节点）。
func TestPoolErrorFallthrough(t *testing.T) {
	f0 := &stubDialer{name: "d0", err: errors.New("d0 挂了")}
	f1 := &stubDialer{name: "d1"}
	f2 := &stubDialer{name: "d2"}
	p := newPoolDialer([]dialer{f0, f1, f2})

	// 第一次：next=0 → f0 失败 → f1 成功
	if _, err := p.DialTunnel(context.Background(), "x:443"); err != nil {
		t.Fatalf("应落到 f1 成功，实际错误：%v", err)
	}
	if f0.dialCount() != 1 || f1.dialCount() != 1 {
		t.Fatalf("f0=%d f1=%d，期望各 1 次", f0.dialCount(), f1.dialCount())
	}

	// 第二次：next=1 → f1 直接成功（f0 不再被撞）
	if _, err := p.DialTunnel(context.Background(), "x:443"); err != nil {
		t.Fatalf("第二次 DialTunnel 失败：%v", err)
	}
	if f0.dialCount() != 1 {
		t.Fatalf("f0 不应被再次调用，当前 %d", f0.dialCount())
	}
	if f1.dialCount() != 2 {
		t.Fatalf("f1 应被调用 2 次，当前 %d", f1.dialCount())
	}
}

// TestPoolAllErrors 验证全部失败返回最后一个错误（而非吞掉）。
func TestPoolAllErrors(t *testing.T) {
	p := newPoolDialer([]dialer{
		&stubDialer{name: "a", err: errors.New("e1")},
		&stubDialer{name: "b", err: errors.New("e2")},
	})
	_, err := p.DialTunnel(context.Background(), "x:443")
	if err == nil {
		t.Fatal("应返回错误")
	}
	if err.Error() != "e2" {
		t.Fatalf("应返回最后一个错误 e2，实际 %v", err)
	}
}

// TestPoolContextCancel 验证 ctx 取消时中途停止尝试。
func TestPoolContextCancel(t *testing.T) {
	f0 := &stubDialer{name: "a", err: errors.New("e1")}
	f1 := &stubDialer{name: "b"}
	p := newPoolDialer([]dialer{f0, f1})

	ctx, cancel := context.WithCancel(context.Background())
	// next=0 → f0 失败；在尝试 f1 前 ctx 已取消 → 不再试 f1
	p.next.Store(0)
	cancel()
	_, _ = p.DialTunnel(ctx, "x:443")
	if f1.dialCount() != 0 {
		t.Fatalf("ctx 取消后不应再尝试 f1，当前 %d", f1.dialCount())
	}
}

// TestPoolSinglePassthrough 验证单拨号器时 DialTunnel 直接透传（无轮询）。
func TestPoolSinglePassthrough(t *testing.T) {
	f := &stubDialer{name: "only"}
	p := newPoolDialer([]dialer{f})
	for i := 0; i < 3; i++ {
		if _, err := p.DialTunnel(context.Background(), "x:443"); err != nil {
			t.Fatalf("DialTunnel 失败：%v", err)
		}
	}
	if f.dialCount() != 3 {
		t.Fatalf("单拨号器应透传全部 3 次，当前 %d", f.dialCount())
	}
}

// healthStubDialer 包装 stubDialer，实现 serviceProbe（EnsureServiceable），
// 用于验证池的健康优先轮询。未实现 serviceProbe 的 stubDialer 保持原语义。
type healthStubDialer struct {
	stubDialer
	healthy     atomic.Bool
	ensureCalls atomic.Int64
}

func (h *healthStubDialer) EnsureServiceable() bool {
	h.ensureCalls.Add(1)
	return h.healthy.Load()
}

// TestPoolSkipsUnhealthyDialTunnel 验证死成员被跳过：不拨号、检查其健康状态，
// 请求落到健康成员。
func TestPoolSkipsUnhealthyDialTunnel(t *testing.T) {
	dead := &healthStubDialer{stubDialer: stubDialer{name: "dead"}}
	dead.healthy.Store(false)
	alive := &healthStubDialer{stubDialer: stubDialer{name: "alive"}}
	alive.healthy.Store(true)
	p := newPoolDialer([]dialer{dead, alive})
	p.next.Store(0)

	if _, err := p.DialTunnel(context.Background(), "x:443"); err != nil {
		t.Fatalf("DialTunnel 应成功：%v", err)
	}
	if dead.stubDialer.dialCount() != 0 {
		t.Fatalf("死成员不应被拨号，实际 %d", dead.stubDialer.dialCount())
	}
	if dead.ensureCalls.Load() != 1 {
		t.Fatalf("死成员应被检查健康 1 次（触发后台自愈），实际 %d", dead.ensureCalls.Load())
	}
	if alive.stubDialer.dialCount() != 1 {
		t.Fatalf("健康成员应承接 1 次，实际 %d", alive.stubDialer.dialCount())
	}
}

// TestPoolSkipsUnhealthyResolveDNS 验证 ResolveDNS 同样跳过死成员。
func TestPoolSkipsUnhealthyResolveDNS(t *testing.T) {
	dead := &healthStubDialer{stubDialer: stubDialer{name: "dead"}}
	dead.healthy.Store(false)
	alive := &healthStubDialer{stubDialer: stubDialer{name: "alive"}}
	alive.healthy.Store(true)
	p := newPoolDialer([]dialer{dead, alive})
	p.next.Store(0)

	if _, err := p.ResolveDNS(context.Background(), "example.com"); err != nil {
		t.Fatalf("ResolveDNS 应成功：%v", err)
	}
	if dead.stubDialer.resolves.Load() != 0 {
		t.Fatalf("死成员不应解析，实际 %d", dead.stubDialer.resolves.Load())
	}
	if alive.stubDialer.resolves.Load() != 1 {
		t.Fatalf("健康成员应承接 1 次，实际 %d", alive.stubDialer.resolves.Load())
	}
}

// TestPoolAllUnhealthyFallsBack 验证全部成员不可用时回退首选成员正常拨号
// （其内部 join 重建航班并等待，等价单连接语义），保证请求不被吞掉。
func TestPoolAllUnhealthyFallsBack(t *testing.T) {
	a := &healthStubDialer{stubDialer: stubDialer{name: "a", err: errors.New("a 不可用")}}
	a.healthy.Store(false)
	b := &healthStubDialer{stubDialer: stubDialer{name: "b", err: errors.New("b 不可用")}}
	b.healthy.Store(false)
	p := newPoolDialer([]dialer{a, b})
	p.next.Store(0)

	_, err := p.DialTunnel(context.Background(), "x:443")
	if err == nil {
		t.Fatal("全部不可用时应返回错误")
	}
	if a.stubDialer.dialCount() != 1 {
		t.Fatalf("回退应拨号首选 a 一次，实际 %d", a.stubDialer.dialCount())
	}
	if b.stubDialer.dialCount() != 0 {
		t.Fatalf("b 不应被拨号，实际 %d", b.stubDialer.dialCount())
	}
}

// TestPoolUnhealthyHealsBack 验证死成员自愈后重新入轮询（健康恢复 → 重新
// 被分配）。两次调用各落到一个成员，死成员自愈后不再被跳过。
func TestPoolUnhealthyHealsBack(t *testing.T) {
	d0 := &healthStubDialer{stubDialer: stubDialer{name: "d0"}}
	d0.healthy.Store(true)
	d1 := &healthStubDialer{stubDialer: stubDialer{name: "d1"}}
	d1.healthy.Store(true)
	p := newPoolDialer([]dialer{d0, d1})

	// d1 先死一次：round 0（next→d0）d0 承接；round 1（next→d1）跳过 d1 → d0
	d1.healthy.Store(false)
	p.next.Store(0)
	if _, err := p.DialTunnel(context.Background(), "x:443"); err != nil {
		t.Fatalf("第 1 次失败：%v", err)
	}
	if d1.stubDialer.dialCount() != 0 {
		t.Fatalf("死成员 d1 不应被拨号，实际 %d", d1.stubDialer.dialCount())
	}

	// d1 自愈：next=2（%2=0）→ d0；next=3（%2=1）→ d1 现在健康，正常承接
	d1.healthy.Store(true)
	p.next.Store(2)
	if _, err := p.DialTunnel(context.Background(), "x:443"); err != nil {
		t.Fatalf("第 2 次失败：%v", err)
	}
	p.next.Store(3)
	if _, err := p.DialTunnel(context.Background(), "x:443"); err != nil {
		t.Fatalf("第 3 次失败：%v", err)
	}
	if d1.stubDialer.dialCount() != 1 {
		t.Fatalf("自愈后 d1 应重新承接 1 次，实际 %d", d1.stubDialer.dialCount())
	}
}

// TestPoolCloseAll 验证 Close 关闭全部拨号器。
func TestPoolCloseAll(t *testing.T) {
	var fs [3]stubDialer
	dials := make([]dialer, len(fs))
	for i := range fs {
		dials[i] = &fs[i]
	}
	p := newPoolDialer(dials)
	if err := p.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	for i := range fs {
		if !fs[i].closed.Load() {
			t.Fatalf("拨号器 %d 未被 Close", i)
		}
	}
}

// TestPoolResolveDNSRoundRobin 验证 ResolveDNS 也轮询分发。
func TestPoolResolveDNSRoundRobin(t *testing.T) {
	var fs [2]stubDialer
	dials := make([]dialer, len(fs))
	for i := range fs {
		dials[i] = &fs[i]
	}
	p := newPoolDialer(dials)
	for i := 0; i < 4; i++ {
		if _, err := p.ResolveDNS(context.Background(), "example.com"); err != nil {
			t.Fatalf("ResolveDNS %d 失败：%v", i, err)
		}
	}
	if fs[0].resolves.Load() != 2 || fs[1].resolves.Load() != 2 {
		t.Fatalf("DNS 分发不均：d0=%d d1=%d", fs[0].resolves.Load(), fs[1].resolves.Load())
	}
}

// TestRotateEdges 验证边缘表左旋：连接 i 从 edges[i%n] 开始，环内顺序保持。
func TestRotateEdges(t *testing.T) {
	edges := []string{"a", "b", "c", "d"}
	got := rotateEdges(edges, 2)
	want := []string{"c", "d", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("长度 %d，期望 %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rotate(2)[%d]=%s，期望 %s", i, got[i], want[i])
		}
	}
	// 单元素与空表原样返回
	if r := rotateEdges(edges[:1], 5); len(r) != 1 || r[0] != "a" {
		t.Fatalf("单元素应原样：%v", r)
	}
	if r := rotateEdges(nil, 3); r != nil {
		t.Fatalf("空表应返回 nil：%v", r)
	}
}

// TestTunnelConnectionsFor 验证连接数解析：≤0 → 1。
func TestTunnelConnectionsFor(t *testing.T) {
	for _, tt := range []struct{ in, want int }{
		{0, 1}, {-3, 1}, {1, 1}, {2, 2}, {5, 5},
	} {
		if got := tunnelConnectionsFor(tt.in); got != tt.want {
			t.Fatalf("tunnelConnectionsFor(%d) = %d，期望 %d", tt.in, got, tt.want)
		}
	}
}
