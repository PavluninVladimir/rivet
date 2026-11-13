<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Архитектура: Rivet

Автономная система разработки: превращает эпики и исследование продукта в выполненные задачи, публикует изменения через SCM и ведёт релизы с наблюдением за окружениями.

## C4 и HLD

- [HLD: обзор архитектуры](hld.md)
- [Контекст (L1)](c4/context.md)
- Контейнеры (L2): [Rivet](c4/container/system.md)
- Компоненты (L3): [rivetd](c4/component/app.rivetd.md)
- Компоненты (L3): [rivet-runner](c4/component/app.runner.md)
- [Развёртывание](c4/deployment.md)
- [Structurizr DSL](c4/workspace.dsl)

## Данные (ERD)

- [Общая схема](erd/all.md)
- Хранилище: [PostgreSQL](erd/tech.postgres.md)

## Сценарии

- [Описание архитектуры проекта](scenarios/sc.architecture-describe.md)
- [Публикация окружения и release notes](scenarios/sc.deployment.md)
- [Декомпозиция Epic в план задач](scenarios/sc.epic-decompose.md)
- [Явное нагрузочное тестирование рабочего локального релиза](scenarios/sc.load-testing.md)
- [Анализ интервью и утверждение бэклога](scenarios/sc.product-discovery.md)
- [Назначение и выполнение этапа runner'ом](scenarios/sc.runner-execution.md)

## Решения

- [Реестр архитектурных решений](adr.md): решений 6

## Принципы

| Принцип | Формулировка | Обоснование |
|---|---|---|
| `p.postgres-single-source` | PostgreSQL — единственный источник истины состояния | Вся изменяемая модель (проекты, эпики, задачи, сессии, релизы) живёт в одной базе; объектное хранилище держит только тяжёлые артефакты по ссылке. |
| `p.pull-protocol` | Runner'ы сами забирают работу по gRPC | Control plane не адресует runner'ов напрямую: runner регистрируется и получает этапы через двунаправленный поток, поэтому очередей сообщений нет. |
| `p.llm-scope` | Модель решает задачу, оркестратор управляет процессом | Детерминированные переходы статусов и проверки находятся в коде, а LLM используется для планирования, анализа, разработки, тестирования и review. |
| `p.local-runner` | Runner выполняется в среде пользователя | Код и песочница находятся на машине runner'а; control plane хранит только отчёты и метаданные сессий. |

## Viewpoint'ы

- [Бизнес](business.md): записей 35
- [Приложения](application.md): записей 37
- [Данные](data.md): записей 38
- [Технологии](technology.md): записей 5

## Процессы

- [Описание архитектуры](processes/biz.proc.architecture.md)
- [Исследование продукта](processes/biz.proc.discovery.md)
- [Выполнение Epic](processes/biz.proc.epic.md)
- [Релиз и наблюдение](processes/biz.proc.release.md)
