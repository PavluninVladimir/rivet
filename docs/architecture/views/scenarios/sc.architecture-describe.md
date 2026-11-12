<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Сценарий: Описание архитектуры проекта

Процесс: [Описание архитектуры](../processes/biz.proc.architecture.md).

```mermaid
sequenceDiagram
  actor n_biz_owner as Владелец проекта
  participant n_app_api as REST API и SSE
  participant n_app_orchestrator as Оркестратор
  participant n_app_stream as gRPC-хаб runner'ов
  participant n_app_runner_engine as Движок runner'а
  participant n_app_runner_agent as Агентский цикл Rivet
  participant n_ext_llm as LLM-провайдеры
  participant n_app_architecture as Снимок архитектуры
  participant n_tech_postgres as PostgreSQL
  n_biz_owner->>n_app_api: POST /api/v1/projects/{id}/architecture/describe
  n_app_api->>n_app_orchestrator: запустить pipeline архитектуры
  n_app_orchestrator-)n_app_stream: PlaneMsg с задачей Архитектора
  n_app_stream-)n_app_runner_engine: назначение по gRPC-потоку
  n_app_runner_engine->>n_app_runner_agent: выполнить роль Архитектора
  n_app_runner_agent->>n_ext_llm: запрос к модели с инструментами
  n_ext_llm-->>n_app_runner_agent: ответ модели
  n_app_runner_agent-->>n_app_runner_engine: изменения в docs/architecture
  n_app_runner_engine-)n_app_stream: PlaneMsg с отчётом
  n_app_stream-)n_app_orchestrator: отчёт и diff
  n_app_orchestrator->>n_app_architecture: проверить описание и представления
  n_app_architecture->>n_tech_postgres: сохранить снимок архитектуры
```

| Участник | Элемент | Вид |
|---|---|---|
| Владелец проекта | `biz.owner` | человек |
| REST API и SSE | `app.api` | участник |
| Оркестратор | `app.orchestrator` | участник |
| gRPC-хаб runner'ов | `app.stream` | участник |
| Движок runner'а | `app.runner.engine` | участник |
| Агентский цикл Rivet | `app.runner.agent` | участник |
| LLM-провайдеры | `ext.llm` | участник |
| Снимок архитектуры | `app.architecture` | участник |
| PostgreSQL | `tech.postgres` | участник |
