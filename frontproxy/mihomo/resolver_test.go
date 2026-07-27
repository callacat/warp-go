package mihomo

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// resolver_test.go covers the B-1 anti-corruption layer's seam contract:
// OvpnEdgeResolver is the surface warp-go's quic transport dials through, and
// the adapter binds it to tunnel.PacketResolver's function-type contract —
// without importing tunnel and without any real mihomo compiled in.
//
// These tests deliberately do NOT instantiate *gonet.UDPConn or a real mihomo
// engine. The spec (#1, Testing Decisions, "什么算好测") is explicit:
// "注入的不是具体的 *gonet.UDPConn，是一个满足 net.PacketConn 的测试替身——
//  证 seam 契约不仿造 mihomo". So we inject a controllable net.PacketConn and
// assert the seam plumbing, exactly mirroring tunnel/resolver_test.go's
// injection sub-seam tests on the other side of the adapter.
//
// Red→green discipline: this file is written first (slice 1) and fails until the
// OvpnEdgeResolver interface, AsPacketResolver adapter, and the nil→fallback
// default are implemented (slice 2).

// stubResolver is a controllable OvpnEdgeResolver test double. It records the
// edgeAddr it was asked for and returns whatever conn/err the test wires. nil
// conn + nil err is the "decline, take the fallback" contract from
// tunnel.PacketResolver — the anti-corruption layer mirrors it exactly.
type stubResolver struct {
	mu       sync.Mutex
	asked    string
	conn     net.PacketConn
	err      error
	declined bool // forces the (nil,nil) fallback path
}

func (s *stubResolver) Resolve(edgeAddr string) (net.PacketConn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = edgeAddr
	if s.declined {
		return nil, nil
	}
	return s.conn, s.err
}

func (s *stubResolver) askedFor() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asked
}

// recordingPacketConn is a minimal net.PacketConn that records the first
// outbound datagram's destination. It never completes a real handshake; it
// only proves the adapter's returned conn is the one a caller writes through —
// i.e. the seam does not silently substitute its own socket (prototype
// decision-dense point #1: the injected path is read by exactly one goroutine
// and never aliased to a listener fd).
type recordingPacketConn struct {
	mu       sync.Mutex
	writeDst net.Addr
	writes   int
	closed   bool
}

func (r *recordingPacketConn) WriteTo(p []byte, addr net.Addr) (n int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.writes == 0 {
		r.writeDst = addr
	}
	r.writes++
	return len(p), nil
}
func (r *recordingPacketConn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	// Block forever by default — a test that needs a return sets a deadline.
	select {}
}
func (r *recordingPacketConn) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}
func (r *recordingPacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{Port: 0} }
func (r *recordingPacketConn) SetDeadline(time.Time) error      { return nil }
func (r *recordingPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (r *recordingPacketConn) SetWriteDeadline(time.Time) error { return nil }

func (r *recordingPacketConn) firstWriteDst() net.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.writeDst
}

// TestOvpnEdgeResolver_AdapterThreadsConnThrough asserts AsPacketResolver wires
// an OvpnEdgeResolver to the tunnel.PacketResolver function-type contract:
// calling the returned func consults the resolver, and a non-nil conn flows back
// unchanged. The seam must not invent its own socket (proto decision-dense #1).
// The conn does not need OOB — quic-go degrades to basicConn (proto #7), which
// is why a controllable net.PacketConn is a faithful test double.
func TestOvpnEdgeResolver_AdapterThreadsConnThrough(t *testing.T) {
	conn := &recordingPacketConn{}
	resolver := &stubResolver{conn: conn}

	packetResolver := AsPacketResolver(resolver)

	if packetResolver == nil {
		t.Fatal("AsPacketResolver returned nil — adapter not implemented")
	}

	got, err := packetResolver("162.159.36.2:2408")
	if err != nil {
		t.Fatalf("adapter surfaced unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("adapter dropped the resolver's conn — seam is not transparent")
	}
	if got != net.PacketConn(conn) {
		t.Fatalf("adapter substituted a different conn: got %T want %T", got, conn)
	}
	if asked := resolver.askedFor(); asked != "162.159.36.2:2408" {
		t.Fatalf("resolver asked for %q, want 162.159.36.2:2408", asked)
	}
}

// TestOvpnEdgeResolver_AdapterPropagatesError asserts a non-nil resolver error
// is returned verbatim (wrapped is fine, errors.Is must hold) and no conn is
// synthesized. tunnel.ObtainUnderlayConn relies on this to abort the dial rather
// than silently fall through to direct connect when the anti-corruption layer
// refuses.
func TestOvpnEdgeResolver_AdapterPropagatesError(t *testing.T) {
	sentinel := errors.New("frontproxy refused: no node for edge")
	resolver := &stubResolver{err: sentinel}

	packetResolver := AsPacketResolver(resolver)

	got, err := packetResolver("any:443")
	if err == nil {
		t.Fatal("expected resolver error to propagate, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel to surface (errors.Is), got: %v", err)
	}
	if got != nil {
		t.Fatalf("adapter synthesized a conn on error: %T", got)
	}
}

// TestOvpnEdgeResolver_AdapterDeclineToFallback asserts the (nil,nil) "decline"
// contract: when the resolver returns (nil,nil) the adapter returns (nil,nil)
// too, so the downstream seam (tunnel.obtainUnderlayConn) takes its
// net.ListenUDP fallback. This is the B-1 roll-back anchor — P1-B itself never
// reaches a real node (#4 acceptance: 不接真实 vpngate 节点), so the fallback is
// the only resolution path the default resolver exercises here.
func TestOvpnEdgeResolver_AdapterDeclineToFallback(t *testing.T) {
	resolver := &stubResolver{declined: true}

	packetResolver := AsPacketResolver(resolver)

	got, err := packetResolver("162.159.36.2:2408")
	if err != nil {
		t.Fatalf("decline path returned error, want (nil,nil): %v", err)
	}
	if got != nil {
		t.Fatalf("decline path returned a conn, want nil (so downstream takes ListenUDP): %T", got)
	}
}

// TestDefaultEdgeResolver_DeclinesByDefault asserts the package's default
// resolver (the one warp-go wiring would use when B-1 frontproxy is enabled but
// no real node is configured) declines on every call — the code-layer gate P1-B
// is, never reaching a real vpngate node (#4 acceptance). A real node resolver
// is B-1-PoC-2's job, not this ticket.
func TestDefaultEdgeResolver_DeclinesByDefault(t *testing.T) {
	r := DefaultEdgeResolver()
	if r == nil {
		t.Fatal("DefaultEdgeResolver returned nil")
	}
	for _, edge := range []string{"162.159.36.2:2408", "engage.cloudflareclient.com:443"} {
		got, err := r.Resolve(edge)
		if err != nil {
			t.Fatalf("DefaultEdgeResolver(%q) errored, want (nil,nil): %v", edge, err)
		}
		if got != nil {
			t.Fatalf("DefaultEdgeResolver(%q) returned a conn, want nil: %T", edge, got)
		}
	}
}

// Compile-time assertion: OvpnEdgeResolver is the surface warp-go's quic
// transport depends on. It must return net.PacketConn (not a concrete
// *gonet.UDPConn) so the tunnel package never sees a mihomo internal type
// (#1 user story 12/13). This lives in the test file so it is exercised on
// every `go test`, not just at build.
var _ OvpnEdgeResolver = (*stubResolver)(nil)

// Compile-time assertion that the adapter produces the function type
// tunnel.PacketResolver expects — without importing tunnel. tunnel.PacketResolver
// is `func(edgeAddr string) (net.PacketConn, error)`; AsPacketResolver must
// return a value assignable to that. We assert the signature directly here.
var _ func(string) (net.PacketConn, error) = AsPacketResolver(&stubResolver{})

// Compile-time guard that the test double is a faithful net.PacketConn — the
// seam contract is "any net.PacketConn", OOB or not (proto #7).
var _ net.PacketConn = (*recordingPacketConn)(nil)
