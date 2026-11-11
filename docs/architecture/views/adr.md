<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Архитектурные решения: Rivet

| № | Решение | Статус | Дата | Элементы | Замена |
|---|---|---|---|---|---|
| 0001 | [PostgreSQL — единственный источник истины состояния](../adr/0001-postgres-single-store.md) | принято | 2026-11-11 | `tech.postgres`, `data.project`, `data.epic`, `data.task`, `data.session`, `data.release`, `data.incident`, `app.api`, `app.orchestrator`, `app.planner`, `app.analyst`, `app.ops` |  |
| 0002 | [Разделение на развёртываемые Go-бинарники](../adr/0002-go-deployable-layout.md) | принято | 2026-11-11 | `app.rivetd`, `app.runner`, `app.cli`, `app.fake-llm`, `app.webui` |  |
| 0003 | [Pull-протокол runner'ов без очереди сообщений](../adr/0003-pull-protocol.md) | принято | 2026-11-11 | `app.stream`, `app.runner`, `app.orchestrator`, `app.runner-grpc`, `biz.proc.epic` |  |
| 0004 | [Политики проекта как данные и встроенный движок OPA/Rego](../adr/0004-embedded-policy-engine.md) | принято | 2026-11-11 | `app.policy`, `data.policy-version`, `biz.project` |  |
| 0005 | [Мультивендорные адаптеры SCM и LLM](../adr/0005-scm-and-llm-adapters.md) | принято | 2026-11-11 | `ext.github`, `ext.gitlab`, `ext.gitea`, `ext.llm`, `app.scm`, `app.llm`, `data.model-connection`, `data.scm-credential` |  |
| 0006 | [Транскрипты сессий в объектном хранилище](../adr/0006-s3-transcripts.md) | принято | 2026-11-11 | `tech.s3`, `data.session`, `app.redact` |  |
