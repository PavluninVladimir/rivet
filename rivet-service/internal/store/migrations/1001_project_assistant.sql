-- Assistant data is additive; domain state remains in existing tables.
CREATE TABLE assistant_configs (
 project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 revision bigint NOT NULL DEFAULT 1,
 enabled boolean NOT NULL DEFAULT false,
 sponsor_user_id uuid REFERENCES users(id),
 connection_id text,
 model_id text,
 limits jsonb NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE assistant_runs (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 revision bigint NOT NULL DEFAULT 1,
 status text NOT NULL DEFAULT 'queued' CHECK(status IN ('queued','running','completed','waiting_human','failed','stopped','budget_exhausted')),
 trigger text NOT NULL CHECK(trigger IN ('question','recover','event')),
 initiator_id uuid NOT NULL REFERENCES users(id),
 snapshot_revision text NOT NULL,
 snapshot jsonb NOT NULL,
 config_revision bigint NOT NULL,
 intent_key text NOT NULL,
 body_hash text NOT NULL,
 question text NOT NULL,
 stop_key text,
 stop_body_hash text,
 created_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 summary text,
 uncertainties jsonb NOT NULL DEFAULT '[]',
 followups jsonb NOT NULL DEFAULT '[]',
 usage jsonb NOT NULL DEFAULT '{"input_tokens":0,"output_tokens":0,"cost_usd":null}',
 reserved_tokens bigint NOT NULL DEFAULT 0,
 reserved_cost_usd numeric,
 lease_epoch bigint,
 UNIQUE(project_id,intent_key)
);
CREATE INDEX assistant_run_queue ON assistant_runs(created_at) WHERE status IN ('queued','running');
CREATE TABLE assistant_messages (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 author text NOT NULL CHECK(author IN('user','assistant')),
 actor_id uuid REFERENCES users(id),
 text text NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(),
 run_id uuid REFERENCES assistant_runs(id),
 ref_ids jsonb NOT NULL DEFAULT '[]'
);
CREATE INDEX assistant_message_history ON assistant_messages(project_id,created_at DESC,id DESC);
CREATE TABLE assistant_actions (
 id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
 run_id uuid NOT NULL REFERENCES assistant_runs(id) ON DELETE CASCADE,
 project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 type text NOT NULL CHECK(type IN('task.retry','epic.resume','environment.resume')),
 target_id uuid NOT NULL,
 expected_revision text NOT NULL,
 status text NOT NULL DEFAULT 'proposed' CHECK(status IN('proposed','executing','succeeded','failed','denied','requires_human','unknown')),
 guard jsonb NOT NULL DEFAULT '{"decision":"unavailable","code":"not_evaluated","message":"проверка не выполнена"}',
 failure_episode_id text,
 evidence_ids jsonb NOT NULL DEFAULT '[]',
 operation_id uuid NOT NULL DEFAULT gen_random_uuid(),
 result jsonb,
 created_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 UNIQUE(run_id,type,target_id),
 UNIQUE(operation_id)
);
CREATE TABLE assistant_leases (
 project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 epoch bigint NOT NULL DEFAULT 0,
 run_id uuid REFERENCES assistant_runs(id),
 expires_at timestamptz NOT NULL DEFAULT '-infinity'
);
CREATE TABLE assistant_retry_episodes (
 project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 task_id uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
 episode_id text NOT NULL,
 action_id uuid NOT NULL UNIQUE REFERENCES assistant_actions(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(project_id,task_id,episode_id)
);
CREATE TABLE assistant_event_causes (
 project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 cause_key text NOT NULL,
 resource_revision text NOT NULL,
 last_run_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY(project_id,cause_key,resource_revision)
);
-- Start watching at installation, not by replaying historical failures.
CREATE TABLE assistant_event_checkpoint(id boolean PRIMARY KEY CHECK(id),cursor bigint NOT NULL);
INSERT INTO assistant_event_checkpoint VALUES(true,(SELECT COALESCE(max(id),0) FROM events));

CREATE TABLE assistant_event_pending (
 project_id uuid PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
 sponsor_user_id uuid NOT NULL REFERENCES users(id),
 revision bigint NOT NULL DEFAULT 1,
 intent_key text NOT NULL,
 cause text NOT NULL,
 not_before timestamptz NOT NULL DEFAULT now()+interval '30 seconds'
);
