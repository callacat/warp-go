# Design: 阶段2 契约层重构

## 设计决策

### D1: route.Stats json tag 命名 — camelCase 单词键

tag: `json:"proxy"` / `json:"direct"` / `json:"rejected"` / `json:"miss"`。

理由（调研 `stats-json-tag.md`）：
- 前端 `ProxyCounters` 接口与 `StatusPage` 消费已固化 `proxy/direct/miss/rejected`，tag 对齐后 `fromCounters` 的大写键兜底变为死代码可删。
- 四个字段本身是单词，snake_case 与 camelCase 拼写一致；唯 `RejectedHits` 若改 `rejected_hits` 需同步改前端，徒增面。
- Go 侧零破坏（全部消费点用结构体字段名 `st.ProxyHits` 访问，无 JSON 键断言）。

### D2: geoBaseURL — 选项 3+2（GetGeo 推导 + 删输入框）

不新增 Config 字段（URL 可由 GeoRepo 推导，避免双源真值）。

改动：
- `gui/service.go:395` GetGeo 的 `info.BaseURL` 改为 `Config.GeoRepo` 推导（`strings.TrimRight(repo,"/") + "/releases/download/latest"`），Config 为 nil 保留默认。
- 删 SettingsPage "GEO 下载 URL" Field + AppConfig.geoBaseURL + fromConfig 读取 + mockConfig.geoBaseURL。

GeoPage 无连带影响（仍消费 `geo.baseURL`，GetGeo tag 不变）。

### D3: bindings 单源化 — 类型化适配器（编译期保障）

**核心目标**：Go 改字段时前端 tsc 编译期失败，而非运行期静默错。

**方案**：`from*` 函数输入参数从 `unknown` 改为生成的类型。生成类型字段名 = json tag 名，与 Go 序列化完全一致，故三向兜底（snake/camel/大写）全部删除。

**保留 vs 删除**：

| from* | 决策 | 理由 |
|---|---|---|
| `fromCounters` | 简化（输入 `Stats`） | Stats 加 tag 后字段名对齐，删大写兜底 |
| `fromConfig` | 简化（输入 `Config`） | 删 camelCase 兜底 + `geoBaseURL` 读取 |
| `fromGeo` | 简化（输入 `GeoInfo`） | 删 camelCase 兜底 |
| `fromRegistration` | 简化（输入 `RegistrationInfo`） | 删 camelCase 兜底 |
| `fromStatus` | 保留语义映射 | `state` 枚举 → `running` 布尔派生（页面消费 `running`，非 `state`） |
| `fromLogs` | 保留 | `level: string` → `LogLevel` 联合 + 未知降级 `info` |

**手工 UI 接口（AppConfig/AppStatus 等）的去留**：保留为 camelCase 人体工学类型，作为 from* 的输出。它们是"镜像"但经 from* 输入类型化后**编译期验证**（Go 改字段 → from* 引用失败 → tsc 报错）。完全删除手工接口（前端全面 snake_case）属阶段 4 GUI 重构范围（页面重写时顺带迁移），本阶段不做以控制爆炸半径。

**契约保障链**：Go struct json tag → `wails3 generate bindings` → 生成 TS 类型 → `from*` 输入类型化 → tsc 编译期检查。任一环改字段，下游编译失败。

### D4: 演示模式检测 — Wails 运行时探测

现状 `__MOCK_BINDINGS__` 占位文件从未存在，靠 import 抛错进演示模式。bindings 生成后 import 必成功，需新检测。

方案：`loadService()` 动态 import bindings（成功 → 有类型）；检测 `typeof window !== 'undefined' && typeof window.wails?.invokeAsync === 'function'`（Wails 注入的全局）。真 → 真调用；假 → 演示模式。

依据：`@wailsio/runtime/dist/runtime.js:185` runtime 后端解析 = `typeof window.wails?.invokeAsync === "function" ? window.wails : null`。

### D5: saveConfig 简化

现状 `saveConfig` 手工逐字段映射 camelCase→snake_case（`api.ts:404-414`）。改造：构造 `core.Config` 对象直接传 `svc.SaveConfig()`，类型由生成类型检查（缺字段/多字段 tsc 报错）。

### D6: register 简化

生成类型给出 `Register(): [boolean, string]`。直接 `const [existing, id] = await svc.Register()`，删对象分支兼容。

## 数据流（改造后）

```
Go service.go          wails3 generate bindings        前端 api.ts/types.ts
┌─────────────┐        ┌──────────────────┐            ┌──────────────────┐
│ Service     │        │ frontend/bindings/│            │ import 生成类型  │
│  GetStatus  │───────▶│ warp/gui/service  │◀───────────│ from*(生成类型)  │
│  →core.Status│       │ warp/core/models │            │  →AppStatus      │
│  GetConfig  │        │ warp/route/models│            │ saveConfig(Config)│
│  →core.Config│       │  (Stats 有 tag)  │            │                  │
└─────────────┘        └──────────────────┘            └──────────────────┘
```

## 工具链（本机已就绪）

- `go1.26.5`：`/home/agent/.local/go/bin/go`
- `wails3`（CGO_ENABLED=0 编译）：`/home/agent/.local/bin/wails3`
- `wails3 generate bindings -ts -clean=true`（在 `gui/` 下）→ 生成 `frontend/bindings/`（gitignored）。
- `npm install` 已完成（99 packages）。

## 风险

| 风险 | 缓解 |
|---|---|
| gui Go 包本机无法编译（无 GTK） | `wails3 generate bindings` 做类型检查门；`npm run build` 做前端门；GUI 完整编译走 CI |
| 生成类型字段必填但 Go 返回零值空串（RegistrationInfo 无 omitempty） | from* 容忍空串（`str(o.account, "")`）|
| bindings gitignored → fresh checkout `npm run build` 失败 | 加 `package.json` `bindings` script + 文档；CI Taskfile 已含 generate:bindings dep |
| 演示模式 mock 数据需符合生成类型 | mock 用 `as` 断言或构造对象满足结构类型 |
