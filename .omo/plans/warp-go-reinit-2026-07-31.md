# warp-go 项目重新初始化与功能升级计划 (v3)

**日期**: 2026-07-31（v3 修订：用户最终确认 + 补充要求并入，开始执行）
**目标仓库**: `callacat/warp-go`（本地工作目录 `/home/warp-go`）
**上游**:
- 主上游: `6Kmfi6HP/warp-go`（fork，HEAD `6d5ab6a`，2026-07-31，含 scanner + Dockerfile）
- 次上游: `badafans/warp-go`（原版，HEAD `ca2f0cc`，2026-07-28）

> **v3 修订记录（用户最终确认内容）**
> 1. 无桌面环境：**不做截图验证**，改用**配置文件方式启动验证**（§10 已改）
> 2. GEO 数据库默认更新仓库：`https://github.com/MetaCubeX/meta-rules-dat/tree/master`（可在 GUI 编辑仓库/URL；可设定自动更新时间；可手动触发更新）
> 3. **配置文件放程序执行目录**（config.json + rules.txt 与可执行文件同目录）+ 热重载
> 4. 专门建立**接手文档**（AGENTS.md，供后续 Agent 快速了解）
> 5. GUI 使用**最新框架/组件库**（Wails v3 + React 19 + Vite + Tailwind v4），适配常用机型/分辨率（响应式）
> 6. 计划文档随项目进度更新（本文件持续维护）
>
> **v2 修订记录**: 集成 Metis 审查修复（Top 5 + 全部决断点收敛，见 §11）；GEO 数据库格式定论（protobuf 非 mmdb，见 §6.4）；明确"规则可自由增删改"设计（见 §6.0）；补充"GitHub Actions 构建 + Release 下载验证"与"无桌面环境 GUI 验证方案"（见 §10）

## 0. 背景与事实（调研结论）

| 事实 | 结论 |
|---|---|
| 上游关系 | `badafans/warp-go` 是原版；`6Kmfi6HP/warp-go` 是其 fork（主动同步上游） |
| 本地状态 | `/home/warp-go` 原为空目录 → 已重新初始化为 git 仓库，重置到 `6d5ab6a`（最新版） |
| 远端仓库 | `callacat/warp-go` 已存在；旧 main（`1ae4d38`）已备份到 `archive/previous-poc` 分支 |
| 现有功能 | SOCKS5 前端（CONNECT + UDP ASSOCIATE）、MASQUE over QUIC/H3 隧道、注册/注销、边缘扫描（fork 新增） |
| 缺失功能 | **无** HTTP 代理、TUN、路由规则、GEO 分流、系统代理、GUI → 全部全新开发 |
| go.mod | `module warp`，go 1.26.5，全部依赖 indirect（quic-go v0.61 等） |
| 本地 Go | 1.18.1 → **已升级到 1.26.5**（M0 完成，基线构建通过） |
| GEO 数据库 | **已定论**：geosite.dat / geoip-lite.dat 均为 **v2ray protobuf 格式**（GeoSiteList/GeoIPList），非 mmdb；`geoip:private` 是库内真实条目（22 硬编码 CIDR）；`geolocation-!cn` 是字面类别名（`!` 烘焙进数据）；`geoip:lan` 需代码内置 netip 检查 |

## 1. 目标（用户需求拆解）

1. **重新初始化**：本地重置到 6Kmfi6HP/warp-go 最新版 ✅（已完成）
2. **自动合并工作流**：合并 `6Kmfi6HP/warp-go` + `badafans/warp-go` 最新代码
3. **二进制发布工作流**：构建多平台二进制 → GitHub Release
4. **Docker 镜像工作流**：构建多架构 Docker 镜像 → GHCR
5. **每个工作流末尾**：删除过时工作流记录（workflow runs 清理）
6. **系统代理功能**：开启/关闭系统代理
7. **GEO 数据库分流**：按 geosite/geoip 匹配规则分流流量（proxy / direct）
8. **GEO 规则格式**：参考 Mihomo 规则；每行 `行为,条件`；默认规则（proxy,geosite:google / proxy,geosite:geolocation-!cn / direct,geoip:private / direct,geosite:private / direct,geosite:cn / direct,geoip:cn）；规则可在文本框编辑
9. **默认数据库**：meta-rules-dat 的 geosite.dat + geoip-lite.dat
10. **GUI**：独立可执行程序，GUI 中可进行以上所有操作
11. **设计评审**：找出不合理处 + 优化建议
12. **执行纪律**：先计划落地 → 确保不偏离 → 推荐路线优先，减少用户决断

## 2. 架构决策（基于调研）

### 2.1 GUI 框架: **Wails v3**（钉住具体 alpha 版本）
- 理由: 同类 VPN 客户端 NetBird 2026 年用 Wails v3 重构；原生系统托盘 + 菜单；跨平台交叉编译官方支持（Docker 方案覆盖 win/linux/mac × amd64/arm64 全部 6 目标）；Web 前端便于实现规则编辑器（CodeMirror）、虚拟化日志、设置面板
- 备选: Fyne v2.8（若必须零 alpha 风险）——计划默认 Wails v3，若构建受阻可切换
- 核心保持纯 Go CLI；GUI 为独立 main 导入核心包

### 2.2 分流引擎: 独立 `route/` 包（不嵌入 mihomo 内核）
- 之前 POC 的 mihomo 内嵌方案（ADR-0001 "反向拓扑"）与本需求**不同**：本需求是 SOCKS5 前端按规则分流（WARP 隧道 / 本地直连），不需要 mihomo 内核
- 规则引擎参考 Mihomo 语法，但独立实现，避免引入庞大 mihomo 依赖
- 数据库解析库: sing-geosite / sing-geoip（或 maxminddb-golang，视 GEO 调研结论）

### 2.3 分流插入点
- `tunnel/masque.go` 的 `HandleSOCKS5` CONNECT 分支（`dialTarget` 前）：按规则判定 → WARP 隧道 or 本地直连
- UDP 保持现状（本地直出）→ 规则仅作用于 TCP CONNECT；UDP 全直连（符合默认规则中 UDP 无代理的现实）

### 2.4 系统代理
- Windows: 注册表 `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`（ProxyEnable + ProxyServer）
- macOS: `networksetup -setwebproxy/-setsecurewebproxy`（scutil 持久化）
- Linux: 桌面环境环境变量（gsettings org.gnome.system.proxy 或手动 export http_proxy/https_proxy）
- 代理地址: 本地 SOCKS5/HTTP 混合端口

### 2.5 三个工作流（重写，替代旧的 sync/build-release/cleanup 三件套）
见第 5 节

## 3. 里程碑分解

### M0: 环境与仓库基线（已完成 ✅）
- [x] 本地 git init + reset 到 6Kmfi6HP 最新版（`6d5ab6a`）
- [x] 远端旧 main（`1ae4d38`）备份到 `archive/previous-poc`
- [x] 本地 Go 升级到 1.26.5（官方 tarball → `/usr/local/go1.26.5`，PATH 前置，不覆盖系统 Go）
- [x] 验证 `go build` + `go vet` + `go test` 全部通过（基线 GREEN）

### M1: 三个 GitHub Actions 工作流（先于功能，形成 CI 闭环）
- [ ] `sync-upstream.yml`（重写）: 双上游自动合并 → PR（badafans 先 merge，6Kmfi6HP 后 merge，冲突即停人工介入）
- [ ] `build-release.yml`（重写）: tag → 5 平台二进制 + GitHub Release + checksums（CLI + GUI 均发布）
- [ ] `docker-ghcr.yml`（新增）: main push + tag → linux/amd64 + arm64 镜像 → GHCR
- [ ] `cleanup` 步骤（每个工作流末尾）: 删除 30 天前 runs，保留最新 20 个（修复 Metis B7/F1）
- [ ] 仓库级前置配置: GHCR 启用、Workflow permissions read+write、auto-merge 设置

### M2: 路由/分流核心（纯 Go 库，无 GUI 依赖）
- [ ] `route/` 包: 规则解析（`行为,条件` 每行一条，支持 `#` 注释与空行）
- [ ] **规则持久化**：`rules.txt` 文件（默认规则集首次初始化写入；GUI 增删改即编辑此文件）
- [ ] GEO 数据库下载/更新/缓存（geosite.dat + geoip-lite.dat，SHA-1 去重 + proto 校验 + 原子写）
- [ ] GEO 解析: `routercommon` protobuf → 内存结构（类别名 EqualFold 匹配）
- [ ] 规则匹配引擎（geosite 域名后缀匹配 / geoip IP 匹配 / `lan` 内置 / 默认 direct 兜底）
- [ ] 规则热重载（文件变更 → 重载；GUI 保存即生效）
- [x] 集成到 SOCKS5 CONNECT 分流（proxy → 隧道，direct → 本地直连）——在 `proxy` 包实现（`Config.Router` + `Config.TunnelDial` 缝），proxy_test.go 单测覆盖四种路径（direct/proxy/未命中兜底/nil Router 全隧道）

### M3: 系统代理 + 代理服务增强
- [x] HTTP 代理（CONNECT + 绝对 URI 转发）→ 与 SOCKS5 同端口 mixed（首字节嗅探：`0x05`→SOCKS5，`CONNECT `/`GET `→HTTP）
- [x] 系统代理设置/清除（三平台: Windows 注册表 / macOS networksetup / Linux gsettings；common.go 统一校验）
- [x] 配置持久化（**config.json 放程序执行目录**；旗标 > config > 默认值；文件变更热重载；config_test.go 单测）
- [x] GUI/系统代理模式默认绑定 `127.0.0.1`（安全，修复 Metis C4；config.go 默认 `127.0.0.1:40000`）

### M4: GUI（Wails v3 + 最新前端栈）✅
- [x] **core 包抽取**（修复 Metis A4）: `core/`（Server 生命周期 Start/Stop/Status/SetSystemProxy/ReloadRules/SaveConfig），CLI 与 GUI 共用；main.go 瘦身 ~450 行薄壳
- [x] Wails v3 项目骨架（gui/main.go + service.go + logs.go + **React 19.2 + Vite 8 + Tailwind v4** 前端）
- [x] **响应式适配**：960x680 默认窗口（720x520 最小），深浅色主题（系统跟随 + 手动切换）
- [x] 系统托盘（打开主窗口 / 启动 / 停止 / 退出；AttachWindow 关窗隐藏到托盘）
- [x] **规则管理页**（用户核心需求）: 行号文本框增删改 + 语法校验（保存前 route.ParseRules）+ 保存写回 rules.txt + 热重载；2s 自动刷新不覆盖未保存编辑
- [x] **GEO 数据库页**: 状态展示（路径/更新时间/仓库/周期）、立即更新按钮、自动更新周期显示
- [x] 设置面板: 完整 config 表单（监听地址/GEO 仓库/URL/更新周期/UDP 开关/系统代理）
- [x] 状态视图: 运行状态/监听地址/分流统计（proxy/direct/miss）/注册信息
- [x] 日志查看器: 最近 500 条环形缓冲，级别着色，1s 轮询
- [x] 交叉编译配置（Taskfile + build/{linux,windows,darwin} 平台 Taskfiles + Dockerfile.cross）
- [x] GUI 构建走 CI（build-gui 分平台 job：ubuntu-24.04/windows-latest/macos-latest；本地 GTK 4.6 < 4.10 无法编译）

### M5: 发布与验证 ✅
- [x] Dockerfile 更新（多阶段 CLI 构建，端口 40000；**不含 GUI**，修复 Metis B2）
- [x] 推送远端 main（备份后 force-push 覆盖 `1ae4d38` → `ad6d6de`，修复 Metis A2）
- [x] 打 tag v0.2.0 触发 Actions → **全绿**：5 平台 CLI + 3 平台 GUI + Release + GHCR 镜像
- [x] 文档（README 重写: 新功能、规则语法、GUI 用法、配置说明）
- [x] 风险回退路径（§8：删 tag + revert + 删 GHCR 镜像）
- [x] 从 GitHub Release 下载产物本地验证：CLI 配置启动（config.json 自动生成）+ GEO 下载 + 分流匹配 + Docker 冒烟全部通过

## 4. 执行顺序（推荐路线，减少决断点）

```
M0(环境) → M1(工作流) → M2(路由核心) → M3(系统代理) → M4(GUI) → M5(发布)
```

- 每个里程碑完成即验证（构建 + 测试），不留半成品
- M1 先做: 工作流闭环能让后续每次提交都自动验证
- M2 先于 M4: GUI 只是路由核心的壳，核心稳定后再包 UI
- 用户无需决断的点（默认选择）:
  - 框架 = Wails v3（已定）
  - 分流实现 = 独立 route 包（已定，不用 mihomo 内核）
  - 工作流触发 = sync 每日 + dispatch；release 按 tag；docker 按 main push + tag
  - 清理策略 = 每个工作流末尾删 30 天前 runs + 每周日全量清理

## 5. 工作流详细设计

### 5.1 sync-upstream.yml（自动合并双上游）
```yaml
name: Sync Upstreams
on:
  schedule: [{cron: '0 4 * * *'}]   # 每天
  workflow_dispatch:

jobs:
  sync:
    runs-on: ubuntu-latest
    permissions: {contents: write, pull-requests: write}
    steps:
      - checkout (fetch-depth 0)
      - 添加 6Kmfi6HP 与 badafans 两个 remote
      - fetch 两者 main
      - 若任一有更新:
        - 建分支 sync/upstream-<ts>
        - 先 merge badafans/main（主上游，--no-ff）
        - 再 merge 6Kmfi6HP/main（fork，--no-ff）
        - 有冲突: 中止 PR 打 conflict 标签（人工介入）
        - 无冲突: push 分支 + 开 PR（自动合并策略: 若 CI 绿则 auto-merge）
      - 末尾: 清理过时 workflow runs
```

### 5.2 build-release.yml（二进制 + Release）
```yaml
name: Build and Release
on:
  push: {tags: ['v*']}
  workflow_dispatch:
jobs:
  build-binary:
    strategy: matrix  # linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64
    steps:
      - checkout
      - setup-go (1.26.x)
      - CGO_ENABLED=0 go build -trimpath -ldflags "-s -w"
      - upload-artifact
  release:
    needs: build-binary
    steps:
      - 下载所有 artifacts
      - gh release create <tag> (含 checksums)
      - 末尾: 清理过时 workflow runs
```

### 5.3 docker-ghcr.yml（Docker 镜像 → GHCR）
```yaml
name: Build Docker GHCR
on:
  push: {branches: [main], tags: ['v*']}
  workflow_dispatch:
jobs:
  docker:
    steps:
      - checkout
      - setup-qemu + setup-buildx
      - login GHCR
      - metadata (latest + tag)
      - build-push (linux/amd64, linux/arm64)
      - 末尾: 清理过时 workflow runs
```

### 5.4 cleanup 步骤（每个工作流末尾 + 独立每周任务）
```yaml
- name: Clean up stale workflow runs
  run: |
    CUTOFF=$(date -d "30 days ago" -u +%Y-%m-%dT%H:%M:%SZ)
    gh run list --limit 100 --json databaseId,createdAt --jq \
      ".[] | select(.createdAt < \"$CUTOFF\") | .databaseId" | \
      while read id; do gh run delete "$id" --yes; done
```

## 6. 路由规则详细设计

### 6.0 规则可自由增删改（核心设计，用户确认）
- **规则不是内置写死的**。规则以纯文本文件 `rules.txt` 持久化（默认在配置目录，路径可在 config.json 指定）
- 首次初始化时写入默认规则集作为**模板**；之后**完全由用户自由编辑**
- 三种编辑途径，共用同一份 `rules.txt`:
  1. **GUI 规则管理页**: 文本框逐行编辑，支持增/删/改/排序；保存时校验语法并写回文件 → 引擎热重载，无需重启
  2. **CLI**: `warp -route <file>` 指定规则文件；或直接编辑 rules.txt 后发送 SIGHUP 重载
  3. **直接改文件**: 任何文本编辑器均可；引擎检测到文件变更自动重载（可选 watch 模式）
- 保存校验: 语法错误 → GUI 高亮报错行并拒绝保存（不写入半成品）；合法才写回并触发热重载

### 6.1 规则语法（每行一条: `行为,条件`）
- 行为: `proxy`（走 WARP 隧道）/ `direct`（本地直连）
- 条件（参考 Mihomo）:
  - `geosite:<name>` — 域名匹配 geosite 分类（后缀匹配，含子域）
  - `geosite:geolocation-!cn` — 非中国站点（**字面类别名**，`!` 烘焙进数据，非取反语法）
  - `geoip:<cc>` — IP 国家匹配（如 cn）
  - `geoip:private` — 私有/保留地址段（**库内真实条目**，22 硬编码 CIDR）
  - `geoip:lan` — 内网地址（**代码内置** netip 检查: IsPrivate/IsLoopback/IsUnspecified/IsMulticast/IsLinkLocalUnicast）
  - `domain:<suffix>` / `domain-suffix:<suffix>` — 直接域名后缀（可选扩展）
  - 空行与 `#` 注释行忽略
- 默认规则模板（首次初始化写入 rules.txt，用户可任意修改）:
  ```
  # 默认路由规则（每行一条，格式: 行为,条件）
  # 行为: proxy = 走 WARP 隧道；direct = 本地直连
  proxy,geosite:google
  proxy,geosite:geolocation-!cn
  direct,geoip:private
  direct,geosite:private
  direct,geosite:cn
  direct,geoip:cn
  ```
- 未匹配 → 引擎末尾隐式 `direct`（保守兜底，文档明示）

### 6.2 GEO 数据库
- 默认更新仓库（用户指定，可在 GUI 编辑）: `https://github.com/MetaCubeX/meta-rules-dat`（release latest 资产）
- 默认下载 URL（用户指定）:
  - `https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geosite.dat`
  - `https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip-lite.dat`
- 仓库/URL 可在 GUI 设置页编辑（自定义更新源）
- **自动更新时间可配置**（GUI 设置项，默认 7 天；可选: 每天/每周/每月/关闭自动更新）
- **手动触发更新**（GUI 按钮 + CLI `-geo-update`）
- 下载到 `<data-dir>/geo/`（程序执行目录下 `geo/`，v3: 配置与数据文件统一放执行目录，见 §12 路径约定）
- 更新策略（参考 mihomo updater）: 启动时本地有则加载；刷新时 **SHA-1 比对**（相同跳过）；**proto.Unmarshal 校验**通过才原子替换
- 保持**内存解析结构**（非原始字节）用于匹配

### 6.3 匹配流程
```
CONNECT target (host:port)
  → 解析 host:
    - IP: geoip 匹配（先 lan 内置检查，再查库: private/国家）
    - 域名: 先 geosite 域名后缀匹配（无需解析）；需要 geoip 时才解析出 IP
  → 按规则顺序匹配第一个命中（类别名 EqualFold 匹配）
  → proxy → WARP 隧道（现有 CONNECT 路径）
  → direct → 本地直连（新 dial）
  → 无命中 → direct
```

### 6.4 GEO 格式定论（调研确认 ✅）
- `geosite.dat` = **v2ray protobuf** `GeoSiteList`（1,541 类别；实测 4,240,673 B）; `geoip-lite.dat` = **v2ray protobuf** `GeoIPList`（12 类别 / 15,129 CIDR；实测 206,283 B）
- **均非 mmdb**（无 `MaxMind.com` magic）；mmdb 是另一族产物（country.mmdb）
- 解析库: **`github.com/v2fly/v2ray-core/v5/app/router/routercommon`** + `google.golang.org/protobuf/proto.Unmarshal`（v2ray-core 与 mihomo 同款做法）
- **不要用** sing-geosite/sing-geoip（仓库只剩发布工具 main.go，无可导入包，已实证）
- 类别名存储为大写，匹配用 `strings.EqualFold`（小写规则可用）
- 域名类型: `Plain=0`(子串) / `Regex=1` / `Domain=2`(根域=后缀匹配，含域与子域) / `Full=3`(精确)
- `geoip:private` 分类在 geoip-lite.dat 中真实存在（第 12/12 类，22 CIDR）；`geosite:private` 来自 v2fly domain-list-community data/private；`geolocation-!cn` 在 geosite.dat 中真实存在（27,037 域名）

## 7. 设计评审（用户要求: 指出不合理处 + 优化建议）

### 7.1 用户原方案中的合理处 ✅
- GEO 分流 + 系统代理方向正确（SOCKS5 前端上做分流是最低侵入方案）
- 默认数据库选 meta-rules-dat（活跃维护、格式与 Mihomo 兼容）
- 规则文本可在 GUI 编辑（灵活，不锁死）

### 7.2 发现的问题与改进建议 ⚠️
| # | 问题 | 建议 |
|---|---|---|
| 1 | **分流只在 SOCKS5 层**，HTTP 代理、GUI 内嵌浏览器流量可能绕过 | 同时提供 HTTP 代理入口（mixed 端口），系统代理设置为 HTTP 代理地址（系统代理不支持 SOCKS5 的场景：Windows 系统代理可设 socks，macOS/Linux 更通用是 HTTP） |
| 2 | **UDP 完全不走隧道**（上游设计），规则对 UDP 无效 | 明确文档声明：UDP 全直连（上游限制）；规则仅 TCP。若需 UDP 进隧道需 TUN（超出本次范围，记录为未来项） |
| 3 | **域名匹配需要先 DNS 解析**才能做 geoip 匹配，解析本身可能泄漏/被污染 | 优化：geosite 域名后缀匹配优先（无需解析）；geoip 匹配仅在已解析出 IP 时进行；DNS 走系统解析（或后续加 DoH 选项） |
| 4 | **默认规则无兜底 direct**（用户给的 6 条规则不覆盖所有流量） | 引擎末尾隐式 `direct`（文档明示）。避免"未匹配却走了代理"的意外 |
| 5 | **`geolocation-!cn` 依赖数据库内置类别**，meta-rules-dat 的 geosite.dat 中该类别存在性需验证 | 调研确认；若缺失则代码内置 geolocation-!cn ≈ 所有非 cn 类别的并集 |
| 6 | **系统代理开关与分流引擎是两回事**：系统代理开启 ≠ 流量必然分流 | GUI 明确两个开关：① 代理监听开关 ② 系统代理开关（指向本地代理端口）。文档说明关系 |
| 7 | **Wails v3 是 alpha** | 钉住版本；核心逻辑（route/tunnel）与 GUI 解耦，未来换框架不动核心 |
| 8 | **本地 Go 1.18 与 go.mod 1.26 不匹配** | 升级本地 Go；CI 用 actions/setup-go 1.26.x |
| 9 | **上游 merge 冲突风险**（main.go 是双方都改的文件） | 冲突时人工介入（PR 打标签），不自动强推；规则化冲突解决（fork 的 scanner 部分优先保留） |
| 10 | **清理脚本误删"刚创建的但超过 30 天"的 runs**（时间戳判断） | 用 createdAt 而非 updatedAt；保留最近 N 个 runs 的例外（跳过最新 20 个） |

### 7.3 额外优化/功能建议（用户征询）
1. **配置热重载**: 规则文本修改保存后自动生效，无需重启（GUI 触发 reload）
2. **分流统计**: GUI 显示 proxy/direct 各命中数（轻量计数器，不侵入隧道）
3. **数据库自动更新**: 启动时后台检查 meta-rules-dat latest 版本（可选开关）
4. **日志轮转/级别**: GUI 日志查看器带级别过滤（info/warn/error）
5. **开机自启选项**: GUI 开关（Windows 启动项/macOS LaunchAgent/Linux autostart desktop 文件）
6. **多配置档**: 规则文本支持多套预设（如"全代理"/"国内直连"），一键切换（优先级中，可在后续迭代）
7. **命令行交互模式**: GUI 之外保留 `warp -route <file>` 以规则文件启动（无 GUI 环境可用）

## 8. 风险与回退

| 风险 | 缓解 |
|---|---|
| Wails v3 alpha 不稳定 | 钉版本；核心解耦；失败回退 Fyne v2.8 |
| GEO 数据库格式与预期不符 | **已调研确认**：protobuf 格式，用 v2ray-core routercommon 解析（已验证） |
| 上游 merge 冲突 | 冲突即停（PR 人工介入 + issue 通知），绝不自动强推 |
| GEO 分流引入延迟（DNS 解析） | geosite 优先；geoip 仅解析后匹配；可配置 |
| 本地 arm64 环境无法构建 darwin | CI（GitHub Actions）负责全部 6 目标交叉编译；本地仅验证 linux |
| M5 push 被拒（远端 main 非 fast-forward） | 已定策略：备份当前远端 main 分支后 force-push（见 §10.3） |
| release 有 bug | 删 tag + revert commit + 删 GHCR 镜像（回退路径文档化） |
| Wails 交叉编译 3 天起不来 | 立即切换 Fyne v2.8 并通知用户（时间盒 1 天） |

## 9. 验收标准（全部满足才算完成）

- [ ] 本地仓库 = 6Kmfi6HP `6d5ab6a` + 上述全部改动，`go build`/`go vet`/`go test` 通过
- [ ] sync-upstream 工作流: 手动 dispatch 成功，无冲突时自动合并双上游
- [ ] build-release 工作流: tag 后产出 5 平台二进制（CLI + GUI）+ Release + checksums
- [ ] docker-ghcr 工作流: push 后产出 linux/amd64 + arm64 镜像到 GHCR
- [ ] 三个工作流末尾均有清理步骤（30 天前 runs，保留最新 20 个）且能删除过时 runs
- [ ] `warp` CLI 支持 `-route` 规则文件 + 首次初始化写默认规则模板，SOCKS5 CONNECT 按规则分流
- [ ] **GUI 规则管理页**: 增/删/改/排序规则，保存 → 写回 rules.txt → 热重载生效（用户核心需求）
- [ ] GEO 数据库自动下载（meta-rules-dat latest）+ SHA-1 去重 + proto 校验 + 缓存 + 更新按钮
- [ ] 系统代理: Windows/macOS/Linux 三平台设置/清除实现（CI 分平台 job 验证）
- [ ] GUI: 托盘、状态、规则编辑、数据库更新、日志、双开关（监听 + 系统代理）
- [ ] 全平台交叉编译通过（CI 验证）；Release 产物从 GitHub 下载本地验证（linux/amd64 需用 qemu 或 CI 内验证）
- [ ] 无桌面环境 GUI 验证方案执行（CI Xvfb 截图，见 §10.2）
- [ ] README 更新完毕
- [ ] 远端 main 推送成功，旧 POC 分支保留于 archive/previous-poc

## 10. 验证方案（本机无桌面环境 · v3 改为配置文件启动验证）

> 用户要求：无桌面环境**不需要截图验证**，使用**配置文件方式启动验证**。构建推 GitHub 触发 Actions，从 Release 下载产物到本地验证。

### 10.1 构建与产物获取链路（CI 为主）
1. 本地提交 → push 到 `callacat/warp-go`（main 或功能分支）
2. 打 tag `vX.Y.Z` → 触发 `build-release.yml` → GitHub Actions 构建 5 平台二进制（CLI + GUI）+ checksums → 发布 GitHub Release
3. **本地验证 CLI**（linux/arm64 原生可跑）:
   - `gh release download <tag> -R callacat/warp-go` 下载 linux-arm64 产物
   - 运行: `./warp -h` 看用法；`./warp -reg` 注册（需网络到 Cloudflare）；`./warp -route rules.txt` 启动分流代理
   - 验证分流: `curl --socks5-hostname 127.0.0.1:40000 https://www.cloudflare.com/cdn-cgi/trace`（预期 `warp=on`）
   - 验证规则引擎: 用 `route` 单元测试 + 本地 CLI `-route` 直连判定（curl 到国内/国外目标看源 IP）
4. **本地验证 Docker 镜像**（linux/arm64）:
   - `docker pull ghcr.io/callacat/warp-go:latest` → `docker run` 冒烟（注册 + 代理端口连通性）

### 10.2 无桌面环境 GUI 验证（配置文件方式启动 · 无截图）
- GUI 程序支持**配置文件启动模式**（如 `warp-gui -config config.json` 或环境变量），在无头环境：
  1. 用配置文件指定: 监听端口、规则文件路径、GEO 更新仓库/间隔、代理开关等
  2. 启动后通过 CLI/HTTP 检查进程存活、配置加载日志、规则文件生效（改 rules.txt 验证热重载）
  3. 断言: 进程不崩溃 + 日志显示配置加载 + 分流逻辑正确（复用 route 单测）
- CI 冒烟 job（linux runner）: `xvfb-run` 或纯配置模式启动 GUI 二进制 → 检查退出码/日志 → 通过即视为验证成功（**不截图**）
- Windows/macOS GUI: CI 对应 runner 构建 + 配置模式启动冒烟（进程存活 + 日志断言）
- 视觉/交互部分: 交付产物 + 文档，由用户在有桌面的机器实测反馈

### 10.3 远端 main 推送策略（Metis A2 修复）
- 当前远端 main = `1ae4d38`（旧 POC，已备份到 `archive/previous-poc`）
- 本地 main = `6d5ab6a` + 新提交（与远端无共同祖先路径）
- **策略**: 推送前再次确认 archive 分支存在 → `git push --force origin main`（一次性覆盖，旧内容安全存于 archive/previous-poc）
- 后续迭代走正常 PR/合并，不再 force-push

## 11. Metis 审查修复汇总（v2 已并入）

| Metis 发现 | 严重度 | v2 处理 |
|---|---|---|
| A1 GEO 附录悬空 | 🔴 致命 | ✅ 已补 §6.4 格式定论 + 库选择 |
| A2 远端 push 策略未定义 | 🔴 致命 | ✅ 已定 §10.3（备份后 force-push） |
| A3 无 CI/测试工作流，"CI 绿"空转 | 🔴 致命 | ✅ build-release 加 test job（M1）；auto-merge 依赖该 job 绿 |
| A4 GUI 运行模型未定 + core 抽取缺失 | 🟠 高 | ✅ 定"core 包 + 子进程"模型：CLI 可独立跑；GUI 嵌 core（M4 首任务） |
| A5 7.3 范围未界定 | 🟠 高 | ✅ 7.3 采纳项并入里程碑（热重载/统计/日志过滤）；不采纳项明示（多配置档延后、开机自启延后） |
| B1 GEO 库三选一未决 | 🟠 高 | ✅ 定 v2ray-core routercommon |
| B2 Dockerfile "含 GUI"歧义 | 🟠 高 | ✅ Docker 镜像仅 CLI，不含 GUI |
| B3 Wails 交叉编译风险低估 | 🟠 高 | ✅ 时间盒 1 天，失败切 Fyne（§8） |
| B4 force-push 危险 | 🟠 高 | ✅ §10.3 策略 |
| B5 三平台无法验证 | 🟡 中高 | ✅ §10 CI 分平台 job + 截图复核 |
| B6 mixed 端口嗅探边界 | 🟡 中 | ✅ 定判定规则（首字节 0x05→SOCKS5；`CONNECT `/`GET `→HTTP）+ 单测 |
| B7 清理脚本 bug（100 上限 + 无保留例外） | 🟡 中 | ✅ cleanup 步骤保留最新 20 + 分页遍历 |
| B8 本地 Go 升级方式 | 🔵 低 | ✅ 已完成（官方 tarball，PATH 前置） |
| C2 路径体系未定义 | 🟠 高 | ✅ 定 `~/.config/warp-go`（Linux）/ `~/Library/Application Support/warp-go`（macOS）/ `%APPDATA%\warp-go`（Windows）；旗标 > config > 默认 |
| C4 安全上下文（开放代理风险） | 🟡 中高 | ✅ GUI/系统代理模式默认绑 127.0.0.1（M3） |
| C6 仓库前置配置 | 🟡 中 | ✅ M1 加"仓库配置确认"清单 |
| C7 供应链细节 | 🟡 中 | ✅ SHA-1 去重 + proto 校验（§6.2） |
| D1 11 个未决"或" | 🔴 致命 | ✅ 全部收敛为默认选择（见上文各处） |
| D3 冲突处理通知机制 | 🟡 中 | ✅ PR 打标签 + issue 通知 + 不自动重试 |

## 12. 路径约定（v3，用户指定：配置文件放程序执行目录）

> 所有配置与数据文件默认放在**程序执行目录**（可执行文件所在目录），便于便携部署与 GUI/CLI 共用。

| 文件 | 位置 | 说明 |
|---|---|---|
| `config.json` | 执行目录 | 主配置：监听端口、规则文件路径、GEO 仓库/URL、自动更新时间、代理开关等；**文件变更热重载**（watcher） |
| `rules.txt` | 执行目录（默认，config 可改） | 路由规则文本（GUI 增删改 + 热重载） |
| `reg.json` | 执行目录（上游原约定，保留） | WARP 注册信息 |
| `geo/` | 执行目录下 `geo/` | geosite.dat + geoip-lite.dat 缓存 |
| `logs/` | 执行目录下 `logs/`（可选） | 日志文件（GUI 日志查看器读取） |

- 旗标 > config.json > 默认值（优先级）
- 热重载：config.json / rules.txt 文件变更 → 自动重载（mtime + 内容 hash 检测），无需重启
- GUI 与 CLI 共用同一路径约定（GUI 由 core 包启动时解析执行目录）

## 13. 接手文档（v3，用户要求）

- 建立 `AGENTS.md`（仓库根目录）：架构总览、目录结构、关键决策记录（ADR 摘要）、构建/测试/验证命令、工作流说明、GEO 格式要点、路径约定
- 计划文档 `.omo/plans/warp-go-reinit-2026-07-31.md` 持续维护（随进度更新勾选状态）
- 目标：后续 Agent 读 `AGENTS.md` + 计划文档即可快速接手

## 14. 执行

**M0–M5 全部完成（2026-07-31）**：每个里程碑均构建+测试验证；核心交付物独立审查；计划文档随进度持续更新。远端 main = `ff6edd9`，tag v0.2.0 已发布，三个工作流验证全绿。

**M6 维护增强完成（2026-08-01，tag v0.4.0）**：用户实测反馈驱动的修复与增强——
- **根因修复**：GUI 首启死锁（`InitDefaults` 持锁调 `serverInstance` 二次加锁 → 全部服务调用阻塞），导致"不读 config.json/reg.json、一键注册无响应无日志"；修复后首启一次生成 config.json + 默认规则（GitHub 下载回退模板）+ GEO 下载
- **新功能**：REJECT 广告拦截（SOCKS5 0x02/HTTP 403 + 拦截统计卡）；GitHub 下载加速前缀（`download_proxy` 默认 gh-proxy.org，GUI 可配）；开启系统代理自动启动内核
- **UI 修复**：侧边栏收起展开按钮点不到、托盘退出残留、流量统计恒 0、扫描按钮→注销、设置页"重新加载"→"重置配置"
- 远端 main = `165d565`，tag `v0.4.0` 已发布；变更记录见 `CHANGELOG.md`

**M7 Android 完成（2026-08-01，T13a 文档；tag v0.5.0 由 T13b 执行）**：Android 版 = Wails v3 壳 + 自写 Java `VpnService`（TUN fd → JNI → Go），`core.Kernel` 抽取供 CLI/GUI/Android 三端复用，`androidvpn/` 决策逻辑宿主可测（reject 绝不拨号），geoip 真实 IP 修复，CI `build-android` job（JDK21+SDK+NDK → c-shared + APK/AAB）。本地无 Android 设备 → **CI-only 构建，运行时行为需真机验收**（清单见 [`.omo/plans/warp-go-android-2026-08-01.md`](warp-go-android-2026-08-01.md) §11）。详见该计划文档与 `AGENTS.md` M7 行。
