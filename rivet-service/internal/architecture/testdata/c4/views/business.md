<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Бизнес

```mermaid
flowchart LR
  subgraph n_type_actor["Акторы"]
    n_biz_admin["Администратор"]
    n_biz_auditor["Аудитор"]
    n_biz_dev["Разработчик"]
  end
  n_app_login["Вход через GitHub"]
  n_app_web["Консоль"]
  n_app_login -->|"serving"| n_biz_dev
  n_app_web -->|"serving"| n_biz_admin
  n_app_web -->|"serving"| n_biz_auditor
  n_app_web -->|"serving"| n_biz_dev
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_login,n_app_web ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `biz.admin` | actor | Администратор |  |
| `biz.auditor` | actor | Аудитор | Проверяет историю сборок снаружи компании |
| `biz.dev` | actor | Разработчик |  |
