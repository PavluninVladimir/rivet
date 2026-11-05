<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Бизнес

```mermaid
flowchart LR
  subgraph n_type_actor["Акторы"]
    n_biz_client["Клиент"]
  end
  subgraph n_type_process["Процессы"]
    n_biz_request["Подача заявки"]
  end
  n_app_api["API заявок"]
  n_app_web["Веб-кабинет"]
  n_app_api -->|"realization"| n_biz_request
  n_app_web -->|"serving"| n_biz_client
  n_biz_client -->|"assignment"| n_biz_request
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_api,n_app_web ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `biz.client` | actor | Клиент |  |
| `biz.request` | process | Подача заявки |  |
