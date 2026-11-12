<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Процесс: Исследование продукта (`biz.proc.discovery`)

Загрузка интервью, анализ Аналитиком и утверждение бэклога владельцем.

```mermaid
flowchart LR
  subgraph n_lane_lane_owner["Владелец проекта"]
    n_node_start(("Материал интервью собран"))
    n_node_approve["Утвердить бэклог"]
    n_node_done((("Бэклог принят")))
  end
  subgraph n_lane_lane_analyst["Аналитик"]
    n_node_analyze["Проанализировать интервью"]
    n_node_create["Создать Epic'и из бэклога"]
  end
  n_node_start --> n_node_analyze
  n_node_analyze --> n_node_approve
  n_node_approve --> n_node_create
  n_node_create --> n_node_done
```

| Дорожка | Элемент модели |
|---|---|
| Владелец проекта | `biz.owner` |
| Аналитик | `biz.analyst` |

| Сервисная задача | Элемент модели |
|---|---|
| Проанализировать интервью | `app.discovery-service` |
| Создать Epic'и из бэклога | `app.discovery-service` |
