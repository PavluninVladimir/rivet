# Контракт API (v1, вертикальный срез)

Две поверхности: HTTP/JSON + SSE для клиентов (web/mobile) и gRPC-протокол для runner'ов (`pkg/protocol`). Требования — в `openspec/specs/`; здесь только форма контракта для первого кода.

## Операции (клиентский REST, префикс `/api/v1`)

### Проекты и Epic'и

- `GET /projects` · `POST /projects` — список/создание проекта (имя, SCM-репозиторий).
- `GET /projects/{id}/epics` · `POST /projects/{id}/epics` — Epic'и проекта; создание: цель, ограничения.
- `POST /epics/{id}/decompose` — запустить декомпозицию (план возвращается на просмотр).
- `POST /epics/{id}/tasks` — правка плана человеком: добавить задачу (title, description, criteria, deps, capabilities, estimate); отклоняется при цикле в DAG.
- `POST /epics/{id}/start` · `/pause` · `/resume` · `/archive` — управление; ошибки: `409` при недопустимом переходе статуса.
- `GET /epics/{id}` — Epic с задачами, DAG (deps), прогрессом, usage-сводкой.

### Задачи и эскалации

- `GET /tasks/{id}` — атрибуты, acceptance criteria со статусами, timeline, сессии попыток.
- `POST /tasks/{id}/answer` — ответ на blocked-вопрос (текст → уточнение критериев; сбрасывает счётчик попыток).
- `POST /tasks/{id}/retry` · `/cancel` — повтор после failed / отмена.
- `POST /tasks/{id}/merge` — подтверждение merge (авто-merge по умолчанию выключен).
- `GET /attention` — сквозная очередь needs attention; `POST /attention/{id}/claim` — взять в работу.

### Runner'ы, события, usage

- `GET /runners` — реестр: агент, модель, хост, capabilities, статус, текущая задача, ctx %.
- `POST /runners/{id}/drain` · `/undrain` — вывод из ротации.
- `GET /events?project=&epic=&task=&type=&cursor=` — event log, keyset-пагинация.
- `GET /usage?group_by=epic|task|runner|model&period=` — токены/стоимость/время.

Ошибки — единый формат: `{code, message, details}`; `404` неизвестный id, `409` недопустимый переход, `422` невалидный ввод.

## Модели данных (ядро)

- **Task**: `id, epic_id, title, description, status` (enum 10 значений из спеки `backend/domain-model`), `deps[], capabilities[], estimate, attempt{used,limit}, runner_id?, branch?, pr_url?, tokens, duration_s, criteria[{text,status}]`.
- **Epic**: `id, project_id, title, status` (`planned|running|paused|done|archived`), `progress{pct,weighted}, stats{tokens,cost,elapsed_s}`.
- **Runner**: `id, agent, model, host, status, capabilities[], task_id?, ctx_pct, last_seen`.
- **Event**: `id, ts, actor{kind:runner|scheduler|system|user, id}, type, project_id, epic_id?, task_id?, text` — id монотонный в рамках проекта (курсор SSE/пагинации).
- **Session**: `id, task_id?, attempt?, driver{kind,id}, agent, model, depth` (full|partial|minimal), `scope?, transcript_ref, tokens, duration_s`.

Все enum-поля — строковые (не числа), новые значения добавляются только аддитивно.

## Реальное время

- `GET /api/v1/stream?project={id}` — SSE; типы событий: `task.status`, `epic.progress`, `runner.status`, `attention.new`, `attention.claimed`, `session.step`, `session.log` (чанк live-вывода). `id:` SSE-события = `Event.id` → переподключение с `Last-Event-ID` доигрывает пропущенное из event log; `session.log` не реплеится (только live), полный транскрипт — по `transcript_ref`.

## Протокол runner'а (gRPC, `pkg/protocol`, отдельный порт)

- `Register(RunnerInfo) → RunnerSession` — агент, модель, хост, capabilities, версия протокола.
- `Channel(stream RunnerMsg) ⇄ (stream PlaneMsg)` — единственный bidi-стрим: runner → `Event | TranscriptChunk | Usage | Heartbeat`; plane → `Assign(task+context) | Answer | Pause | Cancel`. Доставка at-least-once, дедупликация по `msg_id` на приёме.
- Потеря стрима > таймаута heartbeat → runner `offline`, попытка перезапускается (спека `backend/domain-model`).

## Совместимость

- До версии 1.0 контракт нестабилен, но правило уже действует: изменения в `/api/v1` и protobuf — только аддитивные (новые поля optional, новые enum-значения — с fallback-отображением у клиентов «неизвестный статус»).
- **BREAKING** до 1.0 допустим только с одновременным обновлением web-клиента; после появления мобильных клиентов — только через новую версию с окном совместимости (спека `backend/monetization`/`clients/mobile` тут ни при чём — правило из `agent-integration`: старые адаптеры продолжают работать).
- Версия протокола runner'а передаётся в `Register`; plane обязан отвечать понятной ошибкой несовместимой версии, а не рвать соединение молча.
