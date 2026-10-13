<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Процесс: Оформление заказа (`biz.checkout`)

```mermaid
flowchart LR
  subgraph n_lane_lane_buyer["Покупатель"]
    n_node_start(("Корзина собрана"))
    n_node_cart["Проверить корзину"]
    n_node_pay["Оплатить заказ"]
  end
  subgraph n_lane_lane_shop["Магазин"]
    n_node_create_order["Создать заказ"]
    n_node_paid{"Оплата прошла?"}
    n_node_notify["Сообщить об ошибке оплаты"]
    n_node_done((("Заказ оформлен")))
    n_node_failed((("Заказ не оплачен")))
  end
  n_node_start --> n_node_cart
  n_node_cart --> n_node_create_order
  n_node_create_order --> n_node_pay
  n_node_pay --> n_node_paid
  n_node_paid -->|"да"| n_node_done
  n_node_paid -->|"нет"| n_node_notify
  n_node_notify --> n_node_failed
```

| Дорожка | Элемент модели |
|---|---|
| Покупатель | `biz.buyer` |

| Сервисная задача | Элемент модели |
|---|---|
| Создать заказ | `app.order-api` |
| Сообщить об ошибке оплаты | `app.storefront` |
