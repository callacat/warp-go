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
	"sync/atomic"
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
	dns    *dnsInterceptor // TunnelDNS 非 nil 时创建；nil 表示不拦截 TUN DNS
	fd     int             // 原始 TUN fd：tun.New 包装前由 Stop 兜底关闭（Java 已 detachFd）
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
		cfg.MTU = DefaultMTU
	}
	if len(cfg.DNSServers) == 0 {
		cfg.DNSServers = []netip.Addr{netip.MustParseAddr("1.1.1.1")}
	}
	var dns *dnsInterceptor
	if cfg.TunnelDNS != nil {
		dns = NewDNSInterceptor(cfg.TunnelDNS)
	}
	return &Vpn{cfg: cfg, fd: cfg.FileDescriptor, dns: dns}, nil
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
		//
		// DNS 拦截还原（v0.5.24）：IP 字面量目标查 IP→域名映射表，命中则
		// 用域名走 DialTunnel——域名路径内部再次隧道 DoH 解析，CONNECT 目标
		// 永远处于边缘网络视图（系统 DNS 的 IP 与边缘视图不同，CONNECT 会
		// hang 到 deadline；这是 Android 外网不通的根因）。路由判定 host
		// 同步换成域名，让 host/geosite 规则可命中；geoip 仍看 Addr。
		host := destination.AddrString()
		targetAddr := destination.String()
		var mappedHost string
		if v.dns != nil && destination.Addr.IsValid() {
			if domain, ok := v.dns.LookupDomain(destination.Addr); ok {
				host = domain
				mappedHost = domain
			}
		}
		action, _ := decideAction(v.cfg.Route, host, destination.Addr)
		// 日志打还原后的域名（若有）与判定 action，供分流可观测：此前只打
		// 原始 IP，无法判断国内流量是否命中 direct（v0.5.28 反馈"日志全 IP"）。
		// DNS 映射 miss 时 host 就是 IP 字面量，回退展示地址即可。
		log.Printf("[tun] TCP %s → %s（%s，action=%s）",
			source.AddrString(), destination.String(), host, action)
		// IP→域名还原只用于 proxy 分支：direct 保留原始 IP 拨号（v0.5.24
		// 回归：direct 还原域名触发 net.Dialer 物理解析 → 系统 DNS 又进 TUN
		// → 环路 canceled——真机日志 `lookup obus-cn.dc.heytapmobi.com:
		// canceled`）。该 IP 是隧道 DoH 解析出的真实 IP，物理网络同样可达。
		targetAddr = decideTunnelTarget(action, targetAddr, mappedHost)
		// 裸 IPv6 目标快速拒绝（v0.5.29 洞 B）：proxy 分支的裸 IPv6 字面量
		// （IP→域名映射 miss，非隧道 DNS 解析结果）只存在于本地视图，WARP
		// 边缘 CONNECT 不可达——A15 双栈 hang 到 deadline（firstByteMs=-1），
		// A14 边缘快拒。本地立即 RST（connection refused）让客户端 Happy
		// Eyeballs 快速回退 v4（隧道 DNS 解析出的 v4 一定可达）。隧道 DNS
		// 解析出的 v6（映射命中，mappedHost 非空）不受影响。
		if shouldRejectBareV6(action, destination.Addr, mappedHost) {
			log.Printf("[tun] 裸 IPv6 目标（映射 miss，边缘不可达）快速拒绝 %s → %s",
				source.AddrString(), destination.String())
			_ = conn.Close()
			if onClose != nil {
				onClose(errBareV6Proxy)
			}
			return
		}
		upstream, err, rejected := resolveAction(action, ctx, targetAddr, v.cfg.TunnelDial, v.cfg.DirectDial)
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
		t0 := time.Now()
		var upBytes, downBytes int64
		var firstDown atomic.Int64 // 0=未到首字节；>0 为会话起点到首字节的毫秒数
		type relayResult struct {
			n   int64
			err error
			dir string
		}
		done := make(chan relayResult, 2)
		relay := func(dst io.Writer, src io.Reader, dir *int64, isDown bool) {
			r := relayResult{dir: map[bool]string{true: "down", false: "up"}[isDown]}
			defer closeBoth()
			defer func() { done <- r }()
			buf := make([]byte, 32*1024)
			for {
				nr, rerr := src.Read(buf)
				if nr > 0 {
					nw, werr := dst.Write(buf[:nr])
					if nw > 0 {
						*dir += int64(nw)
						r.n += int64(nw)
						if isDown && firstDown.Load() == 0 {
							firstDown.CompareAndSwap(0, time.Since(t0).Milliseconds())
						}
					}
					if werr != nil {
						r.err = werr
						return
					}
				}
				if rerr != nil {
					r.err = rerr
					return
				}
			}
		}
		go relay(upstream, conn, &upBytes, false)
		go relay(conn, upstream, &downBytes, true)
		r1, r2 := <-done, <-done
		firstMs := int(firstDown.Load())
		if downBytes == 0 {
			firstMs = -1
		}
		relayErr := r1.err
		if relayErr == nil {
			relayErr = r2.err
		}
		// 双向都 EOF（正常关闭）→ 记 nil；单方向早退（0 字节）→ 保留错误，
		// 供区分"边缘断流（read upstream: EOF）"与"客户端放弃（read conn: EOF）"。
		other := r2
		if r1.err != nil && r2.err != nil {
			if r1.err == io.EOF || r2.err == io.EOF {
				relayErr = fmt.Errorf("%s:%v %s:%v", r1.dir, r1.err, r2.dir, r2.err)
			}
			_ = other
		}
		logTunnelClosed(host, upBytes, downBytes, firstMs,
			time.Since(t0).Milliseconds(), relayErr)
		if onClose != nil {
			onClose(nil)
		}
	}()
}

// NewPacketConnectionEx 处理 UDP（gVisor 栈解析后回调）。
func (v *Vpn) NewPacketConnectionEx(ctx context.Context, conn N.PacketConn, source, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	// DNS 拦截（v0.5.24）：TUN 内 DNS 拦截服务器地址的 UDP:53 查询走
	// HandleQuery（隧道 DoH 解析 + IP→域名映射），不转发物理网络——否则
	// Android 系统 DNS 解析出的 IP 与 WARP 边缘网络视图不同，CONNECT 该
	// IP 会 hang 到 deadline（Android 外网不通根因）。其余 UDP 保持直连
	// （与桌面端"UDP 不走隧道"一致）。
	if v.dns != nil && destination.Port == 53 && destination.Addr.IsValid() && destination.Addr == DNSInterceptAddr {
		v.handleDNSQuery(conn, destination, onClose)
		return
	}
	// QUIC:443 拦截（v0.5.28 阶段5）：浏览器 HTTP/3（QUIC:443）走 UDP 直连
	// （relayUDP → 物理网络），运营商封锁 UDP/QUIC 直连 → 浏览器外网打不开。
	// 上游 warp-svc 只有 ConnectTcpProxy（不支持 CONNECT-UDP / RFC 9298），
	// UDP 无法走 WARP 隧道。拦截后丢弃 QUIC 包，浏览器自动回退 HTTP/2 over
	// TCP:443 → NewConnectionEx → WARP 隧道 → 通。
	//
	// 这是九轮修复未触碰的 UDP 直连面：v0.5.13→v0.5.27 全在 TCP CONNECT 层
	// 打转，从没动过 UDP:443 直连。Chrome/Firefox 对 QUIC 失败的标准回退
	// 行为保证此方案有效（QUIC 探测超时后立即 TCP fallback，延迟约 100-300ms）。
	if shouldBlockUDP(destination.Port) {
		log.Printf("[tun] UDP %s → %s:%d（QUIC 拦截，丢弃 → 浏览器回退 TCP）",
			source.AddrString(), destination.AddrString(), destination.Port)
		logUDPClosed(destination.AddrString(), "quic-blocked", 0, nil)
		_ = conn.Close()
		if onClose != nil {
			onClose(nil)
		}
		return
	}
	// 其余 UDP：直接经本机网络栈转发（与桌面端"UDP 不走隧道"一致）。
	log.Printf("[tun] UDP %s → %s（直连）", source.AddrString(), destination.String())
	go v.relayUDP(ctx, conn, destination, onClose)
}

// handleDNSQuery 循环读取 TUN 内 DNS 查询报文并写回拦截响应。解析失败 /
// 不支持的查询类型 HandleQuery 返回 nil → 静默丢弃（Android 回退到下一个
// DNS 服务器，行为与非拦截时一致）。
func (v *Vpn) handleDNSQuery(conn N.PacketConn, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	defer func() {
		_ = conn.Close()
		if onClose != nil {
			onClose(nil)
		}
	}()
	for {
		buffer := buf.NewSize(65535)
		_, rerr := conn.ReadPacket(buffer)
		if rerr != nil {
			buffer.Release()
			return
		}
		resp := v.dns.HandleQuery(buffer.Bytes())
		buffer.Release()
		if resp == nil {
			continue
		}
		out := buf.NewSize(len(resp))
		_, _ = out.Write(resp)
		if werr := conn.WritePacket(out, destination); werr != nil {
			out.Release()
			return
		}
		out.Release()
	}
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
	// Android：直接 UDP 数据报经本机栈发出，socket 必须豁免出 VPN 路由
	// （protect），否则重新进入 TUN 造成环路风暴（v0.5.14 只 protect 了
	// QUIC 拨号 socket，UDP relay 未豁免）。
	protectConn(remote)

	var total atomic.Int64
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
			total.Add(int64(buffer.Len()))
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
			total.Add(int64(buffer.Len()))
			buffer.Release()
		}
	}()
	<-done
	<-done
	// debugdiag：量化 UDP 直连泄漏（QUIC:443 / 非拦截 DNS:53），供"打不开
	// 外网"分析。
	logUDPClosed(destination.AddrString(), udpKind(destination.Port), total.Load(), nil)
	if onClose != nil {
		onClose(nil)
	}
}
