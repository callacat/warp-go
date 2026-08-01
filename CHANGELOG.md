# Changelog

本项目所有值得记录的变更。格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

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
