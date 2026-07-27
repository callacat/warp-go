package main

import (
	"testing"

	"warp/internal/warpserver"
	"warp/tunnel"
)

// These tests pin the two bare-port-harden decision points of P1-A that live
// here in main.go rather than in the warpserver package:
//
//  1. isLoopbackListen — the gate main uses to refuse a plaintext SOCKS5 bind
//     on a non-loopback interface without auth. The decision must classify the
//     exact forms the spec names: the literal "127.x"/"::1" IPs and "[::1]:.."
//     as loopback; "0.0.0.0:..", ":port" (wildcard), and any hostname as
//     non-loopback. We assert behavior, not the parsing flags it happens to use.
//  2. *tunnel.MasqueClient must satisfy warpserver.ProxyClient at compile time
//     — the interface-ification is the seam main.go holds. If the method set
//     ever drifts, this stops compiling, which is a better failure mode than a
//     runtime assertion deep in a test.

// TestIsLoopbackListen_DecisionEquivalence asserts the bare-port gate classifies
// every -l form the spec enumerates the way the spec demands. Unparseable input
// is treated as loopback (so net.Listen is the single authority that reports the
// real error, not the gate), matching the explanatory comment on isLoopbackListen.
func TestIsLoopbackListen_DecisionEquivalence(t *testing.T) {
	for _, tc := range []struct {
		addr  string
		want  bool
		note  string
	}{
		{"127.0.0.1:40000", true, "v4 loopback default"},
		{"127.0.0.1:0", true, "v4 loopback ephemeral"},
		{"127.1.2.3:1080", true, "any 127/8 is loopback"},
		{"[::1]:40000", true, "v6 loopback literal (bracketed form parses)"},
		{"::1:40000", true, "unparseable (too many colons) — gate defers to net.Listen as the single authority"},
		{"missing-port", true, "unparseable (no port) — same defer-to-net.Listen branch, true so net.Listen reports the real error"},
		{"0.0.0.0:40000", false, "all-interfaces v4 wildcard — public, refuses without auth"},
		{":40000", false, "no-host wildcard binds every interface — public"},
		{"0.0.0.0:0", false, "wildcard + ephemeral still all-interfaces"},
		{"example.com:1080", false, "hostname resolves to whatever resolver says — treat as public"},
		{"localhost:1080", false, "localhost is a hostname, not an IP literal — public-facing"},
		{"no-host:40000", false, "non-IP hostname (parses but is not an IP literal) — public-facing"},
		{"255.255.255.255:1080", false, "broadcast address is not loopback"},
		{"8.8.8.8:1080", false, "public v4 — refuses"},
		{"[2606:4700:103::2]:443", false, "public v6 — refuses"},
	} {
		got := isLoopbackListen(tc.addr)
		if got != tc.want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v — %s", tc.addr, got, tc.want, tc.note)
		}
	}
}

// TestMasqueClientSatisfiesProxyClient is a compile-time assertion that the
// real *tunnel.MasqueClient still satisfies the warpserver.ProxyClient seam
// main.go holds it as. If the method set drifts (e.g. cfg type renamed, ctx
// removed) this fails at compile time — the cheapest place to catch a refactor
// that would otherwise surface as a runtime error in a tunnel-package test.
func TestMasqueClientSatisfiesProxyClient(t *testing.T) {
	var _ warpserver.ProxyClient = (*tunnel.MasqueClient)(nil)
}
