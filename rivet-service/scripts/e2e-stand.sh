#!/bin/sh
# E2E-стенд: чистая база, локальный bare-репозиторий вместо GitHub,
# rivetd с fake SCM и два runner'а с fake-агентом. Процесс остаётся
# на переднем плане (rivetd), Playwright использует его как webServer.
set -eu

SERVICE_DIR=$(cd "$(dirname "$0")/.." && pwd)
STAND_DIR=${E2E_STAND_DIR:-/tmp/rivet-e2e-stand}
HTTP_PORT=${E2E_HTTP_PORT:-8281}
GRPC_PORT=${E2E_GRPC_PORT:-8291}
DB_BASE=${RIVET_DATABASE_URL:-postgres://rivet:rivet@localhost:5432/rivet?sslmode=disable}

rm -rf "$STAND_DIR"
mkdir -p "$STAND_DIR/repos/e2e" "$STAND_DIR/rw-worker" "$STAND_DIR/rw-reviewer"

# Чистая база на прогон: команда нашего CLI, работает без psql.
DB_NAME="rivet_e2e_$$_$(date +%s)"
DB_URL=$(cd "$SERVICE_DIR" && RIVET_DATABASE_URL="$DB_BASE" go run ./cmd/rivet createdb "$DB_NAME")

# Локальный bare-репозиторий с веткой main: клонируется как file://.../e2e/demo.git
BARE="$STAND_DIR/repos/e2e/demo.git"
git init --bare -b main -q "$BARE"
SEED="$STAND_DIR/seed"
git init -b main -q "$SEED"
(cd "$SEED" \
  && git -c user.email=e2e@rivet -c user.name=e2e commit -q --allow-empty -m "seed" \
  && git push -q "$BARE" main)

# Свежая сборка консоли во встроенную статику, затем бинарники.
(cd "$SERVICE_DIR/../rivet-web" && npm run build >/dev/null)
rm -rf "$SERVICE_DIR/internal/webui/dist"
cp -r "$SERVICE_DIR/../rivet-web/dist" "$SERVICE_DIR/internal/webui/dist"
(cd "$SERVICE_DIR" && go build -o "$STAND_DIR/rivetd" ./cmd/rivetd \
  && go build -o "$STAND_DIR/rivet-runner" ./cmd/rivet-runner)

cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# Идентичность стенда: bootstrap-админ (см. ниже env rivetd) и PAT для
# CLI/скриптов — выпускается фоном после старта rivetd в $STAND_DIR/token.
ADMIN_LOGIN="${E2E_ADMIN_LOGIN:-e2e-admin}"
ADMIN_PASSWORD="${E2E_ADMIN_PASSWORD:-e2e-password}"
(
  for _ in $(seq 1 120); do
    curl -sf "http://localhost:$HTTP_PORT/api/v1/health" >/dev/null 2>&1 && break
    sleep 0.5
  done
  curl -sf -c "$STAND_DIR/cookies" -H 'Content-Type: application/json' \
    -d "{\"login\":\"$ADMIN_LOGIN\",\"password\":\"$ADMIN_PASSWORD\"}" \
    "http://localhost:$HTTP_PORT/api/v1/auth/login" >/dev/null \
  && curl -sf -b "$STAND_DIR/cookies" -H 'Content-Type: application/json' \
    -d '{"name":"e2e-stand"}' "http://localhost:$HTTP_PORT/api/v1/tokens" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["secret"])' > "$STAND_DIR/token" \
  || echo "e2e-stand: не удалось выпустить PAT" >&2
) &

# Runner'ы: fake-агент, клонирование из локального bare.
AGENT_CMD="sh $SERVICE_DIR/scripts/fake-agent.sh"
RIVET_PLANE_ADDR="localhost:$GRPC_PORT" RIVET_GIT_BASE="file://$STAND_DIR/repos/" \
  "$STAND_DIR/rivet-runner" -id e2e-worker -agent fake -caps coding \
  -cmd "$AGENT_CMD" -workdir "$STAND_DIR/rw-worker" &
RIVET_PLANE_ADDR="localhost:$GRPC_PORT" RIVET_GIT_BASE="file://$STAND_DIR/repos/" \
  "$STAND_DIR/rivet-runner" -id e2e-reviewer -agent fake -caps coding,review \
  -cmd "$AGENT_CMD" -workdir "$STAND_DIR/rw-reviewer" &

# Control plane на переднем плане.
exec env \
  RIVET_HTTP_ADDR=":$HTTP_PORT" \
  RIVET_GRPC_ADDR=":$GRPC_PORT" \
  RIVET_DATABASE_URL="$DB_URL" \
  RIVET_SCM=fake \
  RIVET_GITHUB_WEBHOOK_SECRET="${E2E_WEBHOOK_SECRET:-e2e-webhook-secret}" \
  RIVET_ADMIN_LOGIN="$ADMIN_LOGIN" \
  RIVET_ADMIN_PASSWORD="$ADMIN_PASSWORD" \
  "$STAND_DIR/rivetd"
