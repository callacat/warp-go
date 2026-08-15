# warp-go 重构方案（2026-08-15）

> 汇总：码农（东哥编码助手）｜日期：2026-08-15
> **执行策略（2026-08-15 更新）**：重任务（Android 构建/模拟器/交叉编译/Docker 镜像）一律走
> GitHub Actions，本机只做轻量文本审计与 `go build/test`；阶段 0 自主判别不依赖东哥
> （见 `.omo/plans/warp-go-stage0-probe.md` + `.github/workflows/android-root-probe.yml`）。
> 依据：4 份并行调研报告（`.omo/research/`）
> - `go-architect.md` —— Go 核心隧道栈
> - `android-tun-expert.md` —— Android TUN 未决问题
> - `gui-ui-reviewer.md` —— GUI/UI 栈
> - `ci-release-auditor.md` —— CI/发布/工作流
> 配合阅读：`AGENTS.md`（接手指南）、`warp-go-reinit-2026-07-31.md`、`warp-go-android-2026-08-01.md`

---

## 1. 现状评估（各模块结论汇总）

### 1.1 总体判断

- **Go 核心架构健康**：`core.Server`/`core.Kernel` 三端（CLI/GUI/Android）复用设计良好、锁模型清晰、错误处理规范（逐层 `%w` 包装）、测试缝（依赖注入 dialer/engine）到位。
- **GUI 分层清晰**：Wails v3 + React 19 + Tailwind v4 轻量现代，Android 与桌面共用一套前端、平台差异封在 Go 侧；主要问题在**契约层与一致性**，不在结构。
- **CI 体系健康、纪律性强**：发布纪律（AGENTS §6.8）已内化，Android 真实 NDK/cgo/JNI 断言防住了 v0.5.7 式血泪；但有 2 个真实 bug 与 4 个"版本单源漏出口"。
- **Android 未决问题未解**：v0.5.13→v0.5.27 九轮修复未果，2026-08-08 放弃；调研判定最可能根因在 **UDP/HTTP3 直连泄漏 + DNS 视图未统一**（详见 §4）。

### 1.2 模块健康度总表

| 模块 | 健康度 | 核心问题 | 优先级 |
|---|---|---|---|
| `core` | 高 | `Server.UpdateGeo` 与 `Engine.UpdateGeo` 换引擎职责重叠；`Server`/`Kernel` 字段重叠 | P2 |
| `route` | 高 | `matchGeoSite` 对 geosite 大类线性全扫 O(N)；geoip 规则对域名目标在代理路径**永不命中** | P1 |
| `proxy` | 高 | 与 `tunnel` 双份 SOCKS5/UDP 实现（~400 行重复）；HTTP 转发强制 `Connection: close` | P1 |
| `tunnel` | 中-高（风险最高） | `masque.go` 单文件 2219 行、五把锁 + 双 singleflight 重连状态机；Android 未决问题核心区域 | P2 重构 / P0 排查 |
| `gui` 前端 | 高 | 前后端类型双维护（snake/camel 双重归一化已踩坑）；`geoBaseURL` 死字段；手写轮询三处重复 | P0-P1 |
| `gui` 服务层 | 中 | `runtime.GOOS=="android"` 散布 7 处；`GetStatus` 双数据源；Wails alpha 多返回值元组序列化 | P1 |
| `androidvpn` | 中 | UDP/HTTP3 直连泄漏；DNS 映射 miss（洞 B）；「miss→proxy」文档口径与代码矛盾 | P0 排查 |
| CI/发布 | 高 | sync-upstream 冲突预检测 regex 死代码；Windows CLI PE 版本恒陈旧；Docker 镜像恒 `dev`；无构建缓存 | P0 修复 |

---

## 2. 重构目标与范围（对齐东哥需求）

东哥目标拆解与现状差距：

| 目标 | 现状 | 差距 | 对应阶段 |
|---|---|---|---|
| 多平台 GUI 控制 warp-go 内核 | Windows/macOS/Linux/Android 桌面**同一套 React 前端**已实现 | 契约层不稳（类型双维护、死字段）；macOS GUI 缺 arm64 原生 | 阶段 2 / 阶段 4 / 阶段 1 |
| 系统代理模式代理流量 | 桌面系统代理（win/mac/linux）已实现并随系统状态联动 | 基本闭环；托盘操作错误用户无感知 | 阶段 4 |
| TUN 模式代理流量（Android） | VpnService + sing-tun(gVisor) 已实现 | **境外流量打不开（九轮未解）** | 阶段 0 / 阶段 5 |
| GEO 数据库分流 | `route` 引擎 + 热重载 + GEO 下载已实现，18 单测全绿 | 匹配热路径线性扫描；geoip-域名盲区；`geoBaseURL` 死字段 | 阶段 3 / 阶段 2 |

**范围决策**（调研定论，不再重新调研）：
- **不引入** i18n（东哥中文环境，不阻塞）；保留 MASQUE/QUIC 协议；保留 Gvisor TUN 栈。
- Android 修复方向由**阶段 0 判别实验**决定：是修 UDP-in-MASQUE、堵 DNS 洞 B，还是接受「UDP 直连 + 文档明示」限制。
- 重构大原则：**能由标准/技能规范的用标准**；改动隧道前先对照 sync-upstream 冲突策略（AGENTS §6.6），冲突即停、绝不自动解决。

---

## 3. 分阶段计划

### 阶段 0：Android 根因自主判别（前置门，最先做，无代码为主，不依赖东哥）

- **目标**：按 AGENTS.md §6.5「未解决问题交接」优先级，确定「境外流量打不开」的根因归属（我们的锅 / ISP 的锅），为阶段 5 决策提供依据。
- **执行策略（东哥 2026-08-15 指示）**：不自找 debugdiag 包、不要求真机对照，全部自主判别：
  - **静态分析路径**（本机即可跑，零依赖）：`.github/workflows/scripts/udp-egress-audit.py`
    枚举 UDP/DNS 泄漏面（H1 的非 53 拦截 UDP 物理直连、H2 的 DNS 拦截盲区），从代码面取证缩小假设。
  - **CI 判别实验 job**：`.github/workflows/android-root-probe.yml`（workflow_dispatch，可单选）
    ① `udp-egress-audit` 静态取证；② `upstream-udp-support` 拉上游查证 UDP-in-MASQUE 能力
    （决定 H1 修复路径可行性）；③ `emulator-data-plane` CI Android 模拟器装 debugdiag APK
    验证 TUN 数据面 + 观测 udp.tsv quic/dns 泄漏行 + 关 H3 对比（替代真机）。
  - **决策表**：证据组合 → 根因归属 → 阶段 5 路径，见 `.omo/plans/warp-go-stage0-probe.md`。
- **涉及**：无代码改动为主；如需补充 debugdiag 遥测字段（如 DNS 直连采样），小改
  `androidvpn/debugdiag.go`，由 CI 构建验证。
- **验收**：Job1/2/3 产出《阶段 0 判别证据》，决策表落定归因（根因假设收敛到 ≤1 个）；结论写回
  本计划 §阶段 5 或降级为文档说明。

### 阶段 1：发布纪律与版本单源修复（P0，低风险高价值，立即可做）

- **目标**：修 2 个真实 bug + 补版本单源漏出口 + 构建缓存提速。
- **涉及文件**：
  - `.github/workflows/sync-upstream.yml`（L121 预检测 regex：`^(<<<<<<<|>>>>>>>)` → `^\+?<<<<<<<|^\+?>>>>>>>`，并钉 git 版本或改用 `merge-tree --write-tree` 退出码）
  - `versioninfo.json`（FileVersion/ProductVersion 改回 `0.0.0.0` 占位符，恢复 CI sed 命中）
  - `Dockerfile` + `.github/workflows/docker-ghcr.yml`（`-ldflags` 加 `-X main.version=${VERSION}`）
  - `.github/workflows/build-release.yml` + `android-debugdiag.yml`（4 处 setup-go 加 `cache: true`、setup-node 加 npm 缓存、test job 删冗余 `go build`、加 `concurrency` group）
- **验收**：CI 全绿；Windows CLI PE 版本与 tag 一致；GHCR 镜像 `-version` 返回真版本；tag 构建时长明显下降。

### 阶段 2：契约层重构（P0，GUI 数据正确性）

- **目标**：消除前后端类型双维护与死字段，任何一端改字段另一端编译期失败而非运行期静默错。
- **涉及文件**：
  - `gui/frontend/src/lib/types.ts` / `api.ts`（以 `wails3 generate bindings` 的 TS 输出为唯一类型源，删手工镜像与大部分 `from*`）
  - `route/matcher.go:25`（`route.Stats` 补 json tag）
  - `core/config.go` + `gui/service.go:395` + `gui/frontend/src/pages/SettingsPage.tsx:216-222`（`geoBaseURL` 三选一：Config 补 `geo_base_url` 字段 / 前端删输入框 / 由 repo 推导如实展示——当前「能编辑但不生效」最误导）
- **验收**：`wails3 generate bindings` 生成 TS；`npm run build` 通过；`go test ./route/... ./gui/...` 全绿；设置页 GEO URL 编辑不再静默丢失。

### 阶段 3：核心隧道层重构（P1-P2，注意隧道改动回归面）

- **目标**：消除双份 SOCKS5/UDP 实现、优化 geosite 匹配热路径、收敛 GEO 换引擎职责、拆分 `tunnel/masque.go`。
- **涉及文件**：
  - P1-1 退役 `tunnel.MasqueClient.HandleSOCKS5`（`masque.go:916-1148`）+ `tunnel/udp.go`（344 行，与 `proxy/udp.go` 逐行近同），UDP 中继收敛到 `proxy` 单一实现
  - P1-2 `route/matcher.go:137-160` 给 RootDomain 条目建「脱点后缀 → 规则」map/trie，命中从 O(N) 降到 O(后缀长度)
  - P2-3 统一 GEO 换引擎：`route.Engine` 支持「仅热加载新 GEO 数据」，`Server.UpdateGeo`（`core/core.go:727-751`）走它而非整体 `NewEngine`
  - P2-4 `tunnel/masque.go`（2219 行）按关注点拆 `client_conn.go` / `client_doh.go` / `client_dns.go`；保持 `dialer` 接口不动
- **验收**：`go test ./...` 全绿（含 tunnel 回归）；CLI 冒烟 `curl --socks5-hostname 127.0.0.1:40000 https://www.cloudflare.com/cdn-cgi/trace` 得 `warp=on`；GEO 匹配性能基准（大类别 CONNECT 耗时）明显下降；改动前先用桌面 CLI 对照基线（AGENTS §6.5 交接第 1 步）。

### 阶段 4：GUI 体验与架构（P1-P2）

- **目标**：状态管理收敛、消灭 `runtime.GOOS` 散布、统一反馈 UI、规则编辑器升级。
- **涉及文件**：
  - P1-3 `gui/frontend/src/pages/`（StatusPage/LogsPage/RulesPage 三处轮询统一为 react-query 或共享 `usePoll` hook；`saveConfigPartial` 读改写改原子 invalidate）
  - P1-4 统一 Toast/Alert + `useAsyncAction`（5 页 notice/error/busy 三件套收敛）
  - P1-5 `gui/service.go`（`runtime.GOOS` 散布 7 处拆 `PlatformBackend` 接口：桌面实现包 `core.Server`、Android 实现包 `androidRuntime`，Android 逻辑可宿主单测）
  - P1-6 `gui/main.go:75-85`（托盘启停错误经 `Events.Emit` 透出前端）
  - P2-7 `gui/frontend/src/pages/RulesPage.tsx:93-113`（裸 textarea → CodeMirror 6 + 错误行定位；修 `ruleCount` 漏计 REJECT 行）
  - P2-8 `gui/service.go`（加 Go 侧 `ClearLogs()` 或去掉前端假清空）
  - P2-9 `gui/service.go:395/403`（GetGeo 的 BaseURL 由 repo 推导、LastChecked 用真实时间）
- **验收**：`npm run build` + `go test ./gui/...` 全绿；桌面 GUI 走 CI build-gui 通过；托盘错误可见；规则页大文件编辑不卡。

### 阶段 5：Android 修复（按阶段 0 判别结果执行）

- **目标**：依阶段 0 结论执行修复，或以产品限制方式闭环。
- **可能路径**（对应 android-tun-expert 三个根因假设）：
  - 若实锤「UDP/HTTP3 直连被墙」：评估 UDP-in-MASQUE（需查上游是否支持；ADR 5 当前是「UDP 不走隧道」产品限制），或引导 QUIC 降级 H2 + 文档明示
  - 若实锤「DNS 洞 B」：扩展 53 拦截到任意目标 DNS + 物理 DNS 回源视图转换；裸 IP proxy 分支解析失败即报错而非 hang
  - 若实锤「边缘 4443 UDP QoS」：边缘端口优先 443 对照 + KeepAlive/Idle 参数对照
  - 统一「miss→proxy」语义写进 `rules/default-rules.txt` 与 AGENTS.md（消除文档矛盾）
- **涉及文件**：`androidvpn/androidvpn.go`、`androidvpn/dns.go`、`androidvpn/decision.go`、`tunnel/masque.go`、`gui/androidbridge.go`
- **验收**：**验收标准改为「用户真机打开境外网站 + `warp=on`」**（替代「tun0 流量增长」）；CI 构建全绿 + 远程模拟器 TUN 连通。

---

## 4. Android 外网问题：判别实验方案（按 AGENTS.md §6.5 优先级）

> 出自 `android-tun-expert.md`；标记：`[debugdiag]`=用户端调试包，`[adb]`=远程模拟器，`[代码]`=宿主单测。

> **2026-08-15 更新（东哥指示）**：本节的 6 个实验原设计依赖用户（debugdiag 包 / 真机对照），
> **全部改为自主执行**——执行载体为 `.github/workflows/android-root-probe.yml`（三 job）+
> `.omo/plans/warp-go-stage0-probe.md`（决策表）：Job1 静态取证替代"分析用户包"、
> Job3 模拟器替代"真机关 H3 / 同网络对照"。下表保留作**背景知识**（字段含义、排除法思路
> 仍有参考价值，H1-H3 假设与实验映射不变），不再作为执行清单。

### 最可能根因假设（概率排序，供阶段 0 聚焦）

1. **（高）UDP/HTTP3 直连被 ISP 封锁/劣化** —— ADR 5「UDP 不走隧道」在 Android TUN 下把浏览器 QUIC:443、QUIC 应用全部物理直连（`androidvpn.go:302-315` → `relayUDP:350-409`），不经 WARP。9 轮修复全在 TCP CONNECT 层，从未覆盖 UDP 直连面；`decision.go` 的 `udpKind=quic` 设计即自认此泄漏。
2. **（高-中）DNS 视图未统一：映射 miss → 裸 IP 走隧道 → 边缘不可达** —— v0.5.24 确诊根因的未覆盖残留；`dns.go` 只拦 `198.18.0.1:53`，v0.5.25 日志实锤 `114.114.114.114:53` 直连泄漏。
3. **（中）隧道共享 QUIC 被 ISP 对边缘 4443 UDP QoS 周期性掐断 + 恢复延迟** —— v0.5.27 修了 socket 族与 dead 重连，但端口优先级/KeepAlive 参数未对照。

### 判别实验步骤（按排除法优先级）

| 序 | 实验 | 做什么 | 怎么验证 | 期望→下一步 |
|---|---|---|---|---|
| 0 | **同网络对照**（P0，判问题归属） | 用户在同一 WiFi 下：桌面 CLI（`curl --socks5-hostname ... google.com`）+ 官方 1.1.1.1 客户端测同一批境外站 | 官方/桌面都失败 → **WARP 在该 ISP 出口被封锁**，问题降级为文档说明；仅 Android 失败才继续 | 终止或继续 1-6 |
| 1 | **v0.5.27 debugdiag 对比**（P1） | 对比新包与 20260808 包：`tunnels.tsv` 的 `network is unreachable` 是否消失、批量死亡频率是否下降、错误类型变化 | `unreachable` 归零仍打不开 → 隧道死亡非主因，转 2/3；仍频繁死亡 → 转 4/5 | `[debugdiag]` |
| 2 | **关 HTTP/3 复测**（P2，高价值近零成本） | 数 `udp.tsv` 的 `kind=quic` 行；用户浏览器关 H3（`--disable-quic`）后复测打不开的站点 | 关 H3 后可开 = **实锤 QUIC 直连被墙**；quic 行消失 | 修 UDP-in-MASQUE 或引导降级 H2；否则排除假设 1 |
| 3 | **映射 miss 追踪**（P3，验证 DNS 洞 B） | 从 `tunnels.tsv` 找 `host` 为裸 IP 且 `firstByteMs=-1` 的行 | 大量出现 = 洞 B 实锤，主链路 DNS 拦截未覆盖（硬编码 DNS/DoH/第二 DNS 来源） | 扩展 53 拦截 + 回源视图转换 |
| 4 | **端口/KeepAlive 对照**（P4，验证 UDP 边缘 QoS） | 查注册 edgeAddrs 是否含 443 候选并改优先拨号；对照调 `KeepAlivePeriod`/`MaxIdleTimeout`（`masque.go:285-306`） | `tunnels.tsv` 批量死亡消失/频率下降 | 端口优先级或保活策略修复 |
| 5 | **RST 来源判别**（P5） | 统计批量死亡瞬间 `err` 方向：`down:err=reset by peer` 是否与 `quic: transport closed` 同毫秒成对 | 成对 = 隧道死亡先于 RST，是边缘断流传染下游，修隧道活性而非 gVisor | 支撑实验 4 修法 |
| 6 | **生态级对照**（P6） | 用户同网络同开官方 1.1.1.1（Android）与本 APK 各 5 分钟 | 官方正常本 APK 不行 → 缩小到本实现 QUIC 参数/边缘选择差异 | 对照官方 KeepAlive/Idle 参数 |

### 九轮失败的共同教训（重构须避的坑）

1. **打地鼠式逐层暴露**：v0.5.13-17 启动层、v0.5.18-23 通路/重连层、v0.5.24 才触及物理根因；每一轮都在修上一轮暴露的下一层，**没有端到端验收「用户实际打开境外网站」**。
2. **模拟器 ≠ 用户 ISP**：模拟器 root shell 绕过 VpnService，只能看 tun0 计数，CI 全绿 + tun0 增长 ≠ 用户能开境外站。
3. **没有 v0.5.27 复测包**：26→27 的改善完全未验证就被放弃。
4. **UDP/HTTP3 直连从未纳入修复面**（假设 1）。

---

## 5. 风险与依赖

### 风险

| 风险 | 级别 | 说明与缓解 |
|---|---|---|
| **tunnel 改动回归面最大** | 🔴 高 | 五把锁 + 双 singleflight + dead 原子标志，真 QUIC 状态机无法单测覆盖；叠加 Android 未决问题。缓解：任何 tunnel 改动前先桌面 CLI 对照基线（AGENTS §6.5 第 1 步）；阶段 3 拆文件不动逻辑、保持 `dialer` 接口稳定 |
| **Wails v3 alpha 稳定性** | 🟠 中 | 钉在 alpha2.119；多返回值元组序列化、Android bridge 抖动。缓解：版本钉住、core 与 GUI 解耦；升级需回归全部 22 个绑定方法 |
| **前后端契约无编译期保障** | 🟠 中 | 手工双份类型，字段改名靠运行期发现（v0.5.7 双重归一化血泪）。缓解：阶段 2 bindings 单源化 |
| **Android 根因归属悬而未决** | 🟠 中 | 若实验 0 证明是 ISP 封锁，需接受「文档化限制」并改验收口径，避免继续烧轮次 |
| **本地无法编译 GUI / Android** | 🟡 低 | GTK 4.6 < 4.10、无 SDK/NDK/JDK。缓解：GUI 走 CI build-gui、`npm run build` + `go test ./gui/...` 本地兜底；Android 走 CI + 远程模拟器 |
| **仓库 shallow clone（depth=1）** | 🟡 低 | 无法 git 对照上游 androidvpn/ 提交史，上游差异只能靠文档。缓解：需要时补拉全量历史（`git fetch --unshallow`） |
| **config.json 已取消运行中热加载** | 🟡 低 | 用户主动取舍，GUI 设置页已标「重启后生效」；重构勿误加回热加载（会回归「GUI 保存被覆盖」） |
| **sync-upstream 冲突面** | 🟡 低 | tunnel/masque.go 与上游重叠；拆包/改动前需与 sync-upstream 冲突策略对齐，冲突即停、绝不自动解决 |

### 依赖

- **阶段 0 依赖**：**无用户依赖**（2026-08-15 改为自主：CI 模拟器 + 静态取证 + 上游能力查证，见 stage0-probe.md）；需要时 CI 补拉上游全量 git 历史。
- **阶段 1 依赖**：打 tag 前按 AGENTS §6.8 问东哥确认；CI 全绿判定发布完成。
- **阶段 2/4 依赖**：Wails `wails3 generate bindings` 在本机可用（`/root/go/bin` 已装 v3.0.0-alpha2.119）。
- **阶段 5 依赖**：阶段 0 判别结论；若走 UDP-in-MASQUE 需先查证上游协议支持。

---

*本文档由 4 份并行调研汇总而成，各调研报告留存于 `.omo/research/`，供实施阶段逐条引用（含文件:行号依据）。*
