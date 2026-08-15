# Implement: 阶段2 契约层重构

## 执行顺序

### Step 1: route.Stats json tag（Go，零破坏）

1. `route/matcher.go:25-30` — Stats 四字段加 tag：
   ```go
   type Stats struct {
       ProxyHits    int64 `json:"proxy"`
       DirectHits   int64 `json:"direct"`
       RejectedHits int64 `json:"rejected"`
       Misses       int64 `json:"miss"`
   }
   ```
2. 更新 `Stats` 上方注释（删"字段无 JSON tag"说明，改为说明字段 json tag 与前端对齐）。
3. **验证**：`cd /home/agent/workspace/warp-go && /home/agent/.local/go/bin/go test ./route/...`（应全绿，Go 测试用字段名访问不涉 json 键）。

### Step 2: geoBaseURL 死字段（Go + 前端）

1. `gui/service.go` GetGeo（~L395）— `info.BaseURL` 改为 Config.GeoRepo 推导：
   ```go
   if st.Config != nil && st.Config.GeoRepo != "" {
       info.BaseURL = strings.TrimRight(st.Config.GeoRepo, "/") + "/releases/download/latest"
   } else {
       info.BaseURL = "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest"
   }
   ```
   （删原硬编码行；`strings` 已导入。）
2. `gui/frontend/src/lib/types.ts` — `AppConfig` 删 `geoBaseURL` 字段（L45）；`fromConfig` 删 `geoBaseURL` 读取（L150-153）。
3. `gui/frontend/src/lib/api.ts` — `mockConfig` 删 `geoBaseURL`（L150）。
4. `gui/frontend/src/pages/SettingsPage.tsx` — 删"GEO 下载 URL" Field 整块（L216-222）。

### Step 3: 重新生成 bindings

```bash
cd /home/agent/workspace/warp-go/gui
export PATH=/home/agent/.local/go/bin:/home/agent/.local/bin:$PATH
wails3 generate bindings -ts -clean=true
```
确认 `frontend/bindings/warp/route/models.ts` 的 `Stats` 类有 `"proxy": number` 等字段（验证 tag 生效）。

### Step 4: types.ts 改造（from* 输入类型化）

1. 顶部 import 生成类型：
   ```ts
   import { Status, Config, RegistrationInfo } from "../../bindings/warp/core/models.js";
   import { GeoInfo, LogEntry, UpdateGeoResult } from "../../bindings/warp/gui/models.js";
   import { Stats } from "../../bindings/warp/route/models.js";
   ```
2. `fromCounters(v: Stats)` — 输入类型 `Stats`，读 `o.proxy` 等（删 `o.ProxyHits` 大写兜底）。
3. `fromConfig(v: Config)` — 输入 `Config`，删 camelCase 兜底（`o.listen_addr ?? o.listen` → `o.listen_addr`）+ 删 `geoBaseURL`。
4. `fromGeo(v: GeoInfo)` — 输入 `GeoInfo`，删 camelCase 兜底。
5. `fromRegistration(v: RegistrationInfo)` — 输入 `RegistrationInfo`，删 camelCase 兜底。
6. `fromStatus(v: Status)` — 输入 `Status`，删 camelCase 兜底；保留 `state → running` 语义映射。
7. `fromLogs(v: LogEntry[])` — 保留 level 校验降级。
8. 删 `num`/`str` 工具函数中对 `unknown` 的依赖（输入已有类型，可简化但保留无妨）。

### Step 5: api.ts 改造

1. 删 `ServiceAPI` 结构化接口（L30-55）。
2. `loadService()` — 改返回类型为 `typeof import("../../bindings/warp/gui/service.js") | null`；删 `__MOCK_BINDINGS__` 检测；改检测 `typeof window !== 'undefined' && typeof (window as any).wails?.invokeAsync === 'function'`。
3. 各 public 函数返回类型用生成类型（`Promise<Status>` 等），from* 在返回前应用。
4. `register()` — `const [existing, id] = await svc.Register()` 直接解构（删对象分支）。
5. `saveConfig()` — 构造 `Config` 对象直接传 `svc.SaveConfig()`（类型检查；删手工映射）。
6. `getConfig()` — `return fromConfig(await svc.GetConfig())`。
7. mock 数据：字段改 snake_case（如 `listen_addr`/`rules_path`/`geo_dir`/`geo_repo`/`geo_auto_update_days`/`enable_system_proxy`/`allow_udp`/`download_proxy`/`theme_mode`），删 `geoBaseURL`。

### Step 6: types.test.ts 适配

1. `fromStatus` 测试 — 输入对象需满足 `Status` 类型（补必填字段或用 `as Status`）。
2. `fromGeo` 测试 — 同理。
3. 删/改 `fromConfig` 中 `geoBaseURL` 相关断言（如有）。
4. **验证**：`cd gui/frontend && npm test`（vitest 全绿）。

### Step 7: package.json 加 bindings script

```json
"scripts": {
  "bindings": "wails3 generate bindings -ts -clean=true"
}
```

### Step 8: 验证（本机轻量门）

```bash
# Go 根模块（route Stats tag 不破坏测试）
cd /home/agent/workspace/warp-go && PATH=/home/agent/.local/go/bin:$PATH go build ./... && go test ./route/...

# 前端（bindings 已生成 + tsc 类型检查 + vitest）
cd gui/frontend && npm run build && npm test
```

## 验证命令汇总

| 门 | 命令 | 预期 |
|---|---|---|
| bindings 生成 | `wails3 generate bindings -ts -clean=true`（gui/） | exit 0，`frontend/bindings/` 有文件 |
| Go 根模块 | `go build ./... && go test ./route/...` | 通过 |
| 前端编译 | `npm run build`（tsc + vite） | 通过 |
| 前端测试 | `npm test`（vitest） | 全绿 |

## 回滚点

- Step 1（Stats tag）独立可回滚（Go 侧零破坏）。
- Step 2（geoBaseURL）独立可回滚。
- Step 3-6（bindings 单源）是一组：若 tsc 大面积失败无法修复，回滚 types.ts/api.ts 到改造前（保留 Step 1-2）。
