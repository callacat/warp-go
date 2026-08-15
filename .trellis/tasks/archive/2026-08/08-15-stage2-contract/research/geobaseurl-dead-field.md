# Research: geoBaseURL 死字段

- **Query**: geoBaseURL 在 Config 中不存在、前端输入框保存静默丢失；调研三选项方案与改动点
- **Scope**: internal
- **Date**: 2026-08-15

## Findings

### 现状：geoBaseURL 是端到端断裂的死字段

**Go Config 无此字段**：`core/config.go:23-51` 的 `Config` 结构只有 `GeoRepo`（仓库 URL），下载 URL 由 `GeoSiteURL()`/`GeoIPURL()` 推导，无 `GeoBaseURL` 字段。grep `Config.GeoBaseURL` 零命中。

**推导规则**（`core/config.go:95-103` `geoURL(name)`）：
- `GeoRepo` 非空 → `strings.TrimRight(c.GeoRepo, "/") + "/releases/download/latest/" + name`
- `GeoRepo` 为空 → 回退 `route.DefaultGeoSiteURL` / `route.DefaultGeoIPURL`
- 最终经 `AccelerateURL`（`config.go:71-81`）应用 `DownloadProxy` 加速前缀（仅对 `github.com`/`raw.githubusercontent.com` 域名）

**实际下载路径**（`core/core.go:736`、`core/core.go:804`）：`Server.UpdateGeo` 调用 `route.UpdateGeoData(ctx, cfg.GeoDir, cfg.GeoSiteURL(), cfg.GeoIPURL())`——使用推导 URL + 加速。

**GetGeo 硬编码不一致**（`gui/service.go:395`）：
```go
info.BaseURL = "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest"
```
无论用户 `GeoRepo` 设成什么、`DownloadProxy` 是否启用，`GetGeo` 永远返回这个硬编码 URL。

### 断裂链条（端到端）

| 环节 | 文件:行号 | 问题 |
|---|---|---|
| 前端输入框 | `gui/frontend/src/pages/SettingsPage.tsx:216-222` | 绑定 `cfg.geoBaseURL`，用户可编辑 |
| 前端类型 | `gui/frontend/src/lib/types.ts:45` | `AppConfig.geoBaseURL: string` 字段存在 |
| fromConfig 读取 | `gui/frontend/src/lib/types.ts:150-153` | 读 `o.geo_base_url ?? o.geoBaseURL`——Config 无 `geo_base_url` 键，永远走兜底默认值 |
| saveConfig 保存 | `gui/frontend/src/lib/api.ts:404-414` | 手动映射 snake_case 字段列表中**没有** `geo_base_url`——保存静默丢弃 |
| GetGeo 返回 | `gui/service.go:395` | 硬编码 MetaCubeX URL，不反映 `GeoRepo` 或 `DownloadProxy` |
| GeoPage 展示 | `gui/frontend/src/pages/GeoPage.tsx:106` | `geo?.baseURL` 显示 GetGeo 的硬编码值 |
| 演示 mock | `gui/frontend/src/lib/api.ts:150,167` | `mockConfig().geoBaseURL` 与 `mockGeo().baseURL` 均硬编码同一 URL |
| 前端测试 | `gui/frontend/src/lib/types.test.ts:74` | `fromGeo` 测试用 `base_url` 键（GetGeo 实际返回 `base_url` tag，见 `gui/service.go:368`） |

结论：**SettingsPage 输入框是纯误导**——用户编辑后保存，值被 `saveConfig` 丢弃；`GetGeo` 返回的 `baseURL` 也与实际下载 URL 不一致（不含加速前缀、不反映自定义仓库）。

### 三选项分析

#### 选项 1：Config 补 `geo_base_url` 字段

- 新增 `Config.GeoBaseURL string json:"geo_base_url"`
- `saveConfig`（`api.ts:404-414`）补 `geo_base_url: config.geoBaseURL`
- `GetGeo`（`service.go:395`）改用 `st.Config.GeoBaseURL`
- `fromConfig`（`types.ts:150`）已读取此键，无需改
- `DefaultConfig`（`config.go:55-67`）补默认值

问题：
- **双源真值**：`GeoRepo` 推导与 `GeoBaseURL` 显式字段并存，`geoURL()` 推导逻辑与显式字段谁优先？若 `GeoBaseURL` 非空则覆盖推导，但 `DownloadProxy` 加速如何应用？需要额外规则。
- **过度工程**：下载 URL 完全可由 `GeoRepo` + `DownloadProxy` 推导，无独立存储必要。
- **维护负担**：新增字段需同步 `SaveConfig`、`DefaultConfig`、`geoURL()` 推导优先级、前端映射。

不推荐。

#### 选项 2：前端删输入框

- `SettingsPage.tsx:216-222` — 删除"GEO 下载 URL" `Field`
- `types.ts:45` — `AppConfig` 删除 `geoBaseURL` 字段
- `types.ts:150-153` — `fromConfig` 删除 `geoBaseURL` 读取
- `api.ts:150` — `mockConfig` 删除 `geoBaseURL`
- `saveConfig`（`api.ts:404-414`）— 本就没传，无需改
- `GeoPage.tsx:106` — 仍显示 `geo.baseURL`（来自 `GetGeo`），不受影响

最简方案，消除误导。但 `GetGeo` 仍返回硬编码 URL（与实际下载不一致），GeoPage 展示值仍不准。

#### 选项 3：由 repo 推导如实展示（推荐）

修复 `GetGeo` 的 `BaseURL` 推导，使其与 `geoURL()` 逻辑一致（去掉文件名）：

`gui/service.go:395` 改为：
```go
if st.Config != nil && st.Config.GeoRepo != "" {
    info.BaseURL = strings.TrimRight(st.Config.GeoRepo, "/") + "/releases/download/latest"
} else {
    info.BaseURL = "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest"
}
```
（`strings` 已在 `service.go:17` 导入）

这样 GeoPage 展示的 `baseURL` 反映用户配置的 `GeoRepo`。

注意：`DownloadProxy` 加速前缀在展示 URL 中不体现（`AccelerateURL` 只在下载时应用）。这是合理的——展示"源地址"而非"加速后地址"更符合用户预期；如需展示加速 URL，可用 `cfg.GeoSiteURL()` 但会带上文件名与加速前缀，过于冗长。

**推荐：选项 3 + 选项 2 组合**——修 `GetGeo` 推导（让 GeoPage 展示真实源）+ 删 SettingsPage 输入框（消除"保存静默丢失"误导）。

### 改动点清单（推荐方案：选项 3 + 选项 2）

#### Go 侧

| 文件:行号 | 改动 |
|---|---|
| `gui/service.go:395` | `info.BaseURL` 改为由 `st.Config.GeoRepo` 推导（`TrimRight + "/releases/download/latest"`），Config 为 nil 时保留硬编码默认 |

#### 前端 TS

| 文件:行号 | 改动 |
|---|---|
| `gui/frontend/src/pages/SettingsPage.tsx:216-222` | 删除"GEO 下载 URL" `Field` 整块 |
| `gui/frontend/src/lib/types.ts:45` | `AppConfig` 删除 `geoBaseURL` 字段 |
| `gui/frontend/src/lib/types.ts:150-153` | `fromConfig` 删除 `geoBaseURL` 读取与兜底默认 |
| `gui/frontend/src/lib/api.ts:150` | `mockConfig` 删除 `geoBaseURL` |

#### 无需改动

| 文件 | 原因 |
|---|---|
| `gui/frontend/src/lib/types.ts:62-71` `GeoInfo.baseURL` | 保留——GetGeo 仍返回 `base_url`，GeoPage 仍消费 |
| `gui/frontend/src/lib/types.ts:183-186` `fromGeo` baseURL 读取 | 保留——读取 `o.base_url ?? o.baseURL`，GetGeo 返回 `base_url`（`service.go:368` tag） |
| `gui/frontend/src/pages/GeoPage.tsx:106` | 保留——展示 `geo.baseURL`，改 GetGeo 推导后自动显示正确值 |
| `gui/frontend/src/lib/api.ts:167` `mockGeo().baseURL` | 保留（演示模式仍展示默认 URL） |
| `gui/frontend/src/lib/types.test.ts:74` | 保留（`fromGeo` 测试仍用 `base_url` 键，GetGeo tag 不变） |
| `gui/frontend/src/lib/api.ts:404-414` `saveConfig` | 本就没传 `geo_base_url`，无需改 |
| `core/config.go` | 不新增字段 |
| `core/core.go:736,804` `UpdateGeo` | 已用 `cfg.GeoSiteURL()`/`cfg.GeoIPURL()` 推导，无需改 |

### GeoPage 连带影响确认

- `GeoPage.tsx:106` 展示 `geo?.baseURL`，数据源是 `getGeo()` → `fromGeo(await svc.GetGeo())`。
- `GetGeo`（`service.go:374-405`）返回 `GeoInfo.BaseURL`（tag `json:"base_url"`，`service.go:368`）。
- `fromGeo`（`types.ts:183-186`）读 `o.base_url ?? o.baseURL`——GetGeo 返回 `base_url`，命中。
- 改 `GetGeo.BaseURL` 推导后，GeoPage 自动显示推导值，**无前端连带改动**。
- `UpdateGeo` 路径（`service.go:414-429` → `core.go:727-749` → `route.UpdateGeoData`）使用 `cfg.GeoSiteURL()`/`cfg.GeoIPURL()`，与 `GetGeo.BaseURL` 推导同源（均基于 `GeoRepo`），展示与下载一致。

## Caveats / Not Found

- `DownloadProxy` 加速前缀不体现在 `GetGeo.BaseURL` 展示中（`AccelerateURL` 仅下载时应用）。若产品要求展示加速后 URL，需改用 `cfg.GeoSiteURL()` 但会带文件名+加速前缀，偏冗长。当前推荐不展示加速前缀。
- `route.DefaultGeoSiteURL`（`route/download.go:28`）含文件名（`.../geosite.dat`），与 `GetGeo.BaseURL`（不含文件名）格式不同。推导时需注意 `geoURL()` 空仓库分支返回带文件名的完整 URL，而展示用 `baseURL` 去掉文件名——两者语义不同，不混用。
