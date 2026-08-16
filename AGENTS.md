# AGENTS.md — warp-go 接手指南

> 本文件供后续 Agent 快速了解项目。配合计划文档 `.omo/plans/warp-go-reinit-2026-07-31.md`（随进度更新）阅读。
> 最后更新: 2026-08-07（v0.5.26）

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

## 3. 运行时文件约定（v4，用户指定：运行目录下的 config/ 子目录）

| 文件 | 位置 | 说明 |
|---|---|---|
| `config.json` | **运行目录/config/**（自动创建） | 主配置（监听端口/规则路径/GEO 仓库与 URL/自动更新时间/代理开关/下载加速前缀）；**文件变更热重载** |
| `rules.txt` | config/（config 可改） | 路由规则文本；GUI 增删改 + 热重载 |
| `reg.json` | config/ | WARP 注册信息（上游原约定） |
| `geo/` | config/geo/ | geosite.dat + geoip-lite.dat |
| `logs/` | config/logs/ | 日志（可选） |

- **根目录选定**（`core.baseExecRoot`，桌面/Docker）：可执行目录（可写）→
  **当前工作目录**（可写；Docker 的 WORKDIR `/data` 挂载卷——核心修复）→
  用户配置目录（`~/.config/warp-go` 等）→ 可执行目录兜底。
- **所有运行时路径**（config.json/reg.json/rules.txt/geo）经
  `resolveExecPath` 统一收拢进 `<根>/config/`；Docker 只需映射
  `./warp-config:/data/config` 一个目录。
- **Android 例外**：`DataDir` 非空（`gui/datadir_android.go`）时保持沙箱根
  锚定（`getFilesDir()` 根，不套 `config/` 子目录）——真机路径约定未变。
- **一次性旧布局迁移**（`core.migrateLegacyConfig`）：`config/config.json`
  不存在但旧执行根有散落文件时复制进 `config/`；幂等、非破坏（不删原文件）。
- 优先级：**旗标 > config.json > 默认值**。热重载基于 mtime + 内容 hash 检测。

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

- **Android 调试包（debugdiag，v0.5.26）**：build-tag 门控——正式版 `-tags production,android,with_gvisor`（**不带 debugdiag**）编译 `androidvpn/debugdiag_stub.go` no-op stub，零调试代码；调试版在 tag 基础上**加 `,debugdiag`**。构建：`workflow_dispatch` 触发 `.github/workflows/android-debugdiag.yml` → 下载 artifact `warp-android-debugdiag` 的 `app-release.apk`（同 `warp-release.p12` 签名，覆盖安装正式版不丢 reg.json）。用户导出流程：停止 VPN → GUI 日志页显示"调试数据已导出 "+URI → 文件管理器 Download/`warp-go-debugdiag-*.zip` → 发给开发者。
- 验证方式（无桌面环境，v3 约定）：
- CLI：`./warp -reg` 注册 → `./warp -config config.json` 启动 → `curl --socks5-hostname 127.0.0.1:40000 https://www.cloudflare.com/cdn-cgi/trace`（预期 `warp=on`）
- 规则引擎：`go test ./route/...` + 配置文件方式启动（改 rules.txt 验证热重载）
- GUI：**配置文件方式启动冒烟**（无截图）；产物交付用户实测
- Docker：`docker pull ghcr.io/callacat/warp-go:latest` → 冒烟
- Android（M7）：**CI-only 构建**（`build-android` job：JDK 21 + SDK + NDK r27 + c-shared arm64/x86_64 + gradle APK/AAB）→ 下载 `app-debug.apk` 确认非平凡大小；**运行时行为经远程模拟器验收**（v0.5.17 起，见下）——验收项见 android 计划文档 §11（TUN `warp=on`、consent UX、GEO 分流、Always-On 重启、JNI 无 `UnsatisfiedLinkError` 等）
- **远程模拟器验收（v0.5.17 起可行）**：装 `adb`（`apt install adb`，本机 aarch64 需 apt 版而非 Google x86_64 platform-tools）+ 项目级技能 `httprunner/skills@android-adb`（`.agents/skills/android-adb/`）→ `adb connect <IP>:5555` → `adb install -r app-release.apk` → `adb shell uiautomator dump` + `input tap` 驱动 consent/注册/启动。已验证流程：通知权限"允许"（permission_allow_button）→ VPN 授权"确定"（`android:id/button1`）→ 状态页"一键注册"→ 状态页"启动"（bounds 随注册后布局变化，以 dump 为准）。**注意**：模拟器 adb shell 以 root 运行，root 流量绕过 VpnService 路由 → 设备内 `curl` 恒为 `warp=off`；隧道连通性用 `tun0` 接口 rx/tx 计数增长验证（`cat /proc/net/dev`）。

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
| M7.5 Android 可用性修复 + 主题 | ✅ | **Android 沙箱锚定（v0.5.1）**：`core.Options.DataDir` + `resolveWithDir`（非空锚定到沙箱，空保持执行目录）；`gui/datadir_{android,other}.go`（Android=`getFilesDir()`，桌面空串）；`serverInstance()` 传入 DataDir + android 空值守卫 → 修复"生成默认配置 /system/bin/config.json 失败"、注册写盘失败、默认规则不可见（GUI 服务层与 JNI `buildAndroidConfig` 沙箱路径对齐）；`initLogging()` + `log.SetFlags(0)` 去重日志双时间戳；注销提示移到页面顶部 + 立即刷新；侧边栏 `w-16 md:w-52` 竖屏自适应；`useTheme` 三态主题（浅色/深色/跟随系统，Wails `System.IsDarkMode()` + 5 平台 `Events.On` + matchMedia 回退，设置页外观分段选择）；vitest 引入（theme/useTheme 18 单测）；tag v0.5.1 |
| M7.6 Android 真机二轮修复 + 底部导航（v0.5.2） | ✅ | **内核启动根治**：反向 JNI 桥（`MainActivity.requestStartVpn/requestStopVpn` + `nativeBridgeReady` 缓存全局引用/方法 ID，C helper 封装 JNI 调用镜像 Wails 模式）；`service.go` Start/Stop/IsRunning 在 Android 上桥接 VpnService（SOCKS 路径废弃）；`androidbridge.go` kernel/vpn goroutine 错误记录到 `lastErr` + log。**日志时序**：`log.SetOutput` 移到包 `init()`（早于一切 JNI 调用）。**注册状态弹性**：`dataDir()` 首次成功缓存（Wails StoragePath 桥接抖动返回 `""` 导致误报未注册），GetStatus 失败路径兜底检查沙箱 reg.json。**扫描无候选**：`scanFallback()` 空端点报错 + 注册 `extractPeerEndpoints` Host 回退。**状态栏覆盖**：`WindowCompat.setDecorFitsSystemWindows(true)`。**底部导航**：`<md` 隐藏侧边栏 + 固定底部导航（`lib/nav.ts` + 7 单测）。CI build-android 加 JNI 符号双侧 grep 断言 |
| M7.7 Android 构建/启动健壮性修复（v0.5.3） | ✅ | **cgo 编译修复**：C 类型 nil 比较用 `unsafe.Pointer`、`var needsDetach C.int`（`C.jclass == nil` 编译失败，CI grep 抓不到类型错误）。**JNI 签名对齐**：`nativeBridgeReady` Java 改 `int`（与 Go `C.jint` 一致，消除 void vs C.jint 未定义行为），CI 断言升级为返回类型签名级。**启动失败回滚**：kernel/vpn 异步启动失败即回滚（cancel+拆除双方+started=false，`androidRuntime.kernel==kernel` 校验防旧实例覆盖新状态），成功清 `lastErr`；tag v0.5.3 |
| M8 版本号 + 检查更新（v0.5.4） | ✅ | **产物版本号**：单源 = release tag，`-ldflags -X main.version` 注入 CLI（`-version` flag）/ GUI（设置页"关于"卡片）/ Android（gradle versionCode 语义单调递增 0.5.3→503）；Windows PE 版本资源（`go:generate goversioninfo` + `versioninfo.json`，CI sed 写版本再生成 `.syso`，资源管理器可见 → 降低报毒误报）；Release 产物命名带版本。**检查更新**：`core/updater.go` 查 GitHub Releases API（`compareVersions` 纯函数 + 单测），CLI `-check-update` + GUI 设置页按钮，网络失败非致命；CI build-release 各 job 注入 VERSION |
| M8.5 v0.5.7 反馈修复（win/Android 真机） | ✅ | **注册信息完整显示**：`core.Status()` 在 s.reg 为 nil 时从磁盘补读并缓存（GUI 未启动也显示 9 字段）；Register 同步缓存 / Deregister 清空。**初始化完成门控**：`Status.InitDone` + 前端禁用启动按钮直至默认规则/GEO 就绪。**Android 启动/停止/注销全程日志**（WarpVpnService 失败路径经 nativeLogMessage 转发）。**Android 日志系统时间**：`import _ "time/tzdata"`（Android 无系统时区库）。**Android 通知栏对比度**：移除 `SYSTEM_UI_FLAG_LIGHT_STATUS_BAR`（深底配深色图标看不见）。**应用图标**：appicon.png → Windows .ico（PE 资源）/ macOS .icns / Android mipmap 五密度。**浅色日志框** + 长路径 break-all。**注册 id 显示**：Wails 多返回值是元组 `[boolean,string]`，api.ts 兼容两种形态；tag v0.5.7 |
| M8.6 v0.5.8 反馈修复（win/Android 真机） | ✅ | **双重归一化修复**：StatusPage/GeoPage/SettingsPage 对 getStatus/getGeo/getConfig 返回值二次 fromXxx（已归一化 camelCase，二次只认 snake_case → initDone/isAndroid/sysProxyOn 恒 false）→ 页面直接用返回值 + null 兜底。**InitDone 持久化**：`core.Server.InitDone()` 基于 rules.txt+GEO 文件就绪，重开 GUI 不再重复初始化/卡启动。**主题默认跟随系统**：useTheme mount 后延迟 300ms 重查 IsDarkMode（Android bridge 时序）。**系统代理真实状态**：`sysproxy.Enabled(addr)` 三平台读（win 注册表/darwin networksetup/linux gsettings），Status.SysProxyOn 读真实状态，前端开关跟随外部关闭。**停止/退出同步清系统代理**（shutdown 只清指向本程序的）；**关闭按钮最小化到托盘**（RegisterHook WindowClosing + Cancel + Hide，macOS 除外）；tag v0.5.8 |
| M8.7 v0.5.9 反馈修复（Android 真机） | ✅ | **VPN 建立失败修复**：WarpVpnService 从沙箱 reg.json 读 assigned_ipv4/6 兜底（startVpnService 不传 extras → VpnService.Builder 无地址 establish 抛异常）。**注销无确认框**：window.confirm 在 Android WebView 静默 false → 自绘两段确认（首次点击进确认态，5s 自动取消），全平台一致。**启动报 `-ip ""` 边缘解析失败**：`core.ResolveEdgeAddrs` 双空（cfg.EdgeAddr + optsEdgeIP 均空，Android 桥常态）回落 "4"（注册 IPv4 端口展开），不再把空串当显式端点。**应用扫描 IP 后再次启动 GUI ANR 卡死 + VPN 无网**：`nativeStartVpn` 原在 Java 主线程同步装配 Kernel——`NewMasqueClient` 初始拨号无限指数退避重试阻塞主线程 → 5s ANR；TUN fd 已建立但内核未接手 → 无流量。现异步化（`startVpnKernel` goroutine，前置校验后立即返回 0"已受理"）+ 装配取消信号（nativeStartVpn 前置建 ctx，装配各阶段查 ctx.Err，nativeStopVpn 装配中到达即取消）+ 新增 JNI 导出 `nativeVpnRunning`（Java 重入守卫区分"真在运行"与"异步失败"→ stale TUN 重建）；`vpnPfd/nativeRunning` 置位提前到 nativeStartVpn 前；CI JNI 断言加 nativeVpnRunning 签名级检查 |
| M8.8 v0.5.11 反馈修复（Android 真机） | ✅ | **边缘不可达无限重试**：`NewMasqueClient` 初始拨号无限指数退避，移动网络 QUIC 被封锁时永久重连 + 无限刷日志 + 状态停"连接中"。新增 `tunnel.NewMasqueClientContext(ctx,...)` / `core.NewKernelContext(ctx,...)`（拨号循环 select 监听外部 ctx，可取消；`NewKernel` 委托 background，桌面零回归）；Android 装配 ctx 改 `context.WithTimeout(30s)`（`androidDialTimeout`），超时报"连接边缘超时，请检查网络"。**点停止无效（双重根因）**：① Go 侧装配中 nativeStopVpn 只 cancel() 但拨号不响应 ctx → NewKernelContext 取消后立即中止；② Java 侧前台服务（startForegroundService）用 `stopService` 无法停止（Android 8+ 需先 stopForeground，否则 onDestroy 从不触发）→ 新增 `WarpVpnService.stop(Context)`（`stopForeground(STOP_FOREGROUND_REMOVE)`+`stopSelf()`），`requestStopVpn` 改调它；补 `TestKernelNewContextCanceledSkipsDial` 单测 |
| M8.9 v0.5.12 反馈修复（win/Android 真机） | ✅ | **Telegram 无法连接**：默认规则未覆盖 TG——`149.154.175.100` 等 TG IP 落隐式 direct，网络封锁直连失败。默认模板 + `rules/default-rules.txt` 新增 `proxy,geoip:telegram`（geoip 类别大小写不敏感，`Lookup` ToUpper→`TELEGRAM`）；补 telegram 匹配/解析/加载单测。**Android 检查更新在应用内 WebView 打开**：`<a target=_blank>` 被 WebView 捕获 → 新增 `Service.OpenExternalBrowser`（桌面 Wails `application.Get().Browser.OpenURL`，Android 反向 JNI `MainActivity.openExternalBrowser` 用 `Intent.ACTION_VIEW` 跳第三方浏览器），前端"前往下载"改调它 |
| M9 运行时文件统一 config/ 子目录（v0.5.12） | ✅ | **Docker 版"自动生成注册文件无法保存"根因修复**：旧 `resolveExecPath` 锚定可执行目录，Docker exe 在只读 `/usr/local/bin` → 回退容器层 `~/.config/warp-go`，从不落挂载卷 `/data`。现 `core.baseExecRoot`：可执行目录（可写）→ **当前工作目录**（可写；Docker WORKDIR `/data` 挂载卷——核心修复）→ 用户配置目录 → 兜底；`resolveExecPath` 统一收拢进 `<根>/config/`（config.json/reg.json/rules.txt/geo），自动 `MkdirAll` + 一次性旧布局迁移（`migrateLegacyConfig`，幂等非破坏）。**Android 例外**：`DataDir` 非空仍锚定沙箱根（不套 config/ 子目录）。**Docker compose 挂载改 `./warp-config:/data/config`**（只需映射 config 一个目录）；`configDirWritable` 以"能否在候选根下创建 config/ 子目录"探测（适配挂载点父目录 root 属主场景）。测试：6 新增 + 3 更新路径契约测试；Docker 真实冒烟（reg/config/rules/geo 全落挂载） |
| M9.5 Android 自路由修复（v0.5.14） | ✅ | **连接所有边缘地址失败根因**：`VpnService.establish()` 全量路由后应用自身新 socket 也走 TUN（未 protect 时），而 TUN 在拨号成功后才被 sing-tun 读取 → QUIC ClientHello 滞留 tun 里、所有边缘握手超时。修复：`tunnel.socketProtector` 钩子（`dialAddr` 建 UDP 拨号 socket 后、发包前调用）+ Android 桥 `WarpVpnService.protectSocket(fd)`（`VpnService.protect()` 豁免拨号 socket 走物理网络；DoH 复用同一 QUIC 连接无需单独保护）。**启动失败"无法停止"修复**：`failStart`/`rollback` 额外经 `kernelFailed(msg)` JNI 通知 Java 自拆除（stopForeground+stopSelf+关 TUN fd），停止按钮幂等生效。`warpCtl` 缓存 WarpVpnService 类/方法 ID（nativeStartVpn 主线程 GetObjectClass 缓存，避免 goroutine FindClass 错失 classloader）。拨号总超时改可配置 `config.json` `dial_timeout_seconds`（默认 60s）；补 T6 androidconfig 测试 + CI `protectSocket`/`kernelFailed` 双侧签名 grep 断言；tag v0.5.14 |
| M9.6 Android udpnat panic + GUI 配置快照（v0.5.17） | ✅ | **Android `panic: invalid timeout` SIGABRT 根因**：`androidvpn.Vpn.Start` 构造 `tun.StackOptions` 未设 `UDPTimeout/ICMPTimeout`（自 v0.5.15 强制 gVisor 才触达此路径）→ `NewUDPForwarder` → sing v0.8.0 `udpnat.New` 对 `timeout==0` **panic 而非返回错误** → 异步 goroutine 崩溃整个进程。修复：`UDPTimeout: 5m` / `ICMPTimeout: 10s`（对齐 sing-box `constant/timeout.go`）+ `kernel.Start`/`vpn.Start` 异步 goroutine 加 `recover` 兜底（未来库 panic 走 failStart 正常回滚，不 SIGABRT）。**GUI 保存配置后切页看不到变更**：`Server.SaveConfig` 只写盘不更新 `s.cfg` 快照、`applyConfigReload` 也不写回 → `Status().Config`（GUI `GetConfig` 数据源）恒为启动时旧值直到重启。修复：两者都同步 `s.cfg`（内存锚定路径、磁盘仍相对）+ `applyConfigReload` 补路径锚定；新增 `TestSaveConfigUpdatesSnapshot` 回归。**远程模拟器真机验收通过**（LDPlayer `100.64.0.6:5555`，Android 14 x86_64）：安装 APK ✓、consent 流 ✓（通知"允许"+VPN"确定"）、**app 内一键注册 ✓**（沙箱 reg.json 属主 u0_a114=app 自身）、**启动后 tun0 建立且流量持续增长**（rx 计数递增）、`panic: invalid timeout` 计数 0、进程存活无崩溃；`warp=off` 为模拟器 root shell 绕过 VPN 路由所致（非 bug） |
| M9.7 Android 真机三层修复（v0.5.18/0.5.19） | ✅ | **① direct 环路风暴（v0.5.18）**：v0.5.14 只 protect QUIC 拨号 socket，direct TCP/UDP socket 未豁免 → 重新进 TUN → 环路（模拟器 tun0 TX 33GB、CPU 456%、浏览器不通、停止按钮饿死）。修复：`androidvpn.SetSocketProtector`（TCP 用 `Dialer.Control`、UDP 用 `SyscallConn`），桥侧双注册。**② 国外流量直连被墙（v0.5.19）**：TUN 收到 IP 字面量 → `route.Match` 只走 geoip → 国外 IP（Google 172.217.x）miss → 兜底 direct → 直连超时。修复：`decideAction` 未命中兜底 **proxy**（VPN 语义：除显式 direct/reject 外全走隧道）。**③ 停止按钮失效（v0.5.19）**：VpnService 被系统 VpnManager 绑定 + 前台，`stopSelf()` 可能不触发 `onDestroy`（真机：无 onDestroy 日志、tun0 持续涨）→ `WarpVpnService.stop()` 直接 `stopNativeAndClose()`（幂等）；`androidRequestVpnStop` 桥未就绪改打日志。测试：`TestDecideAction` T4/T6 更新；Go 全绿 + Android 交叉编译通过 |
| M9.8 GUI 日志页冻结 + Android 装配 ctx 泄漏（v0.5.20） | ✅ | **① Windows 日志页不自动刷新**：`LogsPage.tsx` 去重只比长度，日志达 200 上限后新条目顶替最旧而长度不变 → 页面冻结（须清空/切页）。修复：比较尾条（time+level+msg）的纯函数 `logsTailChanged` + 8 个 vitest 回归。**② Android 开启后无网络**：真机日志 `[tun] 拨号失败 ... use of closed network connection`。根因：`startVpnKernel` 把带 60s 拨号超时的装配 ctx 继续传给 `kernel.Start/vpn.Start` 作运行期 ctx——移动网络拨号接近 60s 时 sing-tun 栈随 ctx 取消整体关闭但 `started` 仍 true（"VPN 开"无网络）。修复：装配完成后切 background 派生运行期 ctx（仅由 nativeStopVpn 控制）；ctx 校验+状态写入+runCtx 替换合入单临界区（防 nativeStopVpn 插入竞态）；rollback 加 `current` 守卫（过期实例不再误拆新实例 Java 服务）。回归 `TestKernelStartRuntimeCtxCancelKeepsKernel`。**③ 默认规则补 `proxy,geoip:google`**（8.8.8.8 走隧道）。**④ 主题持久化**：`theme_mode` 写入 config.json。**模拟器验收（v0.5.20）**：安装 versionCode=520 → consent 流 → 一键注册（IPv4 172.16.0.2）→ 启动后 tun0 流量持续增长（5.2MB→19.6MB）→ logcat 无 closed-connection/panic |
| M9.9 Android 隧道黑洞不重连 + 签名一致性（v0.5.23） | ✅ | **① Android 外网不通（v0.5.21 回归）**：用户日志 `H3 CONNECT 69.171.235.22:443 失败：读取 CONNECT 响应失败：http3: parsing frame failed: deadline exceeded` 反复出现但从不重连（境内 direct 正常、境外全超时，重启才恢复）。根因：v0.5.21 把 CONNECT 超时重连判定改为"窗口内 3 个**不同目标**失败才重连"（`map[string]struct{}` 去重）——浏览器对少数站点并发重试时**同目标反复失败在 distinct 去重下永不累计**（用户日志 2 目标×各 2 次 = distinct 2 < 3），QUIC 连接黑洞后外网永久不通。修复：`noteProgressingCONNECTFailure`/`noteProgressingQueryTimeout`（DoH 同病，只有 2 个固定服务器同样永不达标）改**计数语义**（`map[string]int` 累计总数），`connectFailureTargets` 3→2；单目标首败仍不重连（保留 v0.5.21 保护共享连接场景），窗口内累计 2 次即触发 retire+重连。回归：`TestConnectFailureSameTargetTwiceTriggersReconnect`/`TestConnectFailureSuccessResetsWindow` 等 6 测试。**② APK 签名不一致（覆盖安装必须卸载）**：CI `assembleRelease` 无 keystore 时兜底 debug keystore，runner 每次现生成 → 签名必不同。修复：仓库内置固定 `gui/build/android/app/warp-release.p12`（openssl 生成 PKCS12，密码 warp123456，随仓库公开仅保覆盖升级签名一致），build.gradle release signingConfig 默认引用，设 `ANDROID_KEYSTORE_*` 环境变量可覆盖为正式 keystore |
| M9.10 Android 隧道目标 IP 边缘不可达 + 取消热加载 + 主题持久化修复（v0.5.24） | ✅ | **① Android 外网依旧不通（v0.5.24 决定性实验确证新根因）**：`TestIPEdgeProbe`（真实边缘 + 用户 reg.json）——隧道内 DoH 解析的 facebook IP（57.145.12.1）CONNECT 成功，Android 系统 DNS 解析的同一域名 IP（69.171.235.22）CONNECT hang 到 deadline。**WARP 边缘 CONNECT 的目标 IP 必须处于边缘网络视图**：域名路径用隧道内 DoH 解析（天然边缘可达），TUN 只给系统 DNS 的 IP → 边缘连不到 → 全挂。修复：`tunnel.ResolveDNS` 导出 + `androidvpn/dns.go` DNS 拦截服务器（sing-box 标准架构：拦截 UDP:53 → 隧道 DoH 解析 → IP→域名映射 → NewConnectionEx 还原域名走 DialTunnel）。9 宿主单测。**接线完成**：`core.Kernel.ResolveDNS`（dialer 接口扩展）+ `WarpVpnService.java addDnsServer(198.18.0.1)` + `androidbridge` 注入 `vpnCfg.TunnelDNS = kernel.ResolveDNS` + `NewPacketConnectionEx` 拦截 `198.18.0.1:53`。**② 取消 config.json 热加载（用户需求）**：删 `WatchConfig`/`applyConfigReload`/`configPollInterval`/`configFileState`/`stopWatch` 字段 + 3 测试；配置只在启动/显式保存读取，运行中修改需重启生效（**rules.txt 规则热重载保留**，独立功能）。根因：热加载每 2s 轮询回读磁盘，GUI 保存后被回写覆盖 → "GUI 改配置被自动重置"。**③ 主题持久化不生效 + GUI 配置重置**：`useTheme` effect（`[mode,systemDark]`）mount/OS 切换时用默认 "system" 写回 config.json 覆盖用户持久化主题，且触发整条 SaveConfig 链。修复：`useTheme.ts` 重写——mount 从 config.json 读取并应用持久化主题；只在用户显式点击（setMode）持久化，effect/OS 事件永不写文件。设置页文案同步（"重启后生效"）。`Server.SaveConfig` 同步内存快照保证 `GetConfig` 立即生效。 |
| M9.11 Android v0.5.24 回归修复：direct 域名还原死循环 + DNS 拦截 drop（v0.5.25） | ✅ | **① 国内 direct 全挂（v0.5.24 回归）**：真机日志 `拨号失败 49.7.252.24:443：lookup obus-cn.dc.heytapmobi.com: canceled`。根因：v0.5.24 的 IP→域名还原在 `NewConnectionEx` **无条件**应用——direct 分支也被还原域名 → `net.Dialer` 物理解析 → 系统 DNS 又进 TUN → 环路 canceled。修复：`androidvpn/decision.go` 新增 `decideTunnelTarget` 纯函数——**域名还原只用于 proxy 分支**（隧道内部再次 DoH 解析，CONNECT 目标永远边缘可达），direct 分支保留原始 IP 拨号（该 IP 是隧道 DoH 解析出的真实 IP，物理同样可达）。3 回归用例。**② DNS 拦截解析失败静默 drop → 系统挂起/fallback 裸 IP**：真机日志 `DNS 拦截：nebula-api-cn.heytapmobi.com 解析失败：没有 TypeA 记录` + `UDP → 114.114.114.114:53（直连）` + `[2001::1]:443 CONNECT 超时`。根因：隧道 DoH 对部分域名无 A/AAAA 记录时 `HandleQuery` 返回 nil **drop**——Android DNS 挂起或 fallback 物理 DNS（114）→ 本地视图 IP → 映射 miss → 裸 IP 走隧道边缘不可达。修复：解析失败返回 **SERVFAIL**（保留原 Question/ID/OpCode），Android 立即回退下个 DNS。 |
| M9.12 调试设施 debugdiag（v0.5.26） | ✅ | **build-tag 门控的遥测收集器**，诊断"Android 无法访问外网"但 CONNECT 隧道全建立。构建 tag：`-tags debugdiag` 启用（`androidvpn/debugdiag.go`）；正式版（CI `-tags production,android,with_gvisor` 不带 debugdiag）编译 `debugdiag_stub.go` no-op stub——**零 IO/内存/磁盘/网络，release 无调试痕迹**。数据写 `<沙箱根>/debugdiag/`（Android `getFilesDir()/debugdiag`）：`tunnels.tsv`（每关闭 TCP 隧道一行 `time seq host upBytes downBytes firstByteMs lifeMs err`，`firstByteMs=-1` = CONNECT 成功但无数据流回，关键诊断）；`udp.tsv`（每行 `time host kind bytes err`，kind = dns(53 非拦截漏直连)/quic(443 浏览器 HTTP/3 直连泄漏)/udp）；`tun0.tsv`（每 2s 一次 rx/tx 采样，区分"隧道建立但 payload 死"与"完全无流量"）。生命周期：VPN 启动 `androidvpn.DebugSetDir(root)`，停止/回滚 `DebugStop()`；导出走反向 JNI `MainActivity.exportDebugDiag()` 把 `debugdiag/` 打 zip 到 MediaStore Downloads（API 29+，`warp-go-debugdiag-<timestamp>.zip`），URI 打 GUI 日志页。CI 新工作流 `.github/workflows/android-debugdiag.yml`（workflow_dispatch）`-tags production,android,with_gvisor,debugdiag` + assembleRelease → artifact `warp-android-debugdiag`（versionCode 按 ref 派生，覆盖安装 v0.5.25+，`warp-release.p12` 签名不丢 reg.json）；tag v0.5.26 |
| M9.13 隧道死连快速恢复（v0.5.27） | ⚠️ **未解决** | **新 debug 包（20260808）数据驱动**：`tunnels.tsv` 42s 内 3 次隧道批量死亡（33× `network is unreachable` 全来自 `[::]:X` 双栈 socket 发往 IPv4 边缘 162.159.198.2:4443；`00:05:47` `quic: transport closed` 同毫秒 8 条流）+ 隧道死瞬间并发境外流全 `read tcp <境外IP>:443: connection reset by peer`（dn=0）→ 浏览器"打不开外网"。修复（tunnel/masque.go + scanner/probe.go）：①**socket 地址族收紧**——`net.ListenUDP("udp")`（双栈 `[::]`，IPv4-mapped 走 v6 路由表 → 无 v6 主机 ENETUNREACH）改显式 **`udp4`/`udp6`**（生产 dial + scanner 同步）；②**connBundle.dead 标志**（atomic.Bool，并发安全）——`noteDeadStream`/运行期探测观测到连接级故障即置位，`currentConnection` 与 `establishCONNECT` 立即把后续请求加入重连航班，**消除死连接上 10s×2 CONNECT 白等**；③**拨号时国际出口探测** `probeInternationalEgress`（8.8.8.8:443 隧道内 CONNECT，5s 超时，失败换下一个边缘）；④**运行期活性探测** `egressProbeLoop`（20s 周期 + `probeFn` 注入 seam）。5 新单测全绿 + 本地/CI 全部通过 + v0.5.27 已发布（APK 21MB）。**但用户真机复测仍失败（境外流量打不开），2026-08-08 决定放弃继续修复**——CI 全绿 ≠ 真机解决，完整证据链与接手方向见下方"未解决问题交接" |

---

### 未解决问题交接（2026-08-08）：Android 境外流量打不开（历经 v0.5.13→v0.5.27 共 9 轮修复未果）

> **状态（2026-08-16 更新）**：阶段6 已实施修复——**隧道重连自伤**。新 debugdiag 数据（72s 8 条 socket 代际、全部 `use of closed network connection` 本地拆线）锁定：共享 QUIC 连接被自身健康逻辑反复 retire，拖死所有在途并发流——探针单次失败即拆、单流非连接级错误即拆、单目标 CONNECT 非超时失败即拆，Android 上触发面（映射 miss 裸 IP 目标 + GMS/浏览器多并发流）远大于桌面，故桌面正常 Android 打不开。修复（`tunnel/client_conn.go` + `client_socks5.go`）：探针连续 2 次失败才 retire；`isConnectionLevelError` 类别化——真实连接级错误立即重连、裸 `net.ErrClosed` 跳过（他人已拆线）、其余走观察窗累计 2 次；`bundle.close` 补 reason 日志便于下次 debugdiag 归因。10 项单测 + 全量测试绿。**待真机验收**（东哥验收标准：真机打开境外网站 + warp=on + 批量死亡消失）。<br>阶段5 修复 — **QUIC:443 拦截**：浏览器 HTTP/3（QUIC:443）走 UDP 直连（`relayUDP` → 物理网络），运营商封 UDP/QUIC 直连 → 外网打不开。修复：在 TUN 栈 `NewPacketConnectionEx` 拦截 UDP:443，丢弃包让浏览器回退 TCP:443 → WARP 隧道（上游 warp-svc 只有 ConnectTcpProxy，不支持 CONNECT-UDP / RFC 9298，UDP 无法走隧道）。
>
> 以下为原交接信息（保留供参考）。

**症状**：Android 版 VPN 能启动、国内流量正常、隧道 CONNECT 全建立，但**境外流量打不开**（浏览器连接被重置/超时）。桌面 CLI（同网络）是否复现未验证。

**已确诊的事实链（debugdiag 20260808 + 多轮真机日志）**：
1. WARP 隧道本身工作（同一域名在隧道活着时 TUN 模式拿到满字节：`www.google.com dn=31155` 等）。
2. 隧道共享 QUIC 连接**周期性批量死亡**：`00:05:47` 同毫秒 8 条流 `quic: transport closed`；`15:22:00.461` 同毫秒 22 条流 `write udp [::]:X->162.159.198.2:4443: sendmsg: network is unreachable`（v0.5.26 的 `[::]` 双栈 socket）。
3. 隧道死的瞬间，同一连接上**所有并发境外流一起 RST**（`read tcp <境外IP>:443: connection reset by peer`、dn=0）——浏览器看到连接重置。
4. 旧布局（v0.5.24 前）已排除：IP 未处于边缘网络视图（已用隧道内 DoH + DNS 拦截修复）、direct 环路（已 protect 修复）、DNS drop（已 SERVFAIL 修复）。

**v0.5.27 已做**（本交接前最后一批）：udp4/udp6 显式绑定、dead 标志快速重连、拨号国际出口探测、20s 运行期活性探测。**用户复测仍失败** → 说明上述都不是根因（或只是加剧因素）。

**下一步接手者应做的判别实验（按优先级）**：
1. **同网络对照**（最关键）：在用户同一 WiFi/移动网络下用**桌面 CLI**（`./warp -reg` + 启动 + `curl --socks5-hostname 127.0.0.1:40000 https://www.google.com`）或**官方 1.1.1.1 WARP 客户端**测同一网站。若官方/桌面也失败 → **WARP 服务本身在该 ISP/区域的出口被 QoS/封锁**，与本实现无关，问题降级为"文档说明"；若桌面成功而 Android 失败 → 问题在 TUN 栈/Android 侧，继续下面。
2. **新 debugdiag 分析**：拿 v0.5.27 复测包，对比 v0.5.26 包——①`network is unreachable` 是否消失（验证 udp4/6 修复生效）；②隧道死亡频率是否降低；③死亡的错误类型是否变化。
3. **UDP 封锁假设**：边缘端口 4443（QUIC）被运营商 UDP QoS/封锁会导致 QUIC 会话频繁被掐。验证：查注册信息 `EndpointPorts` 是否有 443 端口候选（`edgeAddrs` 已含多端口，`dial` 会逐个试）；若 443 可达而 4443 被掐，拨号顺序应优先 443。当前代码按注册端口顺序，未做"优先 QUIC 友好端口"策略。
4. **KeepAlive 频率**：quic.Config KeepAlivePeriod=10s / MaxIdleTimeout=60s。若运营商对高频小包 UDP 限速，KeepAlive 本身可能触发丢包→会话不稳。可试调大 KeepAlive 或关闭（官方 warp-svc 的行为未对比）。
5. **gVisor 栈与隧道交互**：sing-tun gVisor 的 TCP 状态机对"隧道瞬间 RST"的处理（`read tcp <境外IP>:443: connection reset by peer` 是从 gVisor conn 读到的——需确认这是 gVisor 收到了边缘 RST 还是 gVisor 自身因隧道关闭生成的）。可加 debugdiag 区分连接关闭方向（v0.5.27 的 `up:EOF down:err` 格式已有方向信息）。
6. **换边缘策略**：`probeInternationalEgress` 只在拨号时探测一次；若用户 ISP 对某些 WARP 边缘出口不稳，应把"国际出口失败"也纳入运行期边缘轮换（当前只有连接级重连，无边缘级轮换——`addrIdx` 只在拨号失败时前进）。

**放弃原因**：CI 全绿 ≠ 真机解决（9 轮教训）。无设备、无 v0.5.27 新 debug 包、用户已失去耐心。**代码修复保留**（v0.5.27 已发布，属净改进：修复了确定的双栈 socket 问题与死连接恢复延迟），只是未解决根本问题。

**关键文件**：`tunnel/masque.go`（拨号/重连/探测/健康判定）、`androidvpn/dns.go`（DNS 拦截）、`androidvpn/androidvpn.go`（TUN 回调 + debugdiag 记录）、`gui/androidbridge.go`（JNI 装配）。

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

## 6.8 发布流程纪律（v0.5.7 两次"未构建 Android"血泪教训）

**背景**：v0.5.7 发布时连续两轮 CI 失败（build-android cgo 编译、build-gui windows 图标、
release 同名 tag 冲突），但期间 Agent 均提前报告"Android 版已成功构建并发布"。以下纪律
防再犯。

### 6.8.1 判定"发布完成"的唯一标准：所有 CI job 全绿 + 产物可验证

- **"已触发/运行中" ≠ "成功"**。`gh run list` 显示 in_progress 时禁止报告完成。
- 完整检查链（报告完成前必须全部通过）：
  1. `gh run list --limit 3` → 找到本次发布的 run ID
  2. `gh run view <run_id>` → 所有 job 结论为 ✓，尤其 **build-android**（最慢、最易挂）、
     **build-gui (windows)**（icons 步骤）、**release**（同名 tag 冲突）
  3. `gh release view <tag>` → assets 齐全：`app-release.apk`（~18MB 非平凡）+ `app-release.aab`
     + 5 平台 CLI + 3 平台 GUI（含 windows .exe）
- `gh run watch` 的 exit code 不可靠（部分 job 失败仍可能返回 0）——**必须看 `gh run view` 的
  job 级结论**。

### 6.8.2 Android 代码改动后 push + tag 同时构建（一次并行跑完所有平台）

- **本地验证门是假的**：`GOOS=android CGO_ENABLED=0 go build ./...` **不编译**
  `androidbridge.go`（`//go:build android && cgo`）——JNI/cgo 错误只在 CI 的 NDK 构建暴露。
- **发布顺序（v0.5.14 起，经实践确认更合理）**：改动 Android 代码（`gui/androidbridge.go` /
  `*.java` / manifest / gradle / CI Android 步骤）后，**push main + 打 tag 一次性并行构建**，
  让 build-android 与其余平台在 tag 触发下同时跑完。理由：
  - main push 只触发 `docker-ghcr.yml`（不含 build-android；build-android 在
    `build-release.yml`，只有 tag 才触发）——先 dispatch 验证一轮、再打 tag 又跑一轮，
    Android 构建要跑两遍，纯浪费。
  - 若 build-android 翻车：按 §6.8.4 删 tag → 修 → 重建 tag 指向修复 commit，不影响
    已发布的其它产物（Release 挂旧产物时手动删重建即可）。
  - **必须满足的前置**：本地 CI grep 断言（§6.8.3）已跑通（新 JNI 双侧签名）+ `go build
    ./...`/`go vet ./...` 全绿 + 平台 build tag 交叉编译验证（见下）。
- 本地能做的 Android 验证有限：`go vet`（只查非 cgo 部分）、C preamble 可用 gcc 单独编译
  （无 jni.h 时跳过）、CI 的 grep 断言（只查符号存在，抓不到类型/可见性错误）。
- **平台 build tag 文件必须交叉编译验证**：`GOOS=windows/darwin CGO_ENABLED=0 go build ./<pkg>/`
  能抓到 linux 编译不暴露的问题（如 windows-only 文件的未用 import、darwin-only 语法错误）。
  v0.5.8 教训：`sysproxy/windows.go` 挪走 containsTarget 后残留未用的 `strings` import，
  linux 编译正常，CI 的 windows job 才报错。

### 6.8.3 cgo/JNI 与 Java 的固定坑（v0.5.7 两连败直接原因）

1. **Go 侧禁止直接调 JNIEnv 方法**（`(*env)->GetStringUTFChars` 等）——cgo 不支持 `->`
   运算符，`*[0]byte` 不是函数。必须把 JNIEnv 调用封装进 **C preamble helper**
   （如 `jstringToChars`/`releaseChars`），Go 侧只调 `C.xxx`。新增 JNI 导出时先看
   `androidbridge.go` 现有 helper 模式。
2. **Java native 方法被跨类调用时不能 private**（v0.5.7：`WarpVpnService` 调
   `MainActivity.nativeLogMessage` 需 package 可见）。native 方法与可见性无关（JNI 按导出名
   解析），跨类调用就放宽为 `static native`。
3. **新增 `//export Java_*` 后**：Java 侧同步声明 native 方法、CI grep 断言同步加签名级
   （含返回类型）检查——否则下一个会话会在同样位置翻车。

### 6.8.4 tag / Release 操作纪律

- **force-push 同 commit 的 tag 不触发 CI**（GitHub 认为无变化，报 "Everything up-to-date"）。
  要让 tag 触发新 CI：删旧 tag → 重建（指向新 commit）→ force-push；或直接 workflow_dispatch。
- **`gh release create` 遇 "tag already exists"**：Release 可能已被 CI 创建成功——先
  `gh release view <tag>` 确认产物，不要盲目删除重建。产物完整则直接验证，不折腾。
- **移动 tag 会留旧 Release**：tag 指向新 commit 后旧 Release 仍挂在旧产物上；要么删除重建
  （会再触发 CI），要么手动上传新产物。推荐：修完所有问题再一次性打 tag。
- 正确发布顺序：修 bug → push main + 打 tag 同时推 → **等 CI 全绿**（§6.8.1）→
  验证 Release 产物 → 报告完成。若 build-android 翻车：删 tag → 修 → 重建 tag 指向
  修复 commit（§6.8.2）。

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
10. **Kernel 三端复用**：`core.Kernel`（MasqueClient+route.Engine+注册）供 CLI/GUI/Android 共用；androidvpn 无 SOCKS 监听；Android 运行时文件在沙箱 `getFilesDir()`（D8 路径分支），桌面/Docker 统一收拢到 `<执行根>/config/` 子目录（见 ADR 12）
11. **Android 构建 CI-only**：本地无 SDK/NDK/JDK → 仅 CI 构建（`build-android` job），真机行为验收清单交付用户（无设备验证）
12. **运行时文件统一 `<运行目录>/config/` 子目录（v0.5.12）**：config.json / reg.json / rules.txt / geo 全部收拢进执行根下的 `config/`（自动创建），Docker 只需映射 `./warp-config:/data/config` 一个目录。根目录选定 `core.baseExecRoot`：可执行目录（可写）→ **当前工作目录**（可写；Docker WORKDIR 挂载卷——修复"自动生成注册文件无法保存"）→ 用户配置目录 → 兜底。Android 例外：`DataDir` 非空锚定沙箱根，不套 config/ 子目录（真机路径约定未变）。一次性旧布局迁移 `migrateLegacyConfig`（幂等、非破坏）

## 8. 已确认事实（勿重复调研）

- 上游 `6Kmfi6HP/warp-go` HEAD = `6d5ab6a`（2026-07-31）；`badafans/warp-go` HEAD = `ca2f0cc`
- 本地 Go 1.26.5（/usr/local/go1.26.5）；gh 认证为 `callacat`；本机 linux/arm64 无桌面
- `go.mod`: `module warp`，go 1.26.5，quic-go v0.61
- SOCKS5 CONNECT 分支在 `tunnel/masque.go`（`HandleSOCKS5`，CONNECT 处理 ~L786）；分流 seam 在建立 H3 CONNECT 之前
- 远端 `callacat/warp-go` main = `165d565`（2026-08-01）；tag `v0.4.0`（2026-08-01）；旧内容在 `archive/previous-poc`
- `rules/default-rules.txt` 已上线（首启下载 200）；`download_proxy` 默认 gh-proxy.org，GEO 经加速实测下载成功
- 本机 GUI 构建限制：GTK 4.6.9 < wails 需要的 4.10（GtkFileDialog）→ 走 CI
- 本地 Android 环境：**无 SDK/NDK/JDK/设备** → android 构建仅 CI（`build-android` job）；本地只跑 `GOOS=android go build ./...` + `go test ./androidvpn/... ./gui/...`
- **v0.5.1 修复（2026-08-01）**：`core.Options.DataDir`（`resolveWithDir` 分派，空值=默认执行目录锚定，桌面零回归）；`gui/datadir_{android,other}.go`；`gui` module 已 `go mod tidy`（补 sing-tun 等间接依赖）；前端引入 vitest（theme/useTheme 18 单测）；主题事件名 5 平台（`common:/windows:/linux:/android:/ios:ThemeChanged`），Android 由 MainActivity `emitTheme()` 发 `android:ThemeChanged`；`@wailsio/runtime` 的 `Events.On` 回调收到的是 `WailsEvent{name,data}` 对象（payload 在 `.data`，Android 为 JSON 字符串 `{"isDarkMode":bool}`）
- **v0.5.12 config/ 布局（2026-08-03）**：`baseExecRoot()` 根目录选定链（exe 可写 → cwd 可写 → UserConfigDir → 兜底）；`resolveExecPath` 统一 `<根>/config/`；`configDirWritable` 探测"能否在候选根下建 config/ 子目录"（适配 Docker 挂载父目录 root 属主）；Docker 冒烟验证 reg.json/config.json/rules.txt/geo 全落挂载
- **v0.5.12 Android 规则重载（2026-08-03）**：Android 分流引擎在 `androidRuntime.kernel`（非 `Server.kernel`），`Service.ReloadRules` 需 Android 分支路由到 `androidRuntime.kernel.ReloadRules()`（`core.Kernel` 新增该方法）；VPN 未运行时报"请先启动 VPN"。改 JNI 导出面时注意：`androidReloadRules` 是纯 Go 函数（非 `//export Java_*`），仅 `androidbridge.go` 内部使用
- `go.mod` 依赖：sing-tun v0.8.11 为 direct require（T1 已提升）；无 gomobile
- `androidvpn/` 已接线（不再是孤儿包）：`decision.go` 宿主可测（`//go:build android || linux`），TUN 栈 `androidvpn.go` 仅 `//go:build android`
- **v0.5.26 debugdiag 契约（2026-08-07）**：诊断"Android 无法外网"但 CONNECT 隧道全建立的 build-tag 门控遥测收集器。`androidvpn/debugdiag.go`（`-tags debugdiag` 启用）vs `androidvpn/debugdiag_stub.go`（no-op stub，**release 默认零痕迹**）同包按 tag 互斥编译。数据写 `<沙箱根>/debugdiag/`（Android `getFilesDir()/debugdiag`）：`tunnels.tsv`（每关闭 TCP 隧道一行 `time seq host upBytes downBytes firstByteMs lifeMs err`；`firstByteMs` = 会话开始到首下行字节毫秒数，`-1` = CONNECT 成功但无数据流回，关键诊断）；`udp.tsv`（每行 `time host kind bytes err`，`kind`=dns 端口53非拦截泄漏 / quic 端口443浏览器HTTP/3直连泄漏 / udp）；`tun0.tsv`（每 2s 采样，`time txBytes deltaTx rxBytes deltaRx`，区分"隧道建立但 payload 死"与"完全无流量"）。生命周期：VPN 启动 `androidvpn.DebugSetDir(root)`、停止/回滚 `androidvpn.DebugStop()`；Go 停止时反向 JNI 调 `MainActivity.exportDebugDiag()` 把 `debugdiag/` 打 zip 到 `MediaStore Downloads`（`warp-go-debugdiag-<timestamp>.zip`，API 29+），包 URI 打到 GUI 日志页。CI `.github/workflows/android-debugdiag.yml`（workflow_dispatch）`-tags production,android,with_gvisor,debugdiag` + assembleRelease → artifact `warp-android-debugdiag`（versionCode 依 ref，覆盖安装 v0.5.25+，`warp-release.p12` 签名不丢 reg.json）。版本 v0.5.26
- **v0.5.14 Android 自路由修复（2026-08-03）**：`VpnService.establish()` 全量路由后
  应用自身新 socket 也走 TUN（未 protect 时），而 TUN 在拨号成功后才被 sing-tun 读取
  → QUIC ClientHello 滞留 tun 里、所有边缘握手超时。修复：`tunnel.socketProtector`
  钩子（`dialAddr` 建 UDP socket 后调用）+ Android 桥注册 `WarpVpnService.protectSocket(fd)`
  （`VpnService.protect()`）。异步装配失败经 `kernelFailed(msg)` 通知 Java 自拆除
  （stopForeground+stopSelf+关 TUN）。`warpCtl` 缓存 WarpVpnService 类/方法 ID（在
  nativeStartVpn 主线程 GetObjectClass 缓存，避免 goroutine FindClass 错失 classloader）。
  拨号总超时改为可配置 `config.json` `dial_timeout_seconds`（默认 60s）。**改 JNI 前必读
  §6.8.3**（Go 侧不能直接调 JNIEnv 方法，必须走 C preamble helper）。
- **v0.5.17 Android `udpnat` panic + GUI 配置快照（2026-08-04）**：`androidvpn.Vpn.Start`
  构造 `tun.StackOptions` **必须设 `UDPTimeout`（5m）与 `ICMPTimeout`（10s）**——sing
  v0.8.0 的 `udpnat.New` 对 `timeout==0` 直接 `panic("invalid timeout")` 而非返回错误
  （经 `NewUDPForwarder` 触发；v0.5.16 强制 gVisor 后此路径必达，真机 SIGABRT）。取值
  对齐 sing-box `constant/timeout.go`。异步启动 `kernel.Start`/`vpn.Start` goroutine 带
  `recover` 兜底（库 panic 走 failStart 回滚，不崩溃）。**GUI 保存配置后切页看不到变更**：
  `SaveConfig` 与 `applyConfigReload` 都必须同步 `s.cfg` 快照（`Status().Config` →
  `GetConfig` 数据源），否则恒为启动时旧值直到重启；快照内路径锚定运行时目录、磁盘仍
  写相对路径。回归：`TestSaveConfigUpdatesSnapshot`。
- JNI 导出面（v0.5.9 起 5 个，v0.5.14 静态方法桥 2 个）：`gui/androidbridge.go` 的
  `Java_com_wails_app_WarpVpnService_nativeStartVpn/nativeStopVpn/nativeVpnRunning` +
  `Java_com_wails_app_MainActivity_nativeLogMessage/nativeSetTimeZone`；Java 侧
  `WarpVpnService.java`（前三个 + 静态方法 `protectSocket(int)`/`kernelFailed(String)`，
  经 `warpCtl` 从 Go 调用）+ `MainActivity.java`（后两个，`nativeLogMessage`
  为 package 可见——WarpVpnService 也调用）；CI 双侧 grep 断言保障符号一致。
  **nativeStartVpn 必须保持异步**（v0.5.9 ANR 教训）：Java 主线程同步装配 Kernel
  （拨号无限重试）会 5s ANR；只做轻量前置校验 + 返回 0"已受理"，装配进 goroutine，
  失败经 androidRuntime.lastErr 上报
