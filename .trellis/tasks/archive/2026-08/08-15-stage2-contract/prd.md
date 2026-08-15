# 阶段2 契约层重构（bindings 单源/route.Stats/geoBaseURL）

## Goal

消除前后端类型双维护与死字段：任何一端改字段，另一端编译期失败而非运行期静默错。

## Background

- 重构方案 `.omo/plans/warp-go-refactor-2026-08-15.md` 阶段 2（P0）。
- 现状（4 份调研 + 3 份 trellis-research 已取证）：
  - 前端 `types.ts` 手工镜像 Go 类型 + 6 个 `from*` 防御归一化（snake/camel/Go 大写键三向兜底），v0.5.7 双重归一化血泪。
  - `route.Stats` 无 json tag → 序列化大写键 `ProxyHits`，`fromCounters` 被迫双向兼容。
  - `geoBaseURL`：Config 无此字段，SettingsPage 输入框保存静默丢失；GetGeo 硬编码 URL 与实际下载不一致。
- `wails3 generate bindings` 已验证可在本机生成 TS（v3.0.0-alpha2.119；本机已装 go1.26.5 + wails3 CGO_ENABLED=0）。

## Requirements

### R1: route.Stats 补 json tag

- `route/matcher.go` 的 `Stats` 四字段补 json tag：`json:"proxy"` / `json:"direct"` / `json:"rejected"` / `json:"miss"`（与前端既有 `ProxyCounters` 键一致）。
- Go 侧零破坏（全部消费点用结构体字段名访问，无 JSON 键断言；调研 `stats-json-tag.md` 已列全部消费点）。
- 同步更新 `Stats` 结构注释（删除"字段无 JSON tag"说明）。

### R2: geoBaseURL 死字段清理

- 删除 SettingsPage "GEO 下载 URL" 输入框（`:216-222`）。
- 删除 `AppConfig.geoBaseURL` 字段 + `fromConfig` 中对应读取。
- 删除 `mockConfig.geoBaseURL`。
- 修复 `gui/service.go` GetGeo 的 `BaseURL`：由 `Config.GeoRepo` 推导（`TrimRight(repo,"/") + "/releases/download/latest"`），Config 为 nil 时保留硬编码默认——与 `Config.geoURL()` 推导同源，使 GeoPage 展示如实反映用户配置。
- 不新增 Config 字段（URL 完全可由 GeoRepo 推导，避免双源真值）。

### R3: bindings 单源化（编译期契约保障）

- `wails3 generate bindings -ts` 生成的 TS 为前后端类型唯一来源。
- `types.ts` 的 `from*` 函数输入参数类型从 `unknown` 改为生成的类型（`core.Status`/`core.Config`/`GeoInfo` 等）——Go 改字段时 tsc 编译期失败。
- 删除 `from*` 中的 snake/camel/大写三向兜底（生成的类型字段名 = json tag 名，与 Go 序列化完全一致）。
- 删除 `api.ts` 的手工 `ServiceAPI` 结构化接口，改用生成的 `Service` 命名空间类型。
- 删除 `__MOCK_BINDINGS__` 占位检测（占位文件从未存在）；改用 Wails 运行时检测判断演示模式（`window.wails?.invokeAsync`）。
- 简化 `register`：生成类型已给出 `[boolean, string]` 元组，直接解构。
- 简化 `saveConfig`：构造 `core.Config` 对象（类型由生成类型检查），不再手工逐字段映射。
- `package.json` 加 `bindings` script（`wails3 generate bindings -ts -clean=true`）。
- **保留** `fromLogs`（`LogEntry.level` 生成类型为 `string`，前端需 `LogLevel` 联合 + 未知降级 `info`）。
- **保留** `fromStatus` 的 `state → running` 语义映射（`state` 是枚举串，`running` 是布尔派生，页面消费 `running`）。

### R4: 文档与工具链

- 更新 `gui/frontend/tsconfig.json` 如需（bindings 已在 include）。
- 本机工具链记录：go1.26.5（`/home/agent/.local/go`）+ wails3（`/home/agent/.local/bin`，CGO_ENABLED=0 编译）。

## Acceptance Criteria

- [ ] AC1: `wails3 generate bindings -ts -clean=true` 在 `gui/` 下成功生成 `frontend/bindings/` TS（exit 0）。
- [ ] AC2: `npm run build`（`tsc && vite build`）在 `gui/frontend/` 下通过（bindings 已生成）。
- [ ] AC3: `go test ./route/...` 全绿（Stats tag 不破坏 Go 测试）。
- [ ] AC4: `go build ./...`（根模块）通过。
- [ ] AC5: `npm test`（vitest）通过——`types.test.ts` 适配后全绿。
- [ ] AC6: SettingsPage 不再有 "GEO 下载 URL" 输入框；`AppConfig` 无 `geoBaseURL` 字段。
- [ ] AC7: `gui/service.go` GetGeo 的 `BaseURL` 由 `Config.GeoRepo` 推导（非硬编码）。
- [ ] AC8: `route.Stats` 有 json tag（`proxy`/`direct`/`rejected`/`miss`）。
- [ ] AC9: `types.ts` 的 `from*` 函数输入参数类型为生成类型（非 `unknown`）；无 snake/camel/大写三向兜底。
- [ ] AC10: `api.ts` 无 `ServiceAPI` 结构化接口、无 `__MOCK_BINDINGS__` 检测。

## Constraints

- 本机无 GTK/WebKit dev → `go test ./gui/...` 无法本地编译（gui 包 import wails 需 cgo+GTK）；GUI Go 编译走 CI。本机用 `wails3 generate bindings`（类型检查，不需 GTK）+ `npm run build`（前端门）+ `go test ./route/...`（根模块 Go 门）兜底验证。
- trellis-implement 禁止 git commit；按 trellis 流程 finish 阶段由主会话提交。
- 不改动 tunnel/core 隧道逻辑（阶段 3 范围）。
