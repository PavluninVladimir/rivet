-- Архитектура проекта (add-architecture-c4, спека backend/project-architecture):
-- снимок описания с основной ветки, вид Epic и задачи, отметка автоописания.

-- Вид Epic и задачи: обычная работа, описание архитектуры, исследование
-- (add-epic-explore), сопровождение и баг (add-sdlc-cycle). Блок совпадает
-- дословно с 0032 (add-sdlc-cycle): миграции идут в любом порядке, и
-- каждая задаёт полный общий список, иначе вторая отвергла бы значения
-- первой и rivetd не стартовал бы.
ALTER TABLE epics ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'work';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'work';
ALTER TABLE epics ALTER COLUMN kind SET DEFAULT 'work';
ALTER TABLE tasks ALTER COLUMN kind SET DEFAULT 'work';
ALTER TABLE epics DROP CONSTRAINT IF EXISTS epics_kind_check;
ALTER TABLE epics ADD CONSTRAINT epics_kind_check CHECK (kind IN ('work', 'architecture', 'explore', 'maintenance'));
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check CHECK (kind IN ('work', 'architecture', 'explore', 'bug'));

-- Одна незавершённая задача описания на проект: держит частичный
-- уникальный индекс по Epic'ам вида architecture (Epic создаётся вместе
-- с единственной задачей и закрывается вместе с ней).
CREATE UNIQUE INDEX IF NOT EXISTS epics_architecture_active
    ON epics (project_id) WHERE kind = 'architecture' AND status IN ('planned', 'running', 'paused');

-- Вершина ветки задачи описания, на которой шёл шаг проверки: merge
-- привязан к ней, push после проверки возвращает задачу на проверку.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS checked_sha text NOT NULL DEFAULT '';

-- Точный код ситуации эскалации задачи (architecture_files_unavailable,
-- architecture_head_moved): открытая эскалация с тем же кодом одна, новое
-- сообщение заменяет старое. Общая причина TEST_FAILED для этого слишком
-- грубая.
ALTER TABLE attention ADD COLUMN IF NOT EXISTS code text NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS attention_task_code_open
    ON attention (task_id, code) WHERE status <> 'resolved' AND code <> '';

-- Автоописание срабатывает один раз на проект.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS architecture_auto_at timestamptz;

-- Версия набора событий подписки webhook (scm.HookEventsVersion): хуки,
-- заведённые до подписки на push, переподписываются при старте rivetd.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS webhook_events integer NOT NULL DEFAULT 0;
-- Время регистрации хука. Хук у репозитория один на URL, и секрет у него
-- того проекта, что регистрировался последним: переподписка трогает хук
-- только от имени этого проекта. Для прежних регистраций время неизвестно.
ALTER TABLE projects ADD COLUMN IF NOT EXISTS webhook_registered_at timestamptz;

-- Снимок архитектуры: текущая версия и последняя корректная. При invalid
-- и error поля последней корректной версии не меняются.
CREATE TABLE IF NOT EXISTS project_architecture (
    project_id    uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    status        text NOT NULL CHECK (status IN ('absent', 'ok', 'invalid', 'error')),
    sha           text NOT NULL DEFAULT '',
    read_at       timestamptz,
    error         text NOT NULL DEFAULT '',
    defects       jsonb NOT NULL DEFAULT '[]',
    data          jsonb,
    valid_sha     text NOT NULL DEFAULT '',
    valid_read_at timestamptz,
    valid_data    jsonb,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    -- Вид ошибки при status = error: host (сбой хостинга, тот же SHA
    -- перечитывается), limit, data, no_credentials (повтор даст то же).
    error_kind    text NOT NULL DEFAULT '',
    -- SHA дерева docs/architecture прошлого чтения: merge без изменений
    -- описания не перечитывает его целиком.
    tree_sha      text NOT NULL DEFAULT ''
);

-- Архитектор работает на runner'ах, которые сами объявляют capability
-- architecture (rivet-runner с подкомандой architecture). Встроенный
-- шаблон переводится с зарезервированной explore, если её не меняли.
UPDATE agent_roles SET capability = 'architecture'
WHERE id = 'architect' AND builtin AND capability = 'explore';
