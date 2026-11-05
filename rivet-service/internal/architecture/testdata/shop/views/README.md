<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Архитектура: Интернет-магазин

## C4 и HLD

- [HLD: обзор архитектуры](hld.md)
- [Контекст (L1)](c4/context.md)
- Контейнеры (L2): [Интернет-магазин](c4/container/system.md)
- [Развёртывание](c4/deployment.md)
- [Structurizr DSL](c4/workspace.dsl)

## Данные (ERD)

- [Общая схема](erd/all.md)
- Хранилище: [PostgreSQL](erd/tech.postgres.md)

## Принципы

| Принцип | Формулировка | Обоснование |
|---|---|---|
| `p.single-order-source` | У заказа один источник истины | Состояние заказа меняет только сервис заказов, остальные читают его через API |
| `p.buyer-first` | Покупатель не ждёт внутренние системы | Витрина отвечает покупателю сразу, долгие операции идут асинхронно |

## Viewpoint'ы

- [Бизнес](business.md): записей 8
- [Приложения](application.md): записей 5
- [Данные](data.md): записей 1
- [Технологии](technology.md): записей 2
- [Угрозы и меры защиты](security.md): записей 1

## Процессы

- [Оформление заказа](processes/biz.checkout.md)
- [Доставка заказа](processes/biz.delivery.md)

## Изменения

- [Возвраты заказов](changes/refunds.md), целевые схемы C4: [changes/refunds-c4.md](changes/refunds-c4.md)
