# Research: geosite 匹配热路径索引 + GEO 换引擎收敛

- **Query**: 调研 route/ 的 geosite 匹配热路径、GEO 换引擎问题，给出索引方案与收敛接口设计
- **Scope**: internal
- **Date**: 2026-08-16

## Findings

### 1. 当前 matchGeoSite 热路径：线性扫描 O(N)

**文件**: `route/matcher.go:137-160`

```go
func (e *Engine) matchGeoSite(domains []GeoSiteDomain, host string) bool {
    for _, d := range domains {
        switch d.Type {
        case routercommon.Domain_RootDomain: // 根域后缀：域名本身 + 子域
            if domainSuffixMatch(d.Value, host) {
                return true
            }
        case routercommon.Domain_Full: // 精确
            if d.Value == host {
                return true
            }
        case routercommon.Domain_Plain: // 子串
            if strings.Contains(host, d.Value) {
                return true
            }
        case routercommon.Domain_Regex: // 正则
            re := e.compiledRegex(d.Value)
            if re != nil && re.MatchString(host) {
                return true
            }
        }
    }
    return false
}
```

**调用链**（`matcher.go:77-92`）：

```
Engine.Match → 遍历 rules → case KindGeoSite → geoSite.Lookup(r.Value) → matchGeoSite(domains, host)
```

**数据结构**（`geodata.go:34-40`）：

```go
type GeoSiteDB struct {
    mu         sync.RWMutex
    categories map[string][]GeoSiteDomain  // 分类名(大写) → 扁平域名规则切片
    path       string
    loadedAt   time.Time
}
```

`GeoSiteDomain`（`geodata.go:28-32`）：

```go
type GeoSiteDomain struct {
    Type  routercommon.Domain_Type
    Value string
}
```

**复杂度分析**：

- `Lookup` 是 O(1) map 查找（`geodata.go:88-93`）
- `matchGeoSite` 对分类内全部条目线性扫描：**O(N)**，N = 该分类的域名条目数
- 真实数据规模（参考 `.omo/research/go-architect.md:242`）：`geolocation-!cn` 约 2.7 万条域名，`google` 等大类别也有数千条
- 每个 CONNECT 请求对每条 geosite 规则都全量扫描该分类 → 热路径开销随规则数 × 分类大小线性增长
- `domainSuffixMatch`（`matcher.go:180-191`）本身 O(len(suffix))，但外层循环 N 次才是瓶颈

**域名类型分布**（`geodata.go:9-12`）：

- `Plain=0`（子串）/ `Regex=1` / `RootDomain=2`（根域后缀，含子域）/ `Full=3`（精确）
- RootDomain 和 Full 是绝大多数条目（Plain/Regex 极少，个位数到几十）
- 这意味着对 RootDomain+Full 建索引能覆盖 99%+ 的条目，Plain/Regex 保留下沉为兜底线性扫

### 2. geosite 数据加载与存储

**文件**: `route/geodata.go:43-93`

加载流程：

1. `LoadGeoSite(path)`（`:43`）→ `geoSiteFromBytes`（`:54`）
2. `proto.Unmarshal` 解码为 `routercommon.GeoSiteList`
3. 遍历 `list.Entry`，每个 entry 的 `CountryCode` 转大写作为 key
4. 遍历 `entry.Domain`，`Value` 转小写存储（加载期归一化，匹配路径零开销）
5. 存入 `map[string][]GeoSiteDomain`

**关键点**：

- 域名值在加载时已统一小写（`geodata.go:77`）
- 类别名大写存储（`geodata.go:69`）
- `Lookup` 大小写不敏感：查询侧转大写走 map（`geodata.go:88-93`）
- 整个 DB 是不可变快照（加载后不修改，热重载时整体替换）
- `reCache`（`matcher.go:46`）是 Engine 级别的正则编译缓存，用 `sync.Map`，惰性编译

### 3. Engine 结构与热加载能力

**文件**: `route/engine.go:32-52`

```go
type Engine struct {
    mu        sync.RWMutex
    rulesPath string
    geoDir    string
    rules     []Rule
    geoSite   *GeoSiteDB
    geoIP     *GeoIPDB
    stopWatch func()
    reCache   sync.Map // map[string]*regexp.Regexp
    statsProxy   atomic.Int64
    statsDirect  atomic.Int64
    statsReject  atomic.Int64
    statsMiss    atomic.Int64
}
```

**现有热加载能力**：

- **规则热重载**：`WatchRulesFile`（`rules.go:163`）轮询 mtime+SHA-256，变更回调 `applyRules`（`engine.go:82-86`）在写锁下替换 `e.rules`
- **GEO 热加载**：`Engine.UpdateGeo`（`engine.go:90-101`）下载新数据后在写锁下调用 `loadGeoDBs`（`engine.go:122-140`）替换 `e.geoSite` / `e.geoIP`
- **Match 并发安全**：`Match`（`matcher.go:61`）在 RLock 下取 `rules/geoSite/geoIP` 快照，然后释放锁做匹配

**热加载能力已存在但未被 Server 使用**：`Engine.UpdateGeo` 方法存在（`engine.go:90`），但 `Server.UpdateGeo` 走的是整体 `NewEngine` + `engineHolder.swap`（见第 4 节）。

### 4. Server.UpdateGeo：整体 NewEngine 而非热加载

**文件**: `core/core.go:727-751`

```go
func (s *Server) UpdateGeo(ctx context.Context) (bool, error) {
    cfg, err := s.ensureConfig()
    // ...
    updated, err := route.UpdateGeoData(ctx, cfg.GeoDir, cfg.GeoSiteURL(), cfg.GeoIPURL())
    if !updated {
        return false, nil
    }
    if k != nil {
        ne, err := route.NewEngine(cfg.RulesPath, cfg.GeoDir)  // 整体重建
        if err != nil {
            return true, fmt.Errorf("GEO 数据已更新，但重建引擎失败（重启后生效）：%w", err)
        }
        k.engine.swap(ne)  // 原子替换整个引擎
    }
    return true, nil
}
```

**engineHolder.swap**（`core/kernel.go:41-49`）：

```go
func (h *engineHolder) swap(e *route.Engine) {
    h.mu.Lock()
    old := h.e
    h.e = e
    h.mu.Unlock()
    if old != nil {
        old.Close()  // 停掉旧引擎的规则文件监听
    }
}
```

**为什么整体 NewEngine 而非热加载？**

1. **历史路径**：Server 早期只有 `Server.kernel`（CLI 模式），GEO 更新直接重建引擎最简单
2. **`Engine.UpdateGeo` 存在但未被调用**：`route/engine.go:90-101` 的 `Engine.UpdateGeo` 方法做的是"下载+loadGeoDBs"，但 Server 侧没有调用它——Server 自己调 `route.UpdateGeoData`（下载）+ `NewEngine`（重建）
3. **整体重建的成本**：`NewEngine` 会重读规则文件、重新加载 GEO 库、重启文件监听 goroutine。规则文件重读和监听重启是不必要的开销
4. **竞态窗口**：`swap` 期间旧引擎被 Close（停监听），新引擎刚建。虽然有 RWMutex 保护，但监听 goroutine 的停启有短暂窗口
5. **统计丢失**：`swap` 替换整个 Engine 实例，`statsProxy` 等原子计数器归零（`engine.go:48-51`）。而 `loadGeoDBs` 热加载保留统计

**调用方**（`Server.UpdateGeo` 的上游）：

- `gui/service.go:425` → GUI"立即更新"按钮
- `main.go:246` → CLI `-geo-update`
- `core/core.go:873` → `geoUpdateOnce` 自动更新协程（7 天周期）

### 5. geosite 规则数据格式

**protobuf 定义**（`routercommon/common.pb.go:20-30`）：

```go
type Domain_Type int32
const (
    Domain_Plain      Domain_Type = 0  // 子串
    Domain_Regex      Domain_Type = 1  // 正则
    Domain_RootDomain Domain_Type = 2  // 根域后缀（域名本身 + 子域）
    Domain_Full       Domain_Type = 3  // 精确
)
```

**RootDomain 条目结构**：

- `Type = Domain_RootDomain`，`Value = 域名后缀`（如 `google.com`）
- 匹配语义：`domainSuffixMatch(suffix, host)`（`matcher.go:180-191`）
  - `host == suffix` → 命中
  - `host` 以 `suffix` 结尾且前一个字符是 `.` → 命中（标签边界）
  - 例：`google.com` 匹配 `google.com` 和 `www.google.com`，不匹配 `notgoogle.com`

**Full 条目结构**：

- `Type = Domain_Full`，`Value = 精确域名`
- 匹配语义：`d.Value == host`（精确相等）

**Plain 条目**：子串匹配 `strings.Contains(host, d.Value)`
**Regex 条目**：`regexp.MatchString`

### 6. 索引方案设计：反序后缀 Map

#### 6.1 核心思路

对 `RootDomain` 和 `Full` 条目建**反序后缀 map**（reversed-label map），把 O(N) 线性扫描降为 O(标签数) 的逐级查找。

#### 6.2 数据结构设计（Go 伪代码）

```go
// geoIndex 是单个分类的域名索引（不可变，构建后只读）。
// 加载期一次性构建，随 GeoSiteDB 一起被热替换。
type geoIndex struct {
    // 反序标签 map：把域名按标签反转后逐级索引。
    // 例：www.google.com → com → google → www
    // 查找时把 host 反序逐级下沉，O(标签数) 命中。
    root *labelNode

    // Full 条目：精确匹配 map（O(1)）
    exact map[string]struct{}

    // Plain/Regex 条目：占比极低，保留下沉为线性扫描
    plains  []string
    regexes []*regexp.Regexp // 加载期预编译，避免运行时惰性编译
}

// labelNode 是反序标签 trie 的节点。
type labelNode struct {
    children map[string]*labelNode // 下一级标签 → 子节点
    // matched 为 true 表示此节点是某条 RootDomain 规则的终点。
    // 例：规则 "google.com" → com → google(matched=true)。
    // 查找 www.google.com 时走到 com → google(matched) 即命中，无需继续到 www。
    matched bool
}
```

#### 6.3 构建逻辑（加载期）

```go
func buildGeoIndex(domains []GeoSiteDomain) *geoIndex {
    idx := &geoIndex{
        root:   &labelNode{children: make(map[string]*labelNode)},
        exact:  make(map[string]struct{}),
        regexes: make([]*regexp.Regexp, 0),
    }
    for _, d := range domains {
        switch d.Type {
        case routercommon.Domain_RootDomain:
            // 反序逐级插入 trie
            labels := reverseLabels(d.Value) // "google.com" → ["com","google"]
            node := idx.root
            for _, label := range labels {
                child, ok := node.children[label]
                if !ok {
                    child = &labelNode{children: make(map[string]*labelNode)}
                    node.children[label] = child
                }
                node = child
            }
            node.matched = true

        case routercommon.Domain_Full:
            idx.exact[d.Value] = struct{}{}

        case routercommon.Domain_Plain:
            idx.plains = append(idx.plains, d.Value)

        case routercommon.Domain_Regex:
            if re, err := regexp.Compile(d.Value); err == nil {
                idx.regexes = append(idx.regexes, re)
            }
        }
    }
    return idx
}

// reverseLabels 把域名按 "." 分割后反序。
// "www.google.com" → ["com", "google", "www"]
func reverseLabels(domain string) []string {
    parts := strings.Split(domain, ".")
    // 去除空标签（尾部 "."）
    for i := len(parts)/2 - 1; i >= 0; i-- {
        opp := len(parts) - 1 - i
        parts[i], parts[opp] = parts[opp], parts[i]
    }
    return parts
}
```

#### 6.4 查找逻辑（热路径）

```go
// match 用索引查找 host，替代原来的线性 matchGeoSite。
// host 需已小写。
func (idx *geoIndex) match(host string) bool {
    // 1. Full 精确匹配：O(1)
    if _, ok := idx.exact[host]; ok {
        return true
    }

    // 2. RootDomain 后缀匹配：trie 逐级下沉，O(标签数)
    labels := reverseLabels(host)
    node := idx.root
    for _, label := range labels {
        child, ok := node.children[label]
        if !ok {
            break
        }
        node = child
        if node.matched {
            return true // 命中：当前路径是某条 RootDomain 规则的终点
        }
    }

    // 3. Plain 子串：线性扫描（占比极低）
    for _, p := range idx.plains {
        if strings.Contains(host, p) {
            return true
        }
    }

    // 4. Regex 正则：线性扫描（占比极低，已预编译）
    for _, re := range idx.regexes {
        if re.MatchString(host) {
            return true
        }
    }

    return false
}
```

#### 6.5 与现有代码的对接

**改动文件**：

| 文件 | 改动 |
|---|---|
| `route/geodata.go` | `GeoSiteDB.categories` 的 value 从 `[]GeoSiteDomain` 改为 `*geoIndex`（或并存：保留原始列表用于展示，新增索引用于查询）。`geoSiteFromBytes` 构建时调用 `buildGeoIndex`。`Lookup` 返回 `*geoIndex` |
| `route/matcher.go` | `matchGeoSite` 改为调用 `idx.match(host)`。`Engine.Match` 中 `geoSite.Lookup` 返回类型变更。`reCache` 可移除（正则在加载期预编译进索引） |
| `route/geodata_test.go` | 更新 `Lookup` 返回类型断言 |
| `route/matcher_test.go` | 测试不变（外部行为不变） |

**GeoSiteDB 结构变更**：

```go
type GeoSiteDB struct {
    mu         sync.RWMutex
    categories map[string]*geoIndex  // 改为索引
    path       string
    loadedAt   time.Time
}

// Lookup 返回索引而非原始列表
func (db *GeoSiteDB) Lookup(name string) (*geoIndex, bool) {
    db.mu.RLock()
    defer db.mu.RUnlock()
    idx, ok := db.categories[strings.ToUpper(name)]
    return idx, ok
}
```

**Engine.Match 改动**（`matcher.go:86-90`）：

```go
// 旧：
domains, ok := geoSite.Lookup(r.Value)
if !ok { continue }
if e.matchGeoSite(domains, lowerHost) { return e.hit(r) }

// 新：
idx, ok := geoSite.Lookup(r.Value)
if !ok { continue }
if idx.match(lowerHost) { return e.hit(r) }
```

**复杂度对比**：

- 旧：O(N) 线性扫描，N = 分类域名数（万级）
- 新：O(L) trie 下沉 + O(1) exact map，L = host 标签数（通常 2-4）
- Plain/Regex 仍 O(P)，P = Plain+Regex 条目数（个位数到几十）

#### 6.6 备选方案：后缀 map（更简单，无 trie）

```go
type geoIndex struct {
    // 后缀 map：RootDomain 的 Value 直接做 key
    // 查找时把 host 的所有后缀（含本身）逐个查 map
    suffixes map[string]struct{} // RootDomain 条目
    exact    map[string]struct{} // Full 条目
    plains   []string
    regexes  []*regexp.Regexp
}

// match 查找：枚举 host 的所有后缀
func (idx *geoIndex) match(host string) bool {
    if _, ok := idx.exact[host]; ok {
        return true
    }
    // 枚举所有后缀：host, host[dot+1:], ...
    for i := 0; i < len(host); i++ {
        if host[i] == '.' {
            suffix := host[i+1:]
            if _, ok := idx.suffixes[suffix]; ok {
                return true
            }
        }
    }
    // 也查 host 本身（无子域前缀的情况）
    if _, ok := idx.suffixes[host]; ok {
        return true
    }
    // Plain/Regex 兜底...
}
```

**后缀 map vs trie**：

- 后缀 map 更简单，查找 O(L)（L = 标签数），每次一次 map lookup
- trie 稍复杂但内存更紧凑（共享前缀），查找同样 O(L)
- 建议先用后缀 map（实现简单，性能够用），后续如有内存压力再换 trie

### 7. GEO 换引擎收敛方案

#### 7.1 当前问题

两个入口职责重叠：

1. `Server.UpdateGeo`（`core.go:727-751`）：`route.UpdateGeoData` → `route.NewEngine` → `engineHolder.swap`
2. `Engine.UpdateGeo`（`route/engine.go:90-101`）：`UpdateGeoData` → `loadGeoDBs`（热加载，保留统计/监听）

Server 走整体重建而非热加载，导致：

- 统计计数器归零（`swap` 换了整个 Engine 实例）
- 规则文件监听 goroutine 停止再重启（不必要开销 + 短暂竞态窗口）
- 规则文件重读（不必要 I/O）

#### 7.2 收敛方案：Server 走 Engine 热加载

**Engine 新增方法**（`route/engine.go`）：

```go
// ReloadGeo 从 geoDir 重新加载 GEO 数据库到内存（热加载）。
// 保留规则、统计、规则文件监听不变；仅替换 geoSite/geoIP。
// 下载由调用方完成（Server.UpdateGeo 负责 route.UpdateGeoData），
// 此方法只做内存替换。调用方需保证 geoDir 中的文件已是最新。
func (e *Engine) ReloadGeo() {
    e.mu.Lock()
    e.loadGeoDBs()
    e.mu.Unlock()
}
```

实际上 `loadGeoDBs` 已存在（`engine.go:122-140`），只需暴露一个公开方法。

**Server.UpdateGeo 改动**（`core/core.go:727-751`）：

```go
func (s *Server) UpdateGeo(ctx context.Context) (bool, error) {
    cfg, err := s.ensureConfig()
    if err != nil {
        return false, err
    }
    s.mu.Lock()
    k := s.kernel
    s.mu.Unlock()

    updated, err := route.UpdateGeoData(ctx, cfg.GeoDir, cfg.GeoSiteURL(), cfg.GeoIPURL())
    if err != nil {
        return false, err
    }
    if !updated {
        return false, nil
    }
    if k != nil {
        e := k.engine.get()
        if e != nil {
            e.ReloadGeo()  // 热加载，保留统计/监听
        } else {
            // 引擎未就绪（理论上不会发生，k != nil 即 engine 已装配）
            ne, err := route.NewEngine(cfg.RulesPath, cfg.GeoDir)
            if err != nil {
                return true, fmt.Errorf("GEO 数据已更新，但重建引擎失败（重启后生效）：%w", err)
            }
            k.engine.swap(ne)
        }
    }
    return true, nil
}
```

**接口变更总结**：

| 组件 | 变更 |
|---|---|
| `route.Engine` | 新增 `ReloadGeo()` 公开方法（内部调用已有的 `loadGeoDBs`） |
| `core.Server.UpdateGeo` | `NewEngine + swap` 改为 `e.ReloadGeo()`，保留 `swap` 作为引擎未就绪时的 fallback |
| `core.engineHolder` | 不变（`swap` 保留给路径变更等真正需整体重建的场景） |
| `route.Engine.UpdateGeo` | 可废弃（Server 不再走它；或者保留但标记为内部测试用） |

#### 7.3 与索引方案的协同

`ReloadGeo` 热加载新 GEO 数据时，`loadGeoDBs` → `LoadGeoSite` → `geoSiteFromBytes` 会重建 `GeoSiteDB`。如果 `GeoSiteDB` 内部改为 `*geoIndex`，那么索引在加载期自动构建，`ReloadGeo` 替换整个 `GeoSiteDB` 指针即可——索引随 DB 一起被替换，无需额外的索引重建逻辑。

热路径 `Engine.Match` 在 RLock 下取 `geoSite` 快照，`ReloadGeo` 在写锁下替换——并发安全，与现有规则热重载同一模式。

## Caveats / Not Found

- **真实 geosite.dat 未在仓库中**：无法实测索引前后的 benchmark 数据。建议实现后用真实 `geolocation-!cn` 数据跑 `go test -bench` 验证
- **后缀 map 内存开销**：2.7 万条域名的 map[string]struct{} 约 1-2 MB，可接受。如需更紧凑可后续换 trie
- **`Engine.UpdateGeo`（route/engine.go:90）的调用方**：grep 确认 Server 不调用它，只有 `Engine` 自己的测试可能用。收敛后可安全废弃或保留为内部方法
- **geoip 匹配已优化**：`GeoIPDB.Contains`（`geodata.go:180-195`）用二分查找 + 逆序扫描，已非线性。本次索引优化只针对 geosite
