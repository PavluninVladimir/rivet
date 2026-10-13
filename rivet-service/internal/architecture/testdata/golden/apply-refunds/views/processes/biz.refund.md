<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Процесс: Возврат заказа (`biz.refund`)

```mermaid
flowchart LR
  subgraph n_lane_lane_buyer["Покупатель"]
    n_node_start(("Покупатель хочет вернуть заказ"))
    n_node_form["Заполнить форму возврата"]
  end
  subgraph n_lane_lane_manager["Менеджер заказов"]
    n_node_review["Проверить заявку"]
    n_node_approved{"Возврат одобрен?"}
    n_node_refund["Оформить возврат"]
    n_node_reject["Сообщить об отказе"]
    n_node_refunded((("Деньги возвращены")))
    n_node_rejected((("Возврат отклонён")))
  end
  n_node_start --> n_node_form
  n_node_form --> n_node_review
  n_node_review --> n_node_approved
  n_node_approved -->|"да"| n_node_refund
  n_node_approved -->|"нет"| n_node_reject
  n_node_refund --> n_node_refunded
  n_node_reject --> n_node_rejected
```

| Дорожка | Элемент модели |
|---|---|
| Покупатель | `biz.buyer` |
| Менеджер заказов | `biz.manager` |

| Сервисная задача | Элемент модели |
|---|---|
| Оформить возврат | `app.refunds` |
| Сообщить об отказе | `app.refunds` |
