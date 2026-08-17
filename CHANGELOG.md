# Changelog

本项目所有值得记录的变更。格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [v0.5.28] - 2026-08-17

### 修复（Android 外网打不开，阶段 10 — 连接退役风暴误杀在途流）

- **池成员连接被本地主动退役过频，每次退役连坐全部在途流 → 很多网站打不开**
  （真机 debugdiag 第二轮，东哥 2026-08-17 实测「YouTube 快了一点但许多网站
  还是打不开」）：da5115c 健康跳过缓解了「死成员阻塞解析」，但 6.25 分钟
  会话里仍有 59 条 QUIC 连接被本地 retire/reconnect 主动拆线（平均 ~6 秒拆
  一条），每条连接承载 4-23 条流**同毫秒全部连坐**（`use of closed network
  connection` / `quic: transport closed` 均为本地 `bundle.close()` 后流 IO
  失败，非远端错误）；394 条被拆流中 **79% 正在正常传输**（down>0、life
  p50=2.6s / p90=22.7s），说明退役是误杀健康流而非流自然死亡。
  - **根因（退役误判）**：`connectFailureRequiresReconnect` 携带
    `packetsBefore`（CONNECT 交换前 `receivedPackets()` 快照）参数但**从未
    使用**——「交换期间连接仍在新收 QUIC 包 = 路径健康」判定被丢。手机网络上
    个别目标（IPv6、慢节点、边缘拒绝）CONNECT 超时/RST 是常态，纯计数在
    30s 窗口累计 2 次即 retire 整条共享连接；退役→重建窗口（指数退避
    100ms→5s + 握手）内新导航 slow/failed → 浏览器超时 → `connection
    refused` 41 条（全部聚在退役风暴时段）。退役→失败→记窗→再退役形成
    自激循环。
  - **修复 1（恢复健康判定，核心）**：`tunnel/client_conn.go` 的
    `connectFailureRequiresReconnect` 在记观察窗前检查
    `b.receivedPackets() > packetsBefore`——交换期间连接在收包则本次失败
    纯属目标/单流问题，**不累计观察窗、不拆共享连接**；黑洞（连接真死，
    交换期间无任何新包）仍按观察窗累计拆线，v0.5.23 恢复语义保留。
  - **修复 2（首试立即重连）**：`runReconnect` 首次重试退避 100ms→0（失败后
    才从 100ms 指数退避），拆线后新连接尽快就位，缩短风暴窗口内导航等待。
    singleflight 保证不产生拨号风暴。
  - 新增 `TestConnectFailurePacketsDuringExchangeKeepsConnection`（本机真实
    QUIC 连接对验证「在收包不拆线」）+ `TestConnectFailureNoPacketsDuring
    ExchangeCountsWindow`（黑空调语义保留），`go build ./...` /
    `go test ./...` 全绿。
  - **待真机验收**：重测打不开的网站（B站/微软系/导航类）应显著恢复；fbm=-1
    占比与 refused 数应下降。

### 修复（Android 外网打不开，阶段 9 — 连接池死成员阻塞慢解析）

- **连接池轮询命中死成员 → DialTunnel/ResolveDNS 阻塞等待其重连，解析/首包
  慢**（真机 debugdiag 驱动，东哥 2026-08-17 实测反馈「部分网站解析半天打不开」）：
  连接池（阶段 8b）把 DialTunnel 与 ResolveDNS 轮询均分到 2 条 QUIC 连接。
  手机网络抖动/边缘掐线时某条连接被判死并进入后台重连（runReconnect 指数
  退避 100ms→5s），而 `openRequestStream`/`establishCONNECT` 对 dead 成员的
  默认行为是 join 重连航班**等待其完成**——池轮询到死成员的那一半请求
  （含 TUN DNS 拦截的每一次 DoH 查询）全被单个死成员拖到重连完成，真机表现
  为「解析半天」。debugdiag 铁证：5.4 分钟会话 416 条 TCP 隧道 0 条正常关闭，
  196 条在 ~10 次「同一毫秒 7-24 条集体 use of closed network connection」
  风暴中死亡（每次 = 一条 QUIC 传输拆线连坐全部在途流）；firstByte p50=446ms、
  p90=658ms，84 条从未收到首字节。
  - **修复（连接池健康优先轮询）**：`tunnel/client_conn.go` 新增
    `MasqueClient.EnsureServiceable()`（纯状态检查：不健康时**非阻塞**确保
    后台重建航班已启动并返回 false）；`core/pool.go` 的 `poolDialer` 轮询时
    跳过不可用成员、把请求交给立即可用的兄弟成员，被跳过成员后台自愈后
    重新入轮。全部成员都不可用（整网断）时回退首选成员正常拨号（join 航班
    等待，与单连接语义一致，保证总能等回连接）。**不推翻连接池叠加方案**：
    多连接按连接限速叠加的吞吐收益保留（真机 4M/s 实测），只消除死成员阻塞
    的延迟代价。
  - 新增 `TestPoolSkipsUnhealthy*`（死成员跳过 / 健康成员承接 / 全死回退 /
    自愈回轮）+ `TestEnsureServiceable*`（不健康触发后台自愈 / 已关闭安全
    false），`go build ./...` / `go test ./...` 全绿。
  - **待真机验收**：重测「解析慢」站点（Google/YouTube 系）应明显加速；
    QUIC 拦截（UDP:443 丢弃回退 TCP，约 100-300ms/首次导航/域名）为阶段 5
    起的设计取舍，若仍觉慢可后续加 ICMP port-unreachable 快速失败（另立任务）。

### 修复（Android 外网打不开，阶段 8 — 大响应/流媒体卡死，MTU 收窄）

- **小请求通、大流卡死 → 路径 MTU 黑洞 + QUIC 上行包越界**（真机 Device A
  实测驱动：curl 小请求 200，YouTube 标题能载视频不载、linux.do 转圈——
  即大响应/流式传输失败。真机 ping 实测物理路径 MTU≈1478（ICMP 1450 负载
  全通、1460 全丢），1500 的 TUN MTU 下 gVisor 推导 MSS=1460、直连 UDP 中继
  可回传 ≤1480 字节数据报，均越过 1478 上限撞 DF 静默丢包）。
  - **修复 1（QUIC 上行包钳制）**：`tunnel/client_conn.go` 共享 QUIC 连接
    `InitialPacketSize` 1350→1200（本端报文恒 ≤1228，任何 ≥1232 MTU 路径安全），
    `DisablePathMTUDiscovery=true` 杜绝 PMTUD 把包探到 quic-go 通告上限 1452
    （1452+28=1480 越过 1478 仍触发 DF 丢包）。下行仍受边缘自身 1350 上限
    约束，记录在案。
  - **修复 2（TUN MTU 收窄）**：`androidvpn`/GUI/Java 全链路 TUN MTU
    1500→1400（`DefaultMTU` 单一常量，Java `setMtu(1400)` 同步）——gVisor
    MSS=1360、UDP 中继 ≤1400，全部收进 1478 实测上限留余量。
  - 新增 `TestQuicConfig*`（包尺寸/流控窗口/路径 MTU 上限断言）+ `TestDefaultMTU`
    （1400 契约 + MSS < 1450 + MTU < 1478），`go build ./...` / `go test ./...`
    全绿。
  - **待真机验收**（东哥验收标准：真机浏览器打开 YouTube/linux.do 大流不卡）。

### 改进（Android 外网打不开，阶段 8b — 共享 QUIC 连接按连接限速，连接池）

- **单条共享 QUIC 连接被网络按连接限速 ~1Mbps，浏览器多流均分 → 大流饿死**
  （真机 Device A 实测驱动，阶段 8 的剩余瓶颈。curl 单流独占 0.75MB/s 能下
  完 5MB/25MB，浏览器几十条并发流被均分后每条只剩 ~15KB/s → 视频永远转圈、
  linux.do 永不进；判定依据——同机同会话 1 条 / 10 条 / 20 条并发下载的总
  吞吐恒定 0.75 / 0.99 / 0.74 MB/s：总上限与并发数无关、只按流均分，指向
  **单连接（单个 5 元组）被限速**而非全局带宽或单流流控）。
  - **修复（QUIC 连接池）**：`core/pool.go` 新增 `poolDialer` + 边缘表旋转
    `rotateEdges`，`core/kernel.go` 的 `NewKernelContext` 按 `core.Config.
    TunnelConnections`（默认 2，`config.json` 可配）建多条 QUIC 连接，轮询
    分发 `DialTunnel`/`ResolveDNS`，单条连接失败自动换下一条。每条连接复用原
    MasqueClient 自愈机制（各自重连/探针/DoH）；边缘表按连接序号左旋，让不同
    连接落在不同边缘/端口（不同 5 元组），各自拿独立限速额度。
    **无回归下限**：单连接时透传原行为；若瓶颈为全局带宽则总量不变、若为按
    连接限速则总量随连接数叠加（真机 A/B 验收）。
  - 新增 `TestPool*` 单测（轮询均分 / 失败换下一条 / 全败返回末错 / ctx 取消
    早停 / 单连接透传 / Close 全关 / DNS 轮询 / 边缘旋转 / 连接数解析），
    `go build ./...` / `go test ./...` 全绿。
  - **待真机验收**（TunnelConnections=2 的 APK：再测单 vs 10 并发总量，
    预期 >1Mbps；浏览器大流显著提速）。

### 修复（Android 外网打不开，阶段 7 — IPv6 裸 IP CONNECT 洞）

- **AAAA 查询泄漏物理 DNS → 本地视图 v6 IP → 裸 v6 走隧道 CONNECT 挂死**
  （Device A/B debugdiag 对照驱动）：TUN DNS 拦截对 AAAA 查询拿到 v4 时
  `return nil` 丢弃查询 → Android DNS 客户端超时后回退物理 DNS，拿到**本地
  视图** v6 IP（如被封锁/污染的 `2606:4700::6810:7b60`）→ IP→域名映射 miss
  （该 IP 从未经隧道 DoH 解析）→ proxy 分支裸 v6 IP 走隧道
  （`client_dns.go` L43-47 直通）→ WARP 边缘不可达——Device A（A15 双栈）
  CONNECT hang 到 deadline（`firstByteMs=-1` 行 v6=23），Device B（A14）
  边缘快拒（connection refused，v4 回退掩盖）。b99bcd1 已净除共享 QUIC 风暴，
  此为剩余第一嫌疑。
  - **修复 1（防泄漏，治本）**：`dns.go` 地址族不匹配分支（AAA 查询拿到 v4）
    由 `return nil` 改为回 **NOERROR 空应答**（新增 `noData`，权威声明"该类型
    无记录"）。Android 认定无 AAAA、不再回退物理 DNS，直接用 A 查询的 v4 IP
    （隧道 DNS 解析出，边缘必然可达）。
  - **修复 2（快拒兜底，治标）**：`decision.go` 新增 `shouldRejectBareV6`
    纯函数 + `errBareV6Proxy`：proxy 分支的裸 IPv6 字面量（映射 miss）本地
    立即 RST 拒绝，不再发 CONNECT 到边缘——把 A15 的挂死/边缘快拒统一成
    本地瞬时 connection refused，客户端 Happy Eyeballs 快速回退 v4。兜住修复 1
    作用域外的残余（已缓存的污染 v6、应用自带解析器/硬编码 v6 IP、映射过期）。
    隧道 DNS 解析出的 v6（映射命中）走域名不受影响。
  - 新增 `TestDNSInterceptorAAAANoV6Leak`、`TestShouldRejectBareV6`，更新
    `TestDNSInterceptorQueryTypeFilter`，`go build ./...` / `go test ./...` 全绿。
  - **待真机验收**（东哥验收标准：真机打开境外网站 + debugdiag 无 v6 裸 IP
    挂死/快拒行）。

### 修复（Android 外网打不开，阶段 6 — 隧道重连自伤）

- **共享 QUIC 连接被自身健康逻辑反复拆毁 → 境外流批量殉葬**（debugdiag
  20260816 数据驱动）：`tunnels.tsv` 72s 内 8 条本地 UDP socket 代际（间隔
  3s/14s/10s/12s/10s/1.8s/10s），每轮死亡把当时所有在途境外流一起拖死——
  错误签名统一为 `down:quic: transport closed: ... use of closed network
  connection`（本地 `bundle.close()` 拆线的产物，不是运营商掐线）。
  触发源：运行期出口探针单次失败即 `retire`（20s 周期，零容忍）、单条流
  非连接级错误即 `noteDeadStream` 拆整连接、单目标 CONNECT 非超时失败即
  立即重连——手机网络 UDP 抖动 / 边缘对映射 miss 裸 IP 目标的立即重置，
  任一条都足以把几十条健康并发流一起掐死，且重连后同一抖动再触发，永不收敛。
  - **修复**：拆线判定全部窗口化 + 类别化——
    - **运行期探针**：连续 `probeFailureThreshold`(2) 次失败才 `retire`，
      单次毛刺不再拆共享连接（真实黑洞仍有 CONNECT 失败窗口兜底）。
    - **`noteDeadStream`**：新增 `isConnectionLevelError` 类别判定——quic
      `TransportError`/`IdleTimeoutError`/`ApplicationError`/`StatelessResetError`
      （连接本身已死）仍立即重连；裸 `net.ErrClosed`（共享连接已被他人
      retire/换代/关闭，本条流只是被拖累）跳过，不再让每一条垂死流各触发
      一轮恢复；其余（对端 reset 等单目标/单流问题）走新观察窗
      `noteStreamFailure`，窗口内累计 2 次才判定连接死亡。
    - **`connectFailureRequiresReconnect`**：非超时非连接级错误（如边缘对
      单个目标立即 reset）由"单次即重连"改为计入 CONNECT 观察窗；裸
      `net.ErrClosed` 不再计入窗口。
    - **可观测性**：`connBundle.close(reason)` 补日志 `QUIC 隧道连接关闭：<reason>`
      （此前拆线原因全部静默丢弃，批量死亡无法归因是谁先动手）。
  - 新增/更新 10 项单测（探针阈值、`net.ErrClosed` 语义、非连接级错误观察窗、
    TransportError 立即重连等），`go build ./...` / `go test ./...` 全绿。
  - **待真机验收**（东哥验收标准：真机打开境外网站 + warp=on+debugdiag 批量
    死亡消失）。

### 修复（Android 外网打不开，阶段 5 — QUIC:443 拦截）

- **UDP:443 (QUIC/HTTP3) 直连泄漏**（v0.5.13→v0.5.27 九轮未解根因）：
  浏览器 HTTP/3（QUIC:443）走 UDP 直连路径（`relayUDP` → 物理网络），
  运营商封锁 UDP/QUIC 直连 → 浏览器外网打不开。九轮修复全在 TCP CONNECT
  层打转（DNS 拦截、IP→域名还原、SERVFAIL 回退），从未触碰 UDP 直连面。
  上游 warp-svc 只有 `ConnectTcpProxy`，不支持 CONNECT-UDP（RFC 9298），
  UDP 无法走 WARP 隧道。
  - **修复**：在 TUN 栈 `NewPacketConnectionEx` 拦截 UDP:443，丢弃包让
    浏览器回退 HTTP/2 over TCP:443 → `NewConnectionEx` → WARP 隧道 → 通。
    Chrome/Firefox 对 QUIC 失败的标准回退行为保证此方案有效（QUIC 探测
    超时后立即 TCP fallback，延迟约 100-300ms）。
  - 新增 `shouldBlockUDP(port uint16) bool` 纯函数（host-compilable，
    可单测），`NewPacketConnectionEx` 调用它判定。
  - DNS:53 拦截路径不受影响（在 QUIC 拦截之前返回）；非 443 UDP 直连
    不受影响。
  - **待真机验收**（东哥验收标准：真机打开境外网站 + warp=on）。

### 重构（契约层，阶段 2）

- **bindings 单源化**：`gui/frontend/src/lib/types.ts` 从手写防御性 normalizer（`unknown` → 猜测字段名）重构为 Wails 生成类型 → UI 类型的编译期安全适配层。删手写 `num()`/`str()` 猜测逻辑，改用生成的 `BackendStatus`/`BackendConfig`/`BackendStats` 等类型——Go 改字段时 `tsc` 编译期失败而非运行期静默错。
- **`route.Stats` 补 json tag**：字段加 `json:"proxy"/"direct"/"rejected"/"miss"` tag，与前端 `ProxyCounters` 键对齐，生成 bindings 时 TS 属性名即为此 tag。
- **`geoBaseURL` 死字段清理**：`AppConfig.geoBaseURL` 和 `logDir` 前端字段删除（后端无对应字段）；`gui/service.go` 的 `GetGeo()` 改为从 `st.Config.GeoRepo` 动态构建 BaseURL（替代硬编码）。

### 重构（核心隧道层，阶段 3）

- **拆分 masque.go（2218 行）→ 5 个职责文件**：`client_conn.go`（连接管理/重连/探测/健康判定）、`client_doh.go`（DoH 解析）、`client_socks5.go`（SOCKS5 代理）、`client_dns.go`（DNS 拦截）、`masque.go`（占位）。
- **退役双份 UDP**：`tunnel/udp.go`（344 行）退役，UDP 逻辑合并到主流程；`masque_socks5_route_test.go`（260 行）退役。
- **geosite 匹配索引**：新增 `route/geoindex.go`，后缀 map + 精确 map 把线性扫描 O(N) 加速到 O(标签数)；`route/matcher.go` 改用 geoindex。
- 净减 ~2854 行。

### 重构（GUI 架构，阶段 4）

- **共享 hooks 消轮询**：新增 `usePoll`（共享轮询，自动清理+alive 守卫）和 `useAsyncAction`（统一 busy/error/notice 三件套），StatusPage/LogsPage/RulesPage 改用共享 hooks 消除重复轮询逻辑。
- **新增 `codeMirror.ts`**：规则编辑器语法高亮准备。
- **ClearLogs 修复**：`gui/logs.go` 加 `ringLogger.Clear()`，修复"清空按钮只清前端 state，轮询下一帧旧日志又回来"。
- **GEO LastChecked 修正**：`gui/service.go` 用 GEO 文件 mtime 替代 `time.Now()`（后者无信息量）。

### 重构（CI/发布纪律，阶段 1）

> ⚠️ **2026-08-08 用户反馈：v0.5.27 真机复测仍失败（境外流量打不开），已决定放弃继续修复。**
> 下方 v0.5.27 的修复属**净改进但非根治**；完整证据链、已排除方向与接手实验清单见
> AGENTS.md §6.5「未解决问题交接（2026-08-08）」。

### 修复（Android 外网打不开，debugdiag 数据驱动）

- **隧道共享 QUIC 连接反复死亡后恢复慢 → 境外流量 connection reset**（最新
  debug 包 `tunnels.tsv`：42s 内 3 次批量死亡，`network is unreachable` 33 次
  全来自 `[::]:X` 双栈 socket 发往 IPv4 边缘 `162.159.198.2:4443`；隧道被掐
  瞬间同一连接上所有并发境外流 `read tcp <境外IP>:443: connection reset by
  peer` 且 dn=0，浏览器"打不开外网"）。
  - `connBundle` 新增 **`dead` 标志**（并发安全）：`noteDeadStream` / 运行期
    探测观测到连接级故障即置位，`currentConnection` 与 `establishCONNECT`
    立即把后续请求加入重连航班——**消除死连接上 10s×2 CONNECT 白等**（此前
    quic.Context() 在黑洞路径下未 Done，新请求仍叠在死连接上反复超时）。
  - **`dialAddr` socket 地址族收紧**：`net.ListenUDP("udp")`（双栈，`[::]`）
    改显式 **`udp4`/`udp6`**——"udp" + IPv4-mapped 地址把 IPv4 目标路由进
    IPv6 路由表，无可用 IPv6 的主机内核报 `ENETUNREACH`（debugdiag 33 次同源）；
    专用 udp4 socket 正常出网。`scanner/probe.go` 同步。
- **拨号时国际出口探测**（`probeInternationalEgress`）：候选边缘 H3 SETTINGS
  就绪后先做一次到 `8.8.8.8:443` 的隧道内 CONNECT，失败即换下一个边缘——
  排除"握手成功但国际出口被掐"的坏边缘（上一会话遗留的未完成功能，顺带修复
  编译失败）。探测直接在传入 bundle 上开流，不触碰 reconnect（避免初始拨号
  `c.cur` 未安装时的无限递归）。
- **运行期活性探测**（`egressProbeLoop`，20s 周期）：静默死会话（KeepAlive
  往返仍在但出口已坏/路径被掐）由周期探测发现并主动 retire+重连，恢复从
  "下一次用户 CONNECT 超时（10s×2）"提前到 20s 内。`probeFn` seam 供单测注入。

### 修复（CI/发布纪律，版本单源漏出口）

- **sync-upstream 冲突预检测 regex 失效（预检测死代码）**：经典 3 参
  `git merge-tree` 的冲突标记带 `+` 前缀（`+<<<<<<<` / `+>>>>>>>`），旧 regex
  `^(<<<<<<<|>>>>>>>)` 永不命中 → 预检测恒判"无冲突"提前 return，冲突文件提取成
  不可达死代码（真实 `git merge` 守卫仍安全，仅"提前中止、避免开 PR 才发现"的优化
  失效）。修复：版本门控（git ≥ 2.38）优先用 `git merge-tree --write-tree`（冲突时
  退出码 1，输出含 `CONFLICT` 行，无歧义）；老版本 git 回退经典 3 参形式并修正 regex
  为 `^\+?<<<<<<<|^\+?>>>>>>>`。本地构造真实冲突/无冲突 fork 仓库 4 场景
  （write-tree × 冲突/无冲突、legacy × 冲突/无冲突）验证判定与退出码全部正确。
- **Windows CLI PE 版本资源恒为陈旧 `0.5.3`（版本单源漏出口）**：根
  `versioninfo.json` StringFileInfo 写死 `0.5.3`，CI sed 找 `0\.0\.0\.0` 永不命中 →
  每次 tag 发版 Windows CLI 的资源管理器"详细信息"版本恒 0.5.3（`-X main.version`
  注入仍正确，仅 PE 资源陈旧）。修复：改回 `0.0.0.0` 占位符（与
  `gui/versioninfo.json` 一致），恢复 CI sed 命中。
- **Docker 镜像内二进制恒为 `dev`（版本单源漏出口）**：Dockerfile 构建无
  `-X main.version` 注入 → GHCR 镜像 `warp -version` 恒 dev。修复：Dockerfile 增加
  `ARG VERSION=dev` + `-ldflags "-X main.version=${VERSION}"`；docker-ghcr 工作流从
  `github.ref_name` 提取 tag 版本（main 分支回退 dev）经 build-args 注入。

### 变更（CI 构建缓存与并发）

- **Go/npm 依赖缓存**：build-release（test / build-binary / build-gui /
  build-android）4 处 setup-go 显式 `cache: true`（依赖模块缓存，go.sum 不变则复用，
  省每次 tag 全量重编依赖）；build-gui / build-android 与 android-debugdiag 的
  setup-node 加 `cache: 'npm'` + lock 文件路径。
- **test job 去掉冗余全量编译**：`go vet ./...` 与 `go test ./...` 已覆盖编译，
  删除独立的 `go build ./...`（少一遍全量编译）。
- **构建并发取消**：build-release / docker-ghcr 加 `concurrency` group（按 ref），
  防 force-push 同 tag 或连续 dispatch 重叠构建；android-debugdiag 同加。

### 测试

- 新增 5 个单测：dead 置位快速重连 / currentConnection dead 检出 / 探测
  nil 守卫 / 探测失败触发重连 / 无连接跳过探测。

## [v0.5.26] - 2026-08-07

### 调试设施（debugdiag，`-tags debugdiag` 构建）

- **`build tag` 门控的调试数据收集器**，用于诊断"Android 无法访问外网"但所有 CONNECT
  隧道均已建立的问题。启用：`-tags debugdiag`；**正式版（CI 用
  `-tags production,android,with_gvisor`，不带 debugdiag）编译 `androidvpn/
  debugdiag_stub.go` 的 no-op stub**——零 IO、零内存、零磁盘、零网络，
  release 构建不携带任何调试代码（`androidvpn/debugdiag.go` 不被编译）。
- **payload 层字节计数**（`debugdiag/tunnels.tsv`）：每个关闭的 TCP 隧道一行
  `time seq host upBytes downBytes firstByteMs lifeMs err`——`firstByteMs` =
  会话开始到首个下行字节的毫秒数（`-1` 表示未收到任何下行数据 = **CONNECT 成功
  但数据没有流回**，即本特性的关键诊断信号）。
- **UDP 直连量化**（`debugdiag/udp.tsv`）：每个关闭的 UDP 直连中继一行
  `time host kind bytes err`，`kind` = `dns`（端口 53 未被拦截的漏直连）|
  `quic`（端口 443 浏览器 HTTP/3 直连泄漏）| `udp`。
- **tun0 采样**（`debugdiag/tun0.tsv`）：每 2s 采样一次 tun0 rx/tx 字节计数，
  每行 `time txBytes deltaTx rxBytes deltaRx`——区分"隧道已建立但 payload 死
  活"与"完全无流量"。
- **生命周期**：VPN 启动时 `androidvpn.DebugSetDir(root)`（`<沙箱根>/debugdiag/`，
  Android = `getFilesDir()/debugdiag`）；VPN 停止/回滚时 `androidvpn.DebugStop()`。
- **导出**：停止时 Go 经反向 JNI 调 `MainActivity.exportDebugDiag()`，把
  `debugdiag/` 打 zip 到 MediaStore Downloads 为
  `warp-go-debugdiag-<timestamp>.zip`（API 29+），URI 打到 GUI 日志页。
- **CI 构建**：新工作流 `.github/workflows/android-debugdiag.yml`
  （workflow_dispatch）用 `-tags production,android,with_gvisor,debugdiag` +
  `assembleRelease` 构建 APK，上传 artifact `warp-android-debugdiag`
  （versionCode 按 ref 派生，可覆盖安装 v0.5.25+）；沿用 `warp-release.p12`
  签名——**覆盖安装不丢 reg.json**。

## [v0.5.25] - 2026-08-06

### 修复

- **Android v0.5.24 回归：国内 direct 连接全部失败**（真机日志
  `拨号失败 49.7.252.24:443：lookup obus-cn.dc.heytapmobi.com: canceled`）。
  根因：v0.5.24 的 IP→域名还原在 `NewConnectionEx` 里**无条件**应用——direct
  分支也被还原成域名，`net.Dialer` 做物理解析 → 系统 DNS 又进 TUN → 环路
  canceled。修复：新增 `decideTunnelTarget` 纯函数，**域名还原只用于 proxy
  分支**（隧道 `DialTunnel` 收到域名内部再次 DoH 解析，CONNECT 目标永远边缘
  可达——v0.5.24 根因修复的正确路径）；direct 分支保留原始 IP 拨号（该 IP
  是隧道 DoH 解析出的真实 IP，物理网络同样可达）。3 个回归用例锁定契约：
  proxy+映射→域名 / direct+映射→原始 IP / proxy 无映射→原始 IP。
- **Android DNS 拦截解析失败静默丢弃 → 系统挂起/fallback 裸 IP**（v0.5.24
  真机日志 `DNS 拦截：nebula-api-cn.heytapmobi.com 解析失败：没有 TypeA
  记录` + `UDP → 114.114.114.114:53（直连）` + `[2001::1]:443 CONNECT 超时`）。
  根因：隧道 DoH（162.159.36.1）对部分域名无 A/AAAA 记录时 `HandleQuery`
  返回 nil **drop**——Android DNS 挂起直到查询超时，或 fallback 到物理 DNS
  （114.114.114.114）返回本地视图 IP → IP→域名映射 miss → 裸 IP 走隧道 →
  边缘不可达。修复：解析失败返回 **SERVFAIL 响应**（保留原 Question/ID/
  OpCode，无 Answer），Android 立即回退下一个 DNS，行为与非拦截时一致。

## [v0.5.24] - 2026-08-05

### 变更

- **取消 config.json 文件热加载**（用户需求）：`core/config.go` 的
  `WatchConfig`/`configFileState`/`configPollInterval` 全部移除，
  `core/core.go` 的启动 WatchConfig 协程与 `applyConfigReload` 删除，
  `Server` 结构去掉 `stopWatch` 字段，`core/config_test.go` 删 3 个
  WatchConfig 测试。配置只在启动/显式保存时读取，运行中修改需重启生效。
  根因：热加载每 2s 轮询回读磁盘，GUI 保存后被 `applyConfigReload` 用磁盘
  值覆盖刚保存的快照 → "GUI 改配置被自动重置"。**规则文件（rules.txt）热
  重载保留**（独立功能，用户未要求取消）。

### 修复

- **Android 无法访问外网（隧道 CONNECT 目标 IP 边缘不可达）**：决定性实验
  （`TestIPEdgeProbe`，真实边缘 + 用户 reg.json）确证——WARP 边缘 CONNECT 的
  目标 IP 必须处于**边缘网络视图**：隧道内 DoH 解析出的 facebook IP
  （57.145.12.1）CONNECT 成功；Android 系统 DNS 解析出的同一域名 IP
  （69.171.235.22）CONNECT hang 到 deadline（`http3: parsing frame failed:
  deadline exceeded`，与用户日志一字不差）。域名路径成功的根源是
  `resolveTarget` 用隧道内 DoH 解析（天然拿到边缘可达 IP）；TUN 只收到系统
  DNS 的 IP → `DialTunnel("IP:443")` → 边缘连不到 → 全挂。修复：`tunnel`
  导出 `ResolveDNS`（隧道内 DoH）；新增 `androidvpn/dns.go` DNS 拦截服务器
  （sing-box 标准架构）——拦截 UDP:53 → 隧道 DoH 解析 → 返回边缘可达真实 IP
  并记录 IP→域名映射 → `NewConnectionEx` 查表还原域名走 `DialTunnel`。9 个
  宿主单测覆盖 A/AAAA 查询、类型过滤、映射表、过期清理。**接线完成**：
  `core.Kernel` 新增 `ResolveDNS`（委托隧道拨号器，`dialer` 接口扩展 +
  回归测试）；`androidvpn/dns.go` 导出 `DNSInterceptAddr`（198.18.0.1，
  RFC 2544 保留段）；`WarpVpnService.java` `addDnsServer("198.18.0.1")`
  把系统 DNS 指向 TUN 内拦截服务器；`NewPacketConnectionEx` 对
  `198.18.0.1:53` 的 UDP 查询走 `handleDNSQuery`（HandleQuery 响应写回，
  解析失败静默丢弃、Android 回退下个 DNS）；`NewConnectionEx` 对 TCP 目标
  IP 查 IP→域名映射，命中则用域名走 `DialTunnel`（CONNECT 目标永远边缘
  可达）。
- **主题模式持久化不生效**（用户反馈）：根因链有二——① `useTheme` 的
  effect（`[mode,systemDark]` 依赖）在挂载与 OS 主题切换时用默认 `"system"`
  无条件写回 config.json，覆盖用户持久化的 theme_mode；② 主题只在
  SettingsPage 挂载时读取，App 壳不读 → 启动恒 system。修复：
  `useTheme.ts` 重写——mount 时从 config.json 读取持久化主题并应用；只在
  用户显式点击主题按钮（`setMode`）时持久化（localStorage + config.json
  theme_mode），effect/OS 事件永不写文件。
- **GUI 改配置被自动重置**（用户反馈）：主因即热加载回写（见上）；辅因
  `saveConfigPartial`（useTheme 触发）会 `getConfig()+saveConfig({...current,
  ...patch})` 整链覆盖。取消热加载 + 移除 effect 内自动写回后根治。
- **设置页文案同步**：保存提示由"文件变更将触发热重载"改为"重启后生效"，
  说明文字同步更新。

## [v0.5.23] - 2026-08-05

### 修复

- **Android 开启后无法访问外网（隧道黑洞后永不重连，v0.5.21 回归）**：用户日志
  `H3 CONNECT 69.171.235.22:443 失败：读取 CONNECT 响应失败：http3: parsing frame
  failed: deadline exceeded` 反复出现但**从不触发重连**（境内直连正常、境外全
  超时，重启应用才恢复）。根因：v0.5.21 把 CONNECT 超时的重连判定改为"**窗口内
  3 个不同目标**失败才重连"，用 `map[string]struct{}` 去重——浏览器对少数站点
  并发重试时**同一目标反复失败在 distinct 去重下永不累计**（用户日志 2 目标 ×
  各 2 次 = distinct 2 < 3），QUIC 连接进入黑洞态后外网永久不通。修复：
  `noteProgressingCONNECTFailure` 改为**计数语义**（`map[string]int`，窗口内
  累计失败次数），`connectFailureTargets` 从 3 收紧到 2；单目标首次失败仍不
  重连（保留 v0.5.21 的"保护共享连接"场景），同/异目标窗口内累计 2 次即触发
  `establishCONNECT` 的 retire + 重连恢复。新增回归测试
  `TestConnectFailureSameTargetTwiceTriggersReconnect`（同目标第 2 次失败触发
  重连）、`TestConnectFailureSuccessResetsWindow`（成功 CONNECT 清空失败窗口）
  及更新 v0.5.21 窗口测试。
- **Android 每次构建的 APK 签名不一致（覆盖安装必须卸载重装）**：CI
  `assembleRelease` 无 keystore 时兜底用 **debug keystore**，GitHub runner 每次
  现生成 → 每次构建签名不同。修复：仓库内置固定 `gui/build/android/app/
  warp-release.p12`（openssl 生成 PKCS12，密钥与密码随仓库公开——仅保证覆盖
  升级签名一致，非 Play 商店上传密钥），build.gradle `release` signingConfig
  默认引用它，设 `ANDROID_KEYSTORE_*` 环境变量可覆盖为正式 keystore。

## [v0.5.22] - 2026-08-05

### 修复

- **Android 状态页"启动时间不显示 + 流量统计无变化"**：`Service.GetStatus` 的
  Android 分支只覆盖了 `State`/`LastError`，而 `StartTime`/`Stats`/`RulesCount`
  仍来自 `Server.Status()`——但 **Android 上 `Server.kernel` 永不启动**
  （SOCKS 内核在 Android 由 VpnService 驱动，真实内核在 `androidRuntime.kernel`）。
  于是 `StartTime` 恒为零值（Server 从未 Start）、`Stats` 全 0（`s.kernel` 为
  nil）、`RulesCount` 恒 0。用户误判为"内核没真正运行"（实际 tun0 在转发，只
  是状态页数据源脱节）。修复：`core.Kernel` 新增 `Stats()`/`Rules()`/`StartedAt()`
  访问器；`androidRuntime` 记录装配成功时刻 `startTime`；GetStatus Android 分支
  从 `androidRuntime.kernel` 读真实统计与规则数、从 `androidRuntime.startTime`
  填启动时间。新增 `TestKernelStatsAccessors` 回归（proxy/direct/miss 命中计数、
  未 Start 时 StartedAt 零值）。

## [v0.5.21] - 2026-08-05

### 修复

- **Android 开启后无网络（单目标 CONNECT 超时污染共享连接，v0.5.20 回归）**：
  真机日志 `HTTP/3 CONNECT 交换失败（读取 CONNECT 响应失败：http3: parsing
  frame failed: deadline exceeded），淘汰当前连接并重连` + `[tun] 拨号失败
  [2001::1]:443 ... use of closed network connection`。根因：模拟器物理网络
  **IPv4-only**（wlan0 无全局 IPv6、`ip -6 route default` 空），app 发往 IPv6
  目标（如 `[2001::1]:443`）经 TUN 全量路由进隧道后 CONNECT 超时。而
  `tunnel.connectFailureRequiresReconnect` 对"超时 + 交换期间无新包"**立即判定
  路径黑洞** → `retireConnection` 关闭共享 QUIC 连接 → 其他并发流撞上关闭的
  socket → `use of closed network connection`。但 `connectExchangeTimeout`
  （10s）与 `KeepAlivePeriod`（10s）同量级——健康但不可达的目标同样表现为
  "超时无新包"，无法据此区分目标级失败与路径黑洞。修复：超时统一走
  **failure-window**（`noteProgressingCONNECTFailure`，30s 窗口内 3 个**不同**
  目标才触发重连），单个不可达目标只 fail 自己的流、共享连接保留；真正路径
  黑洞（多目标陆续超时）仍能检出。新增 4 个回归单测（单目标不重连/同目标不
  累计/3 不同目标重连/窗口过期重置）。
- **注（方案 C 澄清）**：Android 边缘已是 IPv4（`ResolveEdgeAddrs` 无旗标回落
  `"4"`→`162.159.198.2`），`[2001::1]:443` 是**目标**而非边缘；本地连 IPv4
  边缘、无法预判边缘对目标的可达性，故"本地地址族过滤边缘候选"不适用——
  failure-window 已决定性修复单目标污染，无需单独地址族过滤。

## [v0.5.20] - 2026-08-05

### 修复

- **Windows/macOS/Linux GUI 日志页不自动刷新**：日志页每秒轮询后端，但
  `LogsPage.tsx` 的去重逻辑只比较批次**长度**——一旦日志达到环形缓冲上限 200，
  新日志顶替最旧条目而长度不变，之后内容持续变化但页面不再刷新（必须点清空
  或切页才更新）。修复：改为比较**尾条**（time+level+msg），无变化时不触发
  重渲染、有变化即刷新；抽出纯函数 `logsTailChanged` + 8 个回归单测
  （含"200 上限后长度不变但尾条变化必须刷新"关键场景）。
- **Android 开启后无网络（装配 ctx 泄漏进运行期）**：真机日志 `[tun] 拨号失败
  ...：H3 CONNECT ... 失败：... make udp [::]:60687->...:443: use of closed network
  connection`。根因：`startVpnKernel` 把带 60s 拨号超时的装配 ctx 继续传给
  `kernel.Start(ctx)`/`vpn.Start(ctx)` 作**运行期生命周期 ctx**——移动网络下拨号
  接近 60s 时（或装配超时到期）sing-tun 栈随 ctx 取消整体关闭，但 `started` 仍
  true（Start 对 ctx.Done 返回 nil 不触发 rollback）→ 用户看到"VPN 开"但 TUN 已死，
  后续连接在共享 bundle 上撞上已关闭的 UDP socket。修复：装配完成后切换独立的
  运行期 ctx（background 派生，仅由 nativeStopVpn 的 cancel 控制），装配计时器不再
  约束运行；ctx 身份校验 + 状态写入 + 运行期 ctx 替换合并进同一临界区（消除
  nativeStopVpn 插入竞态）；rollback 增加 `current` 守卫（过期实例失败不再误拆新
  实例的 Java 服务）。新增 `TestKernelStartRuntimeCtxCancelKeepsKernel` 回归（运行期
  ctx 取消后 kernel 仍可用、拨号器不关闭）。
- **默认规则补 `proxy,geoip:google`**：让 8.8.8.8 等 Google IP 走隧道（与 telegram
  规则同模式），`TestDefaultRulesParses` 更新为 9 条。

### 新增

- **桌面 TUN 模式可行性评估**：`docs/tun-desktop-feasibility.md`——复用 sing-tun
  v0.8.11 + `androidvpn/decision.go` 做桌面 TUN（system/mixed 栈、AutoRoute 按平台、
  wintun DLL 内嵌、DNS 劫持），比透明代理方案更稳（Windows 透明代理有 Wi-Fi
  fast-path 硬失败）；macOS 先 raw-utun + admin，透明代理只作 Linux 补充。
- **主题选择持久化到 config.json**（`theme_mode` 字段，默认 `system`）：设置页选择
  主题后写入 config.json（`saveConfigPartial`），重启 GUI 恢复上次选择（此前只存
  localStorage，切运行目录即丢）。

## [v0.5.19] - 2026-08-04

### 修复

- **Android 无法访问互联网（国外流量直连被墙）**：真机日志页显示 `[tun] 拨号失败
  172.217.x:443 / 74.125.x:5228：dial tcp ... i/o timeout`（全是 Google IP，走了
  direct 直连），浏览器打开即超时。根因：TUN 收到的是**已解析的 IP 字面量**，
  `route.Engine.Match` 对 IP 只走 geoip 规则（geosite/domain 的域名语义对 IP 无
  意义）→ 国外 IP 不命中默认规则里唯一的 geoip-proxy（telegram）→ **miss →
  兜底 direct** → 直连被墙。VPN 语义本应是"除显式 direct/reject（私有段、中国
  大陆）外全部走隧道"，故未命中兜底改为 **proxy**（`androidvpn.decideAction`
  miss→proxy）。桌面 SOCKS 拿域名走 route.Engine（其 miss→direct），不经过此
  函数，不受影响。测试 `TestDecideAction` T4/T6 同步更新。
- **Android 停止按钮部分失效**：真机证据——tap 停止后 logcat 无 `onDestroy -
  stopping VPN`、tun0 持续增长。根因：VpnService 被系统 VpnManager 绑定
  （ConnectionRecord）+ 前台服务，`stopSelf()` 可能不触发 `onDestroy`，而
  `WarpVpnService.stop()` 只 stopForeground+stopSelf 依赖 onDestroy 才拆 Go 内核。
  修复：`stop()` 直接调 `stopNativeAndClose()`（拆 Go 内核 + 关 TUN），幂等
  （nativeRunning 守卫），与 onDestroy 双拆除安全。另：`androidRequestVpnStop`
  桥未就绪时不再静默 no-op，改打日志（与启动路径一致），让"点停止没反应"可排查。

## [v0.5.18] - 2026-08-04

### 修复

- **Android 浏览器不通 + 停止按钮失效（direct 环路风暴，v0.5.14 修复不完整的下一层）**：
  模拟器实测 tun0 TX **33.9GB / 5 亿包**、app CPU **456%**、系统负载 12+——direct
  （非隧道）TCP/UDP socket 未豁免出 VPN 路由，包从 Go 进程发出后重新进入 TUN，
  形成环路风暴。风暴打爆 UI 线程 → 点"停止"的 tap 事件饿死（看似按钮失效）、
  浏览器访问全部超时。
  - **根因**：v0.5.14 只给 QUIC 拨号 socket（`tunnel.dialAddr`）加了
    `VpnService.protect`；`androidvpn` 的 direct 直连 socket 没 protect——
    TCP 走 `net.Dialer`（decision.go）、UDP 走 `net.DialUDP`（relayUDP）。
    默认规则 `direct,geoip:cn` + GEO 库缺失 → 流量全落 direct → 全环路。
  - **修复**：`androidvpn` 新增包级 `SetSocketProtector`（与 tunnel 同模式）：
    TCP direct 用 `Dialer.Control` 在建连前 protect fd，UDP relay 用
    `SyscallConn` protect；`gui/androidbridge.go` 同时注册到 tunnel 与 androidvpn
    两个包（复用 `androidProtectSocket`）。
  - **验证**：`go build/vet/test ./...` 全绿；`GOOS=android CGO_ENABLED=0 go
    build ./...` 通过；真机（模拟器）行为待部署新 APK 复测（CPU 回落 + tun0
    计数不再暴涨 + 浏览器可通 + 停止按钮生效）。

- **GEO 页看不到数据库更新时间**：`GetGeo` 用 `os.Stat(geoDir).ModTime()`，但
  GEO 数据从未下载时 `geo/` 目录为空 → `os.Stat` ErrNotExist → `GeositeUpdated/
  GeoIPUpdated` 为空 → 前端显示"—"，且 StatusPill 误标"已就绪"。修复：未下载
  时前端显示"未下载"徽标 + 每文件"未下载"文案（不再误报已就绪）。

## [v0.5.17] - 2026-08-04

### 修复

- **Android VPN 仍启动崩溃（v0.5.16 修复 gVisor 栈选型后暴露的第三层，`panic: invalid timeout` → SIGABRT）**：
  真机 logcat（`logcat_recording_2026-08-04_09-07-28.txt`）显示 `nativeStartVpn`
  已受理、`panic: invalid timeout` 后 `Fatal signal 6 (SIGABRT)` 崩溃，栈：
  `udpnat.New` ← `sing-tun.NewUDPForwarder` ← `GVisor.Start` ← `androidvpn.Vpn.Start`
  ← `startVpnKernel.func`.
  - **根因**：`androidvpn.Vpn.Start` 构造 `tun.StackOptions` 时**未设置
    `UDPTimeout`/`ICMPTimeout`**（自 v0.5.15 起强制 gVisor 才走这条路）。gVisor
    `Start()` 用 `UDPTimeout` 构造 UDP NAT（`NewUDPForwarder` → `udpnat.New`），
    而 sing v0.8.0 的 `udpnat.New` 对 `timeout==0` **直接 `panic("invalid
    timeout")` 而非返回错误**——panic 发生在 `startVpnKernel` 的异步 goroutine
    里，整个进程崩溃。v0.5.16 之前走 `NewSystem`（`udpTimeout` 同样为 0 也会
    panic，但该校验在 system 栈更晚触达），故"与前几版同样的问题"。
    - **修复**：`UDPTimeout: 5*time.Minute`、`ICMPTimeout: 10*time.Second`
      （对齐 sing-box `constant/timeout.go` 默认值；`udpnat.New` 要求
      `timeout>0`）。
  - **加固**：异步启动 `kernel.Start`/`vpn.Start` 的 goroutine 增加
    `recover` 兜底——未来任何库函数直接 panic（如本次）都能经 `failStart`
    正常回滚 + 通知 Java 拆除，而不是 SIGABRT 崩溃。
  - **验证**：`GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -tags
    with_gvisor ./androidvpn/` 通过；`go build/vet/test ./...` 全绿（新增
    `core.TestSaveConfigUpdatesSnapshot` 回归）；真机行为（TUN `warp=on`）仍需
    装 `app-release.apk` 验证。

- **Windows/macOS/Linux GUI 保存配置后切页再回来看不到变更（需重启才刷新）**：
  设置页"保存配置"提示成功，但切走再切回，改动仍是旧值。这是配置热重载的
  **内存快照未同步**问题：
  - **根因**：`Service.GetConfig` 的数据源是 `Server.Status().Config`（即
    `s.cfg` 快照）。`Server.SaveConfig` 只把配置**写盘**（`WriteConfig`），
    从不更新 `s.cfg`；热重载的 `applyConfigReload` 也只做分流引擎重建/系统
    代理副作用，**同样不写 `s.cfg`**。前端每次切页都会重新 `getConfig()`，但
    拿到的始终是启动时或首次 `ensureConfig` 的旧快照——直到重启（Launch 把
    `s.cfg` 重新从磁盘加载）才看到新值。
    - **修复**：`SaveConfig` 写盘后立即更新内存快照（`s.cfg = &snapshot`，
      快照内 `RulesPath/GeoDir` 锚定到运行时目录，磁盘仍写相对路径保持
      可移植）；`applyConfigReload` 校验失败路径外一律 `s.cfg = nc`，运行中
      热重载同样立即反映。另修复 `applyConfigReload` 重建引擎时未对
      `RulesPath/GeoDir` 做运行时目录锚定（相对路径会用 CWD 而非 config/ 目录
      找文件）的隐患。
  - **验证**：新增 `TestSaveConfigUpdatesSnapshot` 回归（保存后 `Status().Config`
    立即为新值、内存锚定、磁盘仍相对）；`go test ./core/...` 全绿。

## [v0.5.16] - 2026-08-04

### 修复

- **Android VPN 仍无法启动（v0.5.15 修复 JNI 闪退后暴露的下一层，真机两层根因）**：
  真机 logcat（`logcat_recording_2026-08-04_07-36-26`）显示 `nativeStartVpn`
  已成功受理（v0.5.15 的 JNI 闪退已修复），但两秒后 `kernel assembly failed`
  → `fdsan: double-close` → SIGABRT。定位到**两个独立 bug**：
  1. **TUN 栈选型退化为 system 栈（`need one more IPv4 address in first
     prefix for system stack`）**：`androidvpn.Start` 调 `tun.NewStack("")`
     空串，sing-tun 按编译标志选栈——CI 的 Android 构建 `-tags
     production,android` **没带 `with_gvisor`** → `WithGVisor=false` → 落到
     `NewSystem`。`NewSystem` 要求 `Inet4Address[0]` 前缀含 **next 地址**
     （`HasNextAddress(prefix,1)`），而 WARP 只分配单个 IP，我们传
     `172.16.0.2/32`——`/32` 单地址无 next → 报错。`NewGVisor` 只取前缀首
     地址、不要求 next，且我们的 handler（`NewConnectionEx`）本就是 gVisor
     型。
     - **修复**：`NewStack("gvisor")` 显式指定 gVisor 栈（不再依赖空串的
       编译标志分支）；CI 的 `build-android` c-shared 两架构 + Android 兼容
       门全部加 `with_gvisor` tag；`github.com/sagernet/gvisor` 依赖确认可用
       （经 sing-tun 传递，indirect 即满足 with_gvisor 构建）。
  2. **fdsan: double-close of file descriptor 329 → SIGABRT**：栈创建失败 →
     `rollback` → Go `vpn.Stop()`→`tun.Close()` 关 fd，同时 `failStart` →
     `kernelFailed`→Java `closeNative()`→`pfd.close()` 再关**同一 fd** →
     Android 15 fdsan 检测 → SIGABRT。**Go 与 Java 同时持有同一 OS fd 的
     所有权**。
     - **修复（fd 所有权归 Go 单一持有）**：Java `establish()` 后
       `pfd.detachFd()` 把 OS fd 转移给 Go（此后 `pfd.close()` 只关
       ParcelFileDescriptor 壳、不关 fd）；Go 侧负责 fd 关闭——`tun.New`
       包装成功后 `NativeTun.Close()`，包装前/同步校验失败路径
       `nativeStartVpn` 内 `unix.Close(fd)`（`androidvpn.Vpn` 增 `fd` 字段 +
       `v.fd=0` 防双关 + Stop 兜底关）。
  - **验证**：`GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -tags
    with_gvisor ./androidvpn/` 通过（决定性——v0.5.15 该命令落 `NewSystem`
    会报错）；`go build/vet/test ./...` 全绿；CI `build-android` job 的 JNI
    双侧 grep 断言保持通过。真机行为（TUN `warp=on`）仍需装
    `app-release.apk` 验证。

## [v0.5.15] - 2026-08-04

### 修复

- **Android 点击启动闪退 + VPN 不开启 + 分流引擎未初始化（同一根因，真机 SIGABRT）**：
  `nativeStartVpn` 缓存 `WarpVpnService` 类引用时对 JNI 第二参数 `obj` 调了
  `GetObjectClass`。但 `nativeStartVpn` 是 **`static native` 方法**——JNI 传给
  Go 导出的第二参数 `obj` **本身就是 `jclass`（WarpVpnService 类对象）**，再
  取类得到的是 `java.lang.Class`。随后 `getProtectSocketMethod` 在
  `java.lang.Class` 上找 `protectSocket` → `NoSuchMethodError` → 主线程抛异常
  → `SIGABRT` 闪退。
  - **症状链**：`TUN established` 后 15ms 内崩溃 → 内核装配从未执行 →
    `androidRuntime.kernel` 恒为 nil → 规则页"分流引擎未初始化（请先启动
    VPN）"、VPN 永远不启动。修好闪退后装配路径走通，引擎随
    `NewKernelContext` 就绪。
  - **修复**：`clsRef := C.newGlobalRef(env, (C.jclass)(obj))`——直接用
    `obj`（已是类对象），删除错误的 `getObjectClass` helper。其余
    `nativeStopVpn`/`nativeVpnRunning`/`nativeLogMessage`/`nativeSetTimeZone`
    同为 static native 但未对 obj 取类，不受影响。
  - **验证**：真机 logcat 复现（`NoSuchMethodError: no static method
    "Ljava/lang/Class;.protectSocket(I)Z"` + SIGABRT 三次重复崩溃）；CI
    `build-android` job 的 JNI 双侧 grep 断言（protectSocket/kernelFailed
    方法签名）保持通过。

- **开启系统代理后 WebSocket（WebSSH 等）会话无法连接（根因修复）**：
  mixed 代理的 HTTP 转发路径（`handleHTTPForward` → `stripHopByHop`）把
  WebSocket 升级请求的 `Connection: Upgrade` 与 `Upgrade: websocket` 逐跳头
  一并剥掉，上游收到普通 GET 无法完成 101 握手 → webssh 会话建立失败。
  - **为什么 V2rayN 正常**：直接走 SOCKS5 CONNECT（原始字节隧道）不解析
    HTTP、头原样透传；系统代理走 HTTP forward 分支才剥头。行业同款坑：
    netbirdio/netbird #6190。
  - **修复（方案 1）**：`handleHTTPForward` 检测到 Upgrade 请求时保留
    `Connection/Upgrade`（及 `Sec-WebSocket-*`）原样透传，不强制
    `Connection: close`，握手成功后 `relay` 直接双向转发帧。
  - **加固（方案 2）**：`sysproxy` 三平台启用系统代理时把本机回环地址加入
    旁路（Linux gsettings `ignore-hosts` / Windows `ProxyOverride`
    `<local>;localhost` / Darwin networksetup `-setproxybypassdomains`），
    浏览器访问 `localhost`/本机 webssh 直连，天然绕开代理剥头。
  - 补集成测试：`TestHTTPForwardWebSocketUpgrade`（真实 mixed 监听器捕获
    上游原始请求字节，断言 WS 头保留 + 101 回传）、普通 GET 回归测试；
    `TestLinuxSetEnable` 增补 ignore-hosts 调用断言。

## [v0.5.14] - 2026-08-03

### 修复

- **Android 连接所有边缘地址失败（根因修复）**：`WarpVpnService.establish()`
  装了全量路由（`addRoute("0.0.0.0", 0)` + `addRoute("::", 0)`）后，**应用
  自身新创建的 socket 也走 TUN**——而 TUN 要等拨号成功后才被 sing-tun 读取，
  于是 QUIC ClientHello 滞留未处理的 tun 里，所有边缘握手 2s 超时 →
  "所有边缘地址均失败" → 30s 装配超时。桌面 CLI 无此问题（无 TUN 自路由），
  故真机"依然连接失败"而桌面正常。
  - **修复**：新增 `tunnel.socketProtector` 钩子（`dialAddr` 创建 UDP 拨号
    socket 后、发任何包前调用）；Android 桥注册
    `WarpVpnService.protectSocket(fd)`（`VpnService.protect()` 把拨号 socket
    豁免出 VPN 路由走物理网络）。DoH 复用同一 QUIC 连接，无需单独保护。
  - 补 `TestKernelNewContextCanceledSkipsDial` 相关路径不受影响；CI 新增
    `protectSocket`/`kernelFailed` 双侧 JNI 签名 grep 断言。
- **Android 启动失败后"无法停止内核"（通知残留）**：异步装配失败只改 Go
  状态，Java 的 `vpnPfd`/`nativeRunning` 保持 true、前台通知残留——用户点
  停止看似无响应。现 `failStart` 额外经新 JNI 桥 `kernelFailed(msg)` 通知
  Java 侧自拆除（`stopForeground` + `stopSelf` + 关 TUN fd），停止按钮随后
  总是幂等生效。
- **Android 拨号总超时不可配置**：`androidDialTimeout` 从固定 30s 改为
  **可配置**（`config.json` 的 `dial_timeout_seconds`，默认 60s；0/缺失 =
  默认）。弱网/慢速运营商下可调大，不必改代码。

## [v0.5.13] - 2026-08-03

### 变更

- **运行时文件统一收拢到运行目录下的 `config/` 子目录**（修复 Docker 版
  "自动生成注册文件无法保存"）：config.json / reg.json / rules.txt / geo 全部
  落到 `<运行目录>/config/`（自动创建），Docker 只需映射
  `./warp-config:/data/config` 一个目录即可持久化全部配置。
  - **根因**：旧逻辑把运行时文件锚定到"可执行文件目录"，Docker 中 exe 位于
    只读 `/usr/local/bin` → 回退到容器层 `~/.config/warp-go`，从不落到
    挂载卷 `/data`。现 `core.baseExecRoot` 改为：可执行目录（可写）→
    **当前工作目录**（可写；Docker WORKDIR `/data` 挂载卷）→ 用户配置目录。
  - **Android 不变**：`DataDir` 非空时仍锚定沙箱根 `getFilesDir()`，不套
    `config/` 子目录（真机路径约定未变）。
  - **一次性旧布局迁移**：`config/config.json` 不存在但旧执行根有散落文件时
    自动复制进 `config/`；幂等、非破坏（不删原文件）。
  - Docker compose 挂载从 `./warp-data:/data` 改为 `./warp-config:/data/config`；
    README/AGENTS 运行时文件约定同步更新（v3 → v4）。

### 修复

- **Android 规则页"重新加载"报"分流引擎未初始化"**：Android 上分流引擎挂在
  `androidRuntime.kernel`（VpnService 驱动的 `core.Kernel`），`core.Server.kernel`
  （SOCKS 内核）在 Android 永不初始化——原 `Service.ReloadRules` 走
  `Server.ReloadRules` → `s.kernel==nil` 报错且规则不生效（可能影响 VPN 网络）。
  现新增 `core.Kernel.ReloadRules()`，`Service.ReloadRules` 在 Android 路由到
  `androidRuntime.kernel.ReloadRules()`（VPN 未运行时报"请先启动 VPN"）；补
  `TestKernelReloadRules` 单测（改规则文件后 ReloadRules 生效）。

## [v0.5.12] - 2026-08-03

### 修复

- **Telegram 无法连接（默认规则未覆盖 TG 流量）**：默认规则模板与
  `rules/default-rules.txt` 新增 `proxy,geoip:telegram`——TG 流量（如
  `149.154.175.100`）此前落入隐式 direct 兜底，在封锁 TG 的网络下直连
  失败；现命中后走 WARP 隧道。geoip 类别大小写不敏感（`TELEGRAM` 库内
  大写，`Lookup` 用 `ToUpper` 归一化）。补 `TestMatchGeoIPTelegram` +
  `TestDefaultRulesParses`（7→8 条）+ `TestLoadGeoIP`（telegram 段）测试。
  > **注意**：已有 `rules.txt` 不会自动更新——需手动加一行或删除后重启
  > 让默认模板重新生成。
- **Android 检查更新"前往下载"在应用内 WebView 打开**：`<a target="_blank">`
  在 Android WebView 被应用内捕获，GitHub 下载页体验差/登录墙。新增
  `Service.OpenExternalBrowser`（桌面走 Wails `BrowserManager`，Android 走
  反向 JNI 桥 `MainActivity.openExternalBrowser` 用 `Intent.ACTION_VIEW`
  跳第三方浏览器）；前端"前往下载"改调它。

## [v0.5.11] - 2026-08-02

### 修复

- **Android 边缘不可达时无限重试刷日志、状态永远"连接中"**（真机，v0.5.10）：
  `NewMasqueClient` 初始拨号无限指数退避重试（3.2s→5s），移动网络下
  QUIC/UDP 被运营商封锁时永久重连，日志无限刷"边缘 X 不可达...3.2s 后
  重试"，状态停在"连接中"。现 Android 装配拨号加 30s 总超时
  （`androidDialTimeout`，`context.WithTimeout`）——超时后经
  `failStartCtx` 报"连接边缘超时（30 秒内未能连接 WARP 边缘，请检查网络
  后重试）"，用户可检查网络重试。
- **Android 点停止无效（装配中/运行中）**（真机，v0.5.10）：
  - **Go 侧**：装配进行中（`NewKernel` 拨号未完成）`nativeStopVpn` 只能
    `cancel()`，但 `NewMasqueClient` 拨号用内部 lifeCtx 不响应外部 ctx →
    无限重连停不掉。新增 `tunnel.NewMasqueClientContext(ctx, ...)` /
    `core.NewKernelContext(ctx, ...)`：初始拨号循环 select 监听外部 ctx，
    取消立即中止（桌面/CLI 的 `NewKernel` 传 background，行为不变）。
  - **Java 侧**：`WarpVpnService` 是 `startForegroundService` 启动的前台
    服务，`MainActivity.requestStopVpn` 用 `stopService` **无法停止前台
    服务**（Android 8+ 需先 `stopForeground`，否则 `onDestroy` 从不触发
    ——日志停在 "TUN established" 后无 onDestroy）。新增
    `WarpVpnService.stop(Context)`：`stopForeground(STOP_FOREGROUND_REMOVE)`
    + `stopSelf()`，`requestStopVpn` 改调它。
  - 补 `TestKernelNewContextCanceledSkipsDial` 回归单测（ctx 已取消时不
    调用拨号工厂）。

## [v0.5.10] - 2026-08-02

### 修复

- **Android 初始化成功后点启动报 `nativeStartVpn：边缘地址解析失败：-ip ""`
  （真机，v0.5.9）**：Android 桥调 `core.ResolveEdgeAddrs(cfg, "", "", reg)`，
  当 `cfg.EdgeAddr` 与 `optsEdgeIP` 均为空时落入 default 分支，把空串当
  显式端点解析 → "missing port in address"。现 `ResolveEdgeAddrs` 双空时
  回落 `"4"`（按注册信息 IPv4 端点展开端口列表，与 CLI 默认 `-ip 4` 一致）；
  补 2 个回归单测（双空回落 / 双空缺 IPv4 报错）。
- **Android 应用扫描 IP 后再次启动 GUI 卡死（ANR）、VPN 无互联网**（真机，
  v0.5.9）：`nativeStartVpn` 在 Java 主线程（`onStartCommand`）同步执行
  Kernel 装配——`NewMasqueClient` 初始拨号无限指数退避重试（每候选 2s
  超时），边缘不可达时永久阻塞主线程 → 5s 后系统 ANR"卡死"；TUN fd 已由
  `establish()` 建立但从未交给 Go 内核 → VPN 已激活但无流量。现：
  - `nativeStartVpn` 改为轻量前置校验 + 立即返回 0（"已受理"），Kernel
    装配/拨号全部移入 Go goroutine（`startVpnKernel`），主线程不再阻塞；
  - 新增装配取消信号：`nativeStartVpn` 前置创建 `context.WithCancel`，
    `nativeStopVpn` 在装配完成前到达即取消，装配各阶段检查 `ctx.Err()`
    中止并释放已建资源（防止"启动后立刻停止"仍运行）；
  - 新增 `Java_com_wails_app_WarpVpnService_nativeVpnRunning` JNI 导出，
    Java 重入守卫据此区分"内核真在运行"（幂等跳过）与"已受理但异步失败"
    （释放旧 TUN fd 重新建立）——否则失败后 `vpnPfd` 残留导致重试被拦；
  - `onStartCommand` 的 `vpnPfd`/`nativeRunning` 置位提前到
    `nativeStartVpn` 之前（异步返回后置位会漏掉 START_STICKY 重投/重入）；
  - `WarpVpnService.closeNative()` 抽取幂等 teardown，`stopNativeAndClose`
    复用。
  - CI `build-release.yml` JNI 双侧 grep 断言新增 `nativeVpnRunning` 签名级
    检查。

### 文档

- README 新增 **Docker 部署**章节（拉取/注册/启动/验证/注意事项）；
  `docker-compose.example.yml` 注释完善（首次注册 → 日常启动 → 配置文件
  进阶的完整切换说明）。

## [v0.5.9] - 2026-08-02

### 修复

- **Android 启动报错"VPN 建立失败：At least one address must be specified"**
  （真机，v0.5.8）：`WarpVpnService.onStartCommand` 从 intent extras 读
  `assigned_ipv4/6`，但 `MainActivity.startVpnService()` 从未传 extras →
  `VpnService.Builder` 无地址 → `establish()` 抛异常。现 WarpVpnService 在
  extras 缺失时从沙箱 `reg.json` 读 `assigned_ipv4/6` 兜底（`getFilesDir()`）。
- **Android 点击注销无确认框、无动作**（真机，v0.5.8）：`window.confirm` 在
  Android WebView 静默返回 false → `onDeregister` 直接 return。现改为自绘
  两段确认：首次点击"注销"进入确认态（按钮变"确认注销？再次点击执行" +
  提示文案），5 秒无操作自动取消；全平台一致，不依赖 WebView 原生 dialog。

## [v0.5.8] - 2026-08-02

### 修复

- **初始化完成但状态页仍显示"正在初始化"无法启动**（win/Android 真机，
  v0.5.7 引入）：StatusPage 对 `getStatus` 返回值二次 `fromStatus`——getStatus
  已返回 camelCase 归一化对象，二次归一化只认 snake_case 的 `init_done`/
  `is_android` → 恒 false。修复：页面直接用 getStatus 返回值（null 兜底），
  GeoPage/SettingsPage 同源修复（getGeo/getConfig 也去掉二次归一化）。
- **重开 GUI 又初始化、无限循环**（win/Android 真机）：`Service.defaultsInit`
  是内存标志，重启丢失；文件已就绪仍重新下载并反复打"初始化完成"。新增
  `core.Server.InitDone()`（基于 rules.txt + GEO 文件就绪），InitDefaults 与
  GetStatus 据此判断——文件在则直接完成，不重复下载。
- **Android 默认白天模式**（真机）：React mount 时 Wails bridge 未就绪，
  `System.IsDarkMode()` 返回 false → 首帧误用浅色，且 `android:ThemeChanged`
  事件早于 mount 发出被漏掉。`useTheme` mount 后延迟 300ms 重查一次。
- **Android 状态页仍显示系统代理**（真机）：双重归一化导致 `isAndroid` 恒
  false，卡片未隐藏。随第一项修复（页面直接用 getStatus 返回值）。
- **Windows 启动后也卡"正在初始化"**（win 真机，v0.5.7 引入）：同第一项
  （双重归一化）——上一版本文件都在，但 initDone 恒 false 禁用了启动按钮。
  已修复；同时 `SetSystemProxy` 自动启动内核的旁路不受影响。
- **其它软件关闭系统代理时 GUI 不跟随**（win 真机）：后端 `Status.SysProxyOn`
  原读内存标志，外部关闭后仍显示"开"。新增 `sysproxy.Enabled(addr)`（三平台
  读真实系统状态：Windows 注册表 / darwin networksetup / linux gsettings）+
  `core.sysProxyCurrentlyOn`；`Status()` 只在本程序设置过代理时读真实状态
  （避免每 2s 轮询外部命令开销）；前端 `proxyEnabled` 跟随轮询的 `sysProxyOn`
  自动变关。

### 变更

- **停止内核同步关闭系统代理**：`Server.shutdown()` 改为"系统代理当前指向
  本程序才清除"——外部软件已改走别的代理时不动它（避免误关其它软件设置）。
- **托盘退出同步关闭系统代理**：托盘"退出"在 `app.Quit()` 前等待内核
  shutdown（最多 2s，含清系统代理），避免进程终止残留代理。
- **关闭按钮最小化到托盘**（win/linux）：`RegisterHook(WindowClosing)` +
  `event.Cancel()` + `window.Hide()`，点关闭藏托盘而非退出；macOS 保持关闭
  即退出（系统惯例）。托盘"退出"改直接 `app.Quit()`（避免被 hook 拦）。

## [v0.5.7] - 2026-08-02

### 修复

- **GUI 状态显示错误**（win/Android 真机，v0.5.5 起暴露）：`fromStatus` 读
  `o.State`（大写）但后端 JSON 是 `state`（小写）→ `running` 恒 false →
  启动后按钮不变"停止"、状态页显示"已停止"（端口实际在监听）。改为 `o.state`，
  `types.test.ts` 用真实后端 JSON 验证映射。
- **GEO 更新时间不显示**（win 真机）：`fromGeo` 读 `o.geositeUpdated`（camelCase）
  但后端是 `geosite_updated`（snake_case）→ 恒 undefined → "更新于 —"。改为
  snake_case 优先 + camelCase 兜底。
- **注册信息只显示设备 ID 和隧道类型**（win 真机）：StatusPage 注册卡片未渲染
  account/密钥类型/边缘端口字段。补全 9 字段；`types.test.ts` 断言全映射。
- **Android 无法开启系统代理**（真机）：`SetSystemProxy` 无 android 分支，走
  桌面逻辑等 SOCKS server running（Android 永不运行）→ 静默超时。现 Android
  明确报错"VPN 接管全部流量，无需系统代理"，前端隐藏系统代理卡片。
- **Android 注销失败**（真机）：`DeleteRegistration` API 注销失败只 log 警告
  不返回错误 → 用户不知 API 侧注册仍在。现 API 失败返回错误（本地仍删除）；
  GUI `Deregister` 用 `cachedDataDir()` 兜底，不依赖可能失败的 serverInstance。

### 新增

- **注册信息完整显示**（win/Android 真机）：`core.Status()` 在 `s.reg` 为 nil
  （GUI 打开、尚未启动）时从磁盘补读 reg.json 视图并缓存，注册卡片在未启动
  时也显示 id/账号/密钥类型/边缘地址端口/分配 IP 全部 9 字段；Register 后同步
  缓存、Deregister 后清空缓存。新增 `status_registration_test.go` 两个回归测试。
- **初始化完成门控**：`Service.InitDefaults` 完成后日志打出"✓ 初始化完成…
  现在可以启动内核"，`Status.InitDone` 置 true；前端启动按钮在初始化完成前
  禁用并提示"正在初始化（默认规则 / GEO 数据库下载中）"。
- **Android 启动/停止/注销全程日志**（真机"点击启动无反映也无日志"）：Java
  侧 `WarpVpnService` 的 establish 失败、授权失效、内核启动失败、onDestroy/
  onRevoke 全部经 `nativeLogMessage` 转发进 GUI 日志页；`Service.Start/Stop`
  Android 分支成功路径也打日志。
- **Android 日志时间显示系统时间**（真机"日志不是系统时间"）：Go 运行时内嵌
  `time/tzdata`（Android 无系统时区库），`nativeSetTimeZone` 的
  `time.LoadLocation` 不再失败。
- **Android 通知栏对比度修正**（真机"通知栏看不清"）：v0.5.5 误用
  `SYSTEM_UI_FLAG_LIGHT_STATUS_BAR`（深色图标）配深色底 → 深色图标看不清。
  移除该 flag，深底配默认浅色（白色）图标，始终可见。
- **应用图标**（全平台）：从 `appicon.png` 生成 Windows `.ico`（PE 资源 +
  versioninfo IconPath）、macOS `icons.icns`、Android `ic_launcher`/`ic_launcher_round`
  五个密度 mipmap（白底圆角 + 居中图标）。
- **浅色模式日志框**（全平台）：日志框由恒黑底改为浅色 `bg-slate-50` / 深色
  `dark:bg-slate-950`，浅色主题不再伤眼；debug 级别颜色调高对比。
- **长路径换行**（全平台）：状态页规则文件路径、GEO 页 geosite/geoip 数据库
  地址加 `break-all`，竖屏窄宽度不再溢出。
- **注册按钮 id 显示**（win 真机"注册成功（id=）"）：Wails 把 Go 多返回值
  `(existing, id, error)` 序列化为元组 `[boolean, string]`，`api.ts` 旧代码按
  对象读导致 id 恒空。现兼容元组/对象两种形态。

## [v0.5.5] - 2026-08-02

### 修复

- **GUI 点击"启动"卡死 + 其他页全部无法显示**（Windows/桌面，v0.5.1 起）：
  `Service.Start()` 持 `s.mu` 调用 `serverInstance()`（内部再次加锁，
  sync.Mutex 不可重入 → 自死锁），GUI 服务线程永久阻塞，所有服务调用
  （GetStatus/GetRules/GetGeo…）卡在锁上。修复：锁内先读状态，锁外调用
  `serverInstance()`；`service_test.go` 加 goroutine+超时回归测试。
- **Android 通知栏透明看不到**（真机）：Android 15+ 强制 edge-to-edge 下
  `setDecorFitsSystemWindows(true)` 只让内容不绘制到状态栏下，但
  statusBarColor 默认透明。显式 `setStatusBarColor` + 亮色图标，通知栏
  深底浅字始终可见。
- **Android 点击启动无反应且无日志**（真机）：`requestStartVpn` 在
  `sInstance==null`（Activity 销毁重建后）时静默 return；Go 侧桥未就绪
  只返回 error（前端按钮旁小字，手机易忽略）。两者改为明确 log（进
  GUI 日志页 + logcat）。
- **全平台侧边栏/底部导航更换主题按钮移除**：主题切换只在设置页保留。

### 新增

- **构建产物版本号**：版本号单一来源 = release tag（`v0.5.3` → `0.5.3`），经
  `-ldflags -X main.version` 注入：
  - CLI `-version` 打印版本（`warp v0.5.3`）；`go version -m` buildinfo 可见
  - Windows PE 版本资源（`goversioninfo` 生成 `.syso`，嵌入 FileVersion/
    ProductName，资源管理器"详细信息"可见，**降低报毒误报**——有版本/描述
    的可信二进制）
  - GUI 设置页"关于"卡片显示版本
  - Release 产物命名带版本（`warp-0.5.3-linux-amd64`、`warp-gui-0.5.3-*.exe`）
  - Android APK/AAB versionCode/versionName 由 CI 注入（versionCode 语义
    单调递增：0.5.3 → 503）
- **检查更新**：`core/updater.go` 查询 GitHub Releases API 最新版本，与当前
  版本比较（纯函数 `compareVersions` 可单测）。CLI `-check-update` flag +
  GUI 设置页"关于"卡片"检查更新"按钮（发现新版本显示 tag + 下载链接）。网络
  失败非致命（显示"检查失败"）。

## [v0.5.3] - 2026-08-02

### 修复

- **Android cgo 构建失败**：`C.jclass`/`C.jmethodID` 不能与 untyped nil 直接
  比较、`*int` 不能传给 `*C.int` 参数——改用 `unsafe.Pointer` 比较 +
  `var needsDetach C.int`（CI 的 grep 断言只查符号存在，抓不到类型错误）。
- **JNI 签名不一致**：`MainActivity.nativeBridgeReady` Java 声明 `void` 而 Go
  返回 `C.jint`（未定义行为，错误码丢失）——改为 `int` 对齐，CI 断言升级为
  签名级检查。
- **Android 启动失败状态撒谎**：kernel/vpn 异步启动失败只写 `lastErr`，
  `started` 保持 true → GUI 显示"运行中"但隧道是死的，且无法再次启动。现
  失败即回滚（cancel + 拆除双方 + 置 started=false，校验仍是本实例防覆盖
  新状态）；启动成功清空 `lastErr`（旧错误不残留）。

## [v0.5.2] - 2026-08-01

### 修复

- **Android"启动内核失败"根治**：GUI"启动/停止"按钮此前走 `core.Server.Start`
  （SOCKS 代理路径，Android 上无意义——隧道必须经 VpnService/TUN）。新增反向
  JNI 桥（`MainActivity.requestStartVpn/requestStopVpn` + `nativeBridgeReady`
  缓存全局引用与方法 ID，C helper 封装 JNI 调用镜像 Wails 模式），Start/Stop/
  IsRunning 在 Android 上桥接 VpnService 生命周期；GetStatus 用 androidRuntime
  状态覆盖生命周期字段。一键注册 → 启动 → consent → TUN 隧道全链路打通。
- **Android 日志"无日志"**：`log.SetOutput` 移到包 `init()`（c-shared 库加载
  时同步执行），早于任何 JNI 调用——`nativeStartVpn` 先于 `main()` 触发时
  内核日志也进 GUI 日志页（此前只进 logcat）。
- **Android 注册状态切页丢失**：`dataDir()` 首次成功取值后缓存（Wails
  StoragePath 桥接抖动会瞬时返回 `""`，`serverInstance` 失败 → `GetStatus`
  误报未注册）；失败路径用缓存沙箱目录兜底检查 reg.json。
- **扫描 v4/v6 无候选**：`scanFallback()` 在注册信息缺边缘地址时返回清晰错误
  （修复前 `net.JoinHostPort("","443")` 生成 `":443"` 垃圾候选）；注册端点
  提取回退 `endpoint.host`（API 可能只返回 host）。
- **状态栏被 UI 覆盖**：`MainActivity.onCreate` 显式
  `WindowCompat.setDecorFitsSystemWindows(getWindow(), true)`（Android 15+
  默认强制 edge-to-edge，此前 WebView 绘制到状态栏下方）。

### 新增

- **手机底部导航**：`<md` 隐藏侧边栏，新增固定底部导航（6 页 + 主题循环格，
  safe-area 底部内边距）；`NAV/TITLES` 抽到 `lib/nav.ts`（+7 vitest 单测）。
- **扫描空结果可操作提示**：琥珀色警告说明可能原因（QUIC 受限/缺边缘地址）
  并引导重新注册。

### 变更

- 前台服务通知渠道名 "Background work" → "warp-go VPN"。
- CI `build-android` 增加 JNI 符号 Java↔Go 双侧 grep 断言。

## [Unreleased] — v0.5.1 计划中

### 修复

- **Android 运行时文件统一锚定到应用沙箱**：GUI 服务层（`service.go` →
  `core.New`）此前把所有相对运行时路径（config.json / reg.json / rules.txt /
  geo）经 `resolveExecPath` 锚定到 `os.Executable()` 目录——Android 上是只读
  的 `/system/bin/app_process`，导致"生成默认配置 /system/bin/config.json
  失败：read-only file system"。新增 `core.Options.DataDir`（非空时所有相对
  路径锚定到该目录）+ `gui/datadir_{android,other}.go`（Android 返回
  `getFilesDir()`，桌面返回空串保持原行为）。GUI 服务层与 JNI 侧
  `buildAndroidConfig` 的沙箱路径就此对齐——注册写盘、内核启动、默认规则
  初始化三条链路打通。
- **日志时间戳去重**：标准库 `log` 默认 `Ldate|Ltime` 前缀经 `logWriter`
  原样进入环形缓冲，前端日志页出现"04:55:44 error 2026/08/01 04:55:44 ⚠…"
  双时间戳。`initLogging()` 加 `log.SetFlags(0)`，只保留环形缓冲按系统时间
  生成的 `HH:MM:SS`。
- **注销按钮无反馈**：注销/注册成功提示原本渲染在"注册信息"卡片内，注销后
  `status.registration` 变 null 导致卡片连同提示一起消失。提示移到页面顶部
  持久显示，注销/注册成功后立即刷新状态。

### 新增

- **跟随系统主题（全平台）**：三态主题模式（浅色 / 深色 / 跟随系统）。
  新增 `useTheme` hook：经 Wails runtime `System.IsDarkMode()` 获取系统偏好，
  `Events.On` 监听 5 个平台主题变更事件（common/windows/linux/android/ios）
  自动切换，`matchMedia` 作浏览器回退；模式持久化到 localStorage。侧边栏
  按钮循环切换三态，设置页新增"外观"卡片分段选择。Android 侧复用既有
  `android:ThemeChanged` 事件（Java 无需改动）。

### 变更

- **竖屏侧边栏自适应**：侧边栏宽度由固定 `w-52` 改为 `w-16 md:w-52`——
  小于 `md`（768px）自动收成 64px 图标栏（标签/品牌文字隐藏），手机竖屏
  不再被 208px 侧边栏挤占，横屏与桌面不变。

## [v0.5.0] - 2026-08-01

### 新增

- **Android 版**（Wails v3 Android 壳 + 自写 Java `VpnService`）：GUI 模块新增
  Android 目标，`WarpVpnService.java` 通过 `VpnService.Builder`（addAddress/
  addRoute/setMtu/setBlocking）`establish()` 取 TUN fd 经 JNI 交给 Go 侧
  `nativeStartVpn(fd)`/`nativeStopVpn()`；`gui/androidbridge.go` 用
  `//export Java_com_wails_app_WarpVpnService_*` 导出 JNI 函数（与 Wails 自带
  18 个导出共存于同一 `libwails.so`）；`MainActivity` 接 `VpnService.prepare()`
  consent 流（singleTask 规避 #5725）；manifest 声明 `WarpVpnService` +
  `BIND_VPN_SERVICE` + `android.software.vpn`；用户可见名 "warp-go"。
- **`core.Kernel` 抽取**：共享运行时（`MasqueClient` + `route.Engine` + 注册信息）
  从 `core.Server` 抽为可复用 `core.Kernel`（`NewKernel`/`Start`/`Stop`/
  `DialTunnel`/`Route`/`AssignedIPv4`/`AssignedIPv6`/`Close`），CLI/GUI/Android
  三端共用；`Server` 公开 API 不变，既有 proxy_test.go 原样作回归契约。
- **androidvpn 决策宿主可测**：`androidvpn/decision.go`（`//go:build android || linux`）
  抽出纯决策逻辑 `decideAction`/`resolveAction`，宿主单测覆盖 proxy/direct/
  未命中兜底/reject 五路径；reject 绝不进入拨号（与桌面 M6 语义对齐）。
- **CI `build-android` job**：ubuntu-24.04 + JDK 21 + `android-actions/setup-android`
  （SDK + NDK r27）+ wails3 alpha2.119 → c-shared（arm64/x86_64）+ gradle
  APK/AAB；既有 test job 加 `GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build ./...`
  编译检查。

### 修复

- **geoip 真实 IP 匹配**：TUN 目标是 IP 字面量时传真实 `netip.Addr` 给
  `RouteFunc`（此前恒为 `netip.Addr{}`，geoip 规则永不命中）；域名目标保持
  零 Addr（geosite/domain 规则适用，geoip-for-domains 局限记录在案）。
- **`-l` 日志回归修复**：`BuildTLSConfig` + `ResolveEdgeAddrs` 从 `Server.Start`
  抽出后日志顺序恢复。

### 变更

- 桌面三端复用同一 `core.Kernel`（CLI/GUI/Android），分流决策与注册信息
  解析逻辑单一来源。
- Android 运行时文件在应用沙箱 `getFilesDir()`（Wails Android 存储路径），
  桌面保持执行目录行为（`gui/androidconfig.go` 路径分支）。

## [Unreleased]

- 无

## [v0.4.0] - 2026-08-01

### 新增

- **REJECT 规则行为（广告拦截）**：规则支持 `reject,条件` 行为，命中即拒绝连接——
  SOCKS5 返回 `0x02`（connection not allowed by ruleset）、HTTP 返回 `403`，
  且不建立任何连接（隧道或直连）。状态页新增"拦截"统计卡片。
- **GitHub 下载加速前缀**：内置默认 `download_proxy = https://gh-proxy.org/`，
  对 `github.com` / `raw.githubusercontent.com` 的下载 URL（GEO 数据库、默认规则）
  自动拼接加速前缀，解决部分网络直连 GitHub 失败的问题；可在 GUI 设置页
  自定义或置空关闭；非 GitHub 地址（镜像仓库）不加速。
- **默认规则托管 GitHub**：首次启动优先从仓库 `rules/default-rules.txt` 下载
  默认规则（失败回退内置模板）；模板新增广告拦截：
  ```
  REJECT,geosite:category-ads-all
  direct,geosite:private
  direct,geoip:private
  proxy,geosite:google
  proxy,geosite:geolocation-!cn
  direct,geosite:cn
  direct,geoip:cn
  ```
- **开启系统代理自动启动内核**：GUI 中开启系统代理且内核未运行时，自动异步
  启动 warp-go（未注册时明确报错不挂起）。

### 修复

- **GUI 首启死锁（根因）**：`InitDefaults` 持有互斥锁时调用 `serverInstance()`
  二次加锁（非重入），导致所有 Wails 服务调用（读配置/注册/读规则/读 GEO）
  永久阻塞——这是"程序启动不读 config.json/reg.json、一键注册无响应无日志"
  的统一根因。现已修复并加回归测试。
- **首启引导**：`-geo-update` 与 GUI 首启一次完成 config.json 生成、默认规则
  下载/回退、GEO 数据库下载。
- **流量统计恒为 0**：后端 `route.Stats` 字段无 JSON tag 序列化为大写键
  （`ProxyHits` 等），前端只读小写键导致真实模式计数恒 0；改为双向读取。
- **状态页扫描按钮错位**：移除"扫描最优边缘"按钮（扫描功能已独立成页），
  替换为"注销（-del）"动作。
- **侧边栏收起后展开按钮点不到**：收起态 `w-14` 底部栏内宽不足导致按钮溢出
  被主面板盖住；加宽并收起时收缩暗色按钮。
- **托盘退出无效**：退出时先关闭窗口再 `app.Quit()`（修复 Linux GTK 退出后
  主窗口残留）。
- **设置页按钮改名**："重新加载" → "重置配置"。

### 变更

- 规则行为/条件解析大小写不敏感（`domain:` 条件值除外，保持原样）。
- 配置热重载对 `download_proxy` 即时生效（GEO/规则下次下载使用新加速前缀）。

## [v0.3.1] - 2026-07-31

### 修复

- Windows 版多项问题：GUI 演示模式根因（bindings 路径 + 构建顺序）、启动黑框、
  侧边栏按钮难以点击。
- 扫描功能独立成页（ScanPage），状态页不再承担扫描入口。

### 新增

- GUI 扫描边缘按钮 + 开机自启（三平台）。
- 路径锚定到可执行目录 + GUI 自动注册引导 + 注册信息展示/注销。
- sync 工作流加冲突预检测（merge-tree）。

## [v0.2.0] - 2026-07-31

### 新增（首个功能完整版本）

- **GEO 分流引擎**（`route/`）：规则解析、rules.txt 模板与热重载、GEO 数据库
  下载（SHA-1 去重 + protobuf 校验 + 原子写）、匹配引擎（geosite/geoip/domain/
  private/lan）。
- **mixed HTTP+SOCKS5 代理**（`proxy/`）：首字节嗅探同端口服务、UDP ASSOCIATE
  中继（数据报直连不经隧道）。
- **系统代理**（`sysproxy/`）：Windows 注册表 / macOS networksetup / Linux gsettings。
- **config.json 热重载**：文件变更自动生效（mtime + 内容 hash 检测）。
- **core/ Server 生命周期抽取**：CLI 与 GUI 共用。
- **Wails v3 GUI**：React 19 前端五页（状态/规则/GEO/设置/日志）+ 系统托盘。
- **CI/CD 三个工作流**：sync-upstream（双上游自动合并）、build-release（5 平台
  CLI + 3 平台 GUI → GitHub Release）、docker-ghcr（多架构镜像 → GHCR）。

## [v0.1.1] - 2026-07-31

### 修复

- 初始版本的小问题修复。

## [v0.1.0] - 2026-07-28

### 新增

- 首个可用版本：MASQUE over QUIC/HTTP-3 隧道 + SOCKS5 前端，基于上游
  badafans/warp-go 与 6Kmfi6HP/warp-go 的 fork。
