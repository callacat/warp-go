#!/usr/bin/env bash
# 清理过期的 GitHub Actions 运行记录与缓存
#
# 规则：
#   - 删除超过 30 天的 workflow 运行记录
#   - 始终保留最新的 20 次运行（按创建时间倒序，前 20 条跳过）
#   - 顺带清理超过 30 天的 Actions 缓存（可选）
#   - 幂等：没有可清理项时安全退出（exit 0）
#
# 依赖：gh CLI + GITHUB_TOKEN（需要 actions: write 权限）
# 用法：bash .github/workflows/scripts/cleanup-runs.sh
set -euo pipefail

REPO="${GITHUB_REPOSITORY:-}"
if [ -z "$REPO" ]; then
  echo "::error::GITHUB_REPOSITORY 环境变量未设置，无法确定仓库"
  exit 1
fi

if ! command -v gh >/dev/null 2>&1; then
  echo "::error::未找到 gh CLI，跳过清理"
  exit 1
fi

# 30 天前的 Unix 时间戳（秒）
CUTOFF=$(date -d '30 days ago' +%s)
echo "::group::清理过期 workflow 运行记录（保留最新 20 次）"
echo "仓库: ${REPO} | 阈值: $(date -d "@${CUTOFF}" '+%Y-%m-%d %H:%M:%S')"

total_deleted=0
while :; do
  # 分页取最新 100 条（gh run list 按创建时间倒序返回；--limit 100 循环直至无可删项）
  mapfile -t runs < <(gh run list --limit 100 --json databaseId,createdAt \
    --jq '.[] | "\(.databaseId) \(.createdAt)"' 2>/dev/null || true)

  if [ "${#runs[@]}" -eq 0 ]; then
    echo "没有可清理的运行记录"
    break
  fi

  round_deleted=0
  index=0
  for entry in "${runs[@]}"; do
    index=$((index + 1))

    # 最新的 20 次运行一律保留（跳过前 20 条）
    if [ "$index" -le 20 ]; then
      continue
    fi

    id="${entry%% *}"
    created_iso="${entry##* }"
    created_epoch=$(date -d "${created_iso}" +%s 2>/dev/null || echo 0)

    # 仅删除超过 30 天的运行
    if [ "$created_epoch" -gt 0 ] && [ "$created_epoch" -lt "$CUTOFF" ]; then
      echo "删除过期运行 #${id}（$(date -d "@${created_epoch}" '+%Y-%m-%d %H:%M:%S')）"
      if gh run delete "$id" >/dev/null 2>&1; then
        total_deleted=$((total_deleted + 1))
        round_deleted=$((round_deleted + 1))
      else
        echo "::warning::删除运行 #${id} 失败（权限不足或运行仍在进行）"
      fi
    fi
  done

  # 一轮未删除任何记录 = 已收敛，结束循环
  if [ "$round_deleted" -eq 0 ]; then
    break
  fi

  # 防御性上限，避免意外死循环
  if [ "$total_deleted" -ge 5000 ]; then
    echo "::warning::单次清理已达 5000 条上限，提前结束"
    break
  fi
done

echo "共清理 ${total_deleted} 条过期运行记录"
echo "::endgroup::"

# ---- 可选：清理超过 30 天的 Actions 缓存 ----
echo "::group::清理过期 Actions 缓存"
cache_deleted=0
while IFS= read -r entry; do
  [ -z "$entry" ] && continue
  cid="${entry%% *}"
  ccreated="${entry##* }"
  cepoch=$(date -d "${ccreated}" +%s 2>/dev/null || echo 0)
  if [ "$cepoch" -gt 0 ] && [ "$cepoch" -lt "$CUTOFF" ]; then
    echo "删除过期缓存 #${cid}（${ccreated}）"
    if gh api -X DELETE "repos/${REPO}/actions/caches/${cid}" >/dev/null 2>&1; then
      cache_deleted=$((cache_deleted + 1))
    else
      echo "::warning::删除缓存 #${cid} 失败"
    fi
  fi
done < <(gh api "repos/${REPO}/actions/caches" --paginate \
  --jq '.actions_caches[] | "\(.id) \(.created_at)"' 2>/dev/null || true)
echo "共清理 ${cache_deleted} 个过期缓存"
echo "::endgroup::"

echo "清理完成"
