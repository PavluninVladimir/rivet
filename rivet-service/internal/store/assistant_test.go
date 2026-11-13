package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/assistant"
	"github.com/PavluninVladimir/rivet/internal/domain"
)

func assistantFixture(t *testing.T) (*Store, domain.User, domain.Project, domain.Task, assistant.Config) {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	u := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "Assistant project", "a/b", nil, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.UpsertConnection(ctx, ConnectionInput{ID: "mtplx-local", Name: "Local", Kind: "local", API: "openai", BaseURL: "http://localhost:12345"}, nil, u.ID, u.Login); err != nil {
		t.Fatal(err)
	}
	_, err = s.SetConnectionModels(ctx, "mtplx-local", []domain.ModelEntry{{ID: "test-model", Label: "Test", Source: "manual"}}, u.ID, u.Login)
	if err != nil {
		t.Fatal(err)
	}
	cID, mID := "mtplx-local", "test-model"
	c, err := s.SaveAssistantConfig(ctx, p.ID, u.ID, 0, assistant.Config{Enabled: true, ConnectionID: &cID, ModelID: &mID, Limits: assistant.DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.CreateEpic(ctx, p.ID, "Epic", "goal")
	if err != nil {
		t.Fatal(err)
	}
	task := mustTask(t, s, e.ID, NewTask{Title: "subject password=secret42"})
	if _, err = s.Pool.Exec(ctx, `UPDATE tasks SET status='failed',step_id='code',block_reason='timeout',attempt_used=3 WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	return s, u, p, task, c
}
func assistantJob(t *testing.T, s *Store, u domain.User, p domain.Project, key string) AssistantJob {
	t.Helper()
	_, r, _, err := s.CreateAssistantRun(context.Background(), p.ID, u.ID, key, "восстанови", "recover", "")
	if err != nil {
		t.Fatal(err)
	}
	j, err := s.ClaimAssistantJob(context.Background())
	if err != nil || j == nil || j.Run.ID != r.ID {
		t.Fatalf("job %v %v", j, err)
	}
	return *j
}
func assistantAllow(context.Context, string, string, domain.User) (assistant.Guard, error) {
	return assistant.Guard{Decision: "allow", Code: "allowed", Message: "allow"}, nil
}
func TestAssistantSnapshotACLAndSanitization(t *testing.T) {
	s, u, p, task, _ := assistantFixture(t)
	ctx := context.Background()
	snap, err := s.AssistantSnapshot(ctx, p.ID, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Facts) == 0 || snap.Revision == "" {
		t.Fatal("empty snapshot")
	}
	for _, f := range snap.Facts {
		if f.ResourceID == task.ID && strings.Contains(fmt.Sprint(f.Value), "secret42") {
			t.Fatal("secret leaked")
		}
	}
	stranger := mustOwner(t, s)
	if _, err = s.AssistantSnapshot(ctx, p.ID, stranger.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign %v", err)
	}
	// Assistant events do not invalidate their own snapshot or trigger loops.
	_, err = s.AppendEvent(ctx, EventInput{ActorKind: domain.ActorSystem, Type: "assistant.run.updated", ProjectID: p.ID})
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.AssistantSnapshot(ctx, p.ID, u.ID)
	if err != nil || next.Revision != snap.Revision {
		t.Fatalf("self-invalidated %v", err)
	}
}
func TestAssistantRetryAtomicReplayAndStableEpisode(t *testing.T) {
	s, u, p, task, _ := assistantFixture(t)
	ctx := context.Background()
	j := assistantJob(t, s, u, p, "retry-1")
	a, err := s.ProposeAssistantAction(ctx, j, assistant.Intent{Type: "task.retry", TargetID: task.ID, EvidenceIDs: []string{"task:" + task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	actionCtx := WithAssistantMutation(ctx, j, a, assistantAllow)
	if err = s.ResolveTask(actionCtx, task.ID, "", u.Login, false); err != nil {
		t.Fatal(err)
	}
	if got := status(t, s, task.ID); got != domain.TaskQueued {
		t.Fatal(got)
	}
	// Replay after commit cannot reset status/counters or enqueue another effect.
	if err = s.ResolveTask(actionCtx, task.ID, "", u.Login, false); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE task_id=$1 AND type='task.status'`, task.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("effects %d %v", n, err)
	}
	if err = s.FinishAssistantJob(ctx, j, "completed", assistant.Diagnosis{Summary: "принято", Uncertainties: []string{}}, assistant.Usage{}); err != nil {
		t.Fatal(err)
	}
	// Same failure after new session / reset counters remains the same episode.
	if _, err = s.Pool.Exec(ctx, `UPDATE tasks SET status='failed',attempt_used=3,updated_at=now() WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	j2 := assistantJob(t, s, u, p, "retry-2")
	a2, err := s.ProposeAssistantAction(ctx, j2, assistant.Intent{Type: "task.retry", TargetID: task.ID, EvidenceIDs: []string{"task:" + task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	err = s.ResolveTask(WithAssistantMutation(ctx, j2, a2, assistantAllow), task.ID, "", u.Login, false)
	var ae *assistant.Error
	if !errors.As(err, &ae) || ae.Code != "failure_episode_repeated" {
		t.Fatalf("loop guard %v", err)
	}
	if got := status(t, s, task.ID); got != domain.TaskFailed {
		t.Fatal(got)
	}
}
func TestAssistantGuardRevocationStalePolicyAndStop(t *testing.T) {
	for _, mode := range []string{"revoke", "stale", "policy", "stop"} {
		t.Run(mode, func(t *testing.T) {
			s, u, p, task, _ := assistantFixture(t)
			ctx := context.Background()
			j := assistantJob(t, s, u, p, "guard")
			a, err := s.ProposeAssistantAction(ctx, j, assistant.Intent{Type: "task.retry", TargetID: task.ID, EvidenceIDs: []string{"task:" + task.ID}})
			if err != nil {
				t.Fatal(err)
			}
			var policy AssistantPolicy = assistantAllow
			switch mode {
			case "revoke":
				_, err = s.Pool.Exec(ctx, `UPDATE users SET disabled=true WHERE id=$1`, u.ID)
			case "stale":
				_, err = s.Pool.Exec(ctx, `UPDATE tasks SET description='corrected',updated_at=now() WHERE id=$1`, task.ID)
			case "policy":
				policy = nil
			case "stop":
				_, err = s.StopAssistantRun(ctx, p.ID, u.ID, j.Run.ID, "stop", j.Run.Revision)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = s.ResolveTask(WithAssistantMutation(ctx, j, a, policy), task.ID, "", u.Login, false); err == nil {
				t.Fatal("guard bypass")
			}
			if got := status(t, s, task.ID); got != domain.TaskFailed {
				t.Fatal(got)
			}
		})
	}
}
func TestAssistantIdempotencyLeaseBudgetAndNoise(t *testing.T) {
	s, u, p, _, c := assistantFixture(t)
	ctx := context.Background()
	_, r, fresh, err := s.CreateAssistantRun(ctx, p.ID, u.ID, "idem", "статус", "question", "")
	if err != nil || !fresh {
		t.Fatal(err)
	}
	_, again, fresh, err := s.CreateAssistantRun(ctx, p.ID, u.ID, "idem", "статус", "question", "")
	if err != nil || fresh || again.ID != r.ID {
		t.Fatalf("idempotency %v", err)
	}
	if _, _, _, err = s.CreateAssistantRun(ctx, p.ID, u.ID, "idem", "другое", "question", ""); err == nil {
		t.Fatal("body conflict ignored")
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	jobs := 0
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, e := s.ClaimAssistantJob(ctx)
			if e != nil {
				t.Error(e)
			}
			if j != nil {
				mu.Lock()
				jobs++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if jobs != 1 {
		t.Fatalf("executors %d", jobs)
	}
	if err = s.NotifyAssistantEvent(ctx, p.ID, "runner.heartbeat", "11111111-1111-1111-1111-111111111111", "same"); err != nil {
		t.Fatal(err)
	}
	var runs int
	if err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM assistant_runs WHERE project_id=$1`, p.ID).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("heartbeat created model work %d %v", runs, err)
	}
	// A required cost cap cannot be enforced with unknown prices.
	amount := 1.0
	c.Limits.DailyCostUSD = &amount
	_, err = s.SaveAssistantConfig(ctx, p.ID, u.ID, c.Revision, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = s.CreateAssistantRun(ctx, p.ID, u.ID, "price", "статус", "question", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE assistant_leases SET expires_at=now()-interval '1 second' WHERE project_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ClaimAssistantJob(ctx); err != nil {
		t.Fatal(err)
	} // expired prior job is failed conservatively
	if j, err := s.ClaimAssistantJob(ctx); err != nil || j != nil {
		t.Fatalf("unknown-priced model call allowed %v %v", j, err)
	}
}
func TestAssistantEventDebounceAndRevokedSponsor(t *testing.T) {
	s, u, p, task, _ := assistantFixture(t)
	ctx := context.Background()
	if err := s.NotifyAssistantEvent(ctx, p.ID, "task.failed", task.ID, "timeout-v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.NotifyAssistantEvent(ctx, p.ID, "task.failed", task.ID, "timeout-v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushAssistantEvents(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM assistant_runs WHERE project_id=$1`, p.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("not debounced %d %v", n, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE assistant_event_pending SET not_before=now() WHERE project_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushAssistantEvents(ctx); err != nil {
		t.Fatal(err)
	}
	j, err := s.ClaimAssistantJob(ctx)
	if err != nil || j == nil || j.Run.Trigger != "event" {
		t.Fatalf("event job %+v %v", j, err)
	}
	if err = s.FinishAssistantJob(ctx, *j, "completed", assistant.Diagnosis{Summary: "checked", Uncertainties: []string{}}, assistant.Usage{}); err != nil {
		t.Fatal(err)
	}
	// Repeated delivery remains deduplicated even outside the debounce window.
	if err = s.NotifyAssistantEvent(ctx, p.ID, "task.failed", task.ID, "timeout-v1"); err != nil {
		t.Fatal(err)
	}
	if err = s.FlushAssistantEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Pool.QueryRow(ctx, `SELECT count(*) FROM assistant_runs WHERE project_id=$1`, p.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("event replay %d %v", n, err)
	}
	// An already queued event cannot call the model after sponsor is disabled.
	if err = s.NotifyAssistantEvent(ctx, p.ID, "task.failed", task.ID, "different-cause"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE assistant_event_pending SET not_before=now() WHERE project_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FlushAssistantEvents(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE users SET disabled=true WHERE id=$1`, u.ID); err != nil {
		t.Fatal(err)
	}
	if j, err = s.ClaimAssistantJob(ctx); err != nil || j != nil {
		t.Fatalf("revoked sponsor claimed %+v %v", j, err)
	}
}
func TestAssistantStopReplayAndBodyConflict(t *testing.T) {
	s, u, p, _, _ := assistantFixture(t)
	ctx := context.Background()
	j := assistantJob(t, s, u, p, "stop-job")
	stopped, err := s.StopAssistantRun(ctx, p.ID, u.ID, j.Run.ID, "stop-key", j.Run.Revision)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.StopAssistantRun(ctx, p.ID, u.ID, j.Run.ID, "stop-key", j.Run.Revision)
	if err != nil || again.Revision != stopped.Revision {
		t.Fatalf("stop replay %v", err)
	}
	if _, err = s.StopAssistantRun(ctx, p.ID, u.ID, j.Run.ID, "stop-key", j.Run.Revision+1); err == nil {
		t.Fatal("stop body conflict ignored")
	}
}
func TestAssistantResumeGuardsAndDomainEvents(t *testing.T) {
	for _, kind := range []string{"epic.resume", "environment.resume"} {
		t.Run(kind, func(t *testing.T) {
			s, u, p, task, _ := assistantFixture(t)
			ctx := context.Background()
			target := task.EpicID
			if kind == "epic.resume" {
				if _, err := s.Pool.Exec(ctx, `UPDATE epics SET status='paused' WHERE id=$1`, target); err != nil {
					t.Fatal(err)
				}
			} else {
				env, err := s.CreateEnvironment(ctx, domain.Environment{ProjectID: p.ID, Name: "Stage", ExecType: domain.ExecSSH, Trigger: "auto", Paused: true})
				if err != nil {
					t.Fatal(err)
				}
				target = env.ID
				if err = s.SetEnvPaused(ctx, target, true); err != nil {
					t.Fatal(err)
				}
			}
			j := assistantJob(t, s, u, p, kind)
			a, err := s.ProposeAssistantAction(ctx, j, assistant.Intent{Type: kind, TargetID: target, EvidenceIDs: []string{strings.Split(kind, ".")[0] + ":" + target}})
			if err != nil {
				t.Fatal(err)
			}
			actx := WithAssistantMutation(ctx, j, a, assistantAllow)
			switch kind {
			case "epic.resume":
				err = s.TransitionEpic(actx, target, domain.EpicRunning, EventInput{ActorKind: domain.ActorUser, ActorID: u.Login, Type: "epic.status", Payload: map[string]any{"status": "running"}})
			case "environment.resume":
				err = s.SetEnvPaused(actx, target, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, actions, err := s.AssistantRun(ctx, p.ID, u.ID, j.Run.ID)
			if err != nil || len(actions) != 1 || actions[0].Status != "succeeded" {
				t.Fatalf("effect %+v %v", actions, err)
			}
		})
	}
}
func TestAssistantDailyTokenCapAndFencedRestart(t *testing.T) {
	s, u, p, task, c := assistantFixture(t)
	ctx := context.Background()
	c.Limits.DailyTokens = 1
	if _, err := s.SaveAssistantConfig(ctx, p.ID, u.ID, c.Revision, c); err != nil {
		t.Fatal(err)
	}
	_, r, _, err := s.CreateAssistantRun(ctx, p.ID, u.ID, "budget", "status", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if j, err := s.ClaimAssistantJob(ctx); err != nil || j != nil {
		t.Fatalf("budget admitted %+v %v", j, err)
	}
	got, _, err := s.AssistantRun(ctx, p.ID, u.ID, r.ID)
	if err != nil || got.Status != "budget_exhausted" {
		t.Fatalf("budget result %s %v", got.Status, err)
	}
	current, err := s.AssistantConfig(ctx, p.ID, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Limits.DailyTokens = 1000000
	if _, err = s.SaveAssistantConfig(ctx, p.ID, u.ID, current.Revision, current); err != nil {
		t.Fatal(err)
	}
	j := assistantJob(t, s, u, p, "fence")
	a, err := s.ProposeAssistantAction(ctx, j, assistant.Intent{Type: "task.retry", TargetID: task.ID, EvidenceIDs: []string{"task:" + task.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE assistant_leases SET epoch=epoch+1 WHERE project_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveTask(WithAssistantMutation(ctx, j, a, assistantAllow), task.ID, "", u.Login, false); err == nil {
		t.Fatal("fenced executor performed mutation")
	}
}

// Unknown charges must not disappear from the monetary cap after an owner
// enables it or selects a model whose current prices happen to be known.
func TestAssistantDailyUnknownCostBeforeEnablingCap(t *testing.T) {
	s, u, p, _, c := assistantFixture(t)
	ctx := context.Background()
	prior := assistantJob(t, s, u, p, "unknown-prior")
	if err := s.FinishAssistantJob(ctx, prior, "completed", assistant.Diagnosis{Summary: "проверено", Uncertainties: []string{}}, assistant.Usage{}); err != nil {
		t.Fatal(err)
	}
	var priorTokens int64
	var priorCost *float64
	if err := s.Pool.QueryRow(ctx, `SELECT reserved_tokens,reserved_cost_usd FROM assistant_runs WHERE id=$1`, prior.Run.ID).Scan(&priorTokens, &priorCost); err != nil || priorTokens == 0 || priorCost != nil {
		t.Fatalf("expected an unknown-cost reservation: tokens=%d cost=%v err=%v", priorTokens, priorCost, err)
	}
	assistantTestKnownPrice(t, s, u)
	capUSD := 1.0
	c.Limits.DailyCostUSD = &capUSD
	if _, err := s.SaveAssistantConfig(ctx, p.ID, u.ID, c.Revision, c); err != nil {
		t.Fatal(err)
	}
	_, run, _, err := s.CreateAssistantRun(ctx, p.ID, u.ID, "known-after-cap", "статус", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if job, err := s.ClaimAssistantJob(ctx); err != nil || job != nil {
		t.Fatalf("unknown prior cost admitted a provider call: job=%+v err=%v", job, err)
	}
	got, _, err := s.AssistantRun(ctx, p.ID, u.ID, run.ID)
	if err != nil || got.Status != "budget_exhausted" || got.Summary == nil || !strings.Contains(*got.Summary, "стоимость предыдущих вызовов") {
		t.Fatalf("unknown daily cost must explain the refusal: %+v %v", got, err)
	}
	var reasons, reservations int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE project_id=$1 AND type='assistant.run.updated' AND payload->>'run_id'=$2 AND payload->>'reason'='daily_cost_unknown'`, p.ID, run.ID).Scan(&reasons); err != nil || reasons != 1 {
		t.Fatalf("distinct daily_cost_unknown audit reason: count=%d err=%v", reasons, err)
	}
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM assistant_runs WHERE id=$1 AND reserved_tokens>0`, run.ID).Scan(&reservations); err != nil || reservations != 0 {
		t.Fatalf("refused run reserved a provider call: count=%d err=%v", reservations, err)
	}
}

func TestAssistantYesterdayUnknownCostDoesNotBlockToday(t *testing.T) {
	s, u, p, _, c := assistantFixture(t)
	ctx := context.Background()
	prior := assistantJob(t, s, u, p, "yesterday-unknown")
	if err := s.FinishAssistantJob(ctx, prior, "completed", assistant.Diagnosis{Summary: "проверено", Uncertainties: []string{}}, assistant.Usage{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE assistant_runs SET reserved_at=(date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')-interval '1 second' WHERE id=$1`, prior.Run.ID); err != nil {
		t.Fatal(err)
	}
	assistantTestKnownPrice(t, s, u)
	capUSD := 1.0
	c.Limits.DailyCostUSD = &capUSD
	if _, err := s.SaveAssistantConfig(ctx, p.ID, u.ID, c.Revision, c); err != nil {
		t.Fatal(err)
	}
	job := assistantJob(t, s, u, p, "today-known")
	if job.Run.Status != "running" || job.Model.InputPrice == nil || job.Model.OutputPrice == nil {
		t.Fatalf("yesterday's unknown cost blocked today's known-price call: %+v", job)
	}
}

func TestAssistantKnownDailyCostSumStillEnforcesCap(t *testing.T) {
	s, u, p, _, c := assistantFixture(t)
	ctx := context.Background()
	assistantTestKnownPrice(t, s, u)
	// The default 36k reserved tokens cost $0.036 at these fixture prices.
	// The second call would cost $0.072 in total, above this $0.05 cap.
	capUSD := 0.05
	c.Limits.DailyCostUSD = &capUSD
	if _, err := s.SaveAssistantConfig(ctx, p.ID, u.ID, c.Revision, c); err != nil {
		t.Fatal(err)
	}
	prior := assistantJob(t, s, u, p, "known-prior")
	if err := s.FinishAssistantJob(ctx, prior, "completed", assistant.Diagnosis{Summary: "проверено", Uncertainties: []string{}}, assistant.Usage{}); err != nil {
		t.Fatal(err)
	}
	_, run, _, err := s.CreateAssistantRun(ctx, p.ID, u.ID, "known-over-cap", "статус", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if job, err := s.ClaimAssistantJob(ctx); err != nil || job != nil {
		t.Fatalf("known charges exceeded the cap: job=%+v err=%v", job, err)
	}
	got, _, err := s.AssistantRun(ctx, p.ID, u.ID, run.ID)
	if err != nil || got.Status != "budget_exhausted" || got.Summary == nil || *got.Summary != "дневной бюджет исчерпан" {
		t.Fatalf("known cost cap result: %+v %v", got, err)
	}
	var unknownReasons int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE project_id=$1 AND payload->>'run_id'=$2 AND payload->>'reason'='daily_cost_unknown'`, p.ID, run.ID).Scan(&unknownReasons); err != nil || unknownReasons != 0 {
		t.Fatalf("known sum was mislabeled unknown: %d %v", unknownReasons, err)
	}
}

func TestAssistantUnknownDailyCostWithoutCapRemainsAllowed(t *testing.T) {
	s, u, p, _, _ := assistantFixture(t)
	ctx := context.Background()
	prior := assistantJob(t, s, u, p, "no-cap-prior")
	if err := s.FinishAssistantJob(ctx, prior, "completed", assistant.Diagnosis{Summary: "проверено", Uncertainties: []string{}}, assistant.Usage{}); err != nil {
		t.Fatal(err)
	}
	job := assistantJob(t, s, u, p, "no-cap-next")
	if job.Config.Limits.DailyCostUSD != nil || job.Run.Status != "running" {
		t.Fatalf("unknown cost without a monetary cap must remain allowed: %+v", job)
	}
}

func assistantTestKnownPrice(t *testing.T, s *Store, u domain.User) {
	t.Helper()
	price := int64(1_000_000) // microdollars per million tokens
	if _, err := s.SetConnectionModels(context.Background(), "mtplx-local", []domain.ModelEntry{{ID: "test-model", Label: "Test", Source: "manual", InputPrice: &price, OutputPrice: &price}}, u.ID, u.Login); err != nil {
		t.Fatal(err)
	}
}
