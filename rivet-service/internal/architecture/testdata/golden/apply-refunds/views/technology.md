<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Технологии

```mermaid
flowchart LR
  subgraph n_type_node["Узлы"]
    n_tech_k8s["Кластер Kubernetes"]
  end
  subgraph n_type_system_software["Системное ПО"]
    n_tech_postgres["PostgreSQL"]
  end
  n_app_orders["Сервис заказов"]
  n_app_refunds["Сервис возвратов"]
  n_app_storefront["Витрина"]
  n_data_order["Запись заказа"]
  n_tech_k8s -->|"serving"| n_app_orders
  n_tech_k8s -->|"serving"| n_app_refunds
  n_tech_k8s -->|"serving"| n_app_storefront
  n_tech_k8s -->|"assignment"| n_tech_postgres
  n_tech_postgres -->|"serving"| n_app_orders
  n_tech_postgres -->|"access: readwrite"| n_data_order
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_orders,n_app_refunds,n_app_storefront,n_data_order ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `tech.k8s` | node | Кластер Kubernetes |  |
| `tech.postgres` | system-software | PostgreSQL |  |
