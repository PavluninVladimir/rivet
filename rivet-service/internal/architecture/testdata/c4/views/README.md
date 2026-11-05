<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Архитектура: Платформа сборок

Собирает проекты разработчиков по событиям из GitHub и хранит историю сборок

## C4 и HLD

- [HLD: обзор архитектуры](hld.md)
- [Контекст (L1)](c4/context.md)
- Контейнеры (L2): [Платформа сборок](c4/container/system.md)
- Компоненты (L3): [API](c4/component/app.api.md)
- [Развёртывание](c4/deployment.md)
- [Structurizr DSL](c4/workspace.dsl)

## Данные (ERD)

- [Общая схема](erd/all.md)
- Хранилище: [PostgreSQL](erd/tech.postgres.md)

## Сценарии

- [Сборка проекта воркером](scenarios/sc.build-run.md)
- [Запуск сборки по push](scenarios/sc.build-trigger.md)

## Решения

- [Реестр архитектурных решений](adr.md): решений 3

## Принципы

| Принцип | Формулировка | Обоснование |
|---|---|---|
| `p.async-builds` | Сборки идут асинхронно через очередь |  |

## Viewpoint'ы

- [Бизнес](business.md): записей 3
- [Приложения](application.md): записей 8
- [Данные](data.md): записей 3
- [Технологии](technology.md): записей 4
