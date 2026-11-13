# Корреляция и восстановление публикации

Изменения: OpenSpec `fix-local-publication-failure-correlation` и `fix-stale-deploy-attention` в store `rivet`.

## Идентичность сбоя

Специфичные причины `recipe_not_detected`, `port_conflict`, `loopback_not_bound`, `image_missing` используют `local-publish-v1`, env, буквальный source `release`, operation `publish`, каноническую стадию и code. Стадии: recipe→build, port/image→start, loopback→verify. SHA, баннер, номер попытки, время и сообщение не участвуют. `build_failed`, `container_exited`, `healthcheck_failed`, `config_invalid`, `docker_unavailable` и неизвестные причины сохраняют прежнюю идентичность по очищенному логу: эти коды слишком широки или описывают инфраструктуру.

Причина читается из принятого сохранённого результата, связанного с исходным deployment. Так же работает incident_pending после перезапуска. accepted completed_operation_id является durable marker до deployment CAS; scheduler сначала восстанавливает такие финалы, затем обрабатывает потерю runner/watchdog. CAS вместе с записью инцидента или incident_pending обеспечивает однократное применение. Технические операции не проходят через publish code-incident path.

На изолированном стенде опубликовать две синтетические версии без entrypoint, меняя первый баннер. Ожидается один активный incident с occurrences=2. Повторить доставку одного результата: счётчик не растёт. Другой env/code/phase даёт другой отпечаток. Generic compiler errors продолжают различаться по выводу. Проверить code/message в error_line, redaction и ограниченный evidence.

Старые fingerprints, incident/task history не переписываются. Возможен один переходный дубль legacy/structured. Сравнить env, причину, версию, PR и состояние задачи. Не отменять работающую задачу и не объединять историю автоматически.

Исторический дубль contract-extractor: incident e8d12426-bd60-4879-818f-f2eae34bb527 был закрыт штатным UI 13.11.2026 12:41 UTC с доказательством исправления. Задача1429 и PR4 сохранены; закрытие incident не означает принятия задачи или снятия attention. Актуальный статус надо перечитать перед следующим решением.

## Положительное доказательство восстановления

Resolver снимает максимум100 подходящих DEPLOY_FAILED за вызов. Кандидаты с доказательством фильтруются до LIMIT. Только open/claimed, связь через исходный deployment с тем же env/project. claimed_by и история сохраняются, resolved_at ставится один раз. Аудит deploy.status пишется в той же транзакции, UI перечитывает очередь обычным способом.

Local Docker: последнее поколение healthy running/desired running, ноль health failures, пустая причина, реальный container/image/port/health/check, завершённые active/completed publish/reconcile/restart и исходная успешная публикация с release. Исходная ошибка должна относиться к меньшему поколению. Старый HTTP200 после нового failure, cleanup, простое done и закрытие incident не являются доказательствами.

Обычная доставка: release-backed последний успешный nonrollback deployment с однозначным созданием/стартом после завершения ошибки и настроенной проверкой. Неоднозначная история остаётся открытой. Paused окружение не используется как witness. Resolver не снимает pause, не меняет incidents/tasks и отдельные credential/host/quality вопросы.

## Выкатка и проверка

До интеграции просмотреть фактический diff, независимое ревью Astra, CI и проверки принятых backend/web/e2e. Rollout только из принятого main с точными version/commit ldflags и ui-sync/ui-check. Не перезапускать рабочий control plane при активных coding/fix contexts. Drain, дождаться безопасного простоя, сохранить image/config и резервную копию БД. Не менять SQL вручную и не создавать фиктивную публикацию ради снятия карточки.

После обновления дать обычному tick выполнить bounded repair. Проверить resolved_at/claimed_by, единственный audit event, сохранность нового failure/другого env/paused/отдельных вопросов, обновление консоли. Доставка и продуктовая приёмка проверяются отдельно. Если paused=true остаётся непринятое решение о resume, его надо разрешить штатным API/guard, а не маскировать карточку.

Откат resolver/correlation не переписывает историю. Пересчёт fingerprints при откате не выполняется. Новые server modules имеют отдельные аддитивные миграции; старую версию допустимо запускать только после проверки совместимости и без удаления новых данных.
