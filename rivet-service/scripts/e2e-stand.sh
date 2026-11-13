#!/bin/sh
# E2E-стенд: чистая база, локальный bare-репозиторий вместо GitHub,
# rivetd с fake SCM, fake-llm вместо провайдера модели и runner'ы агента
# Rivet поверх него (add-native-agent): агентский цикл настоящий, модель —
# детерминированные сценарии fake-llm. Токен регистрации и подключение к
# fake-llm создаются через API после старта rivetd. Процесс остаётся на
# переднем плане (rivetd), Playwright использует его как webServer.
# Fake-провайдер и fake SCM — только тестовая инфраструктура: в консоли и
# пресетах продукта их нет. Так же устроены fake RUVDS (cmd/fake-ruvds) и
# подмены ssh и docker для runner'а выкатки (scripts/e2e-bin,
# add-environment-provisioning): настоящих серверов, денег и Docker стенду
# не нужно.
set -eu

SERVICE_DIR=$(cd "$(dirname "$0")/.." && pwd)
STAND_DIR=${E2E_STAND_DIR:-/tmp/rivet-e2e-stand}
HTTP_PORT=${E2E_HTTP_PORT:-8281}
GRPC_PORT=${E2E_GRPC_PORT:-8291}
DB_BASE=${RIVET_DATABASE_URL:-postgres://rivet:rivet@localhost:5432/rivet?sslmode=disable}

rm -rf "$STAND_DIR"
mkdir -p "$STAND_DIR/repos/e2e"

# Чистая база на прогон: команда нашего CLI, работает без psql.
DB_NAME="rivet_e2e_$$_$(date +%s)"
# Non-secret identity for guarded E2E fixtures; never expose DB_URL/credentials.
printf '%s\n' "$DB_NAME" > "$STAND_DIR/database-name"
DB_URL=$(cd "$SERVICE_DIR" && RIVET_DATABASE_URL="$DB_BASE" go run ./cmd/rivet createdb "$DB_NAME")

# Локальный bare-репозиторий с веткой main: клонируется как file://.../e2e/demo.git
BARE="$STAND_DIR/repos/e2e/demo.git"
git init --bare -b main -q "$BARE"
SEED="$STAND_DIR/seed"
git init -b main -q "$SEED"
(cd "$SEED" \
  && git -c user.email=e2e@rivet -c user.name=e2e commit -q --allow-empty -m "seed" \
  && git push -q "$BARE" main)

# Репозиторий сценариев архитектуры e2e/arch: README и корректное описание
# демо-CRM (internal/architecture/testdata/crm) на основной ветке. Для
# него (и для e2e/gitops ниже) включён git-режим fake SCM
# (RIVET_FAKE_GIT_REPOS): снимок архитектуры читается из настоящей версии,
# остальные сценарии работают на e2e/demo в прежнем режиме.
ARCH_BARE="$STAND_DIR/repos/e2e/arch.git"
git init --bare -b main -q "$ARCH_BARE"
ARCH_SEED="$STAND_DIR/seed-arch"
git init -b main -q "$ARCH_SEED"
mkdir -p "$ARCH_SEED/docs/architecture"
cp -R "$SERVICE_DIR/internal/architecture/testdata/crm/." "$ARCH_SEED/docs/architecture/"
printf '# CRM\n\nУчёт контактов и сделок отдела продаж.\n' > "$ARCH_SEED/README.md"
(cd "$ARCH_SEED" \
  && git add -A \
  && git -c user.email=e2e@rivet -c user.name=e2e commit -q -m "seed" \
  && git push -q "$ARCH_BARE" main)

# Репозиторий конфигурации GitOps e2e/gitops (сценарий публикации через
# GitOps): тоже в git-режиме fake SCM, поэтому публикация коммитит версию
# в него настоящим коммитом. Окружение стенда (gitops-env ниже) отвечает
# версией из его ветки main, как приложение после синхронизации.
GITOPS_BARE="$STAND_DIR/repos/e2e/gitops.git"
git init --bare -b main -q "$GITOPS_BARE"
GITOPS_SEED="$STAND_DIR/seed-gitops"
git init -b main -q "$GITOPS_SEED"
printf '# Конфигурация окружений e2e\n' > "$GITOPS_SEED/README.md"
(cd "$GITOPS_SEED" \
  && git add -A \
  && git -c user.email=e2e@rivet -c user.name=e2e commit -q -m "seed" \
  && git push -q "$GITOPS_BARE" main)

# Свежая сборка консоли во встроенную статику, затем бинарники. Путь консоли
# вычисляется через cd: если rivet-service — символическая ссылка, `..` в
# пути для cp разворачивался бы от цели ссылки, а не от её каталога.
WEB_DIR=$(cd "$SERVICE_DIR/../rivet-web" && pwd -P)
(cd "$WEB_DIR" && npm run build >/dev/null)
rm -rf "$SERVICE_DIR/internal/webui/dist"
cp -r "$WEB_DIR/dist" "$SERVICE_DIR/internal/webui/dist"
(cd "$SERVICE_DIR" && go build -o "$STAND_DIR/rivetd" ./cmd/rivetd \
  && go build -o "$STAND_DIR/rivet-runner" ./cmd/rivet-runner \
  && go build -o "$STAND_DIR/fake-llm" ./cmd/fake-llm \
  && go build -o "$STAND_DIR/gitops-env" ./scripts/e2e-gitops-env \
  && go build -o "$STAND_DIR/fake-ruvds" ./cmd/fake-ruvds)

cleanup() { kill 0 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# Fake-провайдер моделей (add-model-connections): консоль создаёт к нему
# подключение с ключом fake-key, планировщик декомпозирует его планом.
LLM_PORT=${E2E_LLM_PORT:-8283}
"$STAND_DIR/fake-llm" -addr "127.0.0.1:$LLM_PORT" &
for _ in $(seq 1 50); do
  curl -s "http://localhost:$LLM_PORT/v1/models" >/dev/null 2>&1 && break
  sleep 0.1
done

# Окружение GitOps стенда: порт выбирает ОС, адрес — в
# $STAND_DIR/gitops-env.addr (его читает сценарий GitOps rivet-e2e).
# Первый запуск свежего бинарника может задержать проверка ОС, поэтому
# окно до 30 секунд, а ранний выход программы виден сразу.
"$STAND_DIR/gitops-env" -repo "$GITOPS_BARE" -ref main -addr-file "$STAND_DIR/gitops-env.addr" &
GITOPS_PID=$!
for _ in $(seq 1 300); do
  [ -s "$STAND_DIR/gitops-env.addr" ] && break
  kill -0 "$GITOPS_PID" 2>/dev/null || break
  sleep 0.1
done
[ -s "$STAND_DIR/gitops-env.addr" ] || { echo "e2e-stand: окружение GitOps не запустилось" >&2; exit 1; }

# Fake RUVDS (add-environment-provisioning): rivetd ходит в него через
# RIVET_RUVDS_API_URL. Журнал запросов: GET /__fake/calls.
RUVDS_PORT=${E2E_RUVDS_PORT:-$((HTTP_PORT + 4))}
"$STAND_DIR/fake-ruvds" -addr "127.0.0.1:$RUVDS_PORT" &
for _ in $(seq 1 50); do
  curl -s "http://127.0.0.1:$RUVDS_PORT/__fake/calls" >/dev/null 2>&1 && break
  sleep 0.1
done

# Идентичность стенда: bootstrap-админ (см. ниже env rivetd), PAT для
# CLI/скриптов в $STAND_DIR/token и токен регистрации runner'ов. Runner'ы
# стартуют только после выпуска токена (протокол v5: без токена
# регистрация отклоняется), поэтому запускаются из этой же фоновой ветки.
# Токен привязан к идентификаторам runner'ов стенда (add-runner-identity):
# другой идентификатор по нему не регистрируется. Список совпадает с
# start_runner ниже.
#
# Пароль и ключ стенда свои на каждый прогон: rivetd не стартует с
# паролем или ключом, опубликованными в репозитории (спека access-policy
# «Безопасные умолчания установки»). Playwright передаёт свой пароль в
# E2E_ADMIN_PASSWORD (rivet-e2e, e2e/stand.ts), без него пароль
# генерируется здесь и лежит в $STAND_DIR/admin-password для входа руками.
ADMIN_LOGIN="${E2E_ADMIN_LOGIN:-e2e-admin}"
ADMIN_PASSWORD="${E2E_ADMIN_PASSWORD:-$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 24)}"
(umask 077 && printf '%s\n' "$ADMIN_PASSWORD" > "$STAND_DIR/admin-password")
API="http://localhost:$HTTP_PORT/api/v1"
LLM_CONN=${E2E_LLM_CONNECTION:-e2e-llm}
(
  for _ in $(seq 1 120); do
    curl -sf "$API/health" >/dev/null 2>&1 && break
    sleep 0.5
  done
  curl -sf -c "$STAND_DIR/cookies" -H 'Content-Type: application/json' \
    -d "{\"login\":\"$ADMIN_LOGIN\",\"password\":\"$ADMIN_PASSWORD\"}" \
    "$API/auth/login" >/dev/null \
  && curl -sf -b "$STAND_DIR/cookies" -H 'Content-Type: application/json' \
    -d '{"name":"e2e-stand"}' "$API/tokens" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["secret"])' > "$STAND_DIR/token" \
  && curl -sf -b "$STAND_DIR/cookies" -H 'Content-Type: application/json' \
    -d '{"name":"e2e-runners","runner_ids":["e2e-worker","e2e-worker2","e2e-reviewer","e2e-reviewer2","e2e-deployer"]}' "$API/runner-tokens" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["secret"])' > "$STAND_DIR/runner-token" \
  || { echo "e2e-stand: не удалось выпустить токены" >&2; exit 1; }
  # Автоописание архитектуры на стенде выключено, пока нет e2e-сценариев
  # архитектуры (add-architecture-c4): иначе каждый проект получал бы
  # задачу описания, и она занимала бы worker'ы сценариев. Сценарий
  # Архитектора в fake-llm есть (маркер задания describe).
  curl -sf -b "$STAND_DIR/cookies" "$API/system/policy" \
    | python3 -c 'import json,sys; p=json.load(sys.stdin)["presets"]; p["auto_describe_architecture"]=False; print(json.dumps(p))' \
    | curl -sf -b "$STAND_DIR/cookies" -X PUT -H 'Content-Type: application/json' -d @- "$API/system/policy" >/dev/null \
  || { echo "e2e-stand: не удалось выключить автоописание архитектуры" >&2; exit 1; }
  RUNNER_TOKEN=$(cat "$STAND_DIR/runner-token")
  # Служебное подключение стенда к fake-llm (не пресет): модели агента Rivet
  # — модели каталога, поэтому подключение создаётся до старта runner'ов.
  # fake-llm отдаёт модели без цен. Цены fake-small и fake-large (3 и 15
  # долларов за миллион токенов входа и выхода, в каталоге — микродоллары)
  # ставятся поверх обнаруженного списка: агент Rivet считает по ним
  # стоимость сессий, как раньше её сообщал fake-claude, и обзор с отчётом
  # показывают расход в долларах. Остальные модели списка не меняются.
  curl -sf -b "$STAND_DIR/cookies" -X PUT -H 'Content-Type: application/json' \
    -d "{\"name\":\"E2E LLM\",\"kind\":\"local\",\"api\":\"openai\",\"base_url\":\"http://localhost:$LLM_PORT/v1\",\"key\":\"fake-key\"}" \
    "$API/system/connections/$LLM_CONN" >/dev/null \
  && curl -sf -b "$STAND_DIR/cookies" -X POST "$API/system/connections/$LLM_CONN/discover" \
    | python3 -c '
import json, sys
models = json.load(sys.stdin)["connection"]["models"]
prices = {"fake-small": (3_000_000, 15_000_000), "fake-large": (3_000_000, 15_000_000)}
for m in models:
    if m["id"] in prices:
        m["input_price"], m["output_price"] = prices[m["id"]]
missing = set(prices) - {m["id"] for m in models}
if missing:
    sys.exit("e2e-stand: fake-llm не отдал модели " + ", ".join(sorted(missing)))
print(json.dumps({"models": models}))' > "$STAND_DIR/llm-models.json" \
  && curl -sf -b "$STAND_DIR/cookies" -X PUT -H 'Content-Type: application/json' \
    -d @"$STAND_DIR/llm-models.json" "$API/system/connections/$LLM_CONN/models" >/dev/null \
  && curl -sf -b "$STAND_DIR/cookies" -X PUT -H 'Content-Type: application/json' \
    -d "{\"default_model\":{\"connection_id\":\"$LLM_CONN\",\"model\":\"fake-small\"}}" \
    "$API/system/agents/rivet" >/dev/null \
  || { echo "e2e-stand: не удалось подключить fake-llm" >&2; exit 1; }
  # Runner'ы агента Rivet: клонирование из локального bare. Песочница для
  # команд агента на машине стенда может быть недоступна (macOS, Docker с
  # seccomp по умолчанию, Linux без user namespaces): модель здесь —
  # fake-llm, поэтому runner'ы стартуют с -unsandboxed. Флаг только
  # разрешает работать без песочницы: если проба проходит, команды агента
  # идут в песочнице без сети. Итог пробы runner пишет в журнал, журнал
  # каждого runner'а копируется в $STAND_DIR/runner-<id>.log: по нему e2e
  # сверяет флаг изоляции в API. Двое worker'ов — для пересечений работ,
  # двое ревьюеров — для процесса с двумя агентами на шаге review.
  start_runner() { # $1 — id, остальное — флаги
    id=$1; shift
    mkdir -p "$STAND_DIR/rw-$id"
    RIVET_RUNNER_TOKEN="$RUNNER_TOKEN" RIVET_PLANE_ADDR="localhost:$GRPC_PORT" RIVET_GIT_BASE="file://$STAND_DIR/repos/" \
      RIVET_RUNNER_UNSANDBOXED=1 \
      "$STAND_DIR/rivet-runner" -id "$id" -workdir "$STAND_DIR/rw-$id" "$@" 2>&1 \
      | tee "$STAND_DIR/runner-$id.log" >&2 &
  }
  start_runner e2e-worker -agent rivet -caps coding
  start_runner e2e-worker2 -agent rivet -caps coding
  start_runner e2e-reviewer -agent rivet -caps review
  start_runner e2e-reviewer2 -agent rivet -caps review
  # Runner выкатки без агента: окружения e2e исполняют команды локально.
  # Подмены ssh и docker из scripts/e2e-bin первыми в PATH: хост RUVDS
  # и локальный хост проверяются без настоящего сервера и Docker, вызовы
  # пишутся в журналы стенда.
  # Окружение задаётся прямо перед командой runner'а: присваивания перед
  # вызовом функции в разных sh ведут себя по-разному.
  mkdir -p "$STAND_DIR/rw-e2e-deployer"
  PATH="$SERVICE_DIR/scripts/e2e-bin:$PATH" E2E_SSH_LOG="$STAND_DIR/ssh.log" E2E_DOCKER_LOG="$STAND_DIR/docker.log" \
    RIVET_RUNNER_TOKEN="$RUNNER_TOKEN" RIVET_PLANE_ADDR="localhost:$GRPC_PORT" RIVET_GIT_BASE="file://$STAND_DIR/repos/" \
    RIVET_RUNNER_UNSANDBOXED=1 \
    "$STAND_DIR/rivet-runner" -id e2e-deployer -workdir "$STAND_DIR/rw-e2e-deployer" -caps deploy 2>&1 \
    | tee "$STAND_DIR/runner-e2e-deployer.log" >&2 &
  wait
) &

# Control plane на переднем плане.
# RIVET_FAKE_SCM_REVOKED — файл со списком токенов, которые fake-хостинг
# считает отозванными: сценарий отзыва пишет в него токен проекта и
# проверяет, что новый токен подключение возвращает (fix-credential-state).
# Ключ шифрования учётных данных хостингов: без него подключение
# репозитория с токеном отключено (fail-closed), и мастер не создаст проект.
# Генерируется на каждый прогон, как и пароль администратора.
SECRET_KEY=${E2E_SECRET_KEY:-$(head -c 32 /dev/urandom | base64)}

# Только loopback: у стенда известный секрет webhook и разрешены проверки
# URL во внутренней сети, наружу его открывать нельзя.
exec env \
  RIVET_HTTP_ADDR="127.0.0.1:$HTTP_PORT" \
  RIVET_SECRET_KEY="$SECRET_KEY" \
  RIVET_GRPC_ADDR="127.0.0.1:$GRPC_PORT" \
  RIVET_DATABASE_URL="$DB_URL" \
  RIVET_SCM=fake \
  RIVET_FAKE_GIT_DIR="$STAND_DIR/repos" \
  RIVET_FAKE_GIT_REPOS=e2e/arch,e2e/gitops \
  RIVET_FAKE_SCM_REVOKED="$STAND_DIR/scm-revoked" \
  RIVET_ALLOW_PRIVATE_URLS=true \
  RIVET_RUVDS_API_URL="http://127.0.0.1:$RUVDS_PORT" \
  RIVET_GITHUB_WEBHOOK_SECRET="${E2E_WEBHOOK_SECRET:-e2e-webhook-secret}" \
  RIVET_ADMIN_LOGIN="$ADMIN_LOGIN" \
  RIVET_ADMIN_PASSWORD="$ADMIN_PASSWORD" \
  "$STAND_DIR/rivetd"
