#!/bin/sh
# Встроенная консоль rivetd (internal/webui/dist) и коммит rivet-web, из
# которого она собрана (internal/webui/REVISION, полный SHA одной строкой).
# Design fix-console-rivetd-tails, решение 4.
#
#   sh scripts/webui-dist.sh check        # CI: dist совпадает со сборкой REVISION
#   sh scripts/webui-dist.sh sync [ref]   # make ui-sync: собрать ref (по умолчанию
#                                         # main) и записать dist и REVISION
#
# Коммит обязан быть в main rivet-web: встраивается только влитая консоль.
# Сборка rivet-web детерминирована, сверка побайтная. Node той же версии,
# что в CI rivet и rivet-web (24). Репозиторий консоли можно подменить:
# RIVET_WEB_REPO=../rivet-web (форк, работа без сети).
set -eu

SERVICE_DIR=$(cd "$(dirname "$0")/.." && pwd)
DIST="$SERVICE_DIR/internal/webui/dist"
REVISION="$SERVICE_DIR/internal/webui/REVISION"
REPO=${RIVET_WEB_REPO:-https://github.com/PavluninVladimir/rivet-web.git}
NODE_MAJOR=24

fail() { echo "webui-dist: $*" >&2; exit 1; }
notice() {
  if [ "${GITHUB_ACTIONS:-}" = "true" ]; then echo "::notice::$*"; else echo "webui-dist: $*"; fi
}

cmd=${1:-}
case "$cmd" in
  check|sync) ;;
  *) fail "использование: $0 check | sync [ветка или коммит rivet-web]" ;;
esac

node_major=$(node -p 'process.versions.node.split(".")[0]' 2>/dev/null) || fail "нужен Node $NODE_MAJOR и npm"
[ "$node_major" = "$NODE_MAJOR" ] || notice "Node $node_major, а сборка сверяется под Node $NODE_MAJOR (как в CI): при расхождении проверьте версию"

TMP=$(mktemp -d "${TMPDIR:-/tmp}/rivet-webui.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM
WEB="$TMP/rivet-web"
git clone --quiet --filter=blob:none --no-checkout "$REPO" "$WEB" 2>"$TMP/clone.log" \
  || { cat "$TMP/clone.log" >&2; fail "не удалось клонировать $REPO"; }

# Коммит есть в main rivet-web; сколько коммитов main впереди.
in_main() {
  git -C "$WEB" merge-base --is-ancestor "$1" origin/main 2>/dev/null
}

build() { # $1 — SHA
  git -C "$WEB" checkout --quiet "$1"
  (cd "$WEB" && npm ci --no-audit --no-fund && npm run build) >"$TMP/build.log" 2>&1 \
    || { tail -n 40 "$TMP/build.log" >&2; fail "сборка rivet-web $1 не прошла"; }
}

if [ "$cmd" = check ]; then
  [ -f "$REVISION" ] || fail "нет $REVISION: выполните make ui-sync"
  rev=$(tr -d ' \t\r\n' <"$REVISION")
  echo "$rev" | grep -Eq '^[0-9a-f]{40}$' || fail "в REVISION должен быть полный SHA коммита rivet-web, а там «${rev}»"
  git -C "$WEB" cat-file -e "$rev^{commit}" 2>/dev/null || fail "коммита $rev нет в $REPO"
  in_main "$rev" || fail "коммит $rev не влит в main rivet-web: встраивается только консоль из main (make ui-sync)"
  behind=$(git -C "$WEB" rev-list --count "$rev..origin/main")
  [ "$behind" = 0 ] || notice "встроенная консоль отстаёт от main rivet-web на $behind коммит(ов): обновите её через make ui-sync"
  build "$rev"
  if ! diff -r "$WEB/dist" "$DIST" >"$TMP/diff.log" 2>&1; then
    head -n 40 "$TMP/diff.log" >&2
    fail "internal/webui/dist не совпадает со сборкой rivet-web $rev. Не правьте dist руками и не коммитьте сборку make ui или e2e-стенда: обновите консоль командой make ui-sync (WEB_REF=<коммит main rivet-web>) и закоммитьте dist, REVISION, NOTICE и THIRD_PARTY"
  fi
  echo "webui-dist: internal/webui/dist совпадает со сборкой rivet-web $rev"
  exit 0
fi

ref=${2:-main}
sha=$(git -C "$WEB" rev-parse --verify --quiet "origin/$ref^{commit}" || git -C "$WEB" rev-parse --verify --quiet "$ref^{commit}") \
  || fail "в $REPO нет ветки или коммита «${ref}»"
in_main "$sha" || fail "коммит $sha ($ref) не влит в main rivet-web: встраивается только консоль из main. Для локальной проверки ветки есть make ui"
build "$sha"
rm -rf "$DIST"
cp -R "$WEB/dist" "$DIST"
echo "$sha" >"$REVISION"
echo "webui-dist: internal/webui/dist собран из rivet-web $sha, REVISION обновлён"
