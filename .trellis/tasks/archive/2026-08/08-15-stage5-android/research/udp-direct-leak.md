# 阶段5 研究发现：UDP 直连面分析

## 核心根因确认

**UDP:443 (QUIC/HTTP3) 全走物理直连，绕过 WARP 隧道 → 运营商封 UDP 直连 → 浏览器外网打不开。**

### 数据流分析

#### TCP 路径（已修复，v0.5.24 DNS 拦截后正常）
```
浏览器 → TUN → gVisor → NewConnectionEx → decideAction → proxy → TunnelDial → WARP 隧道 → ✅
```

#### UDP 路径（未修复 = 根因）
```
浏览器 QUIC:443 → TUN → gVisor → NewPacketConnectionEx → relayUDP → net.DialUDP + protectConn → 物理网络 → 运营商封 → ❌
```

### 关键代码位置

1. **androidvpn.go:302-315** — `NewPacketConnectionEx`：非 DNS 的 UDP 全部走 `relayUDP`
2. **androidvpn.go:350-409** — `relayUDP`：用 `net.DialUDP` + `protectConn` 直接物理发出，不经隧道
3. **proxy/udp.go:1-25** — 注释明确说明 UDP 不走隧道（桌面侧 SOCKS5 UDP ASSOCIATE 同样直连）
4. **decision.go:96-105** — `decideTunnelTarget` 只处理 TCP（proxy 分支）
5. **proxy/udp.go:15-24** — 上游不支持 CONNECT-UDP（RFC 9298），warp-svc 只有 ConnectTcpProxy

### 为什么九轮修复都没修到

九轮全在 TCP CONNECT 层（DNS 拦截、IP→域名还原、SERVFAIL 回退）。UDP 直连面从未触碰。

### 修复方向

#### 方向 1（推荐）：TUN 栈拦截 UDP:443 → 丢弃 → 浏览器回退 TCP:443

**原理**：浏览器收到 QUIC 失败后自动回退 HTTP/2 over TCP（Chrome/Firefox 标准行为）。

**实现**：在 `NewPacketConnectionEx` 中，对 `destination.Port == 443` 的 UDP 包不调用 `relayUDP`，而是静默丢弃（关闭 conn）。浏览器 QUIC 探测失败 → 回退 TCP → 走 WARP 隧道 → 通。

**影响面**：
- 只影响 Android TUN 栈（桌面无 TUN，SOCKS UDP 不变）
- QUIC 直连本来就是泄漏，拦截后无功能损失
- debugdiag 的 `udpKind(443)` = "quic" 分类已存在，验证只需观察 quic 行是否消失

**风险评估**：
- 浏览器回退 TCP 有约 100-300ms 延迟（QUIC 探测超时），可接受
- 非浏览器 UDP:443 流量也被拦（如某些 app 的 QUIC），但这些走直连本来也是被墙的

#### 方向 2（备选）：UDP-in-MASQUE
上游 warp-svc 只有 ConnectTcpProxy，不支持 CONNECT-UDP（RFC 9298）。**不可行**，除非换 WARP SDK 或自实现 UDP-over-TCP 隧道。

#### 方向 3（补充）：拦截所有非 DNS UDP
更激进，把所有非 DNS UDP 都丢弃，强制回退 TCP。但可能影响非 HTTP 场景（如游戏、VoIP）。方向 1 更精准。

### 实现计划

修改 `androidvpn.go` 的 `NewPacketConnectionEx`：
- `destination.Port == 443` → 静默丢弃（不 relayUDP，直接关 conn）
- 保持现有 DNS 拦截逻辑不变
- 添加日志区分拒绝与直连
- 可选：通过 Config 开关控制（默认开启拦截）

### 验收
- debugdiag udp.tsv 中不再出现 kind=quic 行
- 真机打开境外网站 + warp=on（东哥验收）
