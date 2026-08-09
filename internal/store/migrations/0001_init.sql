-- Базовая схема вертикального среза.
-- Терминология и статусы — по openspec/specs/backend/domain-model.

CREATE TABLE projects (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    repo       text NOT NULL,            -- owner/name на GitHub
    checks     jsonb NOT NULL DEFAULT '[]', -- команды этапа testing: [{"name","cmd"}]
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE epics (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id),
    title      text NOT NULL,
    goal       text NOT NULL DEFAULT '',
    status     text NOT NULL DEFAULT 'planned'
               CHECK (status IN ('planned','running','paused','done','archived')),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE SEQUENCE task_number_seq;

CREATE TABLE tasks (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    epic_id       uuid NOT NULL REFERENCES epics(id),
    num           bigint NOT NULL DEFAULT nextval('task_number_seq'), -- agent/task-<num>
    title         text NOT NULL,
    description   text NOT NULL DEFAULT '',
    status        text NOT NULL DEFAULT 'queued'
                  CHECK (status IN ('queued','ready','running','testing','review',
                                    'fixing','blocked','failed','done','cancelled')),
    estimate      int  NOT NULL DEFAULT 1,          -- трудоёмкость для взвешенного прогресса
    capabilities  text[] NOT NULL DEFAULT '{coding}',
    criteria      jsonb NOT NULL DEFAULT '[]',      -- [{"text","ok"}]
    attempt_used  int NOT NULL DEFAULT 0,
    attempt_limit int NOT NULL DEFAULT 3,
    runner_id     text,
    branch        text,
    pr_url        text,
    block_reason  text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX tasks_num ON tasks(num);
CREATE INDEX tasks_by_epic ON tasks(epic_id);
CREATE INDEX tasks_by_status ON tasks(status);

CREATE TABLE task_deps (
    task_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    dep_id  uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    PRIMARY KEY (task_id, dep_id),
    CHECK (task_id <> dep_id)
);

CREATE TABLE runners (
    id           text PRIMARY KEY,                  -- имя, которое runner объявляет при Register
    agent        text NOT NULL,
    model        text NOT NULL DEFAULT '',
    host         text NOT NULL DEFAULT '',
    capabilities text[] NOT NULL DEFAULT '{coding}',
    status       text NOT NULL DEFAULT 'idle'
                 CHECK (status IN ('idle','running','testing','review','offline')),
    task_id      uuid REFERENCES tasks(id),
    ctx_pct      int NOT NULL DEFAULT 0,
    draining     boolean NOT NULL DEFAULT false,
    last_seen    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id        uuid REFERENCES tasks(id),
    attempt        int,
    driver_kind    text NOT NULL CHECK (driver_kind IN ('user','scheduler')),
    driver_id      text NOT NULL DEFAULT '',
    agent          text NOT NULL,
    model          text NOT NULL DEFAULT '',
    depth          text NOT NULL DEFAULT 'minimal' CHECK (depth IN ('full','partial','minimal')),
    scope          text,
    transcript_ref text,
    tokens         bigint NOT NULL DEFAULT 0,
    started_at     timestamptz NOT NULL DEFAULT now(),
    ended_at       timestamptz
);
CREATE INDEX sessions_by_task ON sessions(task_id);

-- Event log: append-only, источник истины для активности и метеринга.
CREATE TABLE events (
    id         bigserial PRIMARY KEY,               -- монотонный курсор SSE/пагинации
    ts         timestamptz NOT NULL DEFAULT now(),
    actor_kind text NOT NULL CHECK (actor_kind IN ('runner','scheduler','system','user')),
    actor_id   text NOT NULL DEFAULT '',
    type       text NOT NULL,
    project_id uuid NOT NULL REFERENCES projects(id),
    epic_id    uuid REFERENCES epics(id),
    task_id    uuid REFERENCES tasks(id),
    text       text NOT NULL DEFAULT '',
    payload    jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX events_by_project ON events(project_id, id);
CREATE INDEX events_by_task ON events(task_id, id);

CREATE TABLE attention (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id),
    task_id    uuid NOT NULL REFERENCES tasks(id),
    reason     text NOT NULL,                       -- BLOCKED / REVIEW_LIMIT / TEST_FAILED / ...
    message    text NOT NULL,
    status     text NOT NULL DEFAULT 'open' CHECK (status IN ('open','claimed','resolved')),
    claimed_by text,
    created_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz
);
CREATE INDEX attention_open ON attention(status) WHERE status <> 'resolved';

-- Метеринг: идемпотентность по source_msg_id (billing-grade, спека monetization).
CREATE TABLE usage_records (
    id            bigserial PRIMARY KEY,
    ts            timestamptz NOT NULL DEFAULT now(),
    source_msg_id text UNIQUE,                      -- msg_id runner'а; NULL для системных записей
    project_id    uuid NOT NULL REFERENCES projects(id),
    epic_id       uuid REFERENCES epics(id),
    task_id       uuid REFERENCES tasks(id),
    runner_id     text,
    model         text NOT NULL DEFAULT '',
    tokens_in     bigint NOT NULL DEFAULT 0,
    tokens_out    bigint NOT NULL DEFAULT 0,
    cost_usd      numeric(12,6) NOT NULL DEFAULT 0,
    duration_s    int NOT NULL DEFAULT 0
);
CREATE INDEX usage_by_epic ON usage_records(epic_id);
