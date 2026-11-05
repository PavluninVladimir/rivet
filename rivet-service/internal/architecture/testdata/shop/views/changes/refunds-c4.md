<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# C4 изменения: Возвраты заказов (`refunds`)

Целевая архитектура после изменения. Описание изменения: [refunds.md](refunds.md).

## Контекст: Интернет-магазин

```mermaid
C4Context
  title Контекст: Интернет-магазин
  Person(n_biz_buyer, "Покупатель", "")
  Person(n_biz_customer, "Покупатель", "")
  Person(n_biz_manager, "Менеджер заказов", "")
  Person(n_biz_staff, "Сотрудник магазина", "")
  System(n_system, "Интернет-магазин", "")
  UpdateElementStyle(n_biz_manager, $bgColor="#fef7e0", $fontColor="#7a4100", $borderColor="#e37400")
```

## Контейнеры: Интернет-магазин

```mermaid
C4Container
  title Контейнеры: Интернет-магазин
  System_Boundary(n_system, "Интернет-магазин") {
    Container(n_app_mail_refunds, "Возвраты по почте", "", "Скрипт, который разбирает письма о возвратах")
    Container(n_app_orders, "Сервис заказов", "", "")
    Container(n_app_refunds, "Сервис возвратов", "", "")
    Container(n_app_storefront, "Витрина", "", "Веб-приложение для покупателей")
    ContainerDb(n_tech_postgres, "PostgreSQL", "", "")
  }
  Rel(n_app_mail_refunds, n_tech_postgres, "читает Запись заказа")
  Rel(n_app_orders, n_tech_postgres, "использует, пишет Запись заказа")
  Rel(n_app_refunds, n_tech_postgres, "пишет Запись заказа")
  Rel(n_app_storefront, n_app_orders, "использует, передаёт")
  Rel(n_app_storefront, n_app_refunds, "передаёт")
  UpdateElementStyle(n_app_mail_refunds, $bgColor="#fce8e6", $fontColor="#a50e0e", $borderColor="#d93025")
  UpdateElementStyle(n_app_refunds, $bgColor="#e6f4ea", $fontColor="#0d652d", $borderColor="#1e8e3e")
  UpdateElementStyle(n_app_storefront, $bgColor="#fef7e0", $fontColor="#7a4100", $borderColor="#e37400")
```

## Легенда

| Цвет рамки | Состояние |
|---|---|
| `#1e8e3e` | добавлено |
| `#e37400` | изменено |
| `#d93025` | удалено, оставлено на схеме для наглядности |
