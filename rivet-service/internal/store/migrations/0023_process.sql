-- Процесс задачи как данные (change add-process-model, спека backend/process).

-- Runner объявляет список моделей; прежняя колонка остаётся первой моделью
-- списка (старые клиенты и протокол v10).
ALTER TABLE runners ADD COLUMN IF NOT EXISTS models text[] NOT NULL DEFAULT '{}';
UPDATE runners SET models = ARRAY[model] WHERE cardinality(models) = 0 AND model <> '';

-- Текущий шаг задачи, снимок разрешённого процесса и хэш версии политики,
-- откуда он взят; вход на шаг (ok — с начала, changes — исправление) даёт
-- проекцию статуса coding/fixing; отказы по шагам — лимиты проходов.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS step_id text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS step_entry text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS process jsonb;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS process_hash text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS step_rejections jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS wait_reason text NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS step_gen int NOT NULL DEFAULT 0;
-- Поколение входа, чей исход уже применён: одновременные вердикты участников
-- не применяют исход дважды.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS step_closed_gen int NOT NULL DEFAULT 0;

-- Задачи, созданные до процесса: шаг по статусу; снимок процесса заполнит
-- движок при первом входе на шаг (процесс по умолчанию равен прежнему
-- конвейеру, поэтому семантика не меняется).
UPDATE tasks SET step_id = CASE status
    WHEN 'running' THEN 'code'
    WHEN 'fixing'  THEN 'code'
    WHEN 'testing' THEN 'test'
    WHEN 'review'  THEN 'review'
    ELSE '' END,
  step_entry = CASE status WHEN 'fixing' THEN 'changes' WHEN 'running' THEN 'ok' WHEN 'testing' THEN 'ok' WHEN 'review' THEN 'ok' ELSE '' END
WHERE step_id = '';

-- Запуски участников шага: одна строка на участника на проход. Без runner'а
-- и вердикта — ожидает назначения; с runner'ом без вердикта — идёт.
CREATE TABLE IF NOT EXISTS task_step_runs (
    id           bigserial PRIMARY KEY,
    task_id      uuid NOT NULL REFERENCES tasks(id),
    step_id      text NOT NULL,
    step_kind    text NOT NULL DEFAULT '',
    pass         int  NOT NULL DEFAULT 0,          -- поколение входа задачи на шаг (tasks.step_gen)
    participant  text NOT NULL,                    -- p1, p2, …
    agent_kind   text NOT NULL DEFAULT '',
    model        text NOT NULL DEFAULT '',
    capabilities text[] NOT NULL DEFAULT '{}',
    runner_id    text REFERENCES runners(id),
    session_id   uuid,
    verdict      text,                             -- ok | changes | fail | blocked | cancelled
    detail       text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz
);
CREATE INDEX IF NOT EXISTS task_step_runs_task ON task_step_runs(task_id, step_id);
CREATE INDEX IF NOT EXISTS task_step_runs_pending ON task_step_runs(created_at) WHERE runner_id IS NULL AND verdict IS NULL;
CREATE INDEX IF NOT EXISTS task_step_runs_session ON task_step_runs(session_id) WHERE session_id IS NOT NULL;

-- Сессия знает шаг процесса и участника (api-contract: Session.Step, Participant).
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS step_id text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS participant text NOT NULL DEFAULT '';
