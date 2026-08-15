# Research: 前后端契约层现状 + wails3 generate bindings 输出形态（阶段2 bindings 单源化）

- **Query**: 调研前后端契约层现状 + wails3 generate bindings 的 TS 输出形态，为阶段2「bindings 单源化」提供依据
- **Scope**: mixed（内部代码调研 + wails v3 源码查证，源码取自模块缓存而非 web 搜索，更权威）
- **Date**: 2026-08-15

> 重要说明：web 搜索技能（tavily-search / firecrawl-search）在本次会话被权限拒绝。
> 但 wails v3 源码（`v3.0.0-alpha2.119`，与 `gui/go.mod` 钉版一致）完整存在于模块缓存
> `~/go/pkg/mod/github.com/wailsapp/wails/v3@v3.0.0-alpha2.119/`，含生成器源码 + 完整 golden
> testdata。以下结论全部基于源码 + testdata 直接验证，**比官方文档更可靠**（文档滞后于 alpha）。

---

## 1. 现状盘点（内部代码调研）

### Files Found

| File Path | Description |
|---|---|
| `gui/frontend/src/lib/types.ts` | 手工 TS 接口 + 6 个 `from*` 防御归一化函数（fromStatus/fromConfig/fromGeo/fromCounters/fromRegistration/fromLogs） |
| `gui/frontend/src/lib/api.ts` | ServiceAPI 结构化接口、动态 import bindings、`__MOCK_BINDINGS__` 占位检测、演示模式降级、register 元组兼容、saveConfig 手动 snake_case 映射 |
| `gui/service.go` | Service 全部 22 个绑定方法（GetStatus→core.Status、GetConfig→core.Config、Register→(bool,string,error) 等）+ gui 包内 GeoInfo/UpdateGeoResult/LogEntry 类型 |
| `gui/main.go` | Service 注册：`application.NewService(svc)`，模块 `warp/gui`（package main） |
| `gui/logs.go` | LogEntry 结构（`time`/`level`/`msg`，snake_case json tag）+ ringLog 环形缓冲 |
| `core/status.go` | Status / RegistrationInfo 结构，全字段 snake_case json tag；`Stats route.Stats`（无 json tag，嵌入） |
| `core/config.go` | Config 结构，全字段 snake_case json tag |
| `route/matcher.go:25` | `Stats` 结构，**无 json tag**（ProxyHits/DirectHits/RejectedHits/Misses） |
| `gui/frontend/tsconfig.json` | `"include": ["src", "bindings", "vite.config.ts"]` — bindings 目录已在 include |
| `gui/frontend/package.json` | `@wailsio/runtime: "3.0.0-alpha2.119"`；scripts 无 bindings 生成命令 |

### 关键现状：types.ts 的双向兼容逻辑

**`fromCounters`（types.ts:88-98）—— 读 Go 大写键 + camelCase 双向兜底：**
```ts
proxy: num(o.proxy, num(o.ProxyHits, 0)),
direct: num(o.direct, num(o.DirectHits, 0)),
miss: num(o.miss, num(o.Misses, 0)),
rejected: num(o.rejected, num(o.RejectedHits, 0)),
```
原因：`route.Stats` 无 json tag，encoding/json 序列化为 Go 字段名大写键（`ProxyHits` 等）。fromCounters 先读小写 camelCase（演示模式），再读大写 Go 键（真实模式）。

**`fromStatus`（types.ts:100-124）—— snake_case + camelCase 双键：**
每个字段读 `o.snake_case ?? o.camelCase`，例如 `o.start_time ?? o.startedAt`。

**`fromConfig`/`fromGeo`（types.ts:142-195）—— 同样 snake + camel 双键。**

**`saveConfig`（api.ts:404-414）—— 前端 camelCase 手动映射回 snake_case：**
```ts
await svc.SaveConfig({
  listen_addr: config.listen,
  rules_path: config.rulesPath,
  // ...
});
```
注释说明：Go core.Config 的 JSON tag 是 snake_case，直接传对象由 Wails 按字段名映射。

### 关键现状：api.ts 的 bindings 加载机制

**`loadService`（api.ts:94-115）：**
```ts
const mod = (await import("../../bindings/warp/gui/index.js")) as {
  Service?: Record<string, unknown>;
};
const ns = mod.Service;
if (!ns) return null;
if (ns.__MOCK_BINDINGS__ === true) return null; // 占位 → 演示模式
return ns as unknown as ServiceAPI;
```
- 导入路径 `../../bindings/warp/gui/index.js`：从 `frontend/src/lib/` 上溯两级到 `frontend/`，再进 `bindings/warp/gui/`。
- `warp/gui` = 模块路径（go.mod `module warp/gui`，package main）。
- `mod.Service`：绑定生成器把 service 导出为命名空间（见 §2，与 Go 结构体名 `Service` 一致）。
- `__MOCK_BINDINGS__`：当前**无占位文件存在**（`find` 未发现任何 placeholder/bindings 文件），即 standalone `npm run build` 时 import 失败 → catch → 返回 null → 演示模式。这与 api.ts 头部注释「占位文件占据同路径」不符——**实际上占位文件不存在，靠 import 抛错进演示模式**。

**`register`（api.ts:213-231）—— 元组 + 对象双兼容：**
```ts
const raw = (await svc.Register()) as
  | [boolean, string]            // Wails 多返回值 → 元组
  | { existing?: boolean; id?: string }  // 旧对象形态
  | ...;
if (Array.isArray(raw)) {
  return { existing: raw[0] === true, id: typeof raw[1] === "string" ? raw[1] : "" };
}
```
注释明确：「Wails 把 Go 多返回值 (existing, id, error) 序列化为元组 [boolean, string]」——**这已被 wails 源码验证为正确**（见 §2）。

### bindings 目录现状

- `gui/frontend/bindings/` **不存在**（`ls` 失败）。
- git 历史（shallow clone，depth=1，全仓仅 6 commit）**无任何 bindings 提交**：`git log --all --oneline -- gui/frontend/bindings` 无输出，`git ls-files | grep bindings` 无匹配。
- 即：bindings **从未生成过、从未提交过**。当前前端完全靠手工 types.ts + from* 运行期归一化，无编译期类型保障。

---

## 2. wails3 generate bindings 源码查证

> 全部基于 `v3.0.0-alpha2.119` 源码 + golden testdata，非文档转述。

### 2.1 命令用法与 flag

源码：`cmd/wails3/main.go`、`internal/commands/bindings.go`、`internal/flags/bindings.go`

```
wails3 generate bindings [flags] [patterns...]
```

**核心 flag（`internal/flags/bindings.go`）：**

| flag | 含义 | 默认值 |
|---|---|---|
| `-d` OutputDirectory | 输出目录 | `frontend/bindings` |
| `-ts` | 生成 TypeScript（不加则生成 JS） | false |
| `-i` UseInterfaces | 生成 interface 而非 class | false |
| `-names` UseNames | 用方法名而非 ID 调用 | false |
| `--time-type` | time.Time 的 TS 类型（string/Date） | `string` |
| `--models` | 模型文件名（不含扩展） | `models` |
| `--index` | 索引文件名 | `index` |
| `-b` UseBundledRuntime | 用内置 runtime 而非 npm 包 | false |
| `--clean` | 生成前清空输出目录 | **true**（默认开！） |
| `--dry` | 不写文件 | false |

**patterns**：匹配要扫描的 Go 包，格式同 go build（`./...` 匹配当前目录及子目录）。无 pattern 时默认 `.`（当前目录）。

**对 warp-go 的推荐命令**（在 `gui/` 目录下执行）：
```bash
wails3 generate bindings -ts -i -names -d frontend/bindings .
```
- `-ts -i`：生成 TS interface（与现有 types.ts 的 interface 风格一致）
- `-names`：用 `ByName("main.Service.GetStatus")` 而非数字 ID（可读、可调试）
- `-d frontend/bindings`：默认值，与 api.ts import 路径 `../../bindings/warp/gui/index.js` 吻合

**输出目录结构**（验证自 testdata）：
```
frontend/bindings/<go-import-path>/<package>/
  index.ts      # 重导出 service 命名空间 + model 类型
  service.ts    # 按 service 结构体名命名（如 Service.ts? 实际是 service.ts 小写）
  models.ts     # 所有引用的结构体/枚举/别名
```
对 warp-go（module `warp/gui`，package `main`）：
```
frontend/bindings/warp/gui/
  index.ts      # import * as Service from "./service.js"; export { Service }; + export type {...} from "./models.js"
  service.ts    # export function GetStatus(): $CancellablePromise<...> ...
  models.ts     # core.Status, core.Config, route.Stats, GeoInfo, LogEntry 等
```

> 注意：testdata 中 service 文件名是 `greetservice.ts` / `service.ts`——按 **Go 结构体名小写化**命名。
> warp-go 结构体名是 `Service` → 文件名 `service.ts`。index.ts 导出 `Service` 命名空间。
> `api.ts` 的 `mod.Service` 与此一致。

### 2.2 TS 类型命名规则（字段名：json tag 优先，否则 Go 字段名原样）

**这是最关键的发现，源码：`internal/generator/collect/struct.go:173-219`**

字段名（TS 属性名）= **encoding/json 的 JSON key**，规则如下：

1. 解析 `json:"name,opt"` tag（`parseTag`，struct.go:315-336）：
   - `json:"-"` → **不可见，字段被完全省略**
   - `json:"-,"` → key 为字面量 `-`
   - `json:"name"` → key 为 `name`
   - `json:",omitempty"` / `",omitzero"` → 可选（TS 加 `?`），key 仍为 Go 字段名
   - `json:",string"` → 引号包裹（TS 模板字面量 `` `${number}` ``）
2. 若 tag name 非空且 `isValidFieldName`（struct.go:340-352，允许字母/数字/`!#$%&()*+-./:;<=>?@[]^_{|}~ ` 和空格）→ 用 tag name
3. 若 tag name 无效（如含 emoji ❤️）→ 忽略，回退 Go 字段名
4. **若无 tag → `JsonName = field.Name()`（Go 字段名原样，不转 camelCase！）**（struct.go:217-219）

**验证（testdata `complex_json/models.ts`）：**
- `Partner *Person json:"the person's partner ❤️"` → `"Partner"`（emoji 无效 → 回退字段名）
- `embedded4 json:"emb4,omitempty"` → `"emb4"`（tag name 有效）
- `StrangeNumber json:"-,"` → `"-"`
- `Names []string`（无 tag）→ `"Names"`（Go 字段名原样）
- `StrangestString json:",omitempty,string"` → `"StrangestString"?: \`${string}\``

**对 warp-go 的影响——各类型生成的 TS 字段名：**

| Go 类型 | json tag | 生成 TS 属性名 | 现有 types.ts 字段名（camelCase） |
|---|---|---|---|
| `core.Status` | snake_case | `state`, `listen_addr`, `sys_proxy_on`, `init_done`, `is_android`, `registered`, `rules_count`, `geo_ready`, `stats`, `start_time`, `last_error`, `registration`, `config`, `edge_addrs` | `running`, `listening`, `sysProxyOn`, `initDone`, `isAndroid`, `registered`, `counters`, `startedAt`, `error`, `registration` |
| `route.Stats` | **无 tag** | `ProxyHits`, `DirectHits`, `RejectedHits`, `Misses`（Go 字段名大写！） | `proxy`, `direct`, `miss`, `rejected` |
| `core.Config` | snake_case | `listen_addr`, `rules_path`, `geo_dir`, `geo_repo`, `geo_auto_update_days`, `enable_system_proxy`, `allow_udp`, `download_proxy`, `dial_timeout_seconds`, `theme_mode`, `edge_addr` | `listen`, `rulesPath`, `geoDir`, `geoRepo`, `geoBaseURL`, `autoUpdateDays`, `systemProxy`, `allowUDP`, `downloadProxy`, `themeMode` |
| `core.RegistrationInfo` | snake_case | `id`, `account`, `key_type`, `tunnel_type`, `endpoint_v4`, `endpoint_v6`, `endpoint_ports`, `assigned_ipv4`, `assigned_ipv6` | `id`, `account`, `keyType`, `tunnelType`, `endpointV4`, `endpointV6`, `endpointPorts`, `assignedIPv4`, `assignedIPv6` |
| `LogEntry`（gui/logs.go） | snake_case | `time`, `level`, `msg` | `time`, `level`, `msg`（已一致！） |
| `GeoInfo`（gui/service.go） | snake_case | `geosite_path`, `geoip_path`, `geosite_updated`, `geoip_updated`, `repository`, `base_url`, `auto_update_days`, `last_checked` | `geositePath`, `geoipPath`, ... |
| `UpdateGeoResult` | snake_case | `ok`, `message` | `ok`, `message`（已一致！） |

**核心差异：生成的 TS 用 snake_case（因所有 Go 类型都有 snake_case json tag），现有 types.ts 用 camelCase。** 前端消费方需全面适配 snake_case，或保留一层薄归一化。

### 2.3 方法名转换

**源码：`internal/generator/collect/method.go:76`、`service.go:222`**

方法名 = `info.obj.Name()`（Go 方法名原样，**不转 camelCase**）。
FQN = `path + "." + 结构体名 + "." + 方法名`，package main 时 path=`"main"`。

验证（testdata `time/service.ts`）：
```ts
export function GetTime(): $CancellablePromise<string> {
    return $Call.ByName("main.Service.GetTime");
}
```
`GetTime` 保持 Go 原名。`ByName("main.Service.GetTime")`。

**对 warp-go**：`GetStatus` → `GetStatus`，`SaveConfig` → `SaveConfig`，与 api.ts 的 `ServiceAPI` 接口方法名完全一致。无需改方法名。

### 2.4 多返回值：error 剥离 + 元组

**源码：`internal/generator/collect/service.go:315-329`**

```go
// typeError = types.Universe.Lookup("error").Type()  (service.go:195)
for i := range signature.Results().Len() {
    result := signature.Results().At(i)
    if types.Identical(result.Type(), typeError) {
        continue  // error 被剥离，作为 Promise rejection 抛出
    }
    methodInfo.Results = append(methodInfo.Results, result.Type())
}
```
- error **按类型检测**（不是按名称），所有 `error` 类型返回值都被剥离
- 非错误返回值保留顺序

**渲染（`templates/service.ts.tmpl:52-62`）：**
- 0 结果 → `void`
- 1 结果 → 该类型
- ≥2 结果 → 元组 `[T1, T2, ...]`

验证（testdata `complex_method/greetservice.ts`）：
Go: `Greet(...) (person Person, _ any, err1 error, _ []int, err error)`
TS: `$CancellablePromise<[Person, any, number[] | null]>`
（两个 error 都剥离，剩 Person/any/[]int 三元组）

**对 warp-go 各方法：**

| 方法 | Go 返回值 | 生成 TS 返回类型 |
|---|---|---|
| `GetStatus() core.Status` | 1 值 | `$CancellablePromise<core.Status>` |
| `GetConfig() core.Config` | 1 值 | `$CancellablePromise<core.Config>` |
| `Register() (existing bool, id string, err error)` | 2 非错误 + 1 error | `$CancellablePromise<[boolean, string]>` |
| `GetLogs(limit int) []LogEntry` | 1 值 | `$CancellablePromise<LogEntry[]>` |
| `UpdateGeo() UpdateGeoResult` | 1 值 | `$CancellablePromise<UpdateGeoResult>` |
| `Start() error` | 0 非错误 | `$CancellablePromise<void>` |
| `SaveConfig(cfg core.Config) error` | 0 非错误 | `$CancellablePromise<void>` |
| `GetVersion() string` | 1 值 | `$CancellablePromise<string>` |
| `CheckUpdate() (*core.UpdateInfo, error)` | 1 值 | `$CancellablePromise<UpdateInfo>` |
| `GetRules() (string, error)` | 1 值 | `$CancellablePromise<string>` |
| `ScanEdges() ([]string, error)` | 1 值 | `$CancellablePromise<string[]>` |

**`Register` 的元组 `[boolean, string]` 与 api.ts:220-231 的数组分支完全吻合**——可保留该兼容逻辑，或直接用生成类型。

### 2.5 前端调用方式（runtime import + $Call.ByName）

验证自 testdata `time/service.ts`：
```ts
import { Call as $Call, CancellablePromise as $CancellablePromise } from "/wails/runtime.js";
export function GetTime(): $CancellablePromise<string> {
    return $Call.ByName("main.Service.GetTime");
}
```

- runtime import 路径 `/wails/runtime.js`：这是 **Wails 桌面运行时注入的虚拟路径**。
- `-b`（UseBundledRuntime）会改为用内置 runtime（不依赖 npm 包）。
- 不加 `-b` 时依赖 `@wailsio/runtime`（已在 package.json，`3.0.0-alpha2.119`）。
- 调用方式：`$Call.ByName("main.Service.GetStatus", ...args)`（-names 模式）或 `$Call.ByID(12345, ...args)`（默认模式）。
- 返回 `$CancellablePromise<T>`（可取消的 Promise）。

**index.ts 结构**（testdata `time/index.ts`）：
```ts
import * as Service from "./service.js";
export { Service };
export type { TimeAlias, TimeFieldStruct, ... } from "./models.js";
```
即 `mod.Service` 是命名空间，其下是各方法函数。**与 api.ts 的 `const ns = mod.Service` 一致**。

### 2.6 time.Time 处理

验证自 testdata `time/models.ts`：
- `time.Time`（无 tag 或 `,omitempty`）→ TS `string`（默认 `--time-type string`）
- `json:",omitempty"` → `string?`（可选）
- `json:",string"` → 引号包裹

**对 warp-go**：`core.Status.StartTime time.Time json:"start_time,omitempty"` → `start_time?: string`。与 fromStatus 读 `o.start_time` 一致。

### 2.7 嵌入字段处理

源码 `struct.go:163-246`：生成器复刻 encoding/json 的字段展开算法（embedded 结构体递归展平、深度优先、同名冲突优先级）。`core.Status` 的 `Stats route.Stats` 是**命名字段**（非嵌入），直接作为 `stats` 属性。

---

## 3. 落地影响分析

### 3.1 生成干净 TS 的前提（gui/service.go）

生成器扫描 main 包的 Service 结构体的**导出方法**（service.go:134-136 跳过未导出）。方法签名引用的类型必须可见：

- `core.Status` / `core.Config` / `core.RegistrationInfo`：**导出**，在 `warp/core`，生成器会跨包收集（验证：testdata `aliases` 跨包收集 `subpkg.SubStruct`）。✓
- `route.Stats`：**导出**，在 `warp/route`。✓ 但**无 json tag** → 生成 `ProxyHits` 等大写键。阶段2 应补 json tag（计划已列 `route/matcher.go:25`）。
- `GeoInfo` / `UpdateGeoResult` / `LogEntry`：定义在 gui 包（main），导出。✓
- `core.UpdateInfo`：需确认导出（CheckUpdate 返回 `*core.UpdateInfo`）。
- `Service` 结构体本身需**导出**（已是 `Service`）。

**未导出类型会怎样**：生成器只收集被导出方法签名引用的类型；若方法返回未导出类型，生成器会尝试收集其定义但导出名可能冲突（testdata `aliases` 中 `GetButForeignPrivateAlias` 返回 `nobindingshere$0.PrivatePerson`——跨包未导出类型也能生成，但命名可能不理想）。warp-go 无此问题，所有返回类型均导出。

**map/slice/嵌套**：全部支持（testdata `complex_method` 有 `map[int]*bool`、`[]*float32`、匿名 struct 参数）。

### 3.2 生成 TS 与手工 types.ts 的差异

| 差异点 | 生成 TS | 手工 types.ts | 影响 |
|---|---|---|---|
| 字段命名 | snake_case（json tag）/ Go 原名（无 tag） | camelCase | **前端消费方需适配 snake_case 或保留归一化** |
| Stats 字段 | `ProxyHits` 等大写（无 tag） | `proxy` 等（fromCounters 双向兼容） | 补 json tag 后变为 snake_case |
| 可选字段 | `json:",omitempty"` → `?:` | 手工标 `?` | 语义一致 |
| 类型完整度 | 包含 Status 的 `edge_addrs`/`rules_count`/`geo_ready`/`config` 等全字段 | types.ts 省略了部分字段（如 `edge_addrs`、`rules_count`、`geo_ready`、`config`） | 生成更完整 |
| `geoBaseURL` | Config 无 `geo_base_url` 字段 → 不会生成 | types.ts 有 `geoBaseURL`（死字段） | 计划已列「三选一」处理 |
| 返回类型 | 元组/单值/void 精确 | ServiceAPI 全 `Promise<unknown>` | 生成提供编译期类型 |

### 3.3 前端改造清单草案

**types.ts 改为 re-export 生成类型：**
```ts
// 不再手写接口，改为从 bindings 重导出
export type { Status, Config, GeoInfo, LogEntry, UpdateGeoResult, RegistrationInfo }
  from "../../bindings/warp/gui/models.js";
// 或 import 再 export 以保持现有导入路径不变
```
注意：生成类型的字段名是 snake_case，types.ts 重导出后**下游消费方（各 Page 组件）也要改字段访问**。

**可删的 from*：**
- `fromConfig` / `fromGeo`：生成类型字段名 = json tag 名，与 Go 序列化完全一致，无归一化需要（前提：补 Stats json tag 后）。**但 saveConfig 的 camelCase→snake_case 手动映射可删**——生成类型字段已是 snake_case，直接传对象即可。
- `fromCounters`：补 `route.Stats` json tag 后，生成类型字段变为 snake_case，与序列化一致，可删。

**需保留的 from* / 适配逻辑：**
- `fromLogs`（types.ts:199-212）：**需保留**。生成类型 `LogEntry.level` 是 `string`，但前端需要 `LogLevel` 联合类型（`"debug"|"info"|"warn"|"error"`）+ 未知级别降级为 `info`。生成器不会做枚举收窄（除非 Go 用 typed enum）。fromLogs 的级别校验降级逻辑仍有价值。
- `register` 元组解包（api.ts:220-231）：生成类型已是 `[boolean, string]`，可直接用 `const [existing, id] = await svc.Register()`，简化现有代码。对象分支可删。
- `fromStatus` 中的 `running`/`listening` 等语义转换：生成类型字段是 `state`（`"stopped"|"starting"|"running"|"stopping"`），现有 `AppStatus.running` 是布尔派生。**这是语义映射不是命名映射**，需保留一层适配（或改前端消费 `state` 字段）。
- `RegistrationInfo` 可选字段：生成类型用 `?`（因 json tag 有 omitempty？需检查——core.RegistrationInfo 的 json tag **无 omitempty**，所以生成类型字段都是必填 `string`。而现有 types.ts 标为可选 `account?`。**差异点**：生成类型会要求所有字段必填，而实际 Go 可能返回零值空串。前端需容忍空串。

### 3.4 bindings 生成时机建议

**关键约束：本机无 go / wails3 二进制。**
- `which go wails3 wails` 全部失败。
- 计划 §5 依赖声称「`/root/go/bin` 已装 v3.0.0-alpha2.119」——**此说法不成立**，`/root/go/bin/` 不存在。
- wails v3 源码在模块缓存（`~/go/pkg/mod/...`），但无编译好的 `wails3` 可执行文件。

**建议：bindings 生成放 CI。** 两种方案：

1. **CI job 生成 + 提交**（推荐）：新增 workflow job，`go install` wails3 → `wails3 generate bindings -ts -i -names -d frontend/bindings` → `git add frontend/bindings && git commit`。生成的 bindings 入库，前端 `npm run build` 直接用。
   - 优点：前端构建不依赖 wails3；standalone `npm run dev` 也能用真类型。
   - 缺点：Go 改 Service 方法后需手动跑 CI 或加 git hook 触发。

2. **CI 构建时即时生成不入库**：build-gui job 里先 generate 再 build。bindings 不入库。
   - 缺点：standalone `npm run dev` 无类型，仍靠 `__MOCK_BINDINGS__` 占位 + 演示模式。

**package.json 加 script（可选）**：若本机未来装了 wails3：
```json
"scripts": {
  "bindings": "wails3 generate bindings -ts -i -names -d frontend/bindings"
}
```

### 3.5 风险

| 风险 | 说明 | 缓解 |
|---|---|---|
| **本机无 wails3/go** | 无法本地生成验证，计划依赖假设不成立 | 生成放 CI；本机只做文本审计 |
| **snake_case vs camelCase 迁移面大** | 生成类型全 snake_case，现有前端各 Page 用 camelCase 访问字段 | 两种策略：(A) types.ts 加归一化层把 snake 转 camel 再导出；(B) 前端全面改 snake_case。A 改动小但留了归一化层（违背「删 from*」目标），B 改动大但彻底 |
| **shallow clone depth=1** | 无法 git 对照上游 bindings 历史 | 无历史可比，bindings 从零生成 |
| **`--clean` 默认开** | 生成会清空 `frontend/bindings/` 目录 | 若该目录有手工文件会被删；应保持纯生成 |
| **Stats 无 json tag** | 生成大写键 `ProxyHits`，与现有 `proxy` 不符 | 阶段2 补 `route.Stats` json tag（计划已列） |
| **RegistrationInfo 无 omitempty** | 生成类型字段必填，实际可能空串 | 前端容忍空串，或在 Go 补 omitempty |
| **Wails v3 alpha 稳定性** | 钉在 alpha2.119，多返回值元组、runtime 路径可能变 | 版本钉住；升级需回归全部 22 方法 |

---

## Caveats / Not Found

- **web 文档未查证**：tavily-search / firecrawl-search 技能被权限拒绝，无法对照官方文档。所有 wails3 行为结论基于**源码 + golden testdata**直接验证，可靠性 ≥ 文档。
- **`core.UpdateInfo` 结构未读**：CheckUpdate 返回 `*core.UpdateInfo`，其 json tag 现状未核实（api.ts:442-448 的 `UpdateInfo` 用了 `has_update` snake_case，疑似已匹配）。实施时需读 `core` 包确认。
- **生成的 service 文件名**：testdata 中有 `service.ts` 和 `greetservice.ts` 两种，命名规则未在源码中明确定位（疑似结构体名小写）。warp-go 的 `Service` → 需实际生成确认是 `service.ts` 还是 `Service.ts`。index.ts 的 `import * as Service from "./service.js"` 路径会随之变，但 `mod.Service` 命名空间名不变。
- **`-d frontend/bindings` 相对路径基准**：默认值是 `frontend/bindings`，相对 **wails3 执行时的工作目录**。需在 `gui/` 目录下执行（gui/go.mod 所在）。
- **`__MOCK_BINDINGS__` 占位文件**：api.ts 注释声称有占位文件，但实际不存在。当前 standalone 模式靠 import 抛错进演示模式。阶段2 若 bindings 入库则此机制可简化。
