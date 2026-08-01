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
	tmp := t.TempDir()
	rules := filepath.Join(tmp, "rules.txt")
	if err := os.WriteFile(rules, []byte(
		"proxy,domain:proxy.example\n"+
			"direct,domain:direct.example\n"), 0o644); err != nil {
		t.Fatalf("写规则文件失败：%v", err)
	}
	cfg := &Config{RulesPath: rules, GeoDir: filepath.Join(tmp, "geo")}
	reg := &registration.Registration{
		AssignedIPv4: "172.16.0.2",
		AssignedIPv6: "2606:4700:110:8a2e:fb70:7a34:2f7e:1",
	}
	fd := &fakeDialer{}
	k, err := newKernel(cfg, reg, []string{"162.159.192.1:443"}, &tls.Config{}, func() (dialer, error) {
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
