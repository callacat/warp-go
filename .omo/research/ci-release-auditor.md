# warp-go CI / 发布 / 工作流审计报告

> 审计人：ci-release-auditor（码农团队）
> 日期：2026-08-15
> 范围：`.github/workflows/` 全部 4 个工作流 + `scripts/cleanup-runs.sh` + 版本号规则（version.go / versioninfo.json / CHANGELOG.md）+ AGENTS.md 发布纪律
> 结论均基于真实读文件 / 真实执行验证，未改动任何代码或工作流。

---

## 0. 现状总览

warp-go 的 CI/发布体系整体**健康、纪律性强、注释详尽**。东哥的发布习惯（semver、单源版本、Conventional Commits、冲突即停、发布前跑测试、发布完成 = CI 全绿 + 产物可验证）大部分已落地并有 AGENTS.md 明确记录（§6.8 + 血泪教训）。但存在 **2 个真实 bug** 和若干效率/健壮性改进点，详情如下。

---

## 1. 各工作流评估表

### 1.1 `.github/workflows/build-release.yml`（构建与发布 — 核心）

| 维度 | 现状 | 评价 |
|---|---|---|
| 触发 | `push tags v*` + `workflow_dispatch` | OK；tag 触发发版，dispatch 手动 |
| job 依赖 | `test` → `build-binary` / `build-gui` / `build-android` → `release` | 合理分层 |
| 平台矩阵 | CLI：linux/darwin(amd64+arm64)+windows amd64 = 5 平台（L56-65）；GUI：win/darwin/linux 各 1 | CLI 覆盖全，**GUI 全部只产 amd64**（见 §5.1） |
| Android | JDK21 + SDK android-35 + NDK 26.3 + c-shared(arm64/x86_64) + gradle APK/AAB（L219-390） | 完整，含 JNI 双侧 grep 断言 |
| 校验和 | `sha256sums.txt`（L410-416） | OK |
| Release | `gh release create ... --generate-notes`（L423） | generate-notes 默认按 Conventional Commits 识别 |
| 清理 | `cleanup-runs.sh`（L425-429） | OK |
| 风险 | `continue-on-error` 使 GUI/Android 失败不标红整次 run（L116/L221）；无 Go 缓存；无 concurrency | 见 §4/§5 |

**test job（L20-47）**：`go vet` → `go build` → `GOOS=android CGO_ENABLED=0 build` → `go test`。注意 `go build ./...` 与随后的 `go test ./...` 重复编译一遍（`go test` 自己会 build+vet），可省一步。

### 1.2 `.github/workflows/docker-ghcr.yml`（Docker → GHCR）

| 维度 | 现状 | 评价 |
|---|---|---|
| 触发 | main push(latest) + tag v*(semver) + dispatch（L8-12） | OK |
| 平台 | linux/amd64 + arm64 经 QEMU（L56） | 覆盖目标 |
| 版本注入 | **无** — Dockerfile L8 `-ldflags="-s -w"` 未 `-X main.version` | **违反单源版本**，镜像恒为 `dev`（§3.2） |
| tag 元数据 | `type=semver,pattern={{version}}`（L50） | 正确剥 v |

### 1.3 `.github/workflows/sync-upstream.yml`（双上游同步）

| 维度 | 现状 | 评价 |
|---|---|---|
| 触发 | 每日 04:00 UTC + dispatch（L8-9） | OK |
| 双上游 | badafans 先 → 6Kmfi6HP 后（L156-161） | 符合 AGENTS §6.6 |
| 冲突即停 | `git merge --abort` + `conflict ⚠️` 标签 + issue + exit 1（`handle_conflict` L68-100） | **原则正确，绝不自动解决** ✓ |
| 预检测 | `git merge-tree` 3 参经典形式（`precheck_conflict` L107-148） | **存在 regex bug（§2.1）** |
| 无新提交 | `merge-base --is-ancestor` 早退（L48-54） | 高效 |
| 自动合并 | `gh pr merge --auto`（L188-189），未开启 auto-merge 仅告警 | 符合"冲突即停，CI 绿自动合并" |

### 1.4 `.github/workflows/android-debugdiag.yml`（手动调试包）

| 维度 | 现状 | 评价 |
|---|---|---|
| 触发 | 仅 workflow_dispatch | 符合用途（诊断用） |
| 结构 | 与 build-release 的 build-android 几乎完全重复（~160 行复制） | **可并用 composite action 消除重复（§5.3）** |
| tag | `-tags ...,debugdiag` + `debugdiag_stub.go` no-op 互斥 | 正确（v0.5.26） |
| 校验 | JNI grep 断言（含 `DebugSetDir` 收集器断言 L140-141） | 完整 |

### 1.5 `scripts/cleanup-runs.sh`

- 删 30 天前 runs，保留最新 20（L46-57）；顺带清 30 天前缓存（L82-101）
- 幂等、分页、5000 上限防死循环（L73-76）——设计稳健，`gh run list --limit 100` 分页收敛
- 低风险点：遍历是 `gh run list` 倒序 + `--limit 100` 循环，若 30 天内恰好 > 100 条，每轮重复拉同 100 条直到收敛，仍正确（只删 >30 天的），仅性能上多轮。可接受。

---

## 2. 发现的真实 Bug

### 2.1 【中】sync-upstream 冲突**预检测**逻辑失效（死代码）

`sync-upstream.yml` L121：
```bash
if ! printf '%s' "$out" | grep -qE '^(<<<<<<<|>>>>>>>)'; then
  echo "  ${upstream} 预检测：无冲突"
  return 0
fi
```
经典 3 参 `git merge-tree <base> <b1> <b2>` 的冲突输出中，冲突标记行**带 `+` 前缀**（实测输出 `+<<<<<<< .our` / `+=======` / `+>>>>>>> .their`）。已在本机 git 2.47.3 用真实冲突分支验证：

```
=== check line-121 regex ^(<<<<<<<|>>>>>>>) ===
0    # grep 0 命中 → exit 1 → 走进"无冲突"分支
```

即 `^(<<<<<<<|>>>>>>>)` 永远无法匹配 `+<<<<<<<`，**预检测恒判定"无冲突"并 return 0**，L125 的 `^\+<<<<<<<` 冲突文件提取成了不可达死代码。

影响分级：**安全未受损**——真正的守卫仍是 L156-161 的 `git merge` + `handle_conflict`（它们能正确 `--abort`、打标签、建 issue、exit 1）。失效的只是 L102 注释宣称的"提前中止、避免开 PR 才发现"的**预检测优化**。但 docstring 与实际行为矛盾，会给后续维护者错误预期。

顺带：新式 `git merge-tree --write-tree`（git 2.38+）冲突时**退出码为 1** 且输出树哈希，与脚本假设（"经典 3 参形式退出码恒 0/1"）完全不同。若上游 runner 的 git 升级、或有人"顺手"换成 `--write-tree`，整个预检测得到的结果反而不对。建议要么修 regex，要么换 `--write-tree` + 退出码判冲突并显式钉 git 版本。

### 2.2 【中】Windows CLI PE 版本资源恒为陈旧 `0.5.3`（版本单源被破坏）

`build-release.yml` CLI windows 分支（L94-96）：
```bash
if [ "$VERSION" != "dev" ]; then
  sed -i "s/0\.0\.0\.0/${VERSION}/g" versioninfo.json
fi
```
但**根目录 `versioninfo.json` 实际写死的是 `0.5.3` 而非 `0.0.0.0`**（`versioninfo.json:25,32` = `"FileVersion": "0.5.3"` / `"ProductVersion": "0.5.3"`）。sed 的 `0\.0\.0\.0` 永不命中 → **每次 tag 发版 Windows CLI 的 PE 版本资源都烘焙成 `0.5.3`**，与真实 tag（如 0.5.27）不符。（真值 `-X main.version` 注入仍正确，仅 PE 资源/`go version -m` 之外的资源管理器"详细信息"显示陈旧。）

对照：**GUI 侧**（`gui/versioninfo.json:25,32` = `0.0.0.0` 占位符）的 sed 能正确匹配替换 → GUI PE 版本正常。问题只出在根目录 CLI 的 versioninfo.json（被某次发布 `sed -i` 原地改成了 `0.5.3` 且未还原为占位符，或初始就写错占位符）。

修复方向：根 `versioninfo.json` 的 FileVersion/ProductVersion 应改回 `0.0.0.0` 占位符（与 gui 侧一致），双保险再对主版本段（FixedFileInfo）也加替换。

---

## 3. 版本号 / 单源管理对标

| 项目 | 现状 | 结论 |
|---|---|---|
| CLI 版本注入 | `version.go:5` 默认 `dev`；CI `-ldflags "-X main.version"` 注入（build-release L100） | ✅ 符合"tag 优先，CI 注入" |
| GUI 版本注入 | build-gui L194 / build-android L309 c-shared 注入 | ✅ |
| Android versionCode | build.gradle L29 `major*10000+minor*100+patch`，`VERSION_NAME` 由 CI 注入（build-release L378） | ✅ 单调递增 |
| **Windows CLI PE 资源** | 根 `versioninfo.json` 写死 `0.5.3`，sed 找不到 `0.0.0.0` → 陈旧 | ❌ **违反单源（§2.2）** |
| **Docker 版本** | Dockerfile L8 `-ldflags="-s -w"` **无 `-X main.version`** → 无论 tag 与否镜像内二进制恒 `dev` | ❌ 违反单源 |
| CHANGELOG | `[v0.5.x] - 日期` + Unreleased，Keep a Changelog 结构、semver 分类（修复/变更/新增/测试/文档） | ✅ 规范 |
| changelog 未发布条目 | v0.5.27 修复已写入 Unreleased，但该条目标注"未解决放弃" | ✅ 诚实 |
| 发布顺序 | AGENTS §6.8.4：修 bug → push main + tag 同推 → 等 CI 全绿 → 验证产物 | ✅ 文档化；未见自动化门（无 workflow 强制先更 CHANGELOG 再允许 tag） |

**版本单一来源 = release tag** 的左侧通道已基本闭合，但 Windows CLI PE 与 Docker 两个出口漏了 `-X` 注入 / 占位符还原。

---

## 4. 发布流程对标（东哥习惯逐条）

| 东哥习惯 | warp-go 现状（文件） | 符合/违反 |
|---|---|---|
| semver 版本号 | `[v0.5.x]`，tag `v*` 语义化 | ✅ |
| 版本单源（tag 优先，CI 注入 build） | CLI/GUI/Android 注入 ✅；Windows CLI PE 陈旧 ❌；Docker 恒 dev ❌（§3） | @部分违反 |
| 发版前先更 CHANGELOG 再打 tag | AGENTS §6.8 流程，无脚本/门强制 | @靠纪律，无自动化 |
| Conventional Commits | Release `--generate-notes`；提交信息复现均为 `docs:/feat:/fix:` 风格 | ✅ |
| CI 沿用既有 Actions；冲突即停绝不自动解决 | sync-upstream `handle_conflict` abort + label + issue | ✅（安全侧） |
| 打 tag 前问东哥确认 | 属协作约定，非 workflow 内实现 | n/a |
| 发布完成 = CI 全绿 + 产物可验证 | AGENTS §6.8.1 明确 + Release assets 齐全清单 | ✅ |
| 提交前跑构建测试 | test job 含 vet/build/test + android 交叉编译门 | ✅ |
| CI 纪律（构建不会因 GUI/Android 失败阻断 CLI） | `continue-on-error`（L116/L221）**刻意**代价 = run 可能整体绿而 GUI/Android 红 | ⚠️ 见 §5.2 |

---

## 5. 改进建议（按优先级）

### P0 — 修复（影响正确性/真实版本）

- **R1** 修 `sync-upstream.yml` L121 regex：`^(<<<<<<<|>>>>>>>)` → `^\+?<<<<<<<|^\+?>>>>>>>`（或直接对齐 L125 的 `^\+...`），恢复冲突预检测。同时**钉 git 版本**或改用 `git merge-tree --write-tree`（冲突退出码 1），避免 runner git 升级后行为漂移。文件：`sync-upstream.yml`。
- **R2** 根 `versioninfo.json` FileVersion/ProductVersion 改回 `0.0.0.0` 占位符，让 build-release L95 的 sed 命中性恢复（每周发版都会用到）。文件：`versioninfo.json`。
- **R3** Dockerfile L8 `-ldflags` 增加 `-X main.version=${VERSION}`（docker-ghcr 从 tag 提取；main 分支用 `dev`），让 GHCR 镜像产物带真版本。文件：`Dockerfile` + `docker-ghcr.yml`。

### P1 — 效率（每次 tag 发版省分钟级）

- **R4 加 Go 模块缓存**：4 处 `actions/setup-go` 加 `cache: true`（build-release 的 test/build-binary/build-gui/build-android + android-debugdiag），`go.sum` 变化才失效。当前无任何 Go 缓存，每次 v* 全量重编依赖，是最大耗时项。
- **R5 前端缓存**：`actions/setup-node` 加 `cache: 'npm'` + `cache-dependency-path: gui/frontend/package-lock.json`（package-lock.json 已存在，见 `gui/frontend/`）。`npm ci || npm install` 的兜底可保留。
- **R6 test job 去重**：`go test ./...` 已含构建，删掉 test job 里独立的 `go build ./...`（build-release L35-36），少一遍全量编译。

### P1 — 可靠性

- **R7 加 `concurrency` group**（按 ref）：`build-release` / `docker-ghcr` 加 `group: ${{ github.ref }}-${{ github.workflow }}` + `cancel-in-progress: true`，防 force-push 同 tag 或连续 dispatch 的重叠运行（AGENTS §6.8.4 场景）。
- **R8 失败通知**：当前零通知，完全靠人盯 `gh run view`。可在 4 个工作流加失败 step（`if: failure()`，可选 Telegram/DingTalk webhook 或 `repo_dispatch`），或至少给 `android-debugdiag` / `sync-upstream` 冲突时已建 issue 作通知（已有）。发布完成判定仍以 §6.8.1 为准。

### P2 — 覆盖面 / 体验

- **R9 macOS GUI 缺 arm64**：`build-gui` 的 darwin runner 现在 `GOARCH=amd64`（build-release L205），Apple Silicon 用户须 Rosetta。建议 macOS GUI 增 arm64 目标（runner 本身 arm64，直接 `GOARCH=arm64` 即原生），或至少归档时标注解。
- **R10 Android 构建去重**：`android-debugdiag.yml` 与 `build-release.yml` 的 build-android 有 ~160 行重复（NDK 安装 / bindings / c-shared ×2 / grep 断言）。可抽 composite action（`dist/.github/composite-action/android-build/action.yml`，参数 `debugdiag:true|false`），避免两处漂移（NDK 版本改一处漏另一处的风险已在 §1.4 体现）。
- **R11 NDK 版本文档对齐**：`build-release.yml:227` / `android-debugdiag.yml:20` 用 `26.3.11579264`(=r26c)，而 AGENTS.md L117/L132/L201 反复写 "NDK r27"。文档与实现不一，二选一对齐。

### P3 — 细节

- **R12 cleanup 分页小优化**：`cleanup-runs.sh` 30 天内 runs 超过 `--limit 100` 时每轮重复拉前 100 条（只删 >30 天），可加 `--page` 前进或按已删 id 过滤，避免 O(1000) 轮询（低危，现收敛正确）。
- **R13 `en route` 线程安全**：无。
- **R14 tag 推送双触发**：同一 tag 会同时触发 build-release + docker-ghcr 两个独立 Go 编译（docker 在容器内另编一份）。可接受（产物形态不同），仅计耗。

---

## 6. 结论摘要（供汇报）

- **CI 现状健康**：分层清晰、注释详尽、发布纪律文档化（AGENTS §6.8）、Android 真实 cgo/JNI 断言防住 v0.5.7 式血泪、冲突即停安全侧可靠。
- 需优先修的 **2 个真实 bug**：① sync-upstream 冲突**预检测 regex 失效**（`^(<<<<<<<)` 匹配不到带 `+` 前缀的 `+<<<<<<<`，预检测恒判"无冲突"成死代码，但真实 `git merge` 守卫仍安全）；② Windows CLI PE 版本资源因根 `versioninfo.json` 写死 `0.5.3` 而恒陈旧（sed 找 `0.0.0.0` 落空）。
- 另两个版本单源漏洞：Docker 镜像不注入 `-X main.version` 恒 `dev`；GUI 全家只有 amd64（mac arm64 缺原生）。
- 效率：无任何 Go/npm 缓存是最大耗时源；补 `cache: true` 即可。
- **明显违反发布纪律处**：无（纪律在 AGENTS §6.8 已内化）；仅 4 个"版本单源漏出口"（Windows PE / Docker）和 0 通知算短板。
