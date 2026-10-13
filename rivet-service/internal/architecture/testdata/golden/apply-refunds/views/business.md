<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Бизнес

```mermaid
flowchart LR
  subgraph n_type_actor["Акторы"]
    n_biz_customer["Покупатель"]
    n_biz_staff["Сотрудник магазина"]
  end
  subgraph n_type_role["Роли"]
    n_biz_buyer["Покупатель"]
    n_biz_manager["Менеджер заказов"]
  end
  subgraph n_type_business_service["Бизнес-сервисы"]
    n_biz_sales["Продажа товаров"]
  end
  subgraph n_type_process["Процессы"]
    n_biz_checkout["Оформление заказа"]
    n_biz_delivery["Доставка заказа"]
    n_biz_refund["Возврат заказа"]
  end
  subgraph n_type_business_object["Бизнес-объекты"]
    n_biz_order["Заказ"]
  end
  n_app_order_api["API заказов"]
  n_app_refund_form["Форма возврата"]
  n_app_refunds["Сервис возвратов"]
  n_app_storefront["Витрина"]
  n_app_web_ui["Веб-интерфейс витрины"]
  n_data_order["Запись заказа"]
  n_app_order_api -->|"serving"| n_biz_delivery
  n_app_refund_form -->|"serving"| n_biz_buyer
  n_app_refunds -->|"realization"| n_biz_refund
  n_app_storefront -->|"realization"| n_biz_checkout
  n_app_web_ui -->|"serving"| n_biz_buyer
  n_biz_buyer -->|"assignment"| n_biz_checkout
  n_biz_checkout -->|"triggering"| n_biz_delivery
  n_biz_checkout -->|"access: write"| n_biz_order
  n_biz_checkout -->|"realization"| n_biz_sales
  n_biz_customer -->|"assignment"| n_biz_buyer
  n_biz_delivery -->|"access: readwrite"| n_biz_order
  n_biz_delivery -->|"realization"| n_biz_sales
  n_biz_manager -->|"assignment"| n_biz_delivery
  n_biz_manager -->|"assignment"| n_biz_refund
  n_biz_refund -->|"access: readwrite"| n_biz_order
  n_biz_refund -->|"realization"| n_biz_sales
  n_biz_sales -->|"serving"| n_biz_customer
  n_biz_staff -->|"assignment"| n_biz_manager
  n_data_order -->|"realization"| n_biz_order
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_order_api,n_app_refund_form,n_app_refunds,n_app_storefront,n_app_web_ui,n_data_order ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `biz.buyer` | role | Покупатель |  |
| `biz.checkout` | process | Оформление заказа |  |
| `biz.customer` | actor | Покупатель |  |
| `biz.delivery` | process | Доставка заказа |  |
| `biz.manager` | role | Менеджер заказов |  |
| `biz.order` | business-object | Заказ |  |
| `biz.refund` | process | Возврат заказа |  |
| `biz.sales` | business-service | Продажа товаров | Покупатель выбирает товары, оплачивает и получает заказ |
| `biz.staff` | actor | Сотрудник магазина |  |
