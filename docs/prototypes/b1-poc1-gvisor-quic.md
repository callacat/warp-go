# B-1-PoC-1 裁定：gVisor netstack UDPConn 驱动 quic-go 握手

> 本文件是 **原型裁定的 context pointer**，不是实现。按 /prototype 规则：原型代码留在
> `feat/frontproxy` 分支 `frontproxy/mihomo/proto/`（throwaway），本文件记录问题与答案，
> 供实现问题 #7（共用底座）/ #8（B 反向拓扑）引用。

## 被回答的问题

> 一个来自 sing-wireguard `StackDevice.ListenPacket` 的 gVisor netstack `*gonet.UDPConn`
> (即 mihomo OpenVPN outbound 内部使用的同款 underlay)，能否驱动 quic-go 的 listen
> goroutine 完成完整的 QUIC 握手 + 一次 echo RTT？

这是 **B-1 拓扑的门控问题**：
B-1 = 修改 warp-go 的 `diaAddr`，把到 WARP edge 的 QUIC/UDP 数据报注入一个来自 mihomo
OpenVPN 节点 gVisor netstack 的 `net.PacketConn`，使 WARP edge 看到节点出口国 IP 作为源。
若 gVisor 的 `*gonet.UDPConn` 不能驱动 quic-go 握手，则 B-1 死，需回退 A 或 fork mihomo（B-2）。

## 裁定

**GREEN — 3/3 稳定通过。**

```
=== PROTOTYPE VERDICT: GREEN: gVisor netstack gonet.UDPConn drove a quic-go
handshake + one echo RTT (echo=18c6120d1b72adf) ===
```

gVisor netstack 的 `*gonet.UDPConn`（`net.PacketConn`）可完整驱动 quic-go：
`Transport.Dial` 完成握手 → `OpenStreamSync` → 双向字节流 echo 往返成功。

## 原型如何证明

`frontproxy/mihomo/proto/proto_gvisor_quic.go`（`go:build with_gvisor`）：

1. 用 `wg.NewStackDevice` 起一个裸 gVisor netstack（与 OpenVPN outbound 内部 underlay 同款，
   **不** 引入 OpenVPN — 仅验证 netstack↔quic-go 这一层）。
2. 本机起 `net.ListenUDP` + `quic.Listen` 的 echo listener（标准真实 UDP socket）。
3. 用 `dev.ListenPacket(ctx, dst)` 拿到 `*gonet.UDPConn`，喂给 `quic.Transport{Conn:...}`，
   `tr.Dial` 走 netstack 内部 UDP 端点。
4. 一个 `miniRouter` goroutine 把 netstack 的 IPv4/UDP 包 strip 头后写真实 loopback socket，
   并把 listener 的回包重新封 IPv4/UDP 头写回 netstack（dst = netstack host，dst port =
   从首个 outbound 包捕获的 netstack client 源端口）。
5. client 完成 `OpenStreamSync` → write 8B → server echo → client `io.ReadFull` 回 8B。

## 关键设计点（实现 #8 时必须照搬）

1. **listener 与 netstack 的 wire 必须是两根独立 UDP socket**。早期版本让 quic listener
   和 miniRouter 共用同一 socket：两 goroutine 抢读同一 fd，互偷对方的 datagram，且 listener
   回包目标 = netstack 虚拟地址（内核不可路由）直接丢包。修正：miniRouter 独占一根 outbound
   socket 作为 netstack 的 "wire"；quic listener 独占自己的 socket。
2. **回包 dst port 必须是 netstack client 的源端口**，不是 listener 端口。netstack 内
   gonet.DialUDP 选自己的 ephemeral src port；router 从首个 outbound 包捕获它，回包 dst port
   写它，包才能路由回 client 的 gonet endpoint。早期版本写成 listener 端口 → 永远到不了 client。
3. **`netip.IPv4(a,b,c,d)` 在 Go 1.26 已删除**，须用 `netip.AddrFrom4([4]byte{a,b,c,d})`。
4. **自签证书必须带 IP SAN** (`IPAddresses: []net.IP{127.0.0.1, ::1}`)，否则 quic-go client
   TLS 校验 `x509: cannot validate certificate for 127.0.0.1` 失败。（实现侧由 reg.json 即时
   生成客户端证书，注意 SAN 应覆盖 WARP edge 域名 `engage.cloudflareclient.com`。）
5. **echo server 写完回包后不要立即 `c.CloseWithError`** — CONNECTION_CLOSE 会在 client Read
   前冲掉 stream FIN。实现侧不需要这个（WARP 是长期 QUIC 连接），仅原型 echo 一次用此规避。
6. quic-go `Transport.Dial(ctx, addr net.Addr, ...)` 第 2 参必须是 `*net.UDPAddr`（实现
   `net.Addr`），不是 `netip.AddrPort`。
7. quic-go v0.61.0 无 `quic.Connection` 类型，`Listen.Accept`/`Transport.Dial` 返回 `*quic.Conn`。
8. quic-go 的 `wrapConn` 接受任意 `net.PacketConn`：非 `OOBCapablePacketConn` → `basicConn`，
   缓冲区设置失败仅记警告、不中断 — 即 `*gonet.UDPConn` 无OOB 也能跑。

## 对实现问题 #7 #8 的结论

- **#8（B 反向拓扑）**：B-1 路径**活着**。`diaAddr` 注入一个 `net.PacketConn`（来自 OpenVPN
  节点 gVisor netstack `*gonet.UDPConn`，经 `OpenVPN.ListenPacketContext` →
  `tunDevice.ListenPacket` → `NewPacketConn(pc, o)` 包成 `C.PacketConn`，其嵌入的
  `N.EnhancePacketConn` 实现 `net.PacketConn`，`packetConn.LocalAddr()` 注释 "make quic-go's
  connMultiplexer happy" 印证此设计）给 `quic.Transport.Dial`，WARP QUIC 数据报即走节点出口国。
  Edge 合约不变（仍是 Bearer CONNECT-TCP over QUIC）。
- **#7（共用底座）**：B-1 的接入 seam 落在 warp-go 已存的 `tunnel/masque.go` `diaAddr` 周边；
  PacketConn 注入需要一个 resolver seam（防腐层 `frontproxy/mihomo` 暴露一个
  `OvpnEdgeResolver` 接口，返回 `net.PacketConn` 给 warp-go 的 quic transport，禁止 mihomo
  内部包散落到 warp-go 其它包）。

## 仍待验证的后续 PoC（非本次门控）

- **B-1-PoC-2**：OpenVPN 国出口到 WARP edge IP 的真实网络通路（需真实 vpngate 节点，
  网络层非代码层）。
- **B-1-PoC-3**：selector 钉国节点 + Type guard 防 url-test 冷选回直连（mihomo 行为层）。

## 复现

```
go run -tags with_gvisor ./frontproxy/mihomo/proto -timeout 20s
# 末行应为 === PROTOTYPE VERDICT: GREEN ... ===
```
