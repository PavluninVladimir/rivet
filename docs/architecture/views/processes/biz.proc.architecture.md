<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Процесс: Описание архитектуры (`biz.proc.architecture`)

Постановка задачи описания архитектуры и проверка результата в формате Rivet.

```mermaid
flowchart LR
  subgraph n_lane_lane_owner["Владелец проекта"]
    n_node_start(("Нужно описание архитектуры"))
    n_node_task["Поставить задачу Архитектору"]
    n_node_done((("Описание актуально")))
  end
  subgraph n_lane_lane_architect["Архитектор"]
    n_node_describe["Описать архитектуру"]
    n_node_validate["Проверить описание"]
  end
  n_node_start --> n_node_task
  n_node_task --> n_node_describe
  n_node_describe --> n_node_validate
  n_node_validate --> n_node_done
```

| Дорожка | Элемент модели |
|---|---|
| Владелец проекта | `biz.owner` |
| Архитектор | `biz.architect` |

| Сервисная задача | Элемент модели |
|---|---|
| Поставить задачу Архитектору | `app.orchestration-service` |
| Описать архитектуру | `app.architecture-service` |
| Проверить описание | `app.architecture-service` |
