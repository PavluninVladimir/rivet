<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Приложения

```mermaid
flowchart LR
  subgraph n_type_component["Компоненты"]
    n_app_mail_refunds["Возвраты по почте"]
    n_app_orders["Сервис заказов"]
    n_app_storefront["Витрина"]
  end
  subgraph n_type_interface["Интерфейсы"]
    n_app_web_ui["Веб-интерфейс витрины"]
  end
  subgraph n_type_application_service["Сервисы приложений"]
    n_app_order_api["API заказов"]
  end
  n_biz_buyer["Покупатель"]
  n_biz_checkout["Оформление заказа"]
  n_biz_delivery["Доставка заказа"]
  n_data_order["Запись заказа"]
  n_tech_k8s["Кластер Kubernetes"]
  n_tech_postgres["PostgreSQL"]
  n_app_mail_refunds -->|"access: read"| n_data_order
  n_app_order_api -->|"serving"| n_app_storefront
  n_app_order_api -->|"serving"| n_biz_delivery
  n_app_orders -->|"realization"| n_app_order_api
  n_app_orders -->|"access: write"| n_data_order
  n_app_storefront -->|"flow"| n_app_orders
  n_app_storefront -->|"realization"| n_biz_checkout
  n_app_web_ui -->|"serving"| n_biz_buyer
  n_tech_k8s -->|"serving"| n_app_orders
  n_tech_k8s -->|"serving"| n_app_storefront
  n_tech_postgres -->|"serving"| n_app_orders
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_biz_buyer,n_biz_checkout,n_biz_delivery,n_data_order,n_tech_k8s,n_tech_postgres ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `app.mail-refunds` | component | Возвраты по почте | Скрипт, который разбирает письма о возвратах |
| `app.order-api` | application-service | API заказов |  |
| `app.orders` | component | Сервис заказов |  |
| `app.storefront` | component | Витрина | Веб-приложение для покупателей |
| `app.web-ui` | interface | Веб-интерфейс витрины |  |
