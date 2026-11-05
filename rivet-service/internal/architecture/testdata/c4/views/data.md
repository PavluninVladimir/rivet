<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Данные

```mermaid
flowchart LR
  subgraph n_type_data_object["Объекты данных"]
    n_data_build["Сборка"]
    n_data_member["Участник"]
    n_data_project["Проект"]
  end
  n_app_api_builds["Сборки"]
  n_app_worker["Воркер сборок"]
  n_tech_postgres["PostgreSQL"]
  n_app_api_builds -->|"access: write"| n_data_build
  n_app_worker -->|"access: readwrite"| n_data_build
  n_data_project ---|"association"| n_data_member
  n_tech_postgres -->|"access: readwrite"| n_data_build
  n_tech_postgres -->|"access: readwrite"| n_data_project
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_api_builds,n_app_worker,n_tech_postgres ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `data.build` | data-object | Сборка |  |
| `data.member` | data-object | Участник | Учётная запись GitHub, приходит из OAuth и в базе не хранится |
| `data.project` | data-object | Проект |  |
