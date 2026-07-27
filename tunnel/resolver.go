package tunnel

import "net"

// PacketResolver decides how dialAddr obtains the UDP conn that carries the
// QUIC/UDP datagrams to the WARP edge. It is the B-1 injection seam (ADR-0001):
// the frontproxy anti-corruption layer supplies an OpenVPN-backed net stack for
// those datagrams so the edge sees the node country IP as the source.
//
// Returning a non-nil (conn, nil) makes dialAddr dial through it; returning
// (nil, nil) makes dialAddr fall back to a plain net.ListenUDP bound in the
// same address family as the edge — i.e. the current direct-connect behavior,
// unchanged. A non-nil error aborts the dial.
//
// 回流契约：注入通道与 SOCKS5 listener 是两根独立的 fd —— resolver 返的
// net.PacketConn 由 warp-go 自己的拨号 goroutine 独占读取，绝不与任何 listener
// 共用，否则两个 goroutine 会互偷对方 datagram（原型决策密集点 #1）。
type PacketResolver func(edgeAddr string) (net.PacketConn, error)
