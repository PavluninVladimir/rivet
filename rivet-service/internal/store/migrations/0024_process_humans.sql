-- Участники-люди в процессе (change add-process-humans, спека backend/process
-- «Очередь шагов человека»): запуск человека без runner'а и сессии,
-- адресован логином или ролью проекта; вердикт с автором.
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS user_login text NOT NULL DEFAULT '';
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS user_role text NOT NULL DEFAULT '';
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS verdict_by text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS task_step_runs_humans ON task_step_runs(created_at)
    WHERE verdict IS NULL AND (user_login <> '' OR user_role <> '');
