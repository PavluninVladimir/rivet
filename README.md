# Rivet

Rivet is an orchestrator for autonomous software development with AI coding
agents (Claude Code, Codex, OpenCode and others). A human sets an Epic and
approves the plan; Rivet splits it into tasks, builds a dependency graph,
runs the tasks on connected runners, gets them tested and reviewed by an
independent agent, and merges the result. People are pulled in only where a
real decision is needed. This repository holds the Go backend
(`rivet-service`); the web console, end-to-end tests and mobile clients live
in separate repositories listed below. Documentation is in Russian.

Rivet — оркестратор автономной разработки: control plane для команды из
coding-агентов. Человек ставит Epic, проверяет план и запускает выполнение.
Дальше процессом управляет Rivet, а человека зовёт только туда, где нужно
решение: задача заблокирована, исчерпаны попытки, не хватает информации,
автопроверка не дала вердикта.

Конвейер: **Epic → Tasks → DAG → Coding → Tests → Review → Fix → Merge →
Done**. Rivet декомпозирует Epic на небольшие задачи, находит зависимости,
строит DAG и запускает независимые задачи параллельно на свободных runner'ах.
После реализации идут тесты и автопроверки, затем независимый агент-ревьюер.
Найденные проблемы возвращают задачу на исправление, после успешного review
изменения мержатся.

Главный принцип: **модель решает задачу, Rivet управляет процессом**.
Переходы между этапами, проверки, повторы, зависимости и блокировки держит
детерминированный код, а не промпт. Все действия пишутся в журнал событий.

## Репозитории

| Репозиторий | Что внутри |
|---|---|
| [rivet](https://github.com/PavluninVladimir/rivet) | этот репозиторий: Go-бэкенд `rivet-service` (rivetd, rivet-runner, CLI `rivet`) |
| [rivet-web](https://github.com/PavluninVladimir/rivet-web) | веб-консоль на React; прод-сборка встраивается в rivetd |
| [rivet-e2e](https://github.com/PavluninVladimir/rivet-e2e) | сквозные тесты на Playwright: браузер → rivetd → Postgres |
| [rivet-ios](https://github.com/PavluninVladimir/rivet-ios) | нативное iOS-приложение (код ещё не создан) |
| [rivet-android](https://github.com/PavluninVladimir/rivet-android) | нативное Android-приложение (код ещё не создан) |

Сборка консоли и e2e-стенд рассчитаны на то, что клиентские репозитории
лежат внутри клона `rivet`:

```
rivet/
├── rivet-service/
├── rivet-web/      # git clone https://github.com/PavluninVladimir/rivet-web.git
└── rivet-e2e/      # git clone https://github.com/PavluninVladimir/rivet-e2e.git
```

Эти папки перечислены в `.gitignore` и в репозиторий `rivet` не попадают.

## Что в rivet-service

- `cmd/rivetd` — control plane: REST API и SSE для консоли, gRPC для
  runner'ов, встроенная веб-консоль.
- `cmd/rivet-runner` — runner на машине исполнителя: подключается к rivetd,
  забирает задачи и запускает агента (нативный адаптер Claude Code, обёртка
  `wrap` для любого CLI-агента или внешний адаптер по контракту
  `pkg/adapter`).
- `cmd/rivet` — CLI: миграции, создание базы, просмотр проектов, Epic'ов и
  задач, импорт истории, проверка и отрисовка описания архитектуры проекта
  (`rivet architecture`, формат описан в
  [docs/architecture-format.md](docs/architecture-format.md)).
- `cmd/rivet-adapter-example` — пример внешнего адаптера агента.
- `cmd/fake-llm` — поддельный провайдер моделей для e2e-стенда.
- `internal/` — хранилище (PostgreSQL), оркестратор, планировщик, политики
  (OPA/Rego), SCM-адаптеры GitHub и GitLab, маскировка секретов, протокол
  runner'ов.
- `pkg/protocol` — gRPC-протокол между rivetd и runner'ами.

Состояние хранится в PostgreSQL, транскрипты сессий агентов — в
S3-совместимом хранилище (MinIO в dev).

## Сборка и запуск

Нужны Go 1.26.2 или новее, Docker (для Postgres и MinIO) и, если пересобирать консоль,
Node.js 24.

Проще всего поднять всё в Docker:

```sh
cd rivet-service
docker compose up -d --build
```

rivetd слушает `127.0.0.1:8080` (API и веб-консоль) и `127.0.0.1:8090`
(gRPC для runner'ов). Первый администратор — `admin` / `admin-local`; эти
значения по умолчанию годятся только для локального стенда, перед
публикацией задайте `RIVET_ADMIN_LOGIN`, `RIVET_ADMIN_PASSWORD` и
`RIVET_SECRET_KEY`. SCM по умолчанию `fake`: для настоящих pull request'ов
задайте `RIVET_SCM=github` и `RIVET_GITHUB_TOKEN`.

Локально без контейнера rivetd:

```sh
cd rivet-service
docker compose up -d postgres minio
make build            # go build ./...
RIVET_SCM=fake RIVET_ADMIN_LOGIN=admin RIVET_ADMIN_PASSWORD=change-me \
  go run ./cmd/rivetd
```

`RIVET_SCM=fake` подходит для знакомства: pull request'ы и merge фиктивные.
Для настоящего хостинга уберите его и задайте `RIVET_GITHUB_TOKEN`.

Миграции базы rivetd применяет сам при старте. Для декомпозиции Epic нужна
модель: ключ задаётся в консоли («Управление приложением» → «Модели») или
переменными `ANTHROPIC_API_KEY` / `DEEPSEEK_API_KEY`.

Runner подключается с машины, где лежат рабочие копии и установлен агент.
Токен регистрации выпускается в консоли («Управление приложением» →
«Runner'ы»):

```sh
go build -o rivet-runner ./cmd/rivet-runner
RIVET_RUNNER_TOKEN=rrt_... ./rivet-runner -plane localhost:8090 -agent claude-code -caps coding
```

Основные переменные окружения rivetd (полный список — в
`internal/config/config.go`):

| Переменная | Назначение |
|---|---|
| `RIVET_DATABASE_URL` | DSN PostgreSQL |
| `RIVET_HTTP_ADDR`, `RIVET_GRPC_ADDR` | адреса API (`:8080`) и протокола runner'ов (`:8090`) |
| `RIVET_GRPC_TLS_CERT`, `RIVET_GRPC_TLS_KEY` | TLS протокола runner'ов |
| `RIVET_S3_ENDPOINT`, `RIVET_S3_ACCESS_KEY`, `RIVET_S3_SECRET_KEY`, `RIVET_S3_BUCKET` | хранилище транскриптов |
| `RIVET_SCM`, `RIVET_GITHUB_TOKEN`, `RIVET_GITHUB_WEBHOOK_SECRET` | хостинг кода и webhook'и |
| `RIVET_SECRET_KEY` | ключ шифрования учётных данных хостингов (32 байта, base64) |
| `RIVET_PUBLIC_URL` | внешний адрес установки для регистрации webhook'ов |
| `RIVET_ADMIN_LOGIN`, `RIVET_ADMIN_PASSWORD` | первый администратор |
| `RIVET_POLICY_MODE`, `RIVET_POLICY_URL` | встроенный движок политик или внешний OPA |

### Веб-консоль

rivetd отдаёт консоль из `internal/webui/dist` (go:embed). Чтобы пересобрать
её из `rivet-web`:

```sh
cd rivet-service
make ui
```

### Тесты

```sh
cd rivet-service
go test ./...                       # без базы тесты хранилища пропускаются
docker compose up -d postgres
RIVET_DATABASE_URL='postgres://rivet:rivet@localhost:5432/rivet?sslmode=disable' go test ./...
make fuzz                           # короткий фаззинг по всем таргетам
make lint                           # golangci-lint
```

Сквозные тесты живут в [rivet-e2e](https://github.com/PavluninVladimir/rivet-e2e):
стенд поднимает `rivet-service/scripts/e2e-stand.sh`.

## License

Copyright (C) 2026 Vladimir Pavlunin. Licensed under the GNU Affero General Public License v3.0 only (AGPL-3.0-only).

Полный текст лицензии — в файле [LICENSE](LICENSE).
