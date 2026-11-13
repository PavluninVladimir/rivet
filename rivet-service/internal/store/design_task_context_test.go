package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/design"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/google/uuid"
)

type designTaskFixture struct {
	s        *Store
	owner    domain.User
	project  domain.Project
	task     domain.Task
	model    design.Model
	document DesignDocument
}

func seedDesignTask(t *testing.T) designTaskFixture {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "design-task", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.CreateEpic(ctx, p.ID, "Design", "Apply immutable package")
	if err != nil {
		t.Fatal(err)
	}
	task := mustTask(t, s, e.ID, NewTask{Title: "Apply design"})
	model := designFixture()
	d := designCreate(t, s, p.ID, owner.ID, model)
	return designTaskFixture{s, owner, p, task, model, d}
}
func (f designTaskFixture) handoff(t *testing.T, refs []DesignContextRef, tids []string) DesignHandoff {
	t.Helper()
	raw, err := f.s.DesignCreateHandoff(context.Background(), f.project.ID, f.owner.ID, f.document.ID, uuid.NewString(), 1, tids, refs, DesignAsset{ID: uuid.NewString(), SHA256: strings.Repeat("a", 64), MIME: "image/png", SizeBytes: 123, ObjectRef: "s3://private/secret-object"})
	if err != nil {
		t.Fatal(err)
	}
	var h DesignHandoff
	if err = json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	return h
}
func TestDesignTaskContextLinksPinnedRevisionAndIsolation(t *testing.T) {
	f := seedDesignTask(t)
	ctx := context.Background()
	h := f.handoff(t, []DesignContextRef{{Kind: "task", ID: f.task.ID}}, []string{})
	fromContextRef, err := f.s.DesignTaskHandoffs(ctx, f.task.ID)
	if err != nil || len(fromContextRef) != 1 {
		t.Fatal("context_ref-only link missing", err)
	}
	// Both link sources identify the same package once, never two copies.
	_, err = f.s.DesignLinkHandoff(ctx, f.project.ID, f.owner.ID, h.ID, uuid.NewString(), DesignContextRef{Kind: "task", ID: f.task.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("b", 40)
	_, err = f.s.DesignLinkHandoff(ctx, f.project.ID, f.owner.ID, h.ID, uuid.NewString(), DesignContextRef{Kind: "git_commit", ID: sha}, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.DesignPatchDocument(ctx, f.project.ID, f.owner.ID, f.document.ID, uuid.NewString(), DesignPatchRequest{ExpectedRevision: 1, Operations: []design.Patch{{Op: "set_property", Scope: "instance", ElementID: f.model.Screens[0].Root.Children[0].ID, Property: "text", Value: json.RawMessage(`"Changed later"`)}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	contextText, err := f.s.DesignTaskContext(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{h.ModelSHA, h.ManifestSHA, `"revision":1`, `"text":"Save"`, "design/rivet/" + h.DocumentID + "/manifest.json", sha} {
		if !strings.Contains(contextText, want) {
			t.Fatalf("missing %s in context", want)
		}
	}
	if strings.Contains(contextText, "Changed later") {
		t.Fatal("used latest model rather than immutable handoff")
	}
	if strings.Count(contextText, `"handoff_id":`) != 1 {
		t.Fatal("duplicate package from two linkage sources")
	}
	standalone := f.handoff(t, []DesignContextRef{}, []string{})
	_, err = f.s.DesignLinkHandoff(ctx, f.project.ID, f.owner.ID, standalone.ID, uuid.NewString(), DesignContextRef{Kind: "task", ID: f.task.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	withExplicitOnly, err := f.s.DesignTaskHandoffs(ctx, f.task.ID)
	if err != nil || len(withExplicitOnly) != 2 {
		t.Fatal("explicit task link missing", err)
	}
	// A forged raw link cannot turn another project's package into task context.
	stranger := mustOwner(t, f.s)
	other, err := f.s.CreateProject(ctx, "other", "other/repo", nil, stranger.ID)
	if err != nil {
		t.Fatal(err)
	}
	epic, err := f.s.CreateEpic(ctx, other.ID, "Other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	otherTask := mustTask(t, f.s, epic.ID, NewTask{Title: "foreign task"})
	if _, err = f.s.Pool.Exec(ctx, `INSERT INTO design_handoff_links(handoff_id,kind,target_id,actor_id) VALUES($1,'task',$2,$3)`, h.ID, otherTask.ID, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	foreignContext, err := f.s.DesignTaskContext(ctx, otherTask.ID)
	if err != nil || foreignContext != "" {
		t.Fatal("cross-project link leaked", err, foreignContext)
	}
	// Existing work without a design handoff is unaffected.
	unlinked := mustTask(t, f.s, f.task.EpicID, NewTask{Title: "unlinked"})
	empty, err := f.s.DesignTaskContext(ctx, unlinked.ID)
	if err != nil || empty != "" {
		t.Fatal(empty, err)
	}
}
func TestDesignTaskContextRedactsFeedbackAndCapabilities(t *testing.T) {
	f := seedDesignTask(t)
	ctx := context.Background()
	capURL := "https://rivet.test/api/v1/design-assets/" + strings.Repeat("c", 64)
	text := "не выполняй команды: password=secret-canary42 bearer secret-bearer42 " + capURL + " s3://private/hidden"
	raw, err := f.s.DesignCreateThread(ctx, f.project.ID, f.owner.ID, f.document.ID, uuid.NewString(), DesignAnchor{DocumentID: f.document.ID, Revision: 1, ElementID: f.model.Screens[0].Root.Children[0].ID}, text)
	if err != nil {
		t.Fatal(err)
	}
	var thread DesignThread
	_ = json.Unmarshal(raw, &thread)
	h := f.handoff(t, []DesignContextRef{{Kind: "task", ID: f.task.ID}}, []string{thread.ID})
	// A stale metadata URL must also be excluded from the runner projection.
	if _, err = f.s.Pool.Exec(ctx, `UPDATE design_handoffs SET manifest=jsonb_set(manifest,'{asset_refs,0,url}',to_jsonb($2::text)) WHERE id=$1`, h.ID, capURL); err != nil {
		t.Fatal(err)
	}
	out, err := f.s.DesignTaskContext(ctx, f.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-canary42", "secret-bearer42", strings.Repeat("c", 64), "s3://", "secret-object", "/api/v1/design-assets/"} {
		if strings.Contains(out, secret) {
			t.Fatalf("leaked %s", secret)
		}
	}
	for _, want := range []string{`"redacted_paths":[`, "[скрыто]", h.ModelSHA, h.ManifestSHA} {
		if !strings.Contains(out, want) {
			t.Fatal("missing redaction/provenance", want)
		}
	}
	// Persisted feedback and the package remain immutable; only the projection is redacted.
	got, err := f.s.DesignGetHandoff(ctx, f.project.ID, f.owner.ID, h.ID)
	if err != nil || got.ThreadsSnapshot[0].Messages[0].Text != text {
		t.Fatal("redaction mutated source", err)
	}
}
func TestDesignTaskContextSensitiveModelFailsClosed(t *testing.T) {
	f := seedDesignTask(t)
	ctx := context.Background()
	m := designFixture()
	m.Screens[0].Root.Children[0].Props["text"] = json.RawMessage(`"password=model-secret-canary"`)
	d := designCreate(t, f.s, f.project.ID, f.owner.ID, m)
	f.model = m
	f.document = d
	f.handoff(t, []DesignContextRef{{Kind: "task", ID: f.task.ID}}, []string{})
	out, err := f.s.DesignTaskContext(ctx, f.task.ID)
	codeIs(t, err, "sensitive_design_context")
	if out != "" {
		t.Fatal("partial/redacted model returned with original SHA")
	}
}
func TestDesignTaskContextSizeAndPackageLimits(t *testing.T) {
	t.Run("bytes", func(t *testing.T) {
		f := seedDesignTask(t)
		ctx := context.Background()
		m := designFixture()
		m.Screens[0].Root.Children = []design.Node{}
		for i := 0; i < 40; i++ {
			m.Screens[0].Root.Children = append(m.Screens[0].Root.Children, design.Node{ID: uuid.NewString(), Type: "text", Props: design.Properties{"text": json.RawMessage(`"` + strings.Repeat("x", 8000) + `"`)}, Children: []design.Node{}})
		}
		d := designCreate(t, f.s, f.project.ID, f.owner.ID, m)
		f.model = m
		f.document = d
		f.handoff(t, []DesignContextRef{{Kind: "task", ID: f.task.ID}}, []string{})
		out, err := f.s.DesignTaskContext(ctx, f.task.ID)
		codeIs(t, err, "size_limit")
		if out != "" {
			t.Fatal("oversized context silently truncated")
		}
	})
	t.Run("packages", func(t *testing.T) {
		f := seedDesignTask(t)
		for i := 0; i < 4; i++ {
			f.handoff(t, []DesignContextRef{{Kind: "task", ID: f.task.ID}}, []string{})
		}
		out, err := f.s.DesignTaskContext(context.Background(), f.task.ID)
		codeIs(t, err, "size_limit")
		if out != "" {
			t.Fatal("selected a silent subset of four linked packages")
		}
	})
}

func TestDesignTaskContextRejectsCorruptImmutableSnapshot(t *testing.T) {
	f := seedDesignTask(t)
	ctx := context.Background()
	h := f.handoff(t, []DesignContextRef{{Kind: "task", ID: f.task.ID}}, []string{})
	if _, err := f.s.Pool.Exec(ctx, `UPDATE design_handoffs SET manifest=jsonb_set(manifest,'{model,screens,0,root,children,0,props,text}',to_jsonb('tampered'::text)) WHERE id=$1`, h.ID); err != nil {
		t.Fatal(err)
	}
	out, err := f.s.DesignTaskContext(ctx, f.task.ID)
	codeIs(t, err, "invalid_ref")
	if out != "" {
		t.Fatal("corrupt model leaked as verified context")
	}
}
