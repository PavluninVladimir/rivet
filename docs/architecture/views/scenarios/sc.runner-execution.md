<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Сценарий: Назначение и выполнение этапа runner'ом

Процесс: [Выполнение Epic](../processes/biz.proc.epic.md).

```mermaid
sequenceDiagram
  participant n_app_orchestrator as Оркестратор
  participant n_app_stream as gRPC-хаб runner'ов
  participant n_app_runner_engine as Движок runner'а
  participant n_app_runner_agent as Агентский цикл Rivet
  participant n_ext_llm as LLM-провайдеры
  participant n_tech_postgres as PostgreSQL
  n_app_orchestrator-)n_app_stream: PlaneMsg с назначением этапа
  n_app_stream-)n_app_runner_engine: назначение по gRPC-потоку
  n_app_runner_engine->>n_app_runner_agent: выполнить этап по роли
  n_app_runner_agent->>n_ext_llm: запрос к модели с инструментами
  n_ext_llm-->>n_app_runner_agent: ответ модели
  n_app_runner_agent-->>n_app_runner_engine: вердикт и артефакты
  n_app_runner_engine-)n_app_stream: PlaneMsg с вердиктом
  n_app_stream-)n_app_orchestrator: вердикт и usage
  n_app_orchestrator->>n_tech_postgres: обновить статус задачи и этапа
```

| Участник | Элемент | Вид |
|---|---|---|
| Оркестратор | `app.orchestrator` | участник |
| gRPC-хаб runner'ов | `app.stream` | участник |
| Движок runner'а | `app.runner.engine` | участник |
| Агентский цикл Rivet | `app.runner.agent` | участник |
| LLM-провайдеры | `ext.llm` | участник |
| PostgreSQL | `tech.postgres` | участник |
