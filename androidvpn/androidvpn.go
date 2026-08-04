//go:build android

// Package androidvpn 提供 Android VPNService（TUN）集成：Java 侧
// VpnService.Builder.establish() 拿到 TUN fd 后传给本包，sing-tun 负责
// TUN 设备 + gVisor 用户态 TCP/UDP 栈，流量按 GEO 规则分流：
//   - proxy → 走 WARP 隧道（core/tunnel）
//   - direct → 本地直连
//
// 与 wireguard-android / sing-box / mihomo 同一模式：fd 从 Java 经 JNI
// 传 int 给 Go，Go 侧用 sing-tun 的 Options.FileDescriptor 包装。
package androidvpn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/sys/unix"
)

// stdLogger 把 Go 标准 log 适配到 sing 的 logger.Logger 接口。
type stdLogger struct{}

var _ logger.Logger = stdLogger{}

func (stdLogger) Trace(args ...any) { log.Print(args...) }
func (stdLogger) Debug(args ...any) { log.Print(args...) }
func (stdLogger) Info(args ...any)  { log.Print(args...) }
func (stdLogger) Warn(args ...any)  { log.Print(args...) }
func (stdLogger) Error(args ...any) { log.Print(args...) }
func (stdLogger) Fatal(args ...any) { log.Fatal(args...) }
func (stdLogger) Panic(args ...any) { log.Panic(args...) }

// RouteFunc / TunnelDial / DirectDial / Config 与 decideAction 定义在
// decision.go（无 Android 依赖，宿主与 Android 均编译，可单测）。

// Vpn 是 Android TUN 服务的运行实例。
type Vpn struct {
	tun    tun.Tun
	stack  tun.Stack
	cfg    Config
	fd     int // 原始 TUN fd：tun.New 包装前由 Stop 兜底关闭（Java 已 detachFd）
	ctx    context.Context
	cancel context.CancelFunc

	closeMu sync.Mutex
	closed  bool
}

// New 创建 TUN 服务实例（不启动）。
func New(cfg Config) (*Vpn, error) {
	if cfg.FileDescriptor <= 0 {
		return nil, errors.New("无效的 TUN fd")
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1500
	}
	if len(cfg.DNSServers) == 0 {
		cfg.DNSServers = []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	}
	return &Vpn{cfg: cfg, fd: cfg.FileDescriptor}, nil
}

// Start 启动 TUN 设备与 gVisor 栈，阻塞直到 ctx 取消或 Stop。
func (v *Vpn) Start(ctx context.Context) error {
	v.ctx, v.cancel = context.WithCancel(ctx)
	defer v.cancel()

	base := tun.Options{
		FileDescriptor: v.cfg.FileDescriptor,
		MTU:            v.cfg.MTU,
		Inet4Address:   v.cfg.Inet4Address,
		Inet6Address:   v.cfg.Inet6Address,
		DNSServers:     v.cfg.DNSServers,
		Logger:         stdLogger{},
	}
	t, err := tun.New(base)
	if err != nil {
		// fd 尚未被 NativeTun 包装，v.fd 由 Stop() 兜底关闭（Java 已 detachFd，
		// 不再负责关闭）。
		return fmt.Errorf("sing-tun 创建失败：%w", err)
	}
	v.tun = t
	v.fd = 0 // fd 所有权已转移给 NativeTun（其 Close 关闭）
	log.Printf("✓ TUN 已创建（fd=%d, mtu=%d）", v.cfg.FileDescriptor, v.cfg.MTU)

	// gVisor 用户态栈：TUN 上的 TCP/UDP 包在这里被解析成流。
	// 必须显式指定 "gvisor" 而非空串：空串在 sing-tun 里按编译标志
	// （WithGVisor）选择 NewMixed 或 NewSystem——CI 的 Android 构建若没带
	// with_gvisor tag，WithGVisor=false 落到 NewSystem。而 NewSystem 要求
	// Inet4Address[0] 前缀含 next 地址（HasNextAddress），我们传的是
	// /32 单地址（WARP 只分配一个 IP），必然报 "need one more IPv4 address
	// in first prefix for system stack"（v0.5.15 真机：TUN 栈创建失败）。
	// NewGVisor 只取前缀首个地址、不要求 next，与 /32 前缀匹配。
	//
	// UDPTimeout/ICMPTimeout 必须显式设置（v0.5.16 真机 SIGABRT 根因）：
	// sing v0.8.0 的 udpnat.New 对 timeout==0 直接 panic("invalid timeout")
	// 而非返回错误（经 NewUDPForwarder → udpnat.New 触发，异步 goroutine 内
	// panic 拖垮整个进程）。取值对齐 sing-box 默认：UDP NAT 5m / ICMP 10s。
	stack, err := tun.NewStack("gvisor", tun.StackOptions{
		Context:     v.ctx,
		Tun:         t,
		TunOptions:  base,
		UDPTimeout:  5 * time.Minute,
		ICMPTimeout: 10 * time.Second,
		Handler:     v,
		Logger:      stdLogger{},
	})
	if err != nil {
		t.Close()
		return fmt.Errorf("gVisor 栈创建失败：%w", err)
	}
	v.stack = stack
	log.Println("✓ gVisor 栈已就绪")

	if err := stack.Start(); err != nil {
		return fmt.Errorf("栈启动失败：%w", err)
	}
	log.Println("✓ TUN 已启动（阻塞中）")

	<-v.ctx.Done()
	return nil
}

// Stop 停止 TUN 服务。
func (v *Vpn) Stop() error {
	v.closeMu.Lock()
	defer v.closeMu.Unlock()
	if v.closed {
		return nil
	}
	v.closed = true
	if v.cancel != nil {
		v.cancel()
	}
	if v.stack != nil {
		_ = v.stack.Close()
	}
	if v.tun != nil {
		_ = v.tun.Close()
	}
	// 兜底：tun.New 尚未成功（v.fd 仍持有原始 fd，Java 已 detachFd 不再负责）
	// 时显式关闭，防泄漏。fd 已被 NativeTun 包装后 v.fd==0，此处不会双关。
	if v.fd > 0 {
		_ = unix.Close(v.fd)
		v.fd = 0
	}
	return nil
}

// --- sing-tun Handler 接口 ---

// PrepareConnection 在连接建立前回调；返回 nil 表示允许连接。
func (v *Vpn) PrepareConnection(network string, source, destination M.Socksaddr, routeContext tun.DirectRouteContext, timeout time.Duration) (tun.DirectRouteDestination, error) {
	return nil, nil
}

// NewConnectionEx 处理 TCP 连接（gVisor 栈解析后回调）。
func (v *Vpn) NewConnectionEx(ctx context.Context, conn net.Conn, source, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	log.Printf("[tun] TCP %s → %s", source.AddrString(), destination.String())
	go func() {
		var upstream net.Conn
		var err error

		// 路由判定委托 decision.go 的 decideAction：
		// proxy → 隧道；direct → 本地直连；reject → 拒连（绝不建连）。
		// 未命中 → 隐式 direct 兜底（与桌面一致）。
		//
		// 传入真实目标 IP（T8）：sing 的 Socksaddr 对 IP 字面量目标在 Addr
		// 字段保存真实 netip.Addr（Fqdn 为空），对域名目标 Addr 恒为零值
		// （ParseSocksaddrHostPort 只填 Fqdn），二者互斥——因此无条件传
		// destination.Addr 即可：IP 字面量目标让 geoip: 规则可命中，域名
		// 目标退化为零值、仅 host/geosite 规则生效（与修复前行为一致）。
		action, _ := decideAction(v.cfg.Route, destination.AddrString(), destination.Addr)
		upstream, err, rejected := resolveAction(action, ctx, destination.String(), v.cfg.TunnelDial, v.cfg.DirectDial)
		if rejected {
			log.Printf("[tun] 规则 reject：拒绝 %s → %s", source.AddrString(), destination.String())
			_ = conn.Close()
			if onClose != nil {
				onClose(err)
			}
			return
		}
		if err != nil {
			log.Printf("[tun] 拨号失败 %s：%v", destination.String(), err)
			_ = conn.Close()
			if onClose != nil {
				onClose(err)
			}
			return
		}
		var once sync.Once
		closeBoth := func() {
			once.Do(func() {
				_ = conn.Close()
				_ = upstream.Close()
			})
		}
		defer closeBoth()
		done := make(chan struct{}, 2)
		go func() { _, _ = io.Copy(upstream, conn); closeBoth(); done <- struct{}{} }()
		go func() { _, _ = io.Copy(conn, upstream); closeBoth(); done <- struct{}{} }()
		<-done
		if onClose != nil {
			onClose(nil)
		}
	}()
}

// NewPacketConnectionEx 处理 UDP（gVisor 栈解析后回调）。
func (v *Vpn) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, source, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	// UDP：当前直接经本机网络栈转发（与桌面端"UDP 不走隧道"一致）。
	log.Printf("[tun] UDP %s → %s（直连）", source.AddrString(), destination.String())
	go v.relayUDP(ctx, conn, destination, onClose)
}

// relayUDP 把 TUN 上收到的 UDP 数据报经本机栈转发到目标。
func (v *Vpn) relayUDP(ctx context.Context, conn N.PacketConn, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	remote, err := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   destination.Addr.AsSlice(),
		Port: int(destination.Port),
	})
	if err != nil {
		_ = conn.Close()
		return
	}
	defer remote.Close()

	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			buffer := buf.NewSize(65535)
			_, rerr := buffer.ReadOnceFrom(remote)
			if rerr != nil {
				buffer.Release()
				return
			}
			if werr := conn.WritePacket(buffer, destination); werr != nil {
				buffer.Release()
				return
			}
			buffer.Release()
		}
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			buffer := buf.NewSize(65535)
			_, rerr := conn.ReadPacket(buffer)
			if rerr != nil {
				buffer.Release()
				return
			}
			if _, werr := remote.Write(buffer.Bytes()); werr != nil {
				buffer.Release()
				return
			}
			buffer.Release()
		}
	}()
	<-done
	if onClose != nil {
		onClose(nil)
	}
}
