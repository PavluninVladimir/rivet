<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Процесс: Подача заявки (`biz.request`)

```mermaid
flowchart LR
  subgraph n_lane_lane_client["Клиент"]
    n_node_start(("Нужна помощь"))
    n_node_fill["Заполнить заявку"]
    n_node_save["Сохранить заявку"]
    n_node_end((("Заявка принята")))
  end
  n_node_start --> n_node_fill
  n_node_fill --> n_node_save
  n_node_save --> n_node_end
```

| Дорожка | Элемент модели |
|---|---|
| Клиент | `biz.client` |

| Сервисная задача | Элемент модели |
|---|---|
| Сохранить заявку | `app.api` |
