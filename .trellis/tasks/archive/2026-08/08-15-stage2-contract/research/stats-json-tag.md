# Research: route.Stats 补 json tag

- **Query**: route.Stats 结构无 json tag，序列化用大写键；调研加 tag 后受影响的所有消费点 + 推荐 tag 命名
- **Scope**: internal
- **Date**: 2026-08-15

## Findings

### 现状

`route/matcher.go:25-30` — `Stats` 结构四个字段（`ProxyHits` / `DirectHits` / `RejectedHits` / `Misses`）均无 json tag，Go 默认序列化用首字母大写字段名。

序列化路径：`core/status.go:23` 的 `Status` 结构持有 `Stats route.Stats json:"stats"`，整个 `Status` 经 Wails 桥序列化给前端。前端 `fromCounters`（`gui/frontend/src/lib/types.ts:88-98`）被迫同时读大写键（`o.ProxyHits`）与 camelCase 键（`o.proxy`），用 `num(o.proxy, num(o.ProxyHits, 0))` 双向兜底。

### 受影响的消费点清单

#### Go 侧（结构体字段访问，不涉及 JSON 键——加 tag 不影响）

| 文件:行号 | 说明 |
|---|---|
| `route/matcher.go:25-30` | `Stats` 结构定义（待加 tag 处） |
| `route/engine.go:104-110` | `Engine.Stats()` 构造 `Stats{ProxyHits:..., DirectHits:..., RejectedHits:..., Misses:...}` |
| `core/kernel.go:272-278` | `Kernel.Stats()` 调用 `e.Stats()` 返回 `route.Stats` |
| `core/core.go:706` | `Server.Status()` 填充 `st.Stats = e.Stats()` |
| `gui/service.go:168` | Android 分支 `st.Stats = k.Stats()`（androidRuntime.kernel） |
| `core/status.go:23` | `Status.Stats route.Stats json:"stats"`（容器字段，已有 tag） |

#### Go 测试（断言结构体字段名，不涉及 JSON 键——加 tag 不影响）

| 文件:行号 | 说明 |
|---|---|
| `route/matcher_test.go:311-318` | `TestEngineStats` 断言 `st.ProxyHits`/`st.DirectHits`/`st.Misses` |
| `route/matcher_test.go:352-362` | `TestEngineStatsReject` 断言 `st.RejectedHits`/`st.DirectHits`/`st.ProxyHits`/`st.Misses` |
| `core/kernel_test.go:350-370` | `TestKernelStatsAccessors` 断言 `st.ProxyHits`/`st.DirectHits`/`st.Misses`/`st.RejectedHits` |

关键结论：**Go 侧全部通过结构体字段名访问（`st.ProxyHits` 等），不涉及 JSON 键解析。加 json tag 不改变字段名，Go 测试与所有 Go 消费点零影响。** 无任何 Go 测试对 `Stats` 做 JSON marshal/断言键名。

#### 前端侧（JSON 键消费者——加 tag 后可直接简化）

| 文件:行号 | 说明 |
|---|---|
| `gui/frontend/src/lib/types.ts:88-98` | `fromCounters` 双向读取：`num(o.proxy, num(o.ProxyHits, 0))` 等 |
| `gui/frontend/src/lib/types.ts:8-13` | `ProxyCounters` 接口定义 `proxy/direct/miss/rejected` |
| `gui/frontend/src/pages/StatusPage.tsx:58` | 默认值 `{ proxy: 0, direct: 0, miss: 0, rejected: 0 }` |
| `gui/frontend/src/pages/StatusPage.tsx:309` | `status.counters.proxy` |
| `gui/frontend/src/pages/StatusPage.tsx:315` | `status.counters.direct` |
| `gui/frontend/src/pages/StatusPage.tsx:321` | `status.counters.miss` |
| `gui/frontend/src/pages/StatusPage.tsx:327` | `status.counters.rejected` |
| `gui/frontend/src/lib/api.ts:75` | 演示模式 mock：`counters: { proxy: 128, direct: 947, miss: 14, rejected: 23 }` |

前端 `StatusPage` 已全部使用 camelCase 单词键（`proxy/direct/miss/rejected`），无需改动。

### 推荐 tag 命名：camelCase 单词键（proxy/direct/miss/rejected）

推荐：

```go
type Stats struct {
    ProxyHits    int64 `json:"proxy"`
    DirectHits   int64 `json:"direct"`
    RejectedHits int64 `json:"rejected"`
    Misses       int64 `json:"miss"`
}
```

理由：

1. **前端契约已固化**：`ProxyCounters` 接口（`types.ts:8-13`）与 `StatusPage` 消费（`:309-327`）、演示 mock（`api.ts:75`）均已使用 `proxy/direct/miss/rejected`。tag 与前端对齐后，`fromCounters` 的大写键兜底变为死代码可直接删除。
2. **snake_case 不如单词键**：`core.Status` 其余字段用 snake_case（`listen_addr`/`edge_addrs`），但这些是多词缩写。`proxy`/`direct`/`miss` 本身是单词，snake_case 与 camelCase 拼写一致；唯独 `rejected` 若用 `rejected_hits` 则需同步改前端接口与所有消费点，徒增改动面。
3. **命名语义贴合前端**：前端用 `proxy`/`direct`/`rejected`/`miss` 对应"代理/直连/拦截/未命中"四态，与 `ProxyHits` 等 Go 字段名的 "hits" 后缀冗余，前端已主动去掉。tag 沿用前端选定的简短键最自然。

### fromCounters 简化方案

加 tag 后，`fromCounters`（`types.ts:88-98`）可简化为：

```ts
export function fromCounters(v: unknown): ProxyCounters {
  const o = (v ?? {}) as Record<string, unknown>;
  return {
    proxy: num(o.proxy, 0),
    direct: num(o.direct, 0),
    miss: num(o.miss, 0),
    rejected: num(o.rejected, 0),
  };
}
```

大写键兜底（`num(o.ProxyHits, 0)` 等）可全部删除。`types.ts:90-91` 的注释也应同步更新。

### 改动点汇总

| 改动 | 文件:行号 | 内容 |
|---|---|---|
| 加 json tag | `route/matcher.go:26-29` | 四个字段加 `json:"proxy"` / `json:"direct"` / `json:"rejected"` / `json:"miss"` |
| 更新注释 | `route/matcher.go:22-24` | 删除"字段无 JSON tag"说明 |
| 简化 fromCounters | `gui/frontend/src/lib/types.ts:88-98` | 删除大写键兜底，注释更新 |

Go 测试与 `StatusPage` 无需改动。

## Caveats / Not Found

- 无 Go 侧 JSON marshal 测试覆盖 `Stats` 键名（grep `json.Marshal.*Stats` 无结果），因此无测试需更新。
- `androidvpn/` 目录无 `Stats` 直接引用（`androidRuntime.kernel.Stats()` 经 `gui/androidbridge.go` 装配的 `*core.Kernel` 调用，消费点在 `gui/service.go:168`）。
