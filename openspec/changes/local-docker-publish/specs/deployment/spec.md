# deployment — изменения для local-docker-publish

## MODIFIED Requirements

### Requirement: Типы окружений публикации

Система ДОЛЖНА поддерживать типы окружений публикации `ssh`, `k8s`,
`pipeline`, `gitops` и `local_docker`. Тип `local_docker` описан дельтой
`local-docker-publish` и не принимает `deploy_cmd`, `verify_cmd`,
`verify_url` и `host`.

#### Scenario: Неизвестный тип
- **WHEN** при создании окружения приходит `exec_type: docker`
- **THEN** ответ 422 с текстом `exec_type: ожидается ssh, k8s, pipeline, gitops или local_docker`

#### Scenario: Существующие типы без изменений
- **WHEN** создаётся окружение `exec_type: ssh` с `deploy_cmd`
- **THEN** контракт и поведение прежние

### Requirement: Запуск публикации версии

Для окружения `exec_type: local_docker` эндпоинт
`POST /api/v1/environments/{id}/deploy` ДОЛЖЕН быть синонимом
`POST /api/v1/environments/{id}/publish`: та же очередь, те же ограничения
«одна активная публикация», тот же ответ 202 с публикацией.

Для остальных типов `deploy` работает как раньше.

#### Scenario: deploy у local_docker
- **WHEN** участник вызывает `POST /environments/{id}/deploy` у
  `local_docker`
- **THEN** создаётся публикация поколения, ответ 202, в теле поле `status`

#### Scenario: Активная публикация
- **WHEN** публикация уже идёт
- **THEN** ответ 409 с кодом `publication_in_progress`

### Requirement: Итог публикации и эскалация

Провал публикации `local_docker` на любом этапе ДОЛЖЕН обновлять тот же
журнал публикаций и запускать ту же эскалацию с паузой автопубликаций, что
и провал других типов, с кодом причины в `reason.code`.

#### Scenario: Провал сборки
- **WHEN** публикация `local_docker` провалилась с `build_failed`
- **THEN** запись публикации в статусе `failed`, автопубликации окружения
  на паузе, эскалация создана
