<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Сценарий: Декомпозиция Epic в план задач

Процесс: [Выполнение Epic](../processes/biz.proc.epic.md).

```mermaid
sequenceDiagram
  actor n_biz_owner as Владелец проекта
  participant n_app_api as REST API и SSE
  participant n_app_planner as Планировщик
  participant n_app_llm as Клиенты моделей
  participant n_ext_llm as LLM-провайдеры
  participant n_tech_postgres as PostgreSQL
  n_biz_owner->>n_app_api: POST /api/v1/epics/{id}/decompose
  n_app_api->>n_app_planner: requestPlan(Epic)
  n_app_planner->>n_app_llm: составить план задач
  n_app_llm->>n_ext_llm: REST-запрос к модели
  n_ext_llm-->>n_app_llm: ответ модели
  n_app_llm-->>n_app_planner: план
  opt план не прошёл проверку
    n_app_planner->>n_app_llm: запросить исправление плана
  end
  n_app_planner-->>n_app_api: план задач
  n_app_api->>n_tech_postgres: материализовать задачи
  n_app_api-->>n_biz_owner: задачи созданы
```

| Участник | Элемент | Вид |
|---|---|---|
| Владелец проекта | `biz.owner` | человек |
| REST API и SSE | `app.api` | участник |
| Планировщик | `app.planner` | участник |
| Клиенты моделей | `app.llm` | участник |
| LLM-провайдеры | `ext.llm` | участник |
| PostgreSQL | `tech.postgres` | участник |
