# B-1 反向拓扑与嵌入式 mihomo

Status: accepted（B-1-PoC-1 门控 GREEN，见 `docs/prototypes/b1-poc1-gvisor-quic.md`；
可被 B-1-PoC-2 真节点失败 supersede）

## Decision

warp-go 的到 WARP edge 之 QUIC/UDP 数据报经一个注入的 `net.PacketConn` 走 mihomo
OpenVPN 国家节点（gVisor netstack `*gonet.UDPConn`）出口，使 WARP edge 看到节点国 IP
作为源 —— 全球 WARP IP 按国落地。mihomo 以 `github.com/metacubex/mihomo v1.19.29`
的 `hub` 包**库嵌入**（非外部容器编排），所有 mihomo import 收口在 `frontproxy/mihomo`
防腐层；warp-go 其余包零 mihomo 内部依赖。Edge 合约不变（仍是 Bearer CONNECT-TCP over
QUIC）。

## Considered Options

- **A 直连**（被拒）：warp-go 现状，`tunnel/masque.go` 的 `net.ListenUDP` 直达 WARP edge。
  源 IP 是本机/容器出口，无法按国落地。
- **B-1 反向**（入选）：OpenVPN 节点出口置于 edge 之前。门控 PoC-1 已证 gVisor
  netstack `*gonet.UDPConn` 能驱动 quic-go 握手 + echo RTT（GREEN）。
- **B-2 mihomo 内置 masque outbound**（被拒）：mihomo `MasqueOption` 无 token 字段，需
  fork mihomo 维护成本过高。若有外来者半年后提议"为何不用 mihomo 自己的 masque
  outbound"——这正是拒因。

## Consequences

- 接入 seam 落在 `tunnel/masque.go:274` 的 `diaAddr`：注入的 `PacketResolver` 返
  `net.PacketConn`；返 nil 回退 `net.ListenUDP`（零行为变化）。防腐层 `frontproxy/mihomo`
  暴露 `OvpnEdgeResolver` 接口给 warp-go，禁 mihomo 内部包散落。
- B-1-PoC-1 只证了"代码层 netstack↔quic-go 握手"，**未证"OpenVPN 国出口到 WARP edge IP
  的真实网络通路"**（B-1-PoC-2，需 vpngate 真节点）。B-1 仍有网络层回退到 A 的风险。
- 须保留构建标签 `-tags with_gvisor`（Docker + 本地）。
