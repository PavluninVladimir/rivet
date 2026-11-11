<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Процесс: Релиз и наблюдение (`biz.proc.release`)

Публикация окружений, формирование релиза и обработка инцидентов.

```mermaid
flowchart LR
  subgraph n_lane_lane_devops["Релиз-инженер"]
    n_node_start(("Изменения готовы к публикации"))
    n_node_deploy["Опубликовать окружение"]
    n_node_releasenotes["Оформить release notes"]
  end
  subgraph n_lane_lane_sre["Инженер сопровождения"]
    n_node_verify["Проверить наблюдение"]
    n_node_gate{"Окружение стабильно?"}
    n_node_incident["Описать инцидент"]
  end
  subgraph n_lane_lane_owner["Владелец проекта"]
    n_node_done((("Релиз оформлен")))
    n_node_needshuman((("Передано человеку")))
  end
  n_node_start --> n_node_deploy
  n_node_deploy --> n_node_verify
  n_node_verify --> n_node_gate
  n_node_gate -->|"да"| n_node_releasenotes
  n_node_gate -->|"нет"| n_node_incident
  n_node_releasenotes --> n_node_done
  n_node_incident --> n_node_needshuman
```

| Дорожка | Элемент модели |
|---|---|
| Релиз-инженер | `biz.devops` |
| Инженер сопровождения | `biz.sre` |
| Владелец проекта | `biz.owner` |

| Сервисная задача | Элемент модели |
|---|---|
| Опубликовать окружение | `app.orchestration-service` |
| Проверить наблюдение | `app.release-service` |
| Оформить release notes | `app.release-service` |
| Описать инцидент | `app.release-service` |
