<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# HLD: Платформа сборок

Обзор архитектуры верхнего уровня. Документ собран из описания архитектуры, подробности в [C4-схемах](c4/context.md) и [представлениях слоёв](README.md).

## Назначение и принципы

Собирает проекты разработчиков по событиям из GitHub и хранит историю сборок

| Принцип | Формулировка | Обоснование |
|---|---|---|
| `p.async-builds` | Сборки идут асинхронно через очередь |  |

## Контекст (L1)

```mermaid
C4Context
  title Контекст: Платформа сборок
  Person(n_biz_admin, "Администратор", "")
  Person_Ext(n_biz_auditor, "Аудитор", "Проверяет историю сборок снаружи компании")
  Person(n_biz_dev, "Разработчик", "")
  System(n_system, "Платформа сборок", "Собирает проекты разработчиков по событиям из GitHub и хранит историю сборок")
  System_Ext(n_app_github, "GitHub", "Хостинг репозиториев и событий push")
  Rel(n_biz_admin, n_system, "использует")
  Rel(n_biz_auditor, n_system, "использует")
  Rel(n_biz_dev, n_system, "использует")
  Rel(n_system, n_app_github, "вызывает, передаёт")
```

Подробнее: [c4/context.md](c4/context.md).

## Контейнеры (L2)

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

| Контейнер | Вид | Технология | Описание |
|---|---|---|---|
| API (`app.api`) | контейнер | Go, net/http |  |
| Консоль (`app.web`) | контейнер | React | Веб-интерфейс платформы |
| Воркер сборок (`app.worker`) | контейнер | Go |  |
| NATS (`tech.nats`) | очередь | NATS JetStream | Очередь заданий сборки |
| PostgreSQL (`tech.postgres`) | база данных | PostgreSQL 16 |  |

## Ключевые бизнес-процессы

В описании нет бизнес-процессов.

## Данные

| Объект данных | Читают | Пишут | Хранится в |
|---|---|---|---|
| Сборка (`data.build`) | Воркер сборок | Воркер сборок, Сборки | PostgreSQL |
| Участник (`data.member`) | нет | нет | не указано |
| Проект (`data.project`) | нет | нет | PostgreSQL |

ERD: [общая схема](erd/all.md), по хранилищам: [PostgreSQL](erd/tech.postgres.md).

```mermaid
erDiagram
  n_data_build["Сборка"] {
    uuid id PK
    uuid project_id FK "→ data.project.id"
    timestamptz started_at "может быть пустым"
    varchar_16 status "queued, running, done или failed, тип: varchar(16)"
  }
  n_data_member["Участник"] {
    text login PK
  }
  n_data_project["Проект"] {
    uuid id PK
    text repo UK "owner/name на GitHub"
  }
  n_data_build }o--|| n_data_project : "project_id"
  n_data_project }o--o{ n_data_member : "участники"
```

## Развёртывание

```mermaid
C4Deployment
  title Развёртывание: Платформа сборок
  Deployment_Node(n_tech_vm, "Виртуальная машина", "Ubuntu 24.04", "") {
    Deployment_Node(n_tech_docker, "Docker", "системное ПО", "") {
      Container(n_inst_tech_docker_app_api, "API", "Go, net/http", "")
      Container(n_inst_tech_docker_app_web, "Консоль", "React", "Веб-интерфейс платформы")
      Container(n_inst_tech_docker_app_worker, "Воркер сборок", "Go", "")
    }
    ContainerDb(n_inst_tech_vm_tech_postgres, "PostgreSQL", "PostgreSQL 16", "")
  }
```

Подробнее: [c4/deployment.md](c4/deployment.md).

## Внешние интеграции

| Внешняя система | Описание | Связи |
|---|---|---|
| GitHub (`app.github`) | Хостинг репозиториев и событий push | Платформа сборок → GitHub: вызывает, передаёт |

## Ключевые сценарии

| Сценарий | Участники | Представление |
|---|---|---|
| Сборка проекта воркером (`sc.build-run`) | Воркер сборок, GitHub, PostgreSQL, Разработчик, Консоль, API | [scenarios/sc.build-run.md](scenarios/sc.build-run.md) |
| Запуск сборки по push (`sc.build-trigger`) | GitHub, API, PostgreSQL, NATS, Воркер сборок | [scenarios/sc.build-trigger.md](scenarios/sc.build-trigger.md) |

## Архитектурные решения

Принятые решения. Полный журнал со всеми статусами: [adr.md](adr.md).

- [0001. История сборок в PostgreSQL](../adr/0001-postgres-history.md)
- [0003. События push через вебхуки GitHub](../adr/0003-github-webhooks.md)

## Изменения архитектуры в работе

Изменений в работе нет.
