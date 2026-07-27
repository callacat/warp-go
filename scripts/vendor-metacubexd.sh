#!/usr/bin/env bash
# scripts/vendor-metacubexd.sh
#
# P3 (#7)：把 metacubexd 控制面板前端产物拉取、校验、解包到
# frontproxy/frontui/assets/metacubexd/，让 //go:embed all:assets/metacubexd
# 把真面板打进 warp-go 二进制。
#
# 为什么需要这个脚本：metacubexd 产物（v1.270.5, 155 文件 / 7.56 MiB，release tarball
# 解包实测；gh-pages tree 同版 API 报 157，多出的 2 是 GitHub Pages 专属 .nojekyll +
# CNAME，不在 release tarball）不入 git（见 VENDORING.md 与 .gitignore），fresh clone
# 后 assets/metacubexd/ 只有占位 index.html。跑本脚本一次即把真产物覆盖上位，go
# build/test 才能 embed 真面板。
#
# 避开 2025-06 GitHub 黑名单风险：本脚本只在构建期拉取一次，运行时的 warp-go 二进制
# 经 //go:embed 自带全部资源，不依赖运行时访问 GitHub（见 #1 spec US25 + #7 acceptance）。
set -euo pipefail

# ---- 供应链锁定（pin 在脚本顶部，升级 metacubexd 时改这里） -------------
VERSION="v1.270.5"
DIST_URL="https://github.com/MetaCubeX/metacubexd/releases/download/${VERSION}/compressed-dist.tgz"
EXPECTED_SHA256="80236c8fcc36a015741e7e279280ccbcf5eb62e9857f757d36eecf370ee04ff9"
GHPAGES_COMMIT="b2a64d789f1fc441addd4b3d3b8313f4978f08ab"
# ------------------------------------------------------------------------

# 仓库根 = 本脚本所在 scripts/ 的上一级。
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEST_DIR="${REPO_ROOT}/frontproxy/frontui/assets/metacubexd"

echo "[vendor-metacubexd] 拉取 metacubexd ${VERSION}"
echo "[vendor-metacubexd]   URL:    ${DIST_URL}"
echo "[vendor-metacubexd]   sha256: ${EXPECTED_SHA256}"
echo "[vendor-metacubexd]   目标:   ${DEST_DIR}"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

TGZ="${TMPDIR}/compressed-dist.tgz"
echo "[vendor-metacubexd] 下载 ..."
curl -fsSL --max-time 120 -o "${TGZ}" "${DIST_URL}"

echo "[vendor-metacubexd] 校验 sha256 ..."
ACTUAL_SHA256="$(sha256sum "${TGZ}" | awk '{print $1}')"
if [[ "${ACTUAL_SHA256}" != "${EXPECTED_SHA256}" ]]; then
  echo "[vendor-metacubexd] sha256 不匹配！" >&2
  echo "  expected: ${EXPECTED_SHA256}" >&2
  echo "  actual:   ${ACTUAL_SHA256}" >&2
  exit 1
fi
echo "[vendor-metacubexd] sha256 OK"

# 解包前清空目标目录，保留 .gitkeep 占位。tar 归档不含 .gitkeep —— 全删会让 git
# status 出现 tracked-deletion 噪声，故用 ! -name .gitkeep 把它排除在删除之外，
# 其 tracked 内容原样保留，解包后 git status 不显示 .gitkeep 的任何变更。
echo "[vendor-metacubexd] 清空目标目录 ..."
# 先确保目录存在（fresh clone 后该目录因 .gitkeep 被追踪而已建，此处为兜底）。
# 再删除其中除 .gitkeep 外的全部条目（占位 index.html + 上次 vendor 残留）。
# 关键：不吞错 —— 去掉 2>/dev/null / || true，find 返非 0 时 set -e 短路，脚本
# 停在解包前，杜绝半清空后 tar 覆盖式解包把旧产物与新产物混嵌入二进制（混合版本
# embed）。占位 index.html 在此被删，由下方 tar 真产物覆盖（符合工作树真产物、
# 索引占位的双态策略）。本脚本幂等可重复跑：再跑一次只重删同一批残留，.gitkeep
# 恒在、永不被删。
mkdir -p "${DEST_DIR}"
find "${DEST_DIR}" -mindepth 1 ! -name .gitkeep -delete

# compressed-dist.tgz 由 `tar czf ... -C packages/ui/.output/public .` 制作，
# 归档根部直接是 index.html / _nuxt/ / _fonts/，无外层仓库目录，直接解包到目标即可。
echo "[vendor-metacubexd] 解包 ..."
tar -xzf "${TGZ}" -C "${DEST_DIR}"

# 成功不变量：解包后 dest 含 index.html。
if [[ ! -s "${DEST_DIR}/index.html" ]]; then
  echo "[vendor-metacubexd] 解包后 ${DEST_DIR}/index.html 缺失或空 —— 产物异常" >&2
  exit 1
fi

FILE_COUNT="$(find "${DEST_DIR}" -type f | wc -l)"
echo "[vendor-metacubexd] 完成：${FILE_COUNT} 文件落到 ${DEST_DIR}"
echo "[vendor-metacubexd] 占位文件已被真产物覆盖；git 不会跟踪这些产物（见 .gitignore）。"
echo "[vendor-metacubexd] 下一步：go test ./frontproxy/frontui/ -run TestDistFS 应全绿。"
