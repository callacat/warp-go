package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// These tests cover the PacketResolver injection seam (ADR-0001): it is the
// roll-back anchor for B-1 and must be observable without mihomo. They live
// entirely inside the tunnel package's own surface — no frontproxy import, no
// mocked mihomo — exactly the "injection sub-seam stays inside the top seam"
// discipline the spec demands.
//
// Two assertions:
//
//  - fallback: a resolver that returns (nil, nil) takes the net.ListenUDP path,
//    so the dial proceeds against a real OS socket and only fails at the QUIC
//    handshake (never at bind). That is the "zero behavior change" contract.
//  - inject: a resolver that returns a controllable net.PacketConn is the conn
//    quic-go's Transport dials through — observed by feeding a PacketConn that
//    records the first outbound datagram, proving the path is taken.
//
// Both use a short handshake idle timeout so nothing here blocks on the network.

// seamQuicConfig returns a quic config tuned for tests: a short handshake idle
// timeout so a silent target fails the dial fast (well under per-address timeout).
func seamQuicConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout: 300 * time.Millisecond,
		EnableDatagrams:      true,
	}
}

// seamTLS is the throwaway TLS config seam tests dial with. dialAddr against a
// silent target never completes a handshake, so InsecureSkipVerify + a stable SNI
// are sufficient; the seam under test runs entirely before the TLS exchange.
func seamTLS() *tls.Config {
	return &tls.Config{
		ServerName:         "engage.cloudflareclient.com",
		NextProtos:         []string{"h3"},
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
	}
}

// seamClient builds a MasqueClient whose constructor dial is bypassed so dialAddr
// can be exercised directly under the test's own PacketResolver. The constructor
// is not used because it dials at construct time against real edges, which these
// tests never reach.
func seamClient(t *testing.T, edgeAddrs []string, resolver PacketResolver) *MasqueClient {
	t.Helper()
	return &MasqueClient{
		edgeAddrs:  append([]string(nil), edgeAddrs...),
		tlsConfig:  seamTLS(),
		quicConfig: seamQuicConfig(),
		token:      "seam-test",
		dnsCache:   make(map[string]dnsCacheEntry),
		dnsFlight:  make(map[string]*dnsFlightResult),
		resolver:   resolver,
	}
}

// TestDialAddr_FallbackResolverNil_TakesListenUDPPath asserts the zero-behavior-
// change contract: a PacketResolver returning (nil, nil) must fall through to
// net.ListenUDP (the current direct path), bind a real OS socket in the edge's
// family, and only then fail at the QUIC handshake against a silent loopback
// target.
//
// Observable signals:
//   - the resolver was consulted (fallback decision happened, path was offered),
//   - the error is a QUIC dial failure (mentions "QUIC 拨号"), NOT a ListenUDP
//     bind failure (which would mention "监听 UDP"). A bind failure would mean
//     the resolver path was taken and ListenUDP was skipped — exactly the drift
//     the fallback branch exists to forbid.
func TestDialAddr_FallbackResolverNil_TakesListenUDPPath(t *testing.T) {
	resolverCalled := false
	resolver := func(edgeAddr string) (net.PacketConn, error) {
		t.Logf("resolver called for %s, returning (nil,nil) = fallback", edgeAddr)
		resolverCalled = true
		return nil, nil
	}

	c := seamClient(t, []string{"127.0.0.1:1"}, resolver)

	ctx, cancel := context.WithTimeout(context.Background(), perAddrDialTimeout+800*time.Millisecond)
	defer cancel()
	_, err := c.dialAddr(ctx, "127.0.0.1:1")
	if err == nil {
		t.Fatal("dialAddr against silent 127.0.0.1:1 unexpectedly succeeded")
	}
	if !resolverCalled {
		t.Fatal("PacketResolver was not consulted — seam not wired")
	}
	if strings.Contains(err.Error(), "监听 UDP") {
		t.Fatalf("fallback path aborted at net.ListenUDP bind (resolver path was taken): %v", err)
	}
	if !strings.Contains(err.Error(), "QUIC 拨号") {
		t.Fatalf("expected failure at the QUIC dial (fallback ran net.ListenUDP then dialed), got: %v", err)
	}
}

// TestDialAddr_ResolverError_AbortsDial asserts a non-nil resolver error
// short-circuits the dial: dialAddr returns that error (wrapped) and never
// reaches net.ListenUDP. The anti-corruption layer relies on this to report a
// refusal instead of silently falling through to direct connect.
func TestDialAddr_ResolverError_AbortsDial(t *testing.T) {
	sentinel := errors.New("anti-corruption layer refused")
	resolver := func(edgeAddr string) (net.PacketConn, error) {
		return nil, sentinel
	}
	c := seamClient(t, []string{"127.0.0.1:1"}, resolver)

	ctx, cancel := context.WithTimeout(context.Background(), perAddrDialTimeout+800*time.Millisecond)
	defer cancel()
	_, err := c.dialAddr(ctx, "127.0.0.1:1")
	if err == nil {
		t.Fatal("expected resolver error to surface, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the resolver's sentinel error to propagate, got: %v", err)
	}
	if strings.Contains(err.Error(), "监听 UDP") || strings.Contains(err.Error(), "QUIC 拨号") {
		t.Fatalf("resolver error did not short-circuit (reached bind/dial): %v", err)
	}
}
