package mihomo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// engine_test.go covers the Engine that hosts mihomo via hub.Parse — the gate
// ticket #4 demands ("Engine 走 hub.Parse 最小骨架，内核能起再干净退出"). P1-B is
// the code-layer integration gate: it must prove mihomo's library-mode kernel
// boots and shuts down cleanly, and it must do so WITHOUT reaching a real
// vpngate node (#4: 不接真实 vpngate 节点，真节点留给 B-1-PoC-2).
//
// Red→green discipline: this file (slice 3 red) fails to build until Engine,
// NewEngine, Start, and Close exist and meet the round-trip contract.
//
// Safety red lines honored by this suite:
//   - The kernel config carries no real proxy/provider node — hence "不接真实
//     vpngate节点". The YAML is the smallest that boots mihomo's library-mode
//     internals (log level + a loopback mixed inbound on an ephemeral port the
//     OS picks) and nothing more.
//   - net.DefaultResolver is NEVER reassigned here or in the implementation. The
//     spec calls out that mihomo's binary main.go rewrites it, but the library
//     path (hub.Parse → executor.ApplyConfig) does not, and the implementation
//     must not either — the test below pins that invariant by snapshotting
//     DefaultResolver's PreferGo/Dial before Start and asserting they are
//     unchanged after the round trip.
//   - The kernel's home directory is pointed at a test-owned temp dir so mihomo
//     does not touch the repo working tree.

// defaultResolverSnapshot records net.DefaultResolver's two mutation-prone
// fields so a test can assert the Engine did not touch them. mihomo's binary
// main.go sets DefaultResolver.PreferGo=true and replaces DefaultResolver.Dial;
// the library-mode Engine MUST NOT (warp-go relies on DefaultResolver for edge
// resolution — spec safety red line verbatim).
func defaultResolverSnapshot() (preferGo bool, dialSet bool) {
	return netDefaultResolverPreferGo(), netDefaultResolverDialIsSet()
}

// engineRoundTripConfig is the smallest YAML that boots mihomo's library kernel:
//   - log-level silent keeps the test output clean and skips chatty goroutines;
//   - mixed-port 0 makes the OS hand an ephemeral port so the inbound never
//     collides with a real port another test or a host service holds;
//   - bind-address 127.0.0.1 prevents the inbound from ever touching a public
//     interface (#1 user story 17 — 绑回环, 裸口不落公网).
//
// It intentionally has no proxies, providers, or groups, so nothing reaches a
// network node — the code-layer gate, not a real-node path (B-1-PoC-2).
func engineRoundTripConfig() []byte {
	return []byte(`mixed-port: 0
bind-address: 127.0.0.1
log-level: silent
`)
}

// TestEngine_StartStop_RoundTrips asserts the B-1 code-layer integration gate:
// NewEngine parses a minimal config, Start boots the mihomo kernel, Close shuts
// it down cleanly, and the whole round trip is repeatable without leaking state
// between Engines (so a future real-node impl can rebuild the Engine per dial).
func TestEngine_StartStop_RoundTrips(t *testing.T) {
	dir := t.TempDir()

	wantPrefer, wantDialSet := defaultResolverSnapshot()

	eng, err := NewEngine(engineRoundTripConfig(), WithHomeDir(dir))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	if eng == nil {
		t.Fatal("NewEngine returned nil Engine")
	}

	if err := eng.Start(); err != nil {
		t.Fatalf("Engine.Start: %v", err)
	}

	// The safety red line: after the kernel is up, net.DefaultResolver must be
	// untouched. mihomo's library path (hub.Parse) does not rewrite it; the
	// Engine must not either, even if a future change copies more of mihomo's
	// main.go in.
	if gotPrefer, gotDialSet := defaultResolverSnapshot(); gotPrefer != wantPrefer || gotDialSet != wantDialSet {
		t.Fatalf("net.DefaultResolver was mutated by Engine.Start: "+
			"PreferGo before=%v after=%v, Dial set before=%v after=%v "+
			"(warp-go relies on DefaultResolver for edge resolution — spec red line)",
			wantPrefer, gotPrefer, wantDialSet, gotDialSet)
	}

	// Close must be idempotent and return cleanly so Serve/Wait semantics later
	// compose without surprises (P1-A's warpserver.Server.Close is idempotent too;
	// the Engine matches).
	if err := eng.Close(); err != nil {
		t.Fatalf("Engine.Close: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Engine.Close (second call): %v", err)
	}
}

// TestEngine_StartStop_RepeatableWithoutState asserts a second Engine boots and
// shuts down cleanly against the same home dir — i.e. the first Close did not
// leak mihomo global state (a regression guard for the hub.Parse/Shutdown path,
// which holds a package-level mutex). A real-node downstream will rebuild the
// Engine on every dial, so leak-free repetition is the contract.
func TestEngine_StartStop_RepeatableWithoutState(t *testing.T) {
	dir := t.TempDir()
	cfg := engineRoundTripConfig()

	for i := 0; i < 2; i++ {
		eng, err := NewEngine(cfg, WithHomeDir(dir))
		if err != nil {
			t.Fatalf("iter %d NewEngine: %v", i, err)
		}
		if err := eng.Start(); err != nil {
			t.Fatalf("iter %d Start: %v", i, err)
		}
		if err := eng.Close(); err != nil {
			t.Fatalf("iter %d Close: %v", i, err)
		}
	}
}

// TestEngine_RejectsEmptyConfig asserts NewEngine refuses a config with no
// inbound and no payload — a guardrail so a future caller wiring a real-node
// resolver against B-1-PoC-2 cannot boot a kernel with nothing to route on.
// The minimal code-layer gate config (engineRoundTripConfig above) is the floor.
func TestEngine_RejectsEmptyConfig(t *testing.T) {
	dir := t.TempDir()
	_, err := NewEngine(nil, WithHomeDir(dir))
	if err == nil {
		t.Fatal("NewEngine accepted nil config — expected ErrEmptyConfig")
	}
	if !errors.Is(err, ErrEmptyConfig) {
		t.Fatalf("expected ErrEmptyConfig, got: %v", err)
	}
}

// TestWithHomeDir_SetsHomeDir is a unit test on the option plumbing: the temp
// dir the test passes must end up the Engine's home dir, so mihomo writes its
// side artifacts there (geodata cache, etc.) rather than the package's source
// tree. This keeps the test hermetic and the repo clean.
func TestWithHomeDir_SetsHomeDir(t *testing.T) {
	dir := t.TempDir()
	eng, err := NewEngine(engineRoundTripConfig(), WithHomeDir(dir))
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	got := eng.HomeDir()
	if !filepath.IsAbs(got) {
		t.Fatalf("HomeDir not absolute: %q", got)
	}
	if got != dir {
		t.Fatalf("HomeDir = %q, want %q", got, dir)
	}
	// Sanity: the dir exists and is the one the test created (WriteFile in it
	// works), so Start would write there, not into the repo.
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("home dir disappeared: %v", err)
	}
}
