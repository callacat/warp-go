# AGENTS.md — warp-go 接手指南

> 本文件供后续 Agent 快速了解项目。配合计划文档 `.omo/plans/warp-go-reinit-2026-07-31.md`（随进度更新）阅读。
> 最后更新: 2026-08-01

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
├── core/                    # (M4/M7) 可复用核心：Server 生命周期 + kernel.go（Kernel：MasqueClient+Engine，CLI/GUI/Android 三端共用）
├── route/                   # (M2) GEO 分流引擎：规则解析、匹配、rules.txt、GEO 下载
├── proxy/                   # (M3) mixed HTTP+SOCKS5 代理（首字节嗅探）
├── sysproxy/                # (M3) 系统代理设置（Windows/macOS/Linux；android 为 no-op stub）
├── autostart/               # (M6) 开机自启（Windows/macOS/Linux；android 为 no-op stub）
├── androidvpn/              # (M7) TUN 栈（sing-tun，//go:build android）+ decision.go 决策逻辑（宿主可测）
├── gui/                     # (M4/M7) Wails v3 GUI（React 前端）+ androidbridge.go JNI 桥 + build/android/（Android 工程）
├── registration/            # 上游既有：两步注册 API
├── tunnel/                  # 上游既有：MASQUE/QUIC 隧道、SOCKS5 TCP、UDP ASSOCIATE
├── scanner/                 # 上游既有：边缘延迟扫描（-scan）
├── rules/                   # (M6) 仓库内置默认规则（default-rules.txt，首启下载）
├── .github/workflows/       # sync-upstream / build-release / docker-ghcr
├── .omo/plans/              # 计划文档（随进度更新）
├── docs/                    # 上游逆向文档 + 新功能设计
├── CHANGELOG.md             # 版本变更记录
└── AGENTS.md                # 本文件
```

## 3. 运行时文件约定（v3，用户指定：程序执行目录）

| 文件 | 位置 | 说明 |
|---|---|---|
| `config.json` | **执行目录** | 主配置（监听端口/规则路径/GEO 仓库与 URL/自动更新时间/代理开关/下载加速前缀）；**文件变更热重载** |
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
go test ./...       # 测试（scanner/route/proxy/androidvpn/decision 有单测）
wails3 version      # M4 GUI: v3.0.0-alpha2.119（已装 /root/go/bin）
node --version      # M4 GUI 前端: v22（已装）
# GUI（M4）: cd gui && wails3 build / npm run build
# 交叉编译: Taskfile 任务（M4）
# Linux GUI 构建依赖: libgtk-4-dev + libwebkitgtk-6.0-dev（本地已装）
# Android（M7）: 本地无 SDK/NDK/JDK → 走 CI build-android job；本地仅做：
GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build ./...   # android 可移植性编译门（T2 stub）
go test ./androidvpn/... ./gui/...                       # 决策逻辑 + androidconfig 宿主单测
```

验证方式（无桌面环境，v3 约定）：
- CLI：`./warp -reg` 注册 → `./warp -config config.json` 启动 → `curl --socks5-hostname 127.0.0.1:40000 https://www.cloudflare.com/cdn-cgi/trace`（预期 `warp=on`）
- 规则引擎：`go test ./route/...` + 配置文件方式启动（改 rules.txt 验证热重载）
- GUI：**配置文件方式启动冒烟**（无截图）；产物交付用户实测
- Docker：`docker pull ghcr.io/callacat/warp-go:latest` → 冒烟
- Android（M7）：**CI-only 构建**（`build-android` job：JDK 21 + SDK + NDK r27 + c-shared arm64/x86_64 + gradle APK/AAB）→ 下载 `app-debug.apk` 确认非平凡大小；**运行时行为需真机测试**（无设备/模拟器）——见 android 计划文档 §11 验收项（TUN `warp=on`、consent UX、GEO 分流、Always-On 重启、JNI 无 `UnsatisfiedLinkError` 等）

## 6.5 已完成里程碑（2026-08-01）

| 里程碑 | 状态 | 说明 |
|---|---|---|
| M0 环境基线 | ✅ | Go 1.26.5；wails3 alpha2.119；gtk4+webkitgtk-6.0；node22 |
| M1 三个工作流 | ✅ | sync-upstream（双上游，冲突即停）/ build-release（test+5平台+Release）/ docker-ghcr（多架构）→ GHCR；每个末尾 cleanup-runs.sh（30 天前 runs，保留最新 20） |
| M2 route 包 | ✅ | 规则解析/rules.txt 模板与热重载/GEO 下载(SHA-1+proto 校验+原子写)/匹配引擎/单测 18 个全绿 |
| M2.5 SOCKS5 分流集成 | ✅ | 在 `proxy` 包实现（非 tunnel 侧）：`Config.Router` + `Config.TunnelDial` 缝；`proxy_test.go` 覆盖 direct/proxy/未命中兜底/nil Router 全隧道 4 路径；`dial()` 未命中→本地直连、命中 proxy→隧道（TunnelDial 未配则报错） |
| M3 系统代理+config | ✅ | `proxy/` mixed HTTP+SOCKS5（首字节嗅探）+ UDP ASSOCIATE 中继（udp.go）；`sysproxy/` 三平台（common 校验 + linux gsettings/win 注册表/mac networksetup）；`config.json` 执行目录 + mtime/hash 热重载 + 旗标>config>默认；默认绑 `127.0.0.1:40000`；main.go 重写接线；proxy/config/sysproxy 单测全绿 |
| M4 GUI + core | ✅ | `core/` Server 生命周期抽取（CLI/GUI 共用，含 SetSystemProxy/ReloadRules/SaveConfig）；`gui/` Wails v3（main.go + service.go + logs.go + React 19 前端五页：状态/规则/GEO/设置/日志）；前端 npm build 通过；**本地 GTK 4.6 < 4.10 无法编译 wails → GUI 构建走 CI**（build-gui 分平台 job，ubuntu-24.04 有 GTK 4.14） |
| M5 发布 | ✅ | README/AGENTS.md 重写；Dockerfile 端口 40000；docker-compose.example.yml；推送远端（备份后 force-push）；tag v0.2.0 → Actions **全绿**（5 平台 CLI + 3 平台 GUI + Release + GHCR 镜像）；Release 产物本地验证（CLI 配置启动/GEO 下载/分流匹配/Docker 冒烟）全通过 |
| M6 维护增强 | ✅ | REJECT 广告拦截（route+proxy+GUI 拦截统计）；默认规则托管 `rules/default-rules.txt` + 首启 GitHub 下载（失败回退内置模板）；GitHub 下载加速前缀（`download_proxy`，默认 gh-proxy.org，GUI 可配）；GUI 首启死锁根因修复（InitDefaults 二次加锁）；开启系统代理自动启动内核；托盘退出修复；侧边栏展开按钮修复；流量统计恒 0 修复；tag v0.4.0 |
| M7 Android | ✅ | **Android 版（v0.5.0）**：`core.Kernel` 抽取（MasqueClient+Engine+注册，CLI/GUI/Android 三端共用，Server 公开 API 不变）；`gui/androidbridge.go` JNI 桥（nativeStartVpn(fd)/nativeStopVpn，`//export Java_com_wails_app_WarpVpnService_*` 与 Wails 18 导出共存）；`WarpVpnService.java`（VpnService.Builder establish→fd→JNI + dataSync 前台通知）；MainActivity `VpnService.prepare()` consent（singleTask）；manifest VpnService+BIND_VPN_SERVICE+uses-feature vpn；androidvpn 决策逻辑宿主可测（decision.go，reject 绝不拨号）；geoip 真实 IP 修复；CI `build-android` job（JDK21+SDK+NDK r27 → c-shared + APK/AAB）；**无真机验证 → CI-only 构建 + 真机验收清单**（见 §6/§8）；tag v0.5.0 |
| M7.5 Android 可用性修复 + 主题 | ✅ | **Android 沙箱锚定（v0.5.1）**：`core.Options.DataDir` + `resolveWithDir`（非空锚定到沙箱，空保持执行目录）；`gui/datadir_{android,other}.go`（Android=`getFilesDir()`，桌面空串）；`serverInstance()` 传入 DataDir + android 空值守卫 → 修复"生成默认配置 /system/bin/config.json 失败"、注册写盘失败、默认规则不可见（GUI 服务层与 JNI `buildAndroidConfig` 沙箱路径对齐）；`initLogging()` + `log.SetFlags(0)` 去重日志双时间戳；注销提示移到页面顶部 + 立即刷新；侧边栏 `w-16 md:w-52` 竖屏自适应；`useTheme` 三态主题（浅色/深色/跟随系统，Wails `System.IsDarkMode()` + 5 平台 `Events.On` + matchMedia 回退，设置页外观分段选择）；vitest 引入（theme/useTheme 18 单测）；tag v0.5.1（计划中） |

## 6.6 上游冲突处理（重要）

**冲突面**：仅 4 个文件与上游重叠（`main.go` / `tunnel/masque.go` / `tunnel/udp.go` / `registration.go` + go.mod）；`core/ proxy/ route/ sysproxy/ gui/` 全是独立包，上游永不触碰 → 零冲突。

**策略**（sync-upstream.yml 已实现）：
1. 合并顺序：badafans 先 → 6Kmfi6HP 后（fork 冲突时占优）
2. 冲突即停：`git merge --abort` + `conflict ⚠️` 标签 + issue 通知，**绝不自动解决**
3. 冲突分级处理：
   - `tunnel/masque.go`：追加式改动（RouteFunc/DialTunnel），大概率自动合并
   - `main.go`：我们的薄壳（~450 行）——保留薄壳 + 移植上游新 flag 到 core
   - `go.mod`：取并集 + `go mod tidy`
   - 独立包：永不冲突
4. 验证：解决后 `go build ./... && go test ./...`（core 测试保证核心不回归）

## 6.7 构建策略（资源优化）

- **本机只做 CLI 构建验证**（纯 Go 秒级）；GUI 构建因本地 GTK 4.6 过旧（需 4.10+）
- **GUI / Docker / Release / Android 全部走 GitHub Actions**（不占本机磁盘）：
  - push main → docker-ghcr（linux/amd64+arm64 镜像 → GHCR）
  - tag v* → build-release（test → 5 平台 CLI + 3 平台 GUI → GitHub Release）
  - tag v* / dispatch → build-android（JDK21+SDK+NDK → APK/AAB artifact）
  - 下载产物到本机验证（`gh release download`）

## 7. 关键决策记录（ADR 摘要）

1. **GUI 框架 = Wails v3**（钉 alpha 版本）+ React 19 + Vite + Tailwind v4；备选 Fyne v2.8（仅当 Wails 交叉编译 1 天无果）。理由：同类 VPN 客户端 NetBird 2026 用 Wails v3 重构；原生托盘；6 目标交叉编译官方支持
2. **分流引擎 = 独立 route 包**（不嵌入 mihomo 内核）。之前 POC（archive/previous-poc）的 mihomo 内嵌方案是"反向拓扑"（不同需求），未采用
3. **双上游 merge 顺序**：badafans 先（主上游源），6Kmfi6HP 后（fork，含 scanner；冲突时 fork 侧优先）
4. **远端推送**：M5 时备份后 force-push 覆盖 main（archive/previous-poc 已存旧内容）
5. **UDP 不走隧道**（上游限制）：规则仅作用 TCP CONNECT；UDP 全直连，文档明示
6. **系统代理**：mixed 端口（HTTP+SOCKS5 同端口嗅探）；GUI 模式默认绑 127.0.0.1
7. **GitHub 下载加速**：`download_proxy`（默认 `https://gh-proxy.org/`）仅对 github.com / raw.githubusercontent.com 的下载 URL 生效，非 GitHub 地址（镜像仓库/本地测试）原样，置空关闭；GUI 可配
8. **REJECT 行为**：规则 `reject` 命中即拒连（SOCKS5 0x02 / HTTP 403，绝不建连）；命中计入 Stats.RejectedHits，前端拦截统计卡展示
9. **Android 形态 = Wails v3 壳 + 自写 Java VpnService**（不用 gomobile）：TUN fd 以 `jint` 过 JNI 到 Go（`//export Java_com_wails_app_WarpVpnService_*`，与 Wails 18 导出共存）；包名保持 `com.wails.app`（JNI 符号烘焙进包名，仅用户可见名改 "warp-go"）
10. **Kernel 三端复用**：`core.Kernel`（MasqueClient+route.Engine+注册）供 CLI/GUI/Android 共用；androidvpn 无 SOCKS 监听；Android 运行时文件在沙箱 `getFilesDir()`（D8 路径分支），桌面保持执行目录
11. **Android 构建 CI-only**：本地无 SDK/NDK/JDK → 仅 CI 构建（`build-android` job），真机行为验收清单交付用户（无设备验证）

## 8. 已确认事实（勿重复调研）

- 上游 `6Kmfi6HP/warp-go` HEAD = `6d5ab6a`（2026-07-31）；`badafans/warp-go` HEAD = `ca2f0cc`
- 本地 Go 1.26.5（/usr/local/go1.26.5）；gh 认证为 `callacat`；本机 linux/arm64 无桌面
- `go.mod`: `module warp`，go 1.26.5，quic-go v0.61
- SOCKS5 CONNECT 分支在 `tunnel/masque.go`（`HandleSOCKS5`，CONNECT 处理 ~L786）；分流 seam 在建立 H3 CONNECT 之前
- 远端 `callacat/warp-go` main = `165d565`（2026-08-01）；tag `v0.4.0`（2026-08-01）；旧内容在 `archive/previous-poc`
- `rules/default-rules.txt` 已上线（首启下载 200）；`download_proxy` 默认 gh-proxy.org，GEO 经加速实测下载成功
- 本机 GUI 构建限制：GTK 4.6.9 < wails 需要的 4.10（GtkFileDialog）→ 走 CI
- 本地 HEAD = `f2383b4`（T12，2026-08-01）；`main` 即将为 v0.5.0（T13 文档 + tag）
- 本地 Android 环境：**无 SDK/NDK/JDK/设备** → android 构建仅 CI（`build-android` job）；本地只跑 `GOOS=android go build ./...` + `go test ./androidvpn/... ./gui/...`
- **v0.5.1 修复（2026-08-01）**：`core.Options.DataDir`（`resolveWithDir` 分派，空值=默认执行目录锚定，桌面零回归）；`gui/datadir_{android,other}.go`；`gui` module 已 `go mod tidy`（补 sing-tun 等间接依赖）；前端引入 vitest（theme/useTheme 18 单测）；主题事件名 5 平台（`common:/windows:/linux:/android:/ios:ThemeChanged`），Android 由 MainActivity `emitTheme()` 发 `android:ThemeChanged`；`@wailsio/runtime` 的 `Events.On` 回调收到的是 `WailsEvent{name,data}` 对象（payload 在 `.data`，Android 为 JSON 字符串 `{"isDarkMode":bool}`）
- `go.mod` 依赖：sing-tun v0.8.11 为 direct require（T1 已提升）；无 gomobile
- `androidvpn/` 已接线（不再是孤儿包）：`decision.go` 宿主可测（`//go:build android || linux`），TUN 栈 `androidvpn.go` 仅 `//go:build android`
- JNI 导出面：`gui/androidbridge.go`（`Java_com_wails_app_WarpVpnService_nativeStartVpn/nativeStopVpn`）+ Java 侧 `WarpVpnService.java`（`gui/build/android/app/src/main/java/com/wails/app/`）；CI 双侧 grep 断言保障符号名一致
