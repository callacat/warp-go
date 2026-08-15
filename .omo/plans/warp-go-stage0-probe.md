# 阶段 0：Android 根因自主判别方案（不依赖东哥）

> 作者：码农｜日期：2026-08-15
> 配套：`.github/workflows/android-root-probe.yml`（workflow_dispatch 触发，三个独立判别 job）
> 定位：主计划《warp-go-refactor-2026-08-15》阶段 0 的执行载体。

## 1. 为什么改为自主判别

东哥 2026-08-15 新指示：**阶段 0 不依赖东哥做任何事**——不索取 debugdiag 包、不要求真机对照。替代路径：

| 原方案（依赖用户） | 自主替代（CI/代码主动） |
|---|---|
| 索取 v0.5.27 debugdiag 包 | Job1 静态枚举泄漏面（代码即证据，必要时 CI 构建 debugdiag APK 自产数据） |
| 真机同网络对照（实验 0） | 判别决策表（§5）：模拟器数据面 + 泄漏面观测 + 静态取证，把「我们的锅 / ISP 的锅」归属收敛到代码面 |
| 用户关 H3 复测（实验 2） | Job3 模拟器 adb 关 QUIC 前后对比 |
| 用户写官方客户端对照（实验 6） | Job2 上游能力查证 + 文档明示路径 |

**重任务规则**：构建 APK / 跑模拟器 / 拉上游全量历史等重活一律走 GitHub Actions（本机无 SDK/NDK/模拟器且轻量 LXC）；本机只保留 Python 文本审计脚本（`.github/workflows/scripts/udp-egress-audit.py`，无 Go 依赖，秒级可跑）。

## 2. 三个判别 job 与假设映射

| Job | 验证 | 对应假设 | 期望 → 下一步 |
|---|---|---|---|
| **1 udp-egress-audit**（静态，零成本） | 枚举 `androidvpn/relayUDP`（非 53 拦截的 UDP 物理直连）、DNS 拦截盲区（仅 `198.18.0.1:53`）、`udpKind` 泄漏分类 | H1 UDP/HTTP3 直连 / H2 DNS 视图未统一 | 命中 → 泄漏面在代码面成立，H1/H2 不依赖真机即实锤架构缺陷；未命中 → 复核 |
| **2 upstream-udp-support**（静态） | 上游 badafans 是否有 UDP over MASQUE 实现（`git grep` + `git log`） | H1 修复路径 | 有 → H1 走 UDP-in-MASQUE；无 → 只能 QUIC 降级 H2 + 文档明示 |
| **3 emulator-data-plane**（模拟器） | CI 模拟器装 debugdiag APK：TUN 数据面（隧道 TCP CONNECT 是否通）+ udp.tsv quic/dns 泄漏行 + 关 H3 对比 | H1/H2 运行面复现；H3 排除 | 数据面通 + quic 泄漏存在 → 泄漏面运行实证；关 H3 后无 quic 行 → QUIC 直连是唯一泄漏源 |

## 3. 各 job 运行说明

- **触发**：`gh workflow run android-root-probe.yml --ref main`（或 Actions 页面手动）；`inputs.jobs` 可单选（`udp-egress-audit,upstream-udp-support,emulator-data-plane`）。
- **Job1**：产出 `stage0-udp-egress-audit.md` artifact；本机随时可跑 `python3 .github/workflows/scripts/udp-egress-audit.py` 复核。
- **Job2**：产出 `stage0-upstream-udp-support.md`；`git grep`/`git log` 查证上游 UDP 隧道化能力。
- **Job3**：构建 x86_64 debugdiag APK（`abiFilters` 已含 x86_64）+ `reactivecircus/android-emulator-runner`。**已知调参点（首版框架，首次在 CI 校准）**：VpnService consent 预授权（appops ACTIVATE_VPN，失败 fallback `input tap`）；注册/启动 VPN 的 UI 自动化（uiautomator dump 校准）；debugdiag zip 拉取与断言脚本（tun0 增量 / quic 行 / firstByteMs）在真实产物上扩展。

## 4. 已知限制（诚实标注）

- 模拟器物理出口 = CI 网络，**不等于东哥手机所在 ISP**——无法复现"用户 ISP 封锁/劣化"。Job3 定位是**数据面完整性与泄漏面运行观测**，不是替代真机重现。
- 因此「ISP 的锅」只能由决策表（§5）从代码面 + 数据面 + 用户历史证据（v0.5.24 决定性实验、v0.5.25 日志实锤已在 CHANGELOG 存档）**逻辑收敛**，无法像素级复现。

## 5. 判别决策表（阶段 0 验收输出 → 阶段 5 决策）

| 证据组合（Job1∩Job2∩Job3） | 根因归属 | 阶段 5 路径 |
|---|---|---|
| 泄漏面命中(1) + 上游无 UDP-in-MASQUE(2) | H1 代码面实锤：QUIC 直连泄漏是结构缺陷（无论 ISP 是否封锁） | QUIC 降级 H2（或文档明示限制） |
| 泄漏面命中(1) + 上游有 UDP-in-MASQUE(2) | H1 可行性成立 | UDP-in-MASQUE 修复 |
| DNS 盲区命中(1) + 模拟器出现 kind=dns 行(3) | H2 实锤：DNS 映射 miss → 裸 IP 走隧道 → 边缘不可达 | 扩展 53 拦截 + 回源视图转换 |
| 数据面通(3) + 无 quic/dns 泄漏(1/3) | H3 残留或用户网络侧 | 边缘端口 443 候选 + KeepAlive 对照；余者文档化限制 |

## 6. 与主计划的衔接

- 主计划 §阶段 0 以此文件 + workflow 为实现载体；结论写回主计划 §阶段 5 或降级为文档说明。
- 验收口径不变：**「用户真机打开境外网站 + warp=on」** 仍是最终验收，但判别（归因）阶段不再阻塞在东哥配合上。