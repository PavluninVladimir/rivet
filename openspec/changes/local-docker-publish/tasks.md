# local-docker-publish — задачи

## rivet

- [ ] Добавить `ExecLocalDocker = "local_docker"` в домен, расширить список
      допустимых `exec_type` и текст ошибки (`ssh, k8s, pipeline, gitops
      или local_docker`).
- [ ] Ввести `LocalDockerEnvironmentConfig` (`config.local_docker`): модель,
      умолчания, строгий разбор с 422 на неизвестные поля, проверки
      диапазонов, `build_context`, `env` (префикс `RIVET_`, переводы строк).
- [ ] Запретить для `local_docker` поля `deploy_cmd`, `verify_cmd`,
      `verify_url`, `host`, `monitor.check_cmd`, `monitor.logs_cmd`;
      запретить `config.local_docker` у остальных типов.
- [ ] Миграция: таблица публикаций локальной доставки (`version`,
      `generation`, `status`, `desired_state`, `attempt`, `image_digest`,
      `container_id`, `host_port`, `health_url`, `reason`, `log_ref`,
      `deadline_at`), partial unique «одна активная публикация на окружение».
- [ ] Генерация Dockerfile по рецептам `go`, `node`, `static` и
      автоопределению; базовые образы закреплены digest'ом; сборка во
      временном каталоге, без `build-args` и секретов.
- [ ] Запуск контейнера: свободный порт 20000–29999 на хосте runner'а,
      `-p 127.0.0.1:<port>:<container_port>`, `--restart unless-stopped`,
      без `--rm`, метки `rivet.*`, запрет `host`/`macvlan`/`ipvlan`/
      `container:<id>`.
- [ ] Пост-проверка `docker inspect`: `HostIp` только `127.0.0.1`/`::1`;
      нарушение — остановка и удаление контейнера, `failed` с кодом
      `loopback_not_bound`.
- [ ] Healthcheck с runner'а: HTTP GET `127.0.0.1:<port><health_path>`,
      успех 200–399, таймаут 2 с, тело ≤ 4 КиБ, redirect не отслеживается,
      окно `startup_timeout_s`.
- [ ] Состояния и CAS-переходы публикации (`queued`, `building`, `starting`,
      `verifying`, `running`, `unhealthy`, `stopping`, `stopped`, `failed`,
      `unknown`), отбрасывание устаревших результатов runner'а.
- [ ] API: `publish`, `redeploy` (`version` из истории), `restart`, `stop`,
      `publication`, `publication/logs`; `deploy` как синоним `publish` для
      `local_docker`; коды ошибок по `api-contract.md`.
- [ ] Безопасный redeploy: новое поколение рядом со старым, переключение
      после healthcheck, SIGTERM/SIGKILL по `stop_timeout_s`, сохранение
      прежнего поколения при провале, хранение двух образов для отката.
- [ ] Обработка ошибок сборки и проверки: коды `recipe_not_detected`,
      `build_failed`, `healthcheck_failed`, `port_conflict`,
      `docker_unavailable`, `container_exited`, `image_missing`, `timeout`,
      `agent_lost`; пауза автопубликаций и эскалация через общий конвейер.
- [ ] Сверка (reconcile): поиск контейнеров по меткам, приведение
      наблюдаемого статуса, повторный запуск после перезапуска Docker
      (до трёх попыток: 5/15/30 с), удаление осиротевших контейнеров после
      грейса 10 минут, чистка образов старше двух поколений.
- [ ] Восстановление после перезапуска rivetd: `building|starting|verifying`
      с истёкшим дедлайном → `failed` (`agent_lost`), `running|stopped`
      восстанавливаются без пересборки, `desired_state: stopped` не
      воскрешается.
- [ ] Логи публикации: `builder` и `container`, `tail` 1–5000, лимит 2 МиБ
      на поток и три поколения, без секретов.
- [ ] Наблюдение: `monitor.check_url` подставляется платформой из `listen`,
      проверка идёт с сети runner'а, `unhealthy` после трёх провалов,
      `running` после восстановления.
- [ ] Права и журнал: конфигурация — администратор, действия публикации —
      участник; события `environment.publish`, `environment.restart`,
      `environment.stop`, `environment.publication_failed`,
      `environment.publication_unhealthy`, `environment.reconciled`.
- [ ] Обновить README: как пользоваться локальной публикацией без
      Dockerfile, nginx и токенов; что адрес только loopback.
- [ ] Тесты: юнит-тесты конфигурации, рецептов, transitions, парсинга
      `HostIp`, сверки; HTTP-тесты новых эндпоинтов и запретов (422).

## rivet-web

- [ ] Карточка публикации: статус, версия, поколение, `listen` с
      копированием, кнопки «Опубликовать», «Пересобрать», «Перезапустить»,
      «Остановить», «Логи».
- [ ] Форма конфигурации `local_docker`: `container_port`, `health_path`,
      `recipe`, `build_context`, `startup_timeout_s`, `stop_timeout_s`,
      `env`; без полей Dockerfile, nginx, токенов и команд.
- [ ] Ошибки: русские тексты, 409 «публикация уже идёт», 422 с подсветкой
      поля, провал с причиной и ссылкой на лог сборки.
- [ ] Ограничения действий: кнопки неактивны во время активной публикации,
      у `stopped` доступна только публикация, у `failed` — «Пересобрать».
- [ ] Тесты компонентов и контракта DTO публикации.

## rivet-e2e

- [ ] Сценарий: публикация Go-приложения без Dockerfile доходит до
      `running`, `listen` на `127.0.0.1`.
- [ ] Сценарий: loopback-only — `docker inspect` даёт `HostIp: "127.0.0.1"`,
      `ss -ltn` не содержит порта публикации на `0.0.0.0`.
- [ ] Сценарий: redeploy не роняет работающее поколение (непрерывные
      запросы к `listen` без ошибок), провал сборки оставляет прежнее
      поколение.
- [ ] Сценарий: `restart` и `stop`, идемпотентность `stop`, отсутствие
      самоподъёма после `stop`.
- [ ] Сценарий: перезапуск демона Docker — публикация возвращается в
      `running` без пересборки.
- [ ] Сценарий: `deploy_cmd`, `verify_cmd`, `monitor.check_cmd`,
      `dockerfile` отклоняются ответом 422.
- [ ] Сценарий: логи сборки и контейнера отдаются с ограничением `tail`.

## Проверки

- [ ] Сверить заголовки `MODIFIED Requirements` в `specs/deployment` и
      `specs/operations` с именами требований в OpenSpec store (каталог
      `rivet`) и при необходимости поправить формулировки дельты.
- [ ] `go test ./...` в rivet-service.
- [ ] Проверка, что существующие типы окружений и деплой-конвейер не
      изменили поведение (регресс существующих тестов).
- [ ] Прогон e2e-стенда с Docker на хосте runner'а, не в контейнере
      (иначе loopback публикации не виден проверке).
