<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Данные: PostgreSQL [main] (`tech.pg`)

Объекты данных хранилища и их связи. [Общая схема](all.md).

```mermaid
erDiagram
  n_data_line["data.line"] {
    int id PK
    uuid order_pk FK "→ data.order.PK"
  }
  n_data_order["Заказ '100％' ∖ №1, [черновик]"] {
    uuid PK_ PK "имя: PK"
    decimal_10_2 _50 "путь C:∖tmp, а ％％ не комментарий, имя: 50％ скидка, тип: decimal(10,2)"
    uk_ Uk_ UK "имя: Uk, тип: uk"
    string a_b "имя: a|b"
    Fk_ fk_ "имя: fk, тип: Fk"
    string x_y "имя: x`y"
  }
  n_data_tag["Метка · вне хранилищ"] {
  }
  n_data_line }o--|| n_data_order : "order_pk"
  n_data_tag }o--o{ n_data_order : ""
```

## data.line (`data.line`)

Хранится в: PostgreSQL [main].

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | int | PK | нет |  |
| `order_pk` | uuid | FK → `data.order.PK` | нет |  |

## Заказ "100%" \ #1; [черновик] (`data.order`)

Хранится в: PostgreSQL [main].

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `PK` | uuid | PK | нет |  |
| `50% скидка` | decimal(10,2) |  | нет | путь C:\tmp, а %% не комментарий |
| `Uk` | uk | UK | нет |  |
| `a\|b` | string |  | нет |  |
| `fk` | Fk |  | нет |  |
| `` x`y `` | string |  | нет |  |
