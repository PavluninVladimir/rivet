<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Сценарий: Публикация окружения и release notes

Процесс: [Релиз и наблюдение](../processes/biz.proc.release.md).

```mermaid
sequenceDiagram
  actor n_biz_devops as Релиз-инженер
  participant n_app_api as REST API и SSE
  participant n_app_orchestrator as Оркестратор
  participant n_app_stream as gRPC-хаб runner'ов
  participant n_app_runner_engine as Движок runner'а
  participant n_app_ops as Релизы и инциденты
  participant n_tech_postgres as PostgreSQL
  n_biz_devops->>n_app_api: POST /api/v1/environments/{id}/deploy
  n_app_api->>n_tech_postgres: создать deployment
  n_app_orchestrator-)n_app_stream: PlaneMsg с задачей публикации
  n_app_stream-)n_app_runner_engine: назначение по gRPC-потоку
  n_app_runner_engine-)n_app_stream: PlaneMsg с результатом публикации
  n_app_stream-)n_app_orchestrator: результат публикации
  n_app_orchestrator->>n_tech_postgres: обновить deployment и release
  n_app_orchestrator-)n_app_ops: сформировать release notes
  n_app_ops->>n_tech_postgres: сохранить release notes
```

| Участник | Элемент | Вид |
|---|---|---|
| Релиз-инженер | `biz.devops` | человек |
| REST API и SSE | `app.api` | участник |
| Оркестратор | `app.orchestrator` | участник |
| gRPC-хаб runner'ов | `app.stream` | участник |
| Движок runner'а | `app.runner.engine` | участник |
| Релизы и инциденты | `app.ops` | участник |
| PostgreSQL | `tech.postgres` | участник |
