<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Сценарий: Запуск сборки по push

GitHub сообщает о push, API ставит сборку в очередь, воркер её забирает.

```mermaid
sequenceDiagram
  participant n_app_github as GitHub
  participant n_app_api as API
  participant n_tech_postgres as PostgreSQL
  participant n_tech_nats as NATS
  participant n_app_worker as Воркер сборок
  n_app_github-)n_app_api: POST /webhooks/push
  alt подпись вебхука неверна
    n_app_api-->>n_app_github: 401
  else иначе
    n_app_api->>n_tech_postgres: запись сборки со статусом queued
    n_app_api-)n_tech_nats: задание сборки
    n_app_api-->>n_app_github: 202
  end
  n_tech_nats-)n_app_worker: доставка задания
```

| Участник | Элемент | Вид |
|---|---|---|
| GitHub | `app.github` | участник |
| API | `app.api` | участник |
| PostgreSQL | `tech.postgres` | участник |
| NATS | `tech.nats` | участник |
| Воркер сборок | `app.worker` | участник |
