-- Обзор проекта для руководителя (add-project-lead-view, спека
-- backend/project-overview): время начала и завершения задач и Epic'ов,
-- целевая дата Epic'а, момент начала ожидания runner'а, связь записи usage
-- с сессией стадии (разрез расхода по ролям) и индексы агрегатов.

ALTER TABLE tasks ADD COLUMN IF NOT EXISTS started_at timestamptz;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS done_at    timestamptz;
-- wait_since — с какого момента задача ждёт runner'а или агента (wait_reason
-- не пуст). Пороги «ждёт дольше N минут» считаются от него.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS wait_since timestamptz;
-- deferred_since — начало серии откладываний запуска: ставится первым
-- откладыванием, снимается доставкой. Назначения между откладываниями
-- очищают причину ожидания задачи, а начало ожидания берётся отсюда.
ALTER TABLE task_step_runs ADD COLUMN IF NOT EXISTS deferred_since timestamptz;

ALTER TABLE epics ADD COLUMN IF NOT EXISTS started_at timestamptz;
ALTER TABLE epics ADD COLUMN IF NOT EXISTS done_at    timestamptz;
ALTER TABLE epics ADD COLUMN IF NOT EXISTS due_at     timestamptz;

-- Сессия стадии, от которой пришла запись usage. Старые записи остаются без
-- сессии и в разрезе по ролям идут в «без роли».
ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS session_id uuid;

-- ─── заполнение по event log ────────────────────────────────────────────

-- Начало задачи — первый переход в работу, завершение — последний переход
-- в done (событие task.status с payload.status).
UPDATE tasks t SET started_at = ev.ts
FROM (SELECT task_id, min(ts) AS ts FROM events
      WHERE type = 'task.status' AND task_id IS NOT NULL
        AND payload->>'status' IN ('running', 'testing', 'review', 'fixing')
      GROUP BY task_id) ev
WHERE ev.task_id = t.id AND t.started_at IS NULL;

UPDATE tasks t SET done_at = ev.ts
FROM (SELECT task_id, max(ts) AS ts FROM events
      WHERE type = 'task.status' AND task_id IS NOT NULL AND payload->>'status' = 'done'
      GROUP BY task_id) ev
WHERE ev.task_id = t.id AND t.done_at IS NULL;

-- Завершённая задача без события (история, импорт): последняя правка строки.
UPDATE tasks SET done_at = updated_at WHERE status = 'done' AND done_at IS NULL;

-- Ожидание: последнее событие task.wait, иначе последняя правка строки.
UPDATE tasks t SET wait_since = COALESCE(
    (SELECT max(e.ts) FROM events e WHERE e.task_id = t.id AND e.type = 'task.wait'), t.updated_at)
WHERE t.wait_reason <> '' AND t.wait_since IS NULL;

UPDATE epics e SET started_at = ev.ts
FROM (SELECT epic_id, min(ts) AS ts FROM events
      WHERE type = 'epic.status' AND epic_id IS NOT NULL AND payload->>'status' = 'running'
      GROUP BY epic_id) ev
WHERE ev.epic_id = e.id AND e.started_at IS NULL;

UPDATE epics e SET done_at = ev.ts
FROM (SELECT epic_id, max(ts) AS ts FROM events
      WHERE type = 'epic.status' AND epic_id IS NOT NULL AND payload->>'status' = 'done'
      GROUP BY epic_id) ev
WHERE ev.epic_id = e.id AND e.done_at IS NULL;

-- Завершённый (или завершённый и архивированный) Epic без события:
-- завершение последней задачи.
UPDATE epics e SET done_at = (SELECT max(t.done_at) FROM tasks t WHERE t.epic_id = e.id)
WHERE e.done_at IS NULL AND e.status IN ('done', 'archived')
  AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.epic_id = e.id AND t.status NOT IN ('done', 'cancelled'));

-- ─── проставление в транзакции смены статуса ────────────────────────────

-- Триггер ставит время в той же транзакции, что меняет статус: так его не
-- пропустит ни одно место, которое меняет статус задачи или Epic'а.
-- started_at не перезаписывается при повторных назначениях.
CREATE OR REPLACE FUNCTION rivet_tasks_times() RETURNS trigger AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NEW.status IN ('running', 'testing', 'review', 'fixing') AND NEW.started_at IS NULL THEN
            NEW.started_at := now();
        END IF;
        IF NEW.status = 'done' THEN
            NEW.done_at := now();
        END IF;
    END IF;
    IF NEW.wait_reason IS DISTINCT FROM OLD.wait_reason THEN
        IF NEW.wait_reason = '' THEN
            NEW.wait_since := NULL;
        ELSIF NEW.wait_since IS NULL
           OR (COALESCE(OLD.wait_reason, '') = '' AND NEW.wait_since IS NOT DISTINCT FROM OLD.wait_since) THEN
            -- Начало ожидания. Явно заданное время (продолжение серии
            -- откладываний, DeferRun) сохраняется.
            NEW.wait_since := now();
        END IF;
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS tasks_times ON tasks;
CREATE TRIGGER tasks_times BEFORE UPDATE OF status, wait_reason ON tasks
    FOR EACH ROW EXECUTE FUNCTION rivet_tasks_times();

CREATE OR REPLACE FUNCTION rivet_epics_times() RETURNS trigger AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NEW.status = 'running' AND NEW.started_at IS NULL THEN
            NEW.started_at := now();
        END IF;
        IF NEW.status = 'done' THEN
            NEW.done_at := now();
        END IF;
    END IF;
    RETURN NEW;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS epics_times ON epics;
CREATE TRIGGER epics_times BEFORE UPDATE OF status ON epics
    FOR EACH ROW EXECUTE FUNCTION rivet_epics_times();

-- ─── индексы агрегатов обзора ───────────────────────────────────────────

CREATE INDEX IF NOT EXISTS events_by_project_type_ts ON events (project_id, type, ts);
CREATE INDEX IF NOT EXISTS tasks_by_epic_done ON tasks (epic_id, done_at);
-- Расход задач в обзоре считается по task_id: без индекса запрос на 500
-- задачах и 20 тыс. записей шёл 286 мс вместо 38 (design, замер 1.9).
-- Индексы по сессиям не нужны: сессии фильтруются по проекту через
-- задачи, роль записи usage ищется по task_step_runs_session.
CREATE INDEX IF NOT EXISTS usage_by_task ON usage_records (task_id) WHERE task_id IS NOT NULL;
