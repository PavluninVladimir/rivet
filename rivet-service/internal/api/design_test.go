package api

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/PavluninVladimir/rivet/internal/blob"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/PavluninVladimir/rivet/internal/design"
	"github.com/PavluninVladimir/rivet/internal/store"
	"github.com/google/uuid"
)

func designAPICall(t *testing.T, srv *httptest.Server, method, path, session, key string, body any) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(method, srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if session != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}
func TestDesignAPIContractIsolationAndAtomicImport(t *testing.T) {
	st, srv := testServer(t)
	ctx := context.Background()
	owner, err := st.CreateUser(ctx, "design-owner", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "design", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	session := loginSession(t, srv, owner.Login, "pw-testpass")
	node := uuid.NewString()
	m := design.Model{Schema: design.Schema, Tokens: []design.Token{}, Components: []design.Component{}, Screens: []design.Screen{{ID: uuid.NewString(), Name: "Main", Viewport: design.Viewport{Width: 320, Height: 240}, Root: design.Node{ID: uuid.NewString(), Type: "container", Props: design.Properties{}, Children: []design.Node{{ID: node, Type: "button", Props: design.Properties{"text": json.RawMessage(`"Save"`)}, Children: []design.Node{}}}}}}, Overrides: []design.Override{}}
	prefix := "/api/v1/projects/" + p.ID + "/design"
	input := map[string]any{"name": "Main", "model": m}
	status, raw := designAPICall(t, srv, "POST", prefix+"/documents", session, "", input)
	if status != 422 {
		t.Fatal(status, string(raw))
	}
	key := uuid.NewString()
	status, raw = designAPICall(t, srv, "POST", prefix+"/documents", session, key, input)
	if status != 201 {
		t.Fatal(status, string(raw))
	}
	var d store.DesignDocument
	if err = json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	status, replayed := designAPICall(t, srv, "POST", prefix+"/documents", session, key, input)
	if status != 201 || !bytes.Equal(raw, replayed) {
		t.Fatal("replay", status, string(replayed))
	}
	patch := map[string]any{"expected_revision": 1, "operations": []any{map[string]any{"op": "set_property", "scope": "instance", "element_id": node, "property": "padding", "value": 24}}}
	status, raw = designAPICall(t, srv, "POST", prefix+"/documents/"+d.ID+"/preview-diff", session, "", patch)
	if status != 200 || !strings.Contains(string(raw), node) {
		t.Fatal(status, string(raw))
	}
	status, raw = designAPICall(t, srv, "PATCH", prefix+"/documents/"+d.ID, session, uuid.NewString(), patch)
	if status != 200 {
		t.Fatal(status, string(raw))
	}
	status, raw = designAPICall(t, srv, "PATCH", prefix+"/documents/"+d.ID, session, uuid.NewString(), patch)
	if status != 409 || !strings.Contains(string(raw), "revision_conflict") {
		t.Fatal(status, string(raw))
	}
	bad := map[string]any{"schema": "rivet.design/v1", "script": "alert(1)"}
	status, raw = designAPICall(t, srv, "POST", prefix+"/imports/validate", session, "", map[string]any{"document_id": d.ID, "model": bad, "assets": []any{}})
	if status != 200 || !strings.Contains(string(raw), `"valid":false`) {
		t.Fatal(status, string(raw))
	}
	status, raw = designAPICall(t, srv, "POST", prefix+"/documents/"+d.ID+"/import", session, uuid.NewString(), map[string]any{"expected_revision": 2, "model": bad, "assets": []any{}})
	if status != 422 {
		t.Fatal(status, string(raw))
	}
	status, raw = designAPICall(t, srv, "GET", prefix+"/documents/"+d.ID, session, "", nil)
	if status != 200 || !strings.Contains(string(raw), `"revision":2`) {
		t.Fatal(status, string(raw))
	}
	stranger, err := st.CreateUser(ctx, "design-outsider", "", "pw-testpass", true)
	if err != nil {
		t.Fatal(err)
	}
	other := loginSession(t, srv, stranger.Login, "pw-testpass")
	status, raw = designAPICall(t, srv, "GET", prefix+"/documents/"+d.ID, other, "", nil)
	if status != 404 {
		t.Fatal("admin must not bypass project ACL", status, string(raw))
	}
	status, raw = designAPICall(t, srv, "POST", prefix+"/documents/"+d.ID+"/handoffs", session, uuid.NewString(), map[string]any{"revision": 2, "thread_ids": []any{}, "links": []any{}})
	if status != 503 || !strings.Contains(string(raw), "screenshot_unavailable") {
		t.Fatal("no fake screenshot", status, string(raw))
	}
	// Capability endpoint is publicly reachable but cannot authenticate an invented key.
	status, raw = designAPICall(t, srv, "GET", "/api/v1/design-assets/"+strings.Repeat("a", 64), "", "", nil)
	if status != 404 {
		t.Fatal(status, string(raw))
	}
}
func TestDesignImageValidationRejectsExecutableAndOversized(t *testing.T) {
	for _, body := range [][]byte{[]byte(`<svg><script>alert(1)</script></svg>`), []byte(`<html>bad</html>`), []byte("https://internal/metadata"), make([]byte, design.MaxAssetBytes+1)} {
		if _, err := imageSafe(body, ""); err == nil {
			t.Fatal("unsafe asset accepted")
		}
	}
}

func TestDesignRealHandoffPrivatePNG(t *testing.T) {
	if os.Getenv("RIVET_DESIGN_INTEGRATION") != "1" {
		t.Skip("explicit real MinIO/Chromium fixture")
	}
	st, _ := testServer(t)
	ctx := context.Background()
	endpoint := os.Getenv("RIVET_S3_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	access, secret := os.Getenv("RIVET_S3_ACCESS_KEY"), os.Getenv("RIVET_S3_SECRET_KEY")
	if access == "" {
		access = "rivet"
	}
	if secret == "" {
		secret = "rivetsecret"
	}
	bucket := "rivet-design-test-" + uuid.NewString()
	objects, err := blob.New(endpoint, access, secret, bucket, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = objects.EnsureBucket(ctx); err != nil {
		t.Fatal("isolated fixture bucket unavailable", err)
	}
	client, err := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(access, secret, ""), Secure: false})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for obj := range client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			if obj.Err == nil {
				_ = client.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{})
			}
		}
		_ = client.RemoveBucket(ctx, bucket)
	})
	renderer := NewDesignScreenshotRenderer(os.Getenv("RIVET_DESIGN_CHROMIUM_TEST"))
	srv := httptest.NewServer((&Server{St: st, Blob: objects, DesignScreenshot: renderer}).Handler())
	defer srv.Close()
	owner, err := st.CreateUser(ctx, "design-real-owner", "", "pw-testpass", false)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreateProject(ctx, "design-real", "owner/repo", nil, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	session := loginSession(t, srv, owner.Login, "pw-testpass")
	rootID := uuid.NewString()
	model := design.Model{Schema: design.Schema, Tokens: []design.Token{}, Components: []design.Component{}, Screens: []design.Screen{{ID: uuid.NewString(), Name: "Main", Viewport: design.Viewport{Width: 320, Height: 240}, Root: design.Node{ID: rootID, Type: "button", Props: design.Properties{"text": json.RawMessage(`"Реальный снимок"`), "padding": json.RawMessage(`24`), "background": json.RawMessage(`"#3498db"`)}, Children: []design.Node{}}}}, Overrides: []design.Override{}}
	prefix := "/api/v1/projects/" + p.ID + "/design"
	status, raw := designAPICall(t, srv, "POST", prefix+"/documents", session, uuid.NewString(), map[string]any{"name": "Real", "model": model})
	if status != 201 {
		t.Fatal(status, string(raw))
	}
	var d store.DesignDocument
	_ = json.Unmarshal(raw, &d)
	request := map[string]any{"revision": 1, "thread_ids": []any{}, "links": []any{}}
	key := uuid.NewString()
	status, raw = designAPICall(t, srv, "POST", prefix+"/documents/"+d.ID+"/handoffs", session, key, request)
	if status != 201 {
		t.Fatal(status, string(raw))
	}
	var handoff store.DesignHandoff
	if err = json.Unmarshal(raw, &handoff); err != nil {
		t.Fatal(err)
	}
	status, replay := designAPICall(t, srv, "POST", prefix+"/documents/"+d.ID+"/handoffs", session, key, request)
	if status != 201 || !bytes.Equal(raw, replay) {
		t.Fatal("screenshot handoff replay", status, string(replay))
	}
	status, raw = designAPICall(t, srv, "GET", prefix+"/handoffs/"+handoff.ID, session, "", nil)
	if status != 200 {
		t.Fatal(status, string(raw))
	}
	var read store.DesignHandoff
	_ = json.Unmarshal(raw, &read)
	if len(read.AssetRefs) != 1 || read.AssetRefs[0].URL == "" {
		t.Fatal("missing private screenshot URL")
	}
	resp, err := srv.Client().Get(srv.URL + read.AssetRefs[0].URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	cfg, err := png.DecodeConfig(resp.Body)
	if err != nil || resp.StatusCode != 200 || cfg.Width != 320 || cfg.Height != 240 {
		t.Fatal("private PNG", resp.StatusCode, cfg, err)
	}
	if resp.Header.Get("Access-Control-Allow-Credentials") != "" || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("capability CORS uses credentials")
	}
	status, raw = designAPICall(t, srv, "PATCH", prefix+"/documents/"+d.ID, session, uuid.NewString(), store.DesignPatchRequest{ExpectedRevision: 1, Operations: []design.Patch{{Op: "set_property", ElementID: rootID, Property: "padding", Value: json.RawMessage(`40`), Scope: "instance"}}})
	if status != 200 {
		t.Fatal(status, string(raw))
	}
	status, raw = designAPICall(t, srv, "GET", prefix+"/handoffs/"+handoff.ID, session, "", nil)
	if status != 200 {
		t.Fatal(status, string(raw))
	}
	_ = json.Unmarshal(raw, &read)
	if read.ManifestSHA != handoff.ManifestSHA || read.ModelSHA != handoff.ModelSHA || read.Revision != 1 {
		t.Fatal("immutable handoff changed after edit")
	}
}

func TestDesignGitManifestRejectsSelfDeclaredSHA(t *testing.T) {
	model := design.Model{Schema: design.Schema, Tokens: []design.Token{}, Components: []design.Component{}, Screens: []design.Screen{}, Overrides: []design.Override{}}
	_, modelSHA, err := design.Canonical(model)
	if err != nil {
		t.Fatal(err)
	}
	h := store.DesignHandoff{ID: uuid.NewString(), DocumentID: uuid.NewString(), Revision: 1, Model: model, ModelSHA: modelSHA, ScreenshotAssetID: uuid.NewString(), AssetRefs: []store.DesignAsset{}, ThreadsSnapshot: []store.DesignThread{}, ContextRefs: []store.DesignContextRef{}}
	h.ManifestSHA, err = store.DesignManifestSHA(h)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(h)
	if err = verifyDesignManifest(raw, h); err != nil {
		t.Fatal(err)
	}
	forged, _ := json.Marshal(map[string]any{"manifest_sha": h.ManifestSHA})
	if err = verifyDesignManifest(forged, h); err == nil {
		t.Fatal("accepted self-declared SHA without immutable model")
	}
	tampered := h
	tampered.ScreenshotAssetID = uuid.NewString()
	raw, _ = json.Marshal(tampered)
	if err = verifyDesignManifest(raw, h); err == nil {
		t.Fatal("accepted tampered manifest with original declared SHA")
	}
	tampered = h
	tampered.Model.Tokens = []design.Token{{ID: uuid.NewString(), Name: "bad", Type: "spacing", Value: json.RawMessage(`{"px":99}`)}}
	raw, _ = json.Marshal(tampered)
	if err = verifyDesignManifest(raw, h); err == nil {
		t.Fatal("accepted tampered model with original declared SHA")
	}
}
