# 阶段3 核心隧道层重构（UDP退役/索引/拆masque）

## Goal

P1-P2：退役双份 SOCKS5/UDP、geosite 索引、GEO 换引擎收敛、拆 2219 行 masque.go

## Requirements

### P1-1: 退役双份 SOCKS5/UDP（省 ~968 行）
- `tunnel/udp.go`（344 行）与 `proxy/udp.go`（362 行）逐行近同，proxy 侧是生产路径
- `MasqueClient.HandleSOCKS5` + `handleDirectConnect` + `SOCKS5Config` + SOCKS5 原语在生产是死代码（仅测试引用）
- 删除 tunnel 侧的 SOCKS5/UDP 实现，保留 proxy 侧单一实现
- 保留 `DialTunnel`/`ResolveDNS`/`Close` 等生产能力
- 删除 `tunnel/masque_socks5_route_test.go`（测的是被删代码）
- 保留 `tunnel/masque_connect_test.go`（测的是 connBundle 健康度，与 HandleSOCKS5 无关）

### P1-2: geosite 匹配加索引
- `route/matcher.go:matchGeoSite` 当前对 RootDomain/Full 线性扫描 O(N)
- `GeoSiteDB` 加载时为每个分类建后缀 map + 精确 map
- RootDomain 走后缀 map（O(域名标签数)），Full 走精确 map（O(1)）
- Plain/Regex 保持线性扫描（数量极少）

### P2-3: GEO 换引擎收敛
- `core/core.go:UpdateGeo` 当前整体 `NewEngine` 重建，绕过了 `route.Engine.UpdateGeo`
- 改为调用 `engineHolder` 的新 `updateGeo` 方法，走 `Engine.UpdateGeo` 热加载
- 消除 `Server.UpdateGeo` 与 `Engine.UpdateGeo` 职责重叠

### P2-4: 拆 masque.go（2219 行 → 4 文件）
- P1-1 先删 SOCKS5 死代码后，剩余约 1250 行按职责拆分
- `client_conn.go`：拨号/重连/探测/健康判定 + MasqueClient/connBundle 结构体 + 构造器 + Close
- `client_socks5.go`：DialTunnel/establishCONNECT/tunnelConn/releaseStream/streamConn/connectThroughEdge 等 H3 CONNECT 原语
- `client_dns.go`：resolveTarget/resolveDNS/cacheResolution/ResolveDNS + DNS 缓存结构
- `client_doh.go`：dohConn 连接管理/dnsQuery/parseDoHAnswer 等 DoH 全套
- 纯搬迁不动逻辑；MasqueClient/connBundle/dohConn 结构体不拆字段

## Acceptance Criteria

- [ ] `go build ./...` + `GOOS=android CGO_ENABLED=0 go build -tags with_gvisor ./...` 全绿（CI 验证）
- [ ] `go test ./...` 全绿（CI 验证）
- [ ] `go vet ./...` 全绿（CI 验证）
- [ ] tunnel/udp.go 已删除
- [ ] tunnel/masque_socks5_route_test.go 已删除
- [ ] masque.go 行数大幅缩减（从 2219 行降到 0 或极小，符号分散到 4 个新文件）
- [ ] geosite RootDomain 匹配走索引而非线性扫描
- [ ] core.go UpdateGeo 走 Engine.UpdateGeo 而非 NewEngine
- [ ] 静态检查：无未使用的导入/符号

## Notes

- tunnel/ 下文件与上游重叠（AGENTS §6.6），但删死代码不增加冲突面
- 本机无 Go 工具链，build/test 验证靠 CI（push 触发 test job）
- 每个子任务实现后做静态分析（grep/结构检查）确认无悬挂引用
- Conventional Commits；不打 tag（东哥睡了）
