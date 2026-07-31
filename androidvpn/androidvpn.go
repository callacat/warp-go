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

	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing-tun/control"
)

// RouteFunc 判定 (host, ip) 的转发行为，返回 "proxy" / "direct" / ""。
// 与 core 的 route.Engine.Match 语义一致；nil 时全部走 proxy（隧道）。
type RouteFunc func(host string, ip netip.Addr) (action string, matched bool)

// TunnelDial 建立到目标的 WARP 隧道字节流（core.MasqueClient.DialTunnel）。
type TunnelDial func(ctx context.Context, targetAddr string) (net.Conn, error)

// DirectDial 建立到目标的本地直连（net.Dialer 即可）。
type DirectDial func(ctx context.Context, targetAddr string) (net.Conn, error)

// Config 配置 TUN 服务。
type Config struct {
	// FileDescriptor 是 Java VpnService.Builder.establish() 的 TUN fd。
	FileDescriptor int
	// MTU 默认 1500；由 Java 侧传入（与 VpnService.Builder.setMtu 一致）。
	MTU uint32
	// Inet4Address / Inet6Address 是分配给 TUN 网卡的隧道内地址
	// （来自注册信息 assigned_ipv4/assigned_ipv6）。
	Inet4Address []netip.Prefix
	Inet6Address []netip.Prefix
	// DNS 是 TUN 内 DNS 服务器（默认 1.1.1.1）。
	DNSAddress []netip.Addr

	Route      RouteFunc
	TunnelDial TunnelDial
	DirectDial DirectDial
}

// Vpn 是 Android TUN 服务的运行实例。
type Vpn struct {
	tun     tun.Tun
	cfg     Config
	ctx     context.Context
	cancel  context.CancelFunc
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
	if len(cfg.DNSAddress) == 0 {
		cfg.DNSAddress = []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	}
	return &Vpn{cfg: cfg}, nil
}

// Start 启动 TUN 设备与 gVisor 栈，阻塞直到 ctx 取消或 Stop。
func (v *Vpn) Start(ctx context.Context) error {
	v.ctx, v.cancel = context.WithCancel(ctx)
	defer v.cancel()

	t, err := tun.New(tun.Options{
		FileDescriptor: v.cfg.FileDescriptor,
		MTU:            v.cfg.MTU,
		Inet4Address:   v.cfg.Inet4Address,
		Inet6Address:   v.cfg.Inet6Address,
		DNSAddress:     v.cfg.DNSAddress,
		Handler:        v,
		Logger:         log.Default(),
	})
	if err != nil {
		return fmt.Errorf("sing-tun 创建失败：%w", err)
	}
	v.tun = t
	log.Printf("✓ TUN 已创建（fd=%d, mtu=%d）", v.cfg.FileDescriptor, v.cfg.MTU)

	// gVisor 用户态栈：TUN 上的 TCP/UDP 包在这里被解析成流。
	stack, err := tun.NewStack(tun.StackOptions{
		Context:         v.ctx,
		Handler:         v,
		Logger:          log.Default(),
		Forwarder:       tun.StackForwarder,
		DomainResolve:   true,
		IncludeIPv4Range: false,
	})
	if err != nil {
		t.Close()
		return fmt.Errorf("gVisor 栈创建失败：%w", err)
	}
	defer stack.Close()
	log.Println("✓ gVisor 栈已就绪")

	// 注册地址给栈，允许收包。
	for _, p := range v.cfg.Inet4Address {
		_ = stack.AddAddress(netip.AddrFrom4([4]byte{0, 0, 0, 0}), p) // 由 Options 决定实际行为
		_ = p
	}

	// 启动 TUN 读循环（阻塞）。
	err = t.Start()
	if err != nil {
		return fmt.Errorf("TUN 启动失败：%w", err)
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
	if v.tun != nil {
		_ = v.tun.Close()
	}
	return nil
}

// --- sing-tun Handler 接口（N.TCPConnectionHandlerEx / N.UDPConnectionHandlerEx） ---

// NewConnection 处理 TCP 连接（gVisor 栈解析后回调）。
func (v *Vpn) NewConnection(ctx context.Context, conn N.Conn, metadata N.Metadata) error {
	host := metadata.Destination.AddrString()
	log.Printf("[tun] TCP %s → %s", metadata.Source.AddrString(), metadata.Destination.String())

	go func() {
		var upstream net.Conn
		var err error
		if v.shouldProxy(host) {
			if v.cfg.TunnelDial == nil {
				log.Printf("[tun] 隧道未配置，回落直连 %s", metadata.Destination.String())
				err = errors.New("tunnel not configured")
			} else {
				upstream, err = v.cfg.TunnelDial(ctx, metadata.Destination.String())
			}
		} else {
			if v.cfg.DirectDial == nil {
				upstream, err = (&net.Dialer{}).DialContext(ctx, "tcp", metadata.Destination.String())
			} else {
				upstream, err = v.cfg.DirectDial(ctx, metadata.Destination.String())
			}
		}
		if err != nil {
			log.Printf("[tun] 拨号失败 %s：%v", metadata.Destination.String(), err)
			_ = conn.Close()
			return
		}
		// 双向中继：任一侧结束即关闭两侧。
		var once sync.Once
		closeBoth := func() {
			once.Do(func() {
				_ = conn.Close()
				_ = upstream.Close()
			})
		}
		defer closeBoth()
		done := make(chan struct{}, 2)
		go func() { io.Copy(upstream, conn); closeBoth(); done <- struct{}{} }()
		go func() { io.Copy(conn, upstream); closeBoth(); done <- struct{}{} }()
		<-done
	}()
	return nil
}

// NewPacketConnection 处理 UDP（gVisor 栈解析后回调）。
func (v *Vpn) NewPacketConnection(ctx context.Context, conn N.PacketConn, metadata N.Metadata) error {
	// UDP：当前直接经本机网络栈转发（与桌面端"UDP 不走隧道"一致）。
	log.Printf("[tun] UDP %s → %s（直连）", metadata.Source.AddrString(), metadata.Destination.String())
	// 简化：用 net.DialUDP 转发每个数据包。
	go v.relayUDP(ctx, conn, metadata)
	return nil
}

// shouldProxy 用 RouteFunc 判定是否走隧道；无 RouteFunc 时全走隧道。
func (v *Vpn) shouldProxy(host string) bool {
	if v.cfg.Route == nil {
		return true
	}
	action, matched := v.cfg.Route(host, netip.Addr{})
	if !matched {
		return false // 未命中 → direct（与桌面一致）
	}
	return action == "proxy"
}

// relayUDP 把 TUN 上收到的 UDP 数据报经本机栈转发到目标。
func (v *Vpn) relayUDP(ctx context.Context, conn N.PacketConn, metadata N.Metadata) {
	remote, err := net.DialUDP("udp", nil, &net.UDPAddr{
		IP:   metadata.Destination.Addr().AsSlice(),
		Port: int(metadata.Destination.Port),
	})
	if err != nil {
		_ = conn.Close()
		return
	}
	defer remote.Close()

	// 从 TUN 收包 → 发给远程；从远程收包 → 写回 TUN。
	done := make(chan struct{}, 2)
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, rerr := conn.ReadFrom(buf)
			if rerr != nil {
				done <- struct{}{}
				return
			}
			_ = addr
			if _, werr := remote.Write(buf[:n]); werr != nil {
				done <- struct{}{}
				return
			}
		}
	}()
	go func() {
		buf := make([]byte, 65535)
		for {
			n, _, rerr := remote.ReadFrom(buf)
			if rerr != nil {
				done <- struct{}{}
				return
			}
			if _, werr := conn.WriteTo(buf[:n], metadata.Destination); werr != nil {
				done <- struct{}{}
				return
			}
		}
	}()
	<-done
}
