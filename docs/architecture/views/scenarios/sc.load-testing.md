<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Сценарий: Явное нагрузочное тестирование рабочего локального релиза

```mermaid
sequenceDiagram
  actor n_biz_owner as Владелец проекта
  participant n_app_api as REST API и SSE
  participant n_app_load_testing as Управление нагрузочными тестами
  participant n_app_stream as gRPC-хаб runner'ов
  participant n_app_runner_k6 as Выделенный нагрузочный runner
  participant n_tech_k6 as Native k6
  participant n_tech_postgres as PostgreSQL
  n_biz_owner->>n_app_api: Задать target и конечный профиль после первого рабочего релиза
  n_app_api->>n_app_load_testing: Проверить ownership и актуальную привязку публикации
  n_app_load_testing->>n_tech_postgres: Сохранить запрос и назначение выделенному runner на том же хосте
  n_app_load_testing-)n_app_stream: LoadTestJob с fencing и приватным явно выбранным секретом
  n_app_stream-)n_app_runner_k6: Проверить Docker identity и GET-сценарий
  n_app_runner_k6-)n_app_load_testing: Запросить разрешение ступени с предыдущими измерениями
  n_app_load_testing->>n_tech_postgres: Сохранить измерения и проверить активность назначения
  n_app_load_testing-)n_app_runner_k6: LoadTestContinue только после commit
  n_app_runner_k6->>n_tech_k6: Выполнить ограниченную GET-ступень с абсолютным дедлайном
  n_app_runner_k6-)n_app_load_testing: Вернуть финальные измерения после остановки процесса и удаления секрета
  n_app_load_testing->>n_tech_postgres: Сохранить target и capacity отдельно и освободить runner
```

| Участник | Элемент | Вид |
|---|---|---|
| Владелец проекта | `biz.owner` | человек |
| REST API и SSE | `app.api` | участник |
| Управление нагрузочными тестами | `app.load-testing` | участник |
| gRPC-хаб runner'ов | `app.stream` | участник |
| Выделенный нагрузочный runner | `app.runner.k6` | участник |
| Native k6 | `tech.k6` | участник |
| PostgreSQL | `tech.postgres` | участник |
