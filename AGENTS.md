# AGENTS.md — warp-go 接手指南

> 本文件供后续 Agent 快速了解项目。配合计划文档 `.omo/plans/warp-go-reinit-2026-07-31.md`（随进度更新）阅读。
> 最后更新: 2026-07-31

## 1. 项目是什么

Cloudflare WARP 客户端（Go），通过 **MASQUE over QUIC/HTTP-3** 建立隧道，前端暴露 **SOCKS5**（后续扩展 mixed HTTP+SOCKS5）。免 root、无 TUN、不改动路由。

上游关系：
```
badafans/warp-go ──(原版/上游)──▶ 6Kmfi6HP/warp-go ──(fork，含 scanner/Dockerfile)──▶ callacat/warp-go (本仓库)
```
- 本仓库 = `6Kmfi6HP/warp-go` 的 `6d5ab6a` + 新增功能（GEO 分流、系统代理、GUI、工作流）
- 旧 POC 内容备份在 `archive/previous-poc` 分支（勿删）

## 2. 目录结构

```
warp-go/
├── main.go                  # CLI 入口（参数解析、注册编排、SOCKS5 监听循环）
├── core/                    # (M4) 可复用核心：Server 结构体（Start/Stop），CLI 与 GUI 共用
├── route/                   # (M2) GEO 分流引擎：规则解析、匹配、rules.txt、GEO 下载
├── proxy/                   # (M3) mixed HTTP+SOCKS5 代理（首字节嗅探）
├── sysproxy/                # (M3) 系统代理设置（Windows/macOS/Linux）
├── gui/                     # (M4) Wails v3 GUI（React 前端）
├── registration/            # 上游既有：两步注册 API
├── tunnel/                  # 上游既有：MASQUE/QUIC 隧道、SOCKS5 TCP、UDP ASSOCIATE
├── scanner/                 # 上游既有：边缘延迟扫描（-scan）
├── .github/workflows/       # sync-upstream / build-release / docker-ghcr
├── .omo/plans/              # 计划文档（随进度更新）
├── docs/                    # 上游逆向文档 + 新功能设计
└── AGENTS.md                # 本文件
```

## 3. 运行时文件约定（v3，用户指定：程序执行目录）

| 文件 | 位置 | 说明 |
|---|---|---|
| `config.json` | **执行目录** | 主配置（监听端口/规则路径/GEO 仓库与 URL/自动更新时间/代理开关）；**文件变更热重载** |
| `rules.txt` | 执行目录（config 可改） | 路由规则文本；GUI 增删改 + 热重载 |
| `reg.json` | 执行目录 | WARP 注册信息（上游原约定） |
| `geo/` | 执行目录/geo/ | geosite.dat + geoip-lite.dat |
| `logs/` | 执行目录/logs/ | 日志（可选） |

优先级：**旗标 > config.json > 默认值**。热重载基于 mtime + 内容 hash 检测。

## 4. GEO 分流（关键设计，已调研定论）

- **规则不是内置的**：`rules.txt` 纯文本，默认规则集首次初始化写入作为模板，GUI/CLI/直接编辑均可改，保存/变更即热重载
- 规则语法：每行一条 `行为,条件`（行为: `proxy`/`direct`；空行与 `#` 注释忽略）
  - `geosite:<name>`（后缀匹配含子域）、`geoip:<cc>`、`geoip:private`（库内真实条目）、`geoip:lan`（代码内置 netip 检查）、`domain:<suffix>`
  - 未匹配 → 隐式 `direct` 兜底
- 默认规则模板（首次初始化写入）：
  ```
  proxy,geosite:google
  proxy,geosite:geolocation-!cn
  direct,geoip:private
  direct,geosite:private
  direct,geosite:cn
  direct,geoip:cn
  ```
- **GEO 格式定论**（重要，勿再重新调研）：
  - `geosite.dat` / `geoip-lite.dat` = **v2ray protobuf**（GeoSiteList/GeoIPList），**非 mmdb**
  - 解析库：`github.com/v2fly/v2ray-core/v5/app/router/routercommon` + `google.golang.org/protobuf`
  - 类别名大写存储，用 `strings.EqualFold` 匹配；`geolocation-!cn` 是**字面类别名**（`!` 烘焙进数据，非取反语法）
  - 域名类型：`Plain=0`(子串)/`Regex=1`/`Domain=2`(根域后缀)/`Full=3`(精确)
  - **勿用** sing-geosite/sing-geoip（仓库只剩发布工具，无可导入包）
- GEO 更新：默认仓库 `MetaCubeX/meta-rules-dat`；GUI 可编辑仓库/URL；可设自动更新时间（默认 7 天）；可手动触发；SHA-1 去重 + proto 校验 + 原子写

## 5. 工作流（M1）

| 工作流 | 触发 | 作用 |
|---|---|---|
| `sync-upstream.yml` | 每日 04:00 UTC + dispatch | 双上游自动合并（badafans 先 → 6Kmfi6HP 后）→ PR；冲突即停（标签 + issue） |
| `build-release.yml` | tag `v*` + dispatch | test job → 5 平台二进制（CLI+GUI）+ checksums → GitHub Release |
| `docker-ghcr.yml` | main push + tag `v*` + dispatch | linux/amd64+arm64 镜像 → GHCR（latest + tag） |
| cleanup 步骤 | 每个工作流末尾 | 删 30 天前 runs，保留最新 20 |

## 6. 构建/测试/验证命令

```bash
go version          # 需 ≥1.26.5（本地已装 /usr/local/go1.26.5，PATH 前置）
go build ./...      # 构建
go vet ./...        # 静态检查
go test ./...       # 测试（scanner/route/proxy 有单测）
wails3 version      # M4 GUI: v3.0.0-alpha2.119（已装 /root/go/bin）
node --version      # M4 GUI 前端: v22（已装）
# GUI（M4）: cd gui && wails3 build / npm run build
# 交叉编译: Taskfile 任务（M4）
# Linux GUI 构建依赖: libgtk-4-dev + libwebkitgtk-6.0-dev（本地已装）
```

验证方式（无桌面环境，v3 约定）：
- CLI：`./warp -reg` 注册 → `./warp -config config.json` 启动 → `curl --socks5-hostname 127.0.0.1:40000 https://www.cloudflare.com/cdn-cgi/trace`（预期 `warp=on`）
- 规则引擎：`go test ./route/...` + 配置文件方式启动（改 rules.txt 验证热重载）
- GUI：**配置文件方式启动冒烟**（无截图）；产物交付用户实测
- Docker：`docker pull ghcr.io/callacat/warp-go:latest` → 冒烟

## 6.5 已完成里程碑（2026-07-31）

| 里程碑 | 状态 | 说明 |
|---|---|---|
| M0 环境基线 | ✅ | Go 1.26.5；wails3 alpha2.119；gtk4+webkitgtk-6.0；node22 |
| M1 三个工作流 | ✅ | sync-upstream（双上游，冲突即停）/ build-release（test+5平台+Release）/ docker-ghcr（多架构）→ GHCR；每个末尾 cleanup-runs.sh（30 天前 runs，保留最新 20） |
| M2 route 包 | ✅ | 规则解析/rules.txt 模板与热重载/GEO 下载(SHA-1+proto 校验+原子写)/匹配引擎/单测 18 个全绿 |
| M2.5 SOCKS5 分流集成 | ✅ | 在 `proxy` 包实现（非 tunnel 侧）：`Config.Router` + `Config.TunnelDial` 缝；`proxy_test.go` 覆盖 direct/proxy/未命中兜底/nil Router 全隧道 4 路径；`dial()` 未命中→本地直连、命中 proxy→隧道（TunnelDial 未配则报错） |
| M3 系统代理+config | ✅ | `proxy/` mixed HTTP+SOCKS5（首字节嗅探）+ UDP ASSOCIATE 中继（udp.go）；`sysproxy/` 三平台（common 校验 + linux gsettings/win 注册表/mac networksetup）；`config.json` 执行目录 + mtime/hash 热重载 + 旗标>config>默认；默认绑 `127.0.0.1:40000`；main.go 重写接线；proxy/config/sysproxy 单测全绿 |

## 7. 关键决策记录（ADR 摘要）

1. **GUI 框架 = Wails v3**（钉 alpha 版本）+ React 19 + Vite + Tailwind v4；备选 Fyne v2.8（仅当 Wails 交叉编译 1 天无果）。理由：同类 VPN 客户端 NetBird 2026 用 Wails v3 重构；原生托盘；6 目标交叉编译官方支持
2. **分流引擎 = 独立 route 包**（不嵌入 mihomo 内核）。之前 POC（archive/previous-poc）的 mihomo 内嵌方案是"反向拓扑"（不同需求），未采用
3. **双上游 merge 顺序**：badafans 先（主上游源），6Kmfi6HP 后（fork，含 scanner；冲突时 fork 侧优先）
4. **远端推送**：M5 时备份后 force-push 覆盖 main（archive/previous-poc 已存旧内容）
5. **UDP 不走隧道**（上游限制）：规则仅作用 TCP CONNECT；UDP 全直连，文档明示
6. **系统代理**：mixed 端口（HTTP+SOCKS5 同端口嗅探）；GUI 模式默认绑 127.0.0.1

## 8. 已确认事实（勿重复调研）

- 上游 `6Kmfi6HP/warp-go` HEAD = `6d5ab6a`（2026-07-31）；`badafans/warp-go` HEAD = `ca2f0cc`
- 本地 Go 1.26.5（/usr/local/go1.26.5）；gh 认证为 `callacat`；本机 linux/arm64 无桌面
- `go.mod`: `module warp`，go 1.26.5，quic-go v0.61
- SOCKS5 CONNECT 分支在 `tunnel/masque.go`（`HandleSOCKS5`，CONNECT 处理 ~L786）；分流 seam 在建立 H3 CONNECT 之前
- 远端 `callacat/warp-go` 当前 main = `1ae4d38`（旧，已备份）
