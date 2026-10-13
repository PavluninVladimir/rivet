<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Угрозы и меры защиты

```mermaid
flowchart LR
  n_item_sec_card_leak["Утечка данных карты"]
  n_item_sec_refund_fraud["Возврат чужого заказа"]
  n_app_refund_form["Форма возврата"]
  n_app_refunds["Сервис возвратов"]
  n_app_storefront["Витрина"]
  n_item_sec_card_leak -->|"targets"| n_app_storefront
  n_item_sec_refund_fraud -->|"targets"| n_app_refund_form
  n_item_sec_refund_fraud -->|"targets"| n_app_refunds
  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
  class n_app_refund_form,n_app_refunds,n_app_storefront ext
```

| id | title |
|---|---|
| `sec.card-leak` | Утечка данных карты |
| `sec.refund-fraud` | Возврат чужого заказа |
