# GUI/UI 栈调研报告 — warp-go（Wails v3 + React 19 + Tailwind）

> 调研人：gui-ui-reviewer（码农团队子代理）
> 日期：2026-08-15
> 范围：`/home/agent/workspace/warp-go/gui/`（Wails v3 桌面壳 + React 19 前端 + Android JNI 桥），与 `core/` 的交互边界
> 方法：逐一精读源码，结论均标注 `文件:行号`。未修改任何代码。
> 前置限制：本机 GTK 4.6 < 4.10 无法本地编译 Wails GUI → GUI 构建走 CI；前端 `npm run build` 可本地通过（依赖 api.ts 的 demo 兜底）。

---

## 1. GUI 架构概览

```
gui/
├── main.go            # Wails v3 入口：窗口(960x680) + 系统托盘 + Service 注册 + 关窗隐藏到托盘
├── service.go         # Service 结构体：22 个前端可调用方法，持有 core.Server 惰性实例
├── logs.go            # 日志环形缓冲 ringLogger（cap=500）+ logWriter 接管 log.Printf
├── androidbridge.go   # [android&&cgo] JNI 桥：VpnService TUN fd → Go Kernel；反向桥驱动 Java
├── androidconfig.go   # [android||linux] buildAndroidConfig：沙箱内装配 Android 启动配置（宿主可测）
├── datadir_android.go # [android] dataDir() = getFilesDir() 缓存
├── datadir_other.go   # [!android] dataDir() = ""（桌面锚定执行目录）
├── version.go         # ldflags 注入版本号
├── Taskfile.yml       # wails3 构建编排（分平台 include）
├── build/             # 三平台 + android/ios 构建任务、图标、plist、gradle 工程
└── frontend/          # React 19.2 + Vite 8 + Tailwind v4 + lucide-react（唯一运行时依赖）
    ├── src/App.tsx            # 壳：侧边栏 + 顶栏 + 内容区 + 底部导航(<md)
    ├── src/lib/api.ts         # API 层：动态加载 Wails bindings，无 bindings 时降级 demo 数据
    ├── src/lib/types.ts       # 前端类型 + from* 防御性归一化
    ├── src/lib/nav.ts         # 导航模型（6 页，纯数据可测）
    ├── src/lib/theme.ts       # 主题纯函数（localStorage + config.json 持久化）
    ├── src/lib/useTheme.ts    # 三态主题 hook（系统跟随 + 5 平台事件 + matchMedia 兜底）
    ├── src/lib/logsTail.ts    # 日志轮询去重纯函数
    ├── src/components/ui.tsx  # Card/Button/Toggle/StatusPill/Field/inputCls
    └── src/pages/             # StatusPage/RulesPage/GeoPage/ScanPage/SettingsPage/LogsPage
```

**技术栈**：
- Wails v3 `3.0.0-alpha2.119`（`gui/go.mod`，与 `@wailsio/runtime` 同版本钉住）
- React `^19.2.8`、Vite `^8.2.0`、Tailwind `^4.3.3`（`@tailwindcss/vite` 插件）、lucide-react（图标）
- 无 i18n 库、无全局状态库（zustand/redux/react-query 均未引入）、无组件库（自绘 ui.tsx）、无 CSS 框架以外的 UI 依赖
- 前端 `package.json`（`frontend/package.json:12-17`）：运行依赖仅 `@wailsio/runtime`、`lucide-react`、`react`、`react-dom` —— 非常轻

**导航模型**：任务描述"五页"，实际为**六页**：状态/规则/GEO/扫描/设置/日志（`lib/nav.ts:8` 的 `PageKey`）。桌面侧边栏 `md+` 显示、`<md` 底部导航固定（`App.tsx:93-109`，Android 竖屏适配）。

**日志传输方式**：**轮询，非 WebSocket/事件推送**。`core`/`service` 的所有 `log.Printf` 被 `logWriter`（`service.go:641-644`）重定向到 `ringLog`（`logs.go:41`，500 条环形缓冲），前端 `LogsPage` 每 1s `GetLogs(200)` 轮询（`LogsPage.tsx:22-43`）。`logs.go` 包 `init()` 即接管日志（`logs.go:19-22`），保证 Android JNI 调用早于 `newService()` 也能进 GUI 日志页。

**bindings 生成**：`frontend/bindings/` 被 `.gitignore` 排除，CI 构建时由 `build:frontend` 的 dep `generate:bindings` 生成（`gui/build/Taskfile.yml:72-100`，含 `run: once` 防 darwin universal 双构建竞态注释）。`api.ts` 运行时 `import("../../bindings/warp/gui/index.js")`，import 失败则降级 demo 模式（`api.ts:94-115`）。

---

## 2. 前端页面逐页评估

### 2.1 StatusPage（状态）— `frontend/src/pages/StatusPage.tsx`

**职责**：运行开关、注册/注销、系统代理开关、流量统计、注册信息展示。页面核心入口。

- **轮询**：`getStatus` 每 2s（`StatusPage.tsx:43` `usePoll`）、`getConfig` 每 5s（`:44`）。`usePoll` 是页面本地 hook（`:14-39`），实现 fetch+setInterval+卸载清理，**在其它页面未复用**（LogsPage/RulesPage 各自重写轮询 effect）。
- **注册**：一键注册（`:107-119`）；注销用**自绘两段确认**（`:121-143`）——因 Android WebView `window.confirm` 静默 false（注释 `:122`），5s 超时自动取消。这是跨平台一致的好的取舍。
- **系统代理**：`!status.isAndroid` 才渲染（`:333`）。开关跟随轮询的 `sysProxyOn`（`:72-76`），后端每 2s 读真实系统状态（`core/core.go:684-687`）→ 外部软件改掉系统代理 GUI 自动跟随（v0.5.8 修复）。
- **启动按钮**：`disabled={!status.running && !status.initDone}`（`:207`），初始化完成门控（v0.5.7 修复）。
- **流量统计卡**：proxy/direct/miss/reject 四色卡（`:304-331`）。
- **问题点**：
  - `usePoll` 本地定义，与其它页轮询逻辑重复（复用机会）。
  - `getConfigOnce`/`getSystemProxyOnce` 放在文件底部 import（`:355-370`），与顶部 import 分裂，可读性略差。
  - 双重归一化历史 bug（`fromStatus` 二次包装）已用注释反复强调（`:47-50`）——说明类型契约不牢固，靠"记住不要二次归一化"维持。

### 2.2 RulesPage（规则）— `frontend/src/pages/RulesPage.tsx`

**职责**：行号文本框编辑 rules.txt + 语法校验 + 保存/热重载（用户核心需求，plan M4）。

- **轮询**：每 2s `getRules`，但 `dirty` 时不刷新、不覆盖未保存编辑（`:15-35`）——良好的 UX 取舍。
- **语法校验**：Go 侧 `route.ParseRules`（`service.go:330-332`），前端只显示错误字符串。
- **规则计数**：`ruleCount` 只统计 `proxy,`/`direct,` 开头（`:38-46`）——**不统计 REJECT 行为行**（默认规则含 `REJECT,geosite:category-ads-all`），计数偏小，与"共 N 条规则"展示不一致。
- **行号**：`<md` 隐藏（`:94`），竖屏手机无行号但 textarea 保留。
- **问题点**：
  - 纯 textarea 编辑，**无 CodeMirror/Monaco**。plan 文档最初设想 CodeMirror（plan §2.1"规则编辑器（CodeMirror）"），实际是裸 textarea + 自绘行号列。语法高亮/行内错误定位缺失。
  - 行号是渲染 N 个 `<div>`（`:97-99`），大文件性能差（默认规则几百行可接受，但用户粘贴几千行会卡）。
  - "重新加载"按钮 = ReloadRules + getRules（`:63-78`）；与 2s 自动刷新功能重叠。

### 2.3 GeoPage（GEO）— `frontend/src/pages/GeoPage.tsx`

**职责**：只读展示 GEO 数据库状态 + "立即更新"按钮。**纯展示页，无编辑**（仓库/URL/周期编辑在 SettingsPage）。

- 展示 geosite.dat / geoip-lite.dat 路径与更新时间、仓库、自动更新周期、下载地址（`:60-108`）。
- `updateGeo` 后 `res.ok` 才 refresh（`:37`）。
- **问题点**：
  - `GetGeo` 返回的 `BaseURL` 是 service.go 硬编码的默认值（`service.go:395`），**不读 Config**；同时 `LastChecked` 是 `time.Now()`（`service.go:403`）——每次轮询都是"刚刚"，不是真实"上次检查时间"。信息真实性存疑。
  - 无"自动更新已开启/关闭"状态展示（`autoUpdateDays:0` 时 UI 仍显示"每 0 天"）。

### 2.4 ScanPage（扫描）— `frontend/src/pages/ScanPage.tsx`

**职责**：边缘延迟扫描 v4/v6 + 应用最优端点。这是上游 fork（scanner）的 GUI 化。

- 扫描结果分族保存（`ScanResult[]`，`:12-15`），点"应用"调 `ApplyEdge` 写 config.json（`:43-54`）。
- **问题点**：
  - **Android 上此页没有禁用/降级**：扫描需物理网络 QUIC 探测（`core/core.go:819-869`），Android 端 `service.ScanEdges` 走 `core.Server.ScanEdgesFamily`——但 Android 上 `Service` 的 serverInstance 沙箱路径可用，理论上能扫。未验证真机行为。
  - 扫描可能耗时 90s（`service.go:504`），前端无超时提示、无进度，只有 loading。

### 2.5 SettingsPage（设置）— `frontend/src/pages/SettingsPage.tsx`

**职责**：完整 config 表单 + 主题三态 + 关于/更新检查 + 开机自启。最重的页面。

- **config 表单**：listen/rulesPath/geoDir/geoRepo/geoBaseURL/autoUpdateDays/downloadProxy/logDir 共 8 个输入（`:178-240`）。
- **主题**：三态分段控件（`:156-173`），走 `useTheme().setMode`。
- **更新检查**：`CheckUpdate` + `openExternalBrowser`（`:58-77`，Android 跳第三方浏览器 v0.5.11 修复）。
- **开机自启**：`SetAutostart` Toggle（`:263-280`）。
- **问题点（重要契约不一致）**：
  - **`geoBaseURL` 是死字段**：前端 `AppConfig.geoBaseURL` 有输入框（`:216-222`），但 `api.ts saveConfig` 映射时**没有 `geo_base_url`**（`api.ts:404-414`），而 Go `Config` 结构体也**没有 `geo_base_url` 字段**（`core/config.go:23-50`，仅 `geo_repo`，下载 URL 由 `GeoSiteURL()` 推导）。用户在设置页改"GEO 下载 URL"保存后**静默丢失**。与 plan §6.2"仓库/URL 可在 GUI 编辑（自定义更新源）"不符——当前只有 repo 可配，URL 编辑是虚设。
  - 同理 `GeoPage` 展示的 `baseURL` 恒为硬编码默认（`service.go:395`），与用户实际配置的 repo 派生 URL 可能不一致。

### 2.6 LogsPage（日志）— `frontend/src/pages/LogsPage.tsx`

**职责**：日志查看器，1s 轮询 + 自动滚动 + 清空。

- 去重逻辑用 `logsTailChanged`（`logsTail.ts:10-19`）比较尾条 (time+level+msg)——修复"达到 200 条上限后长度不变页面冻结"经典 bug（v0.5.20）。
- 自动滚动开关（`:66`）+ 清空按钮（`:68`）。
- **问题点**：
  - **"清空"是纯前端 `setLogs([])`**（`:68`），Go 侧 ring 缓冲还在，下个 1s tick 立即恢复全部。用户点"清空"期望清除缓冲，实际只是视觉清空。要么加 Go 侧清空方法，要么去掉该按钮。
  - 标题"最多显示最近 200 条"（`:94`）但 Go ringCap=500、GetLogs limit 上限 1000（`service.go:567-572`）。前端请求 200 是合理节流，但文案与后端容量不一致。
  - 日志 level 由 Go 侧字符串包含匹配推断（`logs.go:44-54`，`contains "error"/"失败"/"无法"`），**不是结构化级别**——CLI 侧没有级别概念，GUI 页"级别着色"是启发式，误判难免（如"忽略错误"会标红）。

### 组件复用（ui.tsx）
- 提供 `Card`/`Button`(4 variant)/`Toggle`/`StatusPill`/`Field`/`inputCls`（`ui.tsx` 全文件）。设计统一（orange 主色、slate 灰阶、深色类 `dark:`）。
- **缺失的复用**：每个页面都重复"notice(绿)/error(红) 内联提示"模式（StatusPage.tsx:147-171, 233-236; RulesPage.tsx:126-129; GeoPage.tsx:114-120; ScanPage.tsx:73-89; SettingsPage.tsx:253-256）——没有统一 Toast/Feedback 组件，文案展示方式各异。busy/error/demo 三件套状态在 5 个页面重复定义。

---

## 3. 后端服务层评估（service.go / logs.go / main.go）

### 3.1 API 面
22 个方法（`service.go`）：GetStatus / Start / Stop / IsRunning / Register / Deregister / GetRules / SaveRules / ReloadRules / GetGeo / UpdateGeo / SetSystemProxy / GetSystemProxyEnabled / ScanEdges / ScanEdgesV4 / ScanEdgesV6 / ApplyEdge / SetAutostart / GetAutostartEnabled / GetConfig / SaveConfig / GetLogs / GetVersion / CheckUpdate / OpenExternalBrowser。

**优点**：
- 方法粒度合理，语义清晰，与前端 `api.ts` 的 `ServiceAPI` 接口（`api.ts:30-55`）一一对应。
- 错误处理：Go error → Wails 序列化 → 前端 catch 显示。`SaveRules` 先 `route.ParseRules` 校验再原子写（`service.go:330-336`），不写入半成品。
- 路径处理：`GetRules`/`SaveRules` 从 `Status().Config.RulesPath` 取路径（`service.go:307-310`），与 core 锚定一致。
- 原子写 `atomicWriteFile`（`service.go:616-636`）。

**问题点**：

1. **多返回值契约不稳定**（Wails v3 特性）：`Register() (existing bool, id string, err error)` 序列化为元组 `[boolean, string]`，前端 `api.ts:220-231` 被迫兼容数组/对象两种形态。这是 Wails 绑定层的摩擦点，未来升级 Wails 需回归。
2. **snake_case ↔ camelCase 双重归一化易错**：Go `core.Status`/`Config` 用 snake_case json tag（`status.go:13-29`），前端 `types.ts` 的 `from*` 归一化为 camelCase。曾出双重归一化 bug（v0.5.7），代码里靠注释反复提醒（`StatusPage.tsx:47-50`、`GeoPage.tsx:17-18`、`SettingsPage.tsx:40-41`）。根因是**前端类型系统与 Go DTO 未共享契约**，靠手工维护两份。
3. **`route.Stats` 无 json tag**（`route/matcher.go:25`）→ 序列化为 `ProxyHits`/`DirectHits`/`Misses`/`RejectedHits` 大写键，`fromCounters`（`types.ts:88-98`）被迫同时兼容 camelCase 与大写键。Go 侧类型设计缺陷，应补 json tag。
4. **`GetGeo` 数据真实性问题**：`BaseURL` 硬编码默认（`service.go:395`），`LastChecked=time.Now()`（`:403`），未读 Config 真实值（见 2.5 契约不一致）。
5. **Android 分叉侵入 service.go**：`runtime.GOOS == "android"` 分支散布在 serverInstance（`:68`）、GetStatus（`:149-171`）、Start/Stop/IsRunning（`:185-269`）、Deregister（`:284-288`）、ReloadRules（`:347-349`）、SetSystemProxy（`:439-444`）、OpenExternalBrowser（`:601-603`）。Service 同一份代码横跨桌面/Android 两种运行模型（SOCKS server vs VpnService），可读性与可测性受影响。
6. **`startErr` 竞态**：异步 `Start` 的 goroutine 在 `s.mu` 下写 `startErr`/`started`（`service.go:223-233`），`GetStatus` 读——锁覆盖到位，但 `SetSystemProxy` 的 10s 轮询等待（`:458-469`）与 `IsRunning` 混用 `srv.Status()`/`Service.started` 两套状态，容易产生"点了开但状态页还是停"的中间态。
7. **`GetLogs` 上限钳制**：limit>1000 时钳到 200（`service.go:568-570`），静默改变调用方意图。

### 3.2 main.go（Wails 壳）
- 窗口 960x680 / min 720x520（`main.go:41-48`），托盘（`main.go:63-100`），关窗隐藏到托盘（`main.go:53-60`，macOS 例外）。
- 托盘菜单"启动/停止"直接调 `svc.Start/Stop`（`main.go:75-85`），错误仅 `log.Printf`——**用户看不到托盘操作错误**（不进前端，前端不轮询托盘触发）。
- 退出等待 `svc.IsRunning()` 最多 2s（`main.go:92-95`）保证系统代理清理——注释解释了原因。
- **问题点**：Android 上 `application.New` 的 Mac 选项与 SystemTray 是否有 no-op 保护？Wails v3 的 Android 运行模型由 Wails 内部处理，SystemTray 在 Android 上大概率空转，但代码无显式 `runtime.GOOS=="android"` 分支——依赖 Wails 内部兼容，风险由框架 alpha 承担。

### 3.3 logs.go
- 设计干净：ring buffer + logWriter，包 `init()` 提前接管日志（`logs.go:19-22`）。500 条容量。
- 级别推断是启发式（见 2.6）。

---

## 4. 状态管理 / 主题 / i18n 评估

### 4.1 状态管理：**无全局 store，页面本地 useState + 手写轮询**
- 无 zustand/redux/react-query/jotai。所有数据经 `api.ts` 方法 fetch，页面各自 useState + useEffect 轮询。
- **影响**：
  - 页面切换即卸载组件，切回**重新加载 + 重新轮询**（StatusPage 每次进入重新 fetch，Flash of loading）。
  - 跨页共享状态（如 running 状态、主题 mode、config）无单一数据源：StatusPage 与托盘菜单都管 Start/Stop，但状态不同步（托盘启停后 StatusPage 要等 2s 轮询才反映）。
  - 无缓存/失效策略：`getConfig` 在 SettingsPage 与 StatusPage 重复轮询，`getStatus` 每 2s 全量返回（含注册信息+config 快照，数据量大但频率低）。
  - `saveConfigPartial`（`api.ts:418-421`）实现"先 getConfig 再 saveConfig"的读改写——**并发下丢更新**（两处同时 partial 会互相覆盖），且每次写全量 config。

### 4.2 主题：**实现良好，是前端最成熟的模块**
- 纯函数 `theme.ts`（`loadMode`/`saveMode`/`resolveDark`/`applyDarkClass`）+ hook `useTheme.ts`。
- 三态（light/dark/system），**持久化双源**：`config.json` 的 `theme_mode`（权威，Go 侧 `Config.ThemeMode`，`config.go:50`）+ localStorage `warpgo-theme`（`theme.ts:12`）。
- 5 平台 OS 主题事件监听（`useTheme.ts:33-39`，common/windows/linux/android/ios）+ matchMedia 兜底 + Android 300ms 延迟重查（`useTheme.ts:113-117`）。
- 只在用户显式 `setMode` 时持久化（`useTheme.ts:146-152`），effect/OS 事件从不写文件（v0.5.24 修复）。
- **小瑕疵**：localStorage 写入路径（`theme.ts:49-54` saveMode）在真实模式下**写但不读**——`useTheme` 用 `getConfig().theme_mode` 恢复（`useTheme.ts:127-133`），`loadMode` 读 localStorage 只被测试引用。双写双读产生"两处可能不一致"的隐患，建议收敛到单一权威源。

### 4.3 i18n：**完全没有**
- 所有文案硬编码中文（`nav.ts:10-17`、各页面 JSX、`api.ts` mock 日志、`main.go` 托盘菜单、`service.go` 错误字符串）。
- 计划/用户未提及国际化需求（东哥中文环境），**当前不阻塞**；但若未来要英文版/多语言，需要全量重构（strings 抽离 + react-i18next/lingui）。

---

## 5. 平台差异处理（桌面三平台 + Android）

### 5.1 共用/分叉
- **同一套 React 前端**覆盖桌面 + Android（`App.tsx` 无平台分支，靠 `status.isAndroid` 条件渲染：隐藏系统代理卡片 `StatusPage.tsx:333`）。
- **Go 侧按文件/分支分叉**：
  - `datadir_{android,other}.go`（build tag）
  - `androidbridge.go`（`//go:build android && cgo`）
  - `service.go` 内 `runtime.GOOS=="android"` 运行时分支（见 3.1.5）
- **运行模型差异**：桌面 = core.Server（SOCKS5/mixed 监听 + 系统代理）；Android = VpnService 驱动 `androidRuntime.kernel`（`androidbridge.go:145-162`），SOCKS server 永不启动，`GetStatus` 用 `androidVpnRunning()`/`androidVpnKernel()` 覆盖生命周期字段（`service.go:149-171`）。

### 5.2 桌面三平台
- 系统代理：`sysproxy/` 三平台实现（win 注册表/darwin networksetup/linux gsettings），`core.Status.SysProxyOn` 读真实状态（`core/core.go:684-687`）。
- 开机自启：`autostart/` 三平台（win 注册表/mac LaunchAgent/linux .desktop），GUI Toggle（SettingsPage.tsx:263-280）。
- 关窗行为：`main.go:53-60` 隐藏到托盘，darwin 例外。
- 托盘图标：`SetTemplateIcon`（mac）/`SetDarkModeIcon`（其它）（`main.go:64-69`）。
- **前端呈现差异很小**：仅 `isAndroid` 一处分支；三平台差异全部封装在 Go 服务层/构建层，前端无需感知。这是**好的分层**。

### 5.3 Android 特有 UI 流程
- 同意页（consent）：`MainActivity.connectVpn()`（`MainActivity.java:398-409`）——首次启动自动触发一次（`MainActivity.java:257-260`，SharedPreferences `vpn_consent_prompted`），之后由 React 状态页"启动"按钮 → `Service.Start` → 反向 JNI `androidRequestVpnStart` → `requestStartVpn`（`androidbridge.go:689-712`）。
- 通知权限：Android 13+ `POST_NOTIFICATIONS`（`MainActivity.java:264-270`）。
- 注册流程：与桌面同一套（React 状态页一键注册 → `Service.Register` → core），Android 沙箱路径由 `dataDir()` 锚定。
- 时区：JNI `nativeSetTimeZone`（`androidbridge.go:783-808`）。
- 主题事件：`emitTheme()`（`MainActivity.java:959-968`）。
- **问题点**：ScanPage 在 Android 上无特殊处理（见 2.4）；托盘菜单在 Android 上是死代码依赖 Wails 内部处理。

---

## 6. 与 core 的交互边界

- `Service` 持有 `core.Server` 惰性实例（`service.go:58-77`），`core.Options{ConfigPath, DataDir}`（`service.go:71-74`）。
- `Server` 是 CLI/GUI/Android 三端复用核心（`core/core.go:100-124`）：`Status()` 返回无指针/无 channel 的可序列化快照（`core/status.go:10-12`），专为跨 Wails 边界设计。
- Android 走 `core.Kernel`（`core/kernel.go:93-315`：NewKernelContext/DialTunnel/ResolveDNS/Route/ReloadRules/Stats/Rules），由 `androidbridge.startVpnKernel` 装配（`androidbridge.go:312-490`）。Kernel 是"隧道+引擎"，Server 是"生命周期+监听+系统代理+GEO 更新"——边界清晰。
- `Service` 与 `Server` 的锁协议有历史坑：`Service.mu` 与 `serverInstance()` 二次加锁导致首启死锁（`service.go:94-95` 注释，v0.6 修复），代码反复强调"不能在持有 s.mu 时调 serverInstance"。这是脆弱点，重构时值得把 `Service` 拆成无锁门面。
- `GetStatus` 在 Android 分支从 `androidRuntime.kernel` 读真实 Stats/Rules（`service.go:167-170`），桌面从 `Server.kernel`——**同一 Status 快照两个数据源**，已用注释说明，但属于隐式约定。

---

## 7. 重构机会列表（按优先级）

### P0（契约/数据正确性，强烈建议先做）

1. **统一前后端契约，消除手工 snake_case↔camelCase 双维护**
   - 现状：`core.Status`/`Config` 与前端 `AppConfig`/`AppStatus` 各自独立定义 + `from*` 归一化，历史双重归一化 bug 反复踩（v0.5.7）。
   - 建议：a) 用 `wails3 generate bindings` 的 TS 输出作为前端类型的**唯一来源**（生成到 `frontend/bindings/`），删除 `types.ts` 手工镜像与大部分 `from*`；b) 或给 `route.Stats` 补 json tag（`route/matcher.go:25`），统一驼峰/下划线。目标：任何一端改字段，另一端编译期失败而非运行期静默错。
2. **修复 `geoBaseURL` 死字段契约**（见 2.5）
   - 三选一：a) Go `Config` 补 `geo_base_url` 字段并让 `GeoSiteURL()`/`GeoIPURL()` 支持覆盖；b) 前端删除该输入框与 `AppConfig.geoBaseURL`；c) 保持 repo-only，`GetGeo` 的 `BaseURL` 改为由 repo 推导并如实展示。当前"能编辑但不生效"最误导用户。

### P1（架构/可维护性）

3. **引入轻量状态层，消灭手写轮询**
   - 现状：`usePoll` 仅 StatusPage 用，LogsPage/RulesPage 各自重复 effect（`StatusPage.tsx:14-39` / `LogsPage.tsx:22-43` / `RulesPage.tsx:15-35`）。
   - 建议：引入 `@tanstack/react-query`（对轮询/失效/缓存/后台刷新天然支持，Wails 前端体积可接受）或至少提取共享 `usePoll` hook 到 `lib/`。收益：统一轮询节流、跨页缓存（切换页不重新 fetch）、`saveConfigPartial` 读改写改 atomic invalidate。
4. **统一错误/反馈 UI，收敛 notice/error/busy 模式**
   - 现状：5 页各自内联绿/红提示（见 2.6 缺失复用）。建议：Toast/Alert 组件 + 统一的 `useAsyncAction` hook（loading + error + notice 三态）。
5. **Service 层拆出 Android 分叉，消灭 `runtime.GOOS` 散布**
   - 建议：定义 `PlatformBackend` 接口（`Start/Stop/Status/ReloadRules/...`），桌面实现（包 `core.Server`）、Android 实现（包 `androidRuntime`），`Service` 依赖注入。既消 `service.go:185-269` 等散布分支，也让 Android 逻辑可宿主单测。
6. **托盘/窗口操作错误透出到前端**
   - 现状：托盘启停失败只 `log.Printf`（`main.go:75-85`），用户无感知。建议：经 `Events` 事件推送给前端（Wails `Events.Emit`），或托盘内直接弹出确认。

### P2（体验/细节）

7. **规则编辑器升级**：裸 textarea + N 行 `<div>` 行号（`RulesPage.tsx:93-113`）→ CodeMirror 6（plan 原设想）+ 错误行定位（Go 校验返回行号即可高亮）。至少修 `ruleCount` 漏计 REJECT 行（`RulesPage.tsx:38-46`）。
8. **日志"清空"语义**：加 Go 侧 `ClearLogs()` 或去掉前端假清空（`LogsPage.tsx:68`）。
9. **GetGeo 数据真实性**：`LastChecked` 改为真实上次检查时间（service.go:403），`BaseURL` 由 repo 推导展示。
10. **ScanPage Android 体验**：确认/禁用 Android 上不可用的扫描路径。
11. **i18n（可选）**：若未来要英文版，引入 react-i18next + 抽取字符串；当前中文单语不阻塞。

---

## 8. 风险

| 风险 | 说明 | 缓解 |
|---|---|---|
| **Wails v3 alpha 稳定性** | 钉在 alpha2.119；`Register` 多返回值元组序列化（api.ts:220-231）、Android bridge 抖动返回 `""`（datadir_android.go 缓存应对）都暴露框架层不稳定 | 版本已钉住；核心与 GUI 解耦（core/ 独立）；升级 Wails 需回归全部 22 个绑定方法 |
| **前后端契约无编译期保障** | 手工双份类型 + from* 归一化，字段改名/新增靠运行期发现 | P0-1（bindings 单源化） |
| **Android 与桌面同一前端但两套后端语义** | `GetStatus` 两个数据源（Server.kernel vs androidRuntime.kernel）；service.go 运行时分支多 | P1-5（PlatformBackend 接口） |
| **本地无法编译 GUI** | GTK 4.6 < 4.10；前端改动能本地 `npm run build`（demo 兜底），但 Go 服务层改动只能在 CI 验证 | 沿用 CI build-gui；`npm run build` + `go test ./gui/...` 本地兜底 |
| **前端 mock/demo 代码随生产打包** | api.ts 约 350 行 mock 逻辑始终在 bundle（`api.ts:57-171`），增大体积且存在"误进 demo 模式"风险 | 低危；若在意可拆到独立模块按 import.meta.env 裁剪 |
| **bindings 生成时序** | `build:frontend` 依赖 `generate:bindings`（build/Taskfile.yml:89）；CI 若漏跑或路径变化会退回 demo 模式（界面静默降级） | 已有 darwin universal `run: once` 防护；建议 CI 断言 bindings 存在 |

---

## 9. 结论摘要

GUI 栈**整体健康**：分层清晰（core 复用三端、服务门面、React 壳）、Wails v3 + React 19 + Tailwind v4 组合轻量现代、主题模块成熟、日志轮询有去重修复经验、Android 与桌面共用一套前端且平台差异大多封在 Go 侧。主要问题不在结构而在**契约与一致性**：前后端类型双维护（已踩双重归一化坑）、`geoBaseURL` 死字段、`route.Stats` 无 json tag、轮询/错误提示/AsyncAction 大量重复。重构优先级：P0 统一 bindings 单源契约 + 修死字段；P1 引入 react-query/共享 hook、Service 拆 PlatformBackend、统一 Feedback；P2 规则编辑器升级、日志清空语义、GEO 数据真实性。
