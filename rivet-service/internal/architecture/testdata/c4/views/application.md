<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Приложения

```mermaid
flowchart LR
  subgraph n_type_component["Компоненты"]
    n_app_api["API"]
    n_app_api_auth["Аутентификация"]
    n_app_api_builds["Сборки"]
    n_app_cleanup["Скрипт очистки"]
    n_app_github["GitHub"]
    n_app_web["Консоль"]
    n_app_worker["Воркер сборок"]
  end
  subgraph n_type_interface["Интерфейсы"]
    n_app_login["Вход через GitHub"]
  end
  n_biz_admin["Администратор"]
  n_biz_auditor["Аудитор"]
  n_biz_dev["Разработчик"]
  n_data_build["Сборка"]
  n_tech_docker["Docker"]
  n_tech_nats["NATS"]
  n_app_api -->|"composition"| n_app_api_auth
  n_app_api -->|"composition"| n_app_api_builds
  n_app_api_auth -->|"composition"| n_app_login
  n_app_api_builds -->|"triggering"| n_app_github
  n_app_api_builds -->|"access: write"| n_data_build
  n_app_cleanup -->|"flow"| n_app_worker
  n_app_login -->|"serving"| n_biz_dev
  n_app_web -->|"flow"| n_app_api
  n_app_web -->|"serving"| n_biz_admin
  n_app_web -->|"serving"| n_biz_auditor
  n_app_web -->|"serving"| n_biz_dev
  n_app_worker -->|"flow"| n_app_github
  n_app_worker -->|"access: readwrite"| n_data_build
  n_tech_docker -->|"serving"| n_app_api
  n_tech_docker -->|"serving"| n_app_web
  n_tech_docker -->|"serving"| n_app_worker
  n_tech_nats -->|"serving"| n_app_api_builds
  n_tech_nats -->|"serving"| n_app_worker
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_biz_admin,n_biz_auditor,n_biz_dev,n_data_build,n_tech_docker,n_tech_nats ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `app.api` | component | API |  |
| `app.api-auth` | component | Аутентификация |  |
| `app.api-builds` | component | Сборки | Ставит сборки в очередь и отдаёт их статус |
| `app.cleanup` | component | Скрипт очистки |  |
| `app.github` | component | GitHub | Хостинг репозиториев и событий push |
| `app.login` | interface | Вход через GitHub |  |
| `app.web` | component | Консоль | Веб-интерфейс платформы |
| `app.worker` | component | Воркер сборок |  |
