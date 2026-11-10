package store

import (
	"context"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"testing"
)

func TestCriteriaReportsPassAndReviewer(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "criteria", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	epic, err := s.CreateEpic(ctx, p.ID, "criteria", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, epic.ID, NewTask{Title: "A", Criteria: []domain.Criterion{{Text: "one"}, {Text: "two"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET step_gen=3 WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	record := func(pass int, by string, results ...CriterionResult) {
		t.Helper()
		if err := s.RecordCriteriaResults(ctx, task.ID, CriteriaReport{Pass: pass, By: by, Results: results}); err != nil {
			t.Fatal(err)
		}
	}
	get := func() []domain.Criterion {
		t.Helper()
		task, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		return task.Criteria
	}
	record(3, "first", CriterionResult{Index: 1, OK: false, Note: "нет теста"}, CriterionResult{Index: 99, OK: true})
	record(3, "first", CriterionResult{Index: 1, OK: true})
	record(3, "second", CriterionResult{Index: 1, OK: true}, CriterionResult{Index: 2, OK: true})
	got := get()
	if got[0].Status != "fail" || got[0].By != "first" || !got[1].OK {
		t.Fatalf("FAIL перекрыт: %+v", got)
	}
	record(2, "stale", CriterionResult{Index: 1, OK: true})
	if get()[0].Status != "fail" {
		t.Fatal("устаревший отчёт принят")
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET step_gen=4 WHERE id=$1`, task.ID); err != nil {
		t.Fatal(err)
	}
	record(4, "new", CriterionResult{Index: 1, OK: true})
	got = get()
	if got[0].Status != "ok" || got[0].Note != "" || got[1].Status != "" {
		t.Fatalf("новый проход не заменил старый: %+v", got)
	}
}

// Независимо от порядка гонки, у закрытого ok-прохода не может остаться FAIL.
func TestCriteriaClaimConcurrentReport(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "claim", "o/r", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	epic, err := s.CreateEpic(ctx, p.ID, "claim", "")
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, epic.ID, NewTask{Title: "A", Criteria: []domain.Criterion{{Text: "one"}}})
	if err != nil {
		t.Fatal(err)
	}
	for pass := 1; pass <= 15; pass++ {
		if _, err := s.Pool.Exec(ctx, `UPDATE tasks SET step_gen=$2,criteria='[{"text":"one","ok":false}]' WHERE id=$1`, task.ID, pass); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			<-start
			done <- s.RecordCriteriaResults(ctx, task.ID, CriteriaReport{Pass: pass, By: "same-runner", Results: []CriterionResult{{Index: 1, OK: false, Note: "fail"}}})
		}()
		close(start)
		claim, err := s.ClaimReviewOutcome(ctx, task.ID, pass, "ok")
		if err != nil || !claim.Claimed {
			t.Fatalf("claim: %v %v", claim.Claimed, err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		got, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Criteria[0].Status == "fail" && claim.Outcome != "changes" {
			t.Fatal("ok закрыл проход с FAIL")
		}
		if err := s.RecordCriteriaResults(ctx, task.ID, CriteriaReport{Pass: pass, By: "late", Results: []CriterionResult{{Index: 1, OK: false}}}); err != nil {
			t.Fatal(err)
		}
		late, _ := s.GetTask(ctx, task.ID)
		if late.Criteria[0] != got.Criteria[0] {
			t.Fatal("закрытый проход изменён поздним отчётом")
		}
	}
}
