<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->

# Сценарий: Создание контакта

Менеджер добавляет контакт через форму, API проверяет поля и пишет файл.

```mermaid
sequenceDiagram
  actor n_biz_manager as Менеджер по продажам
  participant n_app_web as Веб-клиент
  participant n_app_api_routes as Маршруты
  participant n_app_api_domain as Домен контактов
  participant n_app_api_storage as Хранилище контактов
  participant n_tech_json_file as JSON-файл
  n_biz_manager->>n_app_web: заполняет форму контакта
  n_app_web->>n_app_api_routes: POST /api/contacts
  n_app_api_routes->>n_app_api_domain: createContact
  alt поля не прошли проверку
    n_app_api_domain-->>n_app_api_routes: ValidationError с полем
    n_app_api_routes-->>n_app_web: 400 и поле с ошибкой
  else иначе
    n_app_api_routes->>n_app_api_storage: create
    n_app_api_storage->>n_tech_json_file: запись во временный файл и rename
    n_app_api_routes-->>n_app_web: 201 и контакт
  end
  n_app_web-->>n_biz_manager: показывает контакт в списке
```

| Участник | Элемент | Вид |
|---|---|---|
| Менеджер по продажам | `biz.manager` | человек |
| Веб-клиент | `app.web` | участник |
| Маршруты | `app.api-routes` | участник |
| Домен контактов | `app.api-domain` | участник |
| Хранилище контактов | `app.api-storage` | участник |
| JSON-файл | `tech.json-file` | участник |
