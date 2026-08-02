# Changelog

本项目所有值得记录的变更。格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [v0.5.2] - 2026-08-01

### 修复

- **Android"启动内核失败"根治**：GUI"启动/停止"按钮此前走 `core.Server.Start`
  （SOCKS 代理路径，Android 上无意义——隧道必须经 VpnService/TUN）。新增反向
  JNI 桥（`MainActivity.requestStartVpn/requestStopVpn` + `nativeBridgeReady`
  缓存全局引用与方法 ID，C helper 封装 JNI 调用镜像 Wails 模式），Start/Stop/
  IsRunning 在 Android 上桥接 VpnService 生命周期；GetStatus 用 androidRuntime
  状态覆盖生命周期字段。一键注册 → 启动 → consent → TUN 隧道全链路打通。
- **Android 日志"无日志"**：`log.SetOutput` 移到包 `init()`（c-shared 库加载
  时同步执行），早于任何 JNI 调用——`nativeStartVpn` 先于 `main()` 触发时
  内核日志也进 GUI 日志页（此前只进 logcat）。
- **Android 注册状态切页丢失**：`dataDir()` 首次成功取值后缓存（Wails
  StoragePath 桥接抖动会瞬时返回 `""`，`serverInstance` 失败 → `GetStatus`
  误报未注册）；失败路径用缓存沙箱目录兜底检查 reg.json。
- **扫描 v4/v6 无候选**：`scanFallback()` 在注册信息缺边缘地址时返回清晰错误
  （修复前 `net.JoinHostPort("","443")` 生成 `":443"` 垃圾候选）；注册端点
  提取回退 `endpoint.host`（API 可能只返回 host）。
- **状态栏被 UI 覆盖**：`MainActivity.onCreate` 显式
  `WindowCompat.setDecorFitsSystemWindows(getWindow(), true)`（Android 15+
  默认强制 edge-to-edge，此前 WebView 绘制到状态栏下方）。
- **Android cgo 构建失败**：`C.jclass`/`C.jmethodID` 不能与 untyped nil 直接
  比较、`*int` 不能传给 `*C.int` 参数——改用 `unsafe.Pointer` 比较 +
  `var needsDetach C.int`（CI 的 grep 断言只查符号存在，抓不到类型错误）。
- **JNI 签名不一致**：`MainActivity.nativeBridgeReady` Java 声明 `void` 而 Go
  返回 `C.jint`（未定义行为，错误码丢失）——改为 `int` 对齐，CI 断言升级为
  签名级检查。
- **Android 启动失败状态撒谎**：kernel/vpn 异步启动失败只写 `lastErr`，
  `started` 保持 true → GUI 显示"运行中"但隧道是死的，且无法再次启动。现
  失败即回滚（cancel + 拆除双方 + 置 started=false，校验仍是本实例防覆盖
  新状态）；启动成功清空 `lastErr`（旧错误不残留）。

### 新增

- **手机底部导航**：`<md` 隐藏侧边栏，新增固定底部导航（6 页 + 主题循环格，
  safe-area 底部内边距）；`NAV/TITLES` 抽到 `lib/nav.ts`（+7 vitest 单测）。
- **扫描空结果可操作提示**：琥珀色警告说明可能原因（QUIC 受限/缺边缘地址）
  并引导重新注册。

### 变更

- 前台服务通知渠道名 "Background work" → "warp-go VPN"。
- CI `build-android` 增加 JNI 符号 Java↔Go 双侧 grep 断言。

## [Unreleased] — v0.5.1 计划中

### 修复

- **Android 运行时文件统一锚定到应用沙箱**：GUI 服务层（`service.go` →
  `core.New`）此前把所有相对运行时路径（config.json / reg.json / rules.txt /
  geo）经 `resolveExecPath` 锚定到 `os.Executable()` 目录——Android 上是只读
  的 `/system/bin/app_process`，导致"生成默认配置 /system/bin/config.json
  失败：read-only file system"。新增 `core.Options.DataDir`（非空时所有相对
  路径锚定到该目录）+ `gui/datadir_{android,other}.go`（Android 返回
  `getFilesDir()`，桌面返回空串保持原行为）。GUI 服务层与 JNI 侧
  `buildAndroidConfig` 的沙箱路径就此对齐——注册写盘、内核启动、默认规则
  初始化三条链路打通。
- **日志时间戳去重**：标准库 `log` 默认 `Ldate|Ltime` 前缀经 `logWriter`
  原样进入环形缓冲，前端日志页出现"04:55:44 error 2026/08/01 04:55:44 ⚠…"
  双时间戳。`initLogging()` 加 `log.SetFlags(0)`，只保留环形缓冲按系统时间
  生成的 `HH:MM:SS`。
- **注销按钮无反馈**：注销/注册成功提示原本渲染在"注册信息"卡片内，注销后
  `status.registration` 变 null 导致卡片连同提示一起消失。提示移到页面顶部
  持久显示，注销/注册成功后立即刷新状态。

### 新增

- **跟随系统主题（全平台）**：三态主题模式（浅色 / 深色 / 跟随系统）。
  新增 `useTheme` hook：经 Wails runtime `System.IsDarkMode()` 获取系统偏好，
  `Events.On` 监听 5 个平台主题变更事件（common/windows/linux/android/ios）
  自动切换，`matchMedia` 作浏览器回退；模式持久化到 localStorage。侧边栏
  按钮循环切换三态，设置页新增"外观"卡片分段选择。Android 侧复用既有
  `android:ThemeChanged` 事件（Java 无需改动）。

### 变更

- **竖屏侧边栏自适应**：侧边栏宽度由固定 `w-52` 改为 `w-16 md:w-52`——
  小于 `md`（768px）自动收成 64px 图标栏（标签/品牌文字隐藏），手机竖屏
  不再被 208px 侧边栏挤占，横屏与桌面不变。

## [v0.5.0] - 2026-08-01

### 新增

- **Android 版**（Wails v3 Android 壳 + 自写 Java `VpnService`）：GUI 模块新增
  Android 目标，`WarpVpnService.java` 通过 `VpnService.Builder`（addAddress/
  addRoute/setMtu/setBlocking）`establish()` 取 TUN fd 经 JNI 交给 Go 侧
  `nativeStartVpn(fd)`/`nativeStopVpn()`；`gui/androidbridge.go` 用
  `//export Java_com_wails_app_WarpVpnService_*` 导出 JNI 函数（与 Wails 自带
  18 个导出共存于同一 `libwails.so`）；`MainActivity` 接 `VpnService.prepare()`
  consent 流（singleTask 规避 #5725）；manifest 声明 `WarpVpnService` +
  `BIND_VPN_SERVICE` + `android.software.vpn`；用户可见名 "warp-go"。
- **`core.Kernel` 抽取**：共享运行时（`MasqueClient` + `route.Engine` + 注册信息）
  从 `core.Server` 抽为可复用 `core.Kernel`（`NewKernel`/`Start`/`Stop`/
  `DialTunnel`/`Route`/`AssignedIPv4`/`AssignedIPv6`/`Close`），CLI/GUI/Android
  三端共用；`Server` 公开 API 不变，既有 proxy_test.go 原样作回归契约。
- **androidvpn 决策宿主可测**：`androidvpn/decision.go`（`//go:build android || linux`）
  抽出纯决策逻辑 `decideAction`/`resolveAction`，宿主单测覆盖 proxy/direct/
  未命中兜底/reject 五路径；reject 绝不进入拨号（与桌面 M6 语义对齐）。
- **CI `build-android` job**：ubuntu-24.04 + JDK 21 + `android-actions/setup-android`
  （SDK + NDK r27）+ wails3 alpha2.119 → c-shared（arm64/x86_64）+ gradle
  APK/AAB；既有 test job 加 `GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build ./...`
  编译检查。

### 修复

- **geoip 真实 IP 匹配**：TUN 目标是 IP 字面量时传真实 `netip.Addr` 给
  `RouteFunc`（此前恒为 `netip.Addr{}`，geoip 规则永不命中）；域名目标保持
  零 Addr（geosite/domain 规则适用，geoip-for-domains 局限记录在案）。
- **`-l` 日志回归修复**：`BuildTLSConfig` + `ResolveEdgeAddrs` 从 `Server.Start`
  抽出后日志顺序恢复。

### 变更

- 桌面三端复用同一 `core.Kernel`（CLI/GUI/Android），分流决策与注册信息
  解析逻辑单一来源。
- Android 运行时文件在应用沙箱 `getFilesDir()`（Wails Android 存储路径），
  桌面保持执行目录行为（`gui/androidconfig.go` 路径分支）。

## [Unreleased]

- 无

## [v0.4.0] - 2026-08-01

### 新增

- **REJECT 规则行为（广告拦截）**：规则支持 `reject,条件` 行为，命中即拒绝连接——
  SOCKS5 返回 `0x02`（connection not allowed by ruleset）、HTTP 返回 `403`，
  且不建立任何连接（隧道或直连）。状态页新增"拦截"统计卡片。
- **GitHub 下载加速前缀**：内置默认 `download_proxy = https://gh-proxy.org/`，
  对 `github.com` / `raw.githubusercontent.com` 的下载 URL（GEO 数据库、默认规则）
  自动拼接加速前缀，解决部分网络直连 GitHub 失败的问题；可在 GUI 设置页
  自定义或置空关闭；非 GitHub 地址（镜像仓库）不加速。
- **默认规则托管 GitHub**：首次启动优先从仓库 `rules/default-rules.txt` 下载
  默认规则（失败回退内置模板）；模板新增广告拦截：
  ```
  REJECT,geosite:category-ads-all
  direct,geosite:private
  direct,geoip:private
  proxy,geosite:google
  proxy,geosite:geolocation-!cn
  direct,geosite:cn
  direct,geoip:cn
  ```
- **开启系统代理自动启动内核**：GUI 中开启系统代理且内核未运行时，自动异步
  启动 warp-go（未注册时明确报错不挂起）。

### 修复

- **GUI 首启死锁（根因）**：`InitDefaults` 持有互斥锁时调用 `serverInstance()`
  二次加锁（非重入），导致所有 Wails 服务调用（读配置/注册/读规则/读 GEO）
  永久阻塞——这是"程序启动不读 config.json/reg.json、一键注册无响应无日志"
  的统一根因。现已修复并加回归测试。
- **首启引导**：`-geo-update` 与 GUI 首启一次完成 config.json 生成、默认规则
  下载/回退、GEO 数据库下载。
- **流量统计恒为 0**：后端 `route.Stats` 字段无 JSON tag 序列化为大写键
  （`ProxyHits` 等），前端只读小写键导致真实模式计数恒 0；改为双向读取。
- **状态页扫描按钮错位**：移除"扫描最优边缘"按钮（扫描功能已独立成页），
  替换为"注销（-del）"动作。
- **侧边栏收起后展开按钮点不到**：收起态 `w-14` 底部栏内宽不足导致按钮溢出
  被主面板盖住；加宽并收起时收缩暗色按钮。
- **托盘退出无效**：退出时先关闭窗口再 `app.Quit()`（修复 Linux GTK 退出后
  主窗口残留）。
- **设置页按钮改名**："重新加载" → "重置配置"。

### 变更

- 规则行为/条件解析大小写不敏感（`domain:` 条件值除外，保持原样）。
- 配置热重载对 `download_proxy` 即时生效（GEO/规则下次下载使用新加速前缀）。

## [v0.3.1] - 2026-07-31

### 修复

- Windows 版多项问题：GUI 演示模式根因（bindings 路径 + 构建顺序）、启动黑框、
  侧边栏按钮难以点击。
- 扫描功能独立成页（ScanPage），状态页不再承担扫描入口。

### 新增

- GUI 扫描边缘按钮 + 开机自启（三平台）。
- 路径锚定到可执行目录 + GUI 自动注册引导 + 注册信息展示/注销。
- sync 工作流加冲突预检测（merge-tree）。

## [v0.2.0] - 2026-07-31

### 新增（首个功能完整版本）

- **GEO 分流引擎**（`route/`）：规则解析、rules.txt 模板与热重载、GEO 数据库
  下载（SHA-1 去重 + protobuf 校验 + 原子写）、匹配引擎（geosite/geoip/domain/
  private/lan）。
- **mixed HTTP+SOCKS5 代理**（`proxy/`）：首字节嗅探同端口服务、UDP ASSOCIATE
  中继（数据报直连不经隧道）。
- **系统代理**（`sysproxy/`）：Windows 注册表 / macOS networksetup / Linux gsettings。
- **config.json 热重载**：文件变更自动生效（mtime + 内容 hash 检测）。
- **core/ Server 生命周期抽取**：CLI 与 GUI 共用。
- **Wails v3 GUI**：React 19 前端五页（状态/规则/GEO/设置/日志）+ 系统托盘。
- **CI/CD 三个工作流**：sync-upstream（双上游自动合并）、build-release（5 平台
  CLI + 3 平台 GUI → GitHub Release）、docker-ghcr（多架构镜像 → GHCR）。

## [v0.1.1] - 2026-07-31

### 修复

- 初始版本的小问题修复。

## [v0.1.0] - 2026-07-28

### 新增

- 首个可用版本：MASQUE over QUIC/HTTP-3 隧道 + SOCKS5 前端，基于上游
  badafans/warp-go 与 6Kmfi6HP/warp-go 的 fork。
