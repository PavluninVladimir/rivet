## 1. rivet (бэкенд)

### Каркас

- [x] 1.1 Скелет модуля: `cmd/rivetd`, `cmd/rivet-runner`, `cmd/rivet`, `internal/{domain,store,orchestrator,scm,api,stream}`, `pkg/protocol`; go.mod, Makefile, конфиг через окружение
- [x] 1.2 docker-compose для dev: PostgreSQL + MinIO; миграции (goose или аналог), подключение pgx
- [x] 1.3 CI на GitHub Actions: build + test + lint на PR

### Домен и хранилище

- [x] 1.4 Доменные типы и статусные машины Task/Epic/Runner по матрице переходов из `backend/domain-model` (недопустимый переход — ошибка + событие)
- [x] 1.5 Схема БД: projects, epics, tasks (+deps), runners, sessions, events (append-only), attention; транзакция «переход статуса + событие» как единственный путь записи
- [x] 1.6 Event log API хранилища: запись, keyset-чтение с фильтрами; монотонный id события в рамках проекта

### Планировщик

- [x] 1.7 Пересчёт готовности по DAG (deps done → ready; failed/cancelled → каскад blocked), проверка ацикличности при правке плана
- [x] 1.8 Очередь назначения: `FOR UPDATE SKIP LOCKED`, подбор runner'а по capabilities, один runner — одна задача
- [x] 1.9 Попытки: лимит (дефолт 3), расход на цикл fix→review, `failed` + эскалация при исчерпании; сброс счётчика при вмешательстве человека

### Протокол и runner

- [x] 1.10 Protobuf-контракт `pkg/protocol` (Register, bidi Channel: Event/TranscriptChunk/Usage/Heartbeat ⇄ Assign/Answer/Pause/Cancel), генерация Go
- [x] 1.11 gRPC-сервер в rivetd: регистрация, heartbeat → offline по таймауту с перезапуском попытки, дедупликация по msg_id
- [x] 1.12 `rivet-runner`: PTY-обёртка произвольного CLI-агента (глубина minimal), локальный журнал недоставленных сообщений, чанки транскрипта → MinIO через plane
- [x] 1.13 Запуск Claude Code через обёртку с передачей контекста задачи промптом (полноценный плагин с хуками — отдельное изменение)

### SCM (GitHub)

- [x] 1.14 SCM-адаптер интерфейс + реализация GitHub: ветка `agent/task-N`, создание PR, чтение checks, merge; токен бота из конфига
- [x] 1.15 Webhook-приёмник: события checks/merge/закрытия PR → event log и конвейер (ручной merge человеком корректно завершает задачу)

### Конвейер задачи

- [x] 1.16 Этап testing: запуск команд проверок проекта (из конфига проекта) на runner'е, провал → fixing в рамках попытки
- [x] 1.17 Этап review: назначение runner'а с capability `review` (≠ исполнитель), промпт с diff + acceptance criteria, вердикт → merge-ожидание или fixing
- [x] 1.18 Merge по подтверждению через API (`POST /tasks/{id}/merge`), пересчёт зависимых, done
- [x] 1.19 Эскалации: blocked (вопрос агента) и review-limit → `GET /attention`, `answer`/`retry`/`cancel`/`claim` по контракту

### API и наблюдаемость

- [x] 1.20 REST `/api/v1` по api-contract.md (projects, epics + управление, tasks, runners, events, usage, attention); единый формат ошибок
- [x] 1.21 SSE `/api/v1/stream`: task.status, epic.progress, runner.status, attention.*, session.step, session.log; реплей по Last-Event-ID из event log
- [x] 1.22 Usage: приём Usage-сообщений runner'а, агрегаты по epic/task/runner/model, `GET /usage`
- [x] 1.23 Декомпозиция Epic: вызов модели (ключ из конфига), валидация плана (ацикличность, criteria у каждой задачи), план в статусе planned до `start`

## 2. Проверка

- [x] 2.1 Юнит-тесты статусных машин и планировщика покрывают все переходы матрицы и сценарии спек `backend/domain-model`, `backend/orchestration`
- [x] 2.2 Интеграционный тест конвейера: фейковый runner + фейковый SCM, задача проходит queued→…→done; сценарии blocked и review-limit дают эскалации
- [ ] 2.3 Dogfooding-прогон: Epic в репозитории rivet, реальная задача через полный цикл (Claude Code в обёртке, PR на GitHub, review вторым runner'ом, merge кнопкой) — сценарий первой демонстрации из требований
