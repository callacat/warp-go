# Changelog

本项目所有值得记录的变更。格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [Unreleased] — v0.5.6 计划中

### 修复

- **GUI 状态显示错误**（win/Android 真机，v0.5.5 起暴露）：`fromStatus` 读
  `o.State`（大写）但后端 JSON 是 `state`（小写）→ `running` 恒 false →
  启动后按钮不变"停止"、状态页显示"已停止"（端口实际在监听）。改为 `o.state`，
  `types.test.ts` 用真实后端 JSON 验证映射。
- **GEO 更新时间不显示**（win 真机）：`fromGeo` 读 `o.geositeUpdated`（camelCase）
  但后端是 `geosite_updated`（snake_case）→ 恒 undefined → "更新于 —"。改为
  snake_case 优先 + camelCase 兜底。
- **注册信息只显示设备 ID 和隧道类型**（win 真机）：StatusPage 注册卡片未渲染
  account/密钥类型/边缘端口字段。补全 9 字段；`types.test.ts` 断言全映射。
- **Android 无法开启系统代理**（真机）：`SetSystemProxy` 无 android 分支，走
  桌面逻辑等 SOCKS server running（Android 永不运行）→ 静默超时。现 Android
  明确报错"VPN 接管全部流量，无需系统代理"，前端隐藏系统代理卡片
  （`System.IsAndroid()`）。
- **Android 注销失败**（真机）：`DeleteRegistration` API 注销失败只 log 警告
  不返回错误 → 用户不知 API 侧注册仍在。现 API 失败返回错误（本地仍删除）；
  GUI `Deregister` 用 `cachedDataDir()` 兜底，不依赖可能失败的 serverInstance。

### 新增（本轮）

- **注册信息完整显示**（win/Android 真机）：`core.Status()` 在 `s.reg` 为 nil
  （GUI 打开、尚未启动）时从磁盘补读 reg.json 视图并缓存，注册卡片在未启动
  时也显示 id/账号/密钥类型/边缘地址端口/分配 IP 全部 9 字段；Register 后同步
  缓存、Deregister 后清空缓存。新增 `status_registration_test.go` 两个回归测试。
- **初始化完成门控**：`Service.InitDefaults` 完成后日志打出"✓ 初始化完成…
  现在可以启动内核"，`Status.InitDone` 置 true；前端启动按钮在初始化完成前
  禁用并提示"正在初始化（默认规则 / GEO 数据库下载中）"——默认配置、默认
  规则、GEO 数据库就绪前不再允许点启动。
- **Android 启动/停止/注销全程日志**（真机"点击启动无反映也无日志"）：Java
  侧 `WarpVpnService` 的 establish 失败、授权失效、内核启动失败、onDestroy/
  onRevoke 全部经 `nativeLogMessage` 转发进 GUI 日志页；`Service.Start/Stop`
  Android 分支成功路径也打日志。
- **Android 日志时间显示系统时间**（真机"日志不是系统时间"）：Go 运行时内嵌
  `time/tzdata`（Android 无系统时区库），`nativeSetTimeZone` 的
  `time.LoadLocation` 不再失败。
- **Android 通知栏对比度修正**（真机"通知栏看不清"）：v0.5.5 误用
  `SYSTEM_UI_FLAG_LIGHT_STATUS_BAR`（深色图标）配深色底 → 深色图标看不清。
  移除该 flag，深底配默认浅色（白色）图标，始终可见。
- **应用图标**（全平台）：从 `appicon.png` 生成 Windows `.ico`（PE 资源 +
  versioninfo IconPath）、macOS `icons.icns`、Android `ic_launcher`/`ic_launcher_round`
  五个密度 mipmap（白底圆角 + 居中图标）。
- **浅色模式日志框**（全平台）：日志框由恒黑底改为浅色 `bg-slate-50` / 深色
  `dark:bg-slate-950`，浅色主题不再伤眼；debug 级别颜色调高对比。
- **长路径换行**（全平台）：状态页规则文件路径、GEO 页 geosite/geoip 数据库
  地址加 `break-all`，竖屏窄宽度不再溢出。
- **注册按钮 id 显示**（win 真机"注册成功（id=）"）：Wails 把 Go 多返回值
  `(existing, id, error)` 序列化为元组 `[boolean, string]`，`api.ts` 旧代码按
  对象读导致 id 恒空。现兼容元组/对象两种形态。

## [v0.5.5] - 2026-08-02

### 修复

- **GUI 点击"启动"卡死 + 其他页全部无法显示**（Windows/桌面，v0.5.1 起）：
  `Service.Start()` 持 `s.mu` 调用 `serverInstance()`（内部再次加锁，
  sync.Mutex 不可重入 → 自死锁），GUI 服务线程永久阻塞，所有服务调用
  （GetStatus/GetRules/GetGeo…）卡在锁上。修复：锁内先读状态，锁外调用
  `serverInstance()`；`service_test.go` 加 goroutine+超时回归测试。
- **Android 通知栏透明看不到**（真机）：Android 15+ 强制 edge-to-edge 下
  `setDecorFitsSystemWindows(true)` 只让内容不绘制到状态栏下，但
  statusBarColor 默认透明。显式 `setStatusBarColor` + 亮色图标，通知栏
  深底浅字始终可见。
- **Android 点击启动无反应且无日志**（真机）：`requestStartVpn` 在
  `sInstance==null`（Activity 销毁重建后）时静默 return；Go 侧桥未就绪
  只返回 error（前端按钮旁小字，手机易忽略）。两者改为明确 log（进
  GUI 日志页 + logcat）。
- **全平台侧边栏/底部导航更换主题按钮移除**：主题切换只在设置页保留。

### 新增

- **构建产物版本号**：版本号单一来源 = release tag（`v0.5.3` → `0.5.3`），经
  `-ldflags -X main.version` 注入：
  - CLI `-version` 打印版本（`warp v0.5.3`）；`go version -m` buildinfo 可见
  - Windows PE 版本资源（`goversioninfo` 生成 `.syso`，嵌入 FileVersion/
    ProductName，资源管理器"详细信息"可见，**降低报毒误报**——有版本/描述
    的可信二进制）
  - GUI 设置页"关于"卡片显示版本
  - Release 产物命名带版本（`warp-0.5.3-linux-amd64`、`warp-gui-0.5.3-*.exe`）
  - Android APK/AAB versionCode/versionName 由 CI 注入（versionCode 语义
    单调递增：0.5.3 → 503）
- **检查更新**：`core/updater.go` 查询 GitHub Releases API 最新版本，与当前
  版本比较（纯函数 `compareVersions` 可单测）。CLI `-check-update` flag +
  GUI 设置页"关于"卡片"检查更新"按钮（发现新版本显示 tag + 下载链接）。网络
  失败非致命（显示"检查失败"）。

## [v0.5.3] - 2026-08-02

### 修复

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
