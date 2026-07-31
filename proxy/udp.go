package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"sync"
	"time"
)

// SOCKS5 UDP ASSOCIATE (RFC 1928 §7).
//
// Datagrams handled here do NOT go through the WARP tunnel. H3 Proxy mode is
// built on plain CONNECT, which is a byte-stream tunnel and cannot carry
// datagrams; WARP itself only proxies UDP in its Connect-IP modes, which need a
// TUN device. warp-svc's own forward proxy is TCP-only for the same reason — its
// `proxy::ConnectProxy` trait has exactly one implementation, `ConnectTcpProxy`,
// and the binary contains no CONNECT-UDP (RFC 9298) code at all.
//
// So UDP here is relayed straight out of the host's own stack: TCP egresses
// through WARP, UDP egresses locally. Callers who need every packet inside the
// tunnel must disable this and use a Connect-IP client instead.

const (
	// udpIdleTimeout tears down an association that has seen no traffic. Rolling
	// read deadlines on both sockets enforce it.
	udpIdleTimeout = 60 * time.Second

	// udpMaxDatagram is the largest datagram we will read. 65507 is the maximum
	// UDP payload over IPv4, so no legitimate datagram is truncated.
	udpMaxDatagram = 65507

	// udpResolveTimeout bounds name resolution for a domain-form destination.
	udpResolveTimeout = 5 * time.Second
)

// udpBufPool avoids a 64 KiB allocation per datagram. Heavy UDP users (QUIC,
// DHT) would otherwise drive heap growth proportional to packet rate.
var udpBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, udpMaxDatagram)
		return &b
	},
}

// handleUDPAssociate services a SOCKS5 UDP ASSOCIATE request. It binds a relay
// socket, tells the client where to send, and relays until the client closes the
// TCP control connection, the server shuts down, or the association goes idle.
//
// conn is the TCP control connection and stays open for the lifetime of the
// association; per RFC 1928 its closure terminates the association.
func (s *Server) handleUDPAssociate(conn net.Conn) {
	tcpLocal, ok := conn.LocalAddr().(*net.TCPAddr)
	if !ok {
		sendErr(conn, 0x01)
		return
	}

	// Bind the client-facing socket on the same IP the client already reached us
	// on, so the address we advertise is guaranteed routable for them.
	clientConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: tcpLocal.IP, Port: 0})
	if err != nil {
		log.Printf("UDP ASSOCIATE：绑定面向客户端的 socket 失败：%v", err)
		sendErr(conn, 0x01)
		return
	}
	defer clientConn.Close()

	// Separate socket for talking to targets. Keeping the two directions on
	// different sockets means client traffic and target replies never have to be
	// told apart by source address.
	remoteConn, err := net.ListenUDP("udp", &net.UDPAddr{})
	if err != nil {
		log.Printf("UDP ASSOCIATE：绑定中继 socket 失败：%v", err)
		sendErr(conn, 0x01)
		return
	}
	defer remoteConn.Close()

	bound, ok := clientConn.LocalAddr().(*net.UDPAddr)
	if !ok {
		sendErr(conn, 0x01)
		return
	}
	if err := sendBound(conn, bound.IP, bound.Port); err != nil {
		return
	}

	log.Printf("SOCKS5 UDP ASSOCIATE 中继于 %s（直连，不经过 WARP）", bound)

	a := &udpAssociation{client: clientConn, remote: remoteConn}

	// Closing both sockets is what unblocks the relay goroutines, so every exit
	// path — client hangup, shutdown, idle — funnels through here.
	stop := make(chan struct{})
	var stopOnce sync.Once
	shutdown := func() {
		stopOnce.Do(func() {
			close(stop)
			clientConn.Close()
			remoteConn.Close()
		})
	}

	go func() {
		select {
		case <-s.ctx.Done():
		case <-stop:
		}
		shutdown()
	}()

	// RFC 1928: the association lives exactly as long as the TCP control
	// connection. Draining it also detects the client going away.
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := conn.Read(buf); err != nil {
				shutdown()
				return
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); defer shutdown(); a.clientToRemote() }()
	go func() { defer wg.Done(); defer shutdown(); a.remoteToClient() }()
	wg.Wait()

	log.Printf("SOCKS5 UDP ASSOCIATE 已关闭（%s）", bound)
}

// udpAssociation relays between one SOCKS5 client and arbitrary UDP targets.
type udpAssociation struct {
	client *net.UDPConn
	remote *net.UDPConn

	// mu guards clientAddr, which is learned from the first datagram: the
	// ASSOCIATE request usually carries 0.0.0.0:0 because the client does not yet
	// know which source address it will use.
	mu         sync.Mutex
	clientAddr *net.UDPAddr
}

func (a *udpAssociation) peer() *net.UDPAddr {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clientAddr
}

// setPeer records the client's source address on first contact. Later datagrams
// from a different address are refused, so an off-path sender cannot hijack the
// association by guessing the relay port.
func (a *udpAssociation) setPeer(addr *net.UDPAddr) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.clientAddr == nil {
		a.clientAddr = addr
		return true
	}
	return a.clientAddr.IP.Equal(addr.IP) && a.clientAddr.Port == addr.Port
}

func (a *udpAssociation) clientToRemote() {
	bufp := udpBufPool.Get().(*[]byte)
	defer udpBufPool.Put(bufp)
	buf := *bufp

	for {
		if err := a.client.SetReadDeadline(time.Now().Add(udpIdleTimeout)); err != nil {
			return
		}
		n, src, err := a.client.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if !a.setPeer(src) {
			continue // not our client; ignore
		}

		dst, payload, err := parseUDPRequest(buf[:n])
		if err != nil {
			continue // malformed or fragmented — RFC 1928 says drop
		}

		target, err := resolveUDPTarget(dst)
		if err != nil {
			log.Printf("UDP 中继出错：%v", err)
			continue
		}
		if _, err := a.remote.WriteToUDP(payload, target); err != nil {
			// A single unreachable target must not kill the association.
			continue
		}
	}
}

func (a *udpAssociation) remoteToClient() {
	bufp := udpBufPool.Get().(*[]byte)
	defer udpBufPool.Put(bufp)
	buf := *bufp

	// Reply frames get their own buffer: the header is prepended to the payload,
	// so it cannot be built in place in the read buffer.
	framep := udpBufPool.Get().(*[]byte)
	defer udpBufPool.Put(framep)

	for {
		if err := a.remote.SetReadDeadline(time.Now().Add(udpIdleTimeout)); err != nil {
			return
		}
		n, src, err := a.remote.ReadFromUDP(buf)
		if err != nil {
			return
		}
		peer := a.peer()
		if peer == nil {
			continue // nothing has been sent yet, so this reply is unsolicited
		}

		frame, err := appendUDPReply((*framep)[:0], src, buf[:n])
		if err != nil {
			continue
		}
		if _, err := a.client.WriteToUDP(frame, peer); err != nil {
			return
		}
	}
}

// parseUDPRequest decapsulates a SOCKS5 UDP request:
//
//	+-----+------+------+----------+----------+----------+
//	| RSV | FRAG | ATYP | DST.ADDR | DST.PORT |   DATA   |
//	+-----+------+------+----------+----------+----------+
//	|  2  |  1   |  1   | Variable |    2     | Variable |
//
// payload aliases buf, so it must be consumed before buf is reused.
func parseUDPRequest(buf []byte) (dst string, payload []byte, err error) {
	if len(buf) < 4 {
		return "", nil, fmt.Errorf("数据报过短（%d 字节）", len(buf))
	}
	// RSV is ignored: some clients leave it unset. FRAG is not — fragmentation is
	// optional in RFC 1928 and universally unimplemented, so reassembly is refused
	// rather than silently mishandled.
	if buf[2] != 0x00 {
		return "", nil, fmt.Errorf("不支持分片数据报（FRAG=%d）", buf[2])
	}

	var host string
	off := 4
	switch buf[3] {
	case 0x01: // IPv4
		if len(buf) < off+4+2 {
			return "", nil, fmt.Errorf("IPv4 头部被截断")
		}
		host = net.IP(buf[off : off+4]).String()
		off += 4
	case 0x03: // domain name
		if len(buf) < off+1 {
			return "", nil, fmt.Errorf("域名长度字段被截断")
		}
		l := int(buf[off])
		off++
		if l == 0 {
			return "", nil, fmt.Errorf("域名为空")
		}
		if len(buf) < off+l+2 {
			return "", nil, fmt.Errorf("域名被截断")
		}
		host = string(buf[off : off+l])
		off += l
	case 0x04: // IPv6
		if len(buf) < off+16+2 {
			return "", nil, fmt.Errorf("IPv6 头部被截断")
		}
		host = net.IP(buf[off : off+16]).String()
		off += 16
	default:
		return "", nil, fmt.Errorf("未知的地址类型 %d", buf[3])
	}

	port := binary.BigEndian.Uint16(buf[off : off+2])
	off += 2
	if port == 0 {
		return "", nil, fmt.Errorf("目标端口为 0")
	}
	return net.JoinHostPort(host, fmt.Sprint(port)), buf[off:], nil
}

// appendUDPReply appends a SOCKS5 UDP reply frame for payload received from src.
// The wire format is the same as a request, with src in the address fields.
func appendUDPReply(dst []byte, src *net.UDPAddr, payload []byte) ([]byte, error) {
	dst = append(dst, 0x00, 0x00, 0x00) // RSV, FRAG

	if v4 := src.IP.To4(); v4 != nil {
		dst = append(dst, 0x01)
		dst = append(dst, v4...)
	} else if v6 := src.IP.To16(); v6 != nil {
		dst = append(dst, 0x04)
		dst = append(dst, v6...)
	} else {
		return nil, fmt.Errorf("回包源地址 %v 不是可用地址", src.IP)
	}

	dst = binary.BigEndian.AppendUint16(dst, uint16(src.Port))
	return append(dst, payload...), nil
}

// resolveUDPTarget turns a SOCKS5 destination into a UDP address. Names are
// resolved with the host resolver, not the tunnel's DoH client: these datagrams
// egress locally, so resolving them through WARP would pair a WARP-chosen address
// with a non-WARP source.
func resolveUDPTarget(dst string) (*net.UDPAddr, error) {
	host, port, err := net.SplitHostPort(dst)
	if err != nil {
		return nil, fmt.Errorf("非法的 UDP 目标 %q：%w", dst, err)
	}
	if ip := net.ParseIP(host); ip != nil {
		addr, err := net.ResolveUDPAddr("udp", dst)
		if err != nil {
			return nil, fmt.Errorf("解析 %q 失败：%w", dst, err)
		}
		return addr, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), udpResolveTimeout)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("在本机解析 %q 失败：%w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("%q 没有解析出地址", host)
	}
	p, err := net.LookupPort("udp", port)
	if err != nil {
		return nil, fmt.Errorf("非法端口 %q：%w", port, err)
	}
	return &net.UDPAddr{IP: ips[0].IP, Port: p, Zone: ips[0].Zone}, nil
}

// sendBound 发送 UDP ASSOCIATE 的绑定响应，携带真实 BND.ADDR/BND.PORT：客户端
// 把数据报发往这个确切地址，CONNECT 用的全零回复会让它无处可发。
func sendBound(conn net.Conn, ip net.IP, port int) error {
	reply := []byte{0x05, 0x00, 0x00}
	if v4 := ip.To4(); v4 != nil {
		reply = append(reply, 0x01)
		reply = append(reply, v4...)
	} else if v6 := ip.To16(); v6 != nil {
		reply = append(reply, 0x04)
		reply = append(reply, v6...)
	} else {
		return fmt.Errorf("绑定地址 %v 不可用", ip)
	}
	reply = binary.BigEndian.AppendUint16(reply, uint16(port))
	_, err := conn.Write(reply)
	return err
}
