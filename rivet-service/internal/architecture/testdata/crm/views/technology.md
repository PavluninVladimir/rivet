<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Технологии

```mermaid
flowchart LR
  subgraph n_type_node["Узлы"]
    n_tech_host["Сервер"]
  end
  subgraph n_type_system_software["Системное ПО"]
    n_tech_json_file["JSON-файл"]
  end
  n_app_api["API-сервер"]
  n_data_contact["Контакт"]
  n_tech_host -->|"serving"| n_app_api
  n_tech_host -->|"assignment"| n_tech_json_file
  n_tech_json_file -->|"access: readwrite"| n_data_contact
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_api,n_data_contact ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `tech.host` | node | Сервер |  |
| `tech.json-file` | system-software | JSON-файл | data/crm.json, путь задаёт CRM_DATA_FILE |
