package orchestrator

import (
	"context"
	"encoding/json"
	"github.com/PavluninVladimir/rivet/internal/design"
	"github.com/PavluninVladimir/rivet/internal/store"
	"github.com/google/uuid"
	"strings"
	"testing"
)

func TestDesignHandoffDeliveredToCoderAndReviewer(t *testing.T) {
	ctx := context.Background()
	f := newPolicyFixture(t, "diff")
	model := design.Model{Schema: design.Schema, Tokens: []design.Token{}, Components: []design.Component{}, Screens: []design.Screen{{ID: uuid.NewString(), Name: "Screen", Viewport: design.Viewport{Width: 320, Height: 240}, Root: design.Node{ID: uuid.NewString(), Type: "button", Props: design.Properties{"text": json.RawMessage(`"Immutable design button"`)}, Children: []design.Node{}}}}, Overrides: []design.Override{}}
	raw, err := f.st.DesignCreateDocument(ctx, f.p.ID, f.owner.ID, uuid.NewString(), "Main", model)
	if err != nil {
		t.Fatal(err)
	}
	var doc store.DesignDocument
	if err = json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	raw, err = f.st.DesignCreateHandoff(ctx, f.p.ID, f.owner.ID, doc.ID, uuid.NewString(), 1, nil, []store.DesignContextRef{{Kind: "task", ID: f.task.ID}}, store.DesignAsset{ID: uuid.NewString(), SHA256: strings.Repeat("a", 64), MIME: "image/png", SizeBytes: 123, ObjectRef: "s3://private/not-for-model"})
	if err != nil {
		t.Fatal(err)
	}
	var h store.DesignHandoff
	if err = json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	if err = f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	coder := f.out.lastAssign(t)
	for _, want := range []string{h.ID, h.ManifestSHA, h.ModelSHA, "Immutable design button"} {
		if !strings.Contains(coder.Description, want) {
			t.Fatalf("coder lacks %q", want)
		}
	}
	if strings.Contains(coder.Description, "s3://") || strings.Contains(coder.Description, "not-for-model") {
		t.Fatal("private asset location escaped")
	}
	// Later design edits cannot change the delivered immutable revision.
	_, err = f.st.DesignPatchDocument(ctx, f.p.ID, f.owner.ID, doc.ID, uuid.NewString(), store.DesignPatchRequest{ExpectedRevision: 1, Operations: []design.Patch{{Op: "set_property", Scope: "instance", ElementID: model.Screens[0].Root.ID, Property: "text", Value: json.RawMessage(`"Later draft"`)}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	driveToReview(t, f)
	if err = f.e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	reviewer := f.out.lastAssign(t)
	if !strings.Contains(reviewer.Description, h.ManifestSHA) || !strings.Contains(reviewer.Description, "Immutable design button") || strings.Contains(reviewer.Description, "Later draft") {
		t.Fatal("reviewer got a different design revision")
	}
	// Domain task description stays user-authored; only assignment context is enriched.
	task, err := f.st.GetTask(ctx, f.task.ID)
	if err != nil || task.Description != f.task.Description {
		t.Fatal("task overwritten", err)
	}
}
