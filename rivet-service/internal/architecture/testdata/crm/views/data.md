<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Данные

```mermaid
flowchart LR
  subgraph n_type_data_object["Объекты данных"]
    n_data_contact["Контакт"]
  end
  n_app_api_storage["Хранилище контактов"]
  n_tech_json_file["JSON-файл"]
  n_app_api_storage -->|"access: readwrite"| n_data_contact
  n_tech_json_file -->|"access: readwrite"| n_data_contact
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_api_storage,n_tech_json_file ext
```

| Элемент | Тип | Название | Описание |
|---|---|---|---|
| `data.contact` | data-object | Контакт | Запись server/domain/contacts.js, хранится массивом в data/crm.json |
