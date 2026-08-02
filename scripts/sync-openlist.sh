#!/usr/bin/env bash
# 同步内置后端 139cas 源码到 openlist/ 目录。
#
# 用法：
#   scripts/sync-openlist.sh              # 拉 main 分支
#   scripts/sync-openlist.sh v4.2.0       # 拉指定 tag/分支
#
# 网络受限时可用镜像：
#   GH_MIRROR=https://ghproxy.net scripts/sync-openlist.sh
set -euo pipefail

REPO="tianjian518/139cas"
REF="${1:-main}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="$ROOT/openlist"
MIRROR="${GH_MIRROR:-}"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fetch() {
  local url="$1"
  echo "  -> $url"
  curl -fsSL --connect-timeout 20 --max-time 300 -o "$TMP/src.tgz" "$url" 2>/dev/null
}

echo "==> 下载 $REPO@$REF"
ok=0
for base in "$MIRROR" "" "https://ghproxy.net" "https://gh-proxy.com"; do
  for kind in heads tags; do
    raw="https://github.com/$REPO/archive/refs/$kind/$REF.tar.gz"
    url="${base:+$base/}$raw"
    if fetch "$url" && file "$TMP/src.tgz" | grep -qi gzip; then ok=1; break 2; fi
  done
done
[ "$ok" = 1 ] || { echo "下载失败：github 不可达，且所有镜像均失败。" >&2; exit 1; }

echo "==> 解包"
tar xzf "$TMP/src.tgz" -C "$TMP"
SRC="$(find "$TMP" -maxdepth 1 -type d -name '139cas-*' | head -1)"
[ -n "$SRC" ] || { echo "解包后未找到源码目录" >&2; exit 1; }

echo "==> 覆盖 $DEST（保留 UPSTREAM.md）"
KEEP="$TMP/UPSTREAM.md"
[ -f "$DEST/UPSTREAM.md" ] && cp "$DEST/UPSTREAM.md" "$KEEP"
rm -rf "$DEST"
cp -r "$SRC" "$DEST"
rm -rf "$DEST/.github" "$DEST/public/dist" "$DEST/bin" "$DEST/data"
[ -f "$KEEP" ] && cp "$KEEP" "$DEST/UPSTREAM.md"

echo "==> 完成。请检查 git diff 后提交。"
