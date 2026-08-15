#!/usr/bin/env python3
"""阶段0 判别实验 Job1：UDP/DNS 直连泄漏面静态审计（不依赖真机/debugdiag 包）。

对 android-tun-expert 三个根因假设做「代码面」取证：
  H1 UDP/HTTP3 直连被墙      —— 所有不经隧道的 UDP 出口
  H2 DNS 视图未统一          —— DNS 拦截仅覆盖 198.18.0.1:53 之外的漏直连
  H3 边缘 4443 UDP QoS       —— 隧道拨号 socket 地址族/边缘端口侧
结果写 stage0-udp-egress-audit.md（供 GitHub Actions upload-artifact 收集）。

用法：python3 .github/workflows/scripts/udp-egress-audit.py（repo 根目录执行）
说明：本机/CI 均可跑（无 Go 工具链依赖，纯文本 scan）。
"""
import os
import re
import sys

REPO = os.path.dirname(os.path.abspath(__file__))
for _ in range(4):
    if os.path.isdir(os.path.join(REPO, "androidvpn")):
        break
    REPO = os.path.dirname(REPO)

report = []
def scan(path, pat, desc, label):
    full = os.path.join(REPO, path)
    if not os.path.exists(full):
        return
    with open(full, encoding="utf-8", errors="replace") as f:
        for lineno, line in enumerate(f, 1):
            if re.search(pat, line):
                report.append(f"- `{path}:{lineno}` {desc}\n  `{line.rstrip()}`\n")

scan("androidvpn/androidvpn.go", r"relayUDP|net\.DialUDP",
     "H1：UDP 直连中继（TUN 上非 53 拦截的 UDP 全部经本机栈物理直连，不经 WARP 隧道）", "H1")
scan("androidvpn/androidvpn.go", r"Port == 53.*DNSInterceptAddr|DNSInterceptAddr.*Port == 53",
     "H2：DNS 拦截判定——仅 198.18.0.1:53 被拦截，其余 DNS 目标（114.114.114.114:53 等）漏直连", "H2")
scan("androidvpn/decision.go", r"udpKind|return \"dns\"|return \"quic\"",
     "H1/H2：debugdiag udpKind 分类（53→dns 泄漏 / 443→quic 直连泄漏）——代码自认泄漏面", "H1")
scan("androidvpn/dns.go", r"DNSInterceptAddr = netip",
     "H2：DNS 拦截地址 198.18.0.1（RFC2544 保留段）", "H2")
scan("tunnel/masque.go", r'listenFamily := (\"udp4\"|\"udp6\")',
     "H3：隧道拨号 socket 显式 udp4/udp6（v0.5.27 已修双栈 ENETUNREACH）", "H3")
scan("tunnel/masque.go", r"KeepAlivePeriod|MaxIdleTimeout",
     "H3：隧道 QUIC 保活/空闲参数（端口优先级未对照项）", "H3")

os.makedirs(REPO, exist_ok=True)  # 输出到工作区
out_path = os.path.join(os.getcwd(), "stage0-udp-egress-audit.md")
with open(out_path, "w", encoding="utf-8") as f:
    f.write("# 阶段0 Job1：UDP/DNS 直连泄漏面静态审计\n\n")
    f.write("> 依据：android-tun-expert 三根因假设（H1 UDP直连 / H2 DNS视图 / H3 边缘UDP QoS）\n\n")
    if report:
        f.write("## 代码面证据\n\n")
        f.write("".join(report))
        f.write("\n## 初步结论（供判别）\n\n")
        f.write("- 若 H1 命中：TUN 非 53 流 UDP 存在物理直连 → 浏览器 QUIC:443 直连泄漏在代码面成立，"
                "无论用户 ISP 是否封锁，架构上就存在『不经 WARP』的 UDP 面。\n")
        f.write("- 若 H2 命中：DNS 拦截只覆盖 198.18.0.1:53 → 其他 DNS 目标映射 miss → 裸 IP 走隧道 "
                "→ 边缘不可达的链路在代码面成立。\n")
    else:
        f.write("## 未命中（全部出口已走隧道？需人工复核）\n")
print(f"written: {out_path} ({len(report)} hits)")