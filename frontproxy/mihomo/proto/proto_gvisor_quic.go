// PROTOTYPE — throwaway. Answers one question:
//   Can a gVisor *gonet.UDPConn (from sing-wireguard StackDevice) drive
//   quic-go's listen goroutine to complete a QUIC handshake + one echo RTT?
// This is the gate for topology B-1: routing warp-go's WARP QUIC/UDP through
// an OpenVPN node's gVisor netstack so WARP edge sees the node country as src.
//
// OpenVPN is NOT used here. We stand up a bare sing-wireguard StackDevice
// (same gVisor netstack OpenVPN uses internally) and a mini-router goroutine
// that bridges StackDevice.Read/Write to a real UDP socket bound to a local
// quic-go echo listener. If the client — its QUIC conn riding the gonet
// UDPConn inside the netstack — completes the handshake and exchanges one
// datagram with the listener, the gate is GREEN.
//
// Run: go run -tags with_gvisor ./frontproxy/mihomo/proto

//go:build with_gvisor
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/netip"
	"os"
	"sync/atomic"
	"time"

	"github.com/metacubex/sing/common/metadata"
	wgTun "github.com/metacubex/wireguard-go/tun"
	"github.com/quic-go/quic-go"
	wg "github.com/metacubex/sing-wireguard"
)

func main() {
	verdict := "pending"
	defer func() {
		fmt.Printf("\n=== PROTOTYPE VERDICT: %s ===\n", verdict)
	}()

	timeout := flag.Duration("timeout", 15*time.Second, "overall handshake+echo timeout")
	flag.Parse()

	// ---- 1. real quic-go echo listener on a dedicated UDP socket ----
	listenConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		verdict = "RED: listen udp: " + err.Error()
		return
	}
	defer listenConn.Close()
	listenAddr := listenConn.LocalAddr().(*net.UDPAddr)
	log.Printf("[listener] real udp on %v", listenAddr)

	// Separate outbound socket: the netstack's "wire" — its src port becomes
	// the dst port the listener replies to, and only miniRouter reads it.
	// Sharing one socket between quic.Listen and our router would make two
	// goroutines race-read the same fd and steal each other's datagrams.
	obConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		verdict = "RED: outbound udp: " + err.Error()
		return
	}
	defer obConn.Close()
	obAddr := obConn.LocalAddr().(*net.UDPAddr)

	tlsCfg := mustSelfSignedTLS()
	ln, err := quic.Listen(listenConn, tlsCfg, &quic.Config{EnableDatagrams: true})
	if err != nil {
		verdict = "RED: quic listen: " + err.Error()
		return
	}
	defer ln.Close()

	accepted := make(chan *quic.Conn, 1)
	go func() {
		c, err := ln.Accept(context.Background())
		if err != nil {
			return
		}
		accepted <- c
	}()

	// ---- 2. bare gVisor netstack (OpenVPN's underlay) ----
	stackLocal := netip.MustParsePrefix("172.31.0.2/24")
	dev, err := wg.NewStackDevice([]netip.Prefix{stackLocal}, 1420)
	if err != nil {
		verdict = "RED: new stack device: " + err.Error()
		return
	}
	defer dev.Close()
	dev.Start() // lifts the NIC up so it has a dispatcher

	// mini-router: netstack ⇄ real socket.
	routerStop := make(chan struct{})
	routerDone := make(chan struct{})
	go miniRouter(dev, obConn, stackLocal.Addr(), obAddr, listenAddr, routerStop, routerDone)
	defer func() { close(routerStop); <-routerDone }()

	// ---- 3. quic-go CLIENT whose Conn is a gonet UDPConn from the netstack ----
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// StackDevice.ListenPacket returns a gonet.UDPConn (net.PacketConn).
	dst := metadata.SocksaddrFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(listenAddr.Port))
	clientPc, err := dev.ListenPacket(ctx, dst.Unwrap())
	if err != nil {
		verdict = "RED: stackd ListenPacket: " + err.Error()
		return
	}
	defer clientPc.Close()
	log.Printf("[client] netstack packetConn type=%T local=%v remote=%v", clientPc, clientPc.LocalAddr())

	tr := &quic.Transport{Conn: clientPc, ConnectionIDLength: 20}
	defer tr.Close()

	cliTlsCfg := tlsCfg.Clone() // server cert is self-signed; treat it as a CA for the client pool
	cliTlsCfg.RootCAs = x509.NewCertPool()
	cliTlsCfg.RootCAs.AddCert(tlsCfg.Certificates[0].Leaf)

	cli, err := tr.Dial(ctx, listenAddr, cliTlsCfg, &quic.Config{
		EnableDatagrams:      true,
		HandshakeIdleTimeout: *timeout,
	})
	if err != nil {
		verdict = "RED: quic Dial (handshake): " + err.Error()
		return
	}
	defer cli.CloseWithError(quic.ApplicationErrorCode(0), "bye")
	log.Printf("[client] handshake done: %v", cli.RemoteAddr())

	// ---- 4. one echo round trip via 1-RTT stream ----
	echErr := make(chan error, 1)
	go func() {
		var c *quic.Conn
		select {
		case c = <-accepted:
		case <-time.After(*timeout):
			echErr <- fmt.Errorf("server accept timeout")
			return
		}
		str, err := c.AcceptStream(ctx)
		if err != nil {
			echErr <- fmt.Errorf("server accept stream: %w", err)
			return
		}
		defer str.Close()
		buf := make([]byte, 8)
		if _, err := io.ReadFull(str, buf); err != nil {
			echErr <- fmt.Errorf("server read: %w", err)
			return
		}
		if _, err := str.Write(buf); err != nil { // echo back
			echErr <- fmt.Errorf("server write: %w", err)
			return
		}
		// Hold the conn open so the FIN we just sent actually flushes before a
		// CONNECTION_CLOSE aborts it. The client cancel()s ctx after reading.
		<-ctx.Done()
		_ = c.CloseWithError(quic.ApplicationErrorCode(0), "bye")
	}()

	str, err := cli.OpenStreamSync(ctx)
	if err != nil {
		verdict = "RED: open stream: " + err.Error()
		return
	}
	defer str.Close()

	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, uint64(time.Now().UnixNano()))
	if _, err := str.Write(payload); err != nil {
		verdict = "RED: write: " + err.Error()
		return
	}
	echo := make([]byte, 8)
	if _, err := io.ReadFull(str, echo); err != nil {
		verdict = "RED: read echo: " + err.Error()
		return
	}
	if binary.BigEndian.Uint64(echo) != binary.BigEndian.Uint64(payload) {
		verdict = fmt.Sprintf("RED: echo mismatch sent=%x got=%x", payload, echo)
		return
	}
	verdict = fmt.Sprintf("GREEN: gVisor netstack gonet.UDPConn drove a quic-go handshake + one echo RTT (echo=%x)", echo)
}

// miniRouter bridges the netstack to a loopback socket pair.
//   outConn  — the netstack's "wire". Outbound packets leave here toward
//              listenAddr; listener replies land back here (only router reads it,
//              the quic listener owns its own socket).
//   listenAddr — the quic listener address. Outbound dst; inbound src.
//   stackAddr — the netstack host address (172.31.0.2). Inbound dst IP.
// The netstack client picks its own ephemeral source port (gonet.DialUDP).
// We capture it from the first outbound IPv4/UDP packet and reuse it as the
// dst port on inbound rewrites, so packets route back to the client's gonet
// endpoint inside the netstack.
// Throwaway; minimal correctness only.
func miniRouter(dev wgTun.Device, outConn *net.UDPConn, stackAddr netip.Addr, outAddr, listenAddr *net.UDPAddr, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	const ipHdr, udpHdr = 20, 8
	var clientPort atomic.Uint32 // captured netstack-side source port

	// outbound: netstack → listener (via outConn)
	go func() {
		bufs := [][]byte{make([]byte, 1500)}
		sizes := make([]int, 1)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := dev.Read(bufs, sizes, 0)
			if err != nil {
				if errors.Is(err, os.ErrClosed) {
					return
				}
				continue
			}
			if n == 0 || sizes[0] == 0 {
				continue
			}
			pkt := bufs[0][:sizes[0]]
			if len(pkt) < ipHdr+udpHdr || pkt[0]>>4 != 4 {
				continue
			}
			srcPort := binary.BigEndian.Uint16(pkt[ipHdr+0 : ipHdr+2])
			if srcPort != 0 {
				clientPort.Store(uint32(srcPort))
			}
			udpPayload := pkt[ipHdr+udpHdr:]
			_, _ = outConn.WriteToUDP(udpPayload, listenAddr)
		}
	}()

	// inbound: listener replies (landing on outConn) → netstack
	inbuf := make([]byte, 1500)
	for {
		select {
		case <-stop:
			return
		default:
		}
		_ = outConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, _, err := outConn.ReadFromUDP(inbuf[ipHdr+udpHdr:])
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			continue
		}
		cp := clientPort.Load()
		if cp == 0 {
			continue // haven't seen an outbound packet yet; nowhere to deliver
		}
		total := ipHdr + udpHdr + n
		out := make([]byte, total)
		copy(out[ipHdr+udpHdr:], inbuf[ipHdr+udpHdr:ipHdr+udpHdr+n])
		out[0] = 0x45 // IPv4, IHL 5
		binary.BigEndian.PutUint16(out[2:4], uint16(total))
		out[6] = 0x40 // DF
		out[8] = 64   // TTL
		out[9] = 17   // UDP
		copy(out[12:16], net.IPv4(127, 0, 0, 1)) // src = listener IP
		copy(out[16:20], stackAddr.AsSlice())    // dst = netstack host
		var csum uint32
		for i := 0; i < 20; i += 2 {
			csum += uint32(binary.BigEndian.Uint16(out[i : i+2]))
		}
		for csum>>16 != 0 {
			csum = (csum & 0xffff) + (csum >> 16)
		}
		binary.BigEndian.PutUint16(out[10:12], ^uint16(csum))
		binary.BigEndian.PutUint16(out[ipHdr+0:ipHdr+2], uint16(listenAddr.Port)) // src port = listener
		binary.BigEndian.PutUint16(out[ipHdr+2:ipHdr+4], uint16(cp))             // dst port = netstack client src
		binary.BigEndian.PutUint16(out[ipHdr+4:ipHdr+6], uint16(n+udpHdr))        // length
		binary.BigEndian.PutUint16(out[ipHdr+6:ipHdr+8], 0)                       // csum (optional)
		_, _ = dev.Write([][]byte{out}, 0)
	}
}

func mustSelfSignedTLS() *tls.Config {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "proto"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: mustParseCert(der)}
	return &tls.Config{Certificates: []tls.Certificate{cert}}
}

func mustParseCert(der []byte) *x509.Certificate {
	c, _ := x509.ParseCertificate(der)
	return c
}
