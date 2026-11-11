# operations — изменения для local-docker-publish

## MODIFIED Requirements

### Requirement: Проверка окружения

Для окружения `exec_type: local_docker` проверка ДОЛЖНА выполняться по URL
с сети runner'а: `monitor.check_url` заполняется платформой из `listen`
публикации. Команды наблюдения (`monitor.check_cmd`, `monitor.logs_cmd`)
для этого типа ЗАПРЕЩЕНЫ: ответ 422.

Control plane ДОЛЖЕН продолжать отклонять проверки loopback и частных
адресов: ослабления фильтра адресов не требуется.

#### Scenario: Платформа подставляет URL
- **WHEN** публикация перешла в `running`
- **THEN** `monitor.check_url` равен `http://127.0.0.1:<port><health_path>`,
  правка этого поля пользователем отклоняется

#### Scenario: Команда наблюдения
- **WHEN** для `local_docker` задан `monitor.check_cmd`
- **THEN** ответ 422 с текстом `config.monitor.check_cmd: локальная публикация проверяется только URL`

### Requirement: Результат проверки

Результат проверки `local_docker` ДОЛЖЕН менять статус публикации:
три провала подряд — `unhealthy`, ответ 200–399 — `running`. Провал
проверки НЕ ДОЛЖЕН останавливать или удалять контейнер.

#### Scenario: Провал проверки
- **WHEN** три проверки подряд не ответили кодом 200–399
- **THEN** `status: unhealthy`, контейнер работает, событие в журнале

#### Scenario: Восстановление
- **WHEN** проверка снова отвечает 200–399
- **THEN** `status: running`, событие в журнале
