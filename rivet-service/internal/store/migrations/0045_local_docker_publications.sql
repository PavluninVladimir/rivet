-- Локальная доставка сохраняет поколения и ожидаемое состояние отдельно
-- от фактического состояния Docker. История выкаток остаётся общей.
ALTER TABLE environments DROP CONSTRAINT environments_exec_type_check;
ALTER TABLE environments ADD CONSTRAINT environments_exec_type_check
    CHECK (exec_type IN ('ssh', 'pipeline', 'k8s', 'gitops', 'local_docker'));

CREATE TABLE local_publications (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
    deployment_id uuid UNIQUE REFERENCES deployments(id) ON DELETE SET NULL,
    version text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    status text NOT NULL DEFAULT 'queued' CHECK (status IN
      ('queued', 'building', 'starting', 'verifying', 'running', 'unhealthy',
       'stopping', 'stopped', 'failed', 'unknown')),
    desired_state text NOT NULL DEFAULT 'running' CHECK (desired_state IN ('running', 'stopped')),
    attempt integer NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    image_digest text NOT NULL DEFAULT '',
    container_id text NOT NULL DEFAULT '',
    host_port integer CHECK (host_port BETWEEN 20000 AND 29999),
    health_url text NOT NULL DEFAULT '',
    reason jsonb,
    log_ref text NOT NULL DEFAULT '',
    deadline_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    UNIQUE (env_id, generation)
);

CREATE UNIQUE INDEX local_publications_active ON local_publications(env_id)
    WHERE status IN ('queued', 'building', 'starting', 'verifying', 'stopping');
CREATE INDEX local_publications_history ON local_publications(env_id, generation DESC);

-- Снимок конфигурации поколения не меняется при правке окружения.
ALTER TABLE local_publications
 ADD COLUMN config jsonb NOT NULL DEFAULT '{}',
 ADD COLUMN runner_id text NOT NULL DEFAULT '',
 ADD COLUMN active_deployment_id uuid UNIQUE REFERENCES deployments(id) ON DELETE SET NULL,
 ADD COLUMN operation text NOT NULL DEFAULT 'publish' CHECK(operation IN ('publish','restart','stop','reconcile','logs','remove','cleanup')),
 ADD COLUMN checked_at timestamptz,
 ADD COLUMN completed_operation_id uuid,
 ADD COLUMN health_failures integer NOT NULL DEFAULT 0,
 ADD COLUMN builder_log text NOT NULL DEFAULT '' CHECK(octet_length(builder_log)<=2097152),
 ADD COLUMN container_log text NOT NULL DEFAULT '' CHECK(octet_length(container_log)<=2097152);

CREATE TABLE local_pending_publications (
 env_id uuid PRIMARY KEY REFERENCES environments(id) ON DELETE CASCADE,
 version text NOT NULL
);

ALTER TABLE environments ADD COLUMN local_delete_requested boolean NOT NULL DEFAULT false, ADD COLUMN local_cleanup_completed boolean NOT NULL DEFAULT false;
-- Прямое каскадное удаление не должно оставлять управляемые Docker-объекты.
CREATE FUNCTION protect_local_publication_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM local_publications WHERE env_id=OLD.id AND (container_id<>'' OR image_digest<>'')) THEN
  RAISE EXCEPTION 'сначала очистите локальную публикацию окружения' USING ERRCODE='23514';
 END IF;
 RETURN OLD;
END $$;
CREATE TRIGGER protect_local_publication_delete BEFORE DELETE ON environments
 FOR EACH ROW EXECUTE FUNCTION protect_local_publication_delete();

CREATE TABLE local_environment_preparations (
 env_id uuid PRIMARY KEY REFERENCES environments(id) ON DELETE CASCADE,
 deployment_id uuid UNIQUE REFERENCES deployments(id) ON DELETE SET NULL,
 status text NOT NULL DEFAULT 'queued' CHECK(status IN('queued','preparing','waiting_artifact','failed')),
 reason text NOT NULL DEFAULT '',
 updated_at timestamptz NOT NULL DEFAULT now()
);
