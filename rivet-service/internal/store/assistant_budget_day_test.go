package store

import (
	"context"
	"github.com/PavluninVladimir/rivet/internal/assistant"
	"testing"
)

func TestAssistantBudgetUsesReservationDayForOldQueue(t *testing.T) {
	s, u, p, _, c := assistantFixture(t)
	ctx := context.Background()
	reserve := int64(c.Limits.MaxInputTokens + c.Limits.MaxOutputTokens)
	c.Limits.DailyTokens = reserve
	if _, err := s.SaveAssistantConfig(ctx, p.ID, u.ID, c.Revision, c); err != nil {
		t.Fatal(err)
	}
	_, r, _, err := s.CreateAssistantRun(ctx, p.ID, u.ID, "old-queue-first", "статус", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE assistant_runs SET created_at=now()-interval '2 days' WHERE id=$1`, r.ID); err != nil {
		t.Fatal(err)
	}
	j, err := s.ClaimAssistantJob(ctx)
	if err != nil || j == nil {
		t.Fatal(j, err)
	}
	if err = s.FinishAssistantJob(ctx, *j, "completed", assistant.Diagnosis{Summary: "Статус проверен"}, assistant.Usage{}); err != nil {
		t.Fatal(err)
	}
	_, second, _, err := s.CreateAssistantRun(ctx, p.ID, u.ID, "old-queue-second", "статус", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Pool.Exec(ctx, `UPDATE assistant_runs SET created_at=now()-interval '1 day' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	j, err = s.ClaimAssistantJob(ctx)
	if err != nil || j != nil {
		t.Fatal("old queue bypassed today's budget", j, err)
	}
	run, _, err := s.AssistantRun(ctx, p.ID, u.ID, second.ID)
	if err != nil || run.Status != "budget_exhausted" {
		t.Fatal(run, err)
	}
}
