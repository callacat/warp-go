# 阶段 4 调研笔记

## P1-3: 手写轮询 → 共享 usePoll hook

### 现状
- StatusPage.tsx:14-39 — 本地 `usePoll` hook（setInterval + 手写 alive/cleanup）
- LogsPage.tsx:22-43 — 内联 useEffect + setInterval（每 1s）
- RulesPage.tsx:15-35 — 内联 useEffect + setInterval（每 2s，dirty 时不覆盖）

### 方案
项目仅 4 个 deps（react/react-dom/lucide-react/@wailsio/runtime），引入 react-query 过重。
提取 StatusPage 的 usePoll 到 `lib/usePoll.ts`，三个页面共用。
另提 `lib/useAsyncAction.ts` 统一 notice/error/busy 三件套（P1-4）。

### 改动文件
- 新建 `gui/frontend/src/lib/usePoll.ts`
- 新建 `gui/frontend/src/lib/useAsyncAction.ts`
- 修改 StatusPage.tsx / LogsPage.tsx / RulesPage.tsx

## P1-5: Service 拆 PlatformBackend

### 现状
service.go 有 10 处 `runtime.GOOS == "android"`：
- L68 serverInstance: Android + dataDir()=="" → error
- L129 GetStatus: Android 分支状态兜底
- L149 GetStatus: Android 状态覆盖
- L185 Start: Android → androidRequestVpnStart
- L239 Stop: Android → androidRequestVpnStop
- L261 IsRunning: Android → androidVpnRunning
- L284 Deregister: Android 用 cachedDataDir 直接删
- L347 ReloadRules: Android → androidReloadRules
- L443 SetSystemProxy: Android → error
- L605 OpenExternalBrowser: Android → androidOpenExternalBrowser

androidctl_other.go（build !android）为这些 Android 函数提供桌面桩。

### 方案
- 新建 `platform.go`：定义 `platformBackend` 接口
- 新建 `platform_desktop.go`（build !android）：桌面实现
- 新建 `platform_android.go`（build android）：Android 实现
- service.go 持有 `platform platformBackend`，替换所有 GOOS 分支
- 删除 `androidctl_other.go`（桌面桩不再需要——桌面后端直接实现）

## P2-7: CodeMirror 规则编辑器

### 现状
RulesPage.tsx 用裸 textarea + 手写行号 div。ruleCount 只计 proxy/direct，漏计 REJECT 行。

### 方案
- npm install @codemirror/state @codemirror/view @codemirror/commands @codemirror/lang-json（已装）
- RulesPage 用 CodeMirror 6 替换 textarea
- ruleCount 加 REJECT 计数

## P2-8: ClearLogs

### 现状
LogsPage 清空只清前端 state（setLogs([])），Go 侧环形缓冲不变。

### 方案
Go 侧加 `ClearLogs()` 方法，前端调它。

## P2-9: GetGeo BaseURL/LastChecked

### 现状
- BaseURL: 从 GeoRepo 推导，已正确（service.go:395-399）
- LastChecked: `time.Now()` — 每次调用都返回当前时间，不是真实"上次检查时间"

### 方案
LastChecked 记录真实检查时间（用 GEO 文件的 mtime 作为代理）。
