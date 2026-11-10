package orchestrator

import (
	"context"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	pb "github.com/PavluninVladimir/rivet/pkg/protocol"
	"strings"
	"testing"
)

func TestReviewCriteriaResults(t *testing.T) {
	for _, mode := range []string{"ok", "fail", "old", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newPolicyFixture(t, "diff")
			driveToReview(t, f)
			if err := f.e.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			as := f.out.lastAssign(t)
			sr := &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_REVIEW, Ok: true, Detail: "review пройден"}
			if mode != "old" {
				sr.Criteria = []*pb.CriterionResult{{Index: 1, Ok: mode == "ok", Note: "нет теста"}}
			}
			if mode == "duplicate" {
				sr.Criteria = append(sr.Criteria, &pb.CriterionResult{Index: 1, Ok: true})
			}
			if err := f.e.OnStageResult(ctx, "reviewer", sr); err != nil {
				t.Fatal(err)
			}
			task := mustTask(t, f)
			c := task.Criteria[0]
			switch mode {
			case "fail":
				if c.Status != "fail" || c.OK || c.By != "reviewer" || c.Note != "нет теста" || task.Status != domain.TaskFixing {
					t.Fatalf("FAIL при APPROVED: %+v %+v", c, task)
				}
				found := false
				for _, ev := range eventsOfType(t, f.st, f.task.ID, "task.status") {
					if s, ok := ev.Payload["detail"].(string); ok && strings.Contains(s, "Не выполнены критерии") {
						found = true
					}
				}
				if !found {
					t.Fatal("нет причины по критериям в timeline")
				}
			case "old":
				if c.Status != "" || c.OK {
					t.Fatalf("старый runner: %+v", c)
				}
			default:
				if c.Status != "ok" || !c.OK || c.Note != "" {
					t.Fatalf("OK: %+v", c)
				}
			}
		})
	}
}
func TestReviewCriteriaFailSurvivesAnotherReviewerAny(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff")
	twoReviewers(t, f)
	setProcess(t, f, reviewWithTwoAgents(policy.ModeParallel, policy.RequireAny))
	driveToReview(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	var reviews []*pb.Assignment
	for _, as := range f.out.assignsSince(0) {
		if as.Stage == pb.StageResult_REVIEW {
			reviews = append(reviews, as)
		}
	}
	if len(reviews) != 2 {
		t.Fatalf("назначений %d", len(reviews))
	}
	for i, as := range reviews {
		run, err := f.st.RunBySession(ctx, as.SessionId)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.e.OnStageResult(ctx, run.RunnerID, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: pb.StageResult_REVIEW, Ok: true, Criteria: []*pb.CriterionResult{{Index: 1, Ok: i == 1, Note: "нет теста"}}}); err != nil {
			t.Fatal(err)
		}
	}
	task := mustTask(t, f)
	if task.Criteria[0].Status != "fail" || task.Status != domain.TaskFixing {
		t.Fatalf("одобрение перекрыло FAIL: %+v", task)
	}
}

func TestReviewCriteriaNewPassWithoutReportClearsOld(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff")
	driveToReview(t, f)
	if err := f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	as := f.out.lastAssign(t)
	run, err := f.st.RunBySession(ctx, as.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.e.OnStageResult(ctx, run.RunnerID, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: as.Stage, Ok: true, Criteria: []*pb.CriterionResult{{Index: 1, Ok: false, Note: "исправить"}}}); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if err := f.e.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		as = f.out.lastAssign(t)
		run, err = f.st.RunBySession(ctx, as.SessionId)
		if err != nil {
			t.Fatal(err)
		}
		if as.Stage == pb.StageResult_REVIEW {
			task := mustTask(t, f)
			if task.Criteria[0].Status != "" || task.Criteria[0].OK {
				t.Fatalf("старые статусы не сброшены: %+v", task.Criteria)
			}
			if err := f.e.OnStageResult(ctx, run.RunnerID, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: as.Stage, Ok: true}); err != nil {
				t.Fatal(err)
			}
			if task := mustTask(t, f); task.Criteria[0].Status != "" {
				t.Fatal("старый runner должен оставить непроверенным")
			}
			return
		}
		if err := f.e.OnStageResult(ctx, run.RunnerID, &pb.StageResult{TaskId: f.task.ID, SessionId: as.SessionId, Stage: as.Stage, Ok: true}); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("не дошли до нового review")
}
