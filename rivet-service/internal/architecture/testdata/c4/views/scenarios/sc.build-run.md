<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Сценарий: Сборка проекта воркером

```mermaid
sequenceDiagram
  participant n_app_worker as Воркер сборок
  participant n_app_github as GitHub
  participant n_tech_postgres as PostgreSQL
  actor n_biz_dev as Разработчик
  participant n_app_web as Консоль
  participant n_app_api as API
  n_app_worker->>n_tech_postgres: статус running
  n_app_worker->>n_app_github: клонирование репозитория
  loop для каждого шага сборки
    n_app_worker->>n_app_worker: выполняет шаг
    n_app_worker->>n_tech_postgres: лог шага
  end
  n_app_worker->>n_tech_postgres: статус done
  opt разработчик открыл консоль
    n_biz_dev->>n_app_web: открывает сборку
    n_app_web->>n_app_api: GET /builds/{id}
    n_app_api-->>n_app_web: сборка и лог
  end
```

| Участник | Элемент | Вид |
|---|---|---|
| Воркер сборок | `app.worker` | участник |
| GitHub | `app.github` | участник |
| PostgreSQL | `tech.postgres` | участник |
| Разработчик | `biz.dev` | человек |
| Консоль | `app.web` | участник |
| API | `app.api` | участник |
