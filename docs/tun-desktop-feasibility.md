# 桌面 TUN 模式可行性评估（解决 WebSSH 等不遵守 HTTP 系统代理的应用）

> 调研日期：2026-08-04
> 背景：用户反馈 WebSSH / Tailscale 等应用在 HTTP 系统代理下无法访问（浏览器预解析域名→IP，
> 代理层只能按 IP 走 geoip，`geoip:private` 把 CGNAT/内网 IP 误判为 private 而直连失败）。
> SOCKS5 协议模式可用，但用户希望**所有流量**（含不遵守代理的应用、DNS、UDP）都被接管 → 考虑桌面 TUN。

## 结论

**桌面 TUN 可行，且是跨平台最稳健的方案。** 复用仓库已有的 `sing-tun v0.8.11`
（`androidvpn/` 已在用）+ `androidvpn/decision.go`（纯逻辑、宿主可编译）。透明代理方案只在
Linux 上更简单；Windows 上严格劣于 TUN（Wi-Fi fast-path 导致不可靠）；macOS 上两者都需要
admin，TUN 更有先例。

## 1. sing-tun 是否支持跨平台桌面 TUN？—— 是，它就是主流方案

`github.com/sagernet/sing-tun v0.8.11` **已是 go.mod 直接依赖**，且就是 sing-box /
mihomo(Clash.Meta) / Clash Verge Rev 的 TUN 实现，Linux / Windows / macOS 开箱支持：

| 平台 | sing-tun 后端 | 机制 |
|---|---|---|
| Linux | `/dev/net/tun` + iproute2 策略路由 + `nftables`(`auto_redirect`) | `tun.New(Options{Name, AutoRoute:true})`；`AutoRedirect` 用 nftables 重定向，避免 TPROXY，避开 Docker bridge 冲突（sing-box 官方推荐） |
| Windows | **wintun**(内部 `CreateAdapter` + `winipcfg` 路由/DNS + WFP `strict_route`) | 签名驱动以 `wintun.dll` 交付——wireguard-go 有 RCDATA 资源内嵌模式（单文件）；无驱动安装步骤。`strict_route`(WFP 阻 53 端口) 需 Win10+ |
| macOS | **utun**(`AF_SYSTEM` socket + `IoctlCtlInfo` + `SIOCAIFADDR` + `RTM_ADD` 路由) | utun *创建*可免 root；IP 指派 + 加路由需 admin |

**栈选择要注意：** `androidvpn.go` 强制 **gVisor** 用户态栈（Android 无内核栈集成，且承载了
v0.8.0 `udpnat` panic 的 `UDPTimeout/ICMPTimeout` 修复）。桌面用 **`system`/`mixed`**
（内核 TCP + gVisor UDP）即可，是 sing-box 默认。Context7 指出 gVisor *LinkEndpoint* 仅
Linux/Darwin，且 Clash Verge Rev #2261 显示 Windows system 栈有防火墙 quirk —— 桌面按平台选栈
是相对 Android 的主要代码差异。

**版本：** 保持 `v0.8.11`（与 sing v0.8.0 / sing-box 1.13.x 对齐）。已知 Windows `system`/`mixed`
TCP NAT 并发 bug（sing-box #4224，仅按源端口做 key）上游已修。

**参考：** https://github.com/sagernet/sing-tun（tun_windows.go/tun_darwin.go）·
https://sing-box.sagernet.org/configuration/inbound/tun/ ·
https://wiki.metacubex.one/en/config/inbound/tun/ · https://github.com/xjasonlyu/tun2socks ·
https://www.wintun.net/

## 2. 各平台权限要求

| 平台 | TUN 提升 | 透明代理提升 | 说明 |
|---|---|---|---|
| Linux | root 或 `CAP_NET_ADMIN`(建 TUN/路由/nftables) | root(iptables/nft)——**相同** | GUI 常用 polkit/systemd 服务；`auto_redirect` 解决 Docker 冲突 |
| Windows | **管理员**(wintun + winipcfg + WFP) | 管理员(WinDivert/WFP)——相同 | Win10+ 才有 WFP `strict_route`；`wintun.dll` 内嵌=单 exe |
| macOS | utun admin + 路由；**或** Apple 支持 `NEPacketTunnelProvider` + `com.apple.developer.networking.networkextension` entitlement(付费账号、签名；直分=Developer ID + **sysex**) | admin(`pfctl` rdr)——相同，无 entitlement 捷径 | Clash Verge/sing-box GUI 走 utun+admin；免 admin 只能走 NE 路径(打包成本大) |

**关键洞察：** TUN **不增加**相对透明代理的权限负担——两者在三平台都要提升。唯一真正的额外成本
是 macOS 想免 admin 时的 Apple 认证路径。

## 3. 更简单的替代？透明代理——只在 Linux 上更简单

在现有 SOCKS5 之上套透明代理前端可行，但各平台现实不均：

- **Linux（成熟）：** iptables `REDIRECT/TPROXY` 或 nftables 重定向 → 本地监听读 `SO_ORIGINAL_DST`/
  `IP_TRANSPARENT` 转发进现有代理拨号路径。经典 redsocks 模式，低成本、文档多。sing-box 的
  `auto_redirect`(nft) 本质就是它，且已集成进 TUN。
- **Windows（差）：** 用户态透明代理（WinDivert，如 NukaColaM/WinTproxy / Arryboom/WinTproxy）
  在 **Wi-Fi 上硬失败**——NDIS/firmware fast-path 在过滤驱动之下构造出站 TCP，出站包到不了
  WinDivert；用户态伪造 TCP 对端架构上不可能（反欺骗）。正确做法 DNAT/SNAT 只在以太网有效。
  可靠捕获 = 内核 WFP 驱动 = 签名驱动负担。**wintun TUN 完全规避**——在路由后、虚拟接口层捕获。
- **macOS（无收益）：** `pf` 包过滤 `rdr`——需 root（同 TUN），按端口(80/443)，pfctl 重载即复位，
  无维护良好的 Go 库。等于用更烂的方式重写 sing-tun 的 darwin utun。
- **通用：** 透明代理前端三平台都要 root，加连接跟踪 + 原目标接续 + DNS 劫持——即把 TUN 栈
  最难的一半用更多内核交互重写一遍。

## 4. 推荐：复用 `androidvpn/decision.go` + sing-tun 做桌面 TUN

**共享代码故事很实在。** `decision.go` 已是 `//go:build android || linux`，纯宿主可编译逻辑
（`Config` / `decideAction`(proxy/direct/reject + VPN-proxy 兜底) / `resolveAction` /
`TunnelDial`/`DirectDial`）+ `reject` 与 socket-protector 模式。`androidvpn.go` 的 `Vpn` 结构、
`Handler`(`NewConnectionEx`/`NewPacketConnectionEx`)、relay 循环 ~90% 可复用。桌面需要的改动：

1. **TUN 设备创建抽象**——Android 传 `FileDescriptor`(VpnService)；桌面调 `tun.New(Options{
   Name, MTU, Inet4/6Address, AutoRoute:true})`。sing-tun 两路径都支持，一个小接口分开。
2. **DNS**——Android 依赖 VPN 提供的 DNS。桌面需 TUN 可达的 DNS（sing-box `dns_address`/
   DNS 劫持模式）或 `native` 系统 DNS。这是最不直观的工作项。
3. **栈选择**——桌面 `system`/`mixed` vs Android 强制 gVisor；Windows 需 `wintun.dll` 内嵌单文件。
4. **提升 UX**——admin 弹窗 / polkit / systemd 服务或 helper 模式；macOS 决策点见下。

**工作量 / 风险表：**

| 路径 | 工作量 | 风险 | 备注 |
|---|---|---|---|
| **桌面 TUN(复用 sing-tun v0.8.11)** | Linux ~1-2 周；+Windows ~1-2 周(DLL 内嵌/admin/防火墙)；+macOS ~1 周核心(或 +2-4 周 Apple NE 免 admin) | **低-中**——库被 sing-box/mihomo 大量验证；风险在路由/DNS 冲突(Docker→`auto_redirect`)、防火墙拦截、admin UX | **推荐** |
| **透明代理前端** | Linux 低-中；Windows **高**(Wi-Fi)；macOS 中-高(pf hack) | Linux 低；**Windows 高**；Windows 无 UDP | 仅作 Linux 补充 |
| **保持 SOCKS5 + 修应用** | 无 | n/a | 作为免权限默认 OK；WebSSH 这类应用无法通用修复——这正是问题本身 |

**底线：** 实现**桌面 TUN(N 复用 `androidvpn/decision.go` + sing-tun v0.8.11)**最稳健——就是
sing-box/Clash Verge Rev 已验证的路。保留 SOCKS5 作为零权限默认。**macOS** 先 `raw-utun + admin`
（对齐 Clash Verge），只有免 admin 成为硬需求才走 `NEPacketTunnelProvider`/sysex。**透明代理**
仅作为 Linux 可选增强，非跨平台答案。

## 实现里程碑建议

1. **M-1 桌面 TUN 核心（Linux 优先）**：抽出共享 TUN handler + decision.go；新增 desktop TUN
   设备创建（Name+AutoRoute）；栈选 system/mixed。Linux 验证 WebSSH/Tailscale。
2. **M-2 Windows**：`wintun.dll` 内嵌（RCDATA），admin 提权 UX，防火墙放行，strict_route。
3. **M-3 macOS**：raw utun + admin 弹窗（对齐 Clash Verge）；评估 NE/sysex 是否值得。
4. **M-4 DNS 收敛**：TUN 内 DNS 劫持 → WARP DoH（避免泄漏，与现有隧道内 DoH 一致）。

## 来源

sing-tun 代码(tun_windows.go/tun_darwin.go) · sing-box tun 文档 · mihomo tun 文档 ·
sing-box #4224(Windows system/mixed NAT bug) + #2261(Clash Verge Windows gVisor/system) ·
wintun.net + wireguard-go RCDATA 内嵌 · WinDivert Wi-Fi fast-path 失败文章 ·
xtls 透明代理指南 · 内核 TPROXY 文档 · Apple NE entitlement + NEPacketTunnelProvider(TN3134) ·
tun2socks。

调研工具：context7(sing-tun/sing-box/tun-rs) · gh_grep(sing-box 内 sing-tun 真实用法) ·
websearch_web_search_exa(sing-box TUN 文档 / WinDivert/WFP 透明代理 / macOS NE entitlements /
Linux TPROXY / wintun 内嵌) · codegraph_explore(现有 androidvpn/decision.go + 栈复用评估)。