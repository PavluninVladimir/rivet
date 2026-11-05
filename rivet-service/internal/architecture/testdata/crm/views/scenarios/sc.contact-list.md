<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Сценарий: Просмотр списка контактов

Список грузится страницами по 50 контактов, следующая страница по кнопке «Показать ещё».

```mermaid
sequenceDiagram
  actor n_biz_manager as Менеджер по продажам
  participant n_app_web as Веб-клиент
  participant n_app_api_routes as Маршруты
  participant n_app_api_storage as Хранилище контактов
  n_biz_manager->>n_app_web: открывает CRM
  n_app_web->>n_app_api_routes: GET /api/contacts?page=1&limit=50
  n_app_api_routes->>n_app_api_storage: list
  n_app_api_routes-->>n_app_web: 200 items, total, hasMore
  loop пока hasMore и нажато «Показать ещё»
    n_app_web->>n_app_api_routes: GET /api/contacts?page=N
    n_app_api_routes-->>n_app_web: следующая страница
  end
```

| Участник | Элемент | Вид |
|---|---|---|
| Менеджер по продажам | `biz.manager` | человек |
| Веб-клиент | `app.web` | участник |
| Маршруты | `app.api-routes` | участник |
| Хранилище контактов | `app.api-storage` | участник |
