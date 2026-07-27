// Package mihomo is the anti-corruption layer that is the ONLY package allowed
// to import github.com/metacubex/mihomo/... in this module. warp-go's tunnel
// package never imports mihomo internals; it sees only the minimal surface this
// package exports — today, the OvpnEdgeResolver seam the QUIC transport dials
// through, and (in later B-1 tickets) the Engine that hosts mihomo.
//
// This file defines the seam contract and the adapter that binds it to
// tunnel.PacketResolver's function type — without importing tunnel. The two
// types are deliberately structurally distinct so the dependency flows one way
// (anti-corruption layer → stay independent of core), while a structural
// signature match lets a func value flow back into tunnel as a PacketResolver.
package mihomo

import "net"

// OvpnEdgeResolver is the surface warp-go's quic transport depends on for the
// UDP underlay that carries WARP QUIC/UDP datagrams to the edge (#1 user story
// 12/13). Its single method returns a net.PacketConn — any net.PacketConn, OOB
// capable or not — so the tunnel package's dialAddr hands it straight to
// quic.Transport.Dial and quic-go's wrapConn degrades a non-OOBCapable conn to
// basicConn (prototype decision-dense #7, B-1-PoC-1 GREEN root cause).
//
// The concrete production implementation is per OpenVPN country node: it stands
// up that node's gVisor netstack and returns the *gonet.UDPConn the netstack's
// gonet.DialUDP produced (its embedded N.EnhancePacketConn satisfies
// net.PacketConn; packetConn.LocalAddr exists "to make quic-go's
// connMultiplexer happy"). That real-node path is B-1-PoC-2's job, not this
// ticket: P1-B is the code-layer integration gate, so the default resolver here
// declines (see DeclineByDefault) and lets the dial fall back to direct connect.
//
// Returning (nil, nil) is the "decline, take the fallback" contract shared with
// tunnel.PacketResolver — the anti-corruption layer mirrors it so the roll-back
// anchor (B-1-PoC-2 failing on real nodes leaves warp-go dialing exactly as
// today) holds at every layer, not just inside tunnel.
type OvpnEdgeResolver interface {
	// Resolve returns the net.PacketConn to carry WARP QUIC/UDP datagrams to the
	// edge at edgeAddr, or (nil, nil) to decline so the caller takes its
	// net.ListenUDP fallback. A non-nil error aborts the dial.
	Resolve(edgeAddr string) (net.PacketConn, error)
}

// packetResolverFunc is the structural alias of tunnel.PacketResolver. It is
// never exported — it exists so AsPacketResolver's return type is provably the
// tunnel package's expected function shape without this package importing
// tunnel (keeping the dependency one-way: the anti-corruption layer stays
// independent of core).
type packetResolverFunc func(edgeAddr string) (net.PacketConn, error)

// AsPacketResolver adapts an OvpnEdgeResolver to the function type
// tunnel.PacketResolver expects. It is the only hand-off point between the
// anti-corruption layer and warp-go's dial: warp-go wiring constructs an
// OvpnEdgeResolver here, calls AsPacketResolver, and feeds the result to
// tunnel.NewMasqueClientWithResolver. Everything mihomo-specific stays on this
// side of the adapter; the tunnel package sees a plain func and never a mihomo
// internal type (#1 user story 3 — 防腐层收口).
//
// The adapter is transparent: it forwards the resolver's (conn, err) verbatim,
// including the (nil, nil) decline, so the seam contract and the fallback
// anchor are the anti-corruption layer's responsibility to honor, not the
// caller's to remember.
func AsPacketResolver(r OvpnEdgeResolver) packetResolverFunc {
	if r == nil {
		// A nil resolver is treated as a permanent decline — identical to
		// tunnel's own nil-resolver branch, preserving zero behavior change.
		return func(string) (net.PacketConn, error) { return nil, nil }
	}
	return func(edgeAddr string) (net.PacketConn, error) {
		return r.Resolve(edgeAddr)
	}
}

// declineResolver is the default OvpnEdgeResolver: every Resolve declines with
// (nil, nil). It is the P1-B code-layer gate — the seam and the adapter are
// wired and verified to compile/round-trip, but no real vpngate node is ever
// reached (#4 acceptance: 不接真实 vpngate 节点). A real-node resolver (the
// per-country OpenVPN gVisor netstack) lands in B-1-PoC-2, not here.
type declineResolver struct{}

// Resolve always declines, so the dial's downstream net.ListenUDP fallback is
// taken and warp-go connects to the edge exactly as it does today. The edge
// argument is accepted and discarded to keep the surface stable for when the
// real resolver switches on it.
func (declineResolver) Resolve(string) (net.PacketConn, error) { return nil, nil }

// DefaultEdgeResolver returns the B-1 default edge resolver: one that declines
// every Resolve, so wiring the anti-corruption layer in is a no-op on the wire
// until a real-node resolver replaces it. It must never return nil so callers
// that forget to populate a resolver get the safe decline path rather than a
// nil-pointer dereference in AsPacketResolver's caller.
func DefaultEdgeResolver() OvpnEdgeResolver { return declineResolver{} }
