package tunnel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// These tests cover the B-1 injection sub-seam: a controlled net.PacketConn
// returned by a PacketResolver is the conn quic-go's Transport actually dials
// through, and the edge target the dial reaches is the one the resolver's path
// points at. They live in the tunnel package (the seam's home) and use a real
// loopback QUIC listener as the mock WARP edge — no mihomo, no frontproxy.
//
// The seam contract tested here is decision-dense points #5 and #7 from the
// prototype:
//
//   #5  quic.Transport.Dial takes the injected Conn as its Conn and the edge
//       *net.UDPAddr as its 2nd argument; the conn the resolver returned is the
//       one that drove the handshake (asserted by LocalAddr match).
//   #7  wrapConn accepts any net.PacketConn — not only OOBCapablePacketConn —
//       and a buffer-tuning failure degrades to basicConn with a warning, not
//       an abort. We inject both a plain *net.UDPConn (OOB-capable, happy path)
//       AND an OOB-incapable wrapper (only net.PacketConn surface) and assert
//       both reach the mock edge.
//
// The cert carries IP SAN 127.0.0.1/::1 (decision-dense point #4); the client
// uses InsecureSkipVerify exactly like the production warp-go dial does, since
// the seam under test is the connection path, not edge authenticity.

// loopbackTLS builds a self-signed cert good for 127.0.0.1/::1 and a TLS config
// usable by both a quic.Listen server and a transport.Dial client.
func loopbackTLS(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "warp-edge-mock"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	srv := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h3"},
		MinVersion:   tls.VersionTLS13,
	}
	// Production warp-go dials with InsecureSkipVerify: true (edge auth is by a
	// pinned endpoint public key, not chain — see main.go's MASQUE tlsConfig).
	// The seam test only needs the QUIC handshake to complete so the path is
	// observable; it does not exercise edge authenticity, so it follows the
	// same skip-verify approach the production dial does.
	client := &tls.Config{
		ServerName:         "127.0.0.1",
		NextProtos:         []string{"h3"},
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
	}
	return srv, client
}

// mockEdge is a loopback QUIC echo for one accepted stream. It is the stand-in
// for the WARP edge in the sub-seam test; its address is the one the injected
// dial must reach.
type mockEdge struct {
	ln        *quic.Listener
	conn      net.PacketConn
	accepted  chan *quic.Stream
	serveDone chan struct{}
}

func newMockEdge(t *testing.T) *mockEdge {
	t.Helper()
	udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("edge listen: %v", err)
	}
	srvTLS, _ := loopbackTLS(t)
	ln, err := quic.Listen(udpConn, srvTLS, &quic.Config{
		EnableDatagrams:      true,
		HandshakeIdleTimeout: 5 * time.Second,
		MaxIdleTimeout:       10 * time.Second,
	})
	if err != nil {
		udpConn.Close()
		t.Fatalf("quic listen: %v", err)
	}
	m := &mockEdge{
		ln:        ln,
		conn:      udpConn,
		accepted:  make(chan *quic.Stream, 1),
		serveDone: make(chan struct{}),
	}
	go m.serve()
	return m
}

func (m *mockEdge) serve() {
	defer close(m.serveDone)
	c, err := m.ln.Accept(context.Background())
	if err != nil {
		return
	}
	str, err := c.AcceptStream(context.Background())
	if err != nil {
		return
	}
	m.accepted <- str
}

func (m *mockEdge) addr() string { return m.conn.LocalAddr().String() }

func (m *mockEdge) close() {
	_ = m.ln.Close()
	_ = m.conn.Close()
	<-m.serveDone
}

// echoStream relays 8 bytes str→str and signals done.
func echoStream(t *testing.T, str *quic.Stream) chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 8)
		if _, err := io.ReadFull(str, buf); err != nil {
			return
		}
		_, _ = str.Write(buf)
	}()
	return done
}

// injectedConn is an OOB-incapable wrapper around a *net.UDPConn that surfaces
// only the net.PacketConn interface. quic-go's wrapConn degrades it to
// basicConn (decision-dense point #7): the handshake still completes and the
// buffer-tuning failure is a warning, not an abort.
type injectedConn struct {
	inner *net.UDPConn
}

func (c *injectedConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.inner.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer c.inner.SetReadDeadline(time.Time{})
	return c.inner.ReadFrom(p)
}
func (c *injectedConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	return c.inner.WriteTo(p, addr)
}
func (c *injectedConn) Close() error                  { return c.inner.Close() }
func (c *injectedConn) LocalAddr() net.Addr           { return c.inner.LocalAddr() }
func (c *injectedConn) RemoteAddr() net.Addr          { return c.inner.RemoteAddr() }
func (c *injectedConn) SetDeadline(t time.Time) error { return c.inner.SetDeadline(t) }
func (c *injectedConn) SetReadDeadline(t time.Time) error {
	return c.inner.SetReadDeadline(t)
}
func (c *injectedConn) SetWriteDeadline(t time.Time) error {
	return c.inner.SetWriteDeadline(t)
}

// dialThroughResolver is the shared seam-exercise body. It:
//  1. asks the MasqueClient's resolver seam (obtainUnderlayConn) for the conn
//     and asserts the resolver was consulted and returned our injected socket
//     (its LocalAddr is the conn handed to quic.Transport — proven below);
//  2. builds a quic.Transport on the returned conn, exactly as dialAddr does,
//     and Dials the mock edge (decision-dense point #5: *net.UDPAddr 2nd arg);
//  3. asserts the established QUIC conn's LocalAddr equals the injected conn's
//     LocalAddr — quic-go's Transport uses Conn.LocalAddr, so a match means the
//     resolver's conn drove the handshake, NOT a fresh ListenUDP (the fallback);
//  4. opens a stream, sends 8 bytes, and asserts the mock edge echoes them back,
//     proving the QUIC conn carries real bidirectional data through the path.
//
// injectPlain=false wraps the socket in injectedConn (OOB-incapable) to assert
// decision-dense point #7. The body is the same; only the resolver's return
// type NotAtTheFront changes.
func dialThroughResolver(t *testing.T, injectWrapped bool) {
	t.Helper()
	edge := newMockEdge(t)
	defer edge.close()
	edgeAddr := edge.addr()

	injectedUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("injected listen: %v", err)
	}
	defer injectedUDP.Close()
	wantLocal := injectedUDP.LocalAddr().String()

	resolverCalled := false
	var returnedAddr string
	c := &MasqueClient{
		edgeAddrs: []string{edgeAddr},
		// quicConfig matches production's essentials but with a short idle so a
		// stalled dial surfaces fast in the test.
		quicConfig: &quic.Config{
			HandshakeIdleTimeout: 5 * time.Second,
			MaxIdleTimeout:       10 * time.Second,
			EnableDatagrams:      true,
			InitialPacketSize:    1350,
			// Keep the peer-imposed stream grant non-zero so OpenStreamSync does
			// not block on a 0 MAX_STREAMS — the default 100 is fine but we name
			// the value for the test so a default change across quic-go cannot
			// silently deadlock this seam test.
			MaxIncomingStreams:    100,
			MaxIncomingUniStreams: 100,
		},
		token: "seam-test",
		resolver: func(addr string) (net.PacketConn, error) {
			resolverCalled = true
			if addr != edgeAddr {
				return nil, fmt.Errorf("unmocked edge %q, want %q", addr, edgeAddr)
			}
			returnedAddr = addr
			if injectWrapped {
				return &injectedConn{inner: injectedUDP}, nil
			}
			return injectedUDP, nil
		},
		dnsCache:  make(map[string]dnsCacheEntry),
		dnsFlight: make(map[string]*dnsFlightResult),
	}
	_, clientTLS := loopbackTLS(t)
	c.tlsConfig = clientTLS

	// 1. Exercise the resolver seam via the package's own obtainUnderlayConn —
	//    the exact branch dialAddr uses. resolverCalled asserting here means
	//    take-resolver (not fallback) ran.
	udpAddr, err := net.ResolveUDPAddr("udp", edgeAddr)
	if err != nil {
		t.Fatalf("resolve edge: %v", err)
	}
	conn, err := c.obtainUnderlayConn(edgeAddr, udpAddr)
	if err != nil {
		t.Fatalf("obtainUnderlayConn: %v", err)
	}
	defer conn.Close()
	if !resolverCalled {
		t.Fatal("PacketResolver never consulted — seam not wired")
	}
	if returnedAddr != edgeAddr {
		t.Fatalf("resolver called with %q, want %q", returnedAddr, edgeAddr)
	}
	if conn.LocalAddr().String() != wantLocal {
		t.Fatalf("obtainUnderlayConn returned a different conn: local=%q want(injected)=%q",
			conn.LocalAddr().String(), wantLocal)
	}

	// 2. Build a quic.Transport on the returned conn (the same shape as dialAddr
	//    does), then Dial the mock edge. decision-dense point #5: Dial's 2nd arg
	//    is the *net.UDPAddr of the edge, not a netip.AddrPort.
	qtr := &quic.Transport{Conn: conn, ConnectionIDLength: connectionIDLength}
	defer qtr.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	quicConn, err := qtr.Dial(ctx, udpAddr, c.tlsConfig.Clone(), c.quicConfig)
	if err != nil {
		t.Fatalf("quic.Transport.Dial through injected conn: %v", err)
	}
	defer quicConn.CloseWithError(0, "test")

	// 3. The decision-dense observable: quic-go's Transport used Conn.LocalAddr,
	//    so the QUIC conn's LocalAddr equals the injected conn's LocalAddr. A
	//    mismatch would mean the Transport ignored the resolver's conn and used a
	//    fresh socket (the fallback) — exactly the seam we assert is wired.
	gotLocal := quicConn.LocalAddr().String()
	if gotLocal != wantLocal {
		t.Fatalf("quic.Transport did not dial through injected conn: local=%q want(injected)=%q",
			gotLocal, wantLocal)
	}

	// 4. One stream + echo: prove the QUIC conn (riding the injected conn through
	//    the seam) carries real data, end-to-end through the mock edge.
	//
	//    Order matters and is non-obvious: quic-go's AcceptStream only returns on
	//    the server once the client has sent a frame on the stream (OpenStreamSync
	//    alone doesn't signal the peer — see the package doc for OpenStreamSync).
	//    So we open, write the payload, and only then wait for the edge to accept
	//    and echo back.
	cliStr, err := quicConn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("open client stream: %v", err)
	}
	defer cliStr.Close()

	payload := []byte("seampo46")
	if _, err := cliStr.Write(payload); err != nil {
		t.Fatalf("client write: %v", err)
	}

	select {
	case str := <-edge.accepted:
		defer str.Close()
		edgedDone := echoStream(t, str)

		echo := make([]byte, 8)
		if _, err := io.ReadFull(cliStr, echo); err != nil {
			t.Fatalf("client read echo: %v", err)
		}
		if string(echo) != string(payload) {
			t.Fatalf("echo mismatch: got %q want %q", echo, payload)
		}
		select {
		case <-edgedDone:
		case <-time.After(2 * time.Second):
			t.Fatal("edge echo never completed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("edge never accepted the injected-path stream")
	}
}

// TestResolverInject_DialsThroughInjectedConn is the injection sub-seam
// assertion with an OOB-capable *net.UDPConn (the happy path). See
// dialThroughResolver for the contract.
func TestResolverInject_DialsThroughInjectedConn(t *testing.T) {
	dialThroughResolver(t, false)
}

// TestResolverInject_OOBIncapableAlsoHandshakes asserts the tolerance half of
// decision-dense point #7: an OOB-incapable net.PacketConn (the injectedConn
// wrapper, only exposing net.PacketConn) still drives a complete handshake and
// bidirectional echo through the seam. quic-go's wrapConn degrades it to
// basicConn; the buffer-tuning failure is a warning, not an abort.
func TestResolverInject_OOBIncapableAlsoHandshakes(t *testing.T) {
	dialThroughResolver(t, true)
}

// keep errors/io reachable when dialect shifts; harmless no-op refs.
var _ = errors.Is
