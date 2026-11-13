package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/design"
	"github.com/PavluninVladimir/rivet/internal/store"
	"github.com/google/uuid"
)

// A renderer receives only validated data and the referenced, verified private
// image bytes. It must not load arbitrary URLs or inherit a browser session.
// Nil is explicitly unavailable; a fake/blank screenshot is never substituted.
type DesignScreenshotAsset struct {
	MIME string
	Data []byte
}
type DesignScreenshotRenderer func(context.Context, design.Model, map[string]DesignScreenshotAsset) ([]byte, error)

func (s *Server) registerDesignWorkspace(mux *http.ServeMux) {
	const p = "/api/v1/projects/{id}/design"
	routes := map[string]string{
		"GET " + p + "/documents": "list", "POST " + p + "/documents": "create", "GET " + p + "/documents/{doc}": "get", "PATCH " + p + "/documents/{doc}": "patch",
		"POST " + p + "/documents/{doc}/preview-diff": "preview", "GET " + p + "/documents/{doc}/revisions": "revisions", "POST " + p + "/documents/{doc}/undo": "undo",
		"POST " + p + "/imports/validate": "validate", "POST " + p + "/documents/{doc}/import": "import", "POST " + p + "/assets": "asset_upload", "GET " + p + "/assets/{asset}": "asset_url",
		"GET " + p + "/documents/{doc}/export": "export", "GET " + p + "/documents/{doc}/threads": "threads", "POST " + p + "/documents/{doc}/threads": "thread_create",
		"POST " + p + "/threads/{thread}/messages": "reply", "PATCH " + p + "/threads/{thread}": "thread_status", "PATCH " + p + "/comments/{comment}": "comment_edit", "DELETE " + p + "/comments/{comment}": "comment_delete",
		"POST " + p + "/documents/{doc}/handoffs": "handoff_create", "GET " + p + "/handoffs/{handoff}": "handoff_get", "POST " + p + "/handoffs/{handoff}/links": "handoff_link",
	}
	for route, op := range routes {
		mux.HandleFunc(route, s.designRoute(op))
	}
	mux.HandleFunc("GET /api/v1/design-assets/{token}", s.designAssetContent)
}
func designError(w http.ResponseWriter, err error) {
	var d *design.Error
	if errors.As(err, &d) {
		status := 422
		switch d.Code {
		case "forbidden":
			status = 403
		case "revision_conflict", "idempotency_conflict", "undo_conflict":
			status = 409
		case "size_limit":
			status = 413
		case "rate_limit":
			status = 429
		case "screenshot_unavailable", "storage_unavailable", "scm_unavailable":
			status = 503
		}
		out := map[string]any{"error": map[string]any{"code": d.Code, "message": d.Message, "paths": []string{d.Path}, "current_revision": d.CurrentRevision}, "code": d.Code, "message": d.Message}
		if d.CurrentRevision > 0 {
			out["current_revision"] = d.CurrentRevision
		}
		writeJSON(w, status, out)
		return
	}
	writeErr(w, err)
}
func designDecode(w http.ResponseWriter, r *http.Request, v any, limit int64) bool {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		designError(w, design.Invalid("size_limit", "$", "слишком большой запрос"))
		return false
	}
	if err = design.Strict(raw, v); err != nil {
		designError(w, err)
		return false
	}
	return true
}
func designRevision(w http.ResponseWriter, r *http.Request, required bool) (int, bool) {
	raw := r.URL.Query().Get("revision")
	if raw == "" && !required {
		return 0, true
	}
	n, e := strconv.Atoi(raw)
	if e != nil || n < 1 {
		designError(w, design.Invalid("invalid_ref", "revision", "нужна revision>=1"))
		return 0, false
	}
	return n, true
}
func cursorValue(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (s *Server) designRoute(op string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pid, actor, doc, key := r.PathValue("id"), currentUser(r).ID, r.PathValue("doc"), r.Header.Get("Idempotency-Key")
		ids := []string{pid}
		for _, k := range []string{"doc", "asset", "thread", "comment", "handoff"} {
			if v := r.PathValue(k); v != "" {
				ids = append(ids, v)
			}
		}
		if !pathIDs(w, ids...) || !s.requireMember(w, r, pid) {
			return
		}
		if r.Method != "GET" && op != "preview" && op != "validate" && (key == "" || len(key) > 200) {
			designError(w, design.Invalid("idempotency_required", "Idempotency-Key", "нужен ключ1–200символов"))
			return
		}
		status := 200
		var result any
		var err error
		switch op {
		case "list", "threads", "revisions":
			limit, ok := listLimit(r, 20)
			if !ok {
				designError(w, design.Invalid("invalid_ref", "limit", "limit1–100"))
				return
			}
			before := r.URL.Query().Get("before")
			if op == "revisions" {
				b := 0
				if before != "" {
					b, err = strconv.Atoi(before)
					if err != nil || b < 1 {
						designError(w, design.Invalid("invalid_ref", "before", "нужна revision"))
						return
					}
				}
				items, next, e := s.St.DesignRevisions(r.Context(), pid, actor, doc, b, limit)
				err = e
				var cursor any
				if next > 0 {
					cursor = strconv.Itoa(next)
				}
				result = map[string]any{"items": items, "next_cursor": cursor}
			} else {
				if before != "" && !design.ValidID(before) {
					designError(w, design.Invalid("invalid_ref", "before", "нужен UUID курсор"))
					return
				}
				if op == "list" {
					items, next, e := s.St.DesignListDocuments(r.Context(), pid, actor, before, limit)
					err = e
					result = map[string]any{"items": items, "next_cursor": cursorValue(next)}
				} else {
					items, next, e := s.St.DesignListThreads(r.Context(), pid, actor, doc, before, limit)
					err = e
					result = map[string]any{"items": items, "next_cursor": cursorValue(next)}
				}
			}
		case "create":
			var in struct {
				Name  string          `json:"name"`
				Model json.RawMessage `json:"model"`
			}
			if !designDecode(w, r, &in, design.MaxDocumentBytes+1024) {
				return
			}
			var model design.Model
			model, err = design.Parse(in.Model)
			if err == nil {
				result, err = s.St.DesignCreateDocument(r.Context(), pid, actor, key, in.Name, model)
				status = 201
			}
		case "get":
			revision, ok := designRevision(w, r, false)
			if !ok {
				return
			}
			result, err = s.St.DesignGetDocument(r.Context(), pid, actor, doc, revision)
		case "patch", "preview":
			var in store.DesignPatchRequest
			if !designDecode(w, r, &in, design.MaxPatchBytes) {
				return
			}
			result, err = s.St.DesignPatchDocument(r.Context(), pid, actor, doc, key, in, op == "preview")
		case "undo":
			var in struct {
				ExpectedRevision int `json:"expected_revision"`
				UndoRevision     int `json:"undo_revision"`
			}
			if !designDecode(w, r, &in, 1024) {
				return
			}
			result, err = s.St.DesignUndo(r.Context(), pid, actor, doc, key, in.ExpectedRevision, in.UndoRevision)
		case "validate", "import":
			var in struct {
				DocumentID       *string             `json:"document_id,omitempty"`
				ExpectedRevision int                 `json:"expected_revision,omitempty"`
				Model            json.RawMessage     `json:"model"`
				Assets           []store.DesignAsset `json:"assets"`
			}
			if !designDecode(w, r, &in, design.MaxDocumentBytes+1<<16) {
				return
			}
			var model design.Model
			model, err = design.Parse(in.Model)
			if op == "validate" {
				if in.DocumentID != nil {
					doc = *in.DocumentID
					if !design.ValidID(doc) {
						designError(w, design.Invalid("invalid_ref", "document_id", "нужен UUID"))
						return
					}
				}
				var diff design.Diff
				if err == nil {
					diff, err = s.St.DesignValidateImport(r.Context(), pid, actor, doc, model, in.Assets)
				}
				diagnostics := []design.Diagnostic{}
				if err != nil {
					var d *design.Error
					if errors.As(err, &d) {
						diagnostics = append(diagnostics, design.Diagnostic{Code: d.Code, Path: d.Path, Message: d.Message})
						result = map[string]any{"valid": false, "diagnostics": diagnostics, "impact": nil}
						err = nil
					}
				} else {
					result = map[string]any{"valid": true, "diagnostics": diagnostics, "impact": diff}
				}
			} else if err == nil {
				result, err = s.St.DesignImport(r.Context(), pid, actor, doc, key, in.ExpectedRevision, model, in.Assets)
			}
		case "export":
			revision, ok := designRevision(w, r, true)
			if !ok {
				return
			}
			result, err = s.St.DesignExport(r.Context(), pid, actor, doc, revision)

		case "asset_upload":
			result, err = s.designUpload(w, r, pid, actor, key)
			status = 201
		case "asset_url":
			result, err = s.St.DesignAssetReadURL(r.Context(), pid, actor, r.PathValue("asset"))
		case "thread_create":
			var in struct {
				Anchor store.DesignAnchor `json:"anchor"`
				Text   string             `json:"text"`
			}
			if !designDecode(w, r, &in, 64<<10) {
				return
			}
			result, err = s.St.DesignCreateThread(r.Context(), pid, actor, doc, key, in.Anchor, in.Text)
			status = 201
		case "reply":
			var in struct {
				Text string `json:"text"`
			}
			if !designDecode(w, r, &in, 64<<10) {
				return
			}
			result, err = s.St.DesignReply(r.Context(), pid, actor, r.PathValue("thread"), key, in.Text)
			status = 201
		case "thread_status":
			var in struct {
				ExpectedRevision int    `json:"expected_revision"`
				Status           string `json:"status"`
			}
			if !designDecode(w, r, &in, 1024) {
				return
			}
			result, err = s.St.DesignSetThread(r.Context(), pid, actor, r.PathValue("thread"), key, in.ExpectedRevision, in.Status)
		case "comment_edit", "comment_delete":
			var in struct {
				ExpectedRevision int    `json:"expected_revision"`
				Text             string `json:"text,omitempty"`
			}
			if !designDecode(w, r, &in, 64<<10) {
				return
			}
			result, err = s.St.DesignEditComment(r.Context(), pid, actor, r.PathValue("comment"), key, in.ExpectedRevision, in.Text, op == "comment_delete")
		case "handoff_create":
			var in struct {
				Revision  int                      `json:"revision"`
				ThreadIDs []string                 `json:"thread_ids"`
				Links     []store.DesignContextRef `json:"links"`
			}
			if !designDecode(w, r, &in, 64<<10) {
				return
			}
			replay, found, replayErr := s.St.DesignHandoffReplay(r.Context(), pid, actor, doc, key, in.Revision, in.ThreadIDs, in.Links)
			if replayErr != nil {
				designError(w, replayErr)
				return
			}
			if found {
				writeJSON(w, 201, replay)
				return
			}
			var screenshot store.DesignAsset
			screenshot, err = s.designScreenshot(r.Context(), pid, actor, doc, in.Revision)
			if err == nil {
				result, err = s.St.DesignCreateHandoff(r.Context(), pid, actor, doc, key, in.Revision, in.ThreadIDs, in.Links, screenshot)
				status = 201
			}
		case "handoff_get":
			result, err = s.St.DesignGetHandoff(r.Context(), pid, actor, r.PathValue("handoff"))
			if err == nil {
				h := result.(store.DesignHandoff)
				for i := range h.AssetRefs {
					h.AssetRefs[i], err = s.St.DesignAssetReadURL(r.Context(), pid, actor, h.AssetRefs[i].ID)
					if err != nil {
						break
					}
				}
				result = h
			}
		case "handoff_link":
			if !s.requireOwner(w, r, pid) {
				return
			}
			var ref store.DesignContextRef
			if !designDecode(w, r, &ref, 2048) {
				return
			}
			verified := false
			if ref.Kind == "git_commit" {
				err = s.designVerifyCommit(r.Context(), pid, actor, r.PathValue("handoff"), ref.ID)
				verified = err == nil
			}
			if err == nil {
				result, err = s.St.DesignLinkHandoff(r.Context(), pid, actor, r.PathValue("handoff"), key, ref, verified)
			}
		}
		if err != nil {
			designError(w, err)
			return
		}
		writeJSON(w, status, result)
	}
}
func imageSafe(raw []byte, required string) (string, error) {
	if len(raw) == 0 || len(raw) > design.MaxAssetBytes {
		return "", design.Invalid("size_limit", "file", "asset до10МиБ")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 4096 || cfg.Height > 4096 {
		return "", design.Invalid("unsupported_property", "file", "нужно PNG/JPEG до4096×4096")
	}
	mime := "image/" + format
	if mime != "image/png" && mime != "image/jpeg" || required != "" && mime != required {
		return "", design.Invalid("unsupported_property", "file", "поддерживаются PNG/JPEG, без HTML/SVG")
	}
	return mime, nil
}
func (s *Server) designUpload(w http.ResponseWriter, r *http.Request, pid, actor, key string) (any, error) {
	if s.Blob == nil {
		return nil, design.Invalid("storage_unavailable", "assets", "object storage недоступно")
	}
	r.Body = http.MaxBytesReader(w, r.Body, design.MaxAssetBytes+1<<16)
	if err := r.ParseMultipartForm(design.MaxAssetBytes + 1<<16); err != nil {
		return nil, design.Invalid("size_limit", "file", "не удалось прочитать multipart до10МиБ")
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	if len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 || len(r.MultipartForm.Value) > 0 {
		return nil, design.Invalid("unsupported_property", "file", "ожидается только один file")
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		return nil, design.Invalid("unsupported_property", "file", "нужен file")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, design.MaxAssetBytes+1))
	if err != nil {
		return nil, err
	}
	mime, err := imageSafe(raw, "")
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(pid+"/"+actor+"/"+key)).String()
	ref, err := s.Blob.Put(r.Context(), "design/"+pid+"/"+id+"/"+hex.EncodeToString(sum[:]), raw)
	if err != nil {
		return nil, design.Invalid("storage_unavailable", "assets", "не удалось сохранить asset")
	}
	a := store.DesignAsset{ID: id, SHA256: hex.EncodeToString(sum[:]), MIME: mime, SizeBytes: int64(len(raw)), ObjectRef: ref}
	result, err := s.St.DesignRegisterAsset(r.Context(), pid, actor, key, a)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(result, &a); err != nil {
		return nil, err
	}
	return s.St.DesignAssetReadURL(r.Context(), pid, actor, a.ID)
}
func (s *Server) designAssetContent(w http.ResponseWriter, r *http.Request) {
	a, err := s.St.DesignResolveAssetRead(r.Context(), r.PathValue("token"))
	if err != nil {
		designError(w, err)
		return
	}
	if s.Blob == nil {
		designError(w, design.Invalid("storage_unavailable", "assets", "object storage недоступно"))
		return
	}
	raw, err := s.Blob.Get(r.Context(), a.ObjectRef)
	if err != nil {
		writeErr(w, store.ErrNotFound)
		return
	}
	sum := sha256.Sum256(raw)
	if int64(len(raw)) != a.SizeBytes || hex.EncodeToString(sum[:]) != a.SHA256 {
		writeErr(w, store.ErrNotFound)
		return
	}
	w.Header().Set("Content-Type", a.MIME)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cross-Origin-Resource-Policy", "cross-origin")
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}
func (s *Server) designScreenshot(ctx context.Context, pid, actor, id string, revision int) (store.DesignAsset, error) {
	var asset store.DesignAsset
	if s.DesignScreenshot == nil {
		return asset, design.Invalid("screenshot_unavailable", "screenshot", "trusted screenshot renderer не настроен")
	}
	if s.Blob == nil {
		return asset, design.Invalid("storage_unavailable", "screenshot", "object storage недоступно")
	}
	if revision < 1 {
		return asset, design.Invalid("invalid_ref", "revision", "нужна конкретная revision")
	}
	doc, err := s.St.DesignGetDocument(ctx, pid, actor, id, revision)
	if err != nil {
		return asset, err
	}
	assets := map[string]DesignScreenshotAsset{}
	total := 0
	for _, aid := range design.AssetIDs(doc.Model) {
		a, e := s.St.DesignGetAsset(ctx, pid, actor, aid)
		if e != nil {
			return asset, e
		}
		raw, e := s.Blob.Get(ctx, a.ObjectRef)
		if e != nil {
			return asset, e
		}
		total += len(raw)
		sum := sha256.Sum256(raw)
		if total > 32<<20 || int64(len(raw)) != a.SizeBytes || hex.EncodeToString(sum[:]) != a.SHA256 {
			return asset, design.Invalid("size_limit", "assets", "невозможен безопасный снимок assets")
		}
		assets[aid] = DesignScreenshotAsset{MIME: a.MIME, Data: raw}
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := s.DesignScreenshot(rctx, doc.Model, assets)
	if err != nil {
		return asset, design.Invalid("screenshot_unavailable", "screenshot", "renderer не выполнил безопасный снимок")
	}
	if _, err = imageSafe(raw, "image/png"); err != nil {
		return asset, err
	}
	sum := sha256.Sum256(raw)
	asset = store.DesignAsset{ID: uuid.NewString(), SHA256: hex.EncodeToString(sum[:]), MIME: "image/png", SizeBytes: int64(len(raw))}
	asset.ObjectRef, err = s.Blob.Put(ctx, "design/"+pid+"/"+asset.ID+"/"+asset.SHA256, raw)
	return asset, err
}
func (s *Server) designVerifyCommit(ctx context.Context, pid, actor, handoffID, sha string) error {
	if len(sha) != 40 && len(sha) != 64 {
		return design.Invalid("invalid_ref", "id", "нужен полный commit SHA")
	}
	if _, err := hex.DecodeString(sha); err != nil || sha != strings.ToLower(sha) {
		return design.Invalid("invalid_ref", "id", "неверный commit SHA")
	}
	h, err := s.St.DesignGetHandoff(ctx, pid, actor, handoffID)
	if err != nil {
		return err
	}
	if s.Engine == nil {
		return design.Invalid("scm_unavailable", "id", "SCM недоступен")
	}
	p, err := s.St.GetProject(ctx, pid)
	if err != nil {
		return err
	}
	adapter, err := s.Engine.SCMFor(ctx, p)
	if err != nil {
		return design.Invalid("scm_unavailable", "id", "SCM недоступен")
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	file, err := adapter.ReadFile(rctx, p.Repo(), sha, "design/rivet/"+h.DocumentID+"/manifest.json")
	if err != nil {
		return design.Invalid("invalid_ref", "id", "commit с manifest недоступен")
	}
	return verifyDesignManifest([]byte(file.Content), h)
}

func verifyDesignManifest(raw []byte, h store.DesignHandoff) error {
	var snapshot store.DesignHandoff
	if design.Strict(raw, &snapshot) != nil {
		return design.Invalid("invalid_ref", "id", "commit manifest не содержит полный handoff")
	}
	if err := design.Validate(snapshot.Model); err != nil {
		return design.Invalid("invalid_ref", "id", "commit manifest не содержит валидную model")
	}
	_, modelSHA, err := design.Canonical(snapshot.Model)
	if err != nil || modelSHA != h.ModelSHA || snapshot.ModelSHA != h.ModelSHA || snapshot.Revision != h.Revision || snapshot.ManifestSHA != h.ManifestSHA {
		return design.Invalid("invalid_ref", "id", "commit model/revision/SHA не соответствует handoff")
	}
	actualSHA, err := store.DesignManifestSHA(snapshot)
	if err != nil || actualSHA != h.ManifestSHA {
		return design.Invalid("invalid_ref", "id", "содержимое commit manifest не соответствует immutable handoff SHA")
	}
	return nil
}
