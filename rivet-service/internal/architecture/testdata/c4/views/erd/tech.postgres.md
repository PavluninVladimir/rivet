<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Данные: PostgreSQL (`tech.postgres`)

Объекты данных хранилища и их связи. [Общая схема](all.md).

```mermaid
erDiagram
  n_data_build["Сборка"] {
    uuid id PK
    uuid project_id FK "→ data.project.id"
    timestamptz started_at "может быть пустым"
    varchar_16 status "queued, running, done или failed, тип: varchar(16)"
  }
  n_data_project["Проект"] {
    uuid id PK
    text repo UK "owner/name на GitHub"
  }
  n_data_member["Участник · вне хранилищ"] {
  }
  n_data_build }o--|| n_data_project : "project_id"
  n_data_project }o--o{ n_data_member : "участники"
```

## Сборка (`data.build`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `project_id` | uuid | FK → `data.project.id` | нет |  |
| `started_at` | timestamptz |  | да |  |
| `status` | varchar(16) |  | нет | queued, running, done или failed |

## Проект (`data.project`)

Хранится в: PostgreSQL.

| Атрибут | Тип | Ключ | Пусто | Описание |
|---|---|---|---|---|
| `id` | uuid | PK | нет |  |
| `repo` | text | UK | нет | owner/name на GitHub |
