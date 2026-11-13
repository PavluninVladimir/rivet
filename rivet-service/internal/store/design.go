package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PavluninVladimir/rivet/internal/design"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type DesignDocument struct {
	ID         string          `json:"id"`
	ProjectID  string          `json:"project_id"`
	Name       string          `json:"name"`
	Revision   int             `json:"revision"`
	ContentSHA string          `json:"content_sha"`
	CreatedBy  string          `json:"created_by"`
	UpdatedAt  time.Time       `json:"updated_at"`
	Base       *design.BaseRef `json:"base"`
}
type DesignDocumentModel struct {
	Document DesignDocument `json:"document"`
	Model    design.Model   `json:"model"`
}
type DesignRevision struct {
	Revision       int         `json:"revision"`
	ParentRevision *int        `json:"parent_revision"`
	ContentSHA     string      `json:"content_sha"`
	ActorID        string      `json:"actor_id"`
	CreatedAt      time.Time   `json:"created_at"`
	Diff           design.Diff `json:"diff"`
}
type DesignSave struct {
	Document DesignDocument `json:"document"`
	Diff     design.Diff    `json:"diff"`
}
type DesignAsset struct {
	ID        string `json:"id"`
	SHA256    string `json:"sha256"`
	MIME      string `json:"mime"`
	SizeBytes int64  `json:"size_bytes"`
	ObjectRef string `json:"-"`
	URL       string `json:"url,omitempty"`
}

func designConflict(code string, current int) error {
	return &design.Error{Code: code, Message: "состояние изменилось, перечитайте документ", CurrentRevision: current}
}

// The user row and project membership locks use the same order as the member
// management services. Removing membership/revoking a user cannot race a save.
func designRoleTx(ctx context.Context, tx pgx.Tx, pid, actor string) (string, error) {
	var disabled bool
	if err := tx.QueryRow(ctx, `SELECT disabled FROM users WHERE id=$1 FOR SHARE`, actor).Scan(&disabled); err != nil {
		return "", nf(err)
	}
	if disabled {
		return "", ErrNotFound
	}
	if err := lockProjects(ctx, tx, []string{pid}); err != nil {
		return "", err
	}
	var role string
	err := tx.QueryRow(ctx, `SELECT role FROM project_members WHERE project_id=$1 AND user_id=$2`, pid, actor).Scan(&role)
	return role, nf(err)
}
func (s *Store) designDo(ctx context.Context, pid, actor, key, operation string, input any, owner bool, fn func(pgx.Tx, string) (any, string, string, int, error)) (json.RawMessage, error) {
	if key == "" || len(key) > 200 || strings.TrimSpace(key) != key {
		return nil, design.Invalid("idempotency_required", "Idempotency-Key", "нужен ключ длиной1–200")
	}
	_, sha, err := design.Canonical(map[string]any{"operation": operation, "input": input})
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		role, e := designRoleTx(ctx, tx, pid, actor)
		if e != nil {
			return e
		}
		if owner && role != domain.RoleOwner {
			return design.Invalid("forbidden", "$", "нужна роль owner")
		}
		var prevSHA string
		var response []byte
		e = tx.QueryRow(ctx, `SELECT request_sha,response FROM design_requests WHERE project_id=$1 AND actor_id=$2 AND key=$3`, pid, actor, key).Scan(&prevSHA, &response)
		if e == nil {
			if prevSHA != sha {
				return designConflict("idempotency_conflict", 0)
			}
			result, _, e = design.Canonical(json.RawMessage(response))
			if e != nil {
				return e
			}
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		out, kind, resource, rev, e := fn(tx, role)
		if e != nil {
			return e
		}
		result, _, e = design.Canonical(out)
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO design_requests(project_id,actor_id,key,request_sha,response) VALUES($1,$2,$3,$4,$5)`, pid, actor, key, sha, []byte(result)); e != nil {
			return e
		}
		documentID := ""
		switch kind {
		case "design.thread.updated":
			e = tx.QueryRow(ctx, `SELECT document_id::text FROM design_threads WHERE project_id=$1 AND id=$2`, pid, resource).Scan(&documentID)
		case "design.handoff.created":
			e = tx.QueryRow(ctx, `SELECT document_id::text FROM design_handoffs WHERE project_id=$1 AND id=$2`, pid, resource).Scan(&documentID)
		default:
			e = tx.QueryRow(ctx, `SELECT id::text FROM design_documents WHERE project_id=$1 AND id=$2`, pid, resource).Scan(&documentID)
			if errors.Is(e, pgx.ErrNoRows) {
				e = nil
			}
		}
		if e != nil {
			return e
		}
		cursor, e := appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: actor, Type: kind, ProjectID: pid, Text: "дизайн-система: " + operation, Payload: map[string]any{"project_id": pid, "document_id": documentID, "resource_id": resource, "revision": rev}})
		if e != nil {
			return e
		}
		// Cursor is bound to the same durable event before the transaction is
		// visible. No later update or separately delivered invalidation exists.
		_, e = tx.Exec(ctx, `UPDATE events SET payload=payload||jsonb_build_object('event_cursor',$2::bigint) WHERE id=$1`, cursor, cursor)
		return e
	})
	return result, err
}
func (s *Store) designCanRead(ctx context.Context, pid, actor string) error {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM project_members m JOIN users u ON u.id=m.user_id WHERE m.project_id=$1 AND m.user_id=$2 AND NOT u.disabled)`, pid, actor).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}
func designDocTx(ctx context.Context, q dbtx, pid, id string, revision int) (DesignDocumentModel, error) {
	var out DesignDocumentModel
	var raw []byte
	err := q.QueryRow(ctx, `SELECT d.id::text,d.project_id::text,d.name,dr.revision,dr.content_sha,d.created_by::text,dr.created_at,dr.model FROM design_documents d JOIN design_revisions dr ON dr.document_id=d.id AND dr.revision=CASE WHEN $3=0 THEN d.revision ELSE $3 END WHERE d.project_id=$1 AND d.id=$2`, pid, id, revision).Scan(&out.Document.ID, &out.Document.ProjectID, &out.Document.Name, &out.Document.Revision, &out.Document.ContentSHA, &out.Document.CreatedBy, &out.Document.UpdatedAt, &raw)
	if err != nil {
		return out, nf(err)
	}
	err = json.Unmarshal(raw, &out.Model)
	out.Document.Base = out.Model.Base
	return out, err
}
func (s *Store) DesignGetDocument(ctx context.Context, pid, actor, id string, revision int) (DesignDocumentModel, error) {
	if err := s.designCanRead(ctx, pid, actor); err != nil {
		return DesignDocumentModel{}, err
	}
	return designDocTx(ctx, s.Pool, pid, id, revision)
}
func (s *Store) DesignListDocuments(ctx context.Context, pid, actor, before string, limit int) ([]DesignDocument, string, error) {
	if err := s.designCanRead(ctx, pid, actor); err != nil {
		return nil, "", err
	}
	if limit < 1 || limit > 100 {
		return nil, "", design.Invalid("invalid_ref", "limit", "limit1–100")
	}
	rows, err := s.Pool.Query(ctx, `SELECT d.id::text FROM design_documents d WHERE d.project_id=$1 AND ($2='' OR d.id<NULLIF($2,'')::uuid) ORDER BY d.id DESC LIMIT $3`, pid, before, limit+1)
	if err != nil {
		return nil, "", err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, "", err
		}
		ids = append(ids, id)
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
	out := []DesignDocument{}
	for _, id := range ids {
		d, e := designDocTx(ctx, s.Pool, pid, id, 0)
		if e != nil {
			return nil, "", e
		}
		out = append(out, d.Document)
	}
	return out, next, nil
}
func designValidateRefs(ctx context.Context, tx pgx.Tx, pid, docID string, m design.Model) error {
	if m.Base != nil {
		if m.Base.DocumentID == docID {
			return design.Invalid("cycle", "base", "документ не может наследовать себя")
		}
		base, err := designDocTx(ctx, tx, pid, m.Base.DocumentID, m.Base.Revision)
		if err != nil {
			return design.Invalid("invalid_ref", "base", "база недоступна")
		}
		if base.Document.ContentSHA != m.Base.ContentSHA {
			return design.Invalid("invalid_ref", "base.content_sha", "SHA базы не совпадает")
		}
		seen := map[string]bool{docID: true}
		for b := &base; b != nil; {
			if seen[b.Document.ID] {
				return design.Invalid("cycle", "base", "цикл наследования")
			}
			seen[b.Document.ID] = true
			if b.Model.Base == nil {
				break
			}
			next, e := designDocTx(ctx, tx, pid, b.Model.Base.DocumentID, b.Model.Base.Revision)
			if e != nil {
				return e
			}
			b = &next
			if len(seen) > 64 {
				return design.Invalid("size_limit", "base", "слишком глубокое наследование")
			}
		}
	}
	for _, id := range design.AssetIDs(m) {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM design_assets WHERE project_id=$1 AND id=$2)`, pid, id).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return design.Invalid("invalid_ref", "asset_id", "asset недоступен")
		}
	}
	return nil
}
func designPutRevision(ctx context.Context, tx pgx.Tx, pid, actor string, doc *DesignDocument, m design.Model, diff design.Diff, previous *design.Model, restore bool) error {
	raw, sha, err := design.Canonical(m)
	if err != nil {
		return err
	}
	newIDs := design.IdentityIDs(m)
	oldIDs := map[string]bool{}
	if previous != nil {
		oldIDs = design.IdentityIDs(*previous)
	}
	for id := range newIDs {
		if oldIDs[id] {
			continue
		}
		var used bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM design_element_ids WHERE document_id=$1 AND element_id=$2)`, doc.ID, id).Scan(&used); err != nil {
			return err
		}
		if used && !restore {
			return design.Invalid("invalid_ref", "id", "удалённый element ID нельзя использовать повторно")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO design_element_ids(document_id,element_id,first_revision) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, doc.ID, id, doc.Revision); err != nil {
			return err
		}
	}
	diffRaw, err := json.Marshal(diff)
	if err != nil {
		return err
	}
	var parent *int
	if doc.Revision > 1 {
		p := doc.Revision - 1
		parent = &p
	}
	if _, err = tx.Exec(ctx, `INSERT INTO design_revisions(document_id,revision,parent_revision,model,content_sha,actor_id,diff) VALUES($1,$2,$3,$4,$5,$6,$7)`, doc.ID, doc.Revision, parent, raw, sha, actor, diffRaw); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `UPDATE design_documents SET revision=$2,content_sha=$3,updated_at=now() WHERE id=$1 RETURNING updated_at`, doc.ID, doc.Revision, sha).Scan(&doc.UpdatedAt); err != nil {
		return err
	}
	doc.ContentSHA = sha
	doc.Base = m.Base
	return nil
}
func (s *Store) DesignCreateDocument(ctx context.Context, pid, actor, key, name string, m design.Model) (json.RawMessage, error) {
	if strings.TrimSpace(name) == "" || len([]rune(name)) > 200 {
		return nil, design.Invalid("unsupported_property", "name", "нужно имя до200символов")
	}
	if err := design.Validate(m); err != nil {
		return nil, err
	}
	return s.designDo(ctx, pid, actor, key, "document.create", map[string]any{"name": name, "model": m}, m.Base != nil, func(tx pgx.Tx, role string) (any, string, string, int, error) {
		id := uuid.NewString()
		if err := designValidateRefs(ctx, tx, pid, id, m); err != nil {
			return nil, "", "", 0, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO design_documents(id,project_id,name,revision,content_sha,created_by) VALUES($1,$2,$3,1,'',$4)`, id, pid, name, actor); err != nil {
			return nil, "", "", 0, err
		}
		doc := DesignDocument{ID: id, ProjectID: pid, Name: name, Revision: 1, CreatedBy: actor}
		diff := design.Diff{BaseRevision: 0, Operations: []design.Patch{}, AffectedElementIDs: []string{}, Conflicts: []design.Diagnostic{}}
		err := designPutRevision(ctx, tx, pid, actor, &doc, m, diff, nil, false)
		return doc, "design.document.updated", id, 1, err
	})
}

type DesignPatchRequest struct {
	ExpectedRevision int            `json:"expected_revision"`
	Operations       []design.Patch `json:"operations"`
}

func (s *Store) DesignPatchDocument(ctx context.Context, pid, actor, id, key string, in DesignPatchRequest, preview bool) (json.RawMessage, error) {
	apply := func(tx pgx.Tx, role string) (any, string, string, int, error) {
		current, err := designDocTx(ctx, tx, pid, id, 0)
		if err != nil {
			return nil, "", "", 0, err
		}
		if current.Document.Revision != in.ExpectedRevision {
			return nil, "", "", 0, designConflict("revision_conflict", current.Document.Revision)
		}
		model, diff, err := design.Apply(current.Model, in.ExpectedRevision, in.Operations)
		if err != nil {
			return nil, "", "", 0, err
		}
		for _, op := range in.Operations {
			if op.Op == "update_base" {
				if role != domain.RoleOwner {
					return nil, "", "", 0, design.Invalid("forbidden", "base", "нужна роль owner")
				}
				if op.Base != nil {
					newBase, e := designDocTx(ctx, tx, pid, op.Base.DocumentID, op.Base.Revision)
					if e != nil {
						return nil, "", "", 0, design.Invalid("invalid_ref", "base", "база недоступна")
					}
					oldBase := design.Model{Schema: design.Schema, Tokens: []design.Token{}, Components: []design.Component{}, Screens: []design.Screen{}, Overrides: []design.Override{}}
					if current.Model.Base != nil {
						old, e := designDocTx(ctx, tx, pid, current.Model.Base.DocumentID, current.Model.Base.Revision)
						if e != nil {
							return nil, "", "", 0, e
						}
						oldBase = old.Model
					}
					model, e = design.Rebase(model, oldBase, newBase.Model)
					if e != nil {
						return nil, "", "", 0, e
					}
					model.Base = op.Base
				}
			}
		}
		if err = designValidateRefs(ctx, tx, pid, id, model); err != nil {
			return nil, "", "", 0, err
		}
		if preview {
			return diff, "", "", 0, nil
		}
		doc := current.Document
		doc.Revision++
		err = designPutRevision(ctx, tx, pid, actor, &doc, model, diff, &current.Model, false)
		return DesignSave{doc, diff}, "design.document.updated", id, doc.Revision, err
	}
	if !preview {
		return s.designDo(ctx, pid, actor, key, "document.patch:"+id, in, false, apply)
	}
	var result json.RawMessage
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		role, e := designRoleTx(ctx, tx, pid, actor)
		if e != nil {
			return e
		}
		out, _, _, _, e := apply(tx, role)
		if e != nil {
			return e
		}
		result, _, e = design.Canonical(out)
		return e
	})
	return result, err
}
func (s *Store) DesignRevisions(ctx context.Context, pid, actor, id string, before, limit int) ([]DesignRevision, int, error) {
	if _, err := s.DesignGetDocument(ctx, pid, actor, id, 0); err != nil {
		return nil, 0, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT revision,parent_revision,content_sha,actor_id::text,created_at,diff FROM design_revisions WHERE document_id=$1 AND ($2=0 OR revision<$2) ORDER BY revision DESC LIMIT $3`, id, before, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []DesignRevision{}
	for rows.Next() {
		var r DesignRevision
		var raw []byte
		if err = rows.Scan(&r.Revision, &r.ParentRevision, &r.ContentSHA, &r.ActorID, &r.CreatedAt, &raw); err != nil {
			return nil, 0, err
		}
		if err = json.Unmarshal(raw, &r.Diff); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	next := 0
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].Revision
	}
	return out, next, rows.Err()
}
func (s *Store) DesignUndo(ctx context.Context, pid, actor, id, key string, expected, undo int) (json.RawMessage, error) {
	return s.designDo(ctx, pid, actor, key, "document.undo:"+id, map[string]int{"expected_revision": expected, "undo_revision": undo}, false, func(tx pgx.Tx, role string) (any, string, string, int, error) {
		current, err := designDocTx(ctx, tx, pid, id, 0)
		if err != nil {
			return nil, "", "", 0, err
		}
		if expected != current.Document.Revision {
			return nil, "", "", 0, designConflict("revision_conflict", current.Document.Revision)
		}
		if undo < 2 || undo > expected {
			return nil, "", "", 0, design.Invalid("undo_conflict", "undo_revision", "нет обратимой предыдущей версии")
		}
		after, err := designDocTx(ctx, tx, pid, id, undo)
		if err != nil {
			return nil, "", "", 0, err
		}
		before, err := designDocTx(ctx, tx, pid, id, undo-1)
		if err != nil {
			return nil, "", "", 0, err
		}
		model, err := design.Inverse(current.Model, before.Model, after.Model)
		if err != nil {
			return nil, "", "", 0, err
		}
		if fmt.Sprint(model.Base) != fmt.Sprint(current.Model.Base) && role != domain.RoleOwner {
			return nil, "", "", 0, design.Invalid("forbidden", "base", "нужна роль owner")
		}
		if err = designValidateRefs(ctx, tx, pid, id, model); err != nil {
			return nil, "", "", 0, err
		}
		diff := design.Diff{BaseRevision: expected, Operations: []design.Patch{}, AffectedElementIDs: []string{}, Conflicts: []design.Diagnostic{}}
		doc := current.Document
		doc.Revision++
		err = designPutRevision(ctx, tx, pid, actor, &doc, model, diff, &current.Model, true)
		return DesignSave{doc, diff}, "design.document.updated", id, doc.Revision, err
	})
}
func (s *Store) DesignImport(ctx context.Context, pid, actor, id, key string, expected int, m design.Model, assets []DesignAsset) (json.RawMessage, error) {
	if err := design.Validate(m); err != nil {
		return nil, err
	}
	return s.designDo(ctx, pid, actor, key, "document.import:"+id, map[string]any{"expected_revision": expected, "model": m, "assets": assets}, false, func(tx pgx.Tx, role string) (any, string, string, int, error) {
		current, err := designDocTx(ctx, tx, pid, id, 0)
		if err != nil {
			return nil, "", "", 0, err
		}
		if current.Document.Revision != expected {
			return nil, "", "", 0, designConflict("revision_conflict", current.Document.Revision)
		}
		_, a, _ := design.Canonical(current.Model.Base)
		_, b, _ := design.Canonical(m.Base)
		if a != b && role != domain.RoleOwner {
			return nil, "", "", 0, design.Invalid("forbidden", "base", "нужна роль owner")
		}
		if err = designValidateRefs(ctx, tx, pid, id, m); err != nil {
			return nil, "", "", 0, err
		}
		if err = designAssetManifest(ctx, tx, pid, assets); err != nil {
			return nil, "", "", 0, err
		}
		doc := current.Document
		doc.Revision++
		diff := design.Diff{BaseRevision: expected, Operations: []design.Patch{}, AffectedElementIDs: []string{}, Conflicts: []design.Diagnostic{}}
		err = designPutRevision(ctx, tx, pid, actor, &doc, m, diff, &current.Model, false)
		return doc, "design.document.updated", id, doc.Revision, err
	})
}
func designAssetManifest(ctx context.Context, q dbtx, pid string, refs []DesignAsset) error {
	for _, a := range refs {
		var sha, mime string
		var size int64
		if err := q.QueryRow(ctx, `SELECT sha256,mime,size_bytes FROM design_assets WHERE project_id=$1 AND id=$2`, pid, a.ID).Scan(&sha, &mime, &size); err != nil {
			return design.Invalid("invalid_ref", "assets", "asset недоступен")
		}
		if sha != a.SHA256 || mime != a.MIME || size != a.SizeBytes {
			return design.Invalid("invalid_ref", "assets", "metadata/checksum не совпадают")
		}
	}
	return nil
}
func (s *Store) DesignValidateImport(ctx context.Context, pid, actor, id string, m design.Model, assets []DesignAsset) (design.Diff, error) {
	diff := design.Diff{Operations: []design.Patch{}, AffectedElementIDs: []string{}, Conflicts: []design.Diagnostic{}}
	if err := design.Validate(m); err != nil {
		return diff, err
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, e := designRoleTx(ctx, tx, pid, actor); e != nil {
			return e
		}
		if id != "" {
			old, e := designDocTx(ctx, tx, pid, id, 0)
			if e != nil {
				return e
			}
			diff.BaseRevision = old.Document.Revision
		}
		if e := designValidateRefs(ctx, tx, pid, id, m); e != nil {
			return e
		}
		return designAssetManifest(ctx, tx, pid, assets)
	})
	return diff, err
}
func (s *Store) DesignRegisterAsset(ctx context.Context, pid, actor, key string, a DesignAsset) (json.RawMessage, error) {
	if !design.ValidID(a.ID) || !design.ValidSHA(a.SHA256) || (a.MIME != "image/png" && a.MIME != "image/jpeg") || a.SizeBytes <= 0 || a.SizeBytes > design.MaxAssetBytes || !strings.HasPrefix(a.ObjectRef, "s3://") {
		return nil, design.Invalid("invalid_ref", "asset", "неверные metadata/private object reference")
	}
	return s.designDo(ctx, pid, actor, key, "asset.upload", a, false, func(tx pgx.Tx, role string) (any, string, string, int, error) {
		_, err := tx.Exec(ctx, `INSERT INTO design_assets(id,project_id,sha256,mime,size_bytes,object_ref,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)`, a.ID, pid, a.SHA256, a.MIME, a.SizeBytes, a.ObjectRef, actor)
		return a, "design.asset.created", a.ID, 0, err
	})
}
func (s *Store) DesignGetAsset(ctx context.Context, pid, actor, id string) (DesignAsset, error) {
	var a DesignAsset
	if err := s.designCanRead(ctx, pid, actor); err != nil {
		return a, err
	}
	err := s.Pool.QueryRow(ctx, `SELECT id::text,sha256,mime,size_bytes,object_ref FROM design_assets WHERE project_id=$1 AND id=$2`, pid, id).Scan(&a.ID, &a.SHA256, &a.MIME, &a.SizeBytes, &a.ObjectRef)
	return a, nf(err)
}
func designAssetsForModel(ctx context.Context, q dbtx, pid string, m design.Model) ([]DesignAsset, error) {
	out := []DesignAsset{}
	for _, id := range design.AssetIDs(m) {
		var a DesignAsset
		err := q.QueryRow(ctx, `SELECT id::text,sha256,mime,size_bytes,object_ref FROM design_assets WHERE project_id=$1 AND id=$2`, pid, id).Scan(&a.ID, &a.SHA256, &a.MIME, &a.SizeBytes, &a.ObjectRef)
		if err != nil {
			return nil, nf(err)
		}
		out = append(out, a)
	}
	return out, nil
}
func (s *Store) DesignExport(ctx context.Context, pid, actor, id string, revision int) (json.RawMessage, error) {
	if revision < 1 {
		return nil, design.Invalid("invalid_ref", "revision", "нужна конкретная revision")
	}
	d, err := s.DesignGetDocument(ctx, pid, actor, id, revision)
	if err != nil {
		return nil, err
	}
	assets, err := designAssetsForModel(ctx, s.Pool, pid, d.Model)
	if err != nil {
		return nil, err
	}
	raw, _, err := design.Canonical(map[string]any{"document": d.Document, "model": d.Model, "assets": assets})
	return raw, err
}
