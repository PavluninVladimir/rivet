<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Процесс: Выполнение Epic (`biz.proc.epic`)

Планирование Epic, выполнение задач по ролям и слияние результата.

```mermaid
flowchart LR
  subgraph n_lane_lane_owner["Владелец проекта"]
    n_node_start(("Epic принят в работу"))
    n_node_approve["Утвердить цель Epic"]
    n_node_done((("Epic выполнен")))
  end
  subgraph n_lane_lane_lead["Тимлид"]
    n_node_plan["Декомпозировать Epic"]
  end
  subgraph n_lane_lane_developer["Разработчик"]
    n_node_run["Выполнить этап по роли"]
    n_node_fix["Доработать задачу"]
  end
  subgraph n_lane_lane_reviewer["Ревьюер"]
    n_node_reviewgate{"Изменения приняты?"}
    n_node_merge["Слить в базовую ветку"]
  end
  n_node_start --> n_node_approve
  n_node_approve --> n_node_plan
  n_node_plan --> n_node_run
  n_node_run --> n_node_reviewgate
  n_node_reviewgate -->|"да"| n_node_merge
  n_node_reviewgate -->|"нет"| n_node_fix
  n_node_fix --> n_node_reviewgate
  n_node_merge --> n_node_done
```

| Дорожка | Элемент модели |
|---|---|
| Владелец проекта | `biz.owner` |
| Тимлид | `biz.lead` |
| Разработчик | `biz.developer` |
| Ревьюер | `biz.reviewer` |

| Сервисная задача | Элемент модели |
|---|---|
| Декомпозировать Epic | `app.planning-service` |
| Выполнить этап по роли | `app.orchestration-service` |
| Доработать задачу | `app.orchestration-service` |
| Слить в базовую ветку | `app.orchestration-service` |
