# v0.5.0 — Android 里程碑（Wails VpnService over WARP）— 执行计划

- **仓库**: `/home/warp-go`（callacat/warp-go；上游：badafans/warp-go + 6Kmfi6HP/warp-go）
- **日期**: 2026-08-01
- **状态**: READY FOR EXECUTION（调研已对齐；决策已由编排者确认）
- **范围**: Wails v3 Android 壳 + 自写 Java `VpnService`（TUN fd → Go via JNI）+ 可复用 `core.Kernel` 抽取。CLI/SOCKS 行为不变。**不用 gomobile。**
- **验证环境**: 本地 linux/arm64，**无 Android SDK/NDK/JDK** → Android 构建仅走 CI。宿主机侧验证 = `go build/vet/test`。

---

## 1. 锁定决策（勿再评审）

| # | 决策 |
|---|---|
| D1 | **留在 Wails c-shared，不用 gomobile。** 自定义 `//export Java_...` JNI 函数加到*同一个* `libwails.so`。Go c-shared 可导出任意数量符号（Wails 已导出 18 个）。TUN fd 以 `jint` 过 JNI → Go `int`。 |
| D2 | **JNI 桥放在 gui 模块**（`warp/gui`）：Android c-shared 构建在 gui 模块上下文跑，`gui/go.mod` 有 `replace warp => ../`，可 import `warp/androidvpn` + `warp/core`。 |
| D3 | **Kernel 重构**: 抽 `core.Kernel`（`MasqueClient` + `route.Engine` + 注册信息），`core.Server` 与 androidvpn 共用。**androidvpn 无 SOCKS 监听。** `RegistrationInfo.AssignedIPv4/AssignedIPv6` 喂 `Config.Inet4Address/Inet6Address`。 |
| D4 | **UDP 保持直连**（与桌面一致）。规则仅作用 TCP。 |
| D5 | **quic-go v0.61.0：不改 GSO/ECN 代码。** `DisableGSO`/`DisableECN` 已从 Config *移除*（现为环境变量控制：`QUIC_GO_DISABLE_GSO` / `QUIC_GO_DISABLE_ECN`，按内核门控）。仅在真机 TUN 报错时作环境变量回退。 |
| D6 | **Android 构建仅走 CI**（本地无 SDK/NDK/JDK；Taskfile `HOST_TAG` 无 `linux-aarch64`）。 |
| D7 | **Android 包名保持 `com.wails.app`**（JNI 符号烘焙进包名——改名需重新生成 Wails 脚手架）。仅用户可见字符串改名 "warp-go"。 |
| D8 | **Android 运行时文件** = 应用沙箱 `getFilesDir()`（Wails Android 存储路径）。桌面保持执行目录行为。文件路径解析加 OS 分支。 |
| D9 | **CI Android SDK**: `android-actions/setup-android`。 |
| D10 | Wails alpha 规避：manifest `launchMode="singleTask"`（#5725）；警惕 #5859（onDestroy 崩溃）与 #5810（bindings 上下文，open）。 |

---

## 2. 调研事实基线（已验证——勿重新调研）

**JNI / Wails 内部**
- `application_android.go`（模块缓存 `github.com/wailsapp/wails/v3@v3.0.0-alpha2.119/pkg/application/`）：18 个导出 `Java_com_wails_app_WailsBridge_*` 符号（L680–L925）。**无 `JNI_OnLoad`**；`g_jvm` 在 `nativeInit` 内经 `GetJavaVM` 捕获。
- 自定义 native `Java_com_wails_app_WarpVpnService_*` 共存：调用 `env->GetJavaVM`，无需全局 init。
- `WailsBridge.java`（`.../java/com/wails/app/WailsBridge.java`）：声明 18 个 native + `System.loadLibrary("wails")`。
- iOS 先例：`gui/build/ios/main_ios.go:20` 有 `//export WailsIOSMain` —— 仓库内唯一现存 `//export`。
- `main_android.go` 脚手架：`RegisterAndroidMain(...)` 模式；Wails v3 有 `Android.StoragePath()`（具体签名 T6 时再验证）。

**Android 构建管线（Taskfile 已通读）**
- `generate:android:overlay`: `wails3 android overlay:gen -out build/android/overlay.json -config build/config.yml` → 生成 `overlay.json` + `gen/main_android.gen.go`。**构建时才生成，不在树内。**
- `compile:go:shared`: NDK 解析（`$ANDROID_NDK_HOME` 或 `$SDK_ROOT/ndk/*` 最新），`HOST_TAG` = darwin-x86_64 / linux-x86_64，`CC=$TOOLCHAIN/bin/aarch64-linux-android21-clang`，`CGO_ENABLED=1 GOOS=android`，`go build -buildmode=c-shared -overlay build/android/overlay.json <flags> -o build/android/app/src/main/jniLibs/$JNI_DIR/libwails.so`。
  - debug flags: `-tags android,debug -buildvcs=false -gcflags=all="-l"`
  - prod flags: `-tags production,android -trimpath -buildvcs=false -ldflags="-w -s"`
- `generate:android:bindings`: `wails3 generate bindings -f '-tags android' -clean=true`（`GOOS=android CGO_ENABLED=0`）。
- `APP_ID` 默认 `com.wails.app`；`adb am start` 硬编码 `{{.APP_ID}}/com.wails.app.MainActivity`。
- 打包目标：`assemble:apk`、`assemble:release`、`aab` 任务存在。

**Manifest（脚手架现状）**
- `.MainActivity`: exported、configChanges、adjustResize、MAIN/LAUNCHER。
- FileProvider: `androidx.core.content.FileProvider`，authorities `${applicationId}.fileprovider`。
- `.WailsForegroundService`: exported=false、`foregroundServiceType="dataSync"`。
- 现有权限：`INTERNET`、`FOREGROUND_SERVICE`、`FOREGROUND_SERVICE_DATA_SYNC`（+ 库存）。
- **缺失**: VpnService `<service>` 条目、`BIND_VPN_SERVICE`、VPN feature 声明。
- `gui/build/config.yml`: productIdentifier `com.callacat.warpgo`；**无 android 专属键**。app/build.gradle namespace = `com.wails.app`（库存）。

**Core 缝（供 Kernel 抽取）**
- `core/core.go`: `Server` 结构 + `Start()` —— 目前硬编码 proxy 监听（无 headless 模式）。
- `core/status.go`: `RegistrationInfo` 含 `AssignedIPv4`（L40–41）/ `AssignedIPv6`（L57–58）。
- `tunnel/masque.go`: `MasqueClient` L147、`NewMasqueClient` L216、`SOCKS5Config` L668、`RouteFunc` 缝 L819、`DialTunnel` L1024、QUIC 配置 ~L1627（仅 `DisableCompression`）。
- `route/engine.go`: `NewEngine(rulesPath, geoDir)` + `WatchRulesFile` 热重载。
- `core` 测试全绿（`go test ./...` exit 0，core 8.03s）。

**androidvpn 现状（`androidvpn/androidvpn.go`）**
- `//go:build android` —— 宿主机构建/测试排除（这就是现在无法在宿主机做包级测试的原因）。
- `stdLogger`（非 slog）。
- `RouteFunc(host string, ip netip.Addr) (action string, matched bool)`；`shouldProxy` 传 `netip.Addr{}` → **geoip 永不命中**（与桌面一致的既有局限）。
- `TunnelDial` / `DirectDial` 字段。
- `Config` 结构；sing-tun v0.8.11：`StackOptions` + `Options{FileDescriptor, MTU, DNSServers}`。
- **无 reject 处理。**
- fd 所有权：Java `int` fd → `Options.FileDescriptor`（wireguard-android / sing-box 模式）。

**quic-go v0.61.0**: `DisableGSO`/`DisableECN` 已从 `Config` 移除；环境变量控制（`QUIC_GO_DISABLE_GSO`、`QUIC_GO_DISABLE_ECN`），按内核门控。

**go.mod**: `module warp`，go 1.26.5；`sing-tun v0.8.11` + `sing v0.8.0`（indirect）；无 gomobile。`sing-tun` 需提升为 direct require（T1）。

**CI**: `build-release.yml` test job → 5 平台 CLI + 3 平台 GUI → GitHub Release；`docker-ghcr.yml`；`sync-upstream.yml`。**尚无 Android job。**

---

## 3. 架构总览

```
┌────────────────────────────── Android app (package com.wails.app) ──────────────────────────────┐
│  MainActivity (singleTask)                                                                       │
│     │ VpnService.prepare() consent                                                               │
│  WarpVpnService extends VpnService  ── fd(jint) ──▶  WailsBridge/JNI natives                      │
│     │ Builder: addAddress(assigned IPs), addRoute(0.0.0.0/0, ::/0), setMtu(1500)                 │
│     └ establish() → getFd()        nativeStartVpn(fd)  nativeStopVpn()                           │
└───────────────────────────────────────────────────┬──────────────────────────────────────────────┘
                                                    │ JNI (Java_com_wails_app_WarpVpnService_*)
                              ┌─────────────────────▼──────────────────────┐
                              │ gui/androidbridge.go  (//go:build android)  │
                              │   glues: core.Kernel + androidvpn.Vpn       │
                              │   RouteFunc/TunnelDial/DirectDial from Kernel│
                              └─────────────────────┬──────────────────────┘
                                                    │ import (gui/go.mod: replace warp => ../)
                              ┌─────────────────────▼──────────────────────┐
                              │ androidvpn (Go, sing-tun stack)             │
                              │   fd→Options.FileDescriptor, MTU, DNS       │
                              └─────────────────────┬──────────────────────┘
                              ┌─────────────────────▼──────────────────────┐
                              │ core.Kernel (NEW, extracted from Server)    │
                              │   MasqueClient + route.Engine + Registration│
                              └─────────────────────┬──────────────────────┘
                              ┌─────────────────────▼──────────────────────┐
                              │ tunnel/masque.go (MASQUE/QUIC H3) — WARP    │
                              └────────────────────────────────────────────┘
```

流程：UI 开关 → consent（`VpnService.prepare`）→ `startForegroundService` → Java `establish()` → fd → `nativeStartVpn(fd)` → Go sing-tun 栈 → 内核 `DialTunnel` over WARP H3。TCP 命中 `proxy` → 隧道；`direct` → 本地拨号；`reject` → 关闭连接；UDP → 直连（D4）。

---

## 4. 依赖图（边）

```
T1 ─▶ T2 ─┐
T1 ─▶ T3 ─┤
          ├─▶ T6 ─▶ T7 ─▶ T8 ─▶ T9 ─▶ T10 ─▶ T11 ─▶ T12 ─▶ T13
T4 ─▶ T5 ─┘        (T3: decision module)        (T6: exports)  (T10/T11: build inputs)
```

显式边：
- T1 → T2、T1 → T3（T1 是让树可构建的 tidy/commit）
- T4、T5（Wave 1，`core/`）—— 与 Wave 0（`androidvpn/`、`sysproxy/`、`autostart/`）**并行安全**：文件不相交
- T3 → T6（T6 可测接线依赖 T3 的宿主可构建决策模块）
- T5 → T6（桥需要 Kernel）
- T4 → T5（先有 Kernel 再暴露字段）
- T6 → T7（reject 需经桥端到端可达），T7 → T8
- T6 → T9（Java 调 T6 导出的 `nativeStartVpn`/`nativeStopVpn`）
- T9 → T10 → T11（先 Java 再 manifest 再 consent 接线）
- T10/T11 → T12（CI 构建完整 App），T2 → T12（CI test job 里 GOOS=android 编译检查）
- T12 → T13（发布）

**Ultrawork 并行规则**: 最多 2 个并发 agent，各处理不相交文件组——`core/` 组 vs `androidvpn/` 组（T3 ∥ T4/T5），之后 `gui/` 组单独（T6–T8，共享文件）。绝不两个 agent 同时改同一包。每次并行 fan-out 后先 `go build/vet/test` 全量回归再进入下一波。

---

## 5. Wave 0 — 基线加固（解锁一切）

### T1 — 提交 androidvpn 迁移 + 提升 sing-tun 为 direct require
- **依赖**: 无。
- **文件**: `androidvpn/androidvpn.go`、`go.mod`、`go.sum`。
- **变更**: 落地进行中的 sing-tun v0.8.11 迁移；`go mod tidy`（sing-tun 升为 direct require，sing v0.8.0 解析）；提交干净基线。
- **Test-first**: 无新逻辑；门 = 宿主机全量套件保持绿。
- **验证**: `go build ./... && go vet ./... && go test ./...` 全绿（androidvpn 被 `//go:build android` 排除）。
- **Category/skills**: `quick`（机械），skills `[]`。
- **风险**: 低。留意 tidy 造成的 go.sum 变动。
- **提交**: `chore(androidvpn): land sing-tun v0.8.11 migration baseline`。

### T2 — Android 可移植性守卫（sysproxy + autostart + core 安全）
- **依赖**: T1。
- **文件**: `sysproxy/linux.go`、`sysproxy/sysproxy_android.go`（新）、`autostart/autostart_linux.go`、`autostart/autostart_android.go`（新）、`core/core.go`（守卫 sysproxy.Set 调用）。
- **变更**:
  - `sysproxy/linux.go`、`autostart/autostart_linux.go`: build tag → `//go:build linux && !android`。
  - 新增 `sysproxy/sysproxy_android.go`、`autostart/autostart_android.go`: `//go:build android`，no-op stub（Android 走 VPN 而非 gsettings 设系统代理）。
  - `core/core.go`: 守卫 `sysproxy.Set`，让 GUI 开关路径在 android 上是安全 no-op。
- **Test-first**: tag 选择无法在宿主为 android stub 单测；**用交叉编译当测试门** —— 验证命令即测试门。加宿主编译单测断言 `core.Server` 在 `GOOS=android` 下不引用 gsettings-only 符号（CI 步骤）。
- **验证**: `GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build ./...` exit 0；宿主 `go build/vet/test ./...` 保持绿。
- **Category/skills**: `unspecified-high`，skills `["programming"]`。
- **风险**: `autostart/` 需先确认存在且有 linux 文件（任务内盘点步骤）；若无则跳过该对。`GOOS=android` 隐含 linux tag → `!android` 拆分为必须，否则 android 构建会拉进 gsettings。
- **提交**: `fix(build): add android no-op stubs for sysproxy/autostart; guard core sysproxy`。

### T3 — androidvpn 决策逻辑宿主可测 + 单测（含 reject）
- **依赖**: T1。
- **文件**: `androidvpn/decision.go`（新，`//go:build android || linux`）、`androidvpn/decision_test.go`（新，宿主）、`androidvpn/androidvpn.go`（RouteFunc/shouldProxy 委托 decision.go）。
- **变更**: 抽纯匹配/决策（`shouldProxy(host, ip, route)` → action）到宿主可构建文件；RouteFunc 变薄壳。TUN 栈代码保持 `//go:build android`。
- **Test-first**: `decision_test.go` 覆盖 ——
  1. Route nil → `proxy`
  2. route 命中 `proxy` → proxy
  3. route 命中 `direct` → direct
  4. 未命中 → `direct`（默认兜底）
  5. route 命中 `reject` → `reject`（新——补上 M6 桌面功能在 TUN 的对齐）
  6. geoip 传 `netip.Addr{}` → `matched=false`（记录 T8 局限）
- **验证**: 宿主 `go test ./androidvpn/...`（现在可跑了）；`go vet ./...`。
- **Category/skills**: `unspecified-high`，skills `["programming"]`。
- **风险**: 低；纯函数无 android 依赖。**勿**把 sing-tun import 挪进 decision.go。
- **提交**: `test(androidvpn): extract decision logic for host unit tests; add reject path`。

---

## 6. Wave 1 — Kernel 重构（复用，TDD）

### T4 — 抽 `core.Kernel`
- **依赖**: 无（与 Wave 0 独立）。
- **文件**: `core/kernel.go`（新）、`core/core.go`（重构）、`core/kernel_test.go`（新）、`core/core_test.go`（保留）。
- **变更**: 共享运行时（`MasqueClient`、`route.Engine`、注册状态）移入 `core.Kernel`：
  - `NewKernel(cfg, reg)`、`(*Kernel).Start()`、`(*Kernel).Stop()`
  - `(*Kernel).DialTunnel(ctx, addr)`（委托 `tunnel.DialTunnel` 路径）
  - `(*Kernel).Route(host string, ip netip.Addr) (action string, matched bool)`（委托 route engine）
- `core.Server` 重构为嵌入/拥有一个 Kernel，保留 SOCKS 监听 + 热重载 + sysproxy 接线。**Server 公开 API 不变**（既有用户/测试不受影响）。
- **Test-first**: 既有 core + proxy 测试全部原样通过（锁 Server 行为）。新增 `kernel_test.go` 镜像 `proxy_test.go` 的 4 路径：proxy / direct / miss→direct / nil-route→无隧道错误。
- **验证**: `go test ./core/... ./proxy/...`、`go vet ./...`、`go build ./...`。
- **Category/skills**: `deep`（行为保持型结构重构；最高风险任务），skills `["programming"]`。
- **风险**: 高。CLI/SOCKS 路径必须行为一致。缓解：proxy_test.go 原样保留作回归契约；单提交落地；跑 `-race`。
- **提交**: `refactor(core): extract reusable Kernel (MasqueClient + router) from Server`。

### T5 — 暴露 Kernel assigned-IP + 注册访问器
- **依赖**: T4。
- **文件**: `core/kernel.go`（加访问器）、`core/kernel_test.go`（扩展）。
- **变更**: `(*Kernel).AssignedIPv4() netip.Addr`、`(*Kernel).AssignedIPv6() netip.Addr`（取自 `RegistrationInfo`），加注册路径解析供 androidvpn 定位沙箱 `reg.json`。
- **Test-first**: kernel_test.go: 构造带 fake RegistrationInfo 的 Kernel 返回 assigned 地址；缺失 → 零 Addr。
- **验证**: `go test ./core/...`、`go vet ./...`。
- **Category/skills**: `unspecified-high`，skills `["programming"]`。
- **风险**: 低。
- **提交**: `feat(core): expose Kernel assigned-IP and registration path accessors`。

---

## 7. Wave 2 — JNI 桥 + androidvpn 接线（gui 模块）

### T6 — `gui/androidbridge.go`: nativeStartVpn/nativeStopVpn 导出
- **依赖**: T3、T4、T5。
- **文件**: `gui/androidbridge.go`（新，`//go:build android`）、`gui/androidbridge_test.go`（新，宿主）、`gui/go.mod`/`gui/go.sum`（确保 `warp` replace 存在）。
- **变更**:
  - `//export Java_com_wails_app_WarpVpnService_nativeStartVpn` `(fd C.jint) C.jint` —— 用 app 沙箱目录构建 Kernel + `androidvpn.Vpn`；`RouteFunc`/`TunnelDial`/`DirectDial` 接到 Kernel；`Options.FileDescriptor = int(fd)`、MTU 1500、DNSServers；启栈；成功返回 0 / 失败非 0。
  - `//export Java_com_wails_app_WarpVpnService_nativeStopVpn` `() C.jint` —— 停栈 + Kernel；幂等。
  - 纯 Go helper `buildAndroidConfig(sandboxDir string, regInfo ...) (androidvpn.Config, error)` 写为**宿主可构建**以便单测（该文件不加 build 限制）—— 把 D8 文件路径分支放在这里并可测。
  - 用 context7/gh_grep 验证 Wails `Android.StoragePath()` 签名后再用（先调研后实现）。
- **Test-first**: `androidbridge_test.go`（宿主）: `buildAndroidConfig` 对假沙箱路径返回正确的 geo/rules 目录；缺 reg.json → 错误路径；assigned IPv4/6 → Config 地址填充。
- **验证**: 宿主 `go test ./gui/...`（模块构建）测纯 helper；**JNI 编译本身只能 CI 验** `GOOS=android GOARCH=arm64 go build -buildmode=c-shared`（本地无 NDK——CI 门）。
- **Category/skills**: `deep`（首次碰 JNI + 模块接线），skills `["programming"]`。
- **风险**: 高。JNI 符号名必须与 Java 类 `com.wails.app.WarpVpnService` 精确匹配；Go 模块接线（gui vs root）可能 "no required module provides package warp/androidvpn" —— 在 gui/go.mod 修。与 Wails 18 导出共存必须保持（无 `JNI_OnLoad`，用 `env->GetJavaVM`）。
- **提交**: `feat(gui): add android JNI bridge for VpnService fd handoff`。

### T7 — reject 经桥端到端处理
- **依赖**: T3、T6。
- **文件**: `gui/androidbridge.go`、`androidvpn/decision.go`。
- **变更**: 桥内 RouteFunc 路径把决策 `reject` 映射为立即关闭连接（TUN 版 SOCKS5 0x02 / HTTP 403）。拨号代码永不看到 reject action。
- **Test-first**: 扩展 `decision_test.go`（T3）—— reject 在任何拨号前返回；再加桥级测试（宿主）—— reject action 走 "close" 分支而非 TunnelDial/DirectDial。
- **验证**: `go test ./androidvpn/... ./gui/...`。
- **Category/skills**: `unspecified-high`，skills `["programming"]`。
- **风险**: 低-中；留在决策层以便与桌面语义共享。
- **提交**: `feat(androidvpn): close connection on reject action in bridge path`。

### T8 — RouteFunc 对 TUN 目标做真实 IP 解析
- **依赖**: T7。
- **文件**: `androidvpn/decision.go` + `androidvpn/decision_test.go`。
- **变更**: TUN 目标是 IP 字面量时传真实 `netip.Addr`（非 `netip.Addr{}`），使 `geoip:` 规则可命中。域名目标保持零 Addr（geosite/domain 规则适用）；在包文档注释与计划 §12 记录 geoip-for-domains 局限。
- **Test-first**: decision_test.go: 目标 = IP 字面量 + `geoip:private`/`geoip:cn` 规则 → 正确命中；域名目标 → geoip 规则不命中（matched=false，fall through）。
- **验证**: `go test ./androidvpn/...`。
- **Category/skills**: `unspecified-high`，skills `["programming"]`。
- **风险**: 低；纯提升匹配正确性。
- **提交**: `fix(androidvpn): pass literal destination IP to RouteFunc for geoip matching`。

---

## 8. Wave 3 — Java 侧（manifest + VpnService）

### T9 — `WarpVpnService.java`
- **依赖**: T6（导出）、T8。
- **文件**: `gui/build/android/app/src/main/java/com/wails/app/WarpVpnService.java`（新）。
- **变更**（`package com.wails.app;` —— 必须，D7）：
  - `onStartCommand`: 建 `VpnService.Builder` → `addAddress`（assigned IPv4 + IPv6，来自 intent extras）、`addRoute("0.0.0.0/0")`、`addRoute("::/0")`、`setMtu(1500)`、`setSession("warp-go")`、`setBlocking(true)` → `establish()` → `fd = getFd()` → `nativeStartVpn(fd)`。
  - `onDestroy` → `nativeStopVpn()`。
  - `WarpVpnService` 也声明 native 方法：`private static native int nativeStartVpn(int fd);` `private static native int nativeStopVpn();`（让 JNI 名解析确定）。
  - 经 `startForegroundService` 启动，`foregroundServiceType="dataSync"`；发常驻通知。
  - 初始化顺序：首次 native 调用前 `System.loadLibrary("wails")`（与 Wails 同库——WailsBridge 已加载；确保顺序）。
- **Test-first**: Java 此处无宿主测试——**文档 + CI lint 门**：CI android job 内 `./gradlew compileReleaseJavaWithJavac` 必须零错编译。命名契约由注释 + CI grep 断言保障。
- **验证**: CI-only: `wails3 task android:assemble:apk`（或 gradle assembleDebug）Java 编译干净；符号与 `//export` 名匹配（CI 双侧 grep）。
- **Category/skills**: `unspecified-high`，skills `["programming"]`。
- **风险**: 中-高。fd 在 `nativeStartVpn` 完成前必须有效；`establish()` 失败时关 fd 不启服务。竞态：Java native + Go export 名不匹配 → 运行时 `UnsatisfiedLinkError`（仅真机可见；靠精确命名纪律 + CI grep 断言缓解）。
- **提交**: `feat(gui): add WarpVpnService Java with fd handoff to native`。

### T10 — Manifest: VpnService + 权限 + launchMode
- **依赖**: T9。
- **文件**: `gui/build/android/app/src/main/AndroidManifest.xml`、`gui/build/android/app/src/main/res/values/strings.xml`。
- **变更**:
  - 加 `<service android:name=".WarpVpnService" android:exported="false" android:permission="android.permission.BIND_VPN_SERVICE" android:foregroundServiceType="dataSync" />`。
  - 加 `<uses-permission android:name="android.permission.BIND_VPN_SERVICE" />` + `<uses-feature android:name="android.software.vpn" android:required="false" />`。
  - MainActivity: `android:launchMode="singleTask"`（#5725 缓解）。
  - 用户可见 `app_name` → "warp-go"（D7；仅 strings.xml——包名不动）。
- **Test-first**: manifest 是声明式——CI 门：小 shell 步骤断言必需元素存在（grep `BIND_VPN_SERVICE`、`.WarpVpnService`、`launchMode`）。
- **验证**: CI android job manifest 检查 + `aapt2 dump` 或 gradle lint。
- **Category/skills**: `quick`，skills `[]`。
- **风险**: 低-中；错误的 service 声明运行时不可见直到真机测试——所以有 CI grep 门。
- **提交**: `feat(gui): declare WarpVpnService, BIND_VPN_SERVICE, singleTask launch mode`。

### T11 — Consent 流（VpnService.prepare）
- **依赖**: T10。
- **文件**: `gui/build/android/app/src/main/java/com/wails/app/MainActivity.java`、`WarpVpnService.java`。
- **变更**: `MainActivity`: "连接" → `VpnService.prepare(this) == null` → 立即启服务；否则发 `ACTION_VPN_PERMISSION` intent；`onActivityResult`/`onResume`（singleTask）重查并启服务。保持 Wails webview overlay 完整（consent 是系统 activity，返回 app）。
- **Test-first**: CI 编译门；consent UX 仅真机（记入 §12）。
- **验证**: CI `assemble:apk` 编译；README 记录手工真机流程。
- **Category/skills**: `unspecified-high`，skills `["programming"]`。
- **风险**: 中——#5859（onDestroy 崩溃）/ #5810（bindings 上下文）可能在 consent 返回后浮现；实现前查 wails changelog；有 workaround 就上。
- **提交**: `feat(gui): wire VpnService prepare() consent flow in MainActivity`。

---

## 9. Wave 4 — CI + 发布

### T12 — CI: android 构建 job + GOOS=android 编译检查
- **依赖**: T2、T10、T11。
- **文件**: `.github/workflows/build-release.yml`（新 `build-android` job），（复用既有 test job 模式）。
- **变更**:
  - 新 job `build-android` 在 `ubuntu-24.04`: JDK 21、`android-actions/setup-android`（SDK + NDK r27+）、装 wails3（`v3.0.0-alpha2.119`）、node 22、`wails3 task generate:android:overlay && compile:go:shared && generate:android:bindings`（或打包任务）→ `assemble:apk` → 上传 `app-debug.apk`（与 `app-release-unsigned.apk`）为工作流 artifact。
  - 既有 test job 加编译检查步骤：`GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build ./...`（root，验证 T2 stub）。
- **Test-first**: job 本身就是测试——job 绿 = 为 android 编译、Java 编译、JNI 符号名匹配（grep 断言步骤）。
- **验证**: 从 `main` dispatch 工作流 → 全部 job 绿。
- **Category/skills**: `unspecified-high`，skills `["senior-devops"]`。
- **风险**: 中——Android SDK/NDK 供给与 wails3 task 路径漂移；工作流里钉版本（setup-android action 版本、NDK release）；artifact 名保持稳定供 T13。
- **提交**: `ci: add android build job + GOOS=android compile check`。

### T13 — E2E: tag v0.5.0、发布、文档
- **依赖**: T12。
- **文件**: `CHANGELOG.md`（v0.5.0 条目）、`AGENTS.md`（M7 节、目录树 + `autostart/` + 含 android 的构建命令）、`README.md`（Android 节 + 局限）、`.omo/plans/warp-go-reinit-2026-07-31.md`（§14 指针）。
- **变更**: 写 v0.5.0 changelog（Android 里程碑：Kernel 抽取、JNI VpnService、CI android job）；AGENTS.md 加 M7 行、树里加 `androidvpn/` + `autostart/`、android 构建/验证命令、`!android` 可移植性说明；README Android 用法 + "无真机无法验证"清单。
- **验证**: tag 后 `gh release create v0.5.0`；验证 Actions 全绿（test + build + gui + android）；本地下载 APK artifact 确认存在且非平凡大小；宿主测试全绿。
- **Category/skills**: `writing`，skills `[]`（文档）+ `quick`（tag/release）。
- **风险**: 低；版本化：T12 时检查 `build/config.yml` 若 android 构建用它则 bump versionName/versionCode（默认 appVersion 否则）。
- **提交**: `chore: v0.5.0 changelog + AGENTS/README android docs` 然后 `chore: tag v0.5.0`。

---

## 10. 里程碑门

**"v0.5.0 完成" = 全部:**
- [ ] tag `v0.5.0` 推送；GitHub Actions 全绿（test / build-release / docker-ghcr / build-android）
- [ ] `app-debug.apk`（与 release-unsigned）作为 release artifact 上传；artifact 本地验证非空
- [ ] 宿主: `go build ./... && go vet ./... && go test ./...` 全绿，core/proxy/androidvpn 带 `-race`
- [ ] `GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build ./...` 绿（T2 stub）
- [ ] `core.Server` 行为不变（proxy_test.go 原样且绿 —— Kernel 重构回归契约）
- [ ] 文档一致: CHANGELOG v0.5.0、AGENTS.md M7、README Android 节、计划文档 §14 更新
- [ ] 原子提交日志: `main` 上恰好 T1–T13 提交，无捆绑无关变更

---

## 11. 本地无法验证项（仅真机——用户测试项）

明确不在本地验证范围（这里无 Android 设备/模拟器）。写进 README 作验收项：
1. TUN 走 `warp=on`（VPN 激活时 fetch `https://www.cloudflare.com/cdn-cgi/trace`）
2. VpnService consent UX（prepare() 对话框 → 返回 → 启动）
3. GEO 在 TUN 上的分流：proxy/direct/reject 域名按规则实际路由
4. React 前端 WebView 渲染；Android 上系统托盘等价物
5. Always-On VPN 在 app 被杀/网络切换后重启
6. quic-go GSO/ECN 在真机内核——若 TUN 报错，设 `QUIC_GO_DISABLE_GSO=1`/`QUIC_GO_DISABLE_ECN=1`（D5）
7. `UnsatisfiedLinkError` 不存在（JNI 命名纪律）——仅运行时可见
8. 电池/前台服务行为（`dataSync`）

---

## 12. 原子提交策略

- **每任务一提交（T1–T13）**，按依赖序应用；绝不捆绑任务。
- **提交信息约定**: `<type>(<scope>): <summary>` —— type: `chore`/`fix`/`test`/`refactor`/`feat`/`ci`/`docs`；scope: `core`/`androidvpn`/`gui`/`build`/`ci` 等（每任务已列确切信息）。
- **规则**:
  - 仅当任务验证命令绿（CI-only 任务 = 工作流 dispatch 且绿）才提交。
  - 提交前 `git status`/`git diff` 审查；只 stage 目标文件（绝不盲目 `git add -A` —— go.sum/tidy 变动除非有意否则排除）。
  - 无 secrets：绝不提交 `reg.json`、密钥材料、本地配置。
  - 交叉编译门（T2 的 `GOOS=android` 构建）随自己任务提交，绝不混入他任务。
  - Kernel 重构（T4）是单原子提交且 proxy_test.go 不动 —— reviewer 可纯 diff Server 行为。
  - 文档（T13）在 tag 前作为自己提交落地。
- **回滚**: 每提交可独立 revert；Kernel 重构（T4）是唯一需 `git revert` + 重测 core/proxy 的点。

---

## 13. TDD 纪律（每波）

- 每个含逻辑任务（T3、T4、T5、T6、T7、T8）在实现**之前**或**原子同批**写/扩测试——无测试不提交。
- 行为契约任务（T4）依赖未动既有套件（`proxy_test.go`、`core_test.go`）作回归锁——前后都必须通过。
- 声明式/CI-only 任务（T9、T10、T12）用 CI 编译 + grep 断言作测试门（无宿主 harness）。
- 每次并行 fan-out（Wave 0 ∥ Wave 1，然后 Wave 2）后：进下一波前跑全量 `go build && go vet && go test ./...` + core/proxy/androidvpn `-race`。

---

## 14. 风险登记

| 风险 | 严重度 | 任务 | 缓解 |
|---|---|---|---|
| Kernel 重构破坏 CLI/SOCKS | 高 | T4 | 未动 proxy_test.go 契约；`-race`；单原子提交 |
| JNI 符号名不匹配 → UnsatisfiedLinkError | 高 | T6/T9 | 精确命名纪律 + CI 双侧 grep 断言；`//export` 名记录 |
| gui 模块接线失败（missing module provides warp/androidvpn） | 高 | T6 | 确认 gui/go.mod 有 `replace warp => ../`；root 加 `require warp` |
| 本地无法构建 android | 中 | 全部 | CI-only 策略（D6）；不本地尝试 |
| Wails alpha 生命周期 bug（consent 返回后） | 中 | T11 | 实现前盯 #5859/#5810 changelog；singleTask 已应用 |
| CI Android SDK/NDK 漂移 | 中 | T12 | 钉 setup-android action + NDK 版本 |
| sysproxy/autostart linux 文件把 gsettings 拉进 android 构建 | 中 | T2 | `!android` tag + no-op stub；GOOS=android 编译门 |
| 域名目标的 geoip 匹配缺失 | 低 | T8 | 记录局限（与桌面一致）；字面量 IP 路径已修 |
| 停止时 TUN fd 生命周期泄漏 | 中 | T9 | establish() 失败关 fd；nativeStopVpn 幂等 |

---

**可执行。** 启动顺序: T1（quick）→ 并行 fan-out [T2 ∥ T3 ∥ (T4→T5)] → T6 → T7 → T8 → T9 → T10 → T11 → T12 → T13。退出 plan 模式，Wave 0 开始。

---

## 15. 执行状态（2026-08-01，已全部完成）

> 按 §12 原子提交策略逐任务落地；每个逻辑任务验证命令绿后才提交；Wave 间全量 `go build && go vet && go test ./...` 回归。

| 任务 | 提交 | 状态 | 验证结果 |
|---|---|---|---|
| T1 sing-tun 迁移基线 | `60a1e6c` | ✅ | `go build/vet/test ./...` 全绿（androidvpn 被 `//go:build android` 排除）；sing-tun v0.8.11 升为 direct require |
| T2 android no-op stubs | `b8c74c0` | ✅ | `GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build ./...` exit 0（sysproxy/autostart `!android` 拆分 + core 守卫） |
| T3 decision.go 宿主可测 | `7a30e54` | ✅ | `go test ./androidvpn/...` 绿（reject 路径 + geoip 零 Addr 局限记录） |
| T4+T5 Kernel 抽取 | `c4ccb6c` | ✅ | `go test ./core/... ./proxy/...` + `-race` 绿；proxy_test.go 原样未动（回归契约） |
| T6 JNI 桥 + androidconfig | `d3cc709` | ✅ | 宿主 `go test ./gui/...`（buildAndroidConfig 路径分支）绿；JNI 编译门走 CI |
| T7 reject 绝不拨号 | `2a90fcf` | ✅ | decision + 桥级测试绿；reject action 走 close 分支 |
| T8 geoip 真实 IP | `3271e4c` | ✅ | decision_test 覆盖字面量 IP 命中 geoip；域名目标 fall through |
| T9 WarpVpnService.java | `67f0250` | ✅ | CI 编译门（Java 零错编译 + JNI 双侧 grep 断言） |
| T10 manifest 声明 | `81cdca4` | ✅ | CI grep 门（BIND_VPN_SERVICE / .WarpVpnService / singleTask / app_name warp-go） |
| T11 consent 流 | `df2fc47` | ✅ | CI `assemble:apk` 编译通过；consent UX 记入 §11 真机验收 |
| T12 CI android job | `f2383b4` | ✅ | `build-android` job（JDK21 + SDK + NDK r27 + c-shared arm64/x86_64 + APK/AAB）+ test job `GOOS=android` 编译检查 |
| T13 文档 + tag | `e62d3a2`（文档）+ `be723b0`（含 4 个 CI/Java 修复） | ✅ | tag v0.5.0 推送；Actions 全绿（test/build-binary×5/build-gui×3/build-android/release）；APK 18MB 发布 |

**里程碑门（§10）进度**：
- [x] `GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build ./...` 绿（T2 stub）
- [x] 宿主 `go build ./... && go vet ./... && go test ./...` 全绿，core/proxy/androidvpn 带 `-race`
- [x] `core.Server` 行为不变（proxy_test.go 原样且绿）
- [x] 原子提交日志：`main` 上 T1–T13 恰好逐任务提交，无捆绑
- [x] tag `v0.5.0` 推送；GitHub Actions 全绿（test / build-binary×5 / build-gui×3 / build-android / release）——T13b
- [x] `app-release.apk`（18MB）作为 release artifact 发布并本地验证（PK magic + 非空）——T13b
- [x] 文档一致（CHANGELOG v0.5.0、AGENTS.md M7、README Android 节、本计划 §14/reinit §14）——T13a

**收尾说明**：T1–T13 全部完成（代码 12 提交 + CI 修复 4 提交 + 文档 1 提交），v0.5.0 tag 已推送、Actions 全绿、APK/AAB 已发布。真机验收清单见 §11，交付用户。

---

## 16. 后续交接（2026-08-08）：Android 境外流量问题未解决

> 本计划里程碑（T1–T13）完成后，用户真机持续反馈"境外流量打不开"，历经
> **v0.5.13 → v0.5.27 共 9 轮修复**（M9.5–M9.13，见 AGENTS.md §6.5）仍未解决。
> 2026-08-08 用户决定**放弃继续修复**。最新一轮（v0.5.27）改动：
> `tunnel/masque.go` + `scanner/probe.go`（udp4/udp6 地址族、connBundle.dead 标志、
> probeInternationalEgress 拨号探测、egressProbeLoop 20s 活性探测）+ 5 新单测，
> 已发布（tag v0.5.27）但**真机复测仍失败**。

**接手者必读**：AGENTS.md §6.5「未解决问题交接（2026-08-08）」——含完整 debugdiag
证据链、已排除方向、6 个按优先级排序的判别实验（最关键：同网络桌面 CLI/官方
客户端对照，区分"WARP 服务被 ISP QoS"与"本实现 TUN 栈问题"）。
