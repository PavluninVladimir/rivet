<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Процесс: Доставка заказа (`biz.delivery`)

```mermaid
flowchart LR
  subgraph n_lane_lane_manager["Менеджер заказов"]
    n_node_start(("Заказ оплачен"))
    n_node_split{"+"}
    n_node_pack["Собрать заказ"]
    n_node_invoice["Выставить документы"]
    n_node_join{"+"}
    n_node_ship["Передать курьеру"]
    n_node_end((("Заказ передан")))
  end
  n_node_start --> n_node_split
  n_node_split --> n_node_pack
  n_node_split --> n_node_invoice
  n_node_pack --> n_node_join
  n_node_invoice --> n_node_join
  n_node_join --> n_node_ship
  n_node_ship --> n_node_end
```

| Дорожка | Элемент модели |
|---|---|
| Менеджер заказов | `biz.manager` |

| Сервисная задача | Элемент модели |
|---|---|
| Выставить документы | `app.order-api` |
