package store

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/design"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type DesignAnchor struct {
	DocumentID string `json:"document_id"`
	Revision   int    `json:"revision"`
	ElementID  string `json:"element_id"`
	Property   string `json:"property,omitempty"`
}
type DesignComment struct {
	ID        string     `json:"id"`
	Revision  int        `json:"revision"`
	AuthorID  string     `json:"author_id"`
	Text      string     `json:"text"`
	CreatedAt time.Time  `json:"created_at"`
	EditedAt  *time.Time `json:"edited_at"`
	Deleted   bool       `json:"deleted"`
}
type DesignThread struct {
	ID        string          `json:"id"`
	Revision  int             `json:"revision"`
	Anchor    DesignAnchor    `json:"anchor"`
	Status    string          `json:"status"`
	Orphaned  bool            `json:"orphaned"`
	CreatedBy string          `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
	Messages  []DesignComment `json:"messages"`
}

func designText(text string) error {
	if strings.TrimSpace(text) == "" || len([]rune(text)) > 8000 {
		return design.Invalid("unsupported_property", "text", "нужен текст1–8000символов")
	}
	return nil
}
func designThreadTx(ctx context.Context, q dbtx, pid, id string) (DesignThread, error) {
	var t DesignThread
	err := q.QueryRow(ctx, `SELECT id::text,revision,document_id::text,anchor_revision,element_id::text,property,status,created_by::text,created_at FROM design_threads WHERE project_id=$1 AND id=$2`, pid, id).Scan(&t.ID, &t.Revision, &t.Anchor.DocumentID, &t.Anchor.Revision, &t.Anchor.ElementID, &t.Anchor.Property, &t.Status, &t.CreatedBy, &t.CreatedAt)
	if err != nil {
		return t, nf(err)
	}
	current, err := designDocTx(ctx, q, pid, t.Anchor.DocumentID, 0)
	if err != nil {
		return t, err
	}
	t.Orphaned = !design.NodeIDs(current.Model)[t.Anchor.ElementID]
	rows, err := q.Query(ctx, `SELECT id::text,revision,author_id::text,text,created_at,edited_at,deleted FROM design_comments WHERE thread_id=$1 ORDER BY created_at,id`, id)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	t.Messages = []DesignComment{}
	for rows.Next() {
		var c DesignComment
		if err = rows.Scan(&c.ID, &c.Revision, &c.AuthorID, &c.Text, &c.CreatedAt, &c.EditedAt, &c.Deleted); err != nil {
			return t, err
		}
		t.Messages = append(t.Messages, c)
	}
	return t, rows.Err()
}
func designCommentHistory(ctx context.Context, tx pgx.Tx, actor string, c DesignComment) error {
	_, err := tx.Exec(ctx, `INSERT INTO design_comment_history(comment_id,revision,text,deleted,actor_id) VALUES($1,$2,$3,$4,$5)`, c.ID, c.Revision, c.Text, c.Deleted, actor)
	return err
}
func designAddCommentTx(ctx context.Context, tx pgx.Tx, threadID, actor, text string) (DesignComment, error) {
	var c DesignComment
	c.ID = uuid.NewString()
	err := tx.QueryRow(ctx, `INSERT INTO design_comments(id,thread_id,author_id,text) VALUES($1,$2,$3,$4) RETURNING revision,author_id::text,text,created_at,edited_at,deleted`, c.ID, threadID, actor, text).Scan(&c.Revision, &c.AuthorID, &c.Text, &c.CreatedAt, &c.EditedAt, &c.Deleted)
	if err != nil {
		return c, err
	}
	return c, designCommentHistory(ctx, tx, actor, c)
}
func (s *Store) DesignListThreads(ctx context.Context, pid, actor, id, before string, limit int) ([]DesignThread, string, error) {
	if _, err := s.DesignGetDocument(ctx, pid, actor, id, 0); err != nil {
		return nil, "", err
	}
	rows, err := s.Pool.Query(ctx, `SELECT id::text FROM design_threads WHERE project_id=$1 AND document_id=$2 AND ($3='' OR id<NULLIF($3,'')::uuid) ORDER BY id DESC LIMIT $4`, pid, id, before, limit+1)
	if err != nil {
		return nil, "", err
	}
	ids := []string{}
	for rows.Next() {
		var tid string
		if err = rows.Scan(&tid); err != nil {
			rows.Close()
			return nil, "", err
		}
		ids = append(ids, tid)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(ids) > limit {
		ids = ids[:limit]
		next = ids[len(ids)-1]
	}
	out := []DesignThread{}
	for _, tid := range ids {
		t, e := designThreadTx(ctx, s.Pool, pid, tid)
		if e != nil {
			return nil, "", e
		}
		out = append(out, t)
	}
	return out, next, nil
}
func (s *Store) DesignCreateThread(ctx context.Context, pid, actor, id, key string, anchor DesignAnchor, text string) (json.RawMessage, error) {
	if err := designText(text); err != nil {
		return nil, err
	}
	return s.designDo(ctx, pid, actor, key, "thread.create:"+id, map[string]any{"anchor": anchor, "text": text}, false, func(tx pgx.Tx, role string) (any, string, string, int, error) {
		if anchor.DocumentID != id || !design.ValidID(anchor.ElementID) || anchor.Revision < 1 {
			return nil, "", "", 0, design.Invalid("invalid_anchor", "anchor", "неверная привязка")
		}
		at, err := designDocTx(ctx, tx, pid, id, anchor.Revision)
		if err != nil {
			return nil, "", "", 0, err
		}
		if !design.NodeIDs(at.Model)[anchor.ElementID] {
			return nil, "", "", 0, design.Invalid("invalid_anchor", "anchor.element_id", "элемент отсутствует в исходной revision")
		}
		if len(anchor.Property) > 50 {
			return nil, "", "", 0, design.Invalid("invalid_anchor", "anchor.property", "слишком длинное свойство")
		}
		tid := uuid.NewString()
		if _, err = tx.Exec(ctx, `INSERT INTO design_threads(id,project_id,document_id,anchor_revision,element_id,property,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)`, tid, pid, id, anchor.Revision, anchor.ElementID, anchor.Property, actor); err != nil {
			return nil, "", "", 0, err
		}
		if _, err = designAddCommentTx(ctx, tx, tid, actor, text); err != nil {
			return nil, "", "", 0, err
		}
		t, err := designThreadTx(ctx, tx, pid, tid)
		return t, "design.thread.updated", tid, t.Revision, err
	})
}
func (s *Store) DesignReply(ctx context.Context, pid, actor, tid, key, text string) (json.RawMessage, error) {
	if err := designText(text); err != nil {
		return nil, err
	}
	return s.designDo(ctx, pid, actor, key, "thread.reply:"+tid, map[string]string{"text": text}, false, func(tx pgx.Tx, role string) (any, string, string, int, error) {
		var n int
		err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM design_comments c JOIN design_threads t ON t.id=c.thread_id WHERE t.project_id=$1 AND t.id=$2`, pid, tid).Scan(&n)
		if err != nil {
			return nil, "", "", 0, err
		}
		if n >= 200 {
			return nil, "", "", 0, design.Invalid("size_limit", "messages", "не более200сообщений в ветке")
		}
		if n == 0 {
			return nil, "", "", 0, ErrNotFound
		}
		c, err := designAddCommentTx(ctx, tx, tid, actor, text)
		threadRevision := 0
		if err == nil {
			err = tx.QueryRow(ctx, `UPDATE design_threads SET revision=revision+1 WHERE id=$1 RETURNING revision`, tid).Scan(&threadRevision)
		}
		return c, "design.thread.updated", tid, threadRevision, err
	})
}
func (s *Store) DesignSetThread(ctx context.Context, pid, actor, tid, key string, expected int, status string) (json.RawMessage, error) {
	if status != "open" && status != "resolved" {
		return nil, design.Invalid("unsupported_property", "status", "ожидается open или resolved")
	}
	return s.designDo(ctx, pid, actor, key, "thread.status:"+tid, map[string]any{"expected_revision": expected, "status": status}, false, func(tx pgx.Tx, role string) (any, string, string, int, error) {
		t, err := designThreadTx(ctx, tx, pid, tid)
		if err != nil {
			return nil, "", "", 0, err
		}
		if t.CreatedBy != actor && role != domain.RoleOwner {
			return nil, "", "", 0, design.Invalid("forbidden", "thread", "нужен автор или owner")
		}
		if t.Revision != expected {
			return nil, "", "", 0, designConflict("revision_conflict", t.Revision)
		}
		if _, err = tx.Exec(ctx, `UPDATE design_threads SET revision=revision+1,status=$2 WHERE id=$1`, tid, status); err != nil {
			return nil, "", "", 0, err
		}
		t, err = designThreadTx(ctx, tx, pid, tid)
		return t, "design.thread.updated", tid, t.Revision, err
	})
}
func (s *Store) DesignEditComment(ctx context.Context, pid, actor, cid, key string, expected int, text string, remove bool) (json.RawMessage, error) {
	if !remove {
		if err := designText(text); err != nil {
			return nil, err
		}
	}
	return s.designDo(ctx, pid, actor, key, "comment.edit:"+cid, map[string]any{"expected_revision": expected, "text": text, "deleted": remove}, false, func(tx pgx.Tx, role string) (any, string, string, int, error) {
		var c DesignComment
		var tid string
		err := tx.QueryRow(ctx, `SELECT c.id::text,c.revision,c.author_id::text,c.text,c.created_at,c.edited_at,c.deleted,t.id::text FROM design_comments c JOIN design_threads t ON t.id=c.thread_id WHERE t.project_id=$1 AND c.id=$2`, pid, cid).Scan(&c.ID, &c.Revision, &c.AuthorID, &c.Text, &c.CreatedAt, &c.EditedAt, &c.Deleted, &tid)
		if err != nil {
			return nil, "", "", 0, nf(err)
		}
		if c.AuthorID != actor && (!remove || role != domain.RoleOwner) {
			return nil, "", "", 0, design.Invalid("forbidden", "comment", "редактирование доступно автору")
		}
		if c.Revision != expected {
			return nil, "", "", 0, designConflict("revision_conflict", c.Revision)
		}
		if c.Deleted {
			return nil, "", "", 0, designConflict("revision_conflict", c.Revision)
		}
		if remove {
			text = ""
		}
		c.Text = text
		c.Deleted = remove
		c.Revision++
		if err = tx.QueryRow(ctx, `UPDATE design_comments SET text=$2,deleted=$3,revision=$4,edited_at=now() WHERE id=$1 RETURNING edited_at`, cid, text, remove, c.Revision).Scan(&c.EditedAt); err != nil {
			return nil, "", "", 0, err
		}
		if err = designCommentHistory(ctx, tx, actor, c); err != nil {
			return nil, "", "", 0, err
		}
		threadRevision := 0
		err = tx.QueryRow(ctx, `UPDATE design_threads SET revision=revision+1 WHERE id=$1 RETURNING revision`, tid).Scan(&threadRevision)
		return c, "design.thread.updated", tid, threadRevision, err
	})
}

type DesignContextRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}
type DesignHandoff struct {
	ID                string             `json:"id"`
	ProjectID         string             `json:"project_id"`
	DocumentID        string             `json:"document_id"`
	Revision          int                `json:"revision"`
	ModelSHA          string             `json:"model_sha"`
	ManifestSHA       string             `json:"manifest_sha"`
	ScreenshotAssetID string             `json:"screenshot_asset_id"`
	AssetRefs         []DesignAsset      `json:"asset_refs"`
	ThreadsSnapshot   []DesignThread     `json:"threads_snapshot"`
	ContextRefs       []DesignContextRef `json:"context_refs"`
	GitCommitSHA      *string            `json:"git_commit_sha"`
	CreatedBy         string             `json:"created_by"`
	CreatedAt         time.Time          `json:"created_at"`
	Links             []DesignContextRef `json:"links"`
	Model             design.Model       `json:"model"`
}

func designContextRefs(ctx context.Context, q dbtx, pid string, refs []DesignContextRef) error {
	if len(refs) > 100 {
		return design.Invalid("size_limit", "links", "не более100ссылок")
	}
	for _, r := range refs {
		if !design.ValidID(r.ID) {
			return design.Invalid("invalid_ref", "links.id", "ожидается UUID")
		}
		var ok bool
		var err error
		switch r.Kind {
		case "task":
			err = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM tasks t JOIN epics e ON e.id=t.epic_id WHERE e.project_id=$1 AND t.id=$2)`, pid, r.ID).Scan(&ok)
		case "research":
			err = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM discovery_runs WHERE project_id=$1 AND id=$2)`, pid, r.ID).Scan(&ok)
		case "architecture":
			err = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM epic_architecture_assessments WHERE project_id=$1 AND epic_id=$2) OR EXISTS(SELECT 1 FROM project_architecture WHERE project_id=$1 AND project_id=$2)`, pid, r.ID).Scan(&ok)
		default:
			return design.Invalid("invalid_ref", "links.kind", "неподдерживаемая связь")
		}
		if err != nil {
			return err
		}
		if !ok {
			return design.Invalid("invalid_ref", "links", "связанный контекст недоступен")
		}
	}
	return nil
}
func (s *Store) DesignCreateHandoff(ctx context.Context, pid, actor, id, key string, revision int, tids []string, refs []DesignContextRef, screenshot DesignAsset) (json.RawMessage, error) {
	if revision < 1 {
		return nil, design.Invalid("invalid_ref", "revision", "нужна конкретная revision")
	}
	return s.designDo(ctx, pid, actor, key, "handoff.create:"+id, map[string]any{"revision": revision, "thread_ids": tids, "links": refs}, len(refs) > 0, func(tx pgx.Tx, role string) (any, string, string, int, error) {
		doc, err := designDocTx(ctx, tx, pid, id, revision)
		if err != nil {
			return nil, "", "", 0, err
		}
		if err = designContextRefs(ctx, tx, pid, refs); err != nil {
			return nil, "", "", 0, err
		}
		if err = designValidateRefs(ctx, tx, pid, id, doc.Model); err != nil {
			return nil, "", "", 0, err
		}
		assets, err := designAssetsForModel(ctx, tx, pid, doc.Model)
		if err != nil {
			return nil, "", "", 0, err
		}
		if screenshot.MIME != "image/png" || !design.ValidID(screenshot.ID) || !design.ValidSHA(screenshot.SHA256) || screenshot.SizeBytes <= 0 || screenshot.ObjectRef == "" {
			return nil, "", "", 0, design.Invalid("invalid_ref", "screenshot", "нужен проверенный PNG снимок")
		}
		if len(tids) > 100 {
			return nil, "", "", 0, design.Invalid("size_limit", "thread_ids", "не более100веток")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO design_assets(id,project_id,sha256,mime,size_bytes,object_ref,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)`, screenshot.ID, pid, screenshot.SHA256, screenshot.MIME, screenshot.SizeBytes, screenshot.ObjectRef, actor); err != nil {
			return nil, "", "", 0, err
		}
		assets = append(assets, screenshot)
		threads := []DesignThread{}
		for _, tid := range tids {
			t, e := designThreadTx(ctx, tx, pid, tid)
			if e != nil {
				return nil, "", "", 0, e
			}
			if t.Anchor.DocumentID != id || t.Anchor.Revision > revision {
				return nil, "", "", 0, design.Invalid("invalid_ref", "thread_ids", "ветка не относится к переданной версии документа")
			}
			t.Orphaned = !design.NodeIDs(doc.Model)[t.Anchor.ElementID]
			threads = append(threads, t)
		}
		h := DesignHandoff{ID: uuid.NewString(), ProjectID: pid, DocumentID: id, Revision: revision, ModelSHA: doc.Document.ContentSHA, ScreenshotAssetID: screenshot.ID, AssetRefs: assets, ThreadsSnapshot: threads, ContextRefs: refs, CreatedBy: actor, CreatedAt: time.Now().UTC(), Links: []DesignContextRef{}, Model: doc.Model}
		h.ManifestSHA, err = DesignManifestSHA(h)
		if err != nil {
			return nil, "", "", 0, err
		}
		raw, err := json.Marshal(h)
		if err != nil {
			return nil, "", "", 0, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO design_handoffs(id,project_id,document_id,revision,manifest,manifest_sha,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, h.ID, pid, id, revision, raw, h.ManifestSHA, actor, h.CreatedAt)
		return h, "design.handoff.created", h.ID, revision, err
	})
}
func designHandoffTx(ctx context.Context, q dbtx, pid, id string) (DesignHandoff, error) {
	var h DesignHandoff
	var raw []byte
	err := q.QueryRow(ctx, `SELECT manifest FROM design_handoffs WHERE project_id=$1 AND id=$2`, pid, id).Scan(&raw)
	if err != nil {
		return h, nf(err)
	}
	if err = json.Unmarshal(raw, &h); err != nil {
		return h, err
	}
	if err = designContextRefs(ctx, q, pid, h.ContextRefs); err != nil {
		return h, err
	}
	if err = designAssetManifest(ctx, q, pid, h.AssetRefs); err != nil {
		return h, err
	}
	rows, err := q.Query(ctx, `SELECT kind,target_id FROM design_handoff_links WHERE handoff_id=$1 ORDER BY created_at,kind,target_id`, id)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		var r DesignContextRef
		if err = rows.Scan(&r.Kind, &r.ID); err != nil {
			rows.Close()
			return h, err
		}
		h.Links = append(h.Links, r)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return h, err
	}
	refs := []DesignContextRef{}
	for _, r := range h.Links {
		if r.Kind == "git_commit" {
			sha := r.ID
			h.GitCommitSHA = &sha
		} else {
			refs = append(refs, r)
		}
	}
	if err = designContextRefs(ctx, q, pid, refs); err != nil {
		return h, err
	}
	return h, nil
}
func (s *Store) DesignGetHandoff(ctx context.Context, pid, actor, id string) (DesignHandoff, error) {
	if err := s.designCanRead(ctx, pid, actor); err != nil {
		return DesignHandoff{}, err
	}
	return designHandoffTx(ctx, s.Pool, pid, id)
}
func (s *Store) DesignLinkHandoff(ctx context.Context, pid, actor, id, key string, ref DesignContextRef, verifiedGit bool) (json.RawMessage, error) {
	return s.designDo(ctx, pid, actor, key, "handoff.link:"+id, ref, true, func(tx pgx.Tx, role string) (any, string, string, int, error) {
		h, err := designHandoffTx(ctx, tx, pid, id)
		if err != nil {
			return nil, "", "", 0, err
		}
		if ref.Kind == "git_commit" {
			if !verifiedGit {
				return nil, "", "", 0, design.Invalid("invalid_ref", "id", "commit должен быть проверен через SCM")
			}
		} else if err = designContextRefs(ctx, tx, pid, []DesignContextRef{ref}); err != nil {
			return nil, "", "", 0, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO design_handoff_links(handoff_id,kind,target_id,actor_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, id, ref.Kind, ref.ID, actor)
		if err != nil {
			return nil, "", "", 0, err
		}
		h, err = designHandoffTx(ctx, tx, pid, id)
		return h, "design.handoff.created", id, h.Revision, err
	})
}

// DesignHandoffReplay checks the exact original request and current permissions
// before a caller spends resources producing another screenshot.
func (s *Store) DesignHandoffReplay(ctx context.Context, pid, actor, docID, key string, revision int, tids []string, refs []DesignContextRef) (json.RawMessage, bool, error) {
	if key == "" || len(key) > 200 {
		return nil, false, design.Invalid("idempotency_required", "Idempotency-Key", "нужен ключ1–200символов")
	}
	_, sha, err := design.Canonical(map[string]any{"operation": "handoff.create:" + docID, "input": map[string]any{"revision": revision, "thread_ids": tids, "links": refs}})
	if err != nil {
		return nil, false, err
	}
	var out json.RawMessage
	found := false
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		role, e := designRoleTx(ctx, tx, pid, actor)
		if e != nil {
			return e
		}
		if len(refs) > 0 && role != domain.RoleOwner {
			return design.Invalid("forbidden", "links", "нужна роль owner")
		}
		var savedSHA string
		var raw []byte
		e = tx.QueryRow(ctx, `SELECT request_sha,response FROM design_requests WHERE project_id=$1 AND actor_id=$2 AND key=$3`, pid, actor, key).Scan(&savedSHA, &raw)
		if e == pgx.ErrNoRows {
			return nil
		}
		if e != nil {
			return e
		}
		found = true
		if savedSHA != sha {
			return designConflict("idempotency_conflict", 0)
		}
		out, _, e = design.Canonical(json.RawMessage(raw))
		return e
	})
	return out, found, err
}

// DesignManifestSHA hashes the complete immutable payload. Temporary capability
// URLs and mutable link records are never part of this content digest.
func DesignManifestSHA(h DesignHandoff) (string, error) {
	assets := append([]DesignAsset{}, h.AssetRefs...)
	for i := range assets {
		assets[i].URL = ""
	}
	manifest := map[string]any{"revision": h.Revision, "model_sha": h.ModelSHA, "model": h.Model, "asset_refs": assets, "screenshot_asset_id": h.ScreenshotAssetID, "threads_snapshot": h.ThreadsSnapshot, "context_refs": h.ContextRefs}
	_, sha, err := design.Canonical(manifest)
	return sha, err
}
