package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/PavluninVladimir/rivet/internal/architecture"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrArchitectureRequired = errors.New("сначала требуется успешная архитектурная оценка актуального Epic")
var ErrAssessmentStale = errors.New("архитектурная оценка устарела или уже выполняется")
var ErrAssessmentAgentBusy = errors.New("архитектор занят")

func IsArchitectureRequired(err error) bool {
	var pg *pgconn.PgError
	return errors.Is(err, ErrArchitectureRequired) || (errors.As(err, &pg) && pg.ConstraintName == "epic_architecture_required")
}

type EpicAssessment struct {
	ExistingPlan       bool                     `json:"existing_plan"`
	PlanningError      string                   `json:"planning_error"`
	PlanningAttempts   int                      `json:"planning_attempts"`
	EpicID             string                   `json:"epic_id"`
	ProjectID          string                   `json:"project_id"`
	RunID              *string                  `json:"discovery_run_id,omitempty"`
	Revision           int64                    `json:"revision"`
	Status             string                   `json:"status"`
	DecomposeRequested bool                     `json:"decompose_requested"`
	Attempts           int                      `json:"attempts"`
	NextAttemptAt      time.Time                `json:"next_attempt_at"`
	AgentID            string                   `json:"agent_id"`
	AgentName          string                   `json:"agent_name"`
	ConnectionID       string                   `json:"connection_id"`
	Model              string                   `json:"model"`
	Error              string                   `json:"error"`
	Result             *architecture.Assessment `json:"result,omitempty"`
	CreatedAt          time.Time                `json:"created_at"`
	UpdatedAt          time.Time                `json:"updated_at"`
	FinishedAt         *time.Time               `json:"finished_at,omitempty"`
}
type AssessmentView struct {
	Required   bool            `json:"required"`
	Revision   int64           `json:"revision"`
	Assessment *EpicAssessment `json:"assessment"`
	Title      string          `json:"-"`
	Goal       string          `json:"-"`
}

func (s *Store) EpicAssessment(ctx context.Context, id string) (AssessmentView, error) {
	var out AssessmentView
	var raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT e.architecture_required,e.architecture_revision,e.title,e.goal,
		(to_jsonb(a)-ARRAY['lease_token','lease_until','planning_token','planning_until','context','instruction_sha','configuration'])
		 || jsonb_build_object('existing_plan',COALESCE(jsonb_array_length(a.context->'existing_plan'),0)>0)
		FROM epics e LEFT JOIN epic_architecture_assessments a ON a.epic_id=e.id WHERE e.id=$1`, id).
		Scan(&out.Required, &out.Revision, &out.Title, &out.Goal, &raw)
	if err != nil {
		return out, nf(err)
	}
	if len(raw) > 0 && string(raw) != "null" {
		var a EpicAssessment
		if err = json.Unmarshal(raw, &a); err != nil {
			return out, err
		}
		out.Assessment = &a
	}
	return out, nil
}

func requireArchitectureTx(ctx context.Context, tx pgx.Tx, id string, revision int64) error {
	var ready bool
	err := tx.QueryRow(ctx, `SELECT epic_architecture_ready(id) AND ($2::bigint=0 OR architecture_revision=$2) FROM epics WHERE id=$1`, id, revision).Scan(&ready)
	if err != nil {
		return nf(err)
	}
	if !ready {
		return ErrArchitectureRequired
	}
	return nil
}

func (s *Store) RequestAssessmentPlanning(ctx context.Context, id string) (AssessmentView, error) {
	_, err := s.Pool.Exec(ctx, `UPDATE epic_architecture_assessments a SET planning_attempts=CASE WHEN decompose_requested THEN planning_attempts ELSE 0 END,
		decompose_requested=true,updated_at=now()
		FROM epics e WHERE e.id=a.epic_id AND a.epic_id=$1 AND e.status='planned' AND e.started_at IS NULL`, id)
	if err != nil {
		return AssessmentView{}, err
	}
	return s.EpicAssessment(ctx, id)
}

func (s *Store) RetryEpicAssessment(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE epic_architecture_assessments a SET status='pending',attempts=0,next_attempt_at=now(),
		lease_token=NULL,lease_until=NULL,error='',updated_at=now() FROM epics e WHERE e.id=a.epic_id AND e.id=$1
		AND e.status='planned' AND e.started_at IS NULL AND a.status='failed'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

type DueAssessment struct{ EpicID, ProjectID string }

func (s *Store) DueEpicAssessments(ctx context.Context) ([]DueAssessment, error) {
	// A third interrupted attempt must become a visible failure, not a
	// permanently running row that can no longer be claimed.
	if _, err := s.Pool.Exec(ctx, `UPDATE epic_architecture_assessments SET status=CASE WHEN attempts>=3 THEN 'failed' ELSE 'pending' END,
		lease_token=NULL,lease_until=NULL,next_attempt_at=now(),error='оценка прервана; требуется повтор',updated_at=now()
		WHERE status='running' AND lease_until<=now()`); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT a.epic_id,a.project_id FROM epic_architecture_assessments a JOIN epics e ON e.id=a.epic_id
		WHERE e.status='planned' AND e.started_at IS NULL AND a.revision=e.architecture_revision
		AND a.status IN ('pending','waiting','running') AND a.next_attempt_at<=now()
		AND (a.lease_until IS NULL OR a.lease_until<=now()) ORDER BY a.next_attempt_at,a.created_at LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueAssessment
	for rows.Next() {
		var a DueAssessment
		if err = rows.Scan(&a.EpicID, &a.ProjectID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Waiting for configuration/budget/another role session does not spend an attempt.
func (s *Store) WaitEpicAssessment(ctx context.Context, id, reason string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var old, project string
		var rev int64
		err := tx.QueryRow(ctx, `SELECT error,project_id,revision FROM epic_architecture_assessments WHERE epic_id=$1
			AND status IN ('pending','waiting','running') AND (lease_until IS NULL OR lease_until<=now()) FOR UPDATE`, id).Scan(&old, &project, &rev)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE epic_architecture_assessments SET status='waiting',error=$2,next_attempt_at=now()+interval '1 minute',updated_at=now(),lease_token=NULL,lease_until=NULL WHERE epic_id=$1`, id, reason)
		if err != nil || old == reason {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorSystem, Type: "architecture.assessment_waiting", ProjectID: project, EpicID: id, Text: reason,
			Payload: map[string]any{"epic_id": id, "revision": rev, "phase_from": "architecture", "phase_to": "architecture"}})
		return err
	})
}

type AssessmentLease struct {
	RunID                             string
	EpicID, ProjectID, Token, AgentID string
	Revision                          int64
}

func (s *Store) ClaimEpicAssessment(ctx context.Context, id, agentID, agentName, conn, model, instructionSHA string, input json.RawMessage, expectedRevision int64) (AssessmentLease, error) {
	l := AssessmentLease{EpicID: id, AgentID: agentID}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Serializes with runner assignment's team-agent reservation.
		var enabled bool
		if err := tx.QueryRow(ctx, `SELECT a.enabled AND r.enabled FROM team_agents a JOIN agent_roles r ON r.id=a.role_id
			WHERE a.id=$1 AND a.role_id='architect' FOR UPDATE OF a`, agentID).Scan(&enabled); err != nil {
			return nf(err)
		}
		if !enabled {
			return ErrAssessmentStale
		}
		var busy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM task_step_runs WHERE agent_id=$1 AND verdict IS NULL)
			OR EXISTS(SELECT 1 FROM epic_architecture_assessments WHERE agent_id=$1 AND lease_until>now())`, agentID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return ErrAssessmentAgentBusy
		}
		config, err := assessmentConfiguration(ctx, tx, agentID, conn, model)
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `UPDATE epic_architecture_assessments a SET status='running',attempts=attempts+1,
			lease_token=gen_random_uuid(),lease_until=now()+interval '4 minutes',agent_id=$2,agent_name=$3,connection_id=$4,model=$5,
			instruction_sha=$6,context=$7,configuration=$9,error='',updated_at=now()
			FROM epics e WHERE a.epic_id=$1 AND e.id=a.epic_id AND e.status='planned' AND e.started_at IS NULL
			AND a.revision=e.architecture_revision AND a.revision=$8 AND a.status IN ('pending','waiting','running') AND attempts<3
			AND a.next_attempt_at<=now() AND (a.lease_until IS NULL OR a.lease_until<=now())
			RETURNING a.project_id,a.revision,a.lease_token,COALESCE(a.discovery_run_id::text,'')`, id, agentID, agentName, conn, model, instructionSHA, input, expectedRevision, config).Scan(&l.ProjectID, &l.Revision, &l.Token, &l.RunID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAssessmentStale
		}
		if err != nil {
			return err
		}
		if _, err = appendEvent(ctx, tx, AgentWorkEvent(agentID, agentName, true)); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorSystem, ActorID: agentID, Type: "architecture.assessment_started", ProjectID: l.ProjectID, EpicID: id,
			Text: agentName + " оценивает архитектурное влияние Epic", Payload: map[string]any{"epic_id": id, "discovery_run_id": l.RunID, "revision": l.Revision, "agent_id": agentID, "phase_from": "architecture", "phase_to": "architecture"}})
		return err
	})
	return l, err
}

func (s *Store) FinishEpicAssessment(ctx context.Context, l AssessmentLease, result *architecture.Assessment, reason string) (bool, error) {
	if result != nil {
		if err := result.Validate(); err != nil {
			return false, err
		}
	}
	var saved bool
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Current Epic is locked before checking generation: a concurrent edit
		// cannot turn an obsolete response into permission to plan.
		var rev int64
		if err := tx.QueryRow(ctx, `SELECT architecture_revision FROM epics WHERE id=$1 FOR NO KEY UPDATE`, l.EpicID).Scan(&rev); err != nil {
			return nf(err)
		}
		if rev != l.Revision {
			return nil
		}
		var conn, model string
		if err := tx.QueryRow(ctx, `SELECT connection_id,model FROM epic_architecture_assessments WHERE epic_id=$1`, l.EpicID).Scan(&conn, &model); err != nil {
			return err
		}
		config, configErr := assessmentConfiguration(ctx, tx, l.AgentID, conn, model)
		var valid bool
		if configErr == nil {
			if err := tx.QueryRow(ctx, `SELECT configuration=$2::jsonb FROM epic_architecture_assessments WHERE epic_id=$1`, l.EpicID, config).Scan(&valid); err != nil {
				return err
			}
		} else if !errors.Is(configErr, ErrAssessmentStale) && !errors.Is(configErr, ErrNotFound) {
			return configErr
		}
		if !valid {
			result = nil
			reason = "Исполнитель или подключение Архитектора изменены; требуется новая оценка"
		}
		status, typ := "waiting", "architecture.assessment_waiting"
		var raw any
		if result != nil {
			status, typ = "passed", "architecture.assessment_passed"
			b, _ := json.Marshal(result)
			raw = b
		}
		var attempts int
		err := tx.QueryRow(ctx, `UPDATE epic_architecture_assessments SET status=$4,result=$5,error=$6,lease_token=NULL,lease_until=NULL,
			updated_at=now(),finished_at=CASE WHEN $4='passed' THEN now() ELSE NULL END,next_attempt_at=CASE WHEN $4='passed' THEN now() ELSE now()+interval '1 minute' END
			WHERE epic_id=$1 AND revision=$2 AND lease_token=$3 AND lease_until>now() AND status='running' RETURNING attempts`, l.EpicID, l.Revision, l.Token, status, raw, reason).Scan(&attempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if result == nil && attempts >= 3 {
			typ = "architecture.assessment_failed"
			if _, err = tx.Exec(ctx, `UPDATE epic_architecture_assessments SET status='failed' WHERE epic_id=$1`, l.EpicID); err != nil {
				return err
			}
		}
		payload := map[string]any{"epic_id": l.EpicID, "discovery_run_id": l.RunID, "revision": l.Revision, "agent_id": l.AgentID, "phase_from": "architecture", "phase_to": "architecture"}
		text := reason
		if result != nil {
			payload["phase_to"] = "planning"
			payload["result"] = result
			text = "Архитектор завершил оценку; Epic передан в планирование"
		}
		if _, err = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorSystem, ActorID: l.AgentID, Type: typ, ProjectID: l.ProjectID, EpicID: l.EpicID, Text: text, Payload: payload}); err != nil {
			return err
		}
		saved = true
		if _, err = appendEvent(ctx, tx, AgentWorkEvent(l.AgentID, l.AgentID, false)); err != nil {
			return err
		}
		return nil
	})
	return saved, err
}

func (s *Store) AssessmentLeaseValid(ctx context.Context, l AssessmentLease) (bool, error) {
	var valid bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM epic_architecture_assessments a JOIN epics e ON e.id=a.epic_id
		JOIN team_agents ag ON ag.id=a.agent_id JOIN agent_roles r ON r.id=ag.role_id JOIN model_connections c ON c.id=a.connection_id
		WHERE a.epic_id=$1 AND a.revision=$2 AND e.architecture_revision=$2 AND a.lease_token=$3 AND a.lease_until>now()
		AND a.status='running' AND ag.enabled AND r.enabled AND c.enabled AND c.state<>'invalid')`, l.EpicID, l.Revision, l.Token).Scan(&valid)
	return valid, err
}

func (s *Store) ReleaseAssessmentLease(ctx context.Context, l AssessmentLease) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE epic_architecture_assessments SET lease_token=NULL,lease_until=NULL,next_attempt_at=now(),
		status=CASE WHEN status='running' THEN 'pending' ELSE status END WHERE epic_id=$1 AND lease_token=$2`, l.EpicID, l.Token)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = appendEvent(ctx, tx, AgentWorkEvent(l.AgentID, l.AgentID, false))
		return err
	})
}

// Lock configuration through the result commit. A concurrent disable/change
// linearizes either before this check (old result rejected) or after commit.
func assessmentConfiguration(ctx context.Context, tx pgx.Tx, agentID, conn, model string) ([]byte, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT a.id FROM team_agents a JOIN agent_roles r ON r.id=a.role_id JOIN model_connections c ON c.id=$2
		WHERE a.id=$1 AND a.role_id='architect' FOR SHARE OF a,r,c`, agentID, conn).Scan(&id)
	if err != nil {
		return nil, nf(err)
	}
	var raw []byte
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT a.enabled AND r.enabled AND c.enabled AND c.state<>'invalid',
		jsonb_build_object('agent',a.agent,'agent_name',a.name,'connection',a.connection_id,'model',a.model,'extra',a.instruction_extra,
		'role_title',r.title,'instruction',r.instruction,'default_model',r.default_model,'selected_connection',$2::text,'selected_model',$3::text,
		'connection_updated_at',c.updated_at)
		FROM team_agents a JOIN agent_roles r ON r.id=a.role_id JOIN model_connections c ON c.id=$2 WHERE a.id=$1`, agentID, conn, model).Scan(&enabled, &raw)
	if err != nil {
		return nil, err
	}
	if !enabled {
		return nil, ErrAssessmentStale
	}
	return raw, nil
}

// Planner leases are independent: Ada is released before Timur starts work.
func (s *Store) DueAssessmentPlans(ctx context.Context) ([]string, error) {
	// A committed plan is the durable completion record even if the process
	// died before Finish. An exhausted interrupted lease must not wait forever.
	if _, err := s.Pool.Exec(ctx, `UPDATE epic_architecture_assessments a SET decompose_requested=false,planning_token=NULL,planning_until=NULL,
		planning_error=CASE WHEN EXISTS(SELECT 1 FROM tasks WHERE epic_id=a.epic_id) THEN '' ELSE 'планирование прервано; лимит повторов исчерпан, запросите декомпозицию повторно' END
		WHERE a.decompose_requested AND (EXISTS(SELECT 1 FROM tasks WHERE epic_id=a.epic_id)
		OR (a.planning_attempts>=3 AND (a.planning_until IS NULL OR a.planning_until<=now())))`); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT a.epic_id FROM epic_architecture_assessments a JOIN epics e ON e.id=a.epic_id
		WHERE a.status='passed' AND a.revision=e.architecture_revision AND a.decompose_requested AND e.status='planned'
		AND (planning_until IS NULL OR planning_until<=now()) AND a.next_attempt_at<=now()
		AND NOT EXISTS(SELECT 1 FROM tasks WHERE epic_id=e.id) ORDER BY a.created_at LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s *Store) ClaimAssessmentPlan(ctx context.Context, id string) (string, error) {
	var token string
	err := s.Pool.QueryRow(ctx, `UPDATE epic_architecture_assessments a SET planning_token=gen_random_uuid(),planning_until=now()+interval '2 minutes',planning_attempts=planning_attempts+1,planning_error=''
		FROM epics e WHERE e.id=a.epic_id AND a.epic_id=$1 AND a.status='passed' AND a.revision=e.architecture_revision
		AND a.decompose_requested AND planning_attempts<3 AND e.status='planned' AND (planning_until IS NULL OR planning_until<=now())
		AND NOT EXISTS(SELECT 1 FROM tasks WHERE epic_id=e.id) RETURNING planning_token`, id).Scan(&token)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrAssessmentStale
	}
	return token, err
}
func (s *Store) RenewAssessmentPlan(ctx context.Context, id, token string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE epic_architecture_assessments a SET planning_until=now()+interval '2 minutes'
		FROM epics e WHERE e.id=a.epic_id AND a.epic_id=$1 AND a.planning_token=$2 AND a.status='passed' AND a.revision=e.architecture_revision`, id, token)
	return tag.RowsAffected() == 1, err
}
func (s *Store) FinishAssessmentPlan(ctx context.Context, id, token string, success bool) error {
	_, err := s.Pool.Exec(ctx, `UPDATE epic_architecture_assessments SET planning_token=NULL,planning_until=NULL,
		decompose_requested=NOT $3 AND planning_attempts<3,next_attempt_at=now()+interval '1 minute' WHERE epic_id=$1 AND planning_token=$2`, id, token, success)
	return err
}

func (s *Store) PostponeAssessmentPlan(ctx context.Context, id, reason string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE epic_architecture_assessments SET planning_error=$2,next_attempt_at=now()+interval '1 minute' WHERE epic_id=$1 AND status='passed'`, id, reason)
	return err
}
