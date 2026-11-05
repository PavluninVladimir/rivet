<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Данные

```mermaid
flowchart LR
  subgraph n_type_data_object["Объекты данных"]
    n_data_request["Заявка"]
  end
  n_app_api["API заявок"]
  n_tech_db["PostgreSQL"]
  n_app_api -->|"access: readwrite"| n_data_request
  n_tech_db -->|"access: readwrite"| n_data_request
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_api,n_tech_db ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `data.request` | data-object | Заявка |  |
