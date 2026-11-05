<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Приложения

```mermaid
flowchart LR
  subgraph n_type_component["Компоненты"]
    n_app_api["API-сервер"]
    n_app_api_domain["Домен контактов"]
    n_app_api_routes["Маршруты"]
    n_app_api_storage["Хранилище контактов"]
    n_app_web["Веб-клиент"]
  end
  n_biz_head["Руководитель отдела продаж"]
  n_biz_manager["Менеджер по продажам"]
  n_data_contact["Контакт"]
  n_tech_host["Сервер"]
  n_app_api -->|"composition"| n_app_api_domain
  n_app_api -->|"composition"| n_app_api_routes
  n_app_api -->|"composition"| n_app_api_storage
  n_app_api_routes -->|"flow"| n_app_api_domain
  n_app_api_routes -->|"flow"| n_app_api_storage
  n_app_api_storage -->|"access: readwrite"| n_data_contact
  n_app_web -->|"flow"| n_app_api
  n_app_web -->|"serving"| n_biz_head
  n_app_web -->|"serving"| n_biz_manager
  n_tech_host -->|"serving"| n_app_api
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_biz_head,n_biz_manager,n_data_contact,n_tech_host ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `app.api` | component | API-сервер | JSON API и раздача статики веб-клиента на одном порту (server/) |
| `app.api-domain` | component | Домен контактов | Создание, правка и проверка полей контакта, чистые функции (server/domain) |
| `app.api-routes` | component | Маршруты | Обработчики /api/health и /api/contacts (server/routes) |
| `app.api-storage` | component | Хранилище контактов | Чтение и атомарная запись JSON-файла, очередь записей (server/storage/json-store.js) |
| `app.web` | component | Веб-клиент | Список контактов с поиском и постраничной загрузкой, форма добавления (web/) |
