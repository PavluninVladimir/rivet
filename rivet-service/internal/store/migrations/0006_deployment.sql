-- Публикация окружений (change implement-deployment, спека backend/deployment).

-- Окружение публикации проекта: тип исполнения пока один (ssh), enum открыт
-- для будущих (k8s, ci, gitops). config — несекретные параметры исполнения
-- и Verify; секреты хоста живут у deploy-runner'а, не в системе.
CREATE TABLE environments (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id uuid NOT NULL REFERENCES projects(id),
    name       text NOT NULL,
    exec_type  text NOT NULL DEFAULT 'ssh' CHECK (exec_type IN ('ssh')),
    trigger    text NOT NULL DEFAULT 'manual' CHECK (trigger IN ('auto','manual')),
    config     jsonb NOT NULL DEFAULT '{}',
    paused     boolean NOT NULL DEFAULT false, -- после провала автопубликации стоят
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, name)
);

-- Публикация: created_at — постановка в очередь, started_at — взята
-- runner'ом (отделяет ожидание от исполнения), ended_at — финал.
CREATE TABLE deployments (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    env_id     uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    version    text NOT NULL,                  -- sha публикуемого коммита
    status     text NOT NULL DEFAULT 'queued'
               CHECK (status IN ('queued','deploying','verifying','done','failed','rolled_back')),
    initiator  text NOT NULL DEFAULT 'auto',   -- login участника или auto
    runner_id  text NOT NULL DEFAULT '',
    detail     text NOT NULL DEFAULT '',
    -- откат выполняется той же публикацией: флаг долговечен (переживает
    -- рестарт rivetd), detail на время отката хранит причину провала
    rollback   boolean NOT NULL DEFAULT false,
    log_ref    text,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    ended_at   timestamptz
);
CREATE INDEX deployments_by_env ON deployments(env_id, created_at DESC);

-- Инварианты очереди в БД: не больше одной queued и одной активной
-- публикации на окружение — конкурентные enqueue упираются в индекс,
-- коалесценция делается через ON CONFLICT по queued-индексу.
CREATE UNIQUE INDEX deployments_one_queued ON deployments(env_id) WHERE status = 'queued';
CREATE UNIQUE INDEX deployments_one_active ON deployments(env_id) WHERE status IN ('deploying','verifying');

-- Эскалация публикации не привязана к задаче: task_id ослабляется,
-- добавляется deployment_id, указан ровно один из двух.
ALTER TABLE attention ALTER COLUMN task_id DROP NOT NULL;
ALTER TABLE attention ADD COLUMN deployment_id uuid REFERENCES deployments(id) ON DELETE CASCADE;
ALTER TABLE attention ADD CONSTRAINT attention_subject CHECK (
    (task_id IS NOT NULL AND deployment_id IS NULL) OR
    (task_id IS NULL AND deployment_id IS NOT NULL));

-- Занятость runner'а публикацией: статус deploying (дельта domain-model)
-- и ссылка на выполняемую публикацию.
ALTER TABLE runners DROP CONSTRAINT runners_status_check;
ALTER TABLE runners ADD CONSTRAINT runners_status_check
    CHECK (status IN ('idle','running','testing','review','deploying','offline'));
ALTER TABLE runners ADD COLUMN deployment_id uuid REFERENCES deployments(id) ON DELETE SET NULL;
