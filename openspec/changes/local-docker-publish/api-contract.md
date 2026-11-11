# local-docker-publish — API-контракт

База: `/api/v1`, аутентификация — сессия консоли или API-ключ проекта,
формат ошибок общий:

```json
{"error": {"code": "publication_in_progress", "message": "публикация уже идёт"}}
```

Ошибки полей формы — 422 с полем:

```json
{"error": {"code": "invalid", "message": "ожидается 1–65535"},
 "field": "config.local_docker.container_port"}
```

Все тексты `message` — по-русски.

## Изменение конфигурации

### `POST /api/v1/projects/{id}/environments`

Роль: администратор проекта. Для `exec_type: local_docker` обязателен
объект `config.local_docker`, поля других типов запрещены.

```json
{
  "name": "локально",
  "slug": "local",
  "exec_type": "local_docker",
  "trigger": "manual",
  "config": {
    "local_docker": {
      "container_port": 8080,
      "health_path": "/healthz",
      "recipe": "auto",
      "build_context": ".",
      "startup_timeout_s": 120,
      "stop_timeout_s": 10,
      "env": {"APP_MODE": "local"}
    }
  }
}
```

Ответ 201 — окружение с заполненными умолчаниями.

### `PATCH /api/v1/environments/{id}`

Роль: администратор проекта. Те же правила проверки. Смена
`container_port`, `health_path`, `recipe`, `build_context`, `env`
применяется к следующей публикации; работающее поколение не перезапускается.

Ошибки: 422 `invalid` с `field`, 409 `conflict` (окружение в работе —
только для смены `exec_type`), 404 `not_found`.

## Публикация

### `POST /api/v1/environments/{id}/publish`

Роль: участник проекта. Ставит публикацию HEAD базовой ветки.

Ответ 202:

```json
{
  "id": "0192b7…",
  "environment_id": "8f3a…",
  "version": "9d1c4b…",
  "generation": 4,
  "status": "queued",
  "desired_state": "running",
  "attempt": 1,
  "image": null,
  "container_id": null,
  "listen": null,
  "health_url": null,
  "reason": null,
  "log_ref": null,
  "created_at": "2026-02-11T10:12:03Z",
  "started_at": null,
  "finished_at": null
}
```

Ошибки: 409 `publication_in_progress`, 409 `environment_paused`,
409 `no_runner` (нет runner'а с capability `docker`), 422 `invalid`
(конфигурация не проходит проверку), 404 `not_found`.

`POST /api/v1/environments/{id}/deploy` для `local_docker` — синоним
`publish`, тело и коды те же.

### `POST /api/v1/environments/{id}/redeploy`

Роль: участник проекта. Пересобирает и публикует версию без простоя
работающего поколения.

Тело (необязательное):

```json
{"version": "9d1c4b…"}
```

Без тела — версия последней успешной публикации. Ответ 202 с публикацией в
статусе `queued`; `generation` растёт только после успешного переключения.

Ошибки: 409 `publication_in_progress`, 409 `environment_paused`,
409 `no_publication` (нет успешной публикации, а версия не передана),
409 `no_runner`, 422 `invalid` (`version` вне истории публикаций
окружения), 404 `not_found`.

### `POST /api/v1/environments/{id}/restart`

Роль: участник проекта. Перезапускает контейнер текущего поколения без
пересборки; порт и образ не меняются. Ответ 202 с публикацией.

Ошибки: 409 `no_publication`, 409 `operation_in_progress`,
409 `publication_stopped`, 404 `not_found`.

### `POST /api/v1/environments/{id}/stop`

Роль: участник проекта. SIGTERM, затем SIGKILL через `stop_timeout_s`;
`desired_state: stopped`. Идемпотентно: повторный вызов — 200, состояние и
журнал не меняются.

Ответ 200 с публикацией. Ошибки: 409 `operation_in_progress` (идёт
публикация — сначала дождаться её завершения), 404 `not_found`.

## Статус и логи

### `GET /api/v1/environments/{id}/publication`

Роль: участник проекта. Последняя публикация окружения.

Ответ 200 — публикация:

```json
{
  "id": "0192b7…",
  "environment_id": "8f3a…",
  "version": "9d1c4b…",
  "generation": 5,
  "status": "running",
  "desired_state": "running",
  "attempt": 2,
  "image": "sha256:5c9f…",
  "container_id": "b41d…",
  "listen": "127.0.0.1:20123",
  "health_url": "http://127.0.0.1:20123/healthz",
  "reason": null,
  "log_ref": "pub_0192b7",
  "created_at": "2026-02-11T10:12:03Z",
  "started_at": "2026-02-11T10:12:05Z",
  "finished_at": "2026-02-11T10:13:41Z"
}
```

`reason` при провале — объект:

```json
{"code": "build_failed", "message": "сборка завершилась с кодом 1"}
```

Коды `reason.code`: `config_invalid`, `recipe_not_detected`,
`build_failed`, `healthcheck_failed`, `loopback_not_bound`, `port_conflict`,
`docker_unavailable`, `container_exited`, `image_missing`, `timeout`,
`agent_lost`.

Статусы (`status`): `queued`, `building`, `starting`, `verifying`,
`running`, `unhealthy`, `stopping`, `stopped`, `failed`, `unknown`.
`desired_state`: `running`, `stopped`.

### `GET /api/v1/environments/{id}/publication/logs`

Роль: участник проекта.

| параметр | значения | умолчание |
| --- | --- | --- |
| `stream` | `builder`, `container` | `builder` |
| `tail` | 1–5000 | 200 |

Ответ 200 `text/plain; charset=utf-8`.

Ошибки: 404 `not_found` (логов нет или контейнера текущего поколения нет),
422 `invalid` (`tail` вне диапазона).

## Совместимость

- `GET /api/v1/environments/{id}/deployments` и
  `GET /api/v1/deployments/{id}/log` не меняются.
- Для остальных `exec_type` `deploy` работает как раньше.
- Поля `deploy_cmd`, `verify_cmd`, `verify_url`, `host`, `dockerfile`,
  `port` в конфигурации `local_docker` отсутствуют: 422 `invalid` с
  указанием поля.
- `monitor.check_url` для `local_docker` заполняется платформой;
  `monitor.check_cmd` и `monitor.logs_cmd` — 422.

## Журнал событий

| событие | когда |
| --- | --- |
| `environment.publish` | `publish`, `redeploy` |
| `environment.restart` | `restart` |
| `environment.stop` | первый `stop` |
| `environment.publication_failed` | переход в `failed` |
| `environment.publication_unhealthy` | переход в `unhealthy` |
| `environment.reconciled` | сверка изменила наблюдаемый статус |
| `environment.config` | правка конфигурации |
