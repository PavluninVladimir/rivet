---
number: 0005
title: Мультивендорные адаптеры SCM и LLM
status: accepted
date: 2026-11-11
elements: [ext.github, ext.gitlab, ext.gitea, ext.llm, app.scm, app.llm, data.model-connection, data.scm-credential]
---

## Контекст

Rivet должен работать с разными хостингами репозиториев и разными API моделей.
Адаптеры SCM перечислены в `internal/scm` (`github`, `gitlab`, `gitea`),
а клиенты моделей — в `internal/llm` (`openai`, `anthropic`, `deepseek`,
`openrouter`, `ollama`, `lmstudio`). Подключения и учётные данные хранятся в
таблицах `model_connections` и `scm_credentials`.

## Решение

Инкапсулировать различия провайдеров за интерфейсами `scm` и `llm` и
конфигурировать их через подключения, а не жёстко задавать в коде. Для тестов
поддерживать `fake`-адаптер SCM и `fake-llm` как отдельный процесс.

## Последствия

- Новые SCM-хостинги и API-совместимые модели добавляются адаптером.
- Учётные данные хранятся зашифрованными (`token_enc`, `key_enc`).
- Webhook'и разных провайдеров нормализуются в один формат в `internal/scm`.
