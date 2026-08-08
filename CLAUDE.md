# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Что такое Rivet

Rivet — оркестратор автономной разработки: control plane для команды из coding-агентов (Claude Code, Codex, Pi.dev, OpenCode и др.). Человек ставит Epic, проверяет план и запускает выполнение; дальше Rivet управляет процессом и привлекает человека только в точках, где действительно нужно решение (задача заблокирована, исчерпаны попытки, не хватает информации, автопроверка не даёт вердикта).

Конвейер: **Epic → Tasks → DAG → Coding → Tests → Review → Fix → Merge → Done**. Rivet декомпозирует Epic на небольшие задачи, определяет зависимости, строит DAG и запускает независимые задачи параллельно на доступных runner'ах. После реализации — тесты и автопроверки, затем независимый агент-ревьюер; найденные проблемы возвращают задачу на исправление, после успешного review изменения мержатся.

Главный принцип: **модель решает задачу, Rivet управляет процессом**. Workflow не хранится в промпте — переходы между этапами, проверки, retry, зависимости и блокировки контролируются детерминированным кодом. Все действия пишутся в event log.

Rivet — не multi-agent framework и не чат с ботами, а control plane для автономной software-engineering-команды.

## Стек и репозитории

Бэкенд (оркестратор, API) пишется на **Go** и живёт в этом репозитории (`PavluninVladimir/rivet`). Клиенты — в отдельных репозиториях:

- `PavluninVladimir/rivet-web` — веб-консоль на **React** (эталон UX — дизайн-прототип из этого репозитория);
- `PavluninVladimir/rivet-android` — нативное Android-приложение;
- `PavluninVladimir/rivet-ios` — нативное iOS-приложение.

Спецификации OpenSpec ведутся здесь и являются источником истины для всех четырёх репозиториев.

## Состояние репозитория

Кода ещё нет — проект на стадии формирования требований. В репозитории:

- `agent-orchestration-console.html` — эталонный дизайн-прототип консоли (сделан в Open Design). Одностраничный HTML без внешних зависимостей: тёмная тема, цветовые токены в oklch, демо-данные в `<script>` внизу файла. Это источник истины по UX и доменной модели — сверяйся с ним при написании требований и кода. Открывается просто в браузере.
- `openspec/` — spec-driven процесс OpenSpec (specs, changes, config.yaml с контекстом проекта).

## Доменная модель (зафиксирована в дизайн-прототипе)

- **Project → Epic → Task.** У задачи: зависимости (DAG), статусы `queued / ready / running / testing / review / fixing / blocked / failed / done`, лимит попыток (`att: 1/3`), ветка `agent/task-NNN`, PR, acceptance criteria, timeline событий, токены/длительность.
- **Runner** — исполнитель: агент + модель + хост, статус (`running/testing/review/idle/offline`), capabilities (`coding`, `frontend`, `review`, `cheap`, `local`, `large-context`), заполненность контекста. Задача в `ready` может ждать свободный runner с нужной capability.
- **Needs attention** — очередь эскалаций человеку: `BLOCKED` (например, неоднозначные acceptance criteria), `REVIEW LIMIT` (исчерпаны попытки review), `TEST FAILED` (например, недоступна среда).
- **Activity** — event log: `start / test / review / fail / block / pr / merge` с субъектом (runner, scheduler, system).
- **Usage** — токены и стоимость в разрезе Epic/задач/runner'ов.
- Отдельный агент-ревьюер проверяет чужой код; критический путь DAG подсвечивается в графе.

## Процесс разработки (OpenSpec)

Любое изменение проходит через OpenSpec (CLI v1.8.0, схема spec-driven):

- `/opsx:explore` — проработать идею/требования до изменения;
- `/opsx:propose "идея"` — создать change с артефактами (proposal, design, delta-specs, tasks) в `openspec/changes/<id>/`;
- `/opsx:apply` — реализовать задачи change;
- `/opsx:sync` — перенести delta-specs в основные `openspec/specs/`;
- `/opsx:archive` — заархивировать завершённый change.

Контекст проекта для артефактов — в `openspec/config.yaml`.

## Соглашения

- Язык документации и всех артефактов — русский.
