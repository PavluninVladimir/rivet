-- Дирижирование внешними системами доставки (change add-external-delivery,
-- спека deployment).

-- Тип исполнения окружения: доставку может выполнять пайплайн хостинга.
ALTER TABLE environments DROP CONSTRAINT environments_exec_type_check;
ALTER TABLE environments ADD CONSTRAINT environments_exec_type_check
    CHECK (exec_type IN ('ssh', 'pipeline'));

-- Прогон внешнего пайплайна: по идентификатору Rivet опрашивает состояние,
-- ссылку показывает человеку. Пусто у собственной доставки.
ALTER TABLE deployments ADD COLUMN external_run_id text NOT NULL DEFAULT '';
ALTER TABLE deployments ADD COLUMN external_url    text NOT NULL DEFAULT '';
