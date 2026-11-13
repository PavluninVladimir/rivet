package store

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/design"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/google/uuid"
)

func designFixture() design.Model {
	return design.Model{Schema: design.Schema, Tokens: []design.Token{{ID: uuid.NewString(), Name: "space", Type: "spacing", Value: json.RawMessage(`{"px":12}`)}}, Components: []design.Component{}, Screens: []design.Screen{{ID: uuid.NewString(), Name: "Main", Viewport: design.Viewport{Width: 320, Height: 240}, Root: design.Node{ID: uuid.NewString(), Type: "container", Props: design.Properties{}, Children: []design.Node{{ID: uuid.NewString(), Type: "button", Props: design.Properties{"text": json.RawMessage(`"Save"`)}, Children: []design.Node{}}}}}}, Overrides: []design.Override{}}
}
func designCreate(t *testing.T, s *Store, pid, actor string, m design.Model) DesignDocument {
	t.Helper()
	raw, err := s.DesignCreateDocument(context.Background(), pid, actor, uuid.NewString(), "Main", m)
	if err != nil {
		t.Fatal(err)
	}
	var d DesignDocument
	if err = json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}
func codeIs(t *testing.T, err error, code string) {
	t.Helper()
	var e *design.Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}
func TestDesignDurableRevisionsCASACLAndUndo(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "design", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := designFixture()
	d := designCreate(t, s, p.ID, owner.ID, m)
	node := m.Screens[0].Root.Children[0].ID
	originalExport, err := s.DesignExport(ctx, p.ID, owner.ID, d.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	patch := DesignPatchRequest{ExpectedRevision: 1, Operations: []design.Patch{{Op: "set_property", ElementID: node, Property: "padding", Value: json.RawMessage(`24`), Scope: "instance"}}}
	key := uuid.NewString()
	raw, err := s.DesignPatchDocument(ctx, p.ID, owner.ID, d.ID, key, patch, false)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := s.DesignPatchDocument(ctx, p.ID, owner.ID, d.ID, key, patch, false)
	if err != nil || string(raw) != string(replayed) {
		t.Fatal("replay", err)
	}
	changed := patch
	changed.ExpectedRevision = 2
	_, err = s.DesignPatchDocument(ctx, p.ID, owner.ID, d.ID, key, changed, false)
	codeIs(t, err, "idempotency_conflict")
	_, err = s.DesignPatchDocument(ctx, p.ID, owner.ID, d.ID, uuid.NewString(), patch, false)
	codeIs(t, err, "revision_conflict")
	saved, err := s.DesignGetDocument(ctx, p.ID, owner.ID, d.ID, 0)
	if err != nil || saved.Document.Revision != 2 || string(saved.Model.Screens[0].Root.Children[0].Props["padding"]) != "24" {
		t.Fatal(saved, err)
	}
	afterExport, err := s.DesignExport(ctx, p.ID, owner.ID, d.ID, 1)
	if err != nil || string(originalExport) != string(afterExport) {
		t.Fatal("historical export drift", err)
	}
	original, err := s.DesignGetDocument(ctx, p.ID, owner.ID, d.ID, 1)
	if err != nil || original.Model.Screens[0].Root.Children[0].Props["padding"] != nil {
		t.Fatal("immutable original", err)
	}
	outsider := mustOwner(t, s)
	_, err = s.DesignGetDocument(ctx, p.ID, outsider.ID, d.ID, 0)
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-project leak", err)
	}
	_, err = s.DesignPatchDocument(ctx, p.ID, outsider.ID, d.ID, uuid.NewString(), changed, false)
	if !errors.Is(err, ErrNotFound) {
		t.Fatal("cross-project mutation", err)
	}
	raw, err = s.DesignUndo(ctx, p.ID, owner.ID, d.ID, uuid.NewString(), 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	var undo DesignSave
	_ = json.Unmarshal(raw, &undo)
	if undo.Document.Revision != 3 || undo.Document.ContentSHA != d.ContentSHA {
		t.Fatal("undo lost content", undo)
	}
	rows, next, err := s.DesignRevisions(ctx, p.ID, owner.ID, d.ID, 0, 2)
	if err != nil || len(rows) != 2 || next != 2 {
		t.Fatal(rows, next, err)
	}
}
func TestDesignAnchorsTombstonesAndImmutableHandoff(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "design", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := designFixture()
	d := designCreate(t, s, p.ID, owner.ID, m)
	node := m.Screens[0].Root.Children[0].ID
	raw, err := s.DesignCreateThread(ctx, p.ID, owner.ID, d.ID, uuid.NewString(), DesignAnchor{DocumentID: d.ID, Revision: 1, ElementID: node}, "<script>feedback</script>")
	if err != nil {
		t.Fatal(err)
	}
	var thread DesignThread
	_ = json.Unmarshal(raw, &thread)
	stranger := mustOwner(t, s)
	if err = s.AddMember(ctx, p.ID, stranger.Login, domain.RoleMember); err != nil {
		t.Fatal(err)
	}
	_, err = s.DesignEditComment(ctx, p.ID, stranger.ID, thread.Messages[0].ID, uuid.NewString(), 1, "hijack", false)
	codeIs(t, err, "forbidden")
	screenshot := DesignAsset{ID: uuid.NewString(), SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", MIME: "image/png", SizeBytes: 123, ObjectRef: "s3://private/example"}
	raw, err = s.DesignCreateHandoff(ctx, p.ID, owner.ID, d.ID, uuid.NewString(), 1, []string{thread.ID}, []DesignContextRef{}, screenshot)
	if err != nil {
		t.Fatal(err)
	}
	var h DesignHandoff
	_ = json.Unmarshal(raw, &h)
	_, err = s.DesignPatchDocument(ctx, p.ID, owner.ID, d.ID, uuid.NewString(), DesignPatchRequest{ExpectedRevision: 1, Operations: []design.Patch{{Op: "remove_node", ElementID: node}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	threads, _, err := s.DesignListThreads(ctx, p.ID, owner.ID, d.ID, "", 20)
	if err != nil || len(threads) != 1 || !threads[0].Orphaned {
		t.Fatal(threads, err)
	}
	_, err = s.DesignEditComment(ctx, p.ID, owner.ID, thread.Messages[0].ID, uuid.NewString(), 1, "", true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.DesignGetHandoff(ctx, p.ID, owner.ID, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ManifestSHA != h.ManifestSHA || got.ThreadsSnapshot[0].Messages[0].Deleted || got.ThreadsSnapshot[0].Orphaned {
		t.Fatal("handoff drifted")
	}
	_, err = s.DesignUndo(ctx, p.ID, owner.ID, d.ID, uuid.NewString(), 2, 2)
	if err != nil {
		t.Fatal("undo must restore same node identity", err)
	}
	threads, _, err = s.DesignListThreads(ctx, p.ID, owner.ID, d.ID, "", 20)
	if err != nil || threads[0].Orphaned || !threads[0].Messages[0].Deleted || threads[0].Messages[0].Text != "" {
		t.Fatal("anchor restoration/tombstone", threads, err)
	}
	removed := DesignPatchRequest{ExpectedRevision: 3, Operations: []design.Patch{{Op: "remove_node", ElementID: node}}}
	_, err = s.DesignPatchDocument(ctx, p.ID, owner.ID, d.ID, uuid.NewString(), removed, false)
	if err != nil {
		t.Fatal(err)
	}
	index := 0
	_, err = s.DesignPatchDocument(ctx, p.ID, owner.ID, d.ID, uuid.NewString(), DesignPatchRequest{ExpectedRevision: 4, Operations: []design.Patch{{Op: "add_node", ParentID: m.Screens[0].Root.ID, Node: &m.Screens[0].Root.Children[0], Index: &index}}}, false)
	codeIs(t, err, "invalid_ref")
}
func TestDesignAssetCapabilityRevocation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "design", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	member := mustOwner(t, s)
	if err = s.AddMember(ctx, p.ID, member.Login, domain.RoleMember); err != nil {
		t.Fatal(err)
	}
	a := DesignAsset{ID: uuid.NewString(), SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", MIME: "image/png", SizeBytes: 1, ObjectRef: "s3://private/a"}
	_, err = s.DesignRegisterAsset(ctx, p.ID, member.ID, uuid.NewString(), a)
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.DesignAssetReadURL(ctx, p.ID, member.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	token := read.URL[len("/api/v1/design-assets/"):]
	if _, err = s.DesignResolveAssetRead(ctx, token); err != nil {
		t.Fatal(err)
	}
	if err = s.RemoveMember(ctx, p.ID, member.Login); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DesignResolveAssetRead(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked capability", err)
	}
}
func TestDesignConcurrentSaveSingleWinner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "design", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := designFixture()
	d := designCreate(t, s, p.ID, owner.ID, m)
	patch := DesignPatchRequest{ExpectedRevision: 1, Operations: []design.Patch{{Op: "set_property", Scope: "instance", ElementID: m.Screens[0].Root.Children[0].ID, Property: "padding", Value: json.RawMessage(`24`)}}}
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.DesignPatchDocument(ctx, p.ID, owner.ID, d.ID, uuid.NewString(), patch, false)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			wins++
		} else {
			var e *design.Error
			if errors.As(err, &e) && e.Code == "revision_conflict" {
				conflicts++
			} else {
				t.Fatal(err)
			}
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatal(wins, conflicts)
	}
}

func TestDesignPinnedBaseAndOwnerGuard(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	owner := mustOwner(t, s)
	p, err := s.CreateProject(ctx, "design", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	m := designFixture()
	base := designCreate(t, s, p.ID, owner.ID, m)
	m.Base = &design.BaseRef{DocumentID: base.ID, Revision: base.Revision, ContentSHA: base.ContentSHA}
	derived := designCreate(t, s, p.ID, owner.ID, m)
	raw, err := s.DesignPatchDocument(ctx, p.ID, owner.ID, base.ID, uuid.NewString(), DesignPatchRequest{ExpectedRevision: 1, Operations: []design.Patch{{Op: "set_token", TokenID: m.Tokens[0].ID, Value: json.RawMessage(`{"px":32}`)}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	var updated DesignSave
	_ = json.Unmarshal(raw, &updated)
	prior, err := s.DesignGetDocument(ctx, p.ID, owner.ID, derived.ID, 0)
	if err != nil || prior.Model.Base.Revision != 1 || string(prior.Model.Tokens[0].Value) != `{"px": 12}` && string(prior.Model.Tokens[0].Value) != `{"px":12}` {
		t.Fatal(prior.Model, err)
	}
	member := mustOwner(t, s)
	if err = s.AddMember(ctx, p.ID, member.Login, domain.RoleMember); err != nil {
		t.Fatal(err)
	}
	op := DesignPatchRequest{ExpectedRevision: 1, Operations: []design.Patch{{Op: "update_base", Base: &design.BaseRef{DocumentID: base.ID, Revision: 2, ContentSHA: updated.Document.ContentSHA}}}}
	_, err = s.DesignPatchDocument(ctx, p.ID, member.ID, derived.ID, uuid.NewString(), op, false)
	codeIs(t, err, "forbidden")
	_, err = s.DesignPatchDocument(ctx, p.ID, owner.ID, derived.ID, uuid.NewString(), op, false)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.DesignGetDocument(ctx, p.ID, owner.ID, derived.ID, 0)
	if err != nil || current.Model.Base.Revision != 2 {
		t.Fatal(current.Model, err)
	}
	var spacing struct {
		PX int `json:"px"`
	}
	_ = json.Unmarshal(current.Model.Tokens[0].Value, &spacing)
	if spacing.PX != 32 {
		t.Fatal("base token not updated")
	}
	// A raw dangling/cross-project reference never grants access to a library.
	otherOwner := mustOwner(t, s)
	other, err := s.CreateProject(ctx, "other", "other/repo", nil, otherOwner.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreign := designCreate(t, s, other.ID, otherOwner.ID, designFixture())
	current.Model.Base = &design.BaseRef{DocumentID: foreign.ID, Revision: foreign.Revision, ContentSHA: foreign.ContentSHA}
	_, err = s.DesignCreateDocument(ctx, p.ID, owner.ID, uuid.NewString(), "bad", current.Model)
	codeIs(t, err, "invalid_ref")
}
