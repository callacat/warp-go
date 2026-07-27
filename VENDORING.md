# Vendoring: metacubexd 控制面板前端

P3（GitHub issue #7）把 [metacubexd](https://github.com/MetaCubeX/metacubexd) 控制面板
前端经 `//go:embed` 嵌进 warp-go 二进制。metacubexd 产物**不入 git**——155 文件 / 7.56 MiB，
每次上游升级内容都变，入仓会污染 diff 且放大仓库体积。本文件记录产物的供应链锁定、来源归属、
复现步骤与回滚路径。

## 供应链锁定

| 项 | 值 |
|---|---|
| 上游仓库 | https://github.com/MetaCubeX/metacubexd |
| 上游许可证 | MIT（见仓库 LICENSE） |
| 版本（tag） | `v1.270.5` |
| Release asset URL | https://github.com/MetaCubeX/metacubexd/releases/download/v1.270.5/compressed-dist.tgz |
| Release asset sha256 | `80236c8fcc36a015741e7e279280ccbcf5eb62e9857f757d36eecf370ee04ff9` |
| 对应 gh-pages commit | `b2a64d789f1fc441addd4b3d3b8313f4978f08ab` |
| 产物规模 | release tarball 解包 155 文件（根 14 + `_nuxt/` 109 JS+CSS + `_fonts/` 32 woff2），未压缩约 7.56 MiB；tgz 2,518,861 bytes（约 2.40 MiB）。gh-pages tree 同版 API 报 157 文件，多出的 2 是 GitHub Pages 专属的 `.nojekyll` + `CNAME`，不在 release tarball，P3 以 release tarball 为源故落地 155。 |
| 顶层布局 | `index.html` / `_nuxt/`(109 JS+CSS) / `_fonts/`(32 woff2) / `config.js` / `200.html` / `404.html` / `sw.js` / `workbox-*.js` / 图标 / `manifest.webmanifest` |
| 落地目录 | `frontproxy/frontui/assets/metacubexd/`（被 `.gitignore` 忽略产物，保留占位） |

## 复现

```bash
# 在仓库根：
bash scripts/vendor-metacubexd.sh
```

脚本动作：`curl` 拉 `compressed-dist.tgz` → `sha256sum` 比对 pin 在脚本顶部的常量 →
清空 `frontproxy/frontui/assets/metacubexd/` → `tar -xzf` 解包到位。幂等，可重复跑。

跑完 `go test ./frontproxy/frontui/ -run TestDistFS` 应全绿（`TestDistFS_FileCountMeetsFloor`
断言 ≥ 100 文件；v1.270.5 实测 155）。

## 为什么不运行时拉取

- **避开 2025-06 GitHub 黑名单风险**（#1 spec US25 + #7 acceptance）：metacubexd 仓库在
  2025-06 处于 GitHub 黑名单风险期。运行时拉取会让 warp-go 二进制依赖运行时 GitHub 可达，
  黑名单触发即面板不可用。`//go:embed` 把全部资源打进二进制，运行时零外网依赖。
- **保留单二进制形态**（#7 acceptance）：不引外置静态资源目录依赖，一个 `warp` 二进制自包含。
- **mihomo 默认下载的隐患**：mihomo `DefaultRawConfig()` 把 `external-ui-url` 默认设为
  `metacubexd gh-pages.zip` URL；frontrender 显式渲染 `external-ui-url: ""` 抹掉默认，避免
  mihomo 的 `AutoDownloadUI` 在目录被清空时去 GitHub 下载覆盖 embed 资源。

## 回滚 / 降级

- **回到占位态**：`git clean -xdf frontproxy/frontui/assets/metacubexd/` 后 `git checkout --
  frontproxy/frontui/assets/metacubexd/` 恢复占位 index.html + .gitkeep（占位被 git 跟踪，
  产物被 .gitignore 忽略，clean 不会删占位）。占位态可编译可起空面板，但 `TestDistFS_FileCountMeetsFloor`
  会红——这是预期的"未 vendor"信号。
- **升级 metacubexd 版本**：改 `scripts/vendor-metacubexd.sh` 顶部的 `VERSION` /
  `EXPECTED_SHA256` / `GHPAGES_COMMIT` 三常量 + 本表格对应行，重跑脚本，重跑测试。

## 归属

metacubexd 由 [MetaCubeX](https://github.com/MetaCubeX) 以 MIT 许可证发布。warp-go 仓库
不修改上游产物，仅作 vendored embed 使用。产物来源的 commit 与 sha256 见上表，可独立复核。
