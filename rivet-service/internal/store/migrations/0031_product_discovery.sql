-- Исследование продукта (add-product-discovery, спека backend/product-discovery):
-- интервью пользователей, версии исследования Аналитиком, кэш конспектов,
-- связь Epic'ов с потребностями и зависимости между Epic'ами.

-- Интервью: нормализованный текст хранится как есть, не меняется после
-- добавления. Удаление мягкое: цитаты прошлых версий остаются видны.
CREATE TABLE discovery_interviews (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title           text NOT NULL,
    respondent_role text NOT NULL DEFAULT '',
    held_at         date,
    text            text NOT NULL,
    sha256          text NOT NULL,
    chars           int  NOT NULL,
    lines           int  NOT NULL,
    created_by      uuid REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    deleted_at      timestamptz
);
CREATE INDEX discovery_interviews_project ON discovery_interviews(project_id, created_at)
    WHERE deleted_at IS NULL;

-- Версии исследования. result — ответ модели после проверки (не меняется),
-- backlog — бэклог с правками человека и ревизией.
CREATE TABLE discovery_runs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id      uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    version         int  NOT NULL,
    status          text NOT NULL CHECK (status IN ('running','ready','failed','approved','superseded')),
    stage           text NOT NULL DEFAULT 'summaries' CHECK (stage IN ('summaries','synthesis','done')),
    progress_done   int  NOT NULL DEFAULT 0,
    progress_total  int  NOT NULL DEFAULT 0,
    interviews      jsonb NOT NULL DEFAULT '[]',
    agent_id        text NOT NULL DEFAULT '',
    agent_name      text NOT NULL DEFAULT '',
    role_title      text NOT NULL DEFAULT '',
    model           text NOT NULL DEFAULT '',
    connection_id   text NOT NULL DEFAULT '',
    instruction_sha text NOT NULL DEFAULT '',
    result          jsonb,
    dropped         jsonb NOT NULL DEFAULT '[]',
    defects         jsonb NOT NULL DEFAULT '[]',
    backlog         jsonb,
    backlog_rev     int  NOT NULL DEFAULT 0,
    outdated        boolean NOT NULL DEFAULT false,
    error           text NOT NULL DEFAULT '',
    tokens_in       bigint,
    tokens_out      bigint,
    cost_usd        numeric(12,6),
    duration_s      int  NOT NULL DEFAULT 0,
    -- approved_epics — [{key, epic_id, action}], decompose — {key: {status, detail}}.
    approved_epics  jsonb,
    decompose       jsonb NOT NULL DEFAULT '{}',
    started_at      timestamptz NOT NULL DEFAULT now(),
    finished_at     timestamptz,
    approved_at     timestamptz,
    approved_by     uuid REFERENCES users(id),
    UNIQUE (project_id, version)
);
-- Не больше одного анализа проекта одновременно: второй запуск получает
-- конфликт на вставке, без гонок (design, решение 5).
CREATE UNIQUE INDEX discovery_runs_one_running ON discovery_runs(project_id) WHERE status = 'running';

-- Кэш конспектов: текст интервью, модель и инструкция Аналитика.
CREATE TABLE discovery_summaries (
    interview_sha256 text NOT NULL,
    model            text NOT NULL,
    instruction_sha  text NOT NULL,
    summary          jsonb NOT NULL,
    dropped          jsonb NOT NULL DEFAULT '[]',
    created_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (interview_sha256, model, instruction_sha)
);

-- Потребности Epic'а: снимок потребности с цитатами, чтобы обоснование
-- было видно и после того, как версия стала superseded.
CREATE TABLE epic_needs (
    epic_id uuid NOT NULL REFERENCES epics(id) ON DELETE CASCADE,
    run_id  uuid NOT NULL REFERENCES discovery_runs(id) ON DELETE CASCADE,
    need_id text NOT NULL,
    need    jsonb NOT NULL,
    PRIMARY KEY (epic_id, run_id, need_id)
);

-- Зависимости между Epic'ами одного проекта (проверка проекта — в коде).
CREATE TABLE epic_deps (
    epic_id    uuid NOT NULL REFERENCES epics(id) ON DELETE CASCADE,
    depends_on uuid NOT NULL REFERENCES epics(id) ON DELETE CASCADE,
    PRIMARY KEY (epic_id, depends_on),
    CHECK (epic_id <> depends_on)
);
CREATE INDEX epic_deps_depends_on ON epic_deps(depends_on);

ALTER TABLE epics ADD COLUMN IF NOT EXISTS priority text CHECK (priority IN ('must','should','could'));
ALTER TABLE epics ADD COLUMN IF NOT EXISTS discovery_run_id uuid REFERENCES discovery_runs(id) ON DELETE SET NULL;

-- Роль, от имени которой выполнен запрос control plane (analyst); NULL у
-- прежних записей.
ALTER TABLE usage_records ADD COLUMN IF NOT EXISTS role text;
