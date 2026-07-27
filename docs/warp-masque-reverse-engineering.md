# WARP MASQUE 代理协议逆向分析与 warp-go 实现

## 1. 文档范围

本文记录两件事：

1. 对 Cloudflare 官方 Linux 守护进程 `warp-svc` 的逆向分析结果
2. `warp-go`（本项目）的实现，以及它与官方行为的对照

**逆向对象**：`warp-svc`，版本 `2026.3.846.0`，ELF 64-bit x86-64 PIE，**未 strip**（Rust 符号完整保留），73,761,888 字节。

**证据标注约定**——本文每条协议结论都属于以下三类之一，不混用：

| 标注 | 含义 |
|---|---|
| **[二进制]** | 从 `warp-svc` 反汇编/字符串直接读出，附函数名与地址 |
| **[运行时]** | 从本机 `/var/lib/cloudflare-warp/` 或官方客户端日志观测到 |
| **[未验证]** | 合理推断但未取得直接证据，不作为实现依据 |

未标注的内容均为 `warp-go` 自身的实现说明。

---

## 2. 逆向方法

### 2.1 工具链

最终采用的是**符号表 + 范围反汇编**，而非交互式反汇编器：

```bash
# 符号定位
nm -C warp-svc | grep -E '<pattern>'

# 范围反汇编（必须限定范围，全量输出以 GB 计）
objdump -d --start-address=0xSTART --stop-address=0xEND -C warp-svc

# 字符串
strings warp-svc | grep -iE '<pattern>'

# 读 .rodata 常量
python3 -c "f=open('warp-svc','rb');f.seek(0xADDR);print(f.read(200))"
```

该二进制的 `.rodata` **虚拟地址等于文件偏移**（已验证：vaddr `0x58c350` → file `0x58c350`），因此可以直接用 `seek` 读常量，无需换算。浮点常量用 `struct.unpack('<f', d)` / `'<d'`。

IDA Pro 9.3 对这个 73 MB 的 Rust 二进制**分析超时**（30 分钟无响应后被中止，未产出数据库），故未使用。上述轻量工具链反而更快。

### 2.2 两个必须知道的陷阱

**陷阱一：tracing 调用点的字符串长度编码为 `2n+1`,而非 `n`。**

```
0x60c8a8 "Running protocol racing connector"  (33 字节)
         配对的立即数是 movq $0x43  (67 = 2×33+1)
```

把这类调用点当作普通 `(ptr, len)` 读会得到垃圾。本项目在分析过程中曾因此得出过一个错误结论。

**陷阱二：Rust 泛型单态化会让同一个源函数出现在多个地址。**

`new_primary_doh`、`new_primary_tls`、`start_fallback_resolver_update_task` 等都有克隆体。任选其一分析即可，但不要误判为不同的函数。

**推论**：panic / tracing 字符串常被内联进 `std::thread::local::LocalKey<T>::with`，用"最近符号"法定位所属函数会错误归属。例如 `"Proxy mode only supports MASQUE"` 的引用点就在 `LocalKey::with` 内部，实际属于 `warp-connection/src/forward_proxy.rs`。

### 2.3 依赖组件还原 **[二进制]**

从符号表可确定 `warp-svc` 的技术栈：

| 组件 | 用途 | 证据 |
|---|---|---|
| `quiche` / `tokio_quiche` | QUIC 传输 | `quiche::Config::*`、`tokio_quiche::settings::*` |
| `hickory-dns` | DNS 解析与本地 DNS 服务器 | `hickory_resolver::*`、`hickory_proto::*` |
| `boring` (BoringSSL) | MASQUE 路径 TLS | `boring::pkey::PKeyRef::public_key_to_pem` |
| `rustls` + `tokio-rustls` | DoH 路径 TLS | `hickory_proto::rustls::client_config` |
| `h2` | DoH 的 HTTP/2 | `h2::client::SendRequest` |
| hyper fork (`httx_hyper`) | REST API 与 H2 隧道 | `httx_hyper::proto::h2::*` |
| `http-capsule` | RFC 9297 capsule | `http_capsule::Header::read_from_buf` |

注意 **MASQUE 用 BoringSSL、DoH 用 rustls**，是两套独立的 TLS 栈。

---

## 3. 注册协议

### 3.1 两步流程

注册必须分两步完成，`POST /v0/reg` 不接受 MASQUE 密钥类型：

**Step 1** — `POST https://api.cloudflareclient.com/v0/reg`

```json
{
  "key": "<base64(Curve25519 公钥, 32 字节)>",
  "key_type": "curve25519",
  "tunnel_type": "wireguard",
  "install_id": "", "fcm_token": "",
  "tos": "<RFC3339，形如 2026-07-26T16:00:00.000-07:00>",
  "model": "PC", "serial_number": "<16 位十六进制>",
  "os_version": "", "locale": "en_US", "warp_enabled": true
}
```

Curve25519 公钥必须是**合法的曲线点**——随机 32 字节会被拒绝并返回 `code 1051 "Invalid public key"`。实现上需要生成私钥、按 WireGuard 规范做 clamp、再用基点做标量乘。

**Step 2** — `PATCH https://api.cloudflareclient.com/v0/reg/{id}`

```json
{
  "key": "<base64(PKIX DER 编码的 ECDSA P-256 公钥)>",
  "key_type": "secp256r1",
  "tunnel_type": "masque"
}
```

带 `Authorization: Bearer <step1 返回的 token>`。

**只有 Step 2 的响应里才有边缘端点信息**，这是必须走两步的实际原因。

### 3.2 请求头 **[二进制]**

```
User-Agent: WARP for Linux
CF-Client-Version: linux-<version>
Content-Type: application/json
```

`warp_api_client::version::cf_client_version` @ `0x25776e0` 动态拼接 `linux-` 前缀；字面量 `"WARP for Linux"` 和 `"cf-client-version"` 位于 `warp-api-client/src/client/mod.rs` 附近的 rodata。

### 3.3 响应结构

响应统一包一层 `{"result": {...}}`。Step 2 的 `result.config` 含：

```
config.peers[0].public_key          边缘公钥（PEM），用于证书固定
config.peers[0].endpoint.v4         形如 "162.159.198.2:0"，端口位是占位
config.peers[0].endpoint.v6         形如 "[2606:4700:103::2]:0"
config.peers[0].endpoint.ports      [443, 500, 1701, 4500, 4443, 8443, 8095]
config.interface.addresses.v4/.v6   分配给本设备的隧道内地址
```

端点地址带 `:0` 占位端口，需剥离后与 `ports` 数组组合。

---

## 4. MASQUE 传输模式

### 4.1 三种模式 **[二进制]**

`MasqueHttpVersion` 有三个取值：`h3_only` / `h2_only` / `h3_with_h2_fallback`。对应三种连接器，均在 `warp_connection::connection_config::ConnectionConfig::retry_with_captive_portal_until` 处单态化：

```
SingleProtocolConnector<'_, h3_tun::MasqueTunnel>
SingleProtocolConnector<'_, h2_tun::H2Tunnel>
ProtocolRacingConnector<'_, h3_tun::MasqueTunnel, h2_tun::H2Tunnel>
```

### 4.2 协议竞速 **[二进制]**

`ProtocolRacingConnector::race_protocols` @ `0x1b5fb00`。`race_pq` @ `0x1b60b30` 与 `race_non_pq` @ `0x1b5f870` **尾调用同一个 `race_protocols` 函数体**，差别仅在一个后量子标志位。

执行顺序由 rodata `0x60c8a8` 起的连续字符串给出：

```
"Running protocol racing connector"
"PQ racing failed, trying non-PQ"
"Non-PQ racing failed"
```

即：先跑一整轮启用 PQ 的 H3/H2 竞速，整轮失败后再跑一轮禁用 PQ 的。

**单轮内 H3 与 H2 同时发起，没有次要协议的延迟启动**。六条结果日志覆盖了双向的胜出/失败等待：

```
0x60fbb3 "Primary protocol won race, upgrading connection"
0x60fbe2 "Primary connection failed, waiting for secondary"
...
```

Primary = H3/MASQUE，Secondary = H2。重试间隔 2 秒（`connect_attempt_delay` @ `0x1ac23f0`，函数体即 `mov $0x2,%eax; xor %edx,%edx; ret`）。

### 4.3 后量子密钥交换 **[二进制]**

`ClientCertificateHook::configure_builder` @ `0x1db7b40`：

- PQ 标志位置位时，**只提供一个 PQ 组：`0xFE32` = `P256Kyber768Draft00`**（不是 MLKEM768）
- 随后**总是**追加经典组 P-256(23) / P-384(24) / P-521(25)，常量数组位于 rodata `0x62620c`
- **X25519 从不出现**

`PostQuantumSupport` 序列化为 `enabled_with_downgrades` / `enabled`，通过结构体 `TunnelPostQuantumSettings { config, use_post_quantum_for_connection }` 传递。

---

## 5. 代理模式的硬约束：只能用 H3

这是决定 `warp-go` 架构的核心结论。**H2 回退不适用于纯代理客户端**，有三条独立证据：

**证据一** — 前向代理的每目标 CONNECT 硬绑 H3。
`<warp_connection::forward_proxy::connect_tcp::ConnectTcpProxy as proxy::ConnectProxy>::connect_tcp` @ `0x1a52d10` 使用 `httx::http3::body::H3Body`，**不存在 H2 变体**。

**证据二** — H2 的数据面只有 Connect-IP 一种形态。
全二进制中 `EdgeConnection` 的实现集合仅三个：`h2_tun::H2CapsuleEdgeConnection`（send `0x1d62370` / recv `0x1d621e0`）、`h3_tun::MasqueEdgeConnection`、WireGuard。H2 那个是 capsule/datagram socket，喂给 TUN 包循环，无法承载 plain CONNECT 字节流。

**证据三** — H2 握手无条件声明 Connect-IP。
`<H2Tunnel as TunnelProtocol>::upgrade_happy_eyeballs_connection` 向绝对 URI `https://cloudflareaccess.com` 发 CONNECT，携带 `cf-connect-proto: cf-connect-ip` 与 `pq-enabled: true|false`。该请求头构造**没有任何条件分支**。

此外 `warp-connection/src/forward_proxy.rs` 直接硬失败：

```
0x60cc92  "Forward proxy requires a MASQUE tunnel"
0x60ce98  "Proxy mode only supports MASQUE"
```

**结论**：即使官方客户端默认 `h3_with_h2_fallback`，该回退也只对 TUN 模式生效。代理模式下官方自身同样是 H3-only。

### 5.1 H2 隧道的其他细节 **[二进制]**（仅供参考，不影响本实现）

- `<H2Tunnel as TunnelProtocol>::restrict_endpoints` @ `0x1d75240` 把端点列表收窄为 v4 或 v6 端口为 443 的条目；若无匹配则原样返回
- 数据面为 RFC 9297 capsule，`http_capsule::Header::read_from_buf` @ `0x2115e40`，**只接受 capsule type 0 (DATAGRAM)**，其余丢弃并记录 `capsule_ty`
- 传输为 hyper HTTP/2 over BoringSSL over TCP
- 统计走 TCP 指标（`update_tcp_stats` @ `0x1d7b000`，含 `latency_ms` / `bytes_rexmit` / `loss_rate`）

---

## 6. QUIC 传输参数 **[二进制]**

全部读自 `<tokio_quiche::settings::quic::QuicSettings as Default>::default` @ `0x21b4990`，以及连接 ID 生成器。

| 参数 | 官方值 | 证据 |
|---|---|---|
| 源连接 ID 长度 | **20 字节** | `SimpleConnectionIdGenerator::new_connection_id` @ `0x21bb3a0`，`movq $0x14` 写入 len 与 cap，分配 `mov $0x14,%esi` |
| 连接接收窗口 | 10,000,000 | `+0xd0` = `0x989680` |
| 流接收窗口 | 1,000,000 | `+0xd8` / `+0xe0` / `+0xe8` = `0xf4240` |
| 最大并发流（bidi/uni） | 100 / 100 | `+0xf0` / `+0xf8` = `0x64` |
| 最大发送 UDP 载荷 | 1350 | `+0x108` / `+0x110` = `0x546` |
| 拥塞控制 | cubic | `movl $0x69627563`（`"cubi"`）+ `movb $0x63`（`'c'`） |

**keepalive 是推导值而非常量**。`warp_edge::h3_tun::idle_timeout::IdleTimeout::keepalive_interval` @ `0x1e22bb0` 由 idle timeout 计算，钳制在 **[5s, 50s]**（浮点常量 5.0 @ `0x58c350`、50.0 @ `0x58b2fc`、1e9 @ `0x58ccc4`）。

> **[未验证]** 20 字节连接 ID 是官方的既成事实，但"使用较短连接 ID 会导致边缘返回 PROTOCOL_VIOLATION"这一因果关系本项目**未做对照实验**。实现上采用 20 字节是为了对齐官方，而非基于已证实的故障机理。

---

## 7. 服务端公钥固定 **[二进制]**

官方客户端**强制**校验边缘公钥。`warp_edge::masque::ClientCertificateHook::validate_server_pubkey` @ `0x1db7d70`：

1. 用 `boring::pkey::PKeyRef::public_key_to_pem` 把对端叶证书公钥渲染为 PEM
2. 与注册时下发的端点公钥做 `bcmp` 逐字节比较
3. 不匹配时记录 `warp-edge/src/masque.rs:214`：
   `"Server's public key does not match what is expected."`（rodata `0x6262ca`）

这一步是必要的：MASQUE 的 SNI 是固定的通用名，证书链由私有 CA 签发，标准链校验无法建立信任，认证完全依赖公钥固定。

SNI 常量位于 `0x6262ca` 之后的连续区域：

```
consumer-masque.cloudflareclient.com          Connect-IP (TUN) 模式
consumer-masque-proxy.cloudflareclient.com    代理模式
zt-masque[-proxy].cloudflareclient.com        Zero Trust
```

---

## 8. DNS 架构

### 8.1 官方在代理模式下不启动 DNS 代理 **[二进制] [运行时]**

这是一个反直觉但明确的结论。`ConnectionDnsMode` 的 `Display` 实现 @ `0x19f3aa0` 只输出两个字面量：`"No DNS"`（`0x60678d`）与 `"DNS Proxy"`（`0x606793`）。在 `WarpProxy` 操作模式下守护进程报告 **No DNS**，三个 `DnsProxy` 构造器一个都不调用；前向代理自带 `hickory_resolver::Resolver` 做名称解析。

本机运行时状态印证：`settings.json` 为 `{"operation_mode":{"WarpProxy":null},"proxy_port":40000}`，`cfwarp_daemon_dns.txt` 为 0 字节，`bound-dns-ports.txt` 不存在。

### 8.2 dns_proxy 的三种上游 **[二进制]**

仅在 TUN 类模式下启用：

| 构造器 | 传输 | 证据 |
|---|---|---|
| `new_primary_doh` | DoH over HTTP/2 | 端口 443（`mov $0x1bb,%ecx` @ `0x23fcd63`） |
| `new_primary_tls` @ `0x1cd59b0` | DoT | 端口 853（`mov $0x355,%ecx` @ `0x1cd5a8f`） |
| `new_primary_tunneled` @ `0x1cd6490` | 明文 DNS，UDP+TCP | 端口 53（`mov $0x35,%ecx`），并把调用方给的 32 字节 socket 地址写入每个 `NameServerConfig` 的 `+0x50`（bind_addr），使包从隧道接口发出 |

解析器选项相对 hickory 默认值被覆盖：**每查询超时 4 秒、重试 0 次**（`DEFAULT_OPTIONS` @ `0x23f9600`），且 `get_doh_and_dot_options` @ `0x23fea20` 把 `num_concurrent_reqs` 强制为 **1** —— 名称服务器扇出被关闭，并发完全由 HTTP/2 提供。

### 8.3 DoH 传输细节 **[二进制]**

**这是 `warp-go` 直接对齐的部分。**

**多路复用机制**：`dns_proxy::resolver::MultiplexedDohProvider` 是自定义的 `hickory_resolver::name_server::ConnectionProvider`。每个 hickory `NameServer` 持有**恰好一条** DoH 连接，存放在 `Arc<futures_util::lock::Mutex<Option<HttpConnection>>>` 中，首次查询时惰性建立。并发不来自连接池或信号量，而来自 HTTP/2 本身：`h2::client::Connection` 驱动任务独立 spawn，每次查询克隆 `h2::client::SendRequest<Bytes>` 句柄并开一条**新的 H2 流**。

这是关键设计点——HTTP/1.1 的 keep-alive 连接是**严格串行**的，无法承载并发查询；HTTP/2 的流多路复用才能在单连接上让多个查询同时在途。

**请求格式**：RFC 8484 **wire format，方法为 POST**。请求头恰好三个：

```
content-type: application/dns-message
accept: application/dns-message
content-length: <n>
```

**没有 user-agent，没有 accept-encoding**。全二进制搜索 `application/dns-json` **零命中**，也不存在 `?dns=` 的 GET 路径——官方不使用 DoH-JSON。

URL 路径 `/dns-query` 由两条立即数指令拼出（`movabs $0x6575712d736e642f` = `"/dns-que"`，`movw $0x7972` = `"ry"`），因此 `strings | grep dns-query` 搜不到。

**上游地址**：`warp_settings::raw_settings::dns::consumer_cloudflare_dns_ips` @ `0x304d490` 分配 68 字节（`mov $0x44,%edi`）并从 rodata `0x82cb71` 填入四个 `IpAddr`：

```
162.159.36.1
162.159.46.1
2606:4700:4700::1111
2606:4700:4700::1001
```

**不是 1.1.1.1 / 1.0.0.1** —— 官方客户端自身的 DoH 从不使用公开解析器地址。可被环境变量 `CF_WARP_DNS_IP`（rodata `0x82cbb5`）或策略字段 `doh_ips` 覆盖。

主机名 `cloudflare-dns.com` **仅用作 TLS SNI 与 `:authority`，从不解析**。过滤变体 `security.` / `family.cloudflare-dns.com` 有各自独立的编译期 IP（1.1.1.2 / 1.1.1.3 系列）。

**TLS**：rustls，ALPN 恰好一个协议 `"h2"`。

**认证头**：`JwtHeaderProvider` 注入 `cf-authorization`（16 字节头名），携带 Access JWT——**仅 Zero Trust / Access 部署**使用，消费级不涉及。

### 8.4 DoH 健康跟踪 **[二进制]**

`DohHealthTracker` 是 4 个 u32 计数器（`+0x18` 总数、`+0x1c` 成功、`+0x20` 全部超时、`+0x24` 计入比率的超时）。健康判定为纯比率：**计数超时 / 总数 ≥ 0.8 即判定不健康**（double 常量 @ rodata `0x589d18`），每个监控周期评估后清零。

两点需要注意：分子只统计 27 个 `ProtoErrorKind` 分支中的 1 个；且二进制中**没有观察到不健康判定真正触发主解析器轮换**——可见路径止于 `verify_dns_proxy` 与遥测。因此这更接近可观测性设施而非故障转移机制。

`warp-go` 未实现健康跟踪：它只有单一上游列表，且失败时按传输错误分类决定是否重建连接（见 §9.4）。

---

## 9. warp-go 实现

### 9.1 定位与架构

纯代理客户端：**免 root、无 TUN、无路由改动**。

```
SOCKS5 客户端 ──► tunnel/masque.go ──► QUIC/H3 ──► WARP 边缘 ──► 目标
                        ▲
                registration/registration.go
                （注册 API、ECDSA P-256 密钥、mTLS 证书、端点与边缘公钥）
```

| 文件 | 行数 | 职责 |
|---|---|---|
| `main.go` | 153 | 参数解析、注册编排、TLS 配置组装、SOCKS5 监听循环 |
| `registration/registration.go` | 544 | 两步注册、状态持久化、边缘公钥固定回调 |
| `tunnel/masque.go` | 1233 | QUIC/H3 连接管理、SOCKS5 TCP、DoH 解析 |
| `tunnel/udp.go` | 344 | SOCKS5 UDP ASSOCIATE |

命令行参数：

```
-l      监听地址（默认 :40000，双栈）
-user   SOCKS5 用户名（与 -pass 同时给出才启用认证，RFC 1929）
-pass   SOCKS5 密码
-ip     边缘选择：4 / 6 / 显式 host:port（默认 4）
-reg    注册并退出（已有注册则跳过）
-del    注销并删除本地注册
```

注册文件固定为工作目录下的 `reg.json`，没有路径参数。**accept 循环不因瞬时错误退出。** EMFILE（文件描述符耗尽）、ECONNABORTED 这类错误是暂时的，早期实现遇到任何 accept 错误就 `break`，会让整个代理停止服务。现在按 5ms→1s 指数退避重试，仅在 `net.ErrClosed` 或收到关停信号时退出。

边缘地址不再由命令行指定，而是从注册文件读取。启动**不会**自动注册——缺少注册文件是致命错误并提示运行 `-reg`；`-reg` 幂等，已有注册时只报告不替换，以免旧注册在 Cloudflare 侧失去本地凭据而无法注销。

### 9.2 连接层

**`connBundle`** 把 udpConn、`quic.Transport`、QUIC 连接、H3 transport 打包为一个生命周期单元，重连时整组拆除，避免部分残留。

**显式 Transport 拨号**。使用 `quic.Transport{Conn: udpConn, ConnectionIDLength: 20}` 而非 `quic.DialAddr`，因为后者无法设置连接 ID 长度。

**端口回退**。注册返回 7 个端口，按序尝试，每个 8 秒超时（`perAddrDialTimeout`），成功的索引被记住供后续重连优先使用。这在 UDP/443 被封锁的网络中是必需的。若某次拨号的错误表明本机根本没有该地址族的路由（`ENETUNREACH` / `EAFNOSUPPORT` / `EHOSTUNREACH`），则直接中止整轮——剩余候选只是端口不同，结果必然相同，否则要白等 7 × 8 秒。

**边缘选择**。`-ip` 有三种取值：`4` / `6` 从注册数据里取对应地址族并遍历完整端口列表；其余值按 `host:port` 解析，替换掉注册给出的端点（只用这一个端口）。域名由**系统解析器**解析——此时隧道尚未建立，隧道内的 DoH 客户端还不可用——解析出的每个地址都作为候选，因此双栈域名在单栈主机上也能工作。

`-ip` 只决定**如何到达边缘**，不影响**隧道内**可达的地址族：目标由边缘代为连接，走 IPv4 边缘一样能访问 IPv6-only 站点。

**等待 H3 SETTINGS**。QUIC 握手完成不等于 H3 可用，需等待 `ReceivedSettings()`，否则首个请求可能失败。

**重连**是惰性的：由 `openRequestStream` 在发现连接已死或开流失败时触发，`reconnectMu` 串行化，并用 `current != stale` 判断避免多个 goroutine 重复拨号。空闲期间断线不会立即重连，下一个请求承担重连延迟。

QUIC 配置全部采用 §6 的官方值。

### 9.3 SOCKS5 与流释放

`HandleSOCKS5` 内联实现 SOCKS5，支持可选的用户名/密码认证。

**流释放是本实现中一个曾出过故障的关键点。** 边缘会保持 plain CONNECT 隧道的自己一侧直到目标关闭，因此只调用 `RequestStream.Close()`（仅关闭发送侧）不足以释放流。客户端中途消失时，读方向的 `io.Copy` 会永久阻塞，同时泄漏 goroutine 与 QUIC 流；而边缘授予的并发流数量有限，攒够若干条被遗弃的流之后，后续 `OpenRequestStream` 会**静默阻塞**——日志停在解析完成、无错误、无重连记录。

当前实现通过 `sync.Once` 保护的 `release()` 完整释放：

```go
reqStream.CancelRead(...)   // 唤醒被挂起的读方向
reqStream.Close()           // 结束发送侧
conn.Close()                // 唤醒另一个 copy
```

触发时机分两种：客户端连接异常断开时**立即**释放；客户端干净半关闭时，给响应方向 `relayDrainGrace`（30 秒）排空，超时强制释放。

> 该故障**无法用 `curl` + `kill -9` 复现**——RST 会让两个方向立刻出错退出。复现需要客户端干净关闭而边缘保持沉默。曾因此做出"修复前后无差异"的错误判断。

**每连接的 goroutine 必须随 handler 退出。** 监听 `ctx` 以便关停时强制关闭连接的那个 goroutine，若只 `<-ctx.Done()`，会因为 `ctx` 与进程同寿而**每处理一个连接就常驻一个**。实测 40 个连接后 goroutine 从 17 涨到 74，其中 40 个停在该闭包上。现在用 `select` 同时等 `ctx.Done()` 与 handler 的 `handlerDone`，实测同样负载下常驻数为 0。`tunnel/udp.go` 里本来就是这个写法，只有 `HandleSOCKS5` 漏了。

**CONNECT 交换的超时**。`SendRequestHeader` 与 `ReadResponse` 都不接受 context、自身也无超时。`connectThroughEdge()` 统一在交换前后设置/清除 stream deadline——成功后**必须清除**，否则残留 deadline 会在传输中途掐断长命隧道。超时预算优先继承调用方（SOCKS5 路径为 20 秒的 setup 预算），无预算时退回固定值。

### 9.4 DNS 解析

对齐 §8.3 的官方设计：**单条 HTTP/2 连接承载所有查询，每查询一条 H2 流**。该连接建立在一条 H3 CONNECT 流内部：

```
H3 CONNECT 到 162.159.36.1:443
  └─ TLS（SNI=cloudflare-dns.com, ALPN=h2）
       └─ HTTP/2 ClientConn
            ├─ 查询 A ──► H2 stream 1
            ├─ 查询 B ──► H2 stream 3
            └─ ...
```

请求为 RFC 8484 wire format POST，头部与官方一致（`content-type` / `accept` 为 `application/dns-message`；显式置空 `user-agent`，并用 `DisableCompression` 阻止 `x/net/http2` 注入 `accept-encoding: gzip` —— 这两项官方都不发送）。

**两级 singleflight**：

1. **名称级**——同一主机的并发查询合并为一次
2. **连接级**（`dohDial`）——冷启动时多个 goroutine 只有一个真正拨号，其余等待其结果

第二级是必需的。官方能天然做到"单连接"是因为 hickory 用 `futures_util::lock::Mutex`（异步锁）可以合法地跨 await 持有；Go 的互斥锁不能跨拨号持有——`dohConnection → dialDoH → openRequestStream → reconnect → invalidateDoH` 会重入 `dohMu`，且 Go 互斥锁不可重入。缺少连接级合并时，8 个并发首次查询会各自建立一条连接、各付一次完整的 CONNECT + TLS + H2 握手，然后丢弃其中 7 条。

**连接可用性判定用 `h2.State()` 而非 `CanTakeNewRequest()`**：后者在连接仅仅是达到服务端 `MAX_CONCURRENT_STREAMS` 而饱和时也返回 false，而饱和的连接是健康的（`RoundTrip` 会等待流槽位并遵守 context）。只有已关闭或正在关闭（GOAWAY / DoNotReuse）的连接才算失效。

**错误分类**决定是否重建连接。只有传输层失败（`errDoHTransport`）才允许退休共享连接或触发重试；DNS 层的应答（NXDOMAIN、无 A 记录、非 200 状态）以及本次查询自身的超时都不算——为这些理由拆连接会中断所有并行查询，因为 `http2.ClientConn.Close()` 会打断在途请求。

**双栈解析**。每个主机名同时发出 A 与 AAAA 两个问题，走同一条 H2 连接的两条流，因此只花一个往返；有 A 记录时优先用 A，否则用 AAAA。这与 hickory 的默认策略 `Ipv4thenIpv6` 语义一致（`warp-svc` 未覆盖该选项），但省掉了顺序查询的第二个 RTT。

WARP 是双栈的——注册时边缘同时分配 IPv4 与 IPv6 隧道地址，且边缘具备 IPv6 出口——所以只查 A 会让 IPv6-only 目标完全不可达。

> 实现提示：解析出的地址交给 `net.JoinHostPort` 组装 CONNECT 目标即可，**不要再手动加方括号**。该函数本身会为含冒号的地址加括号，重复加会产生 `[[2606:...]]:443`，边缘会直接取消该流（观测到 error code 270）。这个缺陷在只查 A 记录时不会暴露。

**缓存**按响应 TTL，钳制在 [5s, 5min]。条目数超过 `dnsCacheSweepAt`（1024）时清扫已过期项，仍超过 `dnsCacheMaxEntries`（8192）则整体丢弃——缓存是优化而非真相来源。此前只增不删：过期条目在读取时被跳过，却永远留在 map 里，浏览器类负载下会随运行时长无界增长。清扫把上限从"运行时长"变成了"单个 TTL 窗口内出现的域名数"。

### 9.5 边缘公钥固定

`Registration.PeerPublicKeyVerifier()` 用注册时保存的 `peer_public_key`（PEM）构造 `tls.Config.VerifyPeerCertificate` 回调，比对对端叶证书的 ECDSA 公钥。

与官方的 `bcmp` PEM 字节比较等价，但用 `ecdsa.PublicKey.Equal()` 实现——避免 PEM 序列化差异带来的假阴性。

旧状态文件不含该字段时降级为**警告**而非静默放行；密钥格式错误则是**硬失败**，不会退化为无固定。

TLS 配置的 `CurvePreferences` 设为 P-256 / P-384 / P-521，与官方的经典组一致且不提供 X25519。Go 标准库无法提供 `P256Kyber768Draft00`，故 PQ 部分无法对齐——但官方自身的 non-PQ 轮次证明边缘接受纯经典 ClientHello。

### 9.6 UDP ASSOCIATE

**这些数据报不经过 WARP 隧道。** plain CONNECT 是字节流隧道，无法承载数据报；承载数据报需要 Connect-IP，而那需要 TUN（见 §5）。因此 UDP 直接从本机网络栈发出：**TCP 走隧道、UDP 走本地，两者对同一目标呈现不同的源地址**。当前实现始终提供 UDP ASSOCIATE，没有关闭开关。

实现要点：每个关联使用两个 socket（面向客户端与面向目标），使方向判别不依赖源地址；首个数据报到达时**钉住客户端源地址**以防离路径劫持；拒绝 `FRAG != 0`；60 秒滚动空闲超时，并在 TCP 控制连接关闭时拆除。

---

## 10. 与官方行为的对照

| 维度 | 官方 warp-svc | warp-go | 说明 |
|---|---|---|---|
| 源连接 ID | 20 字节 | 20 字节 | 一致 |
| 流控窗口 | 10MB / 1MB | 10MB / 1MB | 一致 |
| 并发流上限 | 100 / 100 | 100 / 100 | 一致 |
| UDP 载荷上限 | 1350 | `InitialPacketSize: 1350` | 一致 |
| 边缘公钥固定 | PEM `bcmp` | `ecdsa.Equal` | 等价 |
| TLS 曲线 | PQ + P-256/384/521 | P-256/384/521 | PQ 组 Go 无法提供 |
| 代理 CONNECT | H3 only | H3 only | 一致（见 §5） |
| DoH 传输 | H2 多路复用，单连接 | 同 | 一致 |
| DoH 格式 | RFC 8484 POST wireformat | 同 | 一致 |
| DoH 上游 | 162.159.36.1 / 46.1 | 同 | 一致 |
| 解析地址族策略 | hickory 默认 `Ipv4thenIpv6`（顺序） | A/AAAA 并发，A 优先 | 语义相同，少一个 RTT |
| DoH 位置 | 隧道**外**（代理模式下用宿主解析器） | 隧道**内** | **有意分歧**，见下 |
| keepalive | 由 idle timeout 推导，[5s,50s] | 固定 10s | 在官方区间内 |
| DNS 健康跟踪 | 比率 0.8 | 未实现 | 见 §8.4 |
| H2 隧道回退 | 有（仅 Connect-IP） | 无 | 对代理模式不适用 |
| TUN / Connect-IP | 有 | 无 | 超出项目范围 |

**关于 DoH 位置的分歧**：官方代理模式下 DNS 走宿主网络栈（`No DNS` 模式，前向代理自带 hickory 解析器）。`warp-go` 选择把 DoH 放进隧道内，代价是每次冷启动多一次 CONNECT + TLS + H2 握手，收益是**避免 DNS 查询以真实源地址泄漏**。这是有意的偏离，不是未对齐。

---

## 11. 已知限制

1. **QUIC/UDP 被完全封锁时无回退**。官方 TUN 模式可退到 H2 over TCP，代理模式退不了（§5）。若需该能力，必须实现 Connect-IP + TUN/netstack，那将改变项目免 root 的定位。
2. **重连是惰性的**。空闲断线不会后台恢复，下一个请求承担重连延迟；断线瞬间在途的隧道全部中断，客户端需自行重试。
3. **UDP 不走隧道且无法关闭**（§9.6）。UDP 数据报以真实源地址发出；需要严格避免泄漏的场景应在上层限制客户端只用 TCP。
4. **PQ 密钥交换无法对齐**（§9.5）。
5. **无 DNS 健康跟踪与故障转移**。上游列表按序尝试，失败仅按传输错误分类处理。
6. **隧道内没有 Happy Eyeballs**。A 与 AAAA 并发查询，但只要有 A 记录就一律用 IPv4，不做连通性探测；若边缘到某目标的 IPv4 路径劣化，不会自动改走 IPv6。注意这与 `-ip` 无关——`-ip` 决定的是如何到达边缘。
7. **没有并发上限**。默认监听 `:40000` 且默认不要求认证，TCP 侧靠边缘的流配额提供隐式背压，UDP 关联则没有任何限制（每个占 2 个 socket + 4 个 goroutine）。不可信网络中应绑回环地址或设置 `-user`/`-pass`。
8. **注册信息不会刷新**。注册时拿到的端点一直沿用，官方客户端会周期性拉取配置更新，本实现不会；端点分配若变化，只能 `-del` 后重新 `-reg`。可用 `-scan` 在每次启动时临时优选最低延迟端点缓解（见下 §11.1，仅本次运行有效，不写回 `reg.json`）。

### 11.1 `[未对齐增强]` 端点延迟优选（`-scan`）

> **`[未对齐增强]`** —— 本节描述的是 `warp-go` 相对官方 `warp-svc` 的有意增强，**官方未做**，故无 `[二进制]`/`[运行时]` 证据可通过逆向取得；机制仅由本项目源码（`scanner/`）定义。

**官方行为**：`warp-svc` 不对边缘全段做延迟扫描优选。注册响应只回单个 IPv4 + 单个 IPv6 + 7 个端口，客户端沿此使用；`connect_attempt_delay=2s`（§4.2）是协议竞速重试间隔，与端点选择无关；`DohHealthTracker` 比率 0.8（§11 限制 5）明示不触发故障转移；官方"周期性拉取配置更新"拉取的是**注册配置**而非延迟优选结果。

**`warp-go` 增强**：`-scan` 启动前对 WARP 边缘全段——IPv4 默认 5 个 `/24`（`162.159.192/193/195/197/198`）、IPv6 默认 2 个 `/48`（`2606:4700:d0::/d1::`），`-scan-cidr` 可追加——与 `reg.json` 的 `endpoint_ports` 做笛卡尔积（`PerIPLimit` 默认 16，`/24` 取 16 个；`/32`/`/128` 单主机段唯一地址放行，不视为网络/广播），逐候选做**与正式连接同一协议栈的轻量 QUIC 握手探针**测 RTT：

- 握手即 `CloseWithError(http3.ErrCodeNoError)` 干净释放，不等 H3 SETTINGS（握手完成排序与"等 SETTINGS"排序工程上等价，省 H3 资源）；
- `ConnectionIDLength=20`、mTLS 客户端证书、边缘公钥固定（`VerifyPeerCertificate`）在探针握手阶段全程保留——握手成功即真碰到 WARP 边缘而非中间盒子；
- 族级预探对 `ENETUNREACH/EAFNOSUPPORT/EHOSTUNREACH` 整族跳过（沿用 §6.2 同义判断），避免逐端口刷爆总超时；
- 并发 `min(64, NumCPU×8)`、下限 16，受信号量约束；`TotalTimeout` 45s 硬上限通过 ctx 取消传播；按 RTT 升序取 `top-N`（默认 4）前置到 `edgeAddrs`，注册端点尾接作兜底。

**取证诚实说明**：

- 默认段、端口集来自社区扫描器交叉核实（`masx200/vwarp` 的 `DefaultIPv4Ranges`、`bepass-org/ipscanner` 的 `WithCidrList`），**非**官方二进制证据——属 `[未验证]` 级的"社区经验指引"，可能随 WARP 部署调整而过时。
- 默认 7 端口（`[443,500,1701,4500,4443,8443,8095]`）**复用** `reg.json` 的 `endpoint_ports`（属 `[运行时]` 注册响应），与官方 MASQUE 模式端口一致；不要误用只对 WireGuard 模式有效的 54 端口列表。
- 全失败回退注册端点、不致命；扫描结果**不写回 `reg.json`**，保留 `-reg`/`-del` 的幂等契约（§3）。

---

## 12. 附录：符号与地址索引

按主题排列，便于复核。

### QUIC 与传输

```
0x21b4990  <tokio_quiche::settings::quic::QuicSettings as Default>::default
0x21bb3a0  <SimpleConnectionIdGenerator as ConnectionIdGenerator>::new_connection_id
0x1e22bb0  warp_edge::h3_tun::idle_timeout::IdleTimeout::keepalive_interval
0x21ba600  tokio_quiche::settings::config::Config::new
```

### 认证与 TLS

```
0x1db7d70  warp_edge::masque::ClientCertificateHook::validate_server_pubkey
0x1db7b40  warp_edge::masque::ClientCertificateHook::configure_builder
0x1db8580  warp_edge::masque::validate_registration_keys
0x6262ca   "Server's public key does not match what is expected." + SNI 常量区
0x62620c   经典曲线组常量数组 {0x17, 0x18, 0x19}
```

### 协议竞速与 H2 隧道

```
0x1b5fb00  ProtocolRacingConnector::race_protocols
0x1b60b30  ProtocolRacingConnector::race_pq
0x1b5f870  ProtocolRacingConnector::race_non_pq
0x1ac23f0  ConnectionConfig::connect_attempt_delay      （2 秒）
0x1d75240  <H2Tunnel as TunnelProtocol>::restrict_endpoints
0x1d62370  <H2CapsuleEdgeConnection as EdgeConnection>::send
0x1d621e0  <H2CapsuleEdgeConnection as EdgeConnection>::recv
0x2115e40  http_capsule::Header::read_from_buf
0x60c8a8   竞速日志字符串区
0x60fbb3   竞速结果日志字符串区
```

### 代理模式

```
0x1a52d10  <ConnectTcpProxy as proxy::ConnectProxy>::connect_tcp
0x19f3aa0  <ConnectionDnsMode as Display>::fmt
0x60cc92   "Forward proxy requires a MASQUE tunnel"
0x60ce98   "Proxy mode only supports MASQUE"
0x60678d   "No DNS"
0x606793   "DNS Proxy"
```

### DNS

```
0x240f790  dns_proxy::resolver::MultiplexedDohProvider::new
0x2409660  <MultiplexedDohProvider as ConnectionProvider>::new_connection
0x23fcc80  dns_proxy::proxy::new_doh_resolver                （端口 443）
0x1cd59b0  DnsProxy::new_primary_tls                         （端口 853）
0x1cd6490  DnsProxy::new_primary_tunneled                    （端口 53）
0x23fea20  dns_proxy::proxy::get_doh_and_dot_options
0x23f9600  DEFAULT_OPTIONS                                   （4s 超时 / 0 重试）
0x23eefb0  dns_proxy::health_tracker::DohHealthTracker::new
0x23e8be0  <DohHealthTracker as DnsProxyHealthTracker>::health_status
0x304d490  warp_settings::raw_settings::dns::consumer_cloudflare_dns_ips
0x304d600  get_dns_override_environment_variable             （CF_WARP_DNS_IP）
0x82cb71   四个消费级 DoH IpAddr 常量（68 字节）
0x589d18   健康比率阈值 0.8
0x26ae8d0  <HttpsClientStream as DnsRequestSender>::send_message
0x26a5e80  hickory_proto::rustls::client_config
```

### API 客户端

```
0x25776e0  warp_api_client::version::cf_client_version
```
