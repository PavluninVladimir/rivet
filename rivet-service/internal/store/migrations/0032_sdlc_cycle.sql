-- Цикл разработки агентами (add-sdlc-cycle): Релиз-инженер и Инженер
-- сопровождения, релизы с release notes, наблюдение за окружениями,
-- инциденты и задачи-баги в Epic'е «Сопровождение».

-- Шаблоны ролей devops и sre работают запросом control plane, как Аналитик
-- и Тимлид: capability пустая, runner им не нужен.
INSERT INTO agent_roles (id, title, description, instruction, color, capability, builtin, position) VALUES
  ('devops', 'Релиз-инженер',
   'Выпускает версии: пишет release notes по составу релиза и разбирает провалы публикации.',
   'Ты релиз-инженер. Release notes пиши только по задачам состава версии и языком пользователя продукта: что нового, что исправлено, без технического шума. Каждый пункт ссылайся на задачу состава. Провал публикации разбирай по логу: называй вероятную причину, затронутые задачи состава и что проверить, без догадок сверх доказательств.',
   'green', '', true, 70),
  ('sre', 'Инженер сопровождения',
   'Следит за работающим приложением, описывает сбои и ставит задачи на исправление.',
   'Ты инженер сопровождения. Описывай сбой по доказательствам: выводу проверки и логам. Отделяй факты от предположений: что видно в доказательствах, а что вероятная причина. Формулируй задачу-баг так, чтобы разработчик исправил её без дополнительных вопросов, с проверяемыми критериями. Не предлагай действий с окружением вроде перезапуска или отката: их выполняет только детерминированный механизм Rivet.',
   'slate', '', true, 80)
ON CONFLICT (id) DO NOTHING;

-- Агенты Дина и Глеб, только если агентов этих шаблонов ещё нет.
INSERT INTO team_agents (id, name, role_id, position)
SELECT 'dina', 'Дина', 'devops', 80
WHERE NOT EXISTS (SELECT 1 FROM team_agents WHERE role_id = 'devops')
  AND NOT EXISTS (SELECT 1 FROM team_agents WHERE id = 'dina');
INSERT INTO team_agents (id, name, role_id, position)
SELECT 'gleb', 'Глеб', 'sre', 90
WHERE NOT EXISTS (SELECT 1 FROM team_agents WHERE role_id = 'sre')
  AND NOT EXISTS (SELECT 1 FROM team_agents WHERE id = 'gleb');

-- Вид Epic'а и задачи. Колонки заводит и change архитектуры (0029): блок
-- общий и идемпотентный, тексты в 0029 и 0032 совпадают, поэтому порядок
-- применения не важен.
ALTER TABLE epics ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'work';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS kind text NOT NULL DEFAULT 'work';
ALTER TABLE epics ALTER COLUMN kind SET DEFAULT 'work';
ALTER TABLE tasks ALTER COLUMN kind SET DEFAULT 'work';
ALTER TABLE epics DROP CONSTRAINT IF EXISTS epics_kind_check;
ALTER TABLE epics ADD CONSTRAINT epics_kind_check CHECK (kind IN ('work', 'architecture', 'explore', 'maintenance'));
ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_kind_check;
ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check CHECK (kind IN ('work', 'architecture', 'explore', 'bug'));

-- Точка merge: состав релиза считается по времени merge. Бэкфилл: момент
-- последнего перехода в done по журналу, иначе время обновления строки.
ALTER TABLE tasks ADD COLUMN merged_at timestamptz;
UPDATE tasks t SET merged_at = COALESCE(
    (SELECT max(ev.ts) FROM events ev
     WHERE ev.task_id = t.id AND ev.type = 'task.status' AND ev.payload->>'status' = 'done'),
    t.updated_at)
WHERE t.status = 'done'
  -- История, импортированная из описания проекта, в релизы не входит.
  AND NOT EXISTS (SELECT 1 FROM epics e WHERE e.id = t.epic_id AND e.source_key IS NOT NULL);
CREATE INDEX tasks_merged ON tasks(merged_at) WHERE merged_at IS NOT NULL;

-- Момент версии публикации: создание или последняя коалесценция queued.
-- Состав релиза считается по нему, а не по created_at: merge, пока
-- публикация стояла в очереди, попадает в её версию.
ALTER TABLE deployments ADD COLUMN version_at timestamptz;
UPDATE deployments SET version_at = created_at;
ALTER TABLE deployments ALTER COLUMN version_at SET DEFAULT now();
ALTER TABLE deployments ALTER COLUMN version_at SET NOT NULL;
-- Граница релиза (последняя успешная публикация) на каждое событие задачи.
CREATE INDEX deployments_done_version ON deployments(env_id, version_at DESC) WHERE status = 'done';
-- Инцидент провала публикации не записался (сбой сопровождения не
-- откатывает паузу и эскалацию): тик повторяет запись с растущей паузой,
-- не больше пяти попыток.
ALTER TABLE deployments ADD COLUMN incident_pending boolean NOT NULL DEFAULT false;
ALTER TABLE deployments ADD COLUMN incident_attempts integer NOT NULL DEFAULT 0;
ALTER TABLE deployments ADD COLUMN incident_retry_at timestamptz;
CREATE INDEX deployments_incident_pending ON deployments(incident_retry_at) WHERE incident_pending;

-- Релиз: успешная публикация окружения с составом и release notes.
CREATE TABLE releases (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id     uuid NOT NULL REFERENCES projects(id),
    env_id         uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    deployment_id  uuid NOT NULL UNIQUE REFERENCES deployments(id) ON DELETE CASCADE,
    version        text NOT NULL,
    prev_version   text NOT NULL DEFAULT '',
    initiator      text NOT NULL DEFAULT 'auto',
    created_at     timestamptz NOT NULL DEFAULT now(),
    composition    jsonb NOT NULL DEFAULT '[]',
    notes_md       text NOT NULL DEFAULT '',
    notes_status   text NOT NULL DEFAULT 'pending' CHECK (notes_status IN ('pending', 'ready', 'template')),
    notes_agent_id text REFERENCES team_agents(id) ON DELETE SET NULL,
    notes_model    text NOT NULL DEFAULT '',
    dropped_items  integer NOT NULL DEFAULT 0,
    -- Когда запрошены release notes: залежавшееся задание поднимается заново.
    notes_requested_at timestamptz NOT NULL DEFAULT now(),
    -- Попытки задания: после трёх неудач release notes по шаблону.
    notes_attempts integer NOT NULL DEFAULT 0,
    -- Состав по времени merge у публикации, запущенной человеком.
    by_time        boolean NOT NULL DEFAULT false
);
CREATE INDEX releases_by_project ON releases(project_id, created_at DESC);
CREATE INDEX releases_by_env ON releases(env_id, created_at DESC);

-- Инцидент: сбой проверки наблюдения или провал публикации.
CREATE TABLE incidents (
    id                        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id                uuid NOT NULL REFERENCES projects(id),
    env_id                    uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    source                    text NOT NULL CHECK (source IN ('monitor', 'release')),
    fingerprint               text NOT NULL,
    status                    text NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'mitigated', 'resolved')),
    severity                  text NOT NULL DEFAULT 'medium' CHECK (severity IN ('medium', 'high')),
    title                     text NOT NULL DEFAULT '',
    error_line                text NOT NULL DEFAULT '',
    first_seen                timestamptz NOT NULL DEFAULT now(),
    last_seen                 timestamptz NOT NULL DEFAULT now(),
    occurrences               integer NOT NULL DEFAULT 1,
    evidence                  jsonb NOT NULL DEFAULT '{}',
    description               jsonb NOT NULL DEFAULT '{}',
    description_status        text NOT NULL DEFAULT 'pending' CHECK (description_status IN ('pending', 'ready', 'template')),
    description_agent_id      text REFERENCES team_agents(id) ON DELETE SET NULL,
    description_requested_at  timestamptz NOT NULL DEFAULT now(),
    description_attempts      integer NOT NULL DEFAULT 0,
    bug_task_id               uuid REFERENCES tasks(id) ON DELETE SET NULL,
    bug_cancelled_by          text NOT NULL DEFAULT '',
    skipped_daily             boolean NOT NULL DEFAULT false,
    needs_human               boolean NOT NULL DEFAULT false,
    deployment_id             uuid REFERENCES deployments(id) ON DELETE SET NULL,
    mitigated_at              timestamptz,
    occurrences_at_mitigation integer NOT NULL DEFAULT 0,
    mitigated_release_id      uuid REFERENCES releases(id) ON DELETE SET NULL,
    resolved_at               timestamptz,
    resolution                text NOT NULL DEFAULT '' CHECK (resolution IN ('', 'fixed', 'self', 'manual')),
    -- bug_pending — решение «создать баг» ждёт описания Инженера
    -- сопровождения: задача создаётся, когда описание готово.
    bug_pending               boolean NOT NULL DEFAULT false
);
-- Один активный инцидент на отпечаток: защита от гонки двух проверок.
CREATE UNIQUE INDEX incidents_active_fingerprint ON incidents(project_id, fingerprint)
    WHERE status IN ('open', 'mitigated');
CREATE INDEX incidents_by_project ON incidents(project_id, status, last_seen DESC);
CREATE INDEX incidents_by_fingerprint ON incidents(project_id, fingerprint, resolved_at DESC);

ALTER TABLE tasks ADD COLUMN incident_id uuid REFERENCES incidents(id) ON DELETE SET NULL;
CREATE INDEX tasks_by_incident ON tasks(incident_id) WHERE incident_id IS NOT NULL;

-- История статусов инцидента.
CREATE TABLE incident_events (
    id          bigserial PRIMARY KEY,
    incident_id uuid NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    at          timestamptz NOT NULL DEFAULT now(),
    status      text NOT NULL,
    actor       text NOT NULL DEFAULT 'system',
    note        text NOT NULL DEFAULT ''
);
CREATE INDEX incident_events_by_incident ON incident_events(incident_id, id);

-- Состояние наблюдения окружения и история проверок (ретенция 7 суток).
CREATE TABLE monitor_state (
    env_id           uuid PRIMARY KEY REFERENCES environments(id) ON DELETE CASCADE,
    state            text NOT NULL DEFAULT 'unknown' CHECK (state IN ('ok', 'fail', 'unknown', 'disabled')),
    reason           text NOT NULL DEFAULT '',
    next_at          timestamptz NOT NULL DEFAULT now(),
    pending_check_id text NOT NULL DEFAULT '',
    pending_runner   text NOT NULL DEFAULT '',
    pending_since    timestamptz,
    last_at          timestamptz,
    last_output      text NOT NULL DEFAULT '',
    consecutive_fail integer NOT NULL DEFAULT 0,
    consecutive_ok   integer NOT NULL DEFAULT 0,
    last_change_at   timestamptz NOT NULL DEFAULT now(),
    -- recheck_at — повторная проверка через 2 минуты после публикации.
    recheck_at       timestamptz
);

CREATE TABLE monitor_runs (
    id          bigserial PRIMARY KEY,
    env_id      uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    at          timestamptz NOT NULL DEFAULT now(),
    ok          boolean NOT NULL,
    duration_ms integer NOT NULL DEFAULT 0,
    output      text NOT NULL DEFAULT ''
);
CREATE INDEX monitor_runs_by_env ON monitor_runs(env_id, at DESC);

-- Эскалация «лимит сопровождения» уровня проекта: ни задачи, ни публикации.
-- Одна открытая на проект держит существующий индекс attention_open_project_reason.
ALTER TABLE attention DROP CONSTRAINT attention_subject;
ALTER TABLE attention ADD CONSTRAINT attention_subject CHECK (
    (task_id IS NOT NULL AND deployment_id IS NULL) OR
    (task_id IS NULL AND deployment_id IS NOT NULL) OR
    (task_id IS NULL AND deployment_id IS NULL AND reason IN ('POLICY_ENGINE', 'POLICY_SOURCE', 'OPS_LIMIT')));

-- Usage заданий control plane: роль шаблона (devops, sre) вместо
-- псевдо-runner'а пишется в usage_records.role из 0031 (add-product-discovery,
-- NULL у стадий runner'ов). Своей колонки 0032 не добавляет, а колонку
-- приводит к виду 0031 (nullable, без умолчания), если её создали иначе:
-- ранняя редакция 0032 делала NOT NULL DEFAULT '', и запись стадии с NULL
-- на такой колонке падает. На колонке из 0031 команда ничего не меняет.
-- База, где ранняя 0032 уже записана в schema_migrations, эту миграцию
-- повторно не выполняет, и колонок, добавленных в 0032 позже, в ней тоже
-- нет: такую базу нужно пересоздать.
ALTER TABLE usage_records ALTER COLUMN role DROP NOT NULL, ALTER COLUMN role DROP DEFAULT;

-- Агрегат цикла и хук фазы событий: утверждение планов (epic.status) и
-- последняя фаза задачи читаются по частичным индексам.
CREATE INDEX events_epic_status ON events(epic_id, ts) WHERE type = 'epic.status';
CREATE INDEX events_project_epic_status ON events(project_id, ts) WHERE type = 'epic.status';
CREATE INDEX events_task_phase ON events(task_id, id) WHERE payload ? 'phase_to';
