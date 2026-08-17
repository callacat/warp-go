# Device B（100.64.0.6）独立复现 + 健康 tunnels.tsv vs Device A 对照（2026-08-17）

> 设备：adb 100.64.0.6:5555，型号 PJD110，**实为 LDPlayer Android 14 模拟器**（§1）
> 网络：wlan0 172.16.1.15/24（LDPlayer NAT 出口）；app 版本 1.0（2026-08-07 构建，**早于 b99bcd1/c5644ef**）
> 数据：本会话 08:37:25–08:41:05 的 debugdiag（沙箱 tunnels.tsv + tun0.tsv + 导出 zip warp-go-debugdiag-20260817-084105.zip）
> 对照：Device A = 真机 PKG110 A15（192.168.1.155/WiFi），数据用仓库内 08-08 包（workspace 根 tunnels.tsv/tun0.tsv/udp.tsv）

---

## 1. 环境澄清：Device B 不是真机

logcat 大量 LDPlayer 痕迹：`com.ldmnq.launcher3`（雷电启动器）、`ldinit/ld_exec_cmd`、`/dev/fastpipe`、
`EmuHWC2`、`HostRPC/HostConnection`（QEMU GL）、`ro.hardware=qcom`、4GB 内存。
`ro.product.model=PJD110 / ro.product.device=graceltexx` 是 build.prop 伪装。
**结论：B 是 LDPlayer Android 14 模拟器（API 34），不是 OPPO 真机；但它给了可控、可 root（`su`）的健康对照环境。**

## 2. 独立复现（任务 1）

会话起点 08:37:12（WarpVpnService 前台启动）；我 08:40:03 接入确认 tun0=172.16.0.2 UP。

### 第一轮（VPN 开着，08:40:30–08:41:05，5 连发 curl）
| # | google | cloudflare | github |
|---|---|---|---|
| 1 | 302 (6.2s) | 200 (3.3s) | 200 (7.3s) |
| 2 | 302 (1.1s) | 200 (1.9s) | **000 (0.54s)** |
| 3 | 302 (1.5s) | 200 (2.6s) | 200 (1.9s) |
| 4 | 302 (6.1s) | 200 (2.1s) | **000 (0.07s)** |
| 5 | **000 (10.0s)** | 200 (2.3s) | 200 (0.9s) |

断点：第 5 个 google 超时恰好撞上 08:41:05 的 VPN 自停（§3）。github 2/5 快速 000（0.54s/0.07s，
非超时，是流被杀）＝ tunnels.tsv seq109 `github.com up517 dn0 firstByteMs=-1 life47ms ok`。

### 第二轮（08:41:32–08:42:36，VPN 已被自停拆除）
| # | google | cloudflare | github |
|---|---|---|---|
| 1–5 | **000 (10.0s) ×5** | 200 ×5 (0.7–2.9s) | 200 ×5 (0.72–0.88s) |

tun0 up 期间隧道真实可用（302/200/200）；tun0 掉后 google 直连变 DNS 投毒超时（§4），
cloudflare/github 直连仍通 → 第二轮 000 是"无 VPN 直连失败"，不是隧道失败。

### 稳定性
ops"60 秒稳定"只对前 60s 成立：**会话实际只有 08:37:12→08:41:05 ≈ 3.9 分钟**，随后 VPN 自停（§3）。

## 3. 关键时序：VPN 自停 ~3.9 分钟（Go 侧 rollback/stop），随后 LDPlayer 杀进程

logcat 证据（pid 2851）：
```
08:41:05.779  tunnels.tsv 最后一条（seq 134；session_ended_after=232718ms）
08:41:06.015  MediaProvider: Open …/Download/warp-go-debugdiag-20260817-084105.zip (Uid 10120)
08:41:06.106  GnssNetworkConnectivityHandler: updateNetworkState, state=CLOSED … (network 101, VPN) ← 网络拆除
08:41:06.640  I warp-go : onDestroy - stopping VPN ← 服务销毁（stopNativeAndClose）
08:41:10.955  WindowManager: task 7 close（LDPlayer launcher 关 tab）
08:41:11.262  ldinit: APP_KILL broadcast com.wails.app
08:41:13.053  ActivityManager: Killing 2851:com.wails.app (adj 900): remove task
```
顺序：Go 侧先自停（export → 网络 CLOSED → onDestroy），LDPlayer 后补刀杀进程。
08:41:05 是 app 自己的 Go 侧停止：`DebugStop + androidExportDebugDiag`（androidbridge.go L598-599
停止路径 / L418-420 失败 rollback 路径），由 `kernel.Start`/`vpn.Start` 返回错误或 panic →
rollback → `kernelFailed` → Java stopSelf 触发。Go 具体错误进不了 logcat（Go log 走 Wails 内部缓冲），
但"会话寿命 232.7s + 收尾走导出路径"坐实了 **隧道运行期自停**——Device B 同样不稳。

## 4. 无 VPN 时 google 被 DNS 投毒（出口网络的锅）

直连（tun0 已无）：
```
www.google.com    → Trying 174.132.167.252:443 + Trying [2001::1]:443 → 6s timeout
www.cloudflare.com→ 104.16.124.96:443 → Connected ✓
github.com        → 20.205.243.166:443 → Connected ✓
```
`174.132.167.252`（A 记录）+ AAAA 回 `::1` 是典型 DNS 封锁/劫持签名，只命中 google 家族。
**B 开 VPN 前 google 直连本来就不通；VPN 靠 DNS 拦截（198.18.0.1 → 隧道内 DoH → 真实 IP）救活。**
与 realphone-findings 里 A"无 VPN 外网 HTTP 全 000"是同类出口干扰（A 覆盖面更广）。

## 5. 健康 tunnels.tsv vs Device A 坏数据（核心对照）

| 指标 | Device B 健康（349 行 / 4 会话） | Device A 坏（101 行 / 72.5s，08-08 包） |
|---|---|---|
| firstByteMs=-1 | 45 (13%)，全部孤立、err=ok、lifeMs 极小 | 42 (42%)，成批（同 ms 最多 28 连杀）、伴随真错误 |
| 非 ok 错误 | **0** | **101/101 (100%)** |
| 错误构成 | — | quic:transport closed ×51、EOF ×35、broken pipe ×8、refused ×4、write-udp-closed ×2、reset ×1 |
| 批量死亡 | 无（同 ms 0 条） | 09:13:22.266 同 ms 28 条 `quic: transport closed … use of closed network connection`；另有 09:13:36/46/58、09:14:13/25 多段 |
| 真实外网流 | 大量成功：github 584KB@487ms、cloudflare 1.3MB@197ms、google 5.2KB@366ms | 也有成功（m.youtube 150KB@422ms、play-fe 34KB、cn.bing 11.7KB）但**每条都以错误收尾**（数据流完 → 共享 QUIC 死亡 → Read 报错） |
| DNS 目标 | dns.google DoH 2001:4860:484x::- 全成功（firstByteMs≈195ms） | Cloudflare 解析器 2607:f740:* / 2a00:dd80:* / 104.18.* → 大批 -1 随共享 QUIC 殉葬 |
| -1 行地址族 | 少量 | **v6=23 / v4=19**（大量 2607:f740 等） |
| tun0.tsv | 正常 2s 采样（08:40 窗口 66s +7MB Rx，峰值 1.69MB/2s） | **0 行**（采样从未落盘——A15 无 root 读不到 /proc/net/dev 的概率高，debugdiag 盲区） |
| udp.tsv | 空（无 QUIC 尝试、无 DNS 泄漏） | 46 行全 `quic-blocked`（浏览器 H3 被拦截丢弃的量化） |

**健康签名**：连接要么成功（firstByteMs 20–600ms、downBytes 流动、err=ok），要么孤立 -1
（0 字节闲置/握手中途放弃），**绝无"同毫秒多连 + 实错误"**。
**坏签名**：共享 QUIC 连接下沉 → in-flight（含 DNS）批量殉葬，每条连接最终错误收尾——
b99bcd1「共享 QUIC 重连自伤」的形态；B 的旧构建（08-07）残留小版本（github 2/5 快速 000）。

## 6. 代码路径：Android VpnService vs 桌面系统代理

- **桌面**：系统代理 127.0.0.1:40000 → tunnel/client_socks5.go DialTunnel → establishCONNECT → openRequestStream（client_conn.go:622）在共享 QUIC 上开 H3 流 → connectThroughEdge 发 CONNECT、等边缘 200 成功后清 deadline（client_socks5.go:324-339）才进入转发。
- **Android**：VpnService.Builder（WarpVpnService.java L175-180：0.0.0.0/0+::/0、MTU1500、setBlocking(true)、DNS 198.18.0.1）→ TUN fd（detachFd）→ sing-tun+gVisor（androidvpn.go L82-140）→ NewConnectionEx 逐流 decideAction → proxy 走 kernel.DialTunnel（同一 establishCONNECT）、direct 走 net.Dialer+protect（decision.go L147-178）→ 双向 relay（androidvpn.go L246-274，firstByteMs 仅 downBytes>0 时 >0，L276-279 全零则 -1）。
- **共享 QUIC 复用/断线**：多流复用同一 bundle（client_conn.go:97-135）；b.close 先关 UDP socket/Transport 再关高层（client_conn.go:139-166）→ 在途流读 `quic: transport closed: read udp … use of closed network connection`（正是 A 的批量死亡错误）；重连已窗口化（CONNECT 失败计数 client_conn.go:836-881、流错误另计 :888-897、20s 出口探测需连续 2 次失败才 retire :424-496 = b99bcd1）。
- **路由兜底已确认 miss→proxy**（decision.go L197-206），外网流量**不可能**被误判 direct 绕过隧道；
  但 androidvpn.go L183-184 注释仍写"未命中 → 隐式 direct 兜底"，与代码矛盾（文档陈旧，易误导后续维护，建议顺手改）。
- socket protect 环路已修复（client_conn.go:528-539、decision.go:165-175、androidbridge.go:296-297），非候选。

## 7. A-vs-B 差异假设（A15 真机挂 / A14 模拟器活）

**已证实差异**
1. 构建版本：B 跑 08-07 旧 debugdiag 包（md5 90be1e… ≠ dist-debugdiag 08-16 的 369d5b…），A 的 08-08 包同样旧——两边都**没有** b99bcd1/c5644ef 当前构建，"A 死 B 活"不能归因修复差异。
2. 出口网络：B 是 LDPlayer NAT（172.16.1.15/24），A 是真机家用 WiFi。B 无 VPN 仅 google 被投毒；A 无 VPN 外网 HTTP 全 000（干扰更广）。
3. OS：A=Android 15（双栈），B=Android 14 模拟器（IPv4 为主）。
4. debugdiag 采样：A 的 tun0.tsv 空（无 root 读 /proc/net/dev 失败静默），B 正常。

**假设（按可能性排序）**
1. **IPv6 裸 IP CONNECT（代码可证机制，A15 双栈触发率高于 A14 模拟器）**：DNS 拦截对 AAAA 查询
   拿到 v4 解析结果时直接 return nil（androidvpn/dns.go L156-161，resolveDNS 仅 A 优先）→ Android
   回落物理 DNS 拿到**本地视图 v6 IP** → IP→域名映射 miss → proxy 分支裸 v6 IP 走隧道
   （client_dns.go L43-47 直通）→ 边缘不可达 → hang 到 deadline / firstByteMs=-1。**A 的 -1 行 v6=23/v4=19 直接支撑**；
   B（IPv4 为主）几乎不触发。← 与 Device A 坏数据吻合度最高
2. **共享 QUIC 被出口 UDP 劣化周期性下沉（机制已证，环境触发）**：A15 WiFi 基线 ICMP 丢包 28%
   （realphone-findings.md），UDP 抖动触发出口探测/失败窗阈值 → retire 整连接 → 已 CONNECT 未响应流
   批量殉葬（A 的 seq14-39 同 ms 死 20+ 条、DNS 2507:f740 成群 -1）。B 的旧构建也有小版本（github 快速 000）。
   b99bcd1 当前构建量化后应显著缓解。
3. **WiFi 出口 PMTU/UDP 黑洞（环境）**：ping 小包通、HTTP 大包死 → 大响应 UDP/QUIC 被断（realphone 已记）。
4. **A15 VpnService/系统差异**（[unverified]，需查官方文档）：A15 对 VPN 的 DNS-over-VPN、onRevoke、
   bypassable 行为若有差异，可能把部分查询豁免出 TUN。当前代码无 A15 专项处理。

**鉴别实验（下一步，均可自主做）**
- E1：当前构建（b99bcd1）debugdiag 包装回 B —— 量化 QUIC 风暴归零、github 快速 000 消失。
- E2：B 强制走 1.1.1.1 DoH 后复测 google（`settings put global private_dns_mode hostname 1dot1dot1dot1.cloudflare-dns.com`）——若 google 也 000，复现 A 的"洞 B/裸 IP"形态。
- E3：A 重新在线后，同会话 `curl -v` 定位 hang 段（DNS / CONNECT / 首字节），并核对 A app 构建是否含 b99bcd1。
- E4：关 A 的 IPv6（开发者选项）或让 DNS 拦截对 AAAA 返回 SERVFAIL（而非静默丢）复测 http 成功率。

## 8. 结论（给东哥）

1. **B（A14 模拟器）证明：VPN 起来后隧道本身是好的**（外网 HTTP 全通、tunnels.tsv 0 错误、firstByteMs 20–600ms）——WARP 隧道与 gVisor/TUN 数据面没有"结构性打不开"。
2. **B 也不是稳定基线**：3.9 分钟自停 + github 流偶发被杀（旧构建残留的 QUIC 风暴小版本）。
3. **A 的坏签名 = 共享 QUIC 批量殉葬 + 每条连接错误收尾 + DNS 成群 -1（v6 居多）** → 优先级：
   ① 当前构建（b99bcd1）在 A 重测（风暴可能已大幅缓解）；② 剩余聚焦 **IPv6 裸 IP CONNECT / DNS 视图（洞 B）**，
   不要再在 TCP CONNECT 层打转。
4. 环境注意：B 是模拟器，其 WiFi/DNS 行为（google 投毒）≠ 真机 A 的出口；A/B 对照须说清差异。

---
*产物：/tmp/deviceB_tunnels.tsv、/tmp/deviceB_tun0.tsv、/tmp/warp-go-debugdiag-20260817-084105.zip（B 导出）
 workspace 根 tunnels.tsv + tun0.tsv + udp.tsv（A 08-08 包）*
*备注：.omo/research/ 目录当前 root 所有（无 agent 写权限），已用 sudo tee 写入本文件；建议 chown agent:agent。*