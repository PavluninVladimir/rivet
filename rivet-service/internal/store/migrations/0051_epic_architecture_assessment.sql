-- Каждый новый продуктовый Epic проходит Архитектора до планирования.
ALTER TABLE epics ADD COLUMN architecture_required boolean NOT NULL DEFAULT false;
ALTER TABLE epics ADD COLUMN architecture_revision bigint NOT NULL DEFAULT 1;

CREATE TABLE epic_architecture_assessments (
    epic_id uuid PRIMARY KEY REFERENCES epics(id) ON DELETE CASCADE,
    project_id uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    discovery_run_id uuid REFERENCES discovery_runs(id),
    revision bigint NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','waiting','running','passed','failed')),
    decompose_requested boolean NOT NULL DEFAULT false,
    planning_token uuid,
    planning_until timestamptz,
    planning_attempts int NOT NULL DEFAULT 0,
    planning_error text NOT NULL DEFAULT '',
    configuration jsonb NOT NULL DEFAULT '{}',
    attempts int NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_token uuid,
    lease_until timestamptz,
    agent_id text NOT NULL DEFAULT '',
    agent_name text NOT NULL DEFAULT '',
    connection_id text NOT NULL DEFAULT '',
    model text NOT NULL DEFAULT '',
    instruction_sha text NOT NULL DEFAULT '',
    context jsonb,
    result jsonb,
    error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz
);
CREATE INDEX epic_architecture_due ON epic_architecture_assessments(next_attempt_at) WHERE status <> 'passed';

-- Уже исполнявшаяся работа и импортированная история не переклассифицируются.
UPDATE epics e SET architecture_required=true
WHERE e.kind='work' AND e.source_key IS NULL AND e.discovery_run_id IS NOT NULL
  AND e.status='planned' AND e.started_at IS NULL
  AND NOT EXISTS(SELECT 1 FROM tasks t WHERE t.epic_id=e.id AND
      (t.status NOT IN ('queued','cancelled') OR t.started_at IS NOT NULL OR t.attempt_used>0))
  AND NOT EXISTS(SELECT 1 FROM task_step_runs r JOIN tasks t ON t.id=r.task_id WHERE t.epic_id=e.id);
INSERT INTO epic_architecture_assessments(epic_id,project_id,discovery_run_id,revision)
SELECT id,project_id,discovery_run_id,architecture_revision FROM epics WHERE architecture_required;

CREATE FUNCTION prepare_epic_architecture() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' THEN
        NEW.architecture_required := NEW.kind='work' AND NEW.source_key IS NULL;
    ELSIF NEW.architecture_required AND NEW.status='planned' AND NEW.started_at IS NULL
      AND (NEW.title,NEW.goal,NEW.discovery_run_id) IS DISTINCT FROM (OLD.title,OLD.goal,OLD.discovery_run_id) THEN
        NEW.architecture_revision := OLD.architecture_revision+1;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER prepare_epic_architecture BEFORE INSERT OR UPDATE OF title,goal,discovery_run_id ON epics
FOR EACH ROW EXECUTE FUNCTION prepare_epic_architecture();

CREATE FUNCTION queue_epic_architecture() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE changed int; from_phase text;
BEGIN
    IF NEW.architecture_required THEN
        INSERT INTO epic_architecture_assessments(epic_id,project_id,discovery_run_id,revision)
        VALUES(NEW.id,NEW.project_id,NEW.discovery_run_id,NEW.architecture_revision)
        ON CONFLICT(epic_id) DO UPDATE SET revision=EXCLUDED.revision,discovery_run_id=EXCLUDED.discovery_run_id,
            status='pending',attempts=0,next_attempt_at=now(),
            lease_token=CASE WHEN epic_architecture_assessments.lease_until>now() THEN epic_architecture_assessments.lease_token END,
            lease_until=CASE WHEN epic_architecture_assessments.lease_until>now() THEN epic_architecture_assessments.lease_until END,
            planning_token=NULL,planning_until=NULL,
            planning_attempts=0,planning_error='',
            result=NULL,error='',updated_at=now(),finished_at=NULL
        WHERE epic_architecture_assessments.revision<>EXCLUDED.revision;
        GET DIAGNOSTICS changed = ROW_COUNT;
        IF changed>0 THEN
            from_phase := 'architecture';
            IF TG_OP='INSERT' THEN
                from_phase := CASE WHEN NEW.discovery_run_id IS NOT NULL THEN 'discovery' ELSE NULL END;
            ELSIF NEW.discovery_run_id IS DISTINCT FROM OLD.discovery_run_id THEN
                from_phase := 'discovery';
            END IF;
            INSERT INTO events(actor_kind,actor_id,type,project_id,epic_id,text,payload)
            VALUES('system','architecture','architecture.assessment_pending',NEW.project_id,NEW.id,
                'Epic передан Архитектору до планирования',
                jsonb_build_object('epic_id',NEW.id,'discovery_run_id',NEW.discovery_run_id,'revision',NEW.architecture_revision,
                    'phase_from',from_phase,'phase_to','architecture'));
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER queue_epic_architecture AFTER INSERT OR UPDATE OF title,goal,discovery_run_id,architecture_revision ON epics
FOR EACH ROW EXECUTE FUNCTION queue_epic_architecture();

CREATE FUNCTION invalidate_epic_need_assessment() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE epics SET architecture_revision=architecture_revision+1
    WHERE id=COALESCE(NEW.epic_id,OLD.epic_id) AND architecture_required AND status='planned' AND started_at IS NULL;
    RETURN NULL;
END $$;
CREATE TRIGGER invalidate_epic_need_assessment AFTER INSERT OR UPDATE OR DELETE ON epic_needs
FOR EACH ROW EXECUTE FUNCTION invalidate_epic_need_assessment();

CREATE FUNCTION epic_architecture_ready(epic uuid) RETURNS boolean LANGUAGE plpgsql VOLATILE AS $$
DECLARE required boolean; started timestamptz; rev bigint;
BEGIN
    SELECT architecture_required,started_at INTO required,started FROM epics WHERE id=epic;
    -- Existing execution is grandfathered. Revision invalidation applies only
    -- before first start; active tasks need no inverse task→Epic row lock.
    IF NOT required OR started IS NOT NULL THEN RETURN true; END IF;
    SELECT architecture_required,started_at,architecture_revision INTO required,started,rev
      FROM epics WHERE id=epic FOR NO KEY UPDATE;
    RETURN NOT required OR started IS NOT NULL OR EXISTS(SELECT 1 FROM epic_architecture_assessments a
       WHERE a.epic_id=epic AND a.revision=rev AND a.status='passed');
END
$$;

CREATE FUNCTION guard_epic_architecture_start() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status='running' AND OLD.status<>'running' AND NOT epic_architecture_ready(NEW.id) THEN
        RAISE EXCEPTION 'архитектурная оценка Epic ещё не пройдена'
            USING ERRCODE='23514',CONSTRAINT='epic_architecture_required';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER guard_epic_architecture_start BEFORE UPDATE OF status ON epics
FOR EACH ROW EXECUTE FUNCTION guard_epic_architecture_start();

CREATE FUNCTION guard_task_architecture() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP='INSERT' OR NEW.status IN ('ready','running','testing','review','fixing','done')
       OR NEW.runner_id IS NOT NULL OR NEW.started_at IS NOT NULL THEN
        IF NOT epic_architecture_ready(NEW.epic_id) THEN
            RAISE EXCEPTION 'архитектурная оценка Epic ещё не пройдена'
                USING ERRCODE='23514',CONSTRAINT='epic_architecture_required';
        END IF;
        IF TG_OP='UPDATE' AND NEW.status IN ('ready','running','testing','review','fixing','done')
           AND EXISTS(SELECT 1 FROM epics WHERE id=NEW.epic_id AND architecture_required AND started_at IS NULL) THEN
            RAISE EXCEPTION 'сначала запустите Epic после архитектурной оценки'
                USING ERRCODE='23514',CONSTRAINT='epic_architecture_required';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER guard_task_architecture BEFORE INSERT OR UPDATE OF status,runner_id,started_at,attempt_used ON tasks
FOR EACH ROW EXECUTE FUNCTION guard_task_architecture();

CREATE FUNCTION guard_step_architecture() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE eid uuid;
BEGIN
    SELECT epic_id INTO eid FROM tasks WHERE id=NEW.task_id;
    IF NOT epic_architecture_ready(eid) THEN
        RAISE EXCEPTION 'архитектурная оценка Epic ещё не пройдена'
            USING ERRCODE='23514',CONSTRAINT='epic_architecture_required';
    END IF;
    IF EXISTS(SELECT 1 FROM epics WHERE id=eid AND architecture_required AND started_at IS NULL) THEN
        RAISE EXCEPTION 'сначала запустите Epic после архитектурной оценки'
            USING ERRCODE='23514',CONSTRAINT='epic_architecture_required';
    END IF;
    IF NEW.agent_id IS NOT NULL THEN
        PERFORM 1 FROM team_agents WHERE id=NEW.agent_id FOR UPDATE;
        IF EXISTS(SELECT 1 FROM epic_architecture_assessments WHERE agent_id=NEW.agent_id AND lease_until>now()) THEN
            RAISE EXCEPTION 'Архитектор занят оценкой Epic' USING ERRCODE='23514',CONSTRAINT='architecture_agent_busy';
        END IF;
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER guard_step_architecture BEFORE INSERT OR UPDATE OF agent_id,runner_id ON task_step_runs
FOR EACH ROW EXECUTE FUNCTION guard_step_architecture();
