<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Технологии

```mermaid
flowchart LR
  subgraph n_type_node["Узлы"]
    n_tech_vm["Виртуальная машина"]
  end
  subgraph n_type_system_software["Системное ПО"]
    n_tech_docker["Docker"]
    n_tech_nats["NATS"]
    n_tech_postgres["PostgreSQL"]
  end
  n_app_api["API"]
  n_app_api_builds["Сборки"]
  n_app_web["Консоль"]
  n_app_worker["Воркер сборок"]
  n_data_build["Сборка"]
  n_data_project["Проект"]
  n_tech_docker -->|"serving"| n_app_api
  n_tech_docker -->|"serving"| n_app_web
  n_tech_docker -->|"serving"| n_app_worker
  n_tech_nats -->|"serving"| n_app_api_builds
  n_tech_nats -->|"serving"| n_app_worker
  n_tech_postgres -->|"access: readwrite"| n_data_build
  n_tech_postgres -->|"access: readwrite"| n_data_project
  n_tech_vm -->|"assignment"| n_tech_docker
  n_tech_vm -->|"assignment"| n_tech_postgres
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_api,n_app_api_builds,n_app_web,n_app_worker,n_data_build,n_data_project ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `tech.docker` | system-software | Docker |  |
| `tech.nats` | system-software | NATS | Очередь заданий сборки |
| `tech.postgres` | system-software | PostgreSQL |  |
| `tech.vm` | node | Виртуальная машина |  |
