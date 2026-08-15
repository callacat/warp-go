# Android TUN 栈调研报告 —「境外流量打不开」未决问题（v0.5.13→v0.5.27 九轮失败复盘）

> 调研人：android-tun-expert 子代理
> 日期：2026-08-15
> 状态：只读调研，未改任何代码。本机无 SDK/NDK/JDK，无法编译 Android；全部结论基于静态读码 + AGENTS.md/CHANGELOG/计划文档证据链 + debugdiag 20260808 数据分析。
> 限制：仓库为 shallow clone（`git rev-list --all --count` = 1，仅 `5587626` 一条），**git log 无法对照上游 androidvpn/ 提交史**；上游差异只能依赖 AGENTS.md §6.6 冲突策略与 §8 已确认事实。

---

## 1. 全链路数据流（附证据位置）

```
┌─ Android 用户空间 ─────────────────────────────────────────────────────────────┐
│  任意 app（含浏览器）                                                            │
│    │ TCP/UDP 包 → 系统路由表（VpnService 装了 0.0.0.0/0 + ::/0 全量路由）         │
│    ▼                                                                            │
│  tun0（内核 TUN 设备）                                                          │
│    │ fd: WarpVpnService.establish() → pfd.detachFd() → nativeStartVpn(fd)       │
│    │     gui/build/android/.../WarpVpnService.java L175-224（setBlocking(true)、│
│    │     addRoute 0.0.0.0/0 + ::/0、addDnsServer 198.18.0.1、detachFd）         │
│    ▼                                                                            │
│  sing-tun（androidvpn/androidvpn.go L82-140）                                    │
│    │ tun.New(base{FileDescriptor, MTU 1500, Inet4/6Address, DNSServers})        │
│    │ tun.NewStack("gvisor", {UDPTimeout 5m, ICMPTimeout 10s})  ← gVisor 用户态栈│
│    ▼                                                                            │
│  gVisor 栈解析出 TCP 流 / UDP 报文 → Vpn 的 Handler 回调                         │
│    │                                                                           │
│    ├─ TCP → NewConnectionEx（androidvpn.go L176-299）                            │
│    │     host = destination.AddrString()；若命中 dnsInterceptor.LookupDomain    │
│    │     → host 换成映射域名（v0.5.24 根因修复，L197-205）                        │
│    │     decideAction(kernel.Route, host, destination.Addr)（decision.go）      │
│    │      → proxy      : decideTunnelTarget 还原域名后 DialTunnel → H3 CONNECT   │
│    │      → direct     : 保留原始 IP → net.Dialer（Dialer.Control protect fd）   │
│    │      → reject     : 立即 Close（decision.go L130-139）                     │
│    │     双向 relay：conn↔upstream，debugdiag 记录 upBytes/downBytes/firstMs    │
│    │                                                                           │
│    ├─ UDP → NewPacketConnectionEx（androidvpn.go L302-315）                      │
│    │     destination==198.18.0.1:53 → handleDNSQuery → DNS 拦截                 │
│    │        （dns.go：隧道 DoH 解析 + remember(IP→域名) + SERVFAIL 兜底）        │
│    │     其余 UDP（含 QUIC:443、物理 DNS:53） → relayUDP **物理直连**（protect） │
│    │        ←—— ⚠ 见疑点 6：境外 UDP/HTTP3 不走隧道，全直连                     │
│    ▼                                                                            │
│  core.Kernel（core/kernel.go；built 于 gui/androidbridge.go L312-490）           │
│    │ Route()=route.Engine.Match；DialTunnel()；ResolveDNS()（隧道内 DoH）        │
│    ▼                                                                            │
│  tunnel.MasqueClient（tunnel/masque.go）                                         │
│    │ DialTunnel → resolveTarget（域名→隧道内 DoH 解析，L1478-1503）               │
│    │ establishCONNECT → openRequestStream → https3.RequestStream               │
│    │   → h3Client（quic-go 共享 QUIC 连接，Edge 边缘）                            │
│    ▼                                                                            │
│  物理网络：QUIC UDP socket 已 protect（WarpVpnService.protectSocket）           │
│    → Cloudflare Edge（162.159.198.2:4443 等）→ MASQUE over QUIC → 目标           │
└─────────────────────────────────────────────────────────────────────────────────┘
```

**决策与拨号分支细节（关键）**：
- 路由判定：`decideAction`（androidvpn/decision.go L180-189）——route==nil 或未命中 → **"proxy"兜底**（v0.5.19 从 miss→direct 改为 miss→proxy）。
- 拨号目标：`decideTunnelTarget`（decision.go L96-105）——**IP→域名还原只用于 proxy 分支**（v0.5.25 修复：direct 还原域名会触发 net.Dialer 物理解析再进 TUN 环路）。
- TCP direct：`net.Dialer{Control: protect}`（decision.go L148-157）。
- UDP 中继：`relayUDP`（androidvpn.go L350-409）——`net.DialUDP` + `protectConn(remote)`（L363）→ 物理直连。
- DNS 拦截：仅当目标 == 198.18.0.1:53（dns.go L40 `DNSInterceptAddr`；androidvpn.go L308 判定）。
- 隧道层：`tunnelConn.Read/Write`（masque.go L1242-1254）在连接级错误时 `noteDeadStream` → dead 置位 + 异步 retire/reconnect。

---

## 2. 关键疑点逐条分析

### 疑点 1：TUN fd / JNI 传递与 SELinux / 权限冲突 —— **不是根因（已闭环）**

- fd 从 `WarpVpnService.establish()` → `pfd.detachFd()`（Java L219）→ `nativeStartVpn(fd)` —— **同进程内**传递，无跨进程 binder、无 SELinux 域切换（VpnService 与 Go c-shared 同一 .so 同一进程）。
- fd 所有权：v0.5.16 已修复 fdsan double-close（detachFd 后 Go 单一持有，`androidvpn.Vpn.fd` 字段 + Stop 兜底关，androidvpn.go L56/L101/L161-164）。
- JNI 内存模型（疑点 4 详述）在 9 轮中已打磨，无未解决的 fd/内存泄漏疑点。
- **结论**：此链路虽经多次回滚重写，但症状（境外流量打不开、TUN 流量增长正常）与"fd 没传对/SELinux 拒绝"不符，排除。

### 疑点 2：决策逻辑误判 / 「未命中→direct 兜底」在 Android 上是否成立 —— **已修复，但存在两处残留风险**

- **历史**：v0.5.18 miss→direct 导致国外 IP（172.217.x）全直连被墙 → v0.5.19 改为 miss→proxy 并锁定（decision.go L180-189，`TestDecideAction` T4/T6）。
- **残留风险 A（判定歧义/文档不一致）**：`rules/default-rules.txt` 头注释仍写「全部未命中时兜底 direct」、AGENTS.md §4 也写「未匹配 → 隐式 direct 兜底」——与 `decideAction` 的实际 miss→proxy 语义**矛盾**。虽然代码路径（TUN 走 decideAction，不是 route.Engine）行为正确，但任何未来维护者按注释改代码都会原地复现 v0.5.19 bug。**建议重构时统一口径**。
- **残留风险 B（规则顺序敏感）**：`REJECT` 在最前 → direct(private) → proxy(google/geolocation-!cn/telegram) → direct(cn)。境外域名若被 `geosite:private`/`geosite:cn` 误命中（如小额后缀、CDN 泛域名），会落 direct → 本地直连 → 被墙。规则集 9 条对「非中、非 G、非私有」的境外长尾（如 non-google 海外站点、游戏服、API）依赖 miss→proxy 兜底——**此兜底已正确**，但任何规则热重载导入坏规则都可能破坏它（Android 规则页可编辑 rules.txt）。
- **结论**：核心判定正确，「miss→direct」问题已在 v0.5.19 修复。剩余是文档口径与规则集运维风险，不是当前打不开的直接根因。

### 疑点 3：VpnService 路由规则遗漏 —— **基本完备，但 DNS 覆盖存在结构性缺口**

- 路由：`0.0.0.0/0` + `::/0`（Java L179-180）全量；`setBlocking(true)`（L178）、MTU 1500、`addDnsServer("198.18.0.1")`（L183）。无 UID 排除（全 app 走 VPN，正确）。注册 API/边缘 IP 通过 protect 豁免（拨号 socket + TCP/UDP direct socket）。
- **完整性 OK**：注册/边缘/自路由都已处理。
- **结构化缺口（重要）**：`addDnsServer` 只配了 198.18.0.1。但 v0.5.25 真机日志出现 `UDP → 114.114.114.114:53（直连）`，证明存在**绕过系统 DNS 的查询**（app 硬编码 DNS、或 Android 对拦截 SERVFAIL 后没等回退直接发第二来源查询）。这类查询返回**本地视图 IP** → IP→域名映射 miss → proxy 分支用裸 IP 走 DialTunnel → **边缘不可达**（v0.5.24 确诊的根因路径在「映射 miss」前提下重新复活）。9 轮修复没补这块。
- 上限结论：路由规则本身没漏 `routes/UID`——漏的是「**DNS 视图统一**」这一层（见疑点 5）。

### 疑点 4：JNI 跨线程 / 内存 / 生命周期 —— **装配层已稳固，无直接致「打不开」的 bug**

- `androidbridge.go` 全读出：g_jvm 全局引用（L12-42）、getEnv/AttachCurrentThread 封装进 C preamble（符合 §6.8.3）、androidCtl/warpCtl 全局引用 + methodID 缓存（L167-188）、nativeStartVpn 主线程快速返回 + goroutine 装配（L205-302）、运行期 ctx 切换单临界区（L445-463）、rollback 的 current 守卫（L392-421）、nativeStopVpn 幂等（L571-602）。
- 已知历史 bug 全部闭环：GetObjectClass 误用（v0.5.15）、ANR 阻塞（v0.5.9）、装配 ctx 泄漏进运行期（v0.5.20）、dns 线程/竞态。
- **一处注释-代码不符（低风险）**：AGENTS.md 声称 config.json/rules 在 goroutine 内装配，但 `buildAndroidConfig` 实际在 **nativeStartVpn 主线程**同步执行（L257）——只是读文件+PEM 解码，通常 <100ms，不构成 ANR。重构时可移入 goroutine 对齐注释。
- **结论**：JNI 装配、反向桥、fd 生命周期无「境外打不开」相关缺陷。

### 疑点 5：DNS 解析链路 —— **主路径正确，但有三个未堵住的洞（高嫌疑）**

主链路（v0.5.24-25 修复后）：
系统 getaddrinfo → 198.18.0.1 → TUN → gVisor UDP → `handleDNSQuery`（androidvpn.go L320-347）→ `HandleQuery`（dns.go L114-186）→ `kernel.ResolveDNS`（隧道内 DoH，masque.go L1540）→ 边缘可达 IP + `remember(IP→域名)`（dns.go L167）→ 后接 TCP 还原域名走 DialTunnel。

**洞 A：类型不匹配仍 drop（非 SERVFAIL）**。dns.go L152-162：`AAAA 查询拿到 v4`（或反之）→ `return nil` **静默丢弃**。v0.5.25 只给「解析失败」加了 SERVFAIL，没给「类型不匹配」加。IPv6-only 网络 / AAAA-first 系统上 AAAA 查询概率性挂起。
**洞 B：映射 miss 路径完全未处理**。绕过系统 DNS 的查询（硬编码 DoH/DoT、物理 DNS 直连泄漏——v0.5.25 日志实锤 `114.114.114.114:53`）→ 本地视图 IP → `LookupDomain` miss → proxy 用裸 IP → **边缘不可达 hang 到 deadline**（v0.5.24 确诊根因路径）。这是「浏览器能开一些站、另一些 app/站打不开」的典型形态。
**洞 C：TTL 与映射过期不一致**。应答固定 TTL 300s（dns.go L215），映射 TTL 10min（dns.go L62）——IP 换绑后最长 10 分钟内老映射仍把新流量引向旧域名（影响小，域名路径会重新 DoH；但 remember 覆盖后共享 IP 的 CDN 域名会互相串，host 头还原可能带错域名，边缘按 Host 路由时可能 404/403）。

**结论**：DNS 主链路是 viv0.5.24 真根因的正确修复；**未覆盖的洞 B（绕过系统 DNS 的 app/第二来源查询）是境外流量打不开的高概率残余路径**。

### 疑点 6：UDP/QUIC —— **最大未排除的结构性疑点（高嫌疑）**

- **设计决定**：ADR 5「UDP 不走隧道（上游限制）：规则仅作用 TCP CONNECT；UDP 全直连」。Android 继承（androidvpn.go L313 注释「与桌面端 UDP 不走隧道一致」；NewPacketConnectionEx L308 只拦 198.18.0.1:53，其余 `relayUDP` 物理直连）。
- **推论**：浏览器 HTTP/3（QUIC over UDP:443）、QUIC-based 视频/实时流量在 Android TUN 下**全部 UDP 物理直连**（protect 后出物理网）→ **不经 WARP 隧道 → 若 ISP 对境外 UDP:443 QoS/封锁，这些流量必然失败**。而 TCP:443 走隧道正常 → 恰好形成「TCP 隧道全建立、页面还是打不开」的 debugdiag 观感。
- debugdiag 设计者已意识到这点：`udpKind`（decision.go L111-120）专门定义 kind=quic（443 HTTP/3 直连泄漏）、kind=dns（53 非拦截泄漏）——**udp.tsv 的存在本身就承认这两类泄漏是嫌疑对象**。
- **补证**：多轮修复（v0.5.23/24/25/27）全部聚焦 TCP CONNECT 层（CONNECT 超时、边缘视图、IP 域名还原、隧道重连）；**没有任何一轮动过 UDP 直连路径**。若用户环境「境外流量打不开」的主要形态是 H3/QUIC/UDP 应用（游戏、视频、部分网页），此路径从未被修。
- 需要验证的交互项：Chromium 的 H3 fallback（QUIC 连接失败后回退 H2 over TCP，TCP 走隧道 → 应能通，仅慢）是否在真机上真的触发；UDP NAT 5min 超时（androidvpn.go L121）是否掐断长时间闲置 H3 会话（quic KeepAlive 无法跨越 gVisor UDP NAT 回收）。

### 疑点 7：上游差异 —— **无法 git 对照，只能依赖文档**

- 仓库 shallow（depth=1，仅 1 条 commit 5587626），`git log -- androidvpn/`、`git log -- tunnel/masque.go` 均为空 → **无法做 git 比对**。这是本次调研的环境硬限制，已记录，供编排者补拉全量历史。
- 据 AGENTS.md §6.6/§8：androidvpn/、gui/androidbridge.go 是仓库**自研**（上游 badafans/6Kmfi6HP 无 Android TUN）；tunnel/masque.go 为重叠文件、追加式改动（RouteFunc/DialTunnel/socketProtector/connBundle.dead/探测）。与上游的偏离点即上述疑点 2/5/6 覆盖的 Android 特有路径，无上游参照可比。

---

## 3. 九轮修复失败原因推断（逐轮）

| 版本 | 修了什么 | 层 | 为什么没根治 |
|---|---|---|---|
| v0.5.13 | 自路由根因（QUIC socket 未 protect → ClientHello 滞留 tun） | 启动 | 只 protect 拨号 socket |
| v0.5.14 | protectSocket/kernelFailed 桥、dial_timeout 可配 | 启动 | 只 protect QUIC，direct 未豁免 |
| v0.5.15 | JNI GetObjectClass 闪退 | 启动 | 装配路径仍被上一层阻断，未到流量层 |
| v0.5.16 | gVisor 栈选型 + fdsan double-close | 启动 | 同上 |
| v0.5.17 | udpnat panic（UDPTimeout/ICMPTimeout） | 启动 | 同上 |
| v0.5.18 | direct 环路风暴（SetSocketProtector 全覆盖） | 通路 | 修好环路，暴露下一层「miss→direct 被墙」 |
| v0.5.19 | miss→proxy 兜底 + 停止按钮 | 决策 | 境内/境外直连分开，境外仍不通 |
| v0.5.20 | 装配 ctx 泄漏进运行期 | 生命周期 | 修好「VPN 开但 TUN 死」，暴露 CONNECT 层 |
| v0.5.21 | 单目标超时不再退共享连接（distinct 3） | 重连 | 过保守：真黑洞永不重连 |
| v0.5.23 | CONNECT 失败计数（同目标累计 2 次）重连 | 重连 | 367 模型仍只覆盖「TCP CONNECT」症状 |
| v0.5.24 | **决定性实验**：CONNECT 目标必须边缘可达 → DNS 拦截 | DNS | 正确根因之一，但只覆盖系统 DNS 主链路 |
| v0.5.25 | DNS 拦截回归（direct 还原环路、SERVFAIL） | DNS | 补洞，但洞 B（绕过系统 DNS）仍未堵 |
| v0.5.26 | debugdiag 遥测 | 观测 | 只观察，不修复 |
| v0.5.27 | udp4/6、dead 快速重连、出口探测 | 隧道层 | 用户复测仍失败 → 2026-08-08 放弃 |

**共同失败模式（核心推断）**：
1. **打地鼠式逐层暴露**：v0.5.13-17 是启动崩溃层，v0.5.18-23 是通路/重连层，v0.5.24 才第一次触及「流量真正走不通」的物理根因（边缘视图），v0.5.25-27 在其上打补丁。**每一轮都在修上一轮暴露的下一层，而不是端到端验收「用户实际打开境外网站」**。
2. **模拟器 ≠ 用户 ISP**：模拟器（LDPlayer）出口网络与用户真实 WiFi/移动网络不同；模拟器 root shell 绕过 VPN（war 恒 off，只能看 tun0 计数）。CI 全绿、模拟器 tun0 增长 ≠ 用户能打开境外网站。v0.5.17/0.5.20 的模拟器验收都是「tun0 流量增长」而非「warp=on + 网页可开」。
3. **没有 v0.5.27 复测包**：AGENTS.md 交接明确「接手第一步是索取 v0.5.27 的新 debugdiag 包」——26→27 的改善（network unreachable 是否消失、死亡频率是否下降）完全未验证就被放弃。
4. **UDP/HTTP3 直连从未被纳入修复面**（疑点 6）——若用户环境 QUIC 出境被 QoS/封锁，这 9 轮 TCP 层修复永远不会奏效。

---

## 4. 判别实验方案（按排除法优先级）

> 标注：`[debugdiag]` = 用户端 debug 包分析；`[adb]` = 远程模拟器 adb；`[代码]` = 静态/宿主单测。
> **前置（最关键）**：向用户索取 v0.5.27 复测时的 `warp-go-debugdiag-*.zip`。无此包，实验 2-6 只能盲猜。

### 实验 0（P0，判别问题归属）同网络对照 —— 先分「我们的锅」还是「ISP 的锅」
- **做什么**：让用户在**同一 WiFi/移动网络**下，用桌面 CLI（`./warp -reg` + 启动 + `curl --socks5-hostname 127.0.0.1:40000 https://www.google.com`）测同一批境外网站；再用**官方 1.1.1.1 WARP 客户端**（连同一账号或默认 WARP）测同样网站。
- **验证**：CLI curl 返回 `warp=on` + 网页可开？官方客户端可开？
- **期望**：
  - 官方/桌面都失败 → **WARP 服务在该 ISP/区域出口被 QoS/封锁**，与本实现无关，问题降级为文档说明（终止 Android 侧排查）。
  - 官方/桌面成功、Android 失败 → 问题在 TUN 栈/Android 侧，继续实验 1-6。
- **用什么**：`[adb]` 无法做（模拟器网络不同）——这是**用户真机/PC 操作**，是 AGENTS.md 交接第 1 条。

### 实验 1（P1，验证 v0.5.27 是否有净改善）新 debugdiag 对比
- **做什么**：拿到 v0.5.27 复测包后，与 20260808 包对比：
  1. `tunnels.tsv` 中 `network is unreachable` 是否仍出现（验证 udp4/6 修复是否生效）；
  2. 批量死亡（同毫秒多条 `quic: transport closed` / RST）频率是否下降；
  3. 死亡错误类型是否变化（`[::]:` 尚存 → socket 收紧没生效；`sendmsg: Network is unreachable` 消失 → 生效）。
- **期望**：若 `unreachable` 归零但境外仍打不开 → 隧道死亡不是（或不再是）主因，转向实验 2/3；若仍频繁死亡 → 隧道层问题确认，转实验 4/5（ISP 对 UDP 4443 的 QoS）。
- **用什么**：`[debugdiag]`。

### 实验 2（P2，验证 UDP/HTTP3 直连假设）—— 高价值，几乎零成本
- **做什么**：① 从 debugdiag 包 `udp.tsv` 数 `kind=quic` 行：**如果存在大量 quic 直连的记录且对应时间窗外网站打不开 → UDP 直连路径是嫌疑人**。② 让用户在浏览器设置关闭 HTTP/3（Chrome `--disable-quic` 或换仅 H1/H2 的场景/客户端）后复测打不开的站点。
- **验证**：关 H3 后站点能开 → 实锤 QUIC 直连被墙；`udp.tsv` 的 quic 行消失。
- **期望**：关闭 H3/QUIC 后境外网页可开 = **根因在 UDP 直连（ADR 5 的「UDP 不走隧道」在 Android 上的不良后果）**，修法是把 QUIC:443 UDP 也纳入隧道（需上游支持 UDP-in-MASQUE）或引导浏览器降级 H2；否则排除。
- **用什么**：`[debugdiag]`（udp.tsv）+ 用户浏览器操作。**可选 `[adb]`**：模拟器上 `adb shell settings` 关不了 Chrome H3，可临时用 `curl -4` 或仅 TCP 协议的自测页区分（tcpdump 抓 udp 443 是否出网）。

### 实验 3（P3，验证 DNS 洞 B：绕过系统 DNS 的流量）映射 miss 追踪
- **做什么**：从 debugdiag `tunnels.tsv` 找 `host` 为**裸 IP** 且 `firstByteMs=-1`（CONNECT 成功但无数据回）的行——这些就是「映射 miss → 裸 IP 走隧道 → 边缘不可达」的残留路径。统计这类行占比与对应时段。
- **验证**：裸 IP + firstByteMs=-1 大量出现 → 洞 B 实锤，主链路 DNS 拦截没覆盖到这些流量（来源：硬编码 DNS app / DoH / 第二 DNS）。
- **期望**：明确「哪些流量没进拦截 DNS」。修法候选：SERVFAIL 时记录日志并回源物理 DNS 做视图转换（或扩展 ip→domain 映射到物理解析结果，Convert 视图），或把 `relayUDP` 中 kind=dns 的 53 直连也纳入拦截（对任意目标 53 都走 HandleQuery）。
- **用什么**：`[debugdiag]`（tunnels.tsv）。

### 实验 4（P4，验证 UDP 边缘端口被 ISP 掐）端口/KeepAlive 对照
- **做什么**：① 查注册信息 edgeAddrs 是否含 443 端口候选（当前按注册端口顺序拨号，`dialAddr` L502-523 逐个试）；若有 443 候选，改拨号顺序优先 443。② 对照实验：`quic.Config`（masque.go L285-306）`KeepAlivePeriod=10s` / `MaxIdleTimeout=60s`——若运营商对高频小包 UDP 限速，KeepAlive 本身致会话不稳；试调大/关闭 KeepAlive 对照。
- **验证**：`tunnels.tsv` 批量死亡（`quic: transport closed` / write 失败）消失或频率下降。
- **期望**：若 443 边缘稳定而 4443 频繁被掐 → 端口优先级修复；若 KeepAlive 调大后死亡下降 → 保活策略问题。此为 v0.5.27 之后 AGENTS.md 交接第 3/4 条，依然未做。
- **用什么**：`[debugdiag]` + `[代码]`（dialAddr 端口排序、quicConfig 参数已有 seam，可宿主改 + 单测）。

### 实验 5（P5，隧道死亡触发的 TUN 侧方向判定）RST 来源判别
- **做什么**：debugdiag `tunnels.tsv` 的 err 列已带方向（v0.5.27 起 `up:EOF down:err` 格式）。对批量死亡瞬间的并发行分类：err 是 `read tcp <境外IP>:443: connection reset by peer`（从 gVisor conn 读到的 RST）还是 `write ... sendmsg`（Go 侧自己发不出）。
- **验证**：统计 down:err=reset by peer 的行是否与 `quic: transport closed` 同毫秒成对出现；若是 → 隧道死亡先于 RST（边缘断流传染下游），gVisor 只是转述边缘 RST，非 gVisor 自身误杀。
- **期望**：确认「RST 风暴 = 共享 QUIC 死亡的传染」，支撑修法集中在隧道活性（实验 4）而非 gVisor 栈。
- **用什么**：`[debugdiag]`。

### 实验 6（P6，生态级对照）官方客户端同时跑同网络 + 长连接保活
- **做什么**：用户同时装官方 1.1.1.1 WARP（Android）与本 APK，同一 WiFi 各开 5 分钟看页面；并抓 logcat 对比官方 QUIC 拨号端口/KeepAlive 行为。
- **期望**：若官方正常而本 APK 不行 → 缩小到本实现 QUIC 参数/边缘选择的差异（官方 warp-svc 用 tokio-quiche，其 KeepAlive/Idle 设置可从二进制读出对照 masque.go L292-305 的注释值）。
- **用什么**：`[adb]`（模拟器无法安装官方 VPN 的墙内行为，实为真机项）。

---

## 5. 最可能根因假设（按概率排序，供重构参考）

1. **（高）UDP/HTTP3 直连被 ISP 封锁/劣化** —— ADR 5「UDP 不走隧道」在 Android TUN 模式下把浏览器 QUIC:443、QUIC 应用全部物理直连（androidvpn.go L302-315 → relayUDP L350-409），不经 WARP。9 轮修复全在 TCP CONNECT 层，从未覆盖 UDP 直连面；debugdiag udp.tsv 的 kind=quic 设计即承认此泄漏。若用户环境对境外 QUIC 出境封锁，这一层永远修不好。**判别：实验 2（关 H3 复测）＋ udp.tsv quic 行统计**。
2. **（高-中）DNS 视图未统一：绕过系统 DNS 的流量拿本地视图 IP → 映射 miss → 裸 IP 走隧道边缘不可达** —— v0.5.24 确诊根因的未覆盖残留（dns.go 只拦 198.18.0.1:53；v0.5.25 日志实锤 `114.114.114.114:53` 直连泄漏；tunnels.tsv 若见裸 IP + firstByteMs=-1 即证据）。**判别：实验 3**。
3. **（中）隧道共享 QUIC 被 ISP UDP QoS 周期性掐断 + 恢复延迟** —— debugdiag 20260808 已确诊隧道批量死亡；v0.5.27 修了 socket 族与 dead 重连但用户网络对 4443 的 QoS 未变；KeepAlive/端口选择未对照。**判别：实验 1/4/5**。

## 6. 关键证据文件索引

- `gui/build/android/app/src/main/java/com/wails/app/WarpVpnService.java` L175-224（路由/DNS/establish/detachFd）、L86-95（protectSocket）
- `androidvpn/androidvpn.go` L82-140（TUN/gVisor 栈）、L176-299（NewConnectionEx）、L302-315（UDP 分发）、L350-409（relayUDP 直连）
- `androidvpn/decision.go` L130-161（resolveAction/direct 拨号）、L180-189（miss→proxy）、L96-105（decideTunnelTarget）、L111-120（udpKind quic/dns）
- `androidvpn/dns.go` L40（DNSInterceptAddr）、L114-186（HandleQuery，类型不匹配 drop 在 L152-162）、L62/215（TTL 不一致）
- `gui/androidbridge.go` L205-302（nativeStartVpn）、L312-490（startVpnKernel）、L445-463（运行期 ctx 切换）、L392-421（rollback current 守卫）
- `tunnel/masque.go` L285-306（QUIC 参数）、L502-523（udp4/6 + protect）、L597-620（currentConnection dead）、L1349-1410（establishCONNECT 重连）、L840-874（失败计数窗口）、L1242-1254（Read/Write→noteDeadStream）、L1478-1597（resolveTarget/ResolveDNS）
- `rules/default-rules.txt` L6（「兜底 direct」注释与代码 miss→proxy 矛盾——文档风险）
- `CHANGELOG.md` v0.5.13-27 各条目；`AGENTS.md` §6.5 未解决问题交接
- 环境限制：仓库 shallow（depth=1），无法 git 对照上游

## 7. 给重构方案的落地建议（从调研推论）

1. **第一步一定是向用户要 v0.5.27 复测 debugdiag 包**（AGENTS.md 交接第一条），先做完实验 0（同网络对照）。
2. 若实验 2 证实 UDP 直连假设 → Android 侧需引入 UDP-in-MASQUE（上游是否支持需查证；当前 ADR 5 是「UDP 不走隧道」的产品限制，重构需先改 ADR 还是接受限制改为「UDP 直连+文档明示」）。
3. 若实验 3 证实 DNS 洞 B → 扩展 53 拦截到任意目标 + 物理 DNS 回源视图转换；或对裸 IP 的 proxy 分支先做一次隧道内解析失败即报错（而非 hang）。
4. 文档口径统一：miss→proxy 语义写进 rules 模板与 AGENTS.md（疑点 2 残留风险 A）。
5. 把「验收 = 用户打开境外网站、warp=on」定为 Android 里程碑门（替代「tun0 增长」）。