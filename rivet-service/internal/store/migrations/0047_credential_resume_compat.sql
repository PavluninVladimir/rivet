-- Ранние сборки fix-credential-state могли применить 0038 до добавления
-- полей продолжения. Применённые миграции повторно не исполняются.
ALTER TABLE scm_credentials
    ADD COLUMN IF NOT EXISTS state_reason text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS state_message text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS recheck_at timestamptz;
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS wait_target text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS wait_entry text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS wait_claimed_at timestamptz,
    ADD COLUMN IF NOT EXISTS wait_failures integer NOT NULL DEFAULT 0;
