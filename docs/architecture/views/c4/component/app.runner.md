<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Компоненты: rivet-runner

- Контейнеры: [Rivet](../container/system.md)

```mermaid
C4Component
  title Компоненты: rivet-runner
  Container_Boundary(n_app_runner, "rivet-runner") {
    Component(n_app_runner_agent, "Агентский цикл Rivet", "Go", "Цикл агента роли: чтение репозитория, вызов модели, инструменты и формирование вердикта.")
    Component(n_app_runner_engine, "Движок runner'а", "Go", "Подключение к control plane, получение назначений, исполнение стадий публикации и наблюдения.")
    Component(n_app_runner_k6, "Выделенный нагрузочный runner", "Go, gRPC, native k6 2.3.x", "Единственная capability load, без агента и моделей. Native k6 на хосте публикации; только фиксированная GET-смесь с явным секретом. Следующая ступень ждёт сохранения предыдущей и отдельного разрешения control plane.")
  }
  System_Ext(n_ext_llm, "LLM-провайдеры", "Внешние API моделей: OpenAI, Anthropic, DeepSeek, OpenRouter, Ollama и LM Studio.")
  Container(n_app_rivetd, "rivetd", "Go 1.26, net/http, gRPC, pgx", "Control plane Rivet: HTTP API, gRPC-хаб runner'ов, оркестрация и встроенная веб-консоль.")
  Rel(n_app_rivetd, n_app_runner_k6, "передаёт")
  Rel(n_app_runner_agent, n_ext_llm, "использует")
  Rel(n_app_runner_engine, n_app_rivetd, "использует")
  Rel(n_app_runner_engine, n_app_runner_agent, "передаёт")
  Rel(n_app_runner_k6, n_app_rivetd, "передаёт")
```

| Элемент | Название | Вид | Технология | Описание |
|---|---|---|---|---|
| `ext.llm` | LLM-провайдеры | система, внешний | OpenAI/Anthropic-совместимые REST API | Внешние API моделей: OpenAI, Anthropic, DeepSeek, OpenRouter, Ollama и LM Studio. |
| `app.rivetd` | rivetd | контейнер | Go 1.26, net/http, gRPC, pgx | Control plane Rivet: HTTP API, gRPC-хаб runner'ов, оркестрация и встроенная веб-консоль. |
| `app.runner` | rivet-runner | контейнер | Go 1.26, gRPC | Агент на машине пользователя: подключается к control plane и выполняет этапы задач, публикации и проверки. |
| `app.runner.agent` | Агентский цикл Rivet | компонент | Go | Цикл агента роли: чтение репозитория, вызов модели, инструменты и формирование вердикта. |
| `app.runner.engine` | Движок runner'а | компонент | Go | Подключение к control plane, получение назначений, исполнение стадий публикации и наблюдения. |
| `app.runner.k6` | Выделенный нагрузочный runner | компонент | Go, gRPC, native k6 2.3.x | Единственная capability load, без агента и моделей. Native k6 на хосте публикации; только фиксированная GET-смесь с явным секретом. Следующая ступень ждёт сохранения предыдущей и отдельного разрешения control plane. |
