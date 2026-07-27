# warp-go

基于 Go 的 Cloudflare WARP 客户端，通过 **MASQUE over QUIC/HTTP-3** 建立隧道，前端以 **SOCKS5** 暴露。免 root、无 TUN、不改动路由——纯代理客户端实现，所有协议参数、注册流程、信任模型均对齐官方 `warp-svc`（详见 [`docs/warp-masque-reverse-engineering.md`](docs/warp-masque-reverse-engineering.md)）。

```
SOCKS5 客户端 ──► tunnel/masque.go ──► QUIC/H3 ──► WARP 边缘 ──► 目标
                        ▲
                registration/registration.go
                （两步注册 API、ECDSA P-256 密钥、mTLS 证书、端点与边缘公钥固定）
```

> [!WARNING]
> 默认监听 `:40000` 且**不要求认证**，对任何地址可达。在不可信网络中请绑定回环地址 (`-l 127.0.0.1:40000`) 或设置 `-user` / `-pass`。
>
> **UDP ASSOCIATE 的数据报不经过 WARP 隧道**——plain CONNECT 是字节流隧道，无法承载数据报，UDP 从本机网络栈直接发出，对端看到的是你的真实地址。TCP 走隧道，UDP 不走。

## 依赖

- Go ≥ 1.26.5（`go.mod` 锁定）
- [`quic-go/quic-go`](https://github.com/quic-go/quic-go) v0.61.0 — QUIC + HTTP/3
- [`golang.org/x/net`](https://pkg.go.dev/golang.org/x/net) — DNS wire format、HTTP/2
- [`golang.org/x/crypto`](https://pkg.go.dev/golang.org/x/crypto) — curve25519

## 构建

```bash
cd warp-go
go build -o warp .
```

## 快速开始

```bash
# 1) 首次使用：注册（注册信息保存在工作目录 reg.json）
./warp -reg
# ✓ 注册信息已保存到 reg.json（id=...）
# 不带 -reg 运行即可启动代理。

# 2) 启动代理（默认监听 :40000）
./warp

# 3) 用 curl 验证
curl --socks5-hostname 127.0.0.1:40000 https://www.cloudflare.com/cdn-cgi/trace
# 若走 WARP，返回的 trace 中 warp=on
```

## 命令行参数

```
warp —— Cloudflare WARP 客户端（MASQUE over QUIC/HTTP-3，SOCKS5 前端）

代理：
  -l <host:port>   SOCKS5 监听地址（默认 :40000，双栈，同时接受 IPv4/IPv6）
  -user <用户名>   SOCKS5 用户名；必须同时给出 -user 和 -pass 才启用认证
  -pass <密码>     SOCKS5 密码
  -ip <取值>       连接哪个边缘（默认 4）：
                     4            注册信息中的 IPv4 边缘
                     6            注册信息中的 IPv6 边缘
                     <host:port>  改为连接指定地址，例如 162.159.198.2:4500、
                                  [2606:4700:103::2]:443、example.com:443
                   它决定"如何到达边缘"，不限制隧道内能访问什么——目标由
                   边缘代为连接，所以走 IPv4 边缘一样能访问 IPv6-only 站点。
                   取 4 或 6 时会遍历注册的整个端口列表；给显式 host:port
                   时只使用该端口。域名由系统解析器解析（此时隧道尚未建立），
                   解析出的每个地址都作为候选。

注册：
  -reg             尚未注册时执行注册，然后退出
  -del             向 API 注销并删除本地注册信息

扫描（可选，默认关闭）：
  -scan                启动前扫描 WARP 边缘全段并选用最低延迟的端点
  -scan-cidr <c,...>   追加自定义 CIDR 到默认段（IPv4 5 段或 IPv6 2 段）
  -scan-ports <p,...>  覆盖扫描端口（空则读 reg.json 的 endpoint_ports）
  -scan-concurrency <n> 并发探针数（0=自动 min(64, NumCPU×8)，下限 16）
  -scan-timeout <dur>  扫描总超时（默认 45s）
  -scan-per-probe <dur> 单探针超时（默认 3s）
  -scan-top <n>        选用 RTT 最低的 N 个端点前置（默认 4）
```

### 行为准则

- **启动本身从不注册**——创建账号是需要明确表达的动作。缺少 `reg.json` 是致命错误，提示运行 `warp -reg`。
- **`-reg` 幂等**——已有注册时只报告并退出，**不替换**。替换会让旧注册在 Cloudflare 侧失去本地凭据，再也无法注销。要换注册，请先 `warp -del` 再 `warp -reg`。
- **`-del` 走 API 注销**——仅需 `id` 与 `token`，**不**依赖能解析密钥材料。即使本构建已无法解析该文件的私钥，仍可从 API 侧移除，避免把注册孤立在 Cloudflare 侧。
- **Accept 循环不因瞬时错误退出**——EMFILE、ECONNABORTED 这类瞬态错误按 5ms→1s 指数退避重试，仅在 `net.ErrClosed` 或收到关停信号时退出。
- **`-scan` 不致命、不写回 `reg.json`**——扫描用与正式连接同一协议栈的轻量 QUIC 握手探针，按 RTT 升序取 top-N 端点前置到候选列表，注册端点尾接作兜底。全失败时回退到注册端点并继续启动，而非退出。`-ip 4` 扫 IPv4 段、`-ip 6` 扫 IPv6 段；`-ip <host:port>` 指定显式端点时扫描被忽略（显式端点优先）。这是本项目相对官方 `warp-svc` 的有意增强——官方不做边缘全段延迟优选（详见逆向文档 §11 末的 `[未对齐增强]` 注记）。

## 常用示例

```bash
# 注册（首次使用）
warp -reg

# 用已保存的注册信息运行
warp

# 通过 IPv6 连接边缘
warp -ip 6

# 指定边缘地址与端口
warp -ip 162.159.198.2:4500

# 通过域名连接自定义边缘
warp -ip example.com:443

# 只监听回环地址
warp -l 127.0.0.1:1080

# 对外提供服务并要求认证（RFC 1929 用户名/密码）
warp -l 0.0.0.0:1080 -user u -pass s

# 更换注册
warp -del && warp -reg

# 扫描边缘并选用最低延迟的 IPv4 端点（前置到候选，注册端点兜底）
warp -scan

# 扫描 IPv6 段
warp -scan -ip 6

# 只扫 443 端口（更快覆盖更广），选用 8 个端点
warp -scan -scan-ports 443 -scan-top 8
```

## 配置

注册信息固定保存在**工作目录**下的 `reg.json`（没有路径参数），权限 `0600`，字段如下。

| 字段 | 类型 | 含义 |
|---|---|---|
| `id` / `token` | string | 设备 ID 与 Bearer token，向 WARP API 鉴权、注销时使用 |
| `account` | string | Cloudflare 账户 ID |
| `key_type` / `tunnel_type` | string | `secp256r1` / `masque`（注册后切换为 MASQUE） |
| `private_key` | base64 SEC1 DER | ECDSA P-256 私钥，加载时再派生公钥 |
| `peer_public_key` | base64 PKIX DER | 边缘公钥，用于**证书固定**；旧文件无此字段时降级为警告 |
| `endpoint_v4` / `endpoint_v6` | string | 边缘 IP，`-ip 4/6` 二选一 |
| `endpoint_ports` | `[]int` | 端口列表（默认从中读取，如 `[443,500,1701,4500,4443,8443,8095]`）；`-scan` 默认也复用此列表扫描，`-scan-ports` 可覆盖 |
| `assigned_ipv4` / `assigned_ipv6` | string | 注册时分配的隧道内地址（本项目不直接使用） |

> [!NOTE]
> 端点分配若变化，本项目不会自动刷新——需 `warp -del` 后重新 `warp -reg`。

## 项目结构

```
warp-go/
├── main.go                       # 参数解析、注册编排、TLS 配置、SOCKS5 监听循环
├── registration/
│   └── registration.go           # 两步注册、状态持久化、边缘公钥固定回调
├── tunnel/
│   ├── masque.go                 # QUIC/H3 连接管理、SOCKS5 TCP、隧道内 DoH 解析
│   └── udp.go                     # SOCKS5 UDP ASSOCIATE（直连，不走隧道）
├── scanner/                       # 边缘延迟扫描（-scan）：QUIC 握手探针测 RTT，选 top-N 端点前置
│   ├── endpoints.go              #   默认 WARP CIDR/端口常量、候选展开（BuildCandidates）
│   ├── probe.go                   #   单端点 QUIC 握手探针（probeEdge）、unroutableFamily 判定
│   └── scanner.go                  #   并发编排（Scan）：信号量、族级预探、RTT 排序、TopN 截断
├── docs/
│   └── warp-masque-reverse-engineering.md   # 官方 warp-svc 逆向分析 + 本实现对齐说明
├── go.mod / go.sum
└── README.md
```

| 文件 | 行数 | 职责 |
|---|---|---|
| `main.go` | 513 | 参数解析、注册编排、TLS 配置组装、`-scan` 扫描衔接、SOCKS5 监听循环 |
| `registration/registration.go` | 560 | 两步注册、状态持久化、边缘公钥固定 |
| `tunnel/masque.go` | 1370 | QUIC/H3 连接管理（重连/端口回退）、SOCKS5 TCP 转发、隧道内 DoH |
| `tunnel/udp.go` | 344 | SOCKS5 UDP ASSOCIATE（中继，不经隧道） |
| `scanner/endpoints.go` | 192 | 默认 WARP CIDR/端口常量、候选展开（BuildCandidates、expandCIDR） |
| `scanner/probe.go` | 170 | 单端点 QUIC 握手探针（probeEdge）、unroutableFamily 判定 |
| `scanner/scanner.go` | 409 | 并发编排（Scan）：信号量、族级预探、RTT 排序、TopN 截断 |

## 设计要点

### 连接层

- **端口回退**：边缘返回 7 个端口，按序逐个尝试，每端口 8 秒超时，成功索引被记住供重连优先使用——UDP/443 被封的网络中必需。检测到 `ENETUNREACH/EAFNOSUPPORT/EHOSTUNREACH`（本机无该地址族路由）时直接中止整轮，避免 7×8 秒白等必然重复的失败。
- **20 字节源连接 ID**：与官方 `SimpleConnectionIdGenerator` 一致；需用 `quic.Transport{ConnectionIDLength: 20}` 而非 `quic.DialAddr`，后者无法设置该长度。
- **等待 HTTP/3 SETTINGS**：QUIC 握手完成 ≠ H3 可用，需阻塞直到 `ReceivedSettings()`。
- **惰性重连**：空闲期间断线不会后台恢复，由 `openRequestStream` 在发现连接已死时触发，`reconnectMu` 串行化并用 `current != stale` 判断避免重复拨号——下一个请求承担重连延迟。
- **`connBundle` 单元**：udpConn、`quic.Transport`、QUIC 连接、H3 transport 打包为一组，重连时整组拆除，避免部分残留。
- **端点延迟扫描（`-scan`，可选）**：启动前对 WARP 边缘全段（IPv4 默认 5 个 /24、IPv6 默认 2 个 /48）做与正式连接同一协议栈的**轻量 QUIC 握手探针**测 RTT——握手完成即 `CloseWithError` 干净释放，不等 H3 SETTINGS（排序相关性≈1，省 H3 资源）。按 RTT 升序取 top-4 前置到候选列表，注册端点尾接兜底；族级预探对 `ENETUNREACH` 等整族跳过避免刷爆总超时。并发受信号量约束（`min(64, NumCPU×8)`、下限 16），总超时 45s 硬上限，全失败回退注册端点不致命。不写回 `reg.json`（保留 `-reg`/`-del` 幂等）。这是对官方的有意增强，非对齐——详见逆向文档 §11。

### SOCKS5 与流释放

- 内联实现 SOCKS5（含可选 RFC 1929 用户名/密码认证），支持 CONNECT 与 UDP ASSOCIATE。
- **流释放是关键且曾出过故障**：边缘保持 plain CONNECT 隧道的自己一侧直到目标关闭，仅 `Close()`（关发送侧）不够，会让读方向 `io.Copy` 永久阻塞，同时泄漏 goroutine 与 QUIC 流；攒够若干条被遗弃的流后会耗尽边缘的并发流配额，后续 `OpenRequestStream` **静默阻塞**。当前用 `sync.Once` 保护的 `release()` 完整执行 `CancelRead` + `Close` + 重建连接的 `conn.Close()`。
  - 客户端异常断开 → **立即**释放。
  - 客户端干净半关闭 → 给响应方向 `relayDrainGrace`（30 秒）排空，超时强制释放。
  - 该故障**无法用 `curl` + `kill -9` 复现**——RST 会让两个方向马上出错退出——复现需要客户端干净关闭而边缘保持沉默。
- **每连接 goroutine 随 handler 退出**：监控 `ctx` 以便关停时强关连接的 goroutine，若只 `<-ctx.Done()` 会因 `ctx` 与进程同寿而**每处理一个连接常驻一个**。已用 `select` 同时等 `ctx.Done()` 与 `handlerDone`，实测同负载常驻数为 0。
- **CONNECT 交换的超时**：`SendRequestHeader` 与 `ReadResponse` 都不接受 context，统一在 `connectThroughEdge()` 前后设置/清除 stream deadline——成功后**必须清除**，否则残留 deadline 会在传输中途掐断长命隧道。

### DNS（隧道内 DoH）

对齐官方 `MultiplexedDohProvider`：**单条 HTTP/2 连接承载所有查询，每查询一条 H2 流**。该连接建立在一条 H3 CONNECT 流内部：

```
H3 CONNECT 到 162.159.36.1:443
  └─ TLS（SNI=cloudflare-dns.com, ALPN=h2）
       └─ HTTP/2 ClientConn
            ├─ 查询 A ──► H2 stream 1
            ├─ 查询 B ──► H2 stream 3
            └─ ...
```

- **RFC 8484 wire format POST**，头部与官方一致：`content-type`/`accept` = `application/dns-message`；显式置空 `user-agent`，并用 `DisableCompression` 阻止 `x/net/http2` 注入 `accept-encoding: gzip`（这俩官方都不发）。
- **上游**：`162.159.36.1:443`、`162.159.46.1:443`（官方消费级 DoH，非公开 `1.1.1.1`）；`cloudflare-dns.com` 仅作 SNI，从不解析。
- **两级 singleflight**：(1) 名称级——同主机的并发查询合并为一次；(2) 连接级 `dohDial`——冷启动时多 goroutine 只有一个真正拨号，其余等待其结果。第二级是必需的：Go 互斥锁不能跨拨号持有（`dohConnection → dialDoH → openRequestStream → reconnect → invalidateDoH` 会重入 `dohMu`），否则会各自建一条连接再丢弃其中 N-1 条。
- **连接可用性用 `h2.State()` 而非 `CanTakeNewRequest()`**：后者在连接仅是达到 `MAX_CONCURRENT_STREAMS` 而饱和时也返回 false，而饱和连接是健康的。
- **错误分类**：只有传输层失败（`errDoHTransport`）才允许退休共享连接或触发重试；DNS 应答（NXDOMAIN、无 A 记录、非 200）与本查询自身超时不算——为这些拆连接会中断所有并行查询。
- **双栈**：每个主机名同时发 A/AAAA，走同一条 H2 连接的两条流（只花一个 RTT），有 A 优先用 A（与 hickory `Ipv4thenIpv6` 等价，但少一个 RTT）。
- **缓存**按响应 TTL，钳制 `[5s, 5min]`；超 1024 条清扫过期项，仍超 8192 则整体清空。
- **有意分歧**：官方代理模式下 DNS 走宿主网络栈（`No DNS`），本项目把 DoH 放进隧道内——多一次 CONNECT+TLS+H2 握手，但**避免 DNS 查询以真实源地址泄漏**。

> [!NOTE]
> 用 `net.JoinHostPort` 组装 CONNECT 目标即可，**不要手动加方括号**——重复加会产生 `[[2606:...]]:443`，边缘直接取消该流（观测到 error code 270）。只查 A 记录时不会暴露此 bug。

### 边缘公钥固定

MASQUE 的 SNI 是固定通用名（`consumer-masque-proxy.cloudflareclient.com`），证书链由私有 CA 签发，标准链校验无法建立信任——认证完全依赖**公钥固定**。`Registration.PeerPublicKeyVerifier()` 用注册时保存的 `peer_public_key` 构造 `VerifyPeerCertificate` 回调，用 `ecdsa.PublicKey.Equal()` 比对（等价于官方的 PEM `bcmp`，但避免序列化差异带来假阴性）。旧文件无此字段降级为警告，密钥格式错误则硬失败。TLS `CurvePreferences` 设为 P-256/384/521（与官方经典组一致，不提供 X25519；PQ 组 `P256Kyber768Draft00` Go 无法提供）。

### UDP ASSOCIATE

- **不经过 WARP 隧道**——plain CONNECT 是字节流隧道，承载数据报需 Connect-IP，而那需 TUN。UDP 直接从本机网络栈发出：TCP 走隧道、UDP 走本地，对同一目标呈现不同的源地址。
- 每个关联用两个 socket（面向客户端/面向目标），方向判别不依赖源地址。
- 首个数据报到达时**钉住客户端源地址**以防离路径劫持；拒绝 `FRAG != 0`；60 秒滚动空闲超时，TCP 控制连接关闭时拆除。

## 与官方 `warp-svc` 对齐情况

| 维度 | 官方 | warp-go | 说明 |
|---|---|---|---|
| 源连接 ID | 20 字节 | 20 字节 | 一致 |
| 流控窗口 | 10MB / 1MB | 10MB / 1MB | 一致 |
| 并发流上限 | 100 / 100 | 100 / 100 | 一致 |
| UDP 载荷上限 | 1350 | 1350 | 一致 |
| 边缘公钥固定 | PEM `bcmp` | `ecdsa.Equal` | 等价 |
| TLS 曲线 | PQ + P-256/384/521 | P-256/384/521 | PQ 组 Go 无法提供 |
| 代理 CONNECT | H3 only | H3 only | 一致（官方回退仅对 TUN 模式） |
| DoH 传输 | H2 多路复用，单连接 | 同 | 一致 |
| DoH 格式 | RFC 8484 POST wireformat | 同 | 一致 |
| DoH 上游 | 162.159.36.1 / 46.1 | 同 | 一致 |
| 解析地址族策略 | 顺序 `Ipv4thenIpv6` | A/AAAA 并发，A 优先 | 语义相同，少一个 RTT |
| DoH 位置 | 隧道**外**（宿主解析器） | 隧道**内** | **有意分歧**（避免 DNS 泄漏） |
| keepalive | idle 推导，[5s,50s] | 固定 10s | 在官方区间内 |
| DNS 健康跟踪 | 比率 0.8 | 未实现 | — |
| H2 隧道回退 | 有（仅 Connect-IP） | 无 | 对代理模式不适用 |
| TUN / Connect-IP | 有 | 无 | 超出项目范围 |
| 端点延迟优选 | 无 | `-scan` 手动触发 | **有意增强，非对齐**（官方不扫边缘全段测延迟，详见逆向文档 §11）|

## 已知限制

1. **QUIC/UDP 被完全封锁时无回退**——代理模式官方自身也是 H3-only（详见逆向文档 §5）。
2. **重连是惰性的**——空闲断线不会后台恢复，下一个请求承担重连延迟；断线瞬间在途隧道全部中断，客户端需自行重试。
3. **UDP 不走隧道且无法关闭**——数据报以真实源地址发出；需严格避免泄漏的场景应在上层限制客户端只用 TCP。
4. **PQ 密钥交换无法对齐**（Go 标准库无 `P256Kyber768Draft00`）。
5. **无 DNS 健康跟踪与故障转移**——上游列表按序尝试，失败仅按传输错误分类处理。
6. **隧道内无 Happy Eyeballs**——有 A 记录就一律用 IPv4，不做连通性探测（与 `-ip` 无关）。
7. **没有并发上限**——默认 `:40000` 且默认不要求认证；UDP 关联没有任何限制（每个占 2 个 socket + 4 个 goroutine）。不可信网络中务必绑回环或设认证。
8. **注册信息不会刷新**——端点一直沿用，需 `-del` 后重新 `-reg` 更新。可用 `-scan` 在每次启动时临时优选最低延迟端点（结果不写回 `reg.json`，仅本次运行有效）。
9. **扫描延迟与正式连接可能不完全一致**——`-scan` 用轻量 QUIC 握手 RTT 排序，不等 H3 SETTINGS；正式连接仍走完整流程并在失败时回退。每次启动付一次扫描开销（默认 ≤45s，可 `-scan-ports 443` 加快）。

## 文档

- [WARP MASQUE 代理协议逆向分析与 warp-go 实现](docs/warp-masque-reverse-engineering.md) — 对官方 `warp-svc 2026.3.846.0` 的符号表 + 范围反汇编逆向分析、每个结论的 `[二进制]`/`[运行时]` 证据标注、与本实现的逐项对照。

## 许可证

（见仓库根目录 LICENSE 文件或遵循 Cloudflare WARP 相关服务条款；本项目为独立的第三方实现，非官方产品。）
