package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/wzshiming/socks5"
)

// acquirePacketConn 的注入式单测。镜像 scanner/probe_test.go 的注入思路，但不引入
// 包级可变 seam 变量——acquirePacketConn 仅依赖 c.socks5Dialer，而 *socks5.Dialer 自身
// 已暴露 ProxyDial / ProxyPacketDial 注入点（client.go:21/24）。仅需构造 *MasqueClient
// 并注入 mock dialer，即可直测四条分支，无需全局可变状态、无需触外网。
//
// 这条测试 seam 的选择本身是设计决策：相比 scanner 注入 probeDialer（包级 var），走库
// 自身注入点少一个可变点，且测试与生产共用同一 *socks5.Dialer 代码路径。

// TestAcquirePacketConn_DirectPath 覆盖直连分支（socks5Dialer 为 nil）：对 IPv4 / IPv6
// 边缘各按其地址族绑本地 socket，返回 *net.UDPConn 与解析出的 udpAddr。这条分支是
// warp-go 的原有行为，socks5 改动不得破坏它——族匹配一旦错乱会在双栈主机上选源歧义。
func TestAcquirePacketConn_DirectPath(t *testing.T) {
	c := &MasqueClient{} // socks5Dialer 为 nil → 直连

	t.Run("IPv4", func(t *testing.T) {
		pc, udpAddr, err := c.acquirePacketConn(context.Background(), "162.159.36.1:443")
		if err != nil {
			t.Fatalf("直连 IPv4 应成功，实际 err=%v", err)
		}
		defer pc.Close()

		uc, ok := pc.(*net.UDPConn)
		if !ok {
			t.Fatalf("直连路径应返回 *net.UDPConn，实际 %T", pc)
		}
		if udpAddr == nil || udpAddr.String() != "162.159.36.1:443" {
			t.Fatalf("udpAddr 期望 162.159.36.1:443，实际 %v", udpAddr)
		}
		if udpAddr.IP.To4() == nil {
			t.Fatalf("IPv4 edge 的 udpAddr 应为 IPv4 宿主族，实际 %v", udpAddr)
		}
		// 本地 socket 应能服务 IPv4 edge。net.IPv4zero 在双栈 Linux 上会被 ListenUDP 绑成
		// dual-stack [::]（IPV6_V6ONLY 默认关、0.0.0.0 等价 unspecified），仍能收发 IPv4——
		// 这正是生产路径的隐式行为，安全。故断言放宽为"可服务 IPv4"：dual-stack socket 或
		// IPv4 专用 socket 均合格，仅当绑成 IPv6 专用（如 ::1）才算错配。
		la, ok := uc.LocalAddr().(*net.UDPAddr)
		if !ok || !canServeIPv4(la) {
			t.Fatalf("IPv4 edge 应绑可服务 IPv4 的本地 socket，实际 %v", uc.LocalAddr())
		}
	})

	t.Run("IPv6", func(t *testing.T) {
		pc, udpAddr, err := c.acquirePacketConn(context.Background(), "[2606:4700:d0::a29f:c001]:443")
		if err != nil {
			t.Fatalf("直连 IPv6 应成功，实际 err=%v", err)
		}
		defer pc.Close()

		uc, ok := pc.(*net.UDPConn)
		if !ok {
			t.Fatalf("直连路径应返回 *net.UDPConn，实际 %T", pc)
		}
		if udpAddr == nil || udpAddr.IP == nil || udpAddr.IP.To4() != nil {
			t.Fatalf("IPv6 edge 的 udpAddr 应为 IPv6 宿主族，实际 %v", udpAddr)
		}
		// IPv6 候选绑 IPv6zero（masque.go:337）。同样放宽为"可服务 IPv6"：dual-stack 的
		// [::] 与 IPv6 专用的 ::1 均合格，仅当绑成 IPv4 专用才算错配。
		la, ok := uc.LocalAddr().(*net.UDPAddr)
		if !ok || !canServeIPv6(la) {
			t.Fatalf("IPv6 edge 应绑可服务 IPv6 的本地 socket，实际 %v", uc.LocalAddr())
		}
	})
}

// TestAcquirePacketConn_Socks5Path 覆盖 socks5 分支：注入 ProxyDial / ProxyPacketDial，
// 用 net.Pipe 演代理完成 UDP ASSOCIATE 握手，断言返回 *socks5.UDPConn（实现 net.PacketConn）
// 且 udpAddr 等于 edge。全程不触外网：控制连接走 net.Pipe、本地 UDP socket 走 127.0.0.1
// 回环。这是与 scanner/probe_test.go 同一测试哲学，但走库自身注入点。
func TestAcquirePacketConn_Socks5Path(t *testing.T) {
	// relay 中继本地一端：127.0.0.1:0 随机端口。acquirePacketConn 不在此路径收发数据报，
	// 此 socket 仅给 *socks5.UDPConn 包装用。
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("监听本地 UDP 失败：%v", err)
	}
	defer pc.Close()

	// BND.ADDR 用 pc 的真实本地地址：贴近真实 relay，并使 NewUDPConn 的 proxyAddress 与
	// 底层 PacketConn 同址——任何 conventToUDPAddr 回归都会在构造阶段暴露。
	bndAddr, ok := pc.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("LocalAddr 不是 *net.UDPAddr：%T", pc.LocalAddr())
	}

	// net.Pipe 模拟到代理的 TCP 控制连接。两个端点分别给 client 与脚本代理侧。
	clientConn, proxyConn := net.Pipe()
	defer clientConn.Close()
	defer proxyConn.Close()

	scriptDone := make(chan error, 1)
	go func() {
		scriptDone <- playSocks5AssociateRelay(proxyConn, bndAddr)
	}()

	d, err := socks5.NewDialer("socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatalf("构造 socks5.Dialer 失败：%v", err)
	}
	// 关掉库内部 deadline：NewDialer 默认 1 分钟，会 SetDeadline 到 net.Pipe 上，
	// 在严格一问一答的同步握手里是多余的，且临超时会假失败。生产路径靠 dialAddr
	// 的 context.WithTimeout 管超时，故这里走 d.Timeout=0 等价同一路径。
	d.Timeout = 0
	d.ProxyDial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "tcp" {
			t.Errorf("ProxyDial network 期望 tcp，实际 %q", network)
		}
		if address != "127.0.0.1:1080" {
			t.Errorf("ProxyDial address 期望 127.0.0.1:1080，实际 %q", address)
		}
		return clientConn, nil
	}
	d.ProxyPacketDial = func(ctx context.Context, network, address string) (net.PacketConn, error) {
		// wzshiming 对 UDP ASSOCIATE 调 proxyPacketDial(ctx, "udp", ":0")（client.go:178）。
		if address != ":0" {
			t.Errorf("ProxyPacketDial address 期望 :0，实际 %q", address)
		}
		return pc, nil
	}

	c := &MasqueClient{socks5Dialer: d, socks5Addr: "127.0.0.1:1080"}
	got, udpAddr, err := c.acquirePacketConn(context.Background(), "162.159.36.1:443")
	if err != nil {
		// 握手走通前若失败也要让脚本 goroutine 不挂：关 proxyConn 使其 EOF 退出。
		proxyConn.Close()
		<-scriptDone
		t.Fatalf("socks5 路径应成功，实际 err=%v", err)
	}
	defer got.Close()

	if _, ok := got.(*socks5.UDPConn); !ok {
		t.Fatalf("期望 *socks5.UDPConn，实际 %T", got)
	}
	if udpAddr == nil || udpAddr.String() != "162.159.36.1:443" {
		t.Fatalf("udpAddr 期望 162.159.36.1:443，实际 %v", udpAddr)
	}
	if udpAddr.IP.To4() == nil {
		t.Fatalf("IPv4 edge 的 udpAddr 应为 IPv4 宿主族，实际 %v", udpAddr)
	}

	// 握手完成后脚本 goroutine 进入阻塞读（模拟控制连接常开）；关 proxyConn 让其退出，
	// 再读 scriptDone 确认握手字节序列无误。
	proxyConn.Close()
	if err := <-scriptDone; err != nil {
		t.Fatalf("脚本侧握手失败：%v", err)
	}
}

// TestAcquirePacketConn_Socks5Error 覆盖 socks5 拨号返错分支：edge 地址合法以越过
// ResolveUDPAddr，确保错误确实来自 ProxyDial 路径而非地址解析，并被包成
// "SOCKS5 UDP ASSOCIATE 到 %s 失败"。errors.Is 链确保原始错误被 %w 透传。
func TestAcquirePacketConn_Socks5Error(t *testing.T) {
	dialErr := errors.New("dial boom")
	d, err := socks5.NewDialer("socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatalf("构造 socks5.Dialer 失败：%v", err)
	}
	d.Timeout = 0
	d.ProxyDial = func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, dialErr
	}

	c := &MasqueClient{socks5Dialer: d, socks5Addr: "127.0.0.1:1080"}
	pc, _, err := c.acquirePacketConn(context.Background(), "162.159.36.1:443")
	if err == nil {
		if pc != nil {
			pc.Close()
		}
		t.Fatalf("ProxyDial 返错时应返回错误")
	}
	if pc != nil {
		t.Fatalf("错误路径不应返回 PacketConn")
	}
	if !strings.Contains(err.Error(), "SOCKS5 UDP ASSOCIATE") {
		t.Fatalf("错误应含 \"SOCKS5 UDP ASSOCIATE\"，实际 %q", err.Error())
	}
	if !errors.Is(err, dialErr) {
		t.Fatalf("错误应 errors.Is 原始 dialErr，实际 %v", err)
	}
}

// TestAcquirePacketConn_BadEdgeAddr 覆盖非法 edge 地址：解析在 socks5 调用前失败。
// ProxyDial 设为被调即 t.Fatalf，断言该路径未被触发——这是"解析挡在最前"的不变性。
func TestAcquirePacketConn_BadEdgeAddr(t *testing.T) {
	d, err := socks5.NewDialer("socks5://127.0.0.1:1080")
	if err != nil {
		t.Fatalf("构造 socks5.Dialer 失败：%v", err)
	}
	d.Timeout = 0
	d.ProxyDial = func(ctx context.Context, network, address string) (net.Conn, error) {
		t.Fatalf("ProxyDial 不应在地址解析失败前被调用")
		return nil, nil
	}

	c := &MasqueClient{socks5Dialer: d, socks5Addr: "127.0.0.1:1080"}
	pc, _, err := c.acquirePacketConn(context.Background(), "not:an:addr")
	if err == nil {
		if pc != nil {
			pc.Close()
		}
		t.Fatalf("非法 edge 地址应返回错误")
	}
	if !strings.Contains(err.Error(), "解析边缘地址") {
		t.Fatalf("错误应含 \"解析边缘地址\"，实际 %q", err.Error())
	}
}

// canServeIPv4 判断一个本地 UDP 绑定地址能否收发 IPv4 数据报。net.IPv4zero 在双栈
// 内核上被 ListenUDP 绑成 dual-stack [::]（IPV6_V6OFF + unspecified），仍可服务 IPv4；
// 只有 IPv6 专用地址（::1 等，IP.IsUnspecified 设且 To4==nil 但 To16 非 4in6）才不行。
// 用 "IP 为 IPv4 或为 unspecified（dual-stack 兜底）" 作合格判据——这覆盖生产路径里
// ListenUDP(IPv4zero) 在两类内核上的真实输出。
func canServeIPv4(la *net.UDPAddr) bool {
	if la.IP.To4() != nil {
		return true // IPv4 字面地址（含 0.0.0.0）。
	}
	// 剩下的是 IPv6 字面。unspecified 的 [::] 在双栈主机上等价 dual-stack，可服务 IPv4；
	// 非 unspecified 的 IPv6 专用（如 ::1）则不可。
	return la.IP.IsUnspecified()
}

// canServeIPv6 判断本地 UDP 绑定能否收发 IPv6。任何 IPv6 字面地址（::、::1、[2606:..]）
// 均可；仅纯 IPv4 字面（0.0.0.0 / 127.0.0.1）不行——其 To4 非 nil 且非 unspecified-v6。
func canServeIPv6(la *net.UDPAddr) bool {
	return la.IP.To4() == nil && la.IP.To16() != nil
}

// playSocks5AssociateRelay 模拟 SOCKS5 代理完成一次 UDP ASSOCIATE 握手。
//
// 字节序列严格对应 wzshiming client.go 在无认证时的写法（NewDialer 设 IsResolve=true，
// 但 edge 为 IP 字面量故 do 内不解析；Username 为空走单方法 noAuth）：
//
//	client → proxy: greeting    [0x05, nmethods=0x01, noAuth=0x00]            (3 字节)
//	proxy → client: greet 应答  [0x05, selected=0x00(noAuth)]                 (2 字节)
//	client → proxy: ASSOCIATE  [0x05, cmd=0x03, rsv=0x00,
//	                            ATYP=0x01, 0.0.0.0, port=0x0000]               (10 字节)
//	proxy → client: ASOC 应答   [0x05, success=0x00, rsv=0x00,
//	                            ATYP=0x01, bnd.IP, bnd.port]                    (10 字节)
//
// 握手完成后阻塞读 conn 直到对端关闭——模拟控制连接在 UDP ASSOCIATE 生命周期内常开
// （client.go:196 的监控 goroutine 即基于此连接存活）。返回 nil 表示字节序列吻合。
func playSocks5AssociateRelay(conn net.Conn, bndAddr *net.UDPAddr) error {
	var greeting [3]byte
	if _, err := io.ReadFull(conn, greeting[:]); err != nil {
		return fmt.Errorf("读 greeting 失败：%w", err)
	}
	if greeting[0] != 0x05 || greeting[1] != 1 || greeting[2] != 0x00 {
		return fmt.Errorf("greeting 不符：[% x]", greeting[:])
	}

	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return fmt.Errorf("写 greeting 应答失败：%w", err)
	}

	var req [10]byte
	if _, err := io.ReadFull(conn, req[:]); err != nil {
		return fmt.Errorf("读 ASSOCIATE 请求失败：%w", err)
	}
	if req[0] != 0x05 || req[1] != 0x03 || req[2] != 0x00 || req[3] != 0x01 {
		return fmt.Errorf("ASSOCIATE 请求头不符：[% x]", req[:4])
	}

	bndV4 := bndAddr.IP.To4()
	if bndV4 == nil {
		return fmt.Errorf("BND 地址不是 IPv4：%v", bndAddr)
	}
	reply := []byte{0x05, 0x00, 0x00, 0x01}
	reply = append(reply, bndV4...)
	reply = binary.BigEndian.AppendUint16(reply, uint16(bndAddr.Port))
	if _, err := conn.Write(reply); err != nil {
		return fmt.Errorf("写 ASSOCIATE 应答失败：%w", err)
	}

	// 阻塞读直到对端关闭——模拟控制连接常开期的存活心跳。
	if _, err := io.Copy(io.Discard, conn); err != nil {
		// 对端关闭返回 EOF（转为 nil）或 ErrClosedPipe，均属正常退出，不计错。
		if !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, net.ErrClosed) {
			return fmt.Errorf("阻塞读异常：%w", err)
		}
	}
	return nil
}
