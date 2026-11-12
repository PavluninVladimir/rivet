<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Сценарий: Анализ интервью и утверждение бэклога

Процесс: [Исследование продукта](../processes/biz.proc.discovery.md).

```mermaid
sequenceDiagram
  actor n_biz_owner as Владелец проекта
  participant n_app_api as REST API и SSE
  participant n_app_analyst as Аналитик продукта
  participant n_app_discovery as Проверка исследования
  participant n_app_llm as Клиенты моделей
  participant n_ext_llm as LLM-провайдеры
  participant n_tech_postgres as PostgreSQL
  n_biz_owner->>n_app_api: POST /api/v1/projects/{id}/discovery/interviews
  n_app_api->>n_tech_postgres: сохранить интервью
  n_biz_owner->>n_app_api: POST /api/v1/projects/{id}/discovery/runs
  n_app_api->>n_app_analyst: запустить анализ
  n_app_analyst->>n_app_discovery: проверить конспекты и синтез
  n_app_analyst->>n_app_llm: конспект и синтез потребностей
  n_app_llm->>n_ext_llm: REST-запрос к модели
  n_ext_llm-->>n_app_llm: ответ модели
  n_app_llm-->>n_app_analyst: потребности и бэклог
  n_app_analyst->>n_tech_postgres: сохранить конспекты и бэклог
  n_biz_owner->>n_app_api: POST /api/v1/projects/{id}/discovery/runs/{id}/approve
  n_app_api->>n_tech_postgres: создать Epic'и из бэклога
```

| Участник | Элемент | Вид |
|---|---|---|
| Владелец проекта | `biz.owner` | человек |
| REST API и SSE | `app.api` | участник |
| Аналитик продукта | `app.analyst` | участник |
| Проверка исследования | `app.discovery` | участник |
| Клиенты моделей | `app.llm` | участник |
| LLM-провайдеры | `ext.llm` | участник |
| PostgreSQL | `tech.postgres` | участник |
