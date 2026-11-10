package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

// Подключения к моделям (add-model-connections, спека
// backend/model-connections): CRUD, ключ и секретные заголовки через
// secretbox, список моделей с обнаружением и ручными записями, настройка
// модели планировщика.

// FieldError — ошибка валидации с указанием поля формы (API отдаёт 422 с
// field, консоль показывает у поля).
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Msg }
func (e *FieldError) Unwrap() error { return ErrInvalid }

// ErrInUse — на подключение ссылаются (планировщик, агенты): удалить нельзя.
type ErrInUse struct{ Refs []Ref }

// Ref — кто ссылается на подключение.
type Ref struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func (e *ErrInUse) Error() string { return "подключение используется" }

// ErrSecret — секрет в базе есть, но его не расшифровать (сменили ключ
// шифрования установки): подключение показывается как невалидное.
var ErrSecret = errors.New("секрет не расшифровать")

var connIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// storedHeader — заголовок в jsonb: секретное значение шифрованным.
type storedHeader struct {
	Name     string `json:"name"`
	Value    string `json:"value,omitempty"`
	ValueEnc []byte `json:"value_enc,omitempty"`
	Secret   bool   `json:"secret"`
}

// HeaderInput — заголовок из формы: nil Value у секретного оставляет прежнее.
type HeaderInput struct {
	Name   string
	Value  *string
	Secret bool
}

// ConnectionInput — что сохраняет администратор. Key: nil — не менять,
// пустая строка — удалить ключ.
type ConnectionInput struct {
	ID      string
	Name    string
	Kind    string
	API     string
	BaseURL string
	Key     *string
	Headers *[]HeaderInput
	Enabled *bool
}

const connCols = `c.id, c.name, c.kind, c.api, c.base_url, c.key_prefix, c.key_enc IS NOT NULL, c.headers, c.models, c.enabled, c.state, c.check_detail, c.checked_at, c.updated_at, u.login`

func scanConn(row pgx.Row) (domain.ModelConnection, error) {
	var c domain.ModelConnection
	var headers, models []byte
	err := row.Scan(&c.ID, &c.Name, &c.Kind, &c.API, &c.BaseURL, &c.KeyPrefix, &c.HasKey, &headers, &models,
		&c.Enabled, &c.State, &c.CheckDetail, &c.CheckedAt, &c.UpdatedAt, &c.UpdatedBy)
	if err != nil {
		return c, err
	}
	var sh []storedHeader
	_ = json.Unmarshal(headers, &sh)
	for _, h := range sh {
		out := domain.ConnHeader{Name: h.Name, Secret: h.Secret}
		if !h.Secret {
			out.Value = h.Value
		}
		c.Headers = append(c.Headers, out)
	}
	if c.Headers == nil {
		c.Headers = []domain.ConnHeader{}
	}
	_ = json.Unmarshal(models, &c.Models)
	if c.Models == nil {
		c.Models = []domain.ModelEntry{}
	}
	return c, nil
}

func validateConnection(in ConnectionInput) error {
	if !connIDRe.MatchString(in.ID) {
		return &FieldError{Field: "id", Msg: "идентификатор: латиница, цифры, дефис, до 63 символов"}
	}
	if strings.TrimSpace(in.Name) == "" {
		return &FieldError{Field: "name", Msg: "укажите название"}
	}
	switch in.Kind {
	case "vendor", "aggregator", "local":
	default:
		return &FieldError{Field: "kind", Msg: "вид: vendor, aggregator или local"}
	}
	switch in.API {
	case "anthropic", "openai":
	default:
		return &FieldError{Field: "api", Msg: "тип API: anthropic или openai"}
	}
	u, err := url.Parse(strings.TrimSpace(in.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return &FieldError{Field: "base_url", Msg: "base URL: адрес вида https://host[/path]"}
	}
	if in.Headers != nil {
		for i, h := range *in.Headers {
			if strings.TrimSpace(h.Name) == "" || strings.ContainsAny(h.Name, " :\r\n") {
				return &FieldError{Field: fmt.Sprintf("headers[%d].name", i), Msg: "имя заголовка без пробелов и двоеточий"}
			}
		}
	}
	return nil
}

// UpsertConnection создаёт или меняет подключение. Проверка ключа делается
// вызывающим после сохранения (SetConnectionCheck): сохранение не должно
// зависеть от доступности провайдера.
func (s *Store) UpsertConnection(ctx context.Context, in ConnectionInput, box *secretbox.Box, actorID, actorLogin string) (domain.ModelConnection, bool, error) {
	in.Name, in.BaseURL = strings.TrimSpace(in.Name), strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	if err := validateConnection(in); err != nil {
		return domain.ModelConnection{}, false, err
	}
	if in.Kind != "local" && in.Key != nil && *in.Key == "" {
		return domain.ModelConnection{}, false, &FieldError{Field: "key", Msg: "у вендора и агрегатора ключ обязателен"}
	}
	needsBox := in.Key != nil && *in.Key != ""
	if in.Headers != nil {
		for _, h := range *in.Headers {
			if h.Secret && h.Value != nil && *h.Value != "" {
				needsBox = true
			}
		}
	}
	if needsBox && !box.Enabled() {
		return domain.ModelConnection{}, false, secretbox.ErrNoKey
	}
	var out domain.ModelConnection
	created := false
	err := s.inCatalogTx(ctx, func(tx pgx.Tx) error {
		var oldHeaders []byte
		var exists, hasKey bool
		err := tx.QueryRow(ctx, `SELECT true, headers, key_enc IS NOT NULL FROM model_connections WHERE id=$1 FOR UPDATE`, in.ID).Scan(&exists, &oldHeaders, &hasKey)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if in.Kind != "local" && !hasKey && (in.Key == nil || *in.Key == "") {
			return &FieldError{Field: "key", Msg: "у вендора и агрегатора ключ обязателен"}
		}
		var old []storedHeader
		_ = json.Unmarshal(oldHeaders, &old)
		// Заголовки: секретные без нового значения берут прежнее шифрованное.
		headers := old
		if in.Headers != nil {
			headers = nil
			for _, h := range *in.Headers {
				sh := storedHeader{Name: h.Name, Secret: h.Secret}
				if h.Secret {
					if h.Value != nil && *h.Value != "" {
						enc, err := box.Seal(*h.Value)
						if err != nil {
							return err
						}
						sh.ValueEnc = enc
					} else {
						for _, o := range old {
							if o.Name == h.Name && o.Secret {
								sh.ValueEnc = o.ValueEnc
							}
						}
					}
				} else if h.Value != nil {
					sh.Value = *h.Value
				}
				headers = append(headers, sh)
			}
		}
		if headers == nil {
			headers = []storedHeader{}
		}
		hj, _ := json.Marshal(headers)
		if !exists {
			created = true
			enabled := true
			if in.Enabled != nil {
				enabled = *in.Enabled
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO model_connections (id, name, kind, api, base_url, headers, enabled, updated_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				in.ID, in.Name, in.Kind, in.API, in.BaseURL, hj, enabled, actorID); err != nil {
				return err
			}
		} else {
			// Выключение подключения, модели которого у агентов команды,
			// шаблонов или переопределений ролей, оставило бы их без модели.
			if in.Enabled != nil && !*in.Enabled {
				refs, err := connectionUseRefs(ctx, tx, in.ID, nil)
				if err != nil {
					return err
				}
				if len(refs) > 0 {
					return &ErrInUse{Refs: refs}
				}
			}
			if _, err := tx.Exec(ctx, `
				UPDATE model_connections SET name=$2, kind=$3, api=$4, base_url=$5, headers=$6,
					enabled=COALESCE($7, enabled), updated_at=now(), updated_by=$8 WHERE id=$1`,
				in.ID, in.Name, in.Kind, in.API, in.BaseURL, hj, in.Enabled, actorID); err != nil {
				return err
			}
		}
		keyReplaced := false
		if in.Key != nil {
			if *in.Key == "" {
				if _, err := tx.Exec(ctx, `UPDATE model_connections SET key_prefix='', key_enc=NULL, state='unchecked', check_detail='' WHERE id=$1`, in.ID); err != nil {
					return err
				}
			} else {
				enc, err := box.Seal(*in.Key)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE model_connections SET key_prefix=$2, key_enc=$3, state='unchecked', check_detail='' WHERE id=$1`,
					in.ID, tokenPrefix(*in.Key), enc); err != nil {
					return err
				}
				keyReplaced = true
			}
		}
		out, err = scanConn(tx.QueryRow(ctx, `SELECT `+connCols+` FROM model_connections c JOIN users u ON u.id=c.updated_by WHERE c.id=$1`, in.ID))
		if err != nil {
			return err
		}
		typ, text := "connection.updated", "изменено подключение "+in.ID
		if created {
			typ, text = "connection.created", "создано подключение "+in.ID
		}
		if _, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: typ, Text: text,
			Payload: map[string]any{"connection_id": in.ID, "api": in.API, "kind": in.Kind, "base_url": in.BaseURL, "enabled": out.Enabled},
		}); err != nil {
			return err
		}
		if keyReplaced {
			if _, err := appendEvent(ctx, tx, EventInput{
				ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "connection.key_replaced",
				Text:    "заменён ключ подключения " + in.ID,
				Payload: map[string]any{"connection_id": in.ID, "key_prefix": out.KeyPrefix},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	return out, created, err
}

func (s *Store) ListConnections(ctx context.Context) ([]domain.ModelConnection, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+connCols+` FROM model_connections c JOIN users u ON u.id=c.updated_by ORDER BY c.name, c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.ModelConnection{}
	for rows.Next() {
		c, err := scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) GetConnection(ctx context.Context, id string) (domain.ModelConnection, error) {
	c, err := scanConn(s.Pool.QueryRow(ctx, `SELECT `+connCols+` FROM model_connections c JOIN users u ON u.id=c.updated_by WHERE c.id=$1`, id))
	return c, nf(err)
}

// SetConnectionCheck записывает результат проверки подключения.
func (s *Store) SetConnectionCheck(ctx context.Context, id string, state domain.LLMProviderState, detail, actorLogin string) (domain.ModelConnection, error) {
	var out domain.ModelConnection
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = scanConn(tx.QueryRow(ctx, `
			WITH up AS (UPDATE model_connections SET state=$2, check_detail=$3, checked_at=now() WHERE id=$1 RETURNING *)
			SELECT `+connCols+` FROM up c JOIN users u ON u.id=c.updated_by`, id, state, detail))
		if err != nil {
			return nf(err)
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "connection.checked",
			Text:    "проверено подключение " + id + ": " + string(state),
			Payload: map[string]any{"connection_id": id, "state": state, "detail": detail},
		})
		return err
	})
	return out, err
}

// MarkConnectionRejected — провайдер отклонил ключ подключения при
// проверке control plane после отказа стадии (fix-agent-without-connection):
// состояние invalid, следующие стадии на этом подключении ждут нового
// ключа. Уже invalid не трогается. marked — этот вызов перевёл подключение
// в invalid: эскалация одна на перевод, даже если отказы пришли разом.
func (s *Store) MarkConnectionRejected(ctx context.Context, id, detail string) (marked bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE model_connections SET state='invalid', check_detail=$2, checked_at=now()
			WHERE id=$1 AND state <> 'invalid'`, id, detail)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		marked = true
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorSystem, Type: "connection.checked",
			Text:    "провайдер отклонил ключ подключения " + id + ": состояние invalid",
			Payload: map[string]any{"connection_id": id, "state": domain.LLMStateInvalid, "detail": detail},
		})
		return err
	})
	if err != nil {
		marked = false
	}
	return marked, err
}

// ConnectionRefs — кто ссылается на подключение (планировщик; агенты
// добавятся в add-agent-profiles).
func (s *Store) ConnectionRefs(ctx context.Context, id string) ([]Ref, error) {
	pm, ok, err := s.PlannerModel(ctx)
	if err != nil {
		return nil, err
	}
	var refs []Ref
	if ok && pm.ConnectionID == id {
		refs = append(refs, Ref{Kind: "planner", ID: "planner_model"})
	}
	agents, err := agentRefsForConnection(ctx, s.Pool, id)
	if err != nil {
		return nil, err
	}
	team, err := connectionUseRefs(ctx, s.Pool, id, nil)
	if err != nil {
		return nil, err
	}
	return append(append(refs, agents...), team...), nil
}

func (s *Store) DeleteConnection(ctx context.Context, id, actorLogin string) error {
	err := s.inCatalogTx(ctx, func(tx pgx.Tx) error {
		// Ссылки читаются под общей блокировкой настройки планировщика:
		// параллельный выбор этого подключения не проскочит между проверкой и
		// удалением, а порядок захвата один у всех писателей (нет deadlock).
		if err := lockPlanner(ctx, tx); err != nil {
			return err
		}
		// Строка подключения под блокировкой: параллельная привязка профиля
		// (FOR SHARE в checkBinding) дождётся исхода удаления.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM model_connections WHERE id=$1 FOR UPDATE`, id); err != nil {
			return err
		}
		pm, ok, err := plannerModelTx(ctx, tx, false)
		if err != nil {
			return err
		}
		var refs []Ref
		if ok && pm.ConnectionID == id {
			refs = append(refs, Ref{Kind: "planner", ID: "planner_model"})
		}
		agents, err := agentRefsForConnection(ctx, tx, id)
		if err != nil {
			return err
		}
		team, err := connectionUseRefs(ctx, tx, id, nil)
		if err != nil {
			return err
		}
		if refs = append(append(refs, agents...), team...); len(refs) > 0 {
			return &ErrInUse{Refs: refs}
		}
		tag, err := tx.Exec(ctx, `DELETE FROM model_connections WHERE id=$1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "connection.deleted",
			Text: "удалено подключение " + id, Payload: map[string]any{"connection_id": id},
		})
		return err
	})
	return err
}

// MergeDiscoveredModels сливает обнаруженный список с текущим: новые как
// discovered, пропавшие среди discovered помечаются missing, цены, скрытие
// и ручные записи сохраняются (design, решение 4).
func (s *Store) MergeDiscoveredModels(ctx context.Context, id string, found []llm.Model, actorLogin string) (domain.ModelConnection, []string, []string, error) {
	var out domain.ModelConnection
	var added, missing []string
	err := s.inCatalogTx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT models FROM model_connections WHERE id=$1 FOR UPDATE`, id).Scan(&raw); err != nil {
			return nf(err)
		}
		var cur []domain.ModelEntry
		_ = json.Unmarshal(raw, &cur)
		seen := map[string]bool{}
		for _, m := range found {
			seen[m.ID] = true
		}
		next := make([]domain.ModelEntry, 0, len(cur)+len(found))
		have := map[string]bool{}
		for _, m := range cur {
			have[m.ID] = true
			if m.Source == ModelSourceDiscovered {
				wasMissing := m.Missing
				m.Missing = !seen[m.ID]
				if m.Missing && !wasMissing {
					missing = append(missing, m.ID)
				}
			}
			next = append(next, m)
		}
		for _, f := range found {
			if have[f.ID] {
				continue
			}
			label := f.Label
			if label == "" {
				label = f.ID
			}
			next = append(next, domain.ModelEntry{ID: f.ID, Label: label, Source: ModelSourceDiscovered})
			added = append(added, f.ID)
		}
		mj, _ := json.Marshal(next)
		var err error
		out, err = scanConn(tx.QueryRow(ctx, `
			WITH up AS (UPDATE model_connections SET models=$2, updated_at=now() WHERE id=$1 RETURNING *)
			SELECT `+connCols+` FROM up c JOIN users u ON u.id=c.updated_by`, id, mj))
		if err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "connection.discovered",
			Text:    fmt.Sprintf("обновлён список моделей подключения %s: +%d, пропало %d", id, len(added), len(missing)),
			Payload: map[string]any{"connection_id": id, "added": added, "missing": missing},
		})
		return err
	})
	return out, added, missing, err
}

// Источники моделей подключения.
const (
	ModelSourceDiscovered = "discovered"
	ModelSourceManual     = "manual"
	ModelSourcePreset     = "preset"
)

// ApplyPresetModels дополняет список моделей подключения данными пресета
// (add-native-agent, спека model-connections «Пресеты провайдеров»): у
// моделей списка с тем же идентификатором пустые окно контекста, пределы
// ответа и цены берутся из пресета, заданные значения не меняются.
// addMissing — модели пресета, которых в списке нет, добавляются с
// источником preset (провайдер списка не отдал).
func (s *Store) ApplyPresetModels(ctx context.Context, id string, preset []domain.ModelEntry, addMissing bool, actorLogin string) (domain.ModelConnection, error) {
	var out domain.ModelConnection
	var added []string
	changed := false
	err := s.inCatalogTx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT models FROM model_connections WHERE id=$1 FOR UPDATE`, id).Scan(&raw); err != nil {
			return nf(err)
		}
		var cur []domain.ModelEntry
		_ = json.Unmarshal(raw, &cur)
		idx := map[string]int{}
		for i, m := range cur {
			idx[m.ID] = i
		}
		fill := func(dst **int64, v *int64) {
			if *dst == nil && v != nil {
				x := *v
				*dst = &x
				changed = true
			}
		}
		for _, p := range preset {
			if i, ok := idx[p.ID]; ok {
				e := &cur[i]
				fill(&e.ContextWindow, p.ContextWindow)
				fill(&e.MaxOutputTokens, p.MaxOutputTokens)
				fill(&e.ModelMaxOutput, p.ModelMaxOutput)
				fill(&e.InputPrice, p.InputPrice)
				fill(&e.OutputPrice, p.OutputPrice)
				fill(&e.CachedInputPrice, p.CachedInputPrice)
				continue
			}
			if !addMissing {
				continue
			}
			if p.Label == "" {
				p.Label = p.ID
			}
			p.Source, p.Hidden, p.Missing = ModelSourcePreset, false, false
			idx[p.ID] = len(cur)
			cur = append(cur, p)
			added = append(added, p.ID)
			changed = true
		}
		if !changed {
			var err error
			out, err = scanConn(tx.QueryRow(ctx, `SELECT `+connCols+` FROM model_connections c JOIN users u ON u.id=c.updated_by WHERE c.id=$1`, id))
			return err
		}
		mj, _ := json.Marshal(cur)
		var err error
		out, err = scanConn(tx.QueryRow(ctx, `
			WITH up AS (UPDATE model_connections SET models=$2, updated_at=now() WHERE id=$1 RETURNING *)
			SELECT `+connCols+` FROM up c JOIN users u ON u.id=c.updated_by`, id, mj))
		if err != nil {
			return err
		}
		if added == nil {
			added = []string{}
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "connection.updated",
			Text:    fmt.Sprintf("данные пресета в списке моделей подключения %s: добавлено %d", id, len(added)),
			Payload: map[string]any{"connection_id": id, "preset_models": added},
		})
		return err
	})
	return out, err
}

// SetConnectionModels — ручные правки списка: обнаруженные записи меняют
// только подпись, скрытие, цены, окно и пределы ответа; ручные и записи
// пресета заменяются и удаляются целиком.
func (s *Store) SetConnectionModels(ctx context.Context, id string, models []domain.ModelEntry, actorID, actorLogin string) (domain.ModelConnection, error) {
	ids := map[string]bool{}
	for i := range models {
		models[i].ID = strings.TrimSpace(models[i].ID)
	}
	for i, m := range models {
		if m.ID == "" {
			return domain.ModelConnection{}, &FieldError{Field: fmt.Sprintf("models[%d].id", i), Msg: "укажите идентификатор модели"}
		}
		if ids[m.ID] {
			return domain.ModelConnection{}, &FieldError{Field: fmt.Sprintf("models[%d].id", i), Msg: "модель повторяется"}
		}
		ids[m.ID] = true
		for _, pr := range []struct {
			v *int64
			f string
		}{{m.InputPrice, "input_price"}, {m.OutputPrice, "output_price"}, {m.ContextWindow, "context_window"},
			{m.CachedInputPrice, "cached_input_price"}, {m.MaxOutputTokens, "max_output_tokens"}, {m.ModelMaxOutput, "model_max_output"}} {
			if pr.v != nil && *pr.v < 0 {
				return domain.ModelConnection{}, &FieldError{Field: fmt.Sprintf("models[%d].%s", i, pr.f), Msg: "значение не может быть отрицательным"}
			}
		}
		if m.MaxOutputTokens != nil && m.ModelMaxOutput != nil && *m.ModelMaxOutput > 0 && *m.MaxOutputTokens > *m.ModelMaxOutput {
			return domain.ModelConnection{}, &FieldError{Field: fmt.Sprintf("models[%d].max_output_tokens", i), Msg: "предел ответа на ход больше предельного выхода модели"}
		}
		// Ответ модели идёт в то же окно, что и запрос: предел ответа не
		// меньше окна переполнял бы каждый ход, а предельный выход — повтор
		// обрезанного хода.
		if win := m.ContextWindow; win != nil && *win > 0 {
			if v := m.MaxOutputTokens; v != nil && *v >= *win {
				return domain.ModelConnection{}, &FieldError{Field: fmt.Sprintf("models[%d].max_output_tokens", i), Msg: "предел ответа на ход должен быть меньше окна контекста"}
			}
			if v := m.ModelMaxOutput; v != nil && *v >= *win {
				return domain.ModelConnection{}, &FieldError{Field: fmt.Sprintf("models[%d].model_max_output", i), Msg: "предельный выход модели должен быть меньше окна контекста"}
			}
		}
	}
	var out domain.ModelConnection
	err := s.inCatalogTx(ctx, func(tx pgx.Tx) error {
		if err := lockPlanner(ctx, tx); err != nil {
			return err
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT models FROM model_connections WHERE id=$1 FOR UPDATE`, id).Scan(&raw); err != nil {
			return nf(err)
		}
		var cur []domain.ModelEntry
		_ = json.Unmarshal(raw, &cur)
		curBy := map[string]domain.ModelEntry{}
		for _, m := range cur {
			curBy[m.ID] = m
		}
		next := make([]domain.ModelEntry, 0, len(models))
		for _, m := range models {
			if old, ok := curBy[m.ID]; ok && old.Source == ModelSourceDiscovered {
				old.Label, old.Hidden = m.Label, m.Hidden
				old.InputPrice, old.OutputPrice, old.ContextWindow = m.InputPrice, m.OutputPrice, m.ContextWindow
				old.CachedInputPrice, old.MaxOutputTokens, old.ModelMaxOutput = m.CachedInputPrice, m.MaxOutputTokens, m.ModelMaxOutput
				if old.Label == "" {
					old.Label = old.ID
				}
				next = append(next, old)
				continue
			}
			if m.Label == "" {
				m.Label = m.ID
			}
			// Модель пресета остаётся моделью пресета и после ручной правки
			// (спека model-connections «Пресеты провайдеров»).
			m.Source, m.Missing = ModelSourceManual, false
			if old, ok := curBy[m.ID]; ok && old.Source == ModelSourcePreset {
				m.Source = ModelSourcePreset
			}
			next = append(next, m)
		}
		// Модель планировщика нельзя убрать или скрыть: сначала сменить её.
		if pm, ok, err := plannerModelTx(ctx, tx, false); err != nil {
			return err
		} else if ok && pm.ConnectionID == id {
			kept := false
			for i, m := range next {
				if m.ID == pm.Model && !m.Hidden {
					kept = true
				} else if m.ID == pm.Model {
					return &FieldError{Field: fmt.Sprintf("models[%d].hidden", i), Msg: "модель выбрана для декомпозиции: сначала смените её"}
				}
			}
			if !kept {
				return &FieldError{Field: "models", Msg: "модель " + pm.Model + " выбрана для декомпозиции: сначала смените её"}
			}
		}
		// Модели агентов команды, шаблонов и переопределений ролей тоже нельзя
		// убрать или скрыть: агент перестал бы назначаться.
		gone := map[string]bool{}
		for _, m := range cur {
			gone[m.ID] = true
		}
		for _, m := range next {
			if !m.Hidden {
				delete(gone, m.ID)
			}
		}
		if len(gone) > 0 {
			refs, err := connectionUseRefs(ctx, tx, id, gone)
			if err != nil {
				return err
			}
			if len(refs) > 0 {
				return &ErrInUse{Refs: refs}
			}
		}
		mj, _ := json.Marshal(next)
		var err error
		out, err = scanConn(tx.QueryRow(ctx, `
			WITH up AS (UPDATE model_connections SET models=$2, updated_at=now(), updated_by=$3 WHERE id=$1 RETURNING *)
			SELECT `+connCols+` FROM up c JOIN users u ON u.id=c.updated_by`, id, mj, actorID))
		if err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "connection.updated",
			Text:    "изменён список моделей подключения " + id,
			Payload: map[string]any{"connection_id": id, "models": len(next)},
		})
		return err
	})
	return out, err
}

// ConnectionClient — клиент провайдера с расшифрованным ключом и секретными
// заголовками; ErrNotFound без подключения, ошибка secretbox при смене ключа
// шифрования.
func (s *Store) ConnectionClient(ctx context.Context, id string, box *secretbox.Box) (llm.Client, domain.ModelConnection, error) {
	c, err := s.GetConnection(ctx, id)
	if err != nil {
		return llm.Client{}, c, err
	}
	var keyEnc, headersRaw []byte
	if err := s.Pool.QueryRow(ctx, `SELECT key_enc, headers FROM model_connections WHERE id=$1`, id).Scan(&keyEnc, &headersRaw); err != nil {
		return llm.Client{}, c, nf(err)
	}
	cl := llm.Client{API: llm.API(c.API), BaseURL: c.BaseURL, Headers: map[string]string{}}
	if keyEnc != nil {
		key, err := box.Open(keyEnc)
		if err != nil {
			return llm.Client{}, c, fmt.Errorf("%w: %v", ErrSecret, err)
		}
		cl.Key = key
	}
	var sh []storedHeader
	_ = json.Unmarshal(headersRaw, &sh)
	for _, h := range sh {
		if h.Secret {
			if h.ValueEnc == nil {
				continue
			}
			v, err := box.Open(h.ValueEnc)
			if err != nil {
				return llm.Client{}, c, fmt.Errorf("%w: %v", ErrSecret, err)
			}
			cl.Headers[h.Name] = v
		} else {
			cl.Headers[h.Name] = h.Value
		}
	}
	return cl, c, nil
}

// ─── настройки установки ─────────────────────────────────────────────────

// PlannerModel — выбранная модель декомпозиции; ok=false, если не выбрана.
func (s *Store) PlannerModel(ctx context.Context) (domain.PlannerModel, bool, error) {
	return plannerModelTx(ctx, s.Pool, false)
}

type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// lockPlanner — транзакционная advisory-блокировка настройки планировщика:
// один порядок захвата у удаления подключения, правки списка моделей и
// выбора модели, независимо от того, есть ли строка настройки.
func lockPlanner(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('planner_model'))`)
	return err
}

// plannerModelTx читает настройку; lock — с блокировкой строки внутри транзакции.
func plannerModelTx(ctx context.Context, q querier, lock bool) (domain.PlannerModel, bool, error) {
	sql := `SELECT value FROM installation_settings WHERE key='planner_model'`
	if lock {
		sql += ` FOR UPDATE`
	}
	var raw []byte
	err := q.QueryRow(ctx, sql).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PlannerModel{}, false, nil
	}
	if err != nil {
		return domain.PlannerModel{}, false, err
	}
	var pm domain.PlannerModel
	if err := json.Unmarshal(raw, &pm); err != nil || pm.ConnectionID == "" || pm.Model == "" {
		return domain.PlannerModel{}, false, nil
	}
	return pm, true, nil
}

// SetPlannerModel выбирает модель декомпозиции; nil — сброс на окружение.
// Модель должна быть в списке включённого подключения и не скрыта.
func (s *Store) SetPlannerModel(ctx context.Context, pm *domain.PlannerModel, actorID, actorLogin string) error {
	if pm != nil && (pm.ConnectionID == "" || pm.Model == "") {
		return &FieldError{Field: "model", Msg: "укажите подключение и модель"}
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockPlanner(ctx, tx); err != nil {
			return err
		}
		if pm != nil {
			// Строка подключения под разделяемой блокировкой: параллельная
			// правка списка или удаление дождётся записи настройки.
			var raw []byte
			var enabled bool
			err := tx.QueryRow(ctx, `SELECT models, enabled FROM model_connections WHERE id=$1 FOR SHARE`, pm.ConnectionID).Scan(&raw, &enabled)
			if errors.Is(err, pgx.ErrNoRows) {
				return &FieldError{Field: "connection_id", Msg: "подключение не найдено"}
			}
			if err != nil {
				return err
			}
			if !enabled {
				return &FieldError{Field: "connection_id", Msg: "подключение отключено"}
			}
			var models []domain.ModelEntry
			_ = json.Unmarshal(raw, &models)
			found := false
			for _, m := range models {
				if m.ID == pm.Model && !m.Hidden && !m.Missing {
					found = true
				}
			}
			if !found {
				return &FieldError{Field: "model", Msg: "модели нет в списке подключения"}
			}
		}
		payload := map[string]any{}
		if pm == nil {
			if _, err := tx.Exec(ctx, `DELETE FROM installation_settings WHERE key='planner_model'`); err != nil {
				return err
			}
		} else {
			v, _ := json.Marshal(pm)
			if _, err := tx.Exec(ctx, `
				INSERT INTO installation_settings (key, value, updated_by) VALUES ('planner_model', $1, $2)
				ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=now(), updated_by=EXCLUDED.updated_by`, v, actorID); err != nil {
				return err
			}
			payload["connection_id"], payload["model"] = pm.ConnectionID, pm.Model
		}
		text := "модель декомпозиции сброшена на окружение установки"
		if pm != nil {
			text = "модель декомпозиции: " + pm.ConnectionID + "/" + pm.Model
		}
		if _, err := appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "planner.model_changed", Text: text, Payload: payload}); err != nil {
			return err
		}
		return plannerRoleEvents(ctx, tx, actorLogin, payload)
	})
}

// plannerRoleEvents — модель планировщика — модель Тимлида (add-agent-roles):
// шаблон lead и агенты-тимлиды тоже изменились, консоль перечитывает их по
// role.updated и team.agent_updated.
func plannerRoleEvents(ctx context.Context, tx pgx.Tx, actorLogin string, model map[string]any) error {
	// Версии шаблона и агентов-тимлидов сдвигаются: вкладка с устаревшей
	// формой получит 409 stale, а не откатит модель.
	if _, err := tx.Exec(ctx, `UPDATE agent_roles SET updated_at=now() WHERE id=$1`, domain.RoleLead); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE team_agents SET updated_at=now() WHERE role_id=$1`, domain.RoleLead); err != nil {
		return err
	}
	if _, err := appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "role.updated",
		Text: "изменена модель Тимлида", Payload: map[string]any{"id": domain.RoleLead, "planner_model": model}}); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, name FROM team_agents WHERE role_id=$1 ORDER BY position, id`, domain.RoleLead)
	if err != nil {
		return err
	}
	type agent struct{ id, name string }
	var leads []agent
	for rows.Next() {
		var a agent
		if err := rows.Scan(&a.id, &a.name); err != nil {
			rows.Close()
			return err
		}
		leads = append(leads, a)
	}
	rows.Close()
	for _, a := range leads {
		if _, err := appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "team.agent_updated",
			Text: "изменена модель агента «" + a.name + "»", Payload: map[string]any{"id": a.id, "name": a.name, "role": domain.RoleLead, "planner_model": model}}); err != nil {
			return err
		}
	}
	return nil
}

// inCatalogTx — транзакция правки каталога подключений: модели runner'ов
// агента Rivet пересчитываются в ней же (ревью add-native-agent P3-7).
// Отдельная транзакция после фиксации давала 500 на уже сохранённой правке,
// если пересчёт падал.
func (s *Store) inCatalogTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return syncRivetRunners(ctx, tx)
	})
}

// syncRivetRunners — модели runner'ов агента Rivet равны моделям каталога
// подключений: после правки каталога они обновляются без перезапуска
// runner'ов (спека runners «Регистрация runner'а»).
func syncRivetRunners(ctx context.Context, tx pgx.Tx) error {
	a, err := scanAgent(tx.QueryRow(ctx, `SELECT `+agentCols+` FROM agents a LEFT JOIN users u ON u.id=a.updated_by WHERE a.id=$1`, AgentRivet))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := fillCatalogModels(ctx, tx, &a); err != nil {
		return err
	}
	return syncRunnersForAgent(ctx, tx, a)
}
