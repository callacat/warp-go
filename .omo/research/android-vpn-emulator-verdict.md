# Android VPN 模拟器复现判定 —「境外流量打不开」未在模拟器复现（trace.log 实验证据）

> 类别：复现实验结论 + 真机问题归因假设
> 日期：2026-08-16
> 数据源：`trace.log`（596 行，从 redroid 模拟器 `/data/local/tmp/trace.log` 拉回，VPN 运行 ~30 分钟）
> 状态：只读分析，未改任何代码；结论已独立复核（见 §2 计数）。

---

## 1. 实验概要（测了什么）

- **环境**：redroid11 模拟器 `192.168.1.207:5555`（x86_64 API30，`ro.debuggable=1`）。TUN 已通（ops 已映射 `/dev/net/tun`、cap-add NET_ADMIN/NET_RAW），`establish()` 不再崩。
- **方法**：按 redroid 观测策略，容器内预埋后台循环日志（`tun 状态 / 路由规则数 / ping 1.1.1.1 / ping 223.5.5.5 / app 进程`），窗口期不依赖 live adb，事后拉回 `trace.log` 分析。
- **观测窗口**：08:24:05 → 08:54:46（约 30.7 分钟，596 条采样）。
- **探针含义**：
  - `ping11` = 1.1.1.1（境外/Cloudflare）可达性；
  - `pingCN` = 223.5.5.5（国内/阿里）可达性；
  - `rules:12` = VPN 未建立（直连路由），`rules:19` = VPN 全量路由生效；
  - `app:<pid>` = 前台 app 进程存活。

## 2. trace 证据与独立复核（本次对全文重新计数）

| 指标 | 计数 | 结论 |
|---|---|---|
| 总行数 | 596 | 全量采样 |
| `tun:none`（VPN 未生效） | 71（08:24:05→08:27:55） | 前 55 条 `app:dead`，08:27:06 起 `app:3765` |
| `tun:yes`（VPN 生效） | 525（08:27:58→08:54:46） | **26 分 48 秒持续在线** |
| `ping11:ok` | 596 / 596 | **无一行 FAIL** |
| `pingCN:ok` | 596 / 596 | **无一行 FAIL** |
| `ping11:FAIL` / `pingCN:FAIL` | 0 | 全文 `grep -i fail` 为 0 |
| `tun:yes` + `ping11:ok` + `pingCN:ok` 同时成立 | 525 / 525 | 每一条 VPN-on 采样都是双可达 |
| `tun:yes` + 非 `rules:19`/非 ok/进程死 | 0 | 所有 VPN-on 采样均为满健康态 |
| `app:3765` | 541（08:27:06 起连续） | app 进程全程存活 |

**判定复核**：525 条 `tun:yes` 全部为 `rules:19 + ping11:ok + pingCN:ok + app:3765`，一条异常都没有。结论成立且可复现计数。

## 3. 判定（verdict）

**在该模拟器上，Android VPN 工作正常：tun0 建立、路由生效、境外与国内同时可达、30 分钟稳定。模拟器不复现真机「VPN 开启后境外打不开」的 bug。**

一个关键含义：`ping11` 走的是 **app → tun0 → gVisor → QUIC/UDP → WARP 边缘 → 1.1.1.1 → 回包** 的整条代理链路（境外流量被 WARP 隧道接管），并非本地绕行。因此模拟器上 525/525 的 `ping11:ok` 证明 **QUIC/UDP 到 WARP 边缘的通道是通的**。真机故障的边界不在 warp-go 的隧道实现本身（模拟器与真机跑的是同一份代码），而在**真机所处网络对 QUIC/UDP 出向的处置差异**。

## 4. 真机问题领先假设

**ISP/运营商对蜂窝与 WiFi 上的 UDP（QUIC）限速或丢包**：

- WARP 隧道 = **MASQUE over HTTP/3（QUIC），即 UDP**（`tunnel/client_conn.go`：quic-go + http3，端口候选依次尝试；另有 `socketProtector` 保护 UDP socket 免自身 TUN 回环）。
- 桌面端用有线宽带出墙，UDP 不被限 → QUIC 握手/保活正常 → 境外通。
- 手机走运营商网络（蜂窝/WiFi 出口），常见运营商对非 443/TCP 的 UDP 报文限速、丢包或 QoS 降级 → QUIC 握手超时或保活被掐 → 境外流量全部卡死。
- 而 **CN 流量走 `direct`（protect fd 直连，TCP/IP）**，不依赖 UDP，与运营商 UDP 限速无关 → 223.5.5.5 始终 ok。这与「真机开 VPN 后国内通、境外不通」的症状完全吻合。
- 支持性旁证：`b99bcd1` 正是针对「网络 UDP 抖动 / 边缘偶发慢响应」导致共享 QUIC 连接被误拆、境外流批量殉葬的重连自伤问题——代码里已把「UDP 抖动导致单次 CONNECT 超时」列为真实故障模式（见 §5）。

需要真机实验才能最终确认：同一手机在「运营商网络 vs 家用宽带」下对照开 VPN 测境外，若宽带下正常、运营商网络下失败，则假设坐实。

## 5. 已合并的相关修复（与本次判定无关，但为真实改进，应保留）

- `b99bcd1` — **fix(tunnel): 共享 QUIC 连接重连自伤**：拆线判定窗口化（`egressProbeInterval` 20s 探测 + 连续失败计数），止住网络瞬断时境外流批量殉葬；新增 `client_conn.go` / `client_socks5.go` / `masque_connect_test.go`。
- `c5644ef` — **fix(android): establish() 补捕 IllegalStateException**：内核建接口失败不再崩溃。

即使真机问题是运营商 UDP 限速（非代码缺陷），这两处也让隧道在瞬断/建接口失败时更稳，属于净改进。

## 6. 建议的后续诊断 / 改进（暂不实现，供评估）

### 6.1 传输层切换开关（治本方向）
现状：隧道只有 QUIC/UDP（HTTP/3 MASQUE）一种传输，**无任何替代**（grep 确认 tunnel/ 无 wireguard/TCP 传输分支）。若真机确为运营商 UDP 限速，产品层面只能换传输绕过。
建议：调研 WARP 边缘是否支持非 UDP 传输（如 WireGuard，历史上 1.1.1.1 客户端支持；或 TLS/TCP 承载方案），若支持，在设置页加「隧道传输」开关（QUIC / 备选传输），并在检测到 UDP 出向故障时提示切换。

### 6.2 「UDP 探针」诊断（治标方向，最快落地）
现状：状态页无从判断「运营商掐 UDP」还是「app 坏了」。
建议：在状态页加一项诊断——对 WARP 边缘 IP:目标端口发轻量 UDP/QUIC 探针（测握手成功与 RTT），把结果分级展示：

- **UDP 探针通过** → 网络侧 UDP 正常，问题在 app/隧道链路；
- **UDP 探针超时/丢包、但 TCP/ICMP 通** → 基本锁定运营商限速 UDP，提示用户换传输（配合 6.1）或换网络。

这样用户开 VPN 连不上境外时，一眼区分「网络问题」与「应用问题」，避免反复无头绪重装。

### 6.3 故障阶段归因日志 + 网络身份标记（辅助复现）
现状：真机故障发生时，日志无法定位卡在哪个阶段（DNS？UDP socket 建连？QUIC 握手？H3 CONNECT？）。
建议：QUIC 连接建立/重连路径按阶段打点（UDP socket → QUIC 握手 → H3 CONNECT），出错时记录 `network type（cellular/wifi）+ operator + rssi/signal`。真机电报故障时能直接归属「握手没出去（网络掐 UDP）」还是「握手成功但 CONNECT 失败（app/边缘）」，并与 6.2 的探针结果互证。

**优先级建议**：6.2（纯诊断、改动小、立即帮助用户归因）→ 6.1（真机验证假设后治本）→ 6.3（与 6.2 共用打点，收尾）。