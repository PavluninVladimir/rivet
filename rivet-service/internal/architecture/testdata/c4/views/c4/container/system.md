<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Контейнеры: Платформа сборок

- [Контекст](../context.md)
- Компоненты: [API](../component/app.api.md)

```mermaid
C4Container
  title Контейнеры: Платформа сборок
  Person(n_biz_admin, "Администратор", "")
  Person_Ext(n_biz_auditor, "Аудитор", "Проверяет историю сборок снаружи компании")
  Person(n_biz_dev, "Разработчик", "")
  System_Boundary(n_system, "Платформа сборок") {
    Container(n_app_api, "API", "Go, net/http", "")
    Container(n_app_web, "Консоль", "React", "Веб-интерфейс платформы")
    Container(n_app_worker, "Воркер сборок", "Go", "")
    ContainerQueue(n_tech_nats, "NATS", "NATS JetStream", "Очередь заданий сборки")
    ContainerDb(n_tech_postgres, "PostgreSQL", "PostgreSQL 16", "")
  }
  System_Ext(n_app_github, "GitHub", "Хостинг репозиториев и событий push")
  Rel(n_app_api, n_app_github, "вызывает")
  Rel(n_app_api, n_tech_nats, "использует")
  Rel(n_app_api, n_tech_postgres, "пишет Сборка")
  Rel(n_app_web, n_app_api, "передаёт")
  Rel(n_app_worker, n_app_github, "передаёт")
  Rel(n_app_worker, n_tech_nats, "использует")
  Rel(n_app_worker, n_tech_postgres, "читает и пишет Сборка")
  Rel(n_biz_admin, n_app_web, "использует")
  Rel(n_biz_auditor, n_app_web, "использует")
  Rel(n_biz_dev, n_app_api, "использует")
  Rel(n_biz_dev, n_app_web, "использует")
```

| Элемент | Название | Вид | Технология | Описание |
|---|---|---|---|---|
| `biz.admin` | Администратор | человек |  |  |
| `biz.auditor` | Аудитор | человек, внешний |  | Проверяет историю сборок снаружи компании |
| `biz.dev` | Разработчик | человек |  |  |
| `system` | Платформа сборок | система |  | Собирает проекты разработчиков по событиям из GitHub и хранит историю сборок |
| `app.github` | GitHub | система, внешний |  | Хостинг репозиториев и событий push |
| `app.api` | API | контейнер | Go, net/http |  |
| `app.web` | Консоль | контейнер | React | Веб-интерфейс платформы |
| `app.worker` | Воркер сборок | контейнер | Go |  |
| `tech.nats` | NATS | очередь | NATS JetStream | Очередь заданий сборки |
| `tech.postgres` | PostgreSQL | база данных | PostgreSQL 16 |  |
