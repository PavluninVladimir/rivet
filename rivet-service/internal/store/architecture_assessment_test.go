package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PavluninVladimir/rivet/internal/architecture"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
)

func assessmentFixture(t *testing.T) (*Store, domain.Project) {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	// Other suites seed legacy work. This suite always enables natural new-Epic defaults.
	if _, err := s.Pool.Exec(ctx, `ALTER TABLE epics ENABLE TRIGGER prepare_epic_architecture`); err != nil {
		t.Fatal(err)
	}
	u := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "gate", "o/gate", nil, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `INSERT INTO model_connections(id,name,kind,api,base_url,updated_by) VALUES('assessment-test','Assessment','local','openai','http://127.0.0.1:1',$1)`, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE team_agents SET connection_id='assessment-test',model='m' WHERE id='ada'`); err != nil {
		t.Fatal(err)
	}
	return s, p
}
func assessmentClaim(t *testing.T, s *Store, id string) AssessmentLease {
	t.Helper()
	v, err := s.EpicAssessment(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.ClaimEpicAssessment(context.Background(), id, "ada", "Ада", "assessment-test", "m", "test", []byte(`{}`), v.Revision)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func assessmentPass(t *testing.T, s *Store, id string) {
	t.Helper()
	l := assessmentClaim(t, s, id)
	a := architecture.Assessment{Decision: "no_change", Summary: "Existing boundary is sufficient", Impact: []string{}, Constraints: []string{"Keep tenant isolation"}, Documentation: []string{}, Risks: []string{}}
	if ok, err := s.FinishEpicAssessment(context.Background(), l, &a, ""); err != nil || !ok {
		t.Fatalf("pass: %v %v", ok, err)
	}
}

func TestReassessmentCannotInvalidateConcurrentFirstStart(t *testing.T) {
	s, p := assessmentFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e, err := s.CreateEpic(ctx, p.ID, "Existing feature", "UI only")
	if err != nil {
		t.Fatal(err)
	}
	assessmentPass(t, s, e.ID)
	v, _ := s.EpicAssessment(ctx, e.ID)
	start, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = start.Rollback(context.Background()) }()
	if _, err = start.Exec(ctx, `UPDATE epics SET status='running' WHERE id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.ReevaluateEpicAssessment(ctx, e.ID, v.Revision, "owner") }()
	// Observe the real database lock rather than relying on a timing sleep.
	for {
		var waiting bool
		if err = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
			AND wait_event_type='Lock' AND query LIKE 'SELECT project_id FROM epics WHERE id=%FOR NO KEY UPDATE%')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("reassessment did not serialize with Start: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = start.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrConflict) {
		t.Fatalf("reassessment invalidated running work: %v", err)
	}
	after, _ := s.EpicAssessment(ctx, e.ID)
	if after.Revision != v.Revision || after.Assessment.Status != "passed" {
		t.Fatal("started assessment was invalidated")
	}
}

func TestEpicAssessmentManualAndDatabaseGates(t *testing.T) {
	s, p := assessmentFixture(t)
	ctx := context.Background()
	e, err := s.CreateEpic(ctx, p.ID, "Manual product", "Add a counter")
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.EpicAssessment(ctx, e.ID)
	if err != nil || !v.Required || v.Assessment.Status != "pending" {
		t.Fatalf("manual defaults: %+v %v", v, err)
	}
	if _, err = s.CreateTask(ctx, e.ID, NewTask{Title: "premature"}); !IsArchitectureRequired(err) {
		t.Fatalf("task bypass: %v", err)
	}
	if _, err = s.CreatePlan(ctx, e.ID, []PlanTask{{NewTask: NewTask{Title: "premature"}}}); !IsArchitectureRequired(err) {
		t.Fatalf("plan bypass: %v", err)
	}
	if _, err = s.Pool.Exec(ctx, `INSERT INTO tasks(epic_id,title) VALUES($1,'SQL bypass')`, e.ID); !IsArchitectureRequired(err) {
		t.Fatalf("SQL task bypass: %v", err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE epics SET status='running' WHERE id=$1`, e.ID); !IsArchitectureRequired(err) {
		t.Fatalf("SQL start bypass: %v", err)
	}
	assessmentPass(t, s, e.ID)
	task, err := s.CreateTask(ctx, e.ID, NewTask{Title: "allowed plan"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE tasks SET status='running' WHERE id=$1`, task.ID); !IsArchitectureRequired(err) {
		t.Fatalf("missing explicit first start: %v", err)
	}
	if err = s.TransitionEpic(ctx, e.ID, domain.EpicRunning, EventInput{ActorKind: domain.ActorSystem, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	if err = s.RecomputeEpic(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetTask(ctx, task.ID); err != nil || got.Status != domain.TaskReady {
		t.Fatalf("normal start: %+v %v", got, err)
	}
}

func TestEpicAssessmentStaleRevisionAndLease(t *testing.T) {
	s, p := assessmentFixture(t)
	ctx := context.Background()
	e, err := s.CreateEpic(ctx, p.ID, "Research product", "Add counter")
	if err != nil {
		t.Fatal(err)
	}
	l := assessmentClaim(t, s, e.ID)
	if _, err = s.Pool.Exec(ctx, `UPDATE epics SET goal='Different scope' WHERE id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	a := architecture.Assessment{Decision: "no_change", Summary: "old answer", Impact: []string{}, Constraints: []string{}, Documentation: []string{}, Risks: []string{}}
	if ok, err := s.FinishEpicAssessment(ctx, l, &a, ""); err != nil || ok {
		t.Fatalf("stale response passed: %v %v", ok, err)
	}
	if err = s.ReleaseAssessmentLease(ctx, l); err != nil {
		t.Fatal(err)
	}
	l2 := assessmentClaim(t, s, e.ID)
	if l2.Revision == l.Revision || l2.Token == l.Token {
		t.Fatal("generation not renewed")
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE epic_architecture_assessments SET lease_until=now()-interval '1 second' WHERE epic_id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.FinishEpicAssessment(ctx, l2, &a, ""); err != nil || ok {
		t.Fatalf("expired response passed: %v %v", ok, err)
	}
	if _, err = s.DueEpicAssessments(ctx); err != nil {
		t.Fatal(err)
	}
	assessmentPass(t, s, e.ID)
	if _, err = s.CreatePlanAtArchitectureRevision(ctx, e.ID, []PlanTask{{NewTask: NewTask{Title: "obsolete planner answer"}}}, l.Revision); !IsArchitectureRequired(err) {
		t.Fatalf("old planner revision accepted: %v", err)
	}
	v, _ := s.EpicAssessment(ctx, e.ID)
	if v.Assessment.Attempts != 2 {
		t.Fatalf("restart attempts: %+v", v)
	}
}

// Wait for the actual PostgreSQL row lock, not a sleep-based assumption.
func waitAssessmentLock(t *testing.T, s *Store, pid uint32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var locked bool
		if err := s.Pool.QueryRow(context.Background(), `SELECT COALESCE(wait_event_type='Lock',false) FROM pg_stat_activity WHERE pid=$1`, pid).Scan(&locked); err != nil {
			t.Fatal(err)
		}
		if locked {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("transaction did not wait for expected row lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEpicAssessmentConcurrentDisableCannotPass(t *testing.T) {
	s, p := assessmentFixture(t)
	ctx := context.Background()
	for _, disable := range []string{`UPDATE model_connections SET enabled=false WHERE id='assessment-test'`, `UPDATE agent_roles SET enabled=false WHERE id='architect'`, `UPDATE team_agents SET enabled=false WHERE id='ada'`} {
		e, err := s.CreateEpic(ctx, p.ID, "Concurrent configuration", "counter")
		if err != nil {
			t.Fatal(err)
		}
		l := assessmentClaim(t, s, e.ID)
		tx, err := s.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, disable); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			ok, err := s.FinishEpicAssessment(ctx, l, &architecture.Assessment{Decision: "no_change", Summary: "racing answer", Impact: []string{}, Constraints: []string{}, Documentation: []string{}, Risks: []string{}}, "")
			if err == nil && ok {
				v, e := s.EpicAssessment(ctx, l.EpicID)
				if e != nil {
					err = e
				} else if v.Assessment.Status == "passed" {
					err = errors.New("disabled configuration passed")
				}
			}
			done <- err
		}()
		// The disabled row is uncommitted; result persistence must wait for it.
		select {
		case err := <-done:
			_ = tx.Rollback(ctx)
			t.Fatalf("finish bypassed configuration row lock: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		if _, err = s.Pool.Exec(ctx, `UPDATE model_connections SET enabled=true WHERE id='assessment-test'; UPDATE agent_roles SET enabled=true WHERE id='architect'; UPDATE team_agents SET enabled=true WHERE id='ada'`); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEpicAssessmentConcurrentEditSerializesStart(t *testing.T) {
	s, p := assessmentFixture(t)
	ctx := context.Background()
	e, err := s.CreateEpic(ctx, p.ID, "Race", "counter")
	if err != nil {
		t.Fatal(err)
	}
	assessmentPass(t, s, e.ID)
	if _, err = s.CreateTask(ctx, e.ID, NewTask{Title: "queued plan"}); err != nil {
		t.Fatal(err)
	}
	edit, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = edit.Rollback(ctx) }()
	if _, err = edit.Exec(ctx, `UPDATE epics SET goal='new requirement' WHERE id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	done := make(chan error, 1)
	go func() { _, err := conn.Exec(ctx, `UPDATE epics SET status='running' WHERE id=$1`, e.ID); done <- err }()
	waitAssessmentLock(t, s, conn.Conn().PgConn().PID())
	if err = edit.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !IsArchitectureRequired(err) {
		t.Fatalf("edit/start bypass: %v", err)
	}
}

func TestEpicAssessmentUpgradePreservesQueuedPlanAndActiveWork(t *testing.T) {
	s, p := assessmentFixture(t)
	ctx := context.Background()
	run, err := s.CreateRun(ctx, p.ID, NewRun{Interviews: []RunInterview{{ID: p.ID, Title: "legacy material"}}})
	if err != nil {
		t.Fatal(err)
	}
	legacy := func(title string, research bool) domain.Epic {
		e, err := s.CreateEpic(ctx, p.ID, title, "legacy goal")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.Pool.Exec(ctx, `WITH legacy AS (UPDATE epics SET architecture_required=false WHERE id=$1 RETURNING id) DELETE FROM epic_architecture_assessments WHERE epic_id IN (SELECT id FROM legacy)`, e.ID); err != nil {
			t.Fatal(err)
		}
		if research {
			if _, err = s.Pool.Exec(ctx, `UPDATE epics SET discovery_run_id=$2 WHERE id=$1`, e.ID, run.ID); err != nil {
				t.Fatal(err)
			}
		}
		return e
	}
	e := legacy("five queued research tasks", true)
	ids := map[string]bool{}
	for range 5 {
		task, err := s.CreateTask(ctx, e.ID, NewTask{Title: "keep existing task"})
		if err != nil {
			t.Fatal(err)
		}
		ids[task.ID] = true
	}
	active := legacy("already active research", true)
	if _, err = s.CreateTask(ctx, active.ID, NewTask{Title: "existing work"}); err != nil {
		t.Fatal(err)
	}
	if err = s.TransitionEpic(ctx, active.ID, domain.EpicRunning, EventInput{ActorKind: domain.ActorSystem, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
	manual := legacy("unrelated legacy manual", false)
	raw, err := migrationsFS.ReadFile("migrations/0051_epic_architecture_assessment.sql")
	if err != nil {
		t.Fatal(err)
	}
	backfill := strings.Split(strings.Split(string(raw), "UPDATE epics e SET architecture_required=true")[1], "CREATE FUNCTION prepare_epic_architecture")[0]
	if _, err = s.Pool.Exec(ctx, "UPDATE epics e SET architecture_required=true"+backfill); err != nil {
		t.Fatal(err)
	}
	v, _ := s.EpicAssessment(ctx, e.ID)
	if !v.Required || v.Assessment == nil {
		t.Fatal("eligible legacy research not protected")
	}
	if err = s.TransitionEpic(ctx, e.ID, domain.EpicRunning, EventInput{ActorKind: domain.ActorSystem, Type: "epic.status"}); !IsArchitectureRequired(err) {
		t.Fatalf("queued plan bypass: %v", err)
	}
	for _, id := range []string{active.ID, manual.ID} {
		v, _ = s.EpicAssessment(ctx, id)
		if v.Required || v.Assessment != nil {
			t.Fatalf("legacy work reclassified: %+v", v)
		}
	}
	assessmentPass(t, s, e.ID)
	tasks, err := s.ListEpicTasks(ctx, e.ID)
	if err != nil || len(tasks) != 5 {
		t.Fatalf("tasks reset: %d %v", len(tasks), err)
	}
	for _, task := range tasks {
		if !ids[task.ID] || task.Status != domain.TaskQueued {
			t.Fatalf("task changed: %+v", task)
		}
	}
	if err = s.TransitionEpic(ctx, e.ID, domain.EpicRunning, EventInput{ActorKind: domain.ActorSystem, Type: "epic.status"}); err != nil {
		t.Fatal(err)
	}
}

func TestEpicAssessmentSharesAdaCapacity(t *testing.T) {
	s, p := assessmentFixture(t)
	ctx := context.Background()
	other, err := s.CreateEpic(ctx, p.ID, "legacy architecture session", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `WITH legacy AS (UPDATE epics SET architecture_required=false WHERE id=$1 RETURNING id) DELETE FROM epic_architecture_assessments WHERE epic_id IN (SELECT id FROM legacy)`, other.ID); err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, other.ID, NewTask{Title: "existing architect work"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.CreateEpic(ctx, p.ID, "assessment", "counter")
	if err != nil {
		t.Fatal(err)
	}
	l := assessmentClaim(t, s, e.ID)
	for i := range 2 {
		if i == 1 {
			if _, err = s.Pool.Exec(ctx, `UPDATE epics SET goal='updated' WHERE id=$1`, e.ID); err != nil {
				t.Fatal(err)
			}
		}
		_, err = s.Pool.Exec(ctx, `INSERT INTO task_step_runs(task_id,step_id,participant,agent_id) VALUES($1,'code','p1','ada')`, task.ID)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.ConstraintName != "architecture_agent_busy" {
			t.Fatalf("Ada double booked: %v", err)
		}
	}
	if err = s.ReleaseAssessmentLease(ctx, l); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `INSERT INTO task_step_runs(task_id,step_id,participant,agent_id) VALUES($1,'code','p1','ada')`, task.ID); err != nil {
		t.Fatal(err)
	}
}

func TestEpicAssessmentInterruptedThirdPlanAndCommittedPlanRecovery(t *testing.T) {
	s, p := assessmentFixture(t)
	ctx := context.Background()
	e, err := s.CreateEpic(ctx, p.ID, "third interrupted planner", "counter")
	if err != nil {
		t.Fatal(err)
	}
	assessmentPass(t, s, e.ID)
	if _, err = s.RequestAssessmentPlanning(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE epic_architecture_assessments SET planning_attempts=3,planning_token=gen_random_uuid(),planning_until=now()-interval '1 second' WHERE epic_id=$1`, e.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DueAssessmentPlans(ctx); err != nil {
		t.Fatal(err)
	}
	v, _ := s.EpicAssessment(ctx, e.ID)
	if v.Assessment.DecomposeRequested || v.Assessment.PlanningError == "" {
		t.Fatalf("exhausted planning stuck: %+v", v)
	}
	if _, err = s.RequestAssessmentPlanning(ctx, e.ID); err != nil {
		t.Fatal(err)
	}
	v, _ = s.EpicAssessment(ctx, e.ID)
	if v.Assessment.PlanningAttempts != 0 || !v.Assessment.DecomposeRequested {
		t.Fatalf("explicit retry did not reset: %+v", v)
	}
	token, err := s.ClaimAssessmentPlan(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreatePlan(ctx, e.ID, []PlanTask{{NewTask: NewTask{Title: "committed before crash"}}}); err != nil {
		t.Fatal(err)
	}
	// Crash before Finish: an actual plan consumes intent without a duplicate.
	if ids, err := s.DueAssessmentPlans(ctx); err != nil || len(ids) != 0 {
		t.Fatalf("committed plan scheduled again: %+v %v", ids, err)
	}
	v, _ = s.EpicAssessment(ctx, e.ID)
	if v.Assessment.DecomposeRequested || v.Assessment.PlanningError != "" {
		t.Fatalf("committed intent not consumed %+v", v)
	}
	if err = s.FinishAssessmentPlan(ctx, e.ID, token, false); err != nil {
		t.Fatal(err)
	}
	v, _ = s.EpicAssessment(ctx, e.ID)
	if v.Assessment.DecomposeRequested {
		t.Fatal("stale planner re-enabled intent")
	}
}
