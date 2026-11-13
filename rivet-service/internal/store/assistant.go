package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/PavluninVladimir/rivet/internal/assistant"
	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/jackc/pgx/v5"
)

func (s *Store) AssistantConfig(ctx context.Context, project, actor string) (assistant.Config, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return assistant.Config{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = assistantActorTx(ctx, tx, project, actor, false, false); err != nil {
		return assistant.Config{}, err
	}
	c, err := assistantConfigTx(ctx, tx, project, false)
	if err != nil {
		return c, err
	}
	return c, tx.Commit(ctx)
}
func assistantConfigTx(ctx context.Context, tx pgx.Tx, project string, lock bool) (assistant.Config, error) {
	c := assistant.Config{Limits: assistant.DefaultLimits()}
	var raw []byte
	q := `SELECT revision,enabled,sponsor_user_id::text,connection_id,model_id,limits FROM assistant_configs WHERE project_id=$1`
	if lock {
		q += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, q, project).Scan(&c.Revision, &c.Enabled, &c.SponsorID, &c.ConnectionID, &c.ModelID, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(raw, &c.Limits)
}
func (s *Store) SaveAssistantConfig(ctx context.Context, project, actor string, expected int64, c assistant.Config) (assistant.Config, error) {
	if err := c.Limits.Validate(); err != nil {
		return c, err
	}
	if c.ConnectionID == nil || c.ModelID == nil || *c.ConnectionID == "" || *c.ModelID == "" {
		if c.Enabled {
			return c, assistant.Err(422, "configuration_required", "выберите подключение и модель")
		}
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := assistantActorTx(ctx, tx, project, actor, true, true); err != nil {
			return err
		}
		old, err := assistantConfigTx(ctx, tx, project, true)
		if err != nil {
			return err
		}
		if old.Revision != expected {
			return assistant.Err(409, "stale_revision", "настройка уже изменилась")
		}
		if c.Enabled {
			if _, err = assistantModelTx(ctx, tx, *c.ConnectionID, *c.ModelID); err != nil {
				return err
			}
		}
		raw, _ := json.Marshal(c.Limits)
		c.Revision = expected + 1
		c.SponsorID = &actor
		_, err = tx.Exec(ctx, `INSERT INTO assistant_configs(project_id,revision,enabled,sponsor_user_id,connection_id,model_id,limits) VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT(project_id) DO UPDATE SET revision=$2,enabled=$3,sponsor_user_id=$4,connection_id=$5,model_id=$6,limits=$7,updated_at=now()`, project, c.Revision, c.Enabled, actor, c.ConnectionID, c.ModelID, raw)
		if err != nil {
			return err
		}
		return assistantEventTx(ctx, tx, project, "", "assistant.snapshot.invalidated", "настройка ассистента изменена", map[string]any{"revision": c.Revision})
	})
	return c, err
}
func assistantModelTx(ctx context.Context, tx pgx.Tx, connection, model string) (domain.ModelEntry, error) {
	var enabled bool
	var state string
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT enabled,state,models FROM model_connections WHERE id=$1`, connection).Scan(&enabled, &state, &raw); err != nil {
		return domain.ModelEntry{}, assistant.Err(503, "model_unavailable", "подключение модели не найдено")
	}
	if !enabled || state == "invalid" {
		return domain.ModelEntry{}, assistant.Err(503, "model_unavailable", "подключение недоступно")
	}
	var models []domain.ModelEntry
	if err := json.Unmarshal(raw, &models); err != nil {
		return domain.ModelEntry{}, err
	}
	for _, m := range models {
		if m.ID == model && !m.Hidden && !m.Missing {
			return m, nil
		}
	}
	return domain.ModelEntry{}, assistant.Err(503, "model_unavailable", "модель недоступна в каталоге")
}
func assistantEventTx(ctx context.Context, tx pgx.Tx, project, run, kind, text string, extra map[string]any) error {
	if extra == nil {
		extra = map[string]any{}
	}
	extra["project_id"] = project
	if run != "" {
		extra["run_id"] = run
	}
	_, err := appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorSystem, ActorID: "project-assistant", Type: kind, ProjectID: project, Text: text, Payload: extra})
	return err
}

const assistantRunCols = `id::text,revision,status,trigger,initiator_id::text,snapshot_revision,created_at,finished_at,summary,uncertainties,usage,followups`

func scanAssistantRun(row pgx.Row) (assistant.Run, error) {
	r := assistant.Run{}
	var uncertainty, usage, followups []byte
	err := row.Scan(&r.ID, &r.Revision, &r.Status, &r.Trigger, &r.InitiatorID, &r.SnapshotRevision, &r.CreatedAt, &r.FinishedAt, &r.Summary, &uncertainty, &usage, &followups)
	if err != nil {
		return r, nf(err)
	}
	if err = json.Unmarshal(uncertainty, &r.Uncertainties); err != nil {
		return r, err
	}
	if err = json.Unmarshal(followups, &r.Followups); err != nil {
		return r, err
	}
	return r, json.Unmarshal(usage, &r.Usage)
}

const assistantMessageCols = `id::text,author,actor_id::text,text,created_at,run_id::text,ref_ids`

func scanAssistantMessage(row pgx.Row) (assistant.Message, error) {
	m := assistant.Message{}
	var refs []byte
	err := row.Scan(&m.ID, &m.Author, &m.ActorID, &m.Text, &m.CreatedAt, &m.RunID, &refs)
	if err != nil {
		return m, nf(err)
	}
	return m, json.Unmarshal(refs, &m.RefIDs)
}
func (s *Store) CreateAssistantRun(ctx context.Context, project, actor, key, text, trigger, expected string) (assistant.Message, assistant.Run, bool, error) {
	if trigger != "question" && trigger != "recover" {
		return assistant.Message{}, assistant.Run{}, false, ErrInvalid
	}
	return s.createAssistantRun(ctx, project, actor, key, text, trigger, expected)
}
func (s *Store) createAssistantRun(ctx context.Context, project, actor, key, text, trigger, expected string) (assistant.Message, assistant.Run, bool, error) {
	var m assistant.Message
	var r assistant.Run
	var fresh bool
	if key == "" || len(key) > 200 || len([]rune(text)) < 1 || len([]rune(text)) > 8000 || (trigger != "question" && trigger != "recover" && trigger != "event") {
		return m, r, false, assistant.Err(422, "invalid_input", "нужны Idempotency-Key, text и intent")
	}
	snap, err := s.AssistantSnapshot(ctx, project, actor)
	if err != nil {
		return m, r, false, err
	}
	body := assistant.Hash([]string{text, trigger, expected})
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := assistantActorTx(ctx, tx, project, actor, false, true); err != nil {
			return err
		}
		var oldHash, oldActor string
		er := tx.QueryRow(ctx, `SELECT body_hash,initiator_id::text FROM assistant_runs WHERE project_id=$1 AND intent_key=$2`, project, key).Scan(&oldHash, &oldActor)
		if er == nil {
			if oldHash != body || oldActor != actor {
				return assistant.Err(409, "idempotency_conflict", "ключ уже использован другим запросом")
			}
			r, er = scanAssistantRun(tx.QueryRow(ctx, `SELECT `+assistantRunCols+` FROM assistant_runs WHERE project_id=$1 AND intent_key=$2`, project, key))
			if er != nil {
				return er
			}
			m, er = scanAssistantMessage(tx.QueryRow(ctx, `SELECT `+assistantMessageCols+` FROM assistant_messages WHERE run_id=$1 AND author='user'`, r.ID))
			return er
		}
		if !errors.Is(er, pgx.ErrNoRows) {
			return er
		}
		if expected != "" && expected != snap.Revision {
			return assistant.Err(409, "stale_revision", "снимок проекта изменился, обновите панель")
		}
		c, er := assistantConfigTx(ctx, tx, project, true)
		if er != nil {
			return er
		}
		if trigger == "event" {
			if !c.Enabled || c.SponsorID == nil || *c.SponsorID != actor {
				return assistant.Err(403, "sponsor_revoked", "автоматический режим выключен")
			}
			if _, er = assistantActorTx(ctx, tx, project, actor, true, false); er != nil {
				return er
			}
		}
		if c.ConnectionID == nil || c.ModelID == nil {
			return assistant.Err(422, "configuration_required", "сначала настройте модель ассистента")
		}
		if _, er = assistantModelTx(ctx, tx, *c.ConnectionID, *c.ModelID); er != nil {
			return er
		}
		var pending int
		if er = tx.QueryRow(ctx, `SELECT count(*) FROM assistant_runs WHERE project_id=$1 AND status IN('queued','running')`, project).Scan(&pending); er != nil {
			return er
		}
		if pending >= 3 {
			return assistant.Err(429, "rate_limited", "у проекта уже есть запросы в работе")
		}
		raw, _ := json.Marshal(snap)
		r, er = scanAssistantRun(tx.QueryRow(ctx, `INSERT INTO assistant_runs(project_id,trigger,initiator_id,snapshot_revision,snapshot,config_revision,intent_key,body_hash,question) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+assistantRunCols, project, trigger, actor, snap.Revision, raw, c.Revision, key, body, assistant.Redact(text)))
		if er != nil {
			return er
		}
		m, er = scanAssistantMessage(tx.QueryRow(ctx, `INSERT INTO assistant_messages(project_id,author,actor_id,text,run_id) VALUES($1,'user',$2,$3,$4) RETURNING `+assistantMessageCols, project, actor, assistant.Redact(text), r.ID))
		if er != nil {
			return er
		}
		fresh = true
		return assistantEventTx(ctx, tx, project, r.ID, "assistant.message.created", "запрос ассистенту", map[string]any{"message_id": m.ID})
	})
	return m, r, fresh, err
}
func (s *Store) AssistantMessages(ctx context.Context, project, actor, before string, limit int) ([]assistant.Message, string, error) {
	if _, err := s.AssistantConfig(ctx, project, actor); err != nil {
		return nil, "", err
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	if before != "" && !assistant.ValidID(before) {
		return nil, "", assistant.Err(422, "invalid_input", "неверный курсор")
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+assistantMessageCols+` FROM assistant_messages WHERE project_id=$1 AND ($2='' OR (created_at,id)<(SELECT created_at,id FROM assistant_messages WHERE id=NULLIF($2,'')::uuid AND project_id=$1)) ORDER BY created_at DESC,id DESC LIMIT $3`, project, before, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []assistant.Message{}
	for rows.Next() {
		m, e := scanAssistantMessage(rows)
		if e != nil {
			return nil, "", e
		}
		out = append(out, m)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = out[len(out)-1].ID
	}
	return out, next, rows.Err()
}
func (s *Store) AssistantRun(ctx context.Context, project, actor, id string) (assistant.Run, []assistant.Action, error) {
	if !assistant.ValidID(id) {
		return assistant.Run{}, nil, ErrNotFound
	}
	if _, err := s.AssistantConfig(ctx, project, actor); err != nil {
		return assistant.Run{}, nil, err
	}
	r, err := scanAssistantRun(s.Pool.QueryRow(ctx, `SELECT `+assistantRunCols+` FROM assistant_runs WHERE id=$1 AND project_id=$2`, id, project))
	if err != nil {
		return r, nil, err
	}
	actions, err := s.assistantActions(ctx, id)
	return r, actions, err
}
func (s *Store) ActiveAssistantRun(ctx context.Context, project, actor string) (*assistant.Run, error) {
	if _, err := s.AssistantConfig(ctx, project, actor); err != nil {
		return nil, err
	}
	r, err := scanAssistantRun(s.Pool.QueryRow(ctx, `SELECT `+assistantRunCols+` FROM assistant_runs WHERE project_id=$1 AND status IN('queued','running') ORDER BY created_at LIMIT 1`, project))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return &r, err
}
func (s *Store) StopAssistantRun(ctx context.Context, project, actor, id, key string, revision int64) (assistant.Run, error) {
	var r assistant.Run
	if key == "" {
		return r, assistant.Err(422, "invalid_input", "нужен Idempotency-Key")
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := assistantActorTx(ctx, tx, project, actor, false, true); err != nil {
			return err
		}
		var err error
		r, err = scanAssistantRun(tx.QueryRow(ctx, `SELECT `+assistantRunCols+` FROM assistant_runs WHERE id=$1 AND project_id=$2 FOR UPDATE`, id, project))
		if err != nil {
			return err
		}
		if r.InitiatorID != actor {
			if _, err = assistantActorTx(ctx, tx, project, actor, true, false); err != nil {
				return err
			}
		}
		var stopKey, stopHash *string
		if err = tx.QueryRow(ctx, `SELECT stop_key,stop_body_hash FROM assistant_runs WHERE id=$1`, id).Scan(&stopKey, &stopHash); err != nil {
			return err
		}
		hash := assistant.Hash(revision)
		if stopKey != nil && *stopKey == key {
			if stopHash == nil || *stopHash != hash {
				return assistant.Err(409, "idempotency_conflict", "ключ остановки уже использован с другим телом")
			}
			return nil
		}
		if r.Status == "stopped" {
			return assistant.Err(409, "stale_revision", "запуск уже остановлен другим запросом")
		}

		if r.Revision != revision {
			return assistant.Err(409, "stale_revision", "запуск уже изменился")
		}
		if r.FinishedAt != nil {
			return assistant.Err(409, "stale_revision", "запуск уже завершён")
		}
		r, err = scanAssistantRun(tx.QueryRow(ctx, `UPDATE assistant_runs SET status='stopped',revision=revision+1,finished_at=now(),stop_key=$2,stop_body_hash=$3 WHERE id=$1 RETURNING `+assistantRunCols, id, key, hash))
		if err != nil {
			return err
		}
		return assistantEventTx(ctx, tx, project, id, "assistant.run.updated", "ассистент остановлен, задачи проекта продолжают работу", nil)
	})
	return r, err
}

// AssistantJob is durable input claimed by a fenced project executor.
type AssistantJob struct {
	Project  string
	Run      assistant.Run
	Snapshot assistant.Snapshot
	Config   assistant.Config
	Question string
	Epoch    int64
	Model    domain.ModelEntry
}

func (s *Store) ClaimAssistantJob(ctx context.Context) (*AssistantJob, error) {
	var job *AssistantJob
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var id, project string
		err := tx.QueryRow(ctx, `SELECT r.id::text,r.project_id::text FROM assistant_runs r LEFT JOIN assistant_leases l ON l.project_id=r.project_id WHERE (r.status='queued' OR (r.status='running' AND COALESCE(l.expires_at,'-infinity')<now())) AND COALESCE(l.expires_at,'-infinity')<now() ORDER BY r.created_at FOR UPDATE OF r SKIP LOCKED LIMIT 1`).Scan(&id, &project)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		j := &AssistantJob{Project: project}
		j.Run, err = scanAssistantRun(tx.QueryRow(ctx, `SELECT `+assistantRunCols+` FROM assistant_runs WHERE id=$1`, id))
		if err != nil {
			return err
		}
		if _, err = assistantActorTx(ctx, tx, project, j.Run.InitiatorID, false, false); err != nil {
			return assistantFailClaim(ctx, tx, project, id, "инициатор больше не участник проекта")
		}
		j.Config, err = assistantConfigTx(ctx, tx, project, true)
		if err != nil {
			return err
		}
		c := j.Config
		if j.Run.Trigger == "event" {
			if !c.Enabled || c.SponsorID == nil || *c.SponsorID != j.Run.InitiatorID {
				return assistantFailClaim(ctx, tx, project, id, "автоматический режим выключен или sponsor изменился")
			}
			if _, err = assistantActorTx(ctx, tx, project, j.Run.InitiatorID, true, false); err != nil {
				return assistantFailClaim(ctx, tx, project, id, "sponsor больше не владелец проекта")
			}
		}
		if c.ConnectionID == nil || c.ModelID == nil {
			return assistantFailClaim(ctx, tx, project, id, "модель ассистента не настроена")
		}
		j.Model, err = assistantModelTx(ctx, tx, *c.ConnectionID, *c.ModelID)
		if err != nil {
			return assistantFailClaim(ctx, tx, project, id, "модель ассистента недоступна")
		}
		var raw []byte
		if err = tx.QueryRow(ctx, `SELECT snapshot,question FROM assistant_runs WHERE id=$1`, id).Scan(&raw, &j.Question); err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &j.Snapshot); err != nil {
			return err
		}
		// On restart do not repeat a model call whose acceptance/charge is unknown.
		if j.Run.Status == "running" {
			return assistantFailClaim(ctx, tx, project, id, "выполнение прервано; сохранённые действия сверены, новый запрос нужен явно")
		}
		if c.Limits.DailyCostUSD != nil && (j.Model.InputPrice == nil || j.Model.OutputPrice == nil) {
			return assistantFailClaim(ctx, tx, project, id, "неизвестная цена модели: cost cap не может быть проверен")
		}
		var used int64
		var cost float64
		if err = tx.QueryRow(ctx, `SELECT COALESCE(sum(reserved_tokens),0),COALESCE(sum(reserved_cost_usd),0) FROM assistant_runs WHERE project_id=$1 AND reserved_at>=date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`, project).Scan(&used, &cost); err != nil {
			return err
		}
		reserve := int64(c.Limits.MaxInputTokens + c.Limits.MaxOutputTokens)
		var reserveCost *float64
		if j.Model.InputPrice != nil && j.Model.OutputPrice != nil {
			v := (float64(c.Limits.MaxInputTokens)*float64(*j.Model.InputPrice) + float64(c.Limits.MaxOutputTokens)*float64(*j.Model.OutputPrice)) / 1e12
			reserveCost = &v
		}
		if used+reserve > c.Limits.DailyTokens || (c.Limits.DailyCostUSD != nil && reserveCost != nil && cost+*reserveCost > *c.Limits.DailyCostUSD) {
			_, err = tx.Exec(ctx, `UPDATE assistant_runs SET status='budget_exhausted',revision=revision+1,finished_at=now(),summary='дневной бюджет исчерпан' WHERE id=$1`, id)
			if err != nil {
				return err
			}
			return assistantEventTx(ctx, tx, project, id, "assistant.run.updated", "дневной бюджет исчерпан", nil)
		}
		err = tx.QueryRow(ctx, `INSERT INTO assistant_leases(project_id,epoch,run_id,expires_at) VALUES($1,1,$2,now()+interval '150 seconds') ON CONFLICT(project_id) DO UPDATE SET epoch=assistant_leases.epoch+1,run_id=$2,expires_at=now()+interval '150 seconds' WHERE assistant_leases.expires_at<now() RETURNING epoch`, project, id).Scan(&j.Epoch)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		j.Run, err = scanAssistantRun(tx.QueryRow(ctx, `UPDATE assistant_runs SET status='running',lease_epoch=$2,revision=revision+1,reserved_tokens=$3,reserved_cost_usd=$4,config_revision=$5,reserved_at=now() WHERE id=$1 RETURNING `+assistantRunCols, id, j.Epoch, reserve, reserveCost, c.Revision))
		if err != nil {
			return err
		}
		job = j
		return assistantEventTx(ctx, tx, project, id, "assistant.run.updated", "ассистент начал анализ", nil)
	})
	return job, err
}
func assistantFailClaim(ctx context.Context, tx pgx.Tx, project, id, why string) error {
	_, err := tx.Exec(ctx, `UPDATE assistant_runs SET status='failed',revision=revision+1,finished_at=now(),summary=$2 WHERE id=$1`, id, why)
	if err != nil {
		return err
	}
	return assistantEventTx(ctx, tx, project, id, "assistant.run.updated", why, nil)
}
func (s *Store) FinishAssistantJob(ctx context.Context, j AssistantJob, status string, d assistant.Diagnosis, usage assistant.Usage) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var active string
		if err := tx.QueryRow(ctx, `SELECT status FROM assistant_runs WHERE id=$1 AND lease_epoch=$2 FOR UPDATE`, j.Run.ID, j.Epoch).Scan(&active); err != nil {
			return nf(err)
		}
		if active == "stopped" {
			status = "stopped"
		}
		u, _ := json.Marshal(usage)
		un, _ := json.Marshal(d.Uncertainties)
		follow, _ := json.Marshal(d.Followups)
		if string(un) == "null" {
			un = []byte("[]")
		}
		if string(follow) == "null" {
			follow = []byte("[]")
		}
		_, err := tx.Exec(ctx, `UPDATE assistant_runs SET status=$2,revision=revision+1,finished_at=COALESCE(finished_at,now()),summary=$3,uncertainties=$4,followups=$5,usage=$6 WHERE id=$1`, j.Run.ID, status, assistant.Redact(d.Summary), un, follow, u)
		if err != nil {
			return err
		}
		refs, _ := json.Marshal(d.RefIDs)
		if string(refs) == "null" {
			refs = []byte("[]")
		}
		_, err = tx.Exec(ctx, `INSERT INTO assistant_messages(project_id,author,text,run_id,ref_ids) VALUES($1,'assistant',$2,$3,$4)`, j.Project, assistant.Redact(d.Summary), j.Run.ID, refs)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE assistant_leases SET expires_at=now() WHERE project_id=$1 AND run_id=$2 AND epoch=$3`, j.Project, j.Run.ID, j.Epoch)
		if err != nil {
			return err
		}
		return assistantEventTx(ctx, tx, j.Project, j.Run.ID, "assistant.run.updated", "анализ ассистента завершён", map[string]any{"status": status})
	})
}

// NotifyAssistantEvent persists a debounced cause. Only a bounded allowlist
// can schedule diagnosis; heartbeat/reconcile delivery is never a cause.
func (s *Store) NotifyAssistantEvent(ctx context.Context, project, kind, resource, revision string) error {
	switch kind {
	case "task.failed", "task.blocked", "epic.paused", "environment.paused":
	default:
		return nil
	}
	if !assistant.ValidID(resource) || revision == "" || len(revision) > 200 {
		return ErrInvalid
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		c, err := assistantConfigTx(ctx, tx, project, false)
		if err != nil || !c.Enabled || c.SponsorID == nil {
			return err
		}
		if _, err = assistantActorTx(ctx, tx, project, *c.SponsorID, true, true); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO assistant_event_causes(project_id,cause_key,resource_revision) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, project, kind+":"+resource, revision)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		key := "event:" + assistant.Hash([]string{kind, resource, revision})
		_, err = tx.Exec(ctx, `INSERT INTO assistant_event_pending(project_id,sponsor_user_id,intent_key,cause) VALUES($1,$2,$3,$4) ON CONFLICT(project_id) DO UPDATE SET sponsor_user_id=$2,intent_key=$3,cause=$4,revision=assistant_event_pending.revision+1`, project, *c.SponsorID, key, kind+" "+resource)
		return err
	})
}

// FlushAssistantEvents coalesces a burst into a fresh project snapshot. A
// failed queue insertion stays pending for the next bounded attempt.
func (s *Store) FlushAssistantEvents(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `SELECT project_id::text,sponsor_user_id::text,revision,intent_key,cause FROM assistant_event_pending WHERE not_before<=now() ORDER BY not_before LIMIT 10`)
	if err != nil {
		return err
	}
	type pending struct {
		project, actor string
		rev            int64
		key, cause     string
	}
	items := []pending{}
	for rows.Next() {
		var p pending
		if err = rows.Scan(&p.project, &p.actor, &p.rev, &p.key, &p.cause); err != nil {
			rows.Close()
			return err
		}
		items = append(items, p)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, p := range items {
		_, _, _, e := s.createAssistantRun(ctx, p.project, p.actor, p.key, p.cause, "event", "")
		var ae *assistant.Error
		if e == nil || errors.Is(e, ErrNotFound) || (errors.As(e, &ae) && (ae.Code == "sponsor_revoked" || ae.Code == "insufficient_role")) {
			if _, err = s.Pool.Exec(ctx, `DELETE FROM assistant_event_pending WHERE project_id=$1 AND revision=$2`, p.project, p.rev); err != nil {
				return err
			}
		} else {
			// Back off even for a persistently invalid configuration, without calling
			// the model and without erasing the outstanding significant cause.
			if _, err = s.Pool.Exec(ctx, `UPDATE assistant_event_pending SET not_before=now()+interval '60 seconds' WHERE project_id=$1 AND revision=$2`, p.project, p.rev); err != nil {
				return err
			}
		}
	}
	return nil
}

// AssistantJobActive is rechecked before a new provider call; stopping a run
// never cancels or changes any project job.
func (s *Store) AssistantJobActive(ctx context.Context, j AssistantJob) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = assistantActorTx(ctx, tx, j.Project, j.Run.InitiatorID, j.Run.Trigger == "event", false); err != nil {
		return false, err
	}
	c, err := assistantConfigTx(ctx, tx, j.Project, false)
	if err != nil {
		return false, err
	}
	if c.Revision != j.Config.Revision {
		return false, nil
	}
	if j.Run.Trigger == "event" && (!c.Enabled || c.SponsorID == nil || *c.SponsorID != j.Run.InitiatorID) {
		return false, nil
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM assistant_runs r JOIN assistant_leases l ON l.run_id=r.id AND l.project_id=r.project_id WHERE r.id=$1 AND r.status='running' AND l.epoch=$2 AND l.expires_at>now())`, j.Run.ID, j.Epoch).Scan(&active)
	return active, err
}
