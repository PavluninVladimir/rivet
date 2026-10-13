<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Изменение: Возвраты заказов (`refunds`)

## Видение

### Цель

Покупатель оформляет возврат на сайте, а не письмом. Заявку проверяет менеджер заказов, а результат попадает в учёт заказа.

### Рамки

Возврат заказа целиком. Частичные возвраты и обмен товара в изменение не входят.

### Заинтересованные лица

Покупатели, менеджеры заказов, бухгалтерия.

## Целевая модель

Зелёным отмечены добавленные элементы, жёлтым изменённые, красным пунктиром удалённые. Серым пунктиром показаны соседние элементы без изменений.

```mermaid
flowchart LR
  subgraph n_type_role["Роли"]
    n_biz_manager["Менеджер заказов"]
  end
  subgraph n_type_process["Процессы"]
    n_biz_refund["Возврат заказа"]
  end
  subgraph n_type_component["Компоненты"]
    n_app_mail_refunds["Возвраты по почте"]
    n_app_refunds["Сервис возвратов"]
    n_app_storefront["Витрина"]
  end
  subgraph n_type_interface["Интерфейсы"]
    n_app_refund_form["Форма возврата"]
  end
  subgraph n_type_node["Узлы"]
    n_tech_k8s["Кластер Kubernetes"]
  end
  n_app_order_api["API заказов"]
  n_app_orders["Сервис заказов"]
  n_biz_buyer["Покупатель"]
  n_biz_checkout["Оформление заказа"]
  n_biz_delivery["Доставка заказа"]
  n_biz_order["Заказ"]
  n_biz_sales["Продажа товаров"]
  n_biz_staff["Сотрудник магазина"]
  n_data_order["Запись заказа"]
  n_tech_postgres["PostgreSQL"]
  n_app_mail_refunds -.->|"access: read"| n_data_order
  n_app_order_api -->|"serving"| n_app_storefront
  n_app_refund_form -->|"serving"| n_biz_buyer
  n_app_refunds -->|"realization"| n_biz_refund
  n_app_refunds -->|"access: write"| n_data_order
  n_app_storefront -->|"flow"| n_app_orders
  n_app_storefront -->|"flow"| n_app_refunds
  n_app_storefront -->|"realization"| n_biz_checkout
  n_biz_manager -->|"assignment"| n_biz_delivery
  n_biz_manager -->|"assignment"| n_biz_refund
  n_biz_refund -->|"access: readwrite"| n_biz_order
  n_biz_refund -->|"realization"| n_biz_sales
  n_biz_staff -->|"assignment"| n_biz_manager
  n_tech_k8s -->|"serving"| n_app_orders
  n_tech_k8s -->|"serving"| n_app_refunds
  n_tech_k8s -->|"serving"| n_app_storefront
  n_tech_k8s -->|"assignment"| n_tech_postgres
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_refund_form,n_app_refunds,n_biz_refund added
  class n_app_order_api,n_app_orders,n_biz_buyer,n_biz_checkout,n_biz_delivery,n_biz_order,n_biz_sales,n_biz_staff,n_data_order,n_tech_postgres ext
  class n_app_storefront,n_biz_manager,n_tech_k8s modified
  class n_app_mail_refunds removed
```

## Разрывы

| Разрыв | Вид | Название | Элементы | Процессы |
|---|---|---|---|---|
| `gap.mail-refunds` | remove | Разбор писем о возвратах больше не нужен | `app.mail-refunds` |  |
| `gap.refund-process` | add | Нет процесса возврата | `biz.refund`, `biz.manager` | `biz.refund` |
| `gap.refund-service` | add | Нет сервиса и формы возврата | `app.refunds`, `app.refund-form`, `app.storefront`, `tech.k8s`, `sec.refund-fraud` |  |

## Записи дельты

| Запись | Viewpoint | Изменение | Описание |
|---|---|---|---|
| `app.mail-refunds` | application | удалён | Возвраты переезжают в сервис возвратов |
| `app.refund-form` | application | добавлен | Форма возврата |
| `app.refunds` | application | добавлен | Сервис возвратов |
| `app.storefront` | application | изменён | Витрина |
| `biz.manager` | business | изменён | Менеджер заказов |
| `biz.refund` | business | добавлен | Возврат заказа |
| `sec.refund-fraud` | security | добавлен | Возврат чужого заказа |
| `tech.k8s` | technology | изменён | Кластер Kubernetes |

## Целевые процессы

### Возврат заказа (`biz.refund`)

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
