-- Load testing is independent of coding and deployment queues. A durable
-- question never implies permission to send traffic.
CREATE TABLE load_test_plans (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 env_id uuid NOT NULL UNIQUE REFERENCES environments(id) ON DELETE CASCADE,
 first_release_id uuid NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
 state text NOT NULL DEFAULT 'awaiting_target' CHECK(state IN ('awaiting_target','deferred','answered')),
 profile jsonb,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE load_test_runs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 request_id uuid NOT NULL UNIQUE,
 env_id uuid NOT NULL REFERENCES environments(id) ON DELETE CASCADE,
 project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 release_id uuid NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
 publication_id uuid NOT NULL,
 version text NOT NULL,
 generation bigint NOT NULL CHECK(generation>0),
 profile jsonb NOT NULL,
 limits jsonb NOT NULL,
 target jsonb NOT NULL,
 state text NOT NULL DEFAULT 'queued' CHECK(state IN ('queued','running','passed','failed','inconclusive','config_needed','cancelled','interrupted')),
 reason jsonb,
 runner_id text NOT NULL DEFAULT '',
 assignment_id text NOT NULL DEFAULT '',
 runner_boot_id text NOT NULL DEFAULT '',
 stage text NOT NULL DEFAULT '',
 cancel_pending boolean NOT NULL DEFAULT false,
 evidence jsonb NOT NULL DEFAULT '[]',
 report jsonb,
 created_at timestamptz NOT NULL DEFAULT now(),
 started_at timestamptz,
 finished_at timestamptz,
 deadline_at timestamptz
);
CREATE UNIQUE INDEX load_test_active_env ON load_test_runs(env_id) WHERE state IN ('queued','running') OR cancel_pending;
CREATE UNIQUE INDEX load_test_active_runner ON load_test_runs(runner_id) WHERE state='running';
CREATE INDEX load_test_history ON load_test_runs(env_id,created_at DESC,id DESC);
ALTER TABLE runners ADD COLUMN load_test_id uuid REFERENCES load_test_runs(id) ON DELETE SET NULL;

-- Cascades must not erase the durable cancellation before the k6 process has
-- acknowledged stopping (or its immutable deadline has expired).
CREATE FUNCTION protect_load_test_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
 IF EXISTS(SELECT 1 FROM load_test_runs WHERE env_id=OLD.id AND (state IN('queued','running') OR cancel_pending)) THEN
  RAISE EXCEPTION 'сначала дождитесь остановки нагрузочного теста' USING ERRCODE='23514',CONSTRAINT='load_test_quiescence';
 END IF;
 RETURN OLD;
END $$;
CREATE TRIGGER protect_load_test_delete BEFORE DELETE ON environments FOR EACH ROW EXECUTE FUNCTION protect_load_test_delete();
