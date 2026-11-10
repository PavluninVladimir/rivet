-- Состояние учётных данных хостинга (change fix-credential-state, спека
-- scm-integration «Учётные данные хостинга»): отказ хостинга по
-- авторизации делает подключение нерабочим с причиной, а не роняет задачи.

-- Причина нерабочего состояния: код (bad_token, no_access, ...) и текст
-- для человека. У рабочего подключения обе пустые.
-- recheck_at — последняя повторная проверка нерабочих учётных данных при
-- любом её исходе: недоступный хостинг не держит очередь проверок.
ALTER TABLE scm_credentials
    ADD COLUMN state_reason  text NOT NULL DEFAULT '',
    ADD COLUMN state_message text NOT NULL DEFAULT '',
    ADD COLUMN recheck_at    timestamptz;

-- Эскалация «учётные данные хостинга не работают» — уровня проекта: одна
-- открытая на проект держит индекс attention_open_project_reason.
DO $$
DECLARE def text;
BEGIN
    SELECT pg_get_constraintdef(oid) INTO def FROM pg_constraint
    WHERE conname = 'attention_subject' AND conrelid = 'attention'::regclass;
    IF def IS NULL OR def NOT LIKE 'CHECK %' THEN
        RAISE EXCEPTION 'attention_subject: неожиданное определение %', def;
    END IF;
    EXECUTE 'ALTER TABLE attention DROP CONSTRAINT attention_subject';
    EXECUTE 'ALTER TABLE attention ADD CONSTRAINT attention_subject CHECK ('
        || substr(def, 7)
        || ' OR (task_id IS NULL AND deployment_id IS NULL AND reason = ''SCM_CREDENTIALS''))';
END $$;

-- Задача ждёт восстановления подключения с закреплённым исходом шага
-- (wait_kind = 'credentials'): куда она шла (шаг и вход) и когда её взялся
-- продолжать проход. Захват старше аренды считается брошенным (сбой,
-- рестарт) и подхватывается снова.
-- wait_failures — неудачные продолжения подряд: постоянный отказ хостинга
-- после восстановления подключения эскалируется, а не повторяется вечно.
ALTER TABLE tasks
    ADD COLUMN wait_target     text NOT NULL DEFAULT '',
    ADD COLUMN wait_entry      text NOT NULL DEFAULT '',
    ADD COLUMN wait_claimed_at timestamptz,
    ADD COLUMN wait_failures   integer NOT NULL DEFAULT 0;
