# Research: tunnel/ 与 proxy/ 双份 SOCKS5/UDP 实现重复分析

- **Query**: 调研 warp-go 项目中 tunnel/ 和 proxy/ 的双份 SOCKS5/UDP 实现重复情况
- **Scope**: internal
- **Date**: 2026-08-16

## Findings

### 1. tunnel/udp.go 与 proxy/udp.go 逐行对比

两文件分别为 344 行和 362 行。经 `diff`（归一化函数名后），差异**极小**——仅 3 处实质差异，其余为逐行相同。

#### 文件清单

| File Path | 行数 | 描述 |
|---|---|---|
| `tunnel/udp.go` | 344 | `tunnel` 包的 UDP ASSOCIATE 实现（receiver = `*MasqueClient`） |
| `proxy/udp.go` | 362 | `proxy` 包的 UDP ASSOCIATE 实现（receiver = `*Server`） |

#### 差异清单（仅 3 处）

| # | 位置 | tunnel/udp.go | proxy/udp.go | 说明 |
|---|---|---|---|---|
| 1 | 第 54 行 receiver | `func (c *MasqueClient) handleUDPAssociate(ctx context.Context, conn net.Conn)` | `func (s *Server) handleUDPAssociate(conn net.Conn)` | tunnel 侧接受外部 `ctx`；proxy 侧用 `s.ctx`（`Server` 自身的 context） |
| 2 | 第 109 行 context 引用 | `case <-ctx.Done():` | `case <-s.ctx.Done():` | shutdown 监听：tunnel 用传入的 ctx，proxy 用 Server 内部 ctx |
| 3 | 第 57/66/77/84/87 行 错误/绑定 helper | `sendSocks5Err` / `sendSocks5Bound` | `sendErr` / `sendBound` | 仅函数名不同，实现完全相同 |

#### 完全相同的部分（逐行一致）

以下函数/逻辑在两文件中**逐字节相同**（归一化函数名后）：

- `udpIdleTimeout` / `udpMaxDatagram` / `udpResolveTimeout` 常量（`tunnel/udp.go:26-37` vs `proxy/udp.go:26-37`）
- `udpBufPool` sync.Pool（`tunnel/udp.go:41-46` vs `proxy/udp.go:41-46`）
- `udpAssociation` 结构体 + `peer()` / `setPeer()` 方法（`tunnel/udp.go:136-165` vs `proxy/udp.go:136-165`）
- `clientToRemote()` 中继循环（`tunnel/udp.go:167-199` vs `proxy/udp.go:167-199`）
- `remoteToClient()` 中继循环（`tunnel/udp.go:201-232` vs `proxy/udp.go:201-232`）
- `parseUDPRequest()` SOCKS5 UDP 请求解析（`tunnel/udp.go:242-292` vs `proxy/udp.go:242-292`）
- `appendUDPReply()` UDP 回包封装（`tunnel/udp.go:296-311` vs `proxy/udp.go:296-311`）
- `resolveUDPTarget()` 目标地址解析（`tunnel/udp.go:317-344` vs `proxy/udp.go:317-344`）
- `handleUDPAssociate()` 整体框架（bind client socket → bind remote socket → send bound → shutdown goroutine → drain TCP control → relay 双向）（`tunnel/udp.go:54-134` vs `proxy/udp.go:54-134`）

**结论：两文件是逐行近同的拷贝，差异仅在 receiver 类型和错误 helper 命名。** proxy/udp.go 多出 18 行是因为 `sendBound` 函数定义内联在了文件尾部（`proxy/udp.go:346-362`），而 tunnel 侧的 `sendSocks5Bound` 定义在 `tunnel/masque.go:2205-2219`。

---

### 2. HandleSOCKS5（tunnel/masque.go）与 proxy/proxy.go 的 SOCKS5 处理对比

#### 文件清单

| File Path | 行号范围 | 描述 |
|---|---|---|
| `tunnel/masque.go` | 916-1148 (233 行) | `MasqueClient.HandleSOCKS5` — 遗留 SOCKS5 服务器 |
| `proxy/proxy.go` | 174-274 (101 行) | `Server.handleSOCKS5` — 生产 SOCKS5 处理 |

#### 逻辑对比

| 功能 | tunnel/masque.go HandleSOCKS5 | proxy/proxy.go handleSOCKS5 |
|---|---|---|
| 认证（用户名/密码 RFC 1929） | 手写 `conn.Read(buf)` 读字节，手动偏移解析 | 用 `io.ReadFull` + `bufio.Reader`，更健壮 |
| 请求读取 | `conn.Read(buf)` 一次读全部，手动偏移解析 `parseSOCKS5Addr(data)` | `io.ReadFull` 分段读 + `readSOCKS5Addr(br, addrType)` |
| CONNECT 拨号 | 内联 `resolveTarget` + `establishCONNECT`（H3 CONNECT）+ 手写 relay | 委托 `s.dial()` → `Config.TunnelDial` 或 `net.Dialer` + 通用 `relay()` |
| UDP ASSOCIATE | `c.handleUDPAssociate(ctx, conn)` → tunnel/udp.go | `s.handleUDPAssociate(conn)` → proxy/udp.go |
| GEO 分流 | `SOCKS5Config.RouteFunc` seam（masque.go:1035-1050），命中 direct → `handleDirectConnect` | `Config.Router` seam（proxy.go:331-354），命中 direct → `net.Dialer`，命中 reject → `errRejected` |
| reject 规则 | 不支持（只有 direct/proxy 两路） | 支持 `route.ActionReject`（proxy.go:338） |
| HTTP 支持 | 无 | 有（同端口的 `handleHTTP` / `handleHTTPConnect` / `handleHTTPForward`） |
| 连接监听 | 无（需外部 accept 后传入 conn） | 有（`Serve` / `ListenAndServe` 完整 accept 循环） |

**关键区别：**
- tunnel 侧 `HandleSOCKS5` 用裸 `conn.Read` 做协议解析，proxy 侧用 `bufio.Reader` + `io.ReadFull`，后者更健壮且支持 HTTP 复用同端口。
- tunnel 侧 CONNECT 直接内联 H3 CONNECT 逻辑（`resolveTarget` + `establishCONNECT`），proxy 侧通过 `Config.TunnelDial` 函数注入，解耦了隧道实现。
- tunnel 侧不支持 reject 规则，proxy 侧支持。
- tunnel 侧没有 HTTP 代理能力，proxy 侧是 mixed proxy（HTTP + SOCKS5 同端口）。

#### SOCKS5Config vs Config 分流 seam 重复

| tunnel 侧 | proxy 侧 | 说明 |
|---|---|---|
| `SOCKS5Config.RouteFunc`（masque.go:910） | `Config.Router`（proxy.go:51） | 语义相同：`func(host string, ip netip.Addr) (action string, matched bool)` |
| `HandleSOCKS5` 中 RouteFunc 判定（masque.go:1045-1046） | `dial()` 中 Router 判定（proxy.go:337） | 逻辑相似但 tunnel 侧只处理 direct/proxy，proxy 侧额外处理 reject |

---

### 3. tunnel/masque.go 的 UDP relay 路径与 proxy/udp.go 的 UDP relay 路径对比

两条 UDP relay 路径在**逻辑上完全相同**（见第 1 节逐行对比），差异仅是 context 来源：

- **tunnel 路径**：`HandleSOCKS5`（masque.go:1009）→ `c.handleUDPAssociate(ctx, conn)`（tunnel/udp.go:54）→ 用传入的 `ctx` 监听 shutdown
- **proxy 路径**：`handleSOCKS5`（proxy.go:250）→ `s.handleUDPAssociate(conn)`（proxy/udp.go:54）→ 用 `s.ctx`（Server 内部 context）监听 shutdown

UDP relay 逻辑本身（`udpAssociation` 结构体、`clientToRemote` / `remoteToClient` 循环、`parseUDPRequest` / `appendUDPReply` / `resolveUDPTarget`）完全相同。

---

### 4. 调用方分析

#### `tunnel.MasqueClient.HandleSOCKS5` 的调用方

| 调用方 | 文件:行号 | 类型 |
|---|---|---|
| 测试 `serveSOCKS5` | `tunnel/masque_socks5_route_test.go:64` | 测试 |
| 测试 `RouteFunc==nil` 分支 | `tunnel/masque_socks5_route_test.go:234` | 测试 |

**生产代码中无任何调用。** `HandleSOCKS5` 在生产路径中是死代码。

#### `tunnel/udp.go` 的函数调用方

| 调用方 | 文件:行号 | 说明 |
|---|---|---|
| `MasqueClient.handleUDPAssociate` | `tunnel/masque.go:1009` | 唯一调用方，来自 `HandleSOCKS5` 的 UDP ASSOCIATE 分支 |

由于 `HandleSOCKS5` 在生产无调用，`tunnel/udp.go` 在生产也是死代码。

#### `proxy/udp.go` 的函数调用方

| 调用方 | 文件:行号 | 说明 |
|---|---|---|
| `Server.handleUDPAssociate` | `proxy/proxy.go:250` | 从 `handleSOCKS5` 的 UDP ASSOCIATE 分支调用 |

`proxy.Server` 是生产路径的唯一代理服务器，在 `core/core.go:474` 实例化。

#### 生产代理接线路径

```
core/core.go:474  proxy.NewServer(proxy.Config{...})
    → proxy.Server.Serve / ListenAndServe
        → serveConn (proxy.go:144)
            → handleSOCKS5 (proxy.go:174) / handleHTTP (proxy.go:405)
                → dial() (proxy.go:331) → Config.TunnelDial → kernel.DialTunnel (core/kernel.go:211) → tunnel.MasqueClient.DialTunnel (masque.go:1301)
                → handleUDPAssociate (proxy/udp.go:54)
```

生产路径完全不经过 `tunnel.HandleSOCKS5` 或 `tunnel/udp.go`。`tunnel.MasqueClient` 在生产中只暴露 `DialTunnel`、`ResolveDNS`、`Close` 三个能力。

---

### 5. 退役方案

#### 应保留的实现

**proxy/ 侧保留。** `proxy/udp.go` + `proxy/proxy.go` 是生产路径，设计更优：
- `bufio.Reader` + `io.ReadFull` 更健壮
- `Config.TunnelDial` 函数注入解耦隧道实现
- 支持 reject 规则 + HTTP 代理
- 完整的 accept 循环

#### 应退役的实现

**tunnel/ 侧退役。** 以下函数/类型可安全删除：

| 可安全删除 | 文件:行号 | 理由 |
|---|---|---|
| `HandleSOCKS5` | `tunnel/masque.go:916-1148` (233 行) | 生产无调用，仅测试引用 |
| `handleDirectConnect` | `tunnel/masque.go:1153-1201` (49 行) | 仅 `HandleSOCKS5` 调用 |
| `SOCKS5Config` | `tunnel/masque.go:894-911` (18 行) | 仅 `HandleSOCKS5` 使用 |
| `socks5CmdConnect` / `socks5CmdBind` / `socks5CmdUDPAssociate` 常量 | `tunnel/masque.go:887-891` (5 行) | 仅 `HandleSOCKS5` 使用 |
| `parseSOCKS5Addr` | `tunnel/masque.go:2155-2192` (38 行) | 仅 `HandleSOCKS5` 使用 |
| `sendSocks5Err` | `tunnel/masque.go:2194-2196` (3 行) | tunnel 侧唯一调用方在 `HandleSOCKS5` + `handleUDPAssociate` + `handleDirectConnect`，全部退役后无引用 |
| `sendSocks5Success` | `tunnel/masque.go:2198-2200` (3 行) | 同上 |
| `sendSocks5Bound` | `tunnel/masque.go:2205-2219` (15 行) | 仅 `tunnel/udp.go` 调用，udp.go 退役后无引用 |
| **整个 `tunnel/udp.go`** | `tunnel/udp.go:1-344` (344 行) | 生产无调用，与 `proxy/udp.go` 逐行重复 |

#### 需保留的 MasqueClient 核心能力（不可删）

以下函数是 `proxy.Server` 通过 `Config.TunnelDial` / `kernel.DialTunnel` 间接调用的生产能力，**必须保留**：

| 保留 | 文件:行号 | 理由 |
|---|---|---|
| `DialTunnel` | `tunnel/masque.go:1301-1344` | 生产代理的隧道拨号入口 |
| `ResolveDNS` | `tunnel/masque.go:1540-1542` | Android DNS 拦截用 |
| `resolveTarget` / `resolveDNS` / `dnsQuery` 等 | `tunnel/masque.go:1479-2134` | DialTunnel / ResolveDNS 内部依赖 |
| `establishCONNECT` | `tunnel/masque.go:1349-1410` | DialTunnel 内部依赖 |
| `Close` / `NewMasqueClient` / `NewMasqueClientContext` | `tunnel/masque.go:271-355, 2136-2153` | 生命周期管理 |
| `tunnelConn` / `streamConn` / `releaseStream` 等 | `tunnel/masque.go:66-94, 1207-1293` | DialTunnel 返回值 + H3 流管理 |
| `SetSocketProtector` | `tunnel/masque.go:92-94` | Android VPN bridge 用 |

#### 退役后 tunnel/ 需要的适配

1. **删除测试文件**：`tunnel/masque_socks5_route_test.go`（260 行）——测的是 `HandleSOCKS5`，退役后无意义。
2. **保留 `tunnel/masque_connect_test.go`**——已确认测的是 `connBundle` 健康度判定（`noteProgressingCONNECTFailure` 等），与 `HandleSOCKS5` 无关。
3. **无接口适配需求**：`HandleSOCKS5` 是 `MasqueClient` 的方法但不在任何 interface 中，删除不影响类型契约。`proxy.Server` 通过 `Config.TunnelDial` 函数指针调用 `DialTunnel`，不依赖 `HandleSOCKS5`。
4. **删除后 `tunnel` 包仍需编译**——确认无其他文件引用被删符号（`sendSocks5Err` 等可能在其他 tunnel 文件中被引用，需 `go build` 验证）。

#### 预估收益

| 指标 | 值 |
|---|---|
| 可删除行数 | 约 344（udp.go）+ 233（HandleSOCKS5）+ 49（handleDirectConnect）+ 18（SOCKS5Config）+ 5（常量）+ 38（parseSOCKS5Addr）+ 21（3 个 send helper）+ 260（测试）≈ **968 行** |
| 消除的重复 | `tunnel/udp.go` 与 `proxy/udp.go` 的 344 行逐行重复 |
| 消除的漂移风险 | 两套 `route.ActionProxy` 判定逻辑（`SOCKS5Config.RouteFunc` vs `Config.Router`）收敛为一套 |

## Caveats / Not Found

- `tunnel/masque_connect_test.go`（283 行）已确认测的是 `connBundle` 健康度判定逻辑（`noteProgressingCONNECTFailure` / `connectFailureRequiresReconnect` 等），与 `HandleSOCKS5` 无关——退役后**应保留**。
- `go build ./...` 未执行——退役实现后需验证 `tunnel` 包内无其他文件引用被删符号（`sendSocks5Err` / `sendSocks5Success` / `sendSocks5Bound` / `parseSOCKS5Addr`）。
- `docs/warp-masque-reverse-engineering.md:402` 提到 `HandleSOCKS5` 曾有 goroutine 泄漏修复（handlerDone select 模式），退役后该文档段落需同步更新或标注为历史记录。
