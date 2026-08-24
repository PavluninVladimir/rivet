-- Командная видимость (change add-team-visibility, спека team-visibility).

-- Запрос сессии (снимок названия и описания задачи на момент запуска),
-- итог (текст результата стадии или вопрос blocked) и последний шаг —
-- для реестра активных сессий и поиска по истории без join'ов по event log.
ALTER TABLE sessions ADD COLUMN prompt    text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN outcome   text NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN last_step text NOT NULL DEFAULT '';

-- Поиск по истории: FTS по запросу и итогу (русская конфигурация).
CREATE INDEX sessions_fts ON sessions
    USING gin (to_tsvector('russian', prompt || ' ' || outcome));

-- Реестр активных сессий проекта: выборка ended_at IS NULL по задачам.
CREATE INDEX sessions_open ON sessions(task_id) WHERE ended_at IS NULL;

-- Пересечения работ: files && по активным сессиям на каждом шаге с файлами.
CREATE INDEX sessions_open_files ON sessions USING gin (files) WHERE ended_at IS NULL;
