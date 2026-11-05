<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Контейнеры: Интернет-магазин

- [Контекст](../context.md)

```mermaid
C4Container
  title Контейнеры: Интернет-магазин
  System_Boundary(n_system, "Интернет-магазин") {
    Container(n_app_mail_refunds, "Возвраты по почте", "", "Скрипт, который разбирает письма о возвратах")
    Container(n_app_orders, "Сервис заказов", "", "")
    Container(n_app_storefront, "Витрина", "", "Веб-приложение для покупателей")
    ContainerDb(n_tech_postgres, "PostgreSQL", "", "")
  }
  Rel(n_app_mail_refunds, n_tech_postgres, "читает Запись заказа")
  Rel(n_app_orders, n_tech_postgres, "использует, пишет Запись заказа")
  Rel(n_app_storefront, n_app_orders, "использует, передаёт")
```

| Элемент | Название | Вид | Технология | Описание |
|---|---|---|---|---|
| `system` | Интернет-магазин | система |  |  |
| `app.mail-refunds` | Возвраты по почте | контейнер |  | Скрипт, который разбирает письма о возвратах |
| `app.orders` | Сервис заказов | контейнер |  |  |
| `app.storefront` | Витрина | контейнер |  | Веб-приложение для покупателей |
| `tech.postgres` | PostgreSQL | база данных |  |  |
