# E1：当前构建（b99bcd1+c5644ef）debugdiag 回装 Device B——QUIC 风暴修复量化（2026-08-17）

> 设备：adb 100.64.0.6:5555，LDPlayer Android 14 模拟器（PJD110 伪装，API 34）
> APK：dist-apk-new/apk-new/app-release.apk（sha256 d13eb595…，含 b99bcd1 + c5644ef），`install -r` 于 09:01:41，注册信息随 /data 保留（无需一键注册）
> 数据：会话 09:02:45–09:18:50 的 debugdiag（dist-debugdiag/e1/warp-go-debugdiag-20260817-091850.zip）
> 对照：deviceB-working-vs-A.md 同设备旧构建（08-07，无 b99bcd1/c5644ef）会话 08:37:25–08:41:05

---

## 1. 会话与稳定性：自停消失（232.7s → 966.4s，手动停）

| 指标 | 旧构建（08-07） | 当前构建（E1） |
|---|---|---|
| UI 启动时间 | 08:37:12（logcat） | 2026-08-17T09:02:45.226+08:00 |
| 会话终点 | **自停** 08:41:05（Go 侧 rollback → export → onDestroy → LDPlayer 杀进程） | **手动** tap 停止 09:18:50（导出 zip → 干净停止） |
| session_ended_after | 232718ms（3.88 分钟） | **966366ms（16.1 分钟）** |
| 预埋 trace（3s×260） | — | 09:03:39–09:16:43 **260 条全 tun0:1、ping11/pingCN 双 OK、app:5637 未变**；0 条 tun0:0 |
| tun0.tsv（E1 窗） | — | 484 采样无 >5s 断档；累计 Tx 0.36MB / Rx 9.57MB，峰值 1.34MB/2s |

结语：观察窗覆盖到 9:16（uptime 13 分钟）仍在线，手动停止前**未出现任何自停**——超出旧构建 3.9 分钟毙命点 4.1 倍且无失效。

## 2. curl 5x 三站（VPN 内，t=10s）

**当前构建（09:04:07–09:05:26）**
| # | google | cloudflare | github |
|---|---|---|---|
| 1 | 302 (6.19s) | 200 (8.92s) | 200 (7.53s) |
| 2 | 302 (1.15s) | 200 (3.49s) | 200 (2.40s) |
| 3 | 302 (2.37s) | 200 (8.19s) | 200 (5.29s) |
| 4 | 302 (1.11s) | 200 (9.23s) | 200 (4.03s) |
| 5 | 302 (2.14s) | 200 (8.70s) | 200 (7.96s) |

**旧构建（对照 §2 旧文档）**
| # | google | cloudflare | github |
|---|---|---|---|
| 1–5 | 302/302/302/302/**000(10.0s)** | 200 ×5 | 200/**000(0.54s)**/200/**000(0.07s)**/200 |

**目标达成：github 快速 000 = 0（旧构建 2/5）。** google 无第 5 次超时。cloudflare/github 此轮首字节偏慢（4–9s）但全部成功——网络时延波动，非失败。`session_ended_after=966366ms` 佐证。

## 3. tunnels.tsv 分块对照（单文件含 5 历史会话，块3=旧构建、块4=当前构建）

| 指标 | 旧构建块3（134 行 / 232.7s） | 当前构建块4（33 行 / 966.4s） |
|---|---|---|
| firstByteMs=-1 | 18 (13%)，孤立、小 life、err=ok | 4 (12%)，**全 v6 `connection refused`**（2606:4700::6810:7b60，life=0，快拒非静默杀） |
| 静默杀流签名（fb=-1 + dn=0 + life<100 + err=ok） | **seq109 github.com up517 dn0 life47 err=ok**（=curl 2/5 快速 000 的落盘形态） | **0 条** |
| github 主流 | 2 成功（fb487/388）+ 1 静默杀（seq109） | **5/5 `up:EOF down:H3_NO_ERROR (local)`** fb459–599ms dn592KB |
| 真实完成流 | err=ok 134 | H3_NO_ERROR 14 + clean-EOF 6（google 302/cloudflare 200/github 200 全记录） |
| 显式错误 | 0（err 列只有 ok） | quic-transport-closed 8 + connection-refused 4 + broken-pipe 1 |
| 同 ms 批量 | 13 组（全 ok，最大 16；并行浏览器/curl 流） | 5 组（**最大 3**，全为残余裸 IP 流在 bundle 轮换时殉葬） |
| fb>0 区间 | 20–608ms | 185–599ms |

**关键读法（err 列语义升级）**：旧构建流被杀也记 `err=ok`（把问题藏进 fb=-1）；当前构建 err 列如实写 `down:quic: transport closed: read udp … use of closed network connection`——8 条 transport-closed **全部发生在 firstByteMs>0 之后**（dn 已读完 724KB 等），全是 CONNECT 后闲置的裸 IP 残余流（2607:f8b0:400e×5、172.217.115.4×2、2606:4700×1）在共享 QUIC 轮换时被析构，**主 https 流本身 100% H3_NO_ERROR**，curl 层零感知。broken-pipe 1 条出现在手动停止瞬间（tun 拆除，FCM 5228 保持连接断）。

**结论：旧构建的"共享 QUIC 风暴小版本"（github 偶发快死）在当前构建已净除**——用户可见 0 快速 000、落盘 0 静默杀、批量殉葬上限 16→3 且全为已完成流。

## 4. IPv6 裸 IP CONNECT 残余（b99bcd1 作用域外，留 E2/E4）

4 条 `connection refused` 全部指向 cloudflare AAA 记录 `2606:4700::6810:7b60`（=104.16.123.96 的 v6），fb=-1 life=0 **快速拒绝**——curl 回落 v4 后 `www.cloudflare.com` H3_NO_ERROR 成功，用户无感。与 deviceB-working-vs-A.md §7 假设 1（IPv6 裸 IP CONNECT）吻合，但 B 上是**快拒**而非 A 的**挂死**（A 双栈/出口差异）。此问题不在 b99bcd1 修复范围，A 端重测前仍是第一怀疑对象。

## 5. 其他佐证

- udp.tsv：仅表头，0 行——无 QUIC 拦截、无 DNS 泄漏（健康签名）。
- tun0.tsv 连续采样 484 行，验证 A 的 tun0 空文件是 A15 无 root 读 /proc/net/dev 的盲区（B 正常）。
- app 进程 pid 5637 贯穿全会话，无重启。

## 6. Verdict（给东哥）

1. **修复量化确认**：当前构建（b99bcd1+c5644ef）在 Device B：
   - github 快速 000：**2/5 → 0/5**（净除）；
   - 静默杀流签名（fb=-1+dn=0+err=ok）：**消失**；
   - 自停：**232.7s → 966.4s 无自停**（观察至 13 分钟 + 手动停止），稳定超出旧构建 4.1 倍。
2. **残留噪声（非用户可见）**：8 条 post-first-byte 裸 IP 残余流 transport-closed（同 ms 批 ≤3）+ 4 条 v6 connection refused（快拒，v4 回落成功）。
3. **Device A 未决问题的指针不变**：b99bcd1 已把"共享 QUIC 批量殉葬"这条从 A/B 共同嫌疑中排除；A 重测当前构建后若仍有残余，**聚焦 IPv6 裸 IP CONNECT / DNS 视图（洞 B，假设 1）**，而非 TCP CONNECT 层。
4. 提示：当前构建 err 列语义比旧构建丰富（成功/优雅 EOF/传输关闭/拒绝分开记），对照旧文档"非 ok 错误=0"时须按列语义换算，勿误读为新回退。

---
*产物：dist-debugdiag/e1/warp-go-debugdiag-20260817-091850.zip（+tun0.tsv/tunnels.tsv/udp.tsv 解包同目录）*
*trace：设备 /sdcard/e1_trace.log（260×3s，09:03:39–09:16:43，全在线）*
*对照基线：deviceB-working-vs-A.md §2/§3/§5 表*