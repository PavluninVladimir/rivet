package store

import (
	"context"
	"github.com/PavluninVladimir/rivet/internal/assistant"
	"strconv"
	"testing"
)

func TestAssistantActionErrorPersistsDecisionURL(t *testing.T) {
	for _, code := range []int{403, 409, 429, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			s, u, p, task, _ := assistantFixture(t)
			ctx := context.Background()
			j := assistantJob(t, s, u, p, "error")
			a, err := s.ProposeAssistantAction(ctx, j, assistant.Intent{Type: "task.retry", TargetID: task.ID, EvidenceIDs: []string{"task:" + task.ID}})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.RecordAssistantActionError(ctx, j, a, assistant.Err(code, "guard_test", "guard blocked action")); err != nil {
				t.Fatal(err)
			}
			var status, url string
			if err = s.Pool.QueryRow(ctx, `SELECT status,result->>'decision_url' FROM assistant_actions WHERE id=$1`, a.ID).Scan(&status, &url); err != nil || url == "" || status == "proposed" {
				t.Fatal(status, url, err)
			}
			original, err := s.GetTask(ctx, task.ID)
			if err != nil || original.Status != "failed" {
				t.Fatal("error path mutated task", original, err)
			}
		})
	}
}
