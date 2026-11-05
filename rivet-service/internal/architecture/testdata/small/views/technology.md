<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Технологии

```mermaid
flowchart LR
  subgraph n_type_system_software["Системное ПО"]
    n_tech_db["PostgreSQL"]
  end
  n_app_api["API заявок"]
  n_data_request["Заявка"]
  n_tech_db -->|"serving"| n_app_api
  n_tech_db -->|"access: readwrite"| n_data_request
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_api,n_data_request ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `tech.db` | system-software | PostgreSQL |  |
