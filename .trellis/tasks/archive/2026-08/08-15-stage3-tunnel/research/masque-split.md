# Research: tunnel/masque.go 拆分方案分析

- **Query**: 分析 `tunnel/masque.go`（2219 行）的拆分方案，按职责分类所有函数/类型/常量，分析依赖关系，给出函数→文件分配表
- **Scope**: internal
- **Date**: 2026-08-16

---

## 1. 文件概览

- **文件**: `tunnel/masque.go`，2219 行，`package tunnel`
- **依赖**: quic-go/http3, golang.org/x/net/dns/dnsmessage, golang.org/x/net/http2, `warp/route`
- **冲突面**: AGENTS.md:183 标注仅 4 个文件与上游重叠，`masque.go` 是其中之一——拆分前需与 sync-upstream 冲突策略对齐
- **现有测试**: `tunnel/masque_connect_test.go`（284 行，连接级健康/dead 状态机测试）、`tunnel/masque_socks5_route_test.go`（261 行，SOCKS5 分流路由测试）、`tunnel/udp.go`（345 行，UDP ASSOCIATE 中继，独立但同包）
- **方案文档**: `.omo/plans/warp-go-refactor-2026-08-15.md:102` P2-4「按关注点拆 `client_conn.go` / `client_doh.go` / `client_dns.go`；保持 `dialer` 接口不动」；`.omo/research/go-architect.md:254-260` 给出初步边界

---

## 2. 函数/类型/常量按职责分类

### 2.1 QUIC 拨号（dial/connect 传输层）

| 符号 | 行号 | 类型 | 说明 |
|---|---|---|---|
| `connectionIDLength` | masque.go:78 | const | QUIC 源连接 ID 长度（20 字节，匹配 warp-svc） |
| `socketProtector` | masque.go:88 | var | 包级 socket 保护器（Android VpnService.protect 注入缝） |
| `SetSocketProtector` | masque.go:92-94 | func | 注册 socketProtector（导出，供 androidbridge 调用） |
| `perAddrDialTimeout` | masque.go:98 | const | 单边缘地址拨号超时 |
| `relayDrainGrace` | masque.go:104 | const | 客户端半关闭后响应排水宽限 |
| `connBundle` | masque.go:142-167 | type | 单个 QUIC 连接的所有资源集合（udpConn/qtr/quicConn/h3Client/h3Trans + 健康状态） |
| `(*connBundle).close` | masque.go:171-193 | method | abortive 关闭整个 bundle |
| `(*MasqueClient).dial` | masque.go:360-397 | method | 遍历候选边缘地址拨号 + 出口探测 |
| `unroutableFamily` | masque.go:402-406 | func | 判断错误是否为地址族不可达 |
| `(*MasqueClient).dialAddr` | masque.go:502-595 | method | 单地址 QUIC 拨号 + socket protect + H3 初始化 |

### 2.2 重连（reconnect/singleflight/dead 状态机）

| 符号 | 行号 | 类型 | 说明 |
|---|---|---|---|
| `connectExchangeTimeout` | masque.go:110 | const | 单次 H3 CONNECT 超时 |
| `socksSetupTimeout` | masque.go:111 | const | SOCKS5 建立总预算 |
| `streamOpenTimeout` | masque.go:112 | const | 开流超时 |
| `connectFailureWindow` | masque.go:113 | const | CONNECT 失败计数窗口 |
| `connectFailureTargets` | masque.go:120 | const | 窗口内失败次数阈值（v0.5.23 计数语义） |
| `reconnectRetryInitial` | masque.go:121 | const | 重连退避初值 |
| `reconnectRetryMax` | masque.go:122 | const | 重连退避上限 |
| `reconnectFlight` | masque.go:262-265 | type | 单飞航班（done chan + err） |
| `(*MasqueClient).currentConnection` | masque.go:597-621 | method | 取当前 bundle（含 dead/closed/quic-ctx 判定） |
| `(*MasqueClient).openRequestStream` | masque.go:626-677 | method | 开 H3 流，失效时重连重试一次 |
| `(*connBundle).streamOpenRequiresReconnect` | masque.go:679-684 | method | 开流失败是否需重连 |
| `(*MasqueClient).reconnect` | masque.go:690-728 | method | 单飞合并重连 |
| `(*MasqueClient).runReconnect` | masque.go:730-799 | method | 重连航班循环（退避重试） |
| `(*MasqueClient).retireConnection` | masque.go:804-819 | method | 原子淘汰连接 |
| `(*connBundle).receivedPackets` | masque.go:821-826 | method | 读取收包计数 |

### 2.3 探测（probe/health check）

| 符号 | 行号 | 类型 | 说明 |
|---|---|---|---|
| `probeEgressTarget` | masque.go:127 | const | 8.8.8.8:443 国际出口探测目标 |
| `probeEgressTimeout` | masque.go:128 | const | 探测超时 |
| `egressProbeInterval` | masque.go:137 | const | 运行期探测周期（20s） |
| `(*MasqueClient).probeInternationalEgress` | masque.go:420-447 | method | 真实探测：bundle 上开流 CONNECT |
| `(*MasqueClient).probeEgress` | masque.go:451-456 | method | 探测统一入口（测试注入 probeFn） |
| `(*MasqueClient).egressProbeLoop` | masque.go:462-473 | method | 运行期周期探测循环 |
| `(*MasqueClient).probeEgressOnce` | masque.go:479-487 | method | 单次探测（无当前连接时跳过） |
| `(*MasqueClient).handleProbeFailure` | masque.go:493-500 | method | 探测失败 → dead + retire + reconnect |

### 2.4 健康判定（health/dead/alive）

| 符号 | 行号 | 类型 | 说明 |
|---|---|---|---|
| `(*connBundle).connectFailureRequiresReconnect` | masque.go:840-848 | method | CONNECT 失败是否需重连（超时 vs 非超时） |
| `(*connBundle).noteProgressingCONNECTFailure` | masque.go:861-874 | method | 计数失败窗口（v0.5.23 计数语义） |
| `(*connBundle).noteCONNECTSuccess` | masque.go:876-884 | method | 成功清空窗口 |
| `(*MasqueClient).isClosed` | masque.go:1801-1805 | method | 读 closed（须持 connMu） |

### 2.5 CONNECT/SOCKS5 处理

| 符号 | 行号 | 类型 | 说明 |
|---|---|---|---|
| `socks5CmdConnect` | masque.go:887 | const | 0x01 |
| `socks5CmdBind` | masque.go:888 | const | 0x02 |
| `socks5CmdUDPAssociate` | masque.go:889 | const | 0x03 |
| `SOCKS5Config` | masque.go:894-911 | type | SOCKS5 服务配置（Username/Password/AllowUDP/RouteFunc） |
| `(*MasqueClient).HandleSOCKS5` | masque.go:916-1148 | method | 单条 SOCKS5 连接处理（握手/认证/分流/CONNECT/中继） |
| `(*MasqueClient).handleDirectConnect` | masque.go:1153-1201 | method | 本地直连分流 |
| `releaseStream` | masque.go:1207-1214 | func | 完整释放 H3 流（双向 cancel + close） |
| `tunnelConn` | masque.go:1219-1224 | type | H3 流适配 net.Conn（供 DialTunnel/mixed 代理） |
| `(*tunnelConn).Close` | masque.go:1228-1236 | method | 完整释放 |
| `(*tunnelConn).Read` | masque.go:1242-1248 | method | 读 + noteDeadStream |
| `(*tunnelConn).Write` | masque.go:1251-1257 | method | 写 + noteDeadStream |
| `(*tunnelConn).noteDeadStream` | masque.go:1265-1285 | method | 连接级错误唤醒重连 |
| `(*tunnelConn).SetDeadline` | masque.go:1291-1293 | method | 3 个 no-op deadline（隐式契约） |
| `(*MasqueClient).DialTunnel` | masque.go:1301-1344 | method | 建立隧道字节流（导出，dialer 接口用） |
| `(*MasqueClient).establishCONNECT` | masque.go:1349-1410 | method | H3 CONNECT 交换 + 单次重试 |
| `shouldReconnectH3` | masque.go:1416-1430 | func | 判定连接级 vs 目标级失败 |
| `isTimeout` | masque.go:1432-1438 | func | 超时判定 |
| `connectThroughEdge` | masque.go:1448-1463 | func | H3 CONNECT 交换原语（带 deadline） |
| `connectDeadline` | masque.go:1468-1476 | func | 计算 CONNECT 截止时间 |
| `parseSOCKS5Addr` | masque.go:2155-2192 | func | SOCKS5 地址解析 |
| `sendSocks5Err` | masque.go:2194-2196 | func | SOCKS5 错误响应 |
| `sendSocks5Success` | masque.go:2198-2200 | func | SOCKS5 成功响应 |
| `sendSocks5Bound` | masque.go:2205-2219 | func | SOCKS5 BND 响应（供 udp.go 的 handleUDPAssociate 调用） |
| `streamConn` | masque.go:66-73 | type | http3.RequestStream 包装 net.Conn 接口（socket protect / DoH / DialTunnel 共用） |

### 2.6 DNS/DoH 相关

| 符号 | 行号 | 类型 | 说明 |
|---|---|---|---|
| `dnsServers` | masque.go:38-41 | var | DoH 端点列表（162.159.36.1/46.1:443） |
| `dohServerName` | masque.go:46 | const | TLS SNI cloudflare-dns.com |
| `dohURL` | masque.go:47 | const | https://cloudflare-dns.com/dns-query |
| `dohContentType` | masque.go:48 | const | application/dns-message |
| `dohHandshakeTimeout` | masque.go:50 | const | DoH 建连超时 |
| `dohQueryTimeout` | masque.go:51 | const | DoH 查询超时 |
| `dohMaxResponseSize` | masque.go:52 | const | DoH 响应上限 |
| `dohMinTTL` / `dohMaxTTL` | masque.go:56-57 | const | 缓存 TTL 边界 |
| `dnsCacheSweepAt` / `dnsCacheMaxEntries` | masque.go:61-62 | const | 缓存清理阈值 |
| `dnsCacheEntry` | masque.go:251-254 | type | 缓存项 |
| `dnsFlightResult` | masque.go:256-260 | type | DNS 单飞结果 |
| `errDoHTransport` | masque.go:1622 | var | DoH 连接不可用哨兵错误 |
| `(*MasqueClient).resolveTarget` | masque.go:1479-1503 | method | 目标解析（IP 直通 / 域名走隧道 DoH） |
| `(*MasqueClient).cacheResolution` | masque.go:1515-1532 | method | 缓存写入 + 清扫 |
| `(*MasqueClient).ResolveDNS` | masque.go:1540-1542 | method | 导出 DNS 解析（dialer 接口用） |
| `(*MasqueClient).resolveDNS` | masque.go:1544-1609 | method | 缓存 + 单飞 + 重试一次 |
| `shouldRetryDoH` | masque.go:1629-1631 | func | DoH 重试判定 |
| `dohConn` | masque.go:1643-1654 | type | 长命 DoH 连接（H3 CONNECT + TLS + H2） |
| `(*dohConn).close` | masque.go:1656-1671 | method | 关闭（先释放 H3 流） |
| `(*dohConn).queryTimeoutRequiresReconnect` | masque.go:1673-1681 | method | DoH 查询超时判定 |
| `(*dohConn).noteProgressingQueryTimeout` | masque.go:1683-1696 | method | DoH 失败计数窗口 |
| `(*dohConn).noteQuerySuccess` | masque.go:1698-1706 | method | 清空 DoH 失败窗口 |
| `dohDialFlight` | masque.go:1710-1714 | type | DoH 拨号单飞 |
| `(*MasqueClient).dohConnection` | masque.go:1727-1797 | method | 获取共享 DoH 连接（首用惰性建连 + 单飞） |
| `(*MasqueClient).liveDoH` | masque.go:1815-1832 | method | 校验 DoH 连接活性 |
| `(*MasqueClient).dialAnyDoH` | masque.go:1835-1848 | method | 遍历 DoH 端点 |
| `(*MasqueClient).invalidateDoH` | masque.go:1853-1863 | method | 按身份作废 DoH 连接 |
| `(*MasqueClient).invalidateDoHBundle` | masque.go:1868-1881 | method | 按 bundle 作废 DoH 连接 |
| `(*MasqueClient).dialDoH` | masque.go:1883-1949 | method | 单 DoH 端点建连（CONNECT+TLS+H2） |
| `(*MasqueClient).dnsQuery` | masque.go:1960-2010 | method | A/AAAA 并发查询，A 优先 |
| `(*MasqueClient).dnsQueryType` | masque.go:2016-2087 | method | 单类型 wire 查询 |
| `fqdn` | masque.go:2090-2095 | func | 域名补根标签 |
| `parseDoHAnswer` | masque.go:2100-2134 | func | 解析 DoH wire 响应 |

### 2.7 生命周期 / 构造

| 符号 | 行号 | 类型 | 说明 |
|---|---|---|---|
| `NewMasqueClient` | masque.go:271-273 | func | 构造（导出，cli/gui/android 用） |
| `NewMasqueClientContext` | masque.go:281-355 | func | 可取消构造（导出） |
| `(*MasqueClient).Close` | masque.go:2136-2153 | method | 关闭（导出，dialer 接口用） |
| `MasqueClient` struct | masque.go:196-249 | type | 主类型（见 §3） |

---

## 3. MasqueClient 类型与共享状态

```go
type MasqueClient struct {                    // masque.go:196-249
    // 拨号输入（构造后不变：单线程触碰）
    edgeAddrs  []string                       // :203
    addrIdx    int                            // :204（dial 时写）
    tlsConfig  *tls.Config                    // :205
    quicConfig *quic.Config                   // :206
    token      string                         // :207

    // 连接状态
    connMu    sync.RWMutex                    // :209 保护 cur/closed
    cur       *connBundle                     // :210
    closed    bool                            // :211
    closeOnce sync.Once                       // :212
    lifeCtx   context.Context                 // :213
    lifeStop  context.CancelFunc              // :214

    // 重连单飞
    reconnectMu     sync.Mutex                // :220
    reconnectFlight *reconnectFlight          // :221

    // DoH 共享连接
    dohMu    sync.Mutex                       // :226
    doh      *dohConn                         // :227
    dohDial  *dohDialFlight                   // :228
    dialDoHFn func(context.Context) (*dohConn, error)  // :233 测试注入

    // 测试注入缝
    dialFn  func(context.Context) (*connBundle, error) // :236
    probeFn func(context.Context, *connBundle) error   // :240

    // DNS 缓存 + 单飞
    dnsCache    map[string]dnsCacheEntry       // :243
    dnsCacheMu  sync.RWMutex                   // :244
    dnsFlight   map[string]*dnsFlightResult    // :247
    dnsFlightMu sync.Mutex                     // :248
}
```

### 3.1 connBundle 共享状态

```go
type connBundle struct {                    // masque.go:142-167
    udpConn   *net.UDPConn                  // :143
    qtr       *quic.Transport               // :144
    quicConn  *quic.Conn                    // :145
    h3Client  *http3.ClientConn             // :146
    h3Trans   *http3.Transport              // :147
    closeOnce sync.Once                     // :148
    healthMu  sync.Mutex                    // :149 保护 failureSince/failureTargets
    dead      atomic.Bool                   // :158 连接级故障标记
    failureSince   time.Time                // :165
    failureTargets map[string]int           // :166
}
```

### 3.2 dohConn 共享状态

```go
type dohConn struct {                       // masque.go:1643-1654
    addr   string                           // :1644
    stream *http3.RequestStream             // :1645 H3 CONNECT 载体流
    tls    *tls.Conn                        // :1646
    h2     *http2.ClientConn                // :1647
    bundle *connBundle                      // :1648 所属连接（作废判定关键）
    once   sync.Once                        // :1649
    healthMu       sync.Mutex               // :1651
    failureSince   time.Time                // :1652
    failureTargets map[string]int           // :1653
}
```

### 3.3 锁/原子标志汇总

| 锁/标志 | 类型 | 位置 | 保护对象 | 触碰方 |
|---|---|---|---|---|
| `connMu` | RWMutex | MasqueClient:209 | cur/closed + 间接 DoH 安装校验 | currentConnection / reopenRequestStream / reconnect / runReconnect / retireConnection / isClosed / Close / dohConnection / HandleSOCKS5(经上者) |
| `reconnectMu` + `reconnectFlight` | Mutex + 指针 | :220-221 | 重连单飞 | reconnect / runReconnect |
| `dohMu` + `doh` + `dohDial` | Mutex | :226-228 | 共享 DoH 连接 + DoH 拨号单飞 | dohConnection / liveDoH / dialAnyDoH / invalidateDoH / invalidateDoHBundle / Close |
| `dnsCacheMu` | RWMutex | :244 | DNS 缓存 | resolveDNS / cacheResolution |
| `dnsFlightMu` | Mutex | :248 | DNS 单飞 | resolveDNS |
| `cur.dead` | atomic.Bool | connBundle:158 | 连接级故障 | currentConnection / establishCONNECT / noteDeadStream / handleProbeFailure |
| `cur.healthMu` | Mutex | connBundle:149 | CONNECT 失败窗口 | connectFailureRequiresReconnect / noteProgressingCONNECTFailure / noteCONNECTSuccess |
| `d.healthMu` | Mutex | dohConn:1651 | DoH 失败窗口 | queryTimeoutRequiresReconnect / noteProgressingQueryTimeout / noteQuerySuccess |
| `closeOnce`×3 | sync.Once | MasqueClient:212 / connBundle:148 / dohConn:1649 / tunnelConn:1221 | 幂等关闭 | 各自 close 路径 |

**关键交叉点**: `dohConnection` 在持有 `connMu.RLock()` 时校验 `c.cur != d.bundle` 才安装 DoH 连接（masque.go:1773）——同一代码块同时依赖 connMu 与 dohMu。这是拆分时最要紧的一处，两把锁必须保持同一接收者 c 上。

---

## 4. 依赖关系（函数调用图）

### 4.1 拨号 + 生命周期

```
NewMasqueClient → NewMasqueClientContext
  → c.dial(ctx)                    // 初始拨号循环
      → c.dialAddr → net.ListenUDP / qtr.Dial / h3Trans.NewClientConn / ReceivedSettings
      → c.probeEgress → probeFn? / probeInternationalEgress
           → bundle.h3Client.OpenRequestStream / connectThroughEdge(独立函数)
  → go c.egressProbeLoop()         // 每次成功拨号后启动
```

### 4.2 重连状态机（核心，交叉最密集）

```
[c.request 路径] → openRequestStream → c.currentConnection / c.reopenRequestStream? 
  → currentConnection（读 connMu + dead + quicCtx）
  → 失效 → c.reconnect(ctx, stale)
      → connMu 乐观快查 + reconnectMu 二次快查
      → flight==nil → go c.runReconnect(flight)
      → wait flight.done / ctx / lifeCtx
  → runReconnect → dial（dialFn?/dial）
      → 成功：connMu.Lock 换 cur → old.close → invalidateDoHBundle(old)
      → 失败：退避 timer + lifeCtx 退出
      → reconnectMu 收尾清 flight
retireConnection(stale) → connMu.Lock 摘 cur → stale.close → invalidateDoHBundle(stale)
```

### 4.3 CONNECT 会话路径

```
HandleSOCKS5
  → RouteFunc 命中 direct → handleDirectConnect（不碰隧道状态）
  → 否则 resolveTarget → resolveDNS → dnsQuery[→dohConnection] → establishCONNECT
  → establishCONNECT → openRequestStream → [重连] → connectThroughEdge
      → bundle.receivedPackets / noteCONNECTSuccess / connectFailureRequiresReconnect
      → retireConnection + reconnect（重试一次）
DialTunnel（dialer 接口）→ 与 HandleSOCKS5 的 CONNECT 分支共享 resolveTarget + establishCONNECT
```

### 4.4 DoH 路径

```
resolveDNS → 缓存/单飞 → dnsQuery → dohConnection（+dohDial 单飞）→ dnsQueryType → d.h2.RoundTrip
  → 失败：queryTimeoutRequiresReconnect → invalidateDoH / invalidateDoHBundle
dialDoH → establishCONNECT（复用隧道 CONNECT！）→ TLS(h2) → http2.NewClientConn
invalidateDoH / invalidateDoHBundle 被 retconnect/retire/Close 调用（跨职责调用↓）
```

### 4.5 跨职责调用矩阵（拆分时需要特别处理）

| 调用方 | 被调跨职责函数 | 说明 |
|---|---|---|
| `runReconnect` :759 | `invalidateDoHBundle(old)` | 重连（§2.2）调用 DoH（§2.6）——换 bundle 时按旧 bundle 作废 DoH |
| `retireConnection` :817 | `invalidateDoHBundle(stale)` | 淘汰连接（§2.2）调用 DoH（§2.6） |
| `Close` :2150 | `invalidateDoH(nil)` | 生命周期（§2.7）调用 DoH（§2.6） |
| `dialDoH` :1892 | `establishCONNECT` | DoH 建连（§2.6）复用隧道 CONNECT（§2.5）——DNS 子文件依赖 CONNECT 子文件 |
| `dnsQueryType` :2050/2058/2071 | `bundle.receivedPackets` / `queryTimeoutRequiresReconnect` / `invalidateDoH` | DoH 查询调用连接健康判定 |
| `resolveDNS` :1581 | `dnsQuery` | 解析入口 |
| `dohConnection` :1773 | `c.cur`（connMu 读）| DoH 安装需与重连互斥 |
| `HandleSOCKS5` :1052 | `resolveTarget` | SOCKS5（§2.5）调用 DNS（§2.6） |
| `DialTunnel` :1302 | `resolveTarget` + `establishCONNECT` | 导出 API 同用 |
| `handleUDPAssociate`（udp.go:87） | `sendSocks5Bound` | udp.go 依赖 masque.go 的 SOCKS5 原语 |
| `tunnelConn.noteDeadStream` :1280-1283 | `retireConnection` + `reconnect` | 流层（§2.5）调用重连（§2.2） |

### 4.6 测试注入缝（同包测试直接访问）

- `dialFn`（:236）——masque_connect_test.go 与 masque_socks5_route_test.go 都设置，返回 `*connBundle`（类型依赖 connBundle）
- `probeFn`（:240）——masque_connect_test.go 设置
- `dialDoHFn`（:233）——构造 `*dohConn`（当前无直接测试使用，但保留给 DoH 合并测试）
- `newTestMasqueClient`（masque_socks5_route_test.go:19）直接构造 `MasqueClient{lifeCtx, lifeStop, dnsCache, dnsFlight}` ——字段是**同包测试双方法体**，拆分后仍同包可直接访问（前提：字段跨文件可见，即不移动字段到不可见位置）
- `newTestBundle`（masque_connect_test.go:17）直接构造 `connBundle{healthMu, failureTargets}`

---

## 5. dialer 接口与对外公开 API

### 5.1 dialer 接口定义

**位置**: `core/kernel.go:21-25`（`package core`）

```go
type dialer interface {
    DialTunnel(ctx context.Context, targetAddr string) (net.Conn, error)
    ResolveDNS(ctx context.Context, host string) (net.IP, error)
    Close() error
}
```

**引用方**:
- `core/kernel.go:77`（`dial dialer` 字段）、`:102`（`newKernel` 注入真实工厂）、`:111`（`newKernel` 参数）
- `core/kernel_test.go:17-61`（fakeDialer 实现该接口）
- 生产工厂: `core/kernel.go:102-104` 返回 `tunnel.NewMasqueClientContext(...)`

**不变式**: 接口只要求 3 个导出方法——`DialTunnel` / `ResolveDNS` / `Close`。拆分后只要 `MasqueClient` 指针仍实现这 3 个方法（位置无所谓，同包跨文件），`dialer` 接口与 kernel 全部代码零改动。

### 5.2 MasqueClient 导出的公开方法清单

| 导出方法/函数 | 行号 | 消费者 |
|---|---|---|
| `NewMasqueClient(edgeAddrs, tlsConfig, token)` | masque.go:271 | cli/gui（core.NewKernel 间接） |
| `NewMasqueClientContext(ctx, edgeAddrs, tlsConfig, token)` | masque.go:281 | core/kernel.go:103 |
| `(*MasqueClient).DialTunnel(ctx, targetAddr) (net.Conn, error)` | masque.go:1301 | dialer 接口 + mixed 代理（proxy） |
| `(*MasqueClient).ResolveDNS(ctx, host) (net.IP, error)` | masque.go:1540 | dialer 接口 + androidvpn/dns.go |
| `(*MasqueClient).HandleSOCKS5(ctx, conn, cfg)` | masque.go:916 | proxy 的 SOCKS5 前端 |
| `(*MasqueClient).Close() error` | masque.go:2136 | dialer 接口 + 生命周期 |
| `SetSocketProtector(fn)` | masque.go:92 | gui/androidbridge.go |
| `SOCKS5Config` struct | masque.go:894 | proxy（SOCKS5 前端配置） |

> 注: resolver 相关导出没有——`ResolveDNS` 是唯一的 DNS 导出。`connectionIDLength`/`dnsServers` 是包级未导出常量，同包伙伴（scanner 不依赖 tunnel）无需跨包导出。

---

## 6. 拆分建议

### 6.1 边界判定

方案文档（.omo/plans/warp-go-refactor-2026-08-15.md:102）提议的三个文件名 **`client_conn.go` / `client_doh.go` / `client_dns.go` 基本合理**，方向正确。基于本调研修正为 5 个文件（含 1 个按需拆出的协议原语文件 + 保留一个小的公共文件）：

| 新文件 | 内容 | 理由 |
|---|---|---|
| `client_conn.go` | 拨号 + 重连 + 探测 + 健康判定（§2.1+§2.2+§2.3+§2.4） | 全部围绕 `connBundle` 与 `connMu/reconnectMu` 运作，互相调用密集（4.2 调用图） |
| `client_dns.go` | DNS 缓存/单飞 + `resolveTarget`（§2.6 的解析主干，不含 DoH 建连） | resolveDNS/cacheResolution 自包含；但 dnsQuery 依赖 DoH → 见下方备注 |
| `client_doh.go` | DoH 连接管理 + 查询（§2.6 的 DoH 部分） | dohConn/dohConnection/liveDoH/dialDoH/dnsQuery/dnsQueryType/parseDoHAnswer 自成一体 |
| `client_socks5.go` | SOCKS5 协议层 + CONNECT 交换原语（§2.5）| HandleSOCKS5/parseSOCKS5Addr/sendSocks5*/connectThroughEdge/establishCONNECT/tunnelConn/releaseStream/streamConn |
| `client_dial.go`（或并入 conn） | socketProtector + SetSocketProtector + connectionIDLength + unroutableFamily + streamConn（若 socks5 文件不需要它） | 跨职责共享原语 |

### 6.2 关键取舍点：dnsQuery / dnsQueryType / parseDoHAnswer 归哪？

- `.omo/research/go-architect.md:256` 建议 `dnsQuery` 归 `client_doh.go`——**正确**。理由: `dnsQueryType` 直接调用 `dohConnection`/`dohConn.h2`/`queryTimeoutRequiresReconnect`/`invalidateDoH`，与 DoH 连接生命周期强耦合。
- `resolveDNS`（缓存+单飞+重试编排）归 `client_dns.go`；它调用 `dnsQuery`（在 client_doh.go）。**跨文件调用在同一个包内无任何代价**（同包跨文件直接符号解析），所以不用刻意避免。
- `resolveTarget`（IP 直通/域名分流）归 `client_dns.go`（go-architect 建议一致），被 `HandleSOCKS5`（socks5 文件）与 `DialTunnel`（socks5 文件）调用，同包 OK。

### 6.3 推荐的函数→文件分配表（完整）

约定「原样搬迁不动逻辑」加粗标注。行号为现在 masque.go 中的位置。

#### client_conn.go（连接生命周期：拨号/重连/探测/健康）

| 符号 | 现位置 | 拆分方式 |
|---|---|---|
| `connectionIDLength` | :78 | **纯搬迁**（或并入 dial 文件） |
| `perAddrDialTimeout` | :98 | **纯搬迁** |
| `relayDrainGrace` | :104 | **纯搬迁**（仅 HandleSOCKS5 中继用，放 socks5 文件更贴切，两者皆可） |
| `connBundle` struct | :142-167 | **纯搬迁** |
| `(*connBundle).close` | :171-193 | **纯搬迁** |
| `MasqueClient` struct | :196-249 | **纯搬迁**（需把 DNS/DoH 相关字段一起搬，见 §6.4） |
| `newTestBundle` 相关同包测试 | masque_connect_test.go | 不变（同包） |
| `NewMasqueClient` | :271-273 | **纯搬迁** |
| `NewMasqueClientContext` | :281-355 | **纯搬迁**（构造里初始化 dnsCache/dnsFlight 与 DNS 字段） |
| `(*MasqueClient).dial` | :360-397 | **纯搬迁** |
| `unroutableFamily` | :402-406 | **纯搬迁** |
| `(*MasqueClient).probeInternationalEgress` | :420-447 | **纯搬迁** |
| `(*MasqueClient).probeEgress` | :451-456 | **纯搬迁** |
| `(*MasqueClient).egressProbeLoop` | :462-473 | **纯搬迁** |
| `(*MasqueClient).probeEgressOnce` | :479-487 | **纯搬迁** |
| `(*MasqueClient).handleProbeFailure` | :493-500 | **纯搬迁** |
| `(*MasqueClient).dialAddr` | :502-595 | **纯搬迁** |
| `(*MasqueClient).currentConnection` | :597-621 | **纯搬迁** |
| `(*MasqueClient).openRequestStream` | :626-677 | **纯搬迁** |
| `(*connBundle).streamOpenRequiresReconnect` | :679-684 | **纯搬迁** |
| `(*MasqueClient).reconnect` | :690-728 | **纯搬迁** |
| `(*MasqueClient).runReconnect` | :730-799 | **纯搬迁** |
| `(*MasqueClient).retireConnection` | :804-819 | **纯搬迁** |
| `(*connBundle).receivedPackets` | :821-826 | **纯搬迁** |
| `(*connBundle).connectFailureRequiresReconnect` | :840-848 | **纯搬迁** |
| `(*connBundle).noteProgressingCONNECTFailure` | :861-874 | **纯搬迁** |
| `(*connBundle).noteCONNECTSuccess` | :876-884 | **纯搬迁** |
| `reconnectFlight` | :262-265 | **纯搬迁** |
| `probeEgressTarget/Tim` const | :127-128 | **纯搬迁**（仅探测用） |
| `egressProbeInterval` | :137 | **纯搬迁** |
| `connectExchangeTimeout` 等重连常量 | :110-122 | **纯搬迁** |

#### client_socks5.go（SOCKS5 协议 + H3 CONNECT 交换 + 隧道流）

| 符号 | 现位置 | 拆分方式 |
|---|---|---|
| `socks5CmdConnect/Bind/UDPAssociate` | :887-889 | **纯搬迁** |
| `SOCKS5Config` | :894-911 | **纯搬迁** |
| `(*MasqueClient).HandleSOCKS5` | :916-1148 | **纯搬迁** |
| `(*MasqueClient).handleDirectConnect` | :1153-1201 | **纯搬迁** |
| `streamConn` | :66-73 | **纯搬迁** |
| `releaseStream` | :1207-1214 | **纯搬迁** |
| `tunnelConn` struct + methods | :1219-1293 | **纯搬迁** |
| `(*MasqueClient).DialTunnel` | :1301-1344 | **纯搬迁** |
| `(*MasqueClient).establishCONNECT` | :1349-1410 | **纯搬迁** |
| `shouldReconnectH3` | :1416-1430 | **纯搬迁** |
| `isTimeout` | :1432-1438 | **纯搬迁** |
| `connectThroughEdge` | :1448-1463 | **纯搬迁** |
| `connectDeadline` | :1468-1476 | **纯搬迁** |
| `parseSOCKS5Addr` | :2155-2192 | **纯搬迁** |
| `sendSocks5Err` | :2194-2196 | **纯搬迁** |
| `sendSocks5Success` | :2198-2200 | **纯搬迁** |
| `sendSocks5Bound` | :2205-2219 | **纯搬迁**（udp.go 依赖，同包 OK） |
| `relayDrainGrace`（可选） | :104 | 放这里更贴切（只被 HandleSOCKS5 用） |

#### client_dns.go（DNS 解析入口 + 缓存 + 单飞，不含 DoH 建连）

| 符号 | 现位置 | 拆分方式 |
|---|---|---|
| `dnsServers` | :38-41 | **纯搬迁**（DoH 连接管理用——实际上 dnsServers 只被 dialAnyDoH 用，可放 doh 文件；建议放 doh） |
| `dnsCacheSweepAt`/`dnsCacheMaxEntries` | :61-62 | **纯搬迁** |
| `dnsCacheEntry` | :251-254 | **纯搬迁** |
| `dnsFlightResult` | :256-260 | **纯搬迁** |
| `(*MasqueClient).resolveTarget` | :1479-1503 | **纯搬迁** |
| `(*MasqueClient).cacheResolution` | :1515-1532 | **纯搬迁** |
| `(*MasqueClient).ResolveDNS` | :1540-1542 | **纯搬迁** |
| `(*MasqueClient).resolveDNS` | :1544-1609 | **纯搬迁** |
| `shouldRetryDoH` | :1629-1631 | 依赖 `errDoHTransport`（doh 文件），函数体小，可留在 dns 或 doh ——建议与 errDoHTransport 同放 doh |

#### client_doh.go（DoH 连接生命周期 + 查询）

| 符号 | 现位置 | 拆分方式 |
|---|---|---|
| `dohServerName`/`dohURL`/`dohContentType` | :46-48 | **纯搬迁** |
| `dohHandshakeTimeout`/`dohQueryTimeout`/`dohMaxResponseSize` | :50-52 | **纯搬迁** |
| `dohMinTTL`/`dohMaxTTL` | :56-57 | **纯搬迁** |
| `errDoHTransport` | :1622 | **纯搬迁** |
| `dohConn` struct + methods | :1643-1706 | **纯搬迁** |
| `dohDialFlight` | :1710-1714 | **纯搬迁** |
| `(*MasqueClient).dohConnection` | :1727-1797 | **纯搬迁** |
| `(*MasqueClient).liveDoH` | :1815-1832 | **纯搬迁** |
| `(*MasqueClient).dialAnyDoH` | :1835-1848 | **纯搬迁** |
| `(*MasqueClient).invalidateDoH` | :1853-1863 | **纯搬迁** |
| `(*MasqueClient).invalidateDoHBundle` | :1868-1881 | **纯搬迁** |
| `(*MasqueClient).dialDoH` | :1883-1949 | **纯搬迁** |
| `(*MasqueClient).dnsQuery` | :1960-2010 | **纯搬迁** |
| `(*MasqueClient).dnsQueryType` | :2016-2087 | **纯搬迁** |
| `fqdn` | :2090-2095 | **纯搬迁** |
| `parseDoHAnswer` | :2100-2134 | **纯搬迁** |

### 6.4 MasqueClient 结构体字段去哪（⚠️ 不可拆散）

`MasqueClient` struct 的字段强绑定于锁的使用方式，**不能按职责拆成多个子结构体**——`dohConnection` 需要同时访问 `connMu`/`dohMu`/`cur`，`resolveDNS` 需要 `dnsCacheMu`/`dnsFlightMu`，`reconnect` 需要 `connMu`/`reconnectMu`/`lifeCtx`。**建议原样整体保留在 client_conn.go**（作为主类型所在文件），后续 child struct 化是另一项重构，不属于本次「纯拆分」。

若一定要分文件放字段，会破坏同包测试对 `MasqueClient{...}` 复合字面量的直接构造（masque_socks5_route_test.go:19-29、masque_connect_test.go:34-39）——只有把字段留在同一 struct 顶层才能保持测试不改。

同理 `connBundle`（含 `dead`/`healthMu`）整体放 client_conn.go，`dohConn` 整体放 client_doh.go，c 和 d 各自自包含，跨文件引用直接符号解析。

### 6.5 导入变化

拆文件后各新文件 import 会收敛但不会消失：
- client_conn.go: quic-go, http3, h3qlog, net, sync, atomic, time, context, log, errors, fmt, strings, syscall — 保持
- client_socks5.go: http3, quic-go, net, strconv, io, sync, time, bytes(否——handleDirectConnect 不用 bytes；HandleSOCKS5 不用) —— 收敛中等
- client_doh.go: http3, quic-go, tls, http2, dnsmessage, bytes, io, net, net/url, time, context, errors, log, fmt, strings —— 保持
- client_dns.go: net, netip(否), time, context, sync, log, fmt —— 收敛
- route 只在 HandleSOCKS5 的 RouteFunc 判定用到（masque.go:1046 `route.ActionProxy`）→ 随 client_socks5.go
- dnsmessage 只在 DoH 文件用
- `net/netip` 只被 `SOCKS5Config.RouteFunc` 签名用 → 随 client_socks5.go

### 6.6 哪些是「纯拆分不动逻辑」，哪些可能需要小幅重构

**全部标为「纯搬迁」的项**：不动逻辑，仅移动符号位置 + 调整 import。这些是 surace-level 机械重构，风险来自「漏搬符号导致编译错误」，而非行为变化。

**可能需要小幅重构的地方**（不是必须，但建议顺手）：

1. **`MasqueClient` struct 若想拆字段**（§6.4 不建议）：需要改同包测试的复合字面量。**判定：不要做，保持现状**。
2. **`relayDrainGrace` 归属**：两种放法都成立（conn 的通用常量区 or socks5 的专用常量）；选一个并加注释说明。
3. **`dialFn`/`probeFn`/`dialDoHFn` 归属**：这三个测试注入缝直接引用 `*connBundle`/`*dohConn`，随各自依赖方放置即可（dialFn→conn，probeFn→conn，dialDoHFn→doh）。注意 `newTestMasqueClient`（socks5_route_test）依赖这些字段都存在，字段不能改名/删除。
4. **`connectExchangeTimeout`/`socksSetupTimeout`/`streamOpenTimeout`** 同时被 socks5 与 conn 使用 → 放 client_conn.go 常量区，socks5 文件跨文件引用（同包 OK）。这是唯一一处「常量本来就在公共面，拆后语义不变」的例子。

**明确不需要动逻辑的理由**: 整个文件是**单接收者 `c *MasqueClient` 的方法集合**，方法间通过结构体字段通信；拆文件只是把同一 struct 的方法分布到多个文件，Go 语言层面无任何可见性/语义差异——这是本次拆分「纯机械、低风险」的根本依据。当前文件能在 2219 行内自洽说明它没有引入新的包级全局竞态，拆后同包同样自洽。

---

## 7. 风险点（跨职责紧耦合，拆分最容易出错的地方）

1. **`dohConnection` 的 connMu+dohMu 双重校验安装**（masque.go:1761-1786，✅ 最高风险）。第 1767 行 `c.connMu.RLock()` 配合第 1773 行 `c.cur != d.bundle` 判定：这是「DoH 连接必须挂在当前 bundle 上」的跨职责不变量。拆分时必须让 `dohConnection` 仍能访问 `c.cur`/`c.connMu`。**如果`把 DoH 相关字段拆进子结构体但不带 connMu`，这里会编译不过或破坏不变量**。→ 保持 MasqueClient 单结构体。

2. **`reconnect` 的乐观快查 + 二次快查 + 单飞**（masque.go:690-728）。并发正确性依赖 `currentConnection`（读 connMu）与 `reconnectMu` 之间无新航班漏掉。拆文件不影响（同一 struct 同包），但**不要顺手把 `reconnect` 改成子结构体方法**——它读 `c.cur`（connMu 域）。

3. **`invalidateDoHBundle` 的三处调用方**（runReconnect:759 / retireConnection:817 / Close:2150）：重连逻辑换 bundle 时必须连带作废挂在旧 bundle 上的 DoH 连接。这是「连接层 → DoH 层」的反向依赖。拆文件后这三处跨文件调用照旧（同包），但**负责 client_conn.go 的改动者必须记得保留这三个调用**，漏一个 = 悬挂旧 DoH 连接。

4. **`dialDoH` 复用 `establishCONNECT`**（masque.go:1892）：DoH 建连是「借隧道 CONNECT」实现的。所以 client_doh.go **必须 import client_socks5.go 的 `establishCONNECT`/`releaseStream`**（同包直接可用）。若某个拆分者把 `establishCONNECT` 当作「隧道专用」移走或改名，DoH 断供。**推荐把 establishCONNECT/connectThroughEdge 视为共享基础件放 client_socks5.go（因其语义就是 H3 CONNECT 原语）**。

5. **`tunnelConn.noteDeadStream` 触达重连**（masque.go:1265-1285）：流层错误 → `bundle.dead.Store` + `retireConnection` + `reconnect`。流层（socks5 文件）反向依赖连接层（conn 文件）。同包 OK，但**这是「流→连接」的隐性耦合**，单独看懂一个文件的人可能漏掉这条唤醒链。

6. **`sendSocks5Bound` 被 `udp.go:87` 使用**（UDP ASSOCIATE）：把 SOCKS5 原语移到 client_socks5.go 后，udp.go 仍可访问（同包）。**注意不要把 sendSocks5* 移动成 unexported 到某个子包**——那就断了。当前 udp.go 是独立的同包文件，不受拆分影响。

7. **测试复合字面量**（masque_connect_test.go:17-22 `newTestBundle`、:34-39 `deadBundle`、masque_socks5_route_test.go:19-29 `newTestMasqueClient`）：直接以 `&MasqueClient{lifeCtx:..., dnsCache:...}` / `&connBundle{healthMu:..., failureTargets:...}` 构造。**只要字段留在顶层 struct 且不重命名，测试零改动**；任何「字段下沉子结构体」都会破坏这些测试（编译错误，是特征不是 bug，但需知道）。

8. **`Close` 的关闭顺序依赖**（masque.go:2136-2153）：lifeStop → bundle.close → invalidateDoH。与 probe loop（egressProbeLoop 读 lifeCtx）协同。拆文件不影响顺序，但整个生命周期代码集中在 client_conn.go 时最容易保持心智模型。

9. **`dead` 标志的语义贯穿**（connBundle.dead:158 → currentConnection:610 → establishCONNECT:1374 → noteDeadStream:1278 → handleProbeFailure:495）：dead 是连接层字段，被子系统（流层/探测层）读写。**保持 connBundle 单类型 + dead 留在其内**，任何拆分不得把它「按职责搬走」。

10. **`NewMasqueClientContext` 初始化 DNS 字段**（masque.go:316-317 `dnsCache`/`dnsFlight` make）：构造器在 client_conn.go，字段在 MasqueClient（若继续放 conn 文件）——同包 OK。若有人把 DNS 字段下沉子结构体，构造器要跟着改。

11. **上游冲突面**（AGENTS.md:183、.omo/research/go-architect.md:260）：`tunnel/masque.go` 是与上游重叠的 4 个文件之一。拆分会把文件改名/新增，sync-upstream 自动合并时上游若也改了 masque.go 会冲突。**拆分前需在设计文档/变更计划中写明与 sync-upstream 冲突策略**（如拆后不再跟踪上游 masque.go 的旧路径）。

---

## 8. 拆后验证建议

1. `go build ./...` + `go test ./tunnel/... ./core/... ./proxy/... ./androidvpn/...` 全绿（tunnel 是核心回归面）
2. 确认 `git diff --stat` 显示 masque.go 从 2219 行降到约 0（被 4 个新文件替代），且物理行数之和 = 原行数（纯拆分基准）
3. 检查新文件没有任何一行逻辑改动（可用 `git diff` 逐文件 review 或 `diff` 工具对比符号集合）
4. 特别验证 §7 的 11 个风险点：`dohConnection` 双重锁、`invalidateDoHBundle` 三调用方、`dialDoH→establishCONNECT`、`noteDeadStream→reconnect` 链

---

## 9. 结论

- 方案文档建议的 `client_conn.go` / `client_doh.go` / `client_dns.go` **方向正确**，可以按此执行。
- 修正补充：**建议加 `client_socks5.go`**（SOCKS5 协议 + H3 CONNECT 原语 + 隧道流，约 450 行），因为 CONNECT/SOCKS5 层既不是「连接」也不是「DoH/DNS」，是独立的第三个关注点；它还是 `establishCONNECT`/`releaseStream`/`tunnelConn` 的宿主，DNS 与 DoH 都反向依赖它。
- 核心约束：**`MasqueClient`/`connBundle`/`dohConn` 三个结构体保持单类型不拆字段**；所有符号在同包内跨文件直接可见；`dialer` 接口与 kernel 零改动。
- 除 §6.6 提到的 4 个「可能需要小幅重构」点外，其余全部是纯搬迁。
- 最高风险点是 §7 第 1 条（dohConnection 的 connMu+dohMu 双重锁安装）和第 3 条（invalidateDoHBundle 的跨职责调用），拆分 diff review 时优先盯这两处。