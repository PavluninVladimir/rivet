<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Компоненты: API-сервер

- Контейнеры: [CRM](../container/system.md)

```mermaid
C4Component
  title Компоненты: API-сервер
  Container_Boundary(n_app_api, "API-сервер") {
    Component(n_app_api_domain, "Домен контактов", "JavaScript", "Создание, правка и проверка полей контакта, чистые функции (server/domain)")
    Component(n_app_api_routes, "Маршруты", "node:http", "Обработчики /api/health и /api/contacts (server/routes)")
    Component(n_app_api_storage, "Хранилище контактов", "node:fs", "Чтение и атомарная запись JSON-файла, очередь записей (server/storage/json-store.js)")
  }
  ContainerDb(n_tech_json_file, "JSON-файл", "JSON, атомарная запись через rename", "data/crm.json, путь задаёт CRM_DATA_FILE")
  Rel(n_app_api_routes, n_app_api_domain, "передаёт")
  Rel(n_app_api_routes, n_app_api_storage, "передаёт")
  Rel(n_app_api_storage, n_tech_json_file, "читает и пишет Контакт")
```

| Элемент | Название | Вид | Технология | Описание |
|---|---|---|---|---|
| `app.api` | API-сервер | контейнер | Node.js, node:http | JSON API и раздача статики веб-клиента на одном порту (server/) |
| `tech.json-file` | JSON-файл | база данных | JSON, атомарная запись через rename | data/crm.json, путь задаёт CRM_DATA_FILE |
| `app.api-domain` | Домен контактов | компонент | JavaScript | Создание, правка и проверка полей контакта, чистые функции (server/domain) |
| `app.api-routes` | Маршруты | компонент | node:http | Обработчики /api/health и /api/contacts (server/routes) |
| `app.api-storage` | Хранилище контактов | компонент | node:fs | Чтение и атомарная запись JSON-файла, очередь записей (server/storage/json-store.js) |
