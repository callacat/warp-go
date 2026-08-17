# 真机调试 findings（2026-08-17 凌晨）

> 设备：PKG110（OPPO 系/一加），Android 15
> adb：192.168.1.155:5555（局域网，东哥指出 100.64.0.5 是 Tailscale 地址，VPN 会吞 TS 回包）
> 网络：WiFi 192.168.1.x（家用宽带），运营商卡片=中国电信/中国联通（双卡）
> 数据：/sdcard/trace_real.log（566 行，23:44-00:42 覆盖 VPN 开/关）

## 核心数据（全部实测）

### 基线（无 VPN）— tun:0 段 524 行
- ping 1.1.1.1（境外 ICMP）：OK 378 / FAIL 146 = **丢包 28%**
- ping 223.5.5.5（国内 ICMP）：OK
- http 探测：**http:000 × 110，成功 0**（http:- 553 未测）
- UDP 探测：udp11B:45 有回包（**UDP 不被封**）

### VPN 开启 — tun:1 段 37 行
- tun0 建立、rules 12→69（全量路由生效）
- ping11：OK 35 / FAIL 2（隧道内 ICMP 基本通）
- http 探测：**http:000 × 7，成功 0**

### 决定性对比
| 指标 | 无 VPN | VPN 开 |
|---|---|---|
| ping 1.1.1.1 | 28% 丢包 | 5% 丢包（隧道内反而更稳）|
| http 外网 | **0 成功（110 次 000）** | **0 成功（7 次 000）** |
| UDP 回包 | 45B 有 | 45B 有 |

## 结论（修正先前的"运营商封 UDP"假设）

**这台真机无论开不开 VPN，HTTP 外网探测全部失败（0 成功）**。
- 无 VPN 直连时 http:000 就高达 110 次 → **不是 warp-go 的问题**（不开 VPN 一样断）
- UDP 有回包、ICMP 大多通 → **不是"运营商封 UDP"**
- 真机当前 WiFi 网络对**境外 TCP/HTTP 建立连接基本失败**（ICMP/UDP 通但 TCP 层不通）→ 指向 WiFi 出口/路由器对境外 TCP 的干扰（QoS/防火墙/透明代理误伤）或真机网络栈问题

这与桌面/Linux（有线+不同出口）正常形成对照：**是这台手机当前所处 WiFi 出口的问题，不是 warp-go 隧道问题**。

## 下一步建议
1. 同一手机切蜂窝数据（关 WiFi）再测一轮 —— 若正常则坐实 WiFi 出口问题
2. 或换一台手机/同一 WiFi 其他设备测 HTTP —— 确认是出口还是手机
3. warp-go 侧已有 b99bcd1 + c5644ef 两处改进（真实收益，保留）

## 待办（东哥已记）
- 码农"类 A2A"功能（a2a-laoma.sh 已存在于 ~/.claude/，是码农→老马单向；双向需再做）

## 2026-08-17 追加测试 (100.64.0.6 PJD110 A14) — 见 deviceB-working-vs-A.md

- ops：VPN 正常！tun0 172.16.0.2、google 302、cloudflare 200、ping11 1ms 0 丢包、60s 稳定。
- 码农独立复现（同日）：tun0 UP 期间外网 HTTP 全通属实（curl 多轮 302/200/200）；
  **但会话 3.9 分钟后 VPN 自停（Go 侧 rollback/stop）→ 之后 google 直连被 DNS 投毒超时**。
  完整健康 vs 坏数据对照见 deviceB-working-vs-A.md。
