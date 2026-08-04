# Changelog

本项目所有值得记录的变更。格式基于 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [v0.5.16] - 2026-08-04

### 修复

- **Android VPN 仍无法启动（v0.5.15 修复 JNI 闪退后暴露的下一层，真机两层根因）**：
  真机 logcat（`logcat_recording_2026-08-04_07-36-26`）显示 `nativeStartVpn`
  已成功受理（v0.5.15 的 JNI 闪退已修复），但两秒后 `kernel assembly failed`
  → `fdsan: double-close` → SIGABRT。定位到**两个独立 bug**：
  1. **TUN 栈选型退化为 system 栈（`need one more IPv4 address in first
     prefix for system stack`）**：`androidvpn.Start` 调 `tun.NewStack("")`
     空串，sing-tun 按编译标志选栈——CI 的 Android 构建 `-tags
     production,android` **没带 `with_gvisor`** → `WithGVisor=false` → 落到
     `NewSystem`。`NewSystem` 要求 `Inet4Address[0]` 前缀含 **next 地址**
     （`HasNextAddress(prefix,1)`），而 WARP 只分配单个 IP，我们传
     `172.16.0.2/32`——`/32` 单地址无 next → 报错。`NewGVisor` 只取前缀首
     地址、不要求 next，且我们的 handler（`NewConnectionEx`）本就是 gVisor
     型。
     - **修复**：`NewStack("gvisor")` 显式指定 gVisor 栈（不再依赖空串的
       编译标志分支）；CI 的 `build-android` c-shared 两架构 + Android 兼容
       门全部加 `with_gvisor` tag；`github.com/sagernet/gvisor` 依赖确认可用
       （经 sing-tun 传递，indirect 即满足 with_gvisor 构建）。
  2. **fdsan: double-close of file descriptor 329 → SIGABRT**：栈创建失败 →
     `rollback` → Go `vpn.Stop()`→`tun.Close()` 关 fd，同时 `failStart` →
     `kernelFailed`→Java `closeNative()`→`pfd.close()` 再关**同一 fd** →
     Android 15 fdsan 检测 → SIGABRT。**Go 与 Java 同时持有同一 OS fd 的
     所有权**。
     - **修复（fd 所有权归 Go 单一持有）**：Java `establish()` 后
       `pfd.detachFd()` 把 OS fd 转移给 Go（此后 `pfd.close()` 只关
       ParcelFileDescriptor 壳、不关 fd）；Go 侧负责 fd 关闭——`tun.New`
       包装成功后 `NativeTun.Close()`，包装前/同步校验失败路径
       `nativeStartVpn` 内 `unix.Close(fd)`（`androidvpn.Vpn` 增 `fd` 字段 +
       `v.fd=0` 防双关 + Stop 兜底关）。
  - **验证**：`GOOS=android GOARCH=arm64 CGO_ENABLED=0 go build -tags
    with_gvisor ./androidvpn/` 通过（决定性——v0.5.15 该命令落 `NewSystem`
    会报错）；`go build/vet/test ./...` 全绿；CI `build-android` job 的 JNI
    双侧 grep 断言保持通过。真机行为（TUN `warp=on`）仍需装
    `app-release.apk` 验证。

## [v0.5.15] - 2026-08-04

### 修复

- **Android 点击启动闪退 + VPN 不开启 + 分流引擎未初始化（同一根因，真机 SIGABRT）**：
  `nativeStartVpn` 缓存 `WarpVpnService` 类引用时对 JNI 第二参数 `obj` 调了
  `GetObjectClass`。但 `nativeStartVpn` 是 **`static native` 方法**——JNI 传给
  Go 导出的第二参数 `obj` **本身就是 `jclass`（WarpVpnService 类对象）**，再
  取类得到的是 `java.lang.Class`。随后 `getProtectSocketMethod` 在
  `java.lang.Class` 上找 `protectSocket` → `NoSuchMethodError` → 主线程抛异常
  → `SIGABRT` 闪退。
  - **症状链**：`TUN established` 后 15ms 内崩溃 → 内核装配从未执行 →
    `androidRuntime.kernel` 恒为 nil → 规则页"分流引擎未初始化（请先启动
    VPN）"、VPN 永远不启动。修好闪退后装配路径走通，引擎随
    `NewKernelContext` 就绪。
  - **修复**：`clsRef := C.newGlobalRef(env, (C.jclass)(obj))`——直接用
    `obj`（已是类对象），删除错误的 `getObjectClass` helper。其余
    `nativeStopVpn`/`nativeVpnRunning`/`nativeLogMessage`/`nativeSetTimeZone`
    同为 static native 但未对 obj 取类，不受影响。
  - **验证**：真机 logcat 复现（`NoSuchMethodError: no static method
    "Ljava/lang/Class;.protectSocket(I)Z"` + SIGABRT 三次重复崩溃）；CI
    `build-android` job 的 JNI 双侧 grep 断言（protectSocket/kernelFailed
    方法签名）保持通过。

- **开启系统代理后 WebSocket（WebSSH 等）会话无法连接（根因修复）**：
  mixed 代理的 HTTP 转发路径（`handleHTTPForward` → `stripHopByHop`）把
  WebSocket 升级请求的 `Connection: Upgrade` 与 `Upgrade: websocket` 逐跳头
  一并剥掉，上游收到普通 GET 无法完成 101 握手 → webssh 会话建立失败。
  - **为什么 V2rayN 正常**：直接走 SOCKS5 CONNECT（原始字节隧道）不解析
    HTTP、头原样透传；系统代理走 HTTP forward 分支才剥头。行业同款坑：
    netbirdio/netbird #6190。
  - **修复（方案 1）**：`handleHTTPForward` 检测到 Upgrade 请求时保留
    `Connection/Upgrade`（及 `Sec-WebSocket-*`）原样透传，不强制
    `Connection: close`，握手成功后 `relay` 直接双向转发帧。
  - **加固（方案 2）**：`sysproxy` 三平台启用系统代理时把本机回环地址加入
    旁路（Linux gsettings `ignore-hosts` / Windows `ProxyOverride`
    `<local>;localhost` / Darwin networksetup `-setproxybypassdomains`），
    浏览器访问 `localhost`/本机 webssh 直连，天然绕开代理剥头。
  - 补集成测试：`TestHTTPForwardWebSocketUpgrade`（真实 mixed 监听器捕获
    上游原始请求字节，断言 WS 头保留 + 101 回传）、普通 GET 回归测试；
    `TestLinuxSetEnable` 增补 ignore-hosts 调用断言。

## [v0.5.14] - 2026-08-03

### 修复

- **Android 连接所有边缘地址失败（根因修复）**：`WarpVpnService.establish()`
  装了全量路由（`addRoute("0.0.0.0", 0)` + `addRoute("::", 0)`）后，**应用
  自身新创建的 socket 也走 TUN**——而 TUN 要等拨号成功后才被 sing-tun 读取，
  于是 QUIC ClientHello 滞留未处理的 tun 里，所有边缘握手 2s 超时 →
  "所有边缘地址均失败" → 30s 装配超时。桌面 CLI 无此问题（无 TUN 自路由），
  故真机"依然连接失败"而桌面正常。
  - **修复**：新增 `tunnel.socketProtector` 钩子（`dialAddr` 创建 UDP 拨号
    socket 后、发任何包前调用）；Android 桥注册
    `WarpVpnService.protectSocket(fd)`（`VpnService.protect()` 把拨号 socket
    豁免出 VPN 路由走物理网络）。DoH 复用同一 QUIC 连接，无需单独保护。
  - 补 `TestKernelNewContextCanceledSkipsDial` 相关路径不受影响；CI 新增
    `protectSocket`/`kernelFailed` 双侧 JNI 签名 grep 断言。
- **Android 启动失败后"无法停止内核"（通知残留）**：异步装配失败只改 Go
  状态，Java 的 `vpnPfd`/`nativeRunning` 保持 true、前台通知残留——用户点
  停止看似无响应。现 `failStart` 额外经新 JNI 桥 `kernelFailed(msg)` 通知
  Java 侧自拆除（`stopForeground` + `stopSelf` + 关 TUN fd），停止按钮随后
  总是幂等生效。
- **Android 拨号总超时不可配置**：`androidDialTimeout` 从固定 30s 改为
  **可配置**（`config.json` 的 `dial_timeout_seconds`，默认 60s；0/缺失 =
  默认）。弱网/慢速运营商下可调大，不必改代码。

## [v0.5.13] - 2026-08-03

### 变更

- **运行时文件统一收拢到运行目录下的 `config/` 子目录**（修复 Docker 版
  "自动生成注册文件无法保存"）：config.json / reg.json / rules.txt / geo 全部
  落到 `<运行目录>/config/`（自动创建），Docker 只需映射
  `./warp-config:/data/config` 一个目录即可持久化全部配置。
  - **根因**：旧逻辑把运行时文件锚定到"可执行文件目录"，Docker 中 exe 位于
    只读 `/usr/local/bin` → 回退到容器层 `~/.config/warp-go`，从不落到
    挂载卷 `/data`。现 `core.baseExecRoot` 改为：可执行目录（可写）→
    **当前工作目录**（可写；Docker WORKDIR `/data` 挂载卷）→ 用户配置目录。
  - **Android 不变**：`DataDir` 非空时仍锚定沙箱根 `getFilesDir()`，不套
    `config/` 子目录（真机路径约定未变）。
  - **一次性旧布局迁移**：`config/config.json` 不存在但旧执行根有散落文件时
    自动复制进 `config/`；幂等、非破坏（不删原文件）。
  - Docker compose 挂载从 `./warp-data:/data` 改为 `./warp-config:/data/config`；
    README/AGENTS 运行时文件约定同步更新（v3 → v4）。

### 修复

- **Android 规则页"重新加载"报"分流引擎未初始化"**：Android 上分流引擎挂在
  `androidRuntime.kernel`（VpnService 驱动的 `core.Kernel`），`core.Server.kernel`
  （SOCKS 内核）在 Android 永不初始化——原 `Service.ReloadRules` 走
  `Server.ReloadRules` → `s.kernel==nil` 报错且规则不生效（可能影响 VPN 网络）。
  现新增 `core.Kernel.ReloadRules()`，`Service.ReloadRules` 在 Android 路由到
  `androidRuntime.kernel.ReloadRules()`（VPN 未运行时报"请先启动 VPN"）；补
  `TestKernelReloadRules` 单测（改规则文件后 ReloadRules 生效）。

## [v0.5.12] - 2026-08-03

### 修复

- **Telegram 无法连接（默认规则未覆盖 TG 流量）**：默认规则模板与
  `rules/default-rules.txt` 新增 `proxy,geoip:telegram`——TG 流量（如
  `149.154.175.100`）此前落入隐式 direct 兜底，在封锁 TG 的网络下直连
  失败；现命中后走 WARP 隧道。geoip 类别大小写不敏感（`TELEGRAM` 库内
  大写，`Lookup` 用 `ToUpper` 归一化）。补 `TestMatchGeoIPTelegram` +
  `TestDefaultRulesParses`（7→8 条）+ `TestLoadGeoIP`（telegram 段）测试。
  > **注意**：已有 `rules.txt` 不会自动更新——需手动加一行或删除后重启
  > 让默认模板重新生成。
- **Android 检查更新"前往下载"在应用内 WebView 打开**：`<a target="_blank">`
  在 Android WebView 被应用内捕获，GitHub 下载页体验差/登录墙。新增
  `Service.OpenExternalBrowser`（桌面走 Wails `BrowserManager`，Android 走
  反向 JNI 桥 `MainActivity.openExternalBrowser` 用 `Intent.ACTION_VIEW`
  跳第三方浏览器）；前端"前往下载"改调它。

## [v0.5.11] - 2026-08-02

### 修复

- **Android 边缘不可达时无限重试刷日志、状态永远"连接中"**（真机，v0.5.10）：
  `NewMasqueClient` 初始拨号无限指数退避重试（3.2s→5s），移动网络下
  QUIC/UDP 被运营商封锁时永久重连，日志无限刷"边缘 X 不可达...3.2s 后
  重试"，状态停在"连接中"。现 Android 装配拨号加 30s 总超时
  （`androidDialTimeout`，`context.WithTimeout`）——超时后经
  `failStartCtx` 报"连接边缘超时（30 秒内未能连接 WARP 边缘，请检查网络
  后重试）"，用户可检查网络重试。
- **Android 点停止无效（装配中/运行中）**（真机，v0.5.10）：
  - **Go 侧**：装配进行中（`NewKernel` 拨号未完成）`nativeStopVpn` 只能
    `cancel()`，但 `NewMasqueClient` 拨号用内部 lifeCtx 不响应外部 ctx →
    无限重连停不掉。新增 `tunnel.NewMasqueClientContext(ctx, ...)` /
    `core.NewKernelContext(ctx, ...)`：初始拨号循环 select 监听外部 ctx，
    取消立即中止（桌面/CLI 的 `NewKernel` 传 background，行为不变）。
  - **Java 侧**：`WarpVpnService` 是 `startForegroundService` 启动的前台
    服务，`MainActivity.requestStopVpn` 用 `stopService` **无法停止前台
    服务**（Android 8+ 需先 `stopForeground`，否则 `onDestroy` 从不触发
    ——日志停在 "TUN established" 后无 onDestroy）。新增
    `WarpVpnService.stop(Context)`：`stopForeground(STOP_FOREGROUND_REMOVE)`
    + `stopSelf()`，`requestStopVpn` 改调它。
  - 补 `TestKernelNewContextCanceledSkipsDial` 回归单测（ctx 已取消时不
    调用拨号工厂）。

## [v0.5.10] - 2026-08-02

### 修复

- **Android 初始化成功后点启动报 `nativeStartVpn：边缘地址解析失败：-ip ""`
  （真机，v0.5.9）**：Android 桥调 `core.ResolveEdgeAddrs(cfg, "", "", reg)`，
  当 `cfg.EdgeAddr` 与 `optsEdgeIP` 均为空时落入 default 分支，把空串当
  显式端点解析 → "missing port in address"。现 `ResolveEdgeAddrs` 双空时
  回落 `"4"`（按注册信息 IPv4 端点展开端口列表，与 CLI 默认 `-ip 4` 一致）；
  补 2 个回归单测（双空回落 / 双空缺 IPv4 报错）。
- **Android 应用扫描 IP 后再次启动 GUI 卡死（ANR）、VPN 无互联网**（真机，
  v0.5.9）：`nativeStartVpn` 在 Java 主线程（`onStartCommand`）同步执行
  Kernel 装配——`NewMasqueClient` 初始拨号无限指数退避重试（每候选 2s
  超时），边缘不可达时永久阻塞主线程 → 5s 后系统 ANR"卡死"；TUN fd 已由
  `establish()` 建立但从未交给 Go 内核 → VPN 已激活但无流量。现：
  - `nativeStartVpn` 改为轻量前置校验 + 立即返回 0（"已受理"），Kernel
    装配/拨号全部移入 Go goroutine（`startVpnKernel`），主线程不再阻塞；
  - 新增装配取消信号：`nativeStartVpn` 前置创建 `context.WithCancel`，
    `nativeStopVpn` 在装配完成前到达即取消，装配各阶段检查 `ctx.Err()`
    中止并释放已建资源（防止"启动后立刻停止"仍运行）；
  - 新增 `Java_com_wails_app_WarpVpnService_nativeVpnRunning` JNI 导出，
    Java 重入守卫据此区分"内核真在运行"（幂等跳过）与"已受理但异步失败"
    （释放旧 TUN fd 重新建立）——否则失败后 `vpnPfd` 残留导致重试被拦；
  - `onStartCommand` 的 `vpnPfd`/`nativeRunning` 置位提前到
    `nativeStartVpn` 之前（异步返回后置位会漏掉 START_STICKY 重投/重入）；
  - `WarpVpnService.closeNative()` 抽取幂等 teardown，`stopNativeAndClose`
    复用。
  - CI `build-release.yml` JNI 双侧 grep 断言新增 `nativeVpnRunning` 签名级
    检查。

### 文档

- README 新增 **Docker 部署**章节（拉取/注册/启动/验证/注意事项）；
  `docker-compose.example.yml` 注释完善（首次注册 → 日常启动 → 配置文件
  进阶的完整切换说明）。

## [v0.5.9] - 2026-08-02

### 修复

- **Android 启动报错"VPN 建立失败：At least one address must be specified"**
  （真机，v0.5.8）：`WarpVpnService.onStartCommand` 从 intent extras 读
  `assigned_ipv4/6`，但 `MainActivity.startVpnService()` 从未传 extras →
  `VpnService.Builder` 无地址 → `establish()` 抛异常。现 WarpVpnService 在
  extras 缺失时从沙箱 `reg.json` 读 `assigned_ipv4/6` 兜底（`getFilesDir()`）。
- **Android 点击注销无确认框、无动作**（真机，v0.5.8）：`window.confirm` 在
  Android WebView 静默返回 false → `onDeregister` 直接 return。现改为自绘
  两段确认：首次点击"注销"进入确认态（按钮变"确认注销？再次点击执行" +
  提示文案），5 秒无操作自动取消；全平台一致，不依赖 WebView 原生 dialog。

## [v0.5.8] - 2026-08-02

### 修复

- **初始化完成但状态页仍显示"正在初始化"无法启动**（win/Android 真机，
  v0.5.7 引入）：StatusPage 对 `getStatus` 返回值二次 `fromStatus`——getStatus
  已返回 camelCase 归一化对象，二次归一化只认 snake_case 的 `init_done`/
  `is_android` → 恒 false。修复：页面直接用 getStatus 返回值（null 兜底），
  GeoPage/SettingsPage 同源修复（getGeo/getConfig 也去掉二次归一化）。
- **重开 GUI 又初始化、无限循环**（win/Android 真机）：`Service.defaultsInit`
  是内存标志，重启丢失；文件已就绪仍重新下载并反复打"初始化完成"。新增
  `core.Server.InitDone()`（基于 rules.txt + GEO 文件就绪），InitDefaults 与
  GetStatus 据此判断——文件在则直接完成，不重复下载。
- **Android 默认白天模式**（真机）：React mount 时 Wails bridge 未就绪，
  `System.IsDarkMode()` 返回 false → 首帧误用浅色，且 `android:ThemeChanged`
  事件早于 mount 发出被漏掉。`useTheme` mount 后延迟 300ms 重查一次。
- **Android 状态页仍显示系统代理**（真机）：双重归一化导致 `isAndroid` 恒
  false，卡片未隐藏。随第一项修复（页面直接用 getStatus 返回值）。
- **Windows 启动后也卡"正在初始化"**（win 真机，v0.5.7 引入）：同第一项
  （双重归一化）——上一版本文件都在，但 initDone 恒 false 禁用了启动按钮。
  已修复；同时 `SetSystemProxy` 自动启动内核的旁路不受影响。
- **其它软件关闭系统代理时 GUI 不跟随**（win 真机）：后端 `Status.SysProxyOn`
  原读内存标志，外部关闭后仍显示"开"。新增 `sysproxy.Enabled(addr)`（三平台
  读真实系统状态：Windows 注册表 / darwin networksetup / linux gsettings）+
  `core.sysProxyCurrentlyOn`；`Status()` 只在本程序设置过代理时读真实状态
  （避免每 2s 轮询外部命令开销）；前端 `proxyEnabled` 跟随轮询的 `sysProxyOn`
  自动变关。

### 变更

- **停止内核同步关闭系统代理**：`Server.shutdown()` 改为"系统代理当前指向
  本程序才清除"——外部软件已改走别的代理时不动它（避免误关其它软件设置）。
- **托盘退出同步关闭系统代理**：托盘"退出"在 `app.Quit()` 前等待内核
  shutdown（最多 2s，含清系统代理），避免进程终止残留代理。
- **关闭按钮最小化到托盘**（win/linux）：`RegisterHook(WindowClosing)` +
  `event.Cancel()` + `window.Hide()`，点关闭藏托盘而非退出；macOS 保持关闭
  即退出（系统惯例）。托盘"退出"改直接 `app.Quit()`（避免被 hook 拦）。

## [v0.5.7] - 2026-08-02

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
  明确报错"VPN 接管全部流量，无需系统代理"，前端隐藏系统代理卡片。
- **Android 注销失败**（真机）：`DeleteRegistration` API 注销失败只 log 警告
  不返回错误 → 用户不知 API 侧注册仍在。现 API 失败返回错误（本地仍删除）；
  GUI `Deregister` 用 `cachedDataDir()` 兜底，不依赖可能失败的 serverInstance。

### 新增

- **注册信息完整显示**（win/Android 真机）：`core.Status()` 在 `s.reg` 为 nil
  （GUI 打开、尚未启动）时从磁盘补读 reg.json 视图并缓存，注册卡片在未启动
  时也显示 id/账号/密钥类型/边缘地址端口/分配 IP 全部 9 字段；Register 后同步
  缓存、Deregister 后清空缓存。新增 `status_registration_test.go` 两个回归测试。
- **初始化完成门控**：`Service.InitDefaults` 完成后日志打出"✓ 初始化完成…
  现在可以启动内核"，`Status.InitDone` 置 true；前端启动按钮在初始化完成前
  禁用并提示"正在初始化（默认规则 / GEO 数据库下载中）"。
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
