<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Данные

```mermaid
flowchart LR
  subgraph n_type_data_object["Объекты данных"]
    n_data_order["Запись заказа"]
  end
  n_app_mail_refunds["Возвраты по почте"]
  n_app_orders["Сервис заказов"]
  n_biz_order["Заказ"]
  n_tech_postgres["PostgreSQL"]
  n_app_mail_refunds -->|"access: read"| n_data_order
  n_app_orders -->|"access: write"| n_data_order
  n_data_order -->|"realization"| n_biz_order
  n_tech_postgres -->|"access: readwrite"| n_data_order
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_mail_refunds,n_app_orders,n_biz_order,n_tech_postgres ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `data.order` | data-object | Запись заказа |  |
