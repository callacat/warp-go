# warp-go Go 侧架构调研报告（go-architect）

> 调研人：go-architect 子代理（码农团队）
> 日期：2026-08-15
> 范围：core / tunnel / proxy / route / registration / scanner + main.go
> 依据：全部为真实读代码所得，标注文件:行号可追溯。未修改任何代码。

---

## 0. 结论速览

- **核心架构健康**：`core.Server` / `core.Kernel` 的生命周期与三端（CLI/GUI/Android）复用设计良好，锁模型清晰，错误处理有套路（逐层 `%w` 包装），测试缝（依赖注入 dialer / engine swap）到位。tunnel 的 QUIC/重连恢复逻辑是全文最复杂的部分，工程质量高，但复杂度已接近可维护性上限。
- **最值得修的三处**：
  1. 两套并行的 SOCKS5 + UDP 实现（`tunnel` 的 `HandleSOCKS5`/`udp.go` 在生产是死代码，`proxy` 是活路径），约 400 行重复 + 双 se配置缝。
  2. `engineHolder`（core）与 `Engine`（route）双重的热重载/换引擎机制，加上 route 包内 `Engine.UpdateGeo` 与 `Server.UpdateGeo` 两处 GEO 换引擎入口职责重叠。
  3. `route.matchGeoSite` 对 geosite 分类条目的线性全扫（Plain/后缀/正则逐条），大类别（如 `geolocation-!cn` 约 2.7 万域名）在匹配热路径上存在可预见的性能隐患。
- **最大技术债/风险**：tunnel/masque.go 单文件 2219 行、状态机交织（connMu/reconnect/dohMu/healthMu 五把锁 + 两个 singleflight + dead 原子标记），配合 v0.5.13→v0.5.27 九轮未修复的 Android 境外流量问题（见 AGENTS.md §未解决问题交接），是全文风险最高的一块。

---

## 1. 架构图（包依赖 + 核心抽象）

### 1.1 依赖方向（谁 import 谁）

```
main.go (CLI 薄壳, ~270 行) ──▶ core
                                   │
   gui/ (Wails v3 + androidbridge) │  (GUI + 三端经 core)
                                   ▼
   ┌────────────────────────  core  ────────────────────────────┐
   │ Server (生命周期/配置/状态/系统代理)  ◀── 依赖：          │
   │ Kernel (MasqueClient + engineHolder + 注册)  ──▶ registration
   └──────┬───────────────┬──────────────┬──────────────────────┘
          │(imports)      │(imports)      │
          ▼               ▼               ▼
       proxy ──▶ route  tunnel ──▶ route  scanner ──▶ (quic-go/http3)
          │         │                 │
          │         │(GEO 解析)        │(v2ray routercommon / protobuf)
          ▼         ▼                 ▼
   autostart  sysproxy        v2fly/v2ray-core、quic-go、sing、sing-tun
```

与 AGENTS.md §6.6 冲突面描述一致：`core/proxy/route/sysproxy/gui` 是独立包，上游永不触碰；`tunnel/`、`registration/` 是上游既有，与上游冲突面集中在 `tunnel/masque.go`、`tunnel/udp.go`、`registration.go`、go.mod。

### 1.2 核心抽象

| 抽象 | 位置 | 职责 | 三端共用 |
|---|---|---|---|
| `core.Server` | core/core.go:102 | 生命周期 Start/Stop/Status、配置、系统代理、GEO 自动更新 | CLI + GUI |
| `core.Kernel` | core/kernel.go:70 | MasqueClient(经 dialer 接口) + engineHolder(route.Engine) + 注册 | CLI + GUI + Android |
| `dialer` 接口 | core/kernel.go:21 | DialTunnel / ResolveDNS / Close；生产=*tunnel.MasqueClient，测试注入假拨号器 | — |
| `engineHolder` | core/kernel.go:30 | 持有当前 route.Engine，支持整体 swap（GEO/规则热重载），并发安全 | — |
| `proxy.Server` | proxy/proxy.go:60 | mixed HTTP+SOCKS5，首字节嗅探，dial() 分流缝 | CLI + GUI |
| `route.Engine` | route/engine.go / matcher.go | 规则解析/匹配/GEO 加载/热重载/统计 | 全部 |
| `tunnel.MasqueClient` | tunnel/masque.go:196 | QUIC/H3 隧道、重连单飞、DoH DNS、活性探测 | 全部（经 kernel） |
| `scanner.Scan` | scanner/scanner.go | 边缘延迟扫描（QUIC 握手探针） | CLI + GUI |

### 1.3 数据流（代理模式，核心链路）

```
客户端
  │ TCP conn (mixed 端口 127.0.0.1:40000)
  ▼
proxy.Server.serveConn (proxy/proxy.go:144)
  │ 首字节嗅探: 0x05→SOCKS5; 其余→HTTP
  ├──SOCKS5 CONNECT → s.dial (proxy/proxy.go:331)
  │      │ Config.Router(host, netip.Addr{})   ← ★ 分流缝
  │      │   reject → errRejected (0x02/403)
  │      │   proxy  → TunnelDial(ctx,target) → core.Kernel.DialTunnel → tunnel.MasqueClient.DialTunnel
  │      │                                                          → resolveTarget(隧道内 DoH)
  │      │                                                          → establishCONNECT (H3 CONNECT)
  │      │   direct / miss → net.Dialer 本地直连
  │      └ relay (proxy/proxy.go:369) 双向 io.Copy
  └──HTTP CONNECT/Forward → 同一 s.dial 缝 (proxy/proxy.go:425/460)
```

隧道内数据流：
```
MasqueClient.DialTunnel (tunnel/masque.go:1301)
  → resolveTarget → (域名→ 隧道内 DoH dnsQuery → 边缘可达 IP)
  → establishCONNECT → openRequestStream → currentConnection（共享 QUIC bundle）
  → connectThroughEdge（约定体 10s 超时，成功后清 deadline）
  → 返回 tunnelConn (net.Conn 适配器，Read/Write 触发 noteDeadStream)
```

Android TUN 数据流走 `core.Kernel.ResolveDNS`（`kernel.go:219`）+ androidvpn/dns.go DNS 拦截 + decision.go 决策（TUN 分支，见 AGENTS.md，不在本次 Go 架构核心范围）。

---

## 2. 逐模块架构评估

### 2.1 core 包（Server 生命周期 + Kernel）—— 质量高

**代码质量**
- `Server.Start`（core/core.go:396）启动序列清晰、失败即用 `defer shutdown()` 部分回滚（core.go:408-412），`shutdown` 幂等、按序拆资源（core.go:626-665）。
- 生命周期状态机 `stateStopped→Starting→Running→Stopping`（core.go:47-54），锁语义明确：`Start`/`Stop` **不持有跨阻塞点的锁**，Stop 只置位 + 关 stopCh 唤醒 Start 的 select（core.go:105-108 注释 + Stop 实现 core.go:596-612），避免死锁——这是全文最规范的一处并发设计。
- `baseExecRoot`/`resolveExecPath`/`migrateLegacyConfig`（core.go:306-362, 177-212）可写性探测（`configDirWritable` 实际创建 config/ 子目录 + 临时文件探测）处理了 Docker 挂载父目录 root 属主的边角case，工程成熟。
- `Config` 平铺字段 + `DefaultConfig` 作基底（core/config.go:55-67），JSON 缺省=显式默认等价，注释点明"显式 0 仍能关闭自动更新"。`WriteConfig` 临时文件+rename 原子写（core/config.go:128-155）。

**并发**
- 唯一可变状态集中在 `s.mu` 保护的字段；`Status()` 每次生成新拷贝（core.go:669），跨 Wails 边界安全。
- `sysProxyEnabled atomic.Bool`（core.go:123），其余同步走 mutex，一致。

**可测试性（好）**
- 测试缝到位：`executableDirFn`/`getwdFn` 包级函数变量（core.go:273-275）、`newKernel` 的 `newDial` 注入（kernel.go:111）、`dialer` 接口（kernel.go:21）。`kernel_test.go`、`core_test.go`、`config_test.go`、`status_registration_test.go` 覆盖生命周期与状态。

**性能**：无热点，启动期一次性工作。

**可维护性小问题**
- `Server` 与 `Kernel` 字段职责重叠（都持有 cfg/regData/engine 相关物），`Server` 里有多处 `s.mu.Lock()` 后快速读单字段再 Unlock 的重复模式（如 core.go:731-734、897-901），可抽 getter 简化，但不算硬伤。
- `geoUpdateOnce`/`UpdateGeo`/`InitDefaults` 三处都直接调用 `route.UpdateGeoData`（core.go:727-751, 804, 872-883），与 `route.Engine.UpdateGeo`（route/engine.go:90）换引擎入口存在职责重叠（见 §4 重构机会 4）。

### 2.2 route 包（GEO 分流引擎）—— 实现干净，匹配热路径有隐患

**代码质量**
- `ParseRules`（route/rules.go:84）逐行 `行为,条件`，错误带行号，规范。
- 热重载 `WatchRulesFile`（route/rules.go:163）**基于 2s 轮询 + mtime + 内容 SHA-256 双检测**，而非 fsnotify——`Atomic 替换间隙` fileState 读取失败会 `continue`（rules.go:192），`stop` 用 `sync.Once` 幂等关闭，安全。AGENTS.md §6.5 曾引入 fsnotify（go.mod 有 `github.com/fsnotify/fsnotify`），但热重载实现实际是轮询（fsnotify 是 sing-tun 传递依赖，非本包所用）。
- GEO 下载 `UpdateGeoData`（route/download.go:56）：SHA-1 去重 → proto.Unmarshal 结构校验 → 临时文件原子改名，验收标准齐全。
- GEO 解析 `LoadGeoSite`/`LoadGeoIP`（route/geodata.go:43,119）：类别名大写存储 + 查询 EqualFold（O(1) map），GeoIP 前缀排序 + 二分定位 + 逆序扫描（geodata.go:180-195）。

**并发**
- `Engine` 用单个 `sync.RWMutex` 保护 rules/geoSite/geoIP（matcher.go:34-52），`Match` 先 RLock 取快照再遍历（matcher.go:71-75）——快照语义正确，即使热重载/换库也不会在匹配中看到半套。
- `engineHolder`（core/kernel.go:30）负责整体 swap，`swap` 会 `Close()` 旧引擎（kernel.go:41-49），`close` 幂等防双重 Close（kernel.go:52-60）。
- 统计用 `atomic.Int64`，无锁。

**可测试性（好）**：`rules_test.go`/`matcher_test.go`/`geodata_test.go`/`download_test.go`，AGENTS.md 记 18 个单测全绿。

**性能隐患（需重视）**
- `Match`（matcher.go:61）对每条 geosite 规则，`geoSite.Lookup` 取出整个分类的 `[]GeoSiteDomain`，然后 `matchGeoSite` **线性全扫**（matcher.go:137-160）逐条做后缀/精确/子串/正则匹配。对 `geolocation-!cn`（AGENTS.md §4 记录约 27,037 域名）、`google` 等大类别，每次 CONNECT 都要扫上万条目，且因为 `Match` 在 `RLock` 快照下进行，这个循环完全串行。浏览器批量并发建连时会成为明显的 CPU 热点。
- **无域名后缀索引**：未对 RootDomain 条目建按脱点后缀分桶的 map/trie，每次都是线性退化匹配。这是最该优化的点（见 §4 机会 1）。
- 正则条目用 `sync.Map` 惰性编译缓存（matcher.go:46,164-175）处理得对，但正则条目在数据集中极少，不是瓶颈。

**语义限制（与 proxy 缝相关）**
- `Match` 接收 `ip netip.Addr`，在 proxy 路径调用时恒为 `netip.Addr{}`（见 §2.3）——**域名目标的 geoip 规则在代理模式下永不生效**，引擎只能按 host 匹配（geosite/domain）。这是设计取舍（避免先 DNS 再走隧道），但 GUI 规则编辑页应提示，否则用户配 `proxy,geoip:cn` 对域名永远 miss。

### 2.3 proxy 包（mixed HTTP+SOCKS5）—— 分流缝清晰，嗅探靠首字节

**分流缝（核心，M2.5 的落点）**
- `Config{Router, TunnelDial}`（proxy/proxy.go:31-56）。`dial()`（proxy/proxy.go:331-354）四条路径清晰：Router 命中 reject→`errRejected`（不建连）；命中 proxy→TunnelDial；未命中/direct→本地 `net.Dialer`；Router nil→全隧道。`errRejected` 哨兵错误（proxy.go:325）映射 SOCKS5 0x02 / HTTP 403。
- `dial()` 用 `netip.Addr{}` 调 Router（proxy.go:337）——即上面 §2.2 说的 geoip-域名 miss。

**首字节嗅探**
- `serveConn` 用 `bufio.Reader.Peek(1)`（proxy.go:161-171），`0x05`→SOCKS5，`CONNECT `/`GET `等→HTTP。Peek 不回退字节流，`http.ReadRequest` 复用同一 `br`（proxy.go:406），正确。
- 健壮性考量：首字节非 `0x05` 一律按 HTTP 处理；乱数据由 `http.ReadRequest` 解析失败静默返回（proxy.go:407-408），不 panic。

**HTTP 转发质量**
- `handleHTTPForward`（proxy.go:460）处理了 WebSocket/Upgrade（保留 Connection/Upgrade 逐跳头与长连接，注释引用 netbirdio/netbird #6190 同款坑），`stripHopByHop`（proxy.go:522）剥 Proxy-Authorization 防凭据泄露——细节到位。
- 普通请求强制 `Connection: close`（proxy.go:489），不做 keep-alive 多请求转发——**吞吐受限**（每个页面并发对象各建一条 TCP），但对隧道代理场景可接受，且文档写明。

**并发（良好）**
- `Server` 用 `ctx`/`cancel` + WaitGroup 管理连接，`Close()`（proxy.go:131）cancel + 关 listener + `wg.Wait()` 优雅收尾。每连接起一个 ctx 监控 goroutine 保证关停时 `conn.Close()` 解除阻塞（proxy.go:145-159），并正确在 handler 返回时 `close(handlerDone)` 释放该 goroutine，避免一 goroutine/连接驻留整个运行期。这是刻意设计的资源释放，正确。
- Accept 循环对 transient 错误指数退避（proxy.go:93-125）。

**可测试性（好）**：`proxy_test.go` 覆盖 4 条分流路径（direct/proxy/miss/nil Router）。

### 2.4 tunnel 包（MASQUE/QUIC 隧道）—— 最高价值也最高风险

**总体**
- `MasqueClient`（tunnel/masque.go:196）单文件 2219 行，是全文的工程重镇。状态：共享 QUIC bundle（`connBundle`）+ 惰性 DoH（`dohConn`）+ 重连单飞（`reconnectFlight`）+ DNS 缓存/单飞 + 国际出口活性探测。

**数据流与缓冲区/性能**
- QUIC 流控窗口按 warp-svc 逆向值放大（masque.go:299-305，10MB 连接窗口 / 1MB 流窗口 / 100 并发流 / 1350 packet），避免 quic-go 默认小窗口节流 bulk 传输——与官方对齐的正确调优。
- 隧道字节流用 `tunnelConn`（net.Conn 适配器，masque.go:1219）`Read/Write` 内嵌 `streamConn`→`http3.RequestStream`。**关键点：`SetDeadline` 是 no-op**（masque.go:1291-1293），注释说明"残留 deadline 会掐死长命隧道"——这给上层带来语义约束：tunnelConn 只能靠 Close(ctx cancel) 终止，不能被超时打断，调用方（http.Transport / 上层拨号器）不应在其上设 deadline。
- DNS 缓存（masque.go:1515-1532）：过期条目惰性清理 + `dnsCacheSweepAt`(1024) 触发整扫 + `dnsCacheMaxEntries`(8192) 兜底整体清空（避免 map 无限增长——注释交代了历史 bug）。单飞 `dnsFlight`（masque.go:1554-1607）。
- `dnsQuery` A/AAAA 并发、A 优先（masque.go:1960-2010），利用 H2 多路复用，一个 RTT 出双答案——设计好。

**并发模型（复杂、需谨慎）**
- 五把锁/同步原语交织：
  - `connMu` RWMutex：`cur` bundle / `closed`（masque.go:209）。
  - `reconnectMu`：重连单飞（masque.go:220）。
  - `dohMu` + `dohDialFlight`：DoH 惰性建连单飞（masque.go:226-231, 1716-1796）。注释明确 **dohMu 绝不跨拨号持有**（否则与 reconnect→invalidateDoH 死锁，Go 互斥锁非重入，masque.go:1720-1726 注释）——这是全文最易踩的锁序坑。
  - `connBundle.healthMu`：CONNECT/DoH 失败窗口计数（masque.go:862-884, 1683-1706）。
  - `connBundle.dead atomic.Bool`（masque.go:158）：连接级故障标志，`currentConnection` 检查后返回 `net.ErrClosed`，让后续请求立即加入重连航班而非在死连接上白等 10s（v0.5.26/27 的核心机制）。
- 重连 `runReconnect`（masque.go:730）支持跨网络中断保活一个恢复航班（即使触发它请求已超时也继续重连），是刻意设计。

**错误处理**
- `shouldReconnectH3`（masque.go:1416）区分"共享传输死"与"单目标拒绝"；`connectFailureRequiresReconnect`（masque.go:840）把 CONNECT 超时做窗口内计数（v0.5.21→v0.5.23 从 distinct 目标改为累计计数的血泪教训，masque.go:843-874 注释），逻辑严谨。
- DoH 侧 `errDoHTransport` 哨兵（masque.go:1622）+ `shouldRetryDoH`（masque.go:1629）保证只有传输级故障才换连接。

**可测试性**
- 依赖注入缝充分：`dialFn`/`dialDoHFn`/`probeFn`（masque.go:233-240）；`masque_connect_test.go`/`masque_socks5_route_test.go`。但**真 QUIC 状态机（connBundle/quic.Conn）无法在单测构造**，`handleProbeFailure`/`currentConnection` 等路径靠 `probeFn`/`dialFn` 模拟，覆盖有上限——这解释了为什么 v0.5.13→v0.5.27 九轮 CI 全绿仍真机失败（AGENTS.md §未解决问题交接）。

**风险点**
- **单文件 2219 行**：拨号/重连/DNS/DoS/探测五个关注点挤在一个类型里，方法间共享 20+ 私有状态字段，新增一个字段要横跨五把锁想清楚读写序。
- `HandleSOCKS5`（masque.go:916）是**上游遗留 SOCKS5 服务器**，生产通过 `proxy` 包（见 §2.3），此路径只有 `masque_socks5_route_test.go` 引用；但其 `SOCKS5Config.RouteFunc` se配置与 proxy 的 `Config.Router` 语义重复（见 §4 机会 2）。
- 未解决：Android 境外流量打不开（见 AGENTS.md，v0.5.13→v0.5.27 未果）。本包是排查核心。

### 2.5 registration（上游既有，简要）
- `Register()` 两步流程（curve25519 建号 → PATCH 登记 ECDSA P-256 + MASQUE 端点/公钥/分配地址），`PeerPublicKeyVerifier` 边缘公钥固定（registration.go:448-474），`DeleteRegistration` 故意绕过 Load 以能解析损坏文件（registration.go:504-542，防"不能注销"僵局）。质量好，稳定，不宜动。
- `Save`（registration.go:545）用 `os.WriteFile` 非原子（无临时文件+rename），但注册信息只在 Register 时写一次，且 GUI/CLI 写前有 lock，可接受（与 config/route 的原子写不一致，可顺手统一，低优先级）。

### 2.6 scanner（上游既有，简要）
- `Scan`（scanner/scanner.go:122）编排清晰：TotalTimeout 子 ctx → 候选解析 → **地址族预探**（familyPreProbe，剔除整族不可达，scanner.go:217-305）→ 并发探测（信号量 + 对位切片）→ 排序 top-N。
- `runProbes` 信号量派发有一处被注释为"对抗审查交叉确认的 Critical fix"的槽泄漏边界（scanner.go:341-372）：占槽与 ctx 取消两条路径严格区分，用 label 跳出外层 for（Go 的 break 在 select 只退 select 不退 for）。注释详尽。
- 可测试性靠 `probeDialer` 包级函数变量注入（probe.go:45）。`unroutableFamily`/`probeConnectionIDLength` 在 scanner 和 tunnel 各复制一份（probe.go:61-65 vs masque.go:402-406）——**跨包常量重复**（小，可抽公共常量包）。

---

## 3. 模块间耦合关系

```
              ┌──────────────┐
              │    main.go   │  (薄壳,仅flag→core.New→srv.Start)
              └──────┬───────┘
                     ▼
   ┌──────────────────────────────┐
   │  core.Server                 │  ←── gui/ (Wails) & androidbridge 共用
   │  core.Kernel                 │
   └───┬────────┬────────┬────────┘
       ▼        ▼        ▼
   ┌──proxy─┐ ┌route──┐ ┌tunnel────────────┐
   │Server  │ │Engine │ │MasqueClient      │
   │(mixed) │ │GEODB  │ │(QUIC/H3/DoH/重连) │
   └────┬───┘ └───┬───┘ └────┬─────────────┘
        │(Router/TunnelDial  │(dialer 接口)  │
        │ 接口注入,无import)  ▼              │
        │            route.Engine ├──▶ route (GEO解析)
        ▼                         └──▶ registration (token)
   sysproxy / autostart (core 直依赖)
   registration <- core 直依赖 (Reg 载入/token)
   scanner <- core 直依赖 (启动扫描/ScanEdges)
```

**关键耦合点**：
- core→{proxy, tunnel, route, registration, scanner, sysproxy, autostart} 全依赖（`core` 是编排中枢）。
- probe 通过 `Config.Router`(func) + `Config.TunnelDial`(func) 依赖 route/tunnel，**以接口而非 import 方式**注入——`proxy` 不 import route 类型，只把 `action string` 传回，松耦合好。
- kernel 通过 `dialer` 接口依赖 tunnel（kernel.go:21），kernel 不 import proxy——注释明示"反向：Server 同时导入 proxy 与 Kernel"（kernel.go:20，消除 kernel↔proxy 循环依赖的正确手法）。
- **唯一剥不掉的硬耦合**：`tunnel` 和 `route` 通过常量/动作串耦合（route.ActionProxy 字符串，masque.go:1046 与 proxy.go:341 都判 `== route.ActionProxy`）。

---

## 4. 重构机会清单（按优先级）

### 🔴 P1-1. 消除双 SOCKS5/UDP 实现（最大低垂果实）
生产走 `proxy.Server`（core/core.go:474 接线），但 `tunnel` 仍保留:
- `MasqueClient.HandleSOCKS5`（tunnel/masque.go:916-1148，约 230 行）+ `SOCKS5Config.RouteFunc` se配置（masque.go:894-911）。
- `MasqueClient.handleUDPAssociate` + `tunnel/udp.go`（344 行）。
- `tunnel/udp.go` 与 `proxy/udp.go` 是**逐行近同的两份拷贝**（仅 struct receiver 与错误 helper 名不同），diff 见实测。`tunnel` 的 `handleUDPAssociate`（tunnel/masque.go:1009）仅从 `HandleSOCKS5` 引用。
- 方案：确认 `HandleSOCKS5` 在生产无调用（仅 `masque_socks5_route_test.go`），将其 SOCKS5/UDP 实现退役，保留 `MasqueClient` 的 Core 能力（`DialTunnel`/`ResolveDNS`/`Close`）。UDP 中继收敛为单一实现（建议留在 proxy，因它只服务 SOCKS5 前端）；`tunnel/udp.go` 删除或与 `proxy/udp.go` 合到共享小包。省约 400 行重复，消除两套 `route.ActionProxy` 判定逻辑漂移风险。

### 🔴 P1-2. 域名后缀索引优化 geosite 匹配热路径
`matchGeoSite`（route/matcher.go:137-160）对分类条目全扫。大类别（`geolocation-!cn` 约 2.7 万域名 / `google`）每次 CONNECT 线性匹配。建议：
- 对 `RootDomain`(Type=2) 条目建「脱点后缀 → 规则」的 map（或 AC 自动机/trie，v2ray 用 `DomainMatcher` 的分组算法），命中点从 O(N) 降到 O(后缀长度)。
- `Plain`(子串)/`Regex` 条目占比低，可保留下沉为兜底线性扫。
- 与 `domainSuffixMatch`（matcher.go:180）的标签边界语义保持一致（已实现）。
- 同时修复语义盲区：`domain` 规则也用同一 index 收益。**注意改的是只读热路径 + 一次构建索引**，热重载安全（索引随 Engine swap/applyRules 一起替换）。

### 🟠 P2-3. 收敛 GEO 换引擎职责（core 与 route.Engine 重叠）
存在两处更新 GEO 后重建引擎的入口:
- `Server.UpdateGeo`（core/core.go:727-751）：`route.UpdateGeoData` 后 `route.NewEngine` + `engineHolder.swap`。
- `route.Engine.UpdateGeo`（route/engine.go:90-101）：`UpdateGeoData` 后 `loadGeoDBs`。
后者实际被调用方很少（需查调用点）。建议统一到一条：`Engine` 内部支持「仅热加载新 GEO 数据」的方法（保留 stats/监听，只重解析 DB），`Server.UpdateGeo` 走它，而不是整个 `NewEngine`（会重建 watcher、重读规则，成本更高且与热重载竞态窗口更大）。`engineHolder.swap` 保留给路径变更等真正需整体重建的场景。

### 🟠 P2-4. 拆分 tunnel/masque.go（单文件 2219 行）
将 `MasqueClient` 拆为可独立维护的件：
- `connBundle` + 拨号/重连态（connMu/reconnectMu/dead）→ `client_conn.go`。
- DoH 连接（dohMu/dohDial/dohConn/dnsQuery）→ `client_doh.go`。
- DNS 缓存/单飞 + `resolveTarget` → `client_dns.go`。
- `HandleSOCKS5`/旧 SOCKS 协议层如 P1-1 退役。
保持 `dialer` 接口（kernel 依赖）与 `dnsServers`/`ProbeQuicConfig` 常量不动。风险：这是 AGENTS.md 冲突面文件，拆包前需与 sync-upstream 冲突策略对齐。

### 🟡 P3-5. 协议常量跨包去重
`connectionIDLength=20`、`unroutableFamily`、`probeConnectionIDLength` 在 tunnel（masque.go:78,402）与 scanner（probe.go:52,61）各一份（注释说明 "scanner 不依赖 tunnel"）。可抽 `warp/edge` 小包共享，或保留复制 + 加交叉测试断言两者一致。低风险。

### 🟡 P3-6. 认证/握手细节统一与 review
HTTP 侧 `checkHTTPAuth` 对 Basic 做 base64 解码（proxy.go:539-553），SOCKS5 用 RFC1929（proxy.go:191-226），两套已实现。可统一为共享 `auth` helper（尤其 base64 解码错误路径）。优先级低。

### 🟡 P3-7. 小型一致性
- `registration.Save`（registration.go:545）非原子写，与 config/route 的原子写风格不一致（低风险，注册信息低频）。
- `Server` 内多处「Lock→读单字段→Unlock」可抽 getter 降低样板（core.go:731-734 等），纯风格。

---

## 5. 风险点

1. **tunnel 隧道健康/重连逻辑复杂度（最高）**：五把锁 + 双 singleflight + dead 原子标志交织，且真 QUIC 状态机无法单测覆盖。叠加 AGENTS.md 未解决的 Android 境外流量问题（v0.5.13→v0.5.27 九轮 CI 全绿仍真机失败 + 已放弃继续修复），隧道的每一处改动都有回归面。接手任何 tunnel 改动前必读 AGENTS.md §未解决问题交接 + 建议先用 debugdiag 实测桌面 CLI 对照（交接第 1 步）。
2. **`tunnelConn.SetDeadline` 是 no-op（masque.go:1291-1293）**：上层若依赖 deadline 超时（如某些 http.Transport）会得不到预期行为，只能靠 Close/ctx 取消。这是隐式契约，文档已提，但调用方（proxy/dialer）需确认不依赖 deadline。
3. **geoip 规则对域名目标在代理路径永不命中**（proxy.go:337 传空 IP）——功能盲区，用户配规则可能困惑，GUI 应提示。
4. **HTTP plain 转发强制 Connection: close（proxy.go:489）**：吞吐与建连开销受限；若未来做 GUI 内嵌浏览器高并发，可能是性能瓶颈。已知取舍。
5. **热重载基于 2s 轮询（route/rules.go:153）**：不是事件驱动，规则生效有至多 2s 延迟（GUI 编辑保存后）。AGENTS.md 曾引入 fsnotify（go.mod 里是传递依赖）但实现未用。对"GUI 编辑即时生效"体验是慢感来源，可在 GUI 保存路径显式 `Reload` 触发（已有 `ReloadRules`，见 core.go:955），已缓解。
6. **config.json 取消运行中热加载（AGENTS.md v0.5.24）**：配置改动需重启生效——这是用户主动取舍，但 GUI 设置页要明确标"重启后生效"（AGENTS.md M9.10 已标）。设计决策，非缺陷，记录以防重构时误加回热加载导致"GUI 保存被覆盖"回归。

---

## 6. 附：关键文件与行号索引

- 生命周期：`core/core.go:396` (Start), `:596` (Stop), `:626` (shutdown), `:669` (Status)
- Kernel 三端复用：`core/kernel.go:70` (Kernel), `:93` (NewKernel), `:156` (Start), `:203` (DialTunnel), `:219` (ResolveDNS), `:236` (Route), `:315` (Close)
- 分流缝：`proxy/proxy.go:31` (Config), `:331` (dial), `:368` (relay); `core/core.go:474` (接线)
- 嗅探：`proxy/proxy.go:144` (serveConn), `:161` (Peek)
- HTTP 转发：`proxy/proxy.go:405` (handleHTTP), `:425` (CONNECT), `:460` (Forward), `:522` (stripHopByHop)
- UDP（活）：`proxy/udp.go:54` (handleUDPAssociate)
- UDP（死拷贝）：`tunnel/udp.go:54`
- SOCKS（死）：`tunnel/masque.go:916` (HandleSOCKS5)
- 隧道核心：`tunnel/masque.go:196` (MasqueClient), `:281` (NewMasqueClientContext), `:316-317` (dnsServers), `:360` (dial), `:690` (reconnect), `:1301` (DialTunnel), `:1349` (establishCONNECT), `:1479` (resolveTarget), `:1540` (ResolveDNS), `:2100` (parseDoHAnswer)
- 隧道健康：`tunnel/masque.go:158` (dead), `:840` (connectFailureRequiresReconnect), `:1265` (noteDeadStream), `:1416` (shouldReconnectH3)
- 规则/GEO：`route/rules.go:84` (ParseRules), `:163` (WatchRulesFile); `route/matcher.go:61` (Match), `:137` (matchGeoSite); `route/geodata.go:43,119` (LoadGeoSite/GeoIP), `:180` (Contains); `route/download.go:56` (UpdateGeoData)
- scanner：`scanner/scanner.go:122` (Scan), `:217` (familyPreProbe), `:316` (runProbes); `scanner/probe.go:79` (probeEdge)

（本报告由 go-architect 子代理产出，供「重构方案」引用；未修改任何代码。）
