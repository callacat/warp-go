package core

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"warp/registration"
)

// fakeDialer 记录拨号参数并返回管道连接，避免单测发起真实网络连接。
// 实现 dialer 接口（DialTunnel + Close）。
type fakeDialer struct {
	mu     sync.Mutex
	addr   string
	calls  int
	closed bool
}

func (f *fakeDialer) DialTunnel(_ context.Context, targetAddr string) (net.Conn, error) {
	f.mu.Lock()
	f.calls++
	f.addr = targetAddr
	f.mu.Unlock()
	a, _ := net.Pipe()
	return a, nil
}

func (f *fakeDialer) ResolveDNS(_ context.Context, host string) (net.IP, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return net.ParseIP("1.1.1.1"), nil
}

func (f *fakeDialer) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeDialer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeDialer) lastAddr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addr
}

func (f *fakeDialer) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// newTestKernel 用临时规则文件 + 假拨号器构建 Kernel（无网络）。
func newTestKernel(t *testing.T) (*Kernel, *fakeDialer) {
	t.Helper()
	return newTestKernelRules(t, "proxy,domain:proxy.example\n"+
		"direct,domain:direct.example\n")
}

// newTestKernelRules 与 newTestKernel 相同，但规则内容由调用方指定
// （reject 等测试需要自定义规则文件）。
func newTestKernelRules(t *testing.T, rules string) (*Kernel, *fakeDialer) {
	t.Helper()
	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "rules.txt")
	if err := os.WriteFile(rulesPath, []byte(rules), 0o644); err != nil {
		t.Fatalf("写规则文件失败：%v", err)
	}
	cfg := &Config{RulesPath: rulesPath, GeoDir: filepath.Join(tmp, "geo")}
	reg := &registration.Registration{
		AssignedIPv4: "172.16.0.2",
		AssignedIPv6: "2606:4700:110:8a2e:fb70:7a34:2f7e:1",
	}
	fd := &fakeDialer{}
	k, err := newKernel(context.Background(), cfg, reg, []string{"162.159.192.1:443"}, &tls.Config{}, func() (dialer, error) {
		return fd, nil
	})
	if err != nil {
		t.Fatalf("newKernel 失败：%v", err)
	}
	t.Cleanup(func() { _ = k.Close() })
	return k, fd
}

// T1：有效临时 config/rules 构建 Kernel → Start → Route 返回动作；
// 附带校验 AssignedIPv4/6 访问器。
func TestKernelNewStartRoute(t *testing.T) {
	k, _ := newTestKernel(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- k.Start(ctx) }()

	if action, matched := k.Route("proxy.example", netip.Addr{}); action != "proxy" || !matched {
		t.Errorf("Route(proxy.example) = (%q, %v)，期望 (\"proxy\", true)", action, matched)
	}
	if v4 := k.AssignedIPv4(); v4.String() != "172.16.0.2" {
		t.Errorf("AssignedIPv4() = %s，期望 172.16.0.2", v4)
	}
	if v6 := k.AssignedIPv6(); v6.String() != "2606:4700:110:8a2e:fb70:7a34:2f7e:1" {
		t.Errorf("AssignedIPv6() = %s，期望 2606:4700:110:8a2e:fb70:7a34:2f7e:1", v6)
	}

	k.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start 返回错误：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start 未在 Stop 后返回")
	}
}

// T2：命中 proxy 规则 → ("proxy", true)。
func TestKernelRouteProxy(t *testing.T) {
	k, _ := newTestKernel(t)
	action, matched := k.Route("www.proxy.example", netip.Addr{})
	if action != "proxy" || !matched {
		t.Errorf("Route = (%q, %v)，期望 (\"proxy\", true)", action, matched)
	}
}

// T3：命中 direct 规则 → ("direct", true)。
func TestKernelRouteDirect(t *testing.T) {
	k, _ := newTestKernel(t)
	action, matched := k.Route("direct.example", netip.Addr{})
	if action != "direct" || !matched {
		t.Errorf("Route = (%q, %v)，期望 (\"direct\", true)", action, matched)
	}
}

// T3b：命中 reject 规则 → ("reject", true)。验证 reject 规则能从规则文件
// 解析（route 包 validActions 含 ActionReject）并原样透传到 Kernel.Route，
// 由 androidvpn.NewConnectionEx 关闭连接（T7：Android TUN 的 REJECT 通路）。
func TestKernelRouteReject(t *testing.T) {
	k, _ := newTestKernelRules(t, "reject,domain:blocked.example\n")
	action, matched := k.Route("blocked.example", netip.Addr{})
	if action != "reject" || !matched {
		t.Errorf("Route = (%q, %v)，期望 (\"reject\", true)", action, matched)
	}
	if action, matched := k.Route("allowed.example", netip.Addr{}); action != "" || matched {
		t.Errorf("Route(未命中) = (%q, %v)，期望 (\"\", false)", action, matched)
	}
}

// T3c：ReloadRules 从磁盘重新加载规则（GUI 规则页"重新加载"按钮；Android
// 规则页报"分流引擎未初始化"的修复依赖此方法在 Kernel 上可用）。修改规则
// 文件后 ReloadRules 应让新规则生效；引擎未初始化（构造失败路径）报明确错误。
func TestKernelReloadRules(t *testing.T) {
	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "rules.txt")
	if err := os.WriteFile(rulesPath, []byte("proxy,domain:proxy.example\n"), 0o644); err != nil {
		t.Fatalf("写规则文件失败：%v", err)
	}
	cfg := &Config{RulesPath: rulesPath, GeoDir: filepath.Join(tmp, "geo")}
	reg := &registration.Registration{
		AssignedIPv4: "172.16.0.2",
		AssignedIPv6: "2606:4700:110:8a2e:fb70:7a34:2f7e:1",
	}
	k, err := newKernel(context.Background(), cfg, reg, []string{"162.159.192.1:443"}, &tls.Config{}, func() (dialer, error) {
		return &fakeDialer{}, nil
	})
	if err != nil {
		t.Fatalf("newKernel 失败：%v", err)
	}
	defer k.Close()

	// 初始规则命中 proxy.example → proxy。
	if action, matched := k.Route("proxy.example", netip.Addr{}); action != "proxy" || !matched {
		t.Fatalf("初始 Route(proxy.example) = (%q, %v)，期望 (\"proxy\", true)", action, matched)
	}

	// 修改规则文件（direct 化）并 ReloadRules → 新规则应生效。
	if err := os.WriteFile(rulesPath, []byte("direct,domain:proxy.example\n"), 0o644); err != nil {
		t.Fatalf("改写规则文件失败：%v", err)
	}
	if err := k.ReloadRules(); err != nil {
		t.Fatalf("ReloadRules 失败：%v", err)
	}
	if action, matched := k.Route("proxy.example", netip.Addr{}); action != "direct" || !matched {
		t.Errorf("ReloadRules 后 Route(proxy.example) = (%q, %v)，期望 (\"direct\", true)", action, matched)
	}
}

// T4：未命中 → ("", false)（隐式 direct 兜底）；引擎已关闭（Close 后）也返回 ("", false)。
func TestKernelRouteUnmatched(t *testing.T) {
	k, _ := newTestKernel(t)
	if action, matched := k.Route("other.example", netip.Addr{}); action != "" || matched {
		t.Errorf("Route(未命中) = (%q, %v)，期望 (\"\", false)", action, matched)
	}
	if err := k.Close(); err != nil {
		t.Fatalf("Close 失败：%v", err)
	}
	if action, matched := k.Route("proxy.example", netip.Addr{}); action != "" || matched {
		t.Errorf("Close 后 Route = (%q, %v)，期望 (\"\", false)", action, matched)
	}
}

// T5：DialTunnel 委托给拨号器并传递目标地址。
func TestKernelDialTunnel(t *testing.T) {
	k, fd := newTestKernel(t)
	conn, err := k.DialTunnel(context.Background(), "example.com:443")
	if err != nil {
		t.Fatalf("DialTunnel 失败：%v", err)
	}
	defer conn.Close()
	if fd.callCount() != 1 {
		t.Errorf("拨号器调用次数 = %d，期望 1", fd.callCount())
	}
	if addr := fd.lastAddr(); addr != "example.com:443" {
		t.Errorf("拨号目标 = %q，期望 example.com:443", addr)
	}
}

// T5b：ResolveDNS 委托给拨号器并返回解析结果。
func TestKernelResolveDNS(t *testing.T) {
	k, fd := newTestKernel(t)
	ip, err := k.ResolveDNS(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("ResolveDNS 失败：%v", err)
	}
	if fd.callCount() != 1 {
		t.Errorf("拨号器调用次数 = %d，期望 1", fd.callCount())
	}
	if !ip.Equal(net.ParseIP("1.1.1.1")) {
		t.Errorf("解析结果 = %v，期望 1.1.1.1", ip)
	}
}

// T6：Close 幂等（两次调用不 panic，拨号器只关闭一次）；关闭后 DialTunnel 报错。
func TestKernelCloseIdempotent(t *testing.T) {
	k, fd := newTestKernel(t)
	if err := k.Close(); err != nil {
		t.Fatalf("第一次 Close 失败：%v", err)
	}
	if err := k.Close(); err != nil {
		t.Fatalf("第二次 Close 失败：%v", err)
	}
	if !fd.isClosed() {
		t.Error("拨号器未被关闭")
	}
	if _, err := k.DialTunnel(context.Background(), "x:1"); err == nil {
		t.Error("Close 后 DialTunnel 应返回错误")
	}
}

// T7：NewKernelContext 在 ctx 已取消时立即失败（不调用拨号工厂）——Android
// 桥"装配中停止"依赖此语义：nativeStopVpn cancel() 后装配必须中止，而非
// 继续拨号重试（v0.5.10 反馈"停止无效"）。
func TestKernelNewContextCanceledSkipsDial(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 装配开始前已取消

	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "rules.txt")
	if err := os.WriteFile(rulesPath, []byte("proxy,domain:x\n"), 0o644); err != nil {
		t.Fatalf("写规则文件失败：%v", err)
	}
	cfg := &Config{RulesPath: rulesPath, GeoDir: filepath.Join(tmp, "geo")}
	reg := &registration.Registration{AssignedIPv4: "172.16.0.2"}

	dialCalled := false
	_, err := newKernel(ctx, cfg, reg, []string{"162.159.192.1:443"}, &tls.Config{}, func() (dialer, error) {
		dialCalled = true
		return &fakeDialer{}, nil
	})
	if err == nil {
		t.Fatal("ctx 已取消时 NewKernelContext 应返回错误")
	}
	if dialCalled {
		t.Error("ctx 已取消时不应调用拨号工厂")
	}
}

// T8：装配完成后，运行期 ctx（background 派生）取消时 Start 返回 nil 且
// kernel 仍可用——Android 桥 v0.5.20 的修复语义：装配 ctx（60s 拨号超时）
// 与运行期 ctx 分离，装配超时到期不得静默杀死 TUN 栈（此前直接把带超时的
// 装配 ctx 传给 Start，60s 后 sing-tun 栈整体关闭但 started 仍 true——
// 用户看到"VPN 开"却无网络，真机报"use of closed network connection"）。
func TestKernelStartRuntimeCtxCancelKeepsKernel(t *testing.T) {
	k, fd := newTestKernel(t)

	// 运行期 ctx：与装配分离，background 派生（模拟 startVpnKernel 的
	// runCtx 切换）。
	runCtx, runCancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- k.Start(runCtx) }()

	// 运行中 kernel 可用：Route 正常判定、DialTunnel 正常建流。
	if action, matched := k.Route("proxy.example", netip.Addr{}); action != "proxy" || !matched {
		t.Errorf("Route(proxy.example) = (%q, %v)，期望 (\"proxy\", true)", action, matched)
	}
	conn, err := k.DialTunnel(context.Background(), "8.7.198.46:443")
	if err != nil {
		t.Fatalf("DialTunnel 失败：%v", err)
	}
	_ = conn.Close()
	if n := fd.callCount(); n != 1 {
		t.Errorf("DialTunnel 调用次数 = %d，期望 1", n)
	}

	// 取消运行期 ctx：Start 返回 nil（不报错），但 kernel 未被拆除
	// （拆除是 Stop/Close 的职责）——Route 仍可用。
	runCancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start 返回错误：%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start 未在运行期 ctx 取消后返回")
	}
	if action, matched := k.Route("proxy.example", netip.Addr{}); action != "proxy" || !matched {
		t.Errorf("ctx 取消后 Route 不可用：(%q, %v)，期望 kernel 保持可用", action, matched)
	}
	if fd.isClosed() {
		t.Error("ctx 取消不应关闭隧道拨号器（拆除是 Close 的职责）")
	}
}

// T9：Kernel.Stats / StartedAt 访问器——Android 状态页修复的底层契约：
// GUI GetStatus 的 Android 分支从 androidRuntime.kernel 读统计与启动时间，
// 不再读从未启动的 Server.kernel（nil → Stats 全 0、StartTime 零值——
// v0.5.22 修复"状态页启动时间不显示 + 流量统计无变化"）。统计经 Match 累加
// 后必须可读；未 Start 时 StartedAt 为零值（Android 桥自行记录真实开始时间）。
func TestKernelStatsAccessors(t *testing.T) {
	k, _ := newTestKernel(t)

	// 未 Start：StartedAt 零值（Server 语义），Stats 全 0。
	if !k.StartedAt().IsZero() {
		t.Errorf("未 Start 时 StartedAt() = %v，期望零值", k.StartedAt())
	}
	st := k.Stats()
	if st.ProxyHits != 0 || st.DirectHits != 0 || st.Misses != 0 || st.RejectedHits != 0 {
		t.Errorf("未匹配时 Stats() = %+v，期望全 0", st)
	}

	// 命中 proxy 规则 → ProxyHits 累加。
	_, _ = k.Route("www.proxy.example", netip.Addr{})
	st = k.Stats()
	if st.ProxyHits != 1 {
		t.Errorf("命中 proxy 后 ProxyHits = %d，期望 1", st.ProxyHits)
	}
	// 命中 direct 规则 → DirectHits 累加。
	_, _ = k.Route("direct.example", netip.Addr{})
	st = k.Stats()
	if st.DirectHits != 1 {
		t.Errorf("命中 direct 后 DirectHits = %d，期望 1", st.DirectHits)
	}
	// 未命中 → Misses 累加（引擎层返回 matched=false）。
	_, _ = k.Route("other.example", netip.Addr{})
	st = k.Stats()
	if st.Misses != 1 {
		t.Errorf("未命中后 Misses = %d，期望 1", st.Misses)
	}
}
