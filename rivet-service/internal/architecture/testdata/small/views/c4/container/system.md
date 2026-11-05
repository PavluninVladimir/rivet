<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Контейнеры: Учёт заявок

- [Контекст](../context.md)

```mermaid
C4Container
  title Контейнеры: Учёт заявок
  Person(n_biz_client, "Клиент", "")
  System_Boundary(n_system, "Учёт заявок") {
    Container(n_app_api, "API заявок", "Go", "")
    Container(n_app_web, "Веб-кабинет", "TypeScript, React", "Страницы для клиентов")
    ContainerDb(n_tech_db, "PostgreSQL", "PostgreSQL 16", "")
  }
  Rel(n_app_api, n_tech_db, "использует, читает и пишет Заявка")
  Rel(n_app_web, n_app_api, "передаёт")
  Rel(n_biz_client, n_app_web, "использует")
```

| Элемент | Название | Вид | Технология | Описание |
|---|---|---|---|---|
| `biz.client` | Клиент | человек |  |  |
| `system` | Учёт заявок | система |  | Принимает заявки клиентов и ведёт их до закрытия |
| `app.api` | API заявок | контейнер | Go |  |
| `app.web` | Веб-кабинет | контейнер | TypeScript, React | Страницы для клиентов |
| `tech.db` | PostgreSQL | база данных | PostgreSQL 16 |  |
