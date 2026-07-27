# warp-go frontproxy

把 warp-go（Cloudflare WARP 的 MASQUE-over-QUIC 客户端）变成"每国一个 SOCKS5 出口"的
前置代理：把到 WARP edge 的 UDP 数据报注入 mihomo OpenVPN 国家节点的 gVisor netstack
出口，使 WARP edge 看到节点国 IP 作为源 —— 从而全球 WARP IP 可按国落地。

## Language

### 拓扑

**WARP edge**:
Cloudflare 的 MASQUE 隧道对端；warp-go 向它发 Bearer CONNECT-TCP over QUIC。
_Avoid_: Cloudflare 服务器, 边缘节点（"边缘"指 edge 时才用）

**Mihomo OpenVPN 节点**:
来自 vpngate 的 OpenVPN profile，经 mihomo 加载后出口 IP 落在节点国。B-1 里它位于
WARP edge 之前，是 WARP 数据报的 underlay。
_Avoid_: vpngate 节点（只在指 provider 来源时用）, VPN 节点（太泛）

**Country exit / 国家出口**:
一个 mihomo OpenVPN 节点提供的、按 ISO 国家码绑定的出口 IP。每国一个 listener 端口。
_Avoid_: 国别国国, 出口节点（不够精确）

### 端口

**Per-country listener**:
每个国家一个 mihomo listener，绑 `127.0.0.1:<port>`（草案 7841..7847 = JP/KR/US/VN/RU/ID/TH）。
对外暴露须经前置 Caddy/Traefik TLS+auth。
_Avoid_: 国国端口, 国家端口

**Internal SOCKS5**:
warp-go 内核暴露给前置链路的 SOCKS5 入口，绑 `127.0.0.1:40000`。
_Avoid_: 主代理, 内口

**Mihomo mixed**:
mihomo 自身的 mixed inbound，绑 `127.0.0.1:7840`。
_Avoid_: mihomo 入口, mixed 端口

**External controller**:
mihomo 的管理 API 端点，绑 `127.0.0.1:9090`，须 32 字符随机 secret + 精确 CORS
（never `*`）+ firewall。
_Avoid_: 控制器, 控制端口
