package orchestrator

import (
	"context"
	"strings"
	"testing"
)

func TestPassedAssessmentFeedsAssignmentWithoutRewritingTask(t *testing.T) {
	f := newPolicyFixture(t, "")
	ctx := context.Background()
	if _, err := f.st.Pool.Exec(ctx, `UPDATE epics SET architecture_required=true WHERE id=$1`, f.epic.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Pool.Exec(ctx, `INSERT INTO epic_architecture_assessments(epic_id,project_id,revision,status,result,context)
		VALUES($1,$2,1,'passed','{"decision":"changes_required","summary":"legacy plan needs constraints","impact":[],"constraints":["Keep tenant isolation"],"documentation":["Document counter API"],"risks":[]}', '{"existing_plan":[{"title":"preserved"}]}')`, f.epic.ID, f.p.ID); err != nil {
		t.Fatal(err)
	}
	original := f.task.Description
	description, err := f.e.assessedTaskDescription(ctx, f.task)
	if err != nil || !strings.Contains(description, "Keep tenant isolation") || !strings.Contains(description, "Document counter API") {
		t.Fatalf("missing execution context %s %v", description, err)
	}
	got, err := f.st.GetTask(ctx, f.task.ID)
	if err != nil || got.Description != original {
		t.Fatalf("task record rewritten: %+v %v", got, err)
	}
}
