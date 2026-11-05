<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Компоненты: API

- Контейнеры: [Платформа сборок](../container/system.md)

```mermaid
C4Component
  title Компоненты: API
  Person(n_biz_dev, "Разработчик", "")
  Container_Boundary(n_app_api, "API") {
    Component(n_app_api_auth, "Аутентификация", "Go", "")
    Component(n_app_api_builds, "Сборки", "Go", "Ставит сборки в очередь и отдаёт их статус")
  }
  System_Ext(n_app_github, "GitHub", "Хостинг репозиториев и событий push")
  ContainerQueue(n_tech_nats, "NATS", "NATS JetStream", "Очередь заданий сборки")
  ContainerDb(n_tech_postgres, "PostgreSQL", "PostgreSQL 16", "")
  Rel(n_app_api_builds, n_app_github, "вызывает")
  Rel(n_app_api_builds, n_tech_nats, "использует")
  Rel(n_app_api_builds, n_tech_postgres, "пишет Сборка")
  Rel(n_biz_dev, n_app_api_auth, "использует")
```

| Элемент | Название | Вид | Технология | Описание |
|---|---|---|---|---|
| `biz.dev` | Разработчик | человек |  |  |
| `app.github` | GitHub | система, внешний |  | Хостинг репозиториев и событий push |
| `app.api` | API | контейнер | Go, net/http |  |
| `tech.nats` | NATS | очередь | NATS JetStream | Очередь заданий сборки |
| `tech.postgres` | PostgreSQL | база данных | PostgreSQL 16 |  |
| `app.api-auth` | Аутентификация | компонент | Go |  |
| `app.api-builds` | Сборки | компонент | Go | Ставит сборки в очередь и отдаёт их статус |
