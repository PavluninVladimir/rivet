package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/llm"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

// Каталог агентов (add-agent-profiles, спека backend/agents): после
// add-native-agent в нём один профиль rivet с адаптером native. Его модели —
// все видимые модели включённых подключений, назначение несёт endpoint
// модели с секретами по режиму профиля.

var agentIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// AgentInput — что сохраняет администратор.
type AgentInput struct {
	ID           string
	Name         string
	Adapter      string
	Command      string
	Capabilities []string
	Models       []domain.AgentModelRef
	DefaultModel *domain.AgentModelRef
	Env          []domain.EnvVar
	Args         []string
	Secrets      string
	Enabled      *bool
	Limits       *domain.AgentLimits
	// KeepName, KeepDefaultModel, KeepLimits — поле не передано в запросе:
	// у существующего профиля сохраняется прежнее значение, у нового
	// действует Name, DefaultModel и Limits (умолчания). Без флага поле
	// заменяется, nil DefaultModel — автоматический выбор, nil или пустые
	// Limits — умолчания цикла.
	KeepName, KeepDefaultModel, KeepLimits bool
}

// AdapterNative — адаптер агента Rivet: единственный в каталоге
// (add-native-agent). AgentRivet — его профиль.
const (
	AdapterNative = "native"
	AgentRivet    = "rivet"
)

const agentCols = `a.id, a.name, a.adapter, a.command, a.capabilities, a.models, a.default_model, a.env, a.args, a.secrets, a.enabled, a.preset, a.updated_at, COALESCE(u.login, ''),
	(SELECT count(*) FROM runners r WHERE r.agent = a.id), a.limits`

func scanAgent(row pgx.Row) (domain.AgentProfile, error) {
	var a domain.AgentProfile
	var models, def, env, args, limits []byte
	err := row.Scan(&a.ID, &a.Name, &a.Adapter, &a.Command, &a.Capabilities, &models, &def, &env, &args,
		&a.Secrets, &a.Enabled, &a.Preset, &a.UpdatedAt, &a.UpdatedBy, &a.Runners, &limits)
	if err != nil {
		return a, err
	}
	_ = json.Unmarshal(limits, &a.Limits)
	_ = json.Unmarshal(models, &a.Models)
	if a.Models == nil {
		a.Models = []domain.AgentModelRef{}
	}
	if def != nil {
		var d domain.AgentModelRef
		if json.Unmarshal(def, &d) == nil && d.Model != "" {
			a.DefaultModel = &d
		}
	}
	_ = json.Unmarshal(env, &a.Env)
	if a.Env == nil {
		a.Env = []domain.EnvVar{}
	}
	_ = json.Unmarshal(args, &a.Args)
	if a.Args == nil {
		a.Args = []string{}
	}
	if a.Capabilities == nil {
		a.Capabilities = []string{}
	}
	return a, nil
}

func validateAgent(in AgentInput) error {
	if !agentIDRe.MatchString(in.ID) {
		return &FieldError{Field: "id", Msg: "идентификатор: латиница, цифры, дефис, до 63 символов"}
	}
	if strings.TrimSpace(in.Name) == "" {
		return &FieldError{Field: "name", Msg: "укажите название"}
	}
	if in.ID != AgentRivet || in.Adapter != AdapterNative {
		return &FieldError{Field: "adapter", Msg: "агентом может быть только агент Rivet (адаптер native)"}
	}
	if in.Command != "" || len(in.Env) > 0 || len(in.Args) > 0 {
		return &FieldError{Field: "command", Msg: "у агента Rivet нет команды, окружения и аргументов CLI"}
	}
	if l := in.Limits; l != nil && (l.MaxTurns < 0 || l.MaxTokens < 0 || l.MaxMinutes < 0) {
		return &FieldError{Field: "limits", Msg: "лимиты не могут быть отрицательными"}
	}
	switch in.Secrets {
	case "never", "secure", "always":
	default:
		return &FieldError{Field: "secrets", Msg: "режим секретов: never, secure или always"}
	}
	for i, c := range in.Capabilities {
		if strings.TrimSpace(c) == "" || strings.ContainsAny(c, " \t\r\n") {
			return &FieldError{Field: fmt.Sprintf("capabilities[%d]", i), Msg: "некорректная capability"}
		}
	}
	return nil
}

// UpsertAgent создаёт или меняет профиль: модель по умолчанию проверяется
// по каталогу подключений, runner'ы агента получают модели каталога.
// Непереданный режим секретов у существующего профиля сохраняется (поле
// безопасности не сбрасывается молча), у нового — secure. Так же
// сохраняются название, модель по умолчанию и лимиты с флагами Keep*.
func (s *Store) UpsertAgent(ctx context.Context, in AgentInput, actorID, actorLogin string) (domain.AgentProfile, bool, error) {
	in.Name, in.Command = strings.TrimSpace(in.Name), strings.TrimSpace(in.Command)
	keepSecrets := in.Secrets == ""
	if keepSecrets {
		in.Secrets = "secure"
	}
	if err := validateAgent(in); err != nil {
		return domain.AgentProfile{}, false, err
	}
	var out domain.AgentProfile
	created := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Модели агента Rivet вычисляются из каталога подключений: свои
		// привязки не хранятся, модель по умолчанию — одна из моделей
		// каталога.
		catalog, err := catalogModels(ctx, tx)
		if err != nil {
			return err
		}
		in.Models = []domain.AgentModelRef{}
		seen := map[string]bool{}
		for _, m := range catalog {
			seen[m.ConnectionID+"/"+m.Model] = true
		}
		if in.DefaultModel != nil && !seen[in.DefaultModel.ConnectionID+"/"+in.DefaultModel.Model] {
			return &FieldError{Field: "default_model", Msg: "модель по умолчанию должна быть моделью включённого подключения"}
		}
		var cur string
		if err := tx.QueryRow(ctx, `SELECT secrets FROM agents WHERE id=$1 FOR UPDATE`, in.ID).Scan(&cur); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		exists := cur != ""
		if keepSecrets && exists {
			in.Secrets = cur
		}
		if in.Env == nil {
			in.Env = []domain.EnvVar{}
		}
		if in.Args == nil {
			in.Args = []string{}
		}
		if in.Capabilities == nil {
			in.Capabilities = []string{}
		}
		// Профиль, на который ссылаются агенты команды, шаблоны или
		// переопределения ролей, нельзя выключить, а их модели — отвязать.
		if exists {
			// Включённый профиль сохраняет все модели каталога: ссылками
			// мешают только модели вне его.
			var keep map[string]bool
			if in.Enabled == nil || *in.Enabled {
				keep = map[string]bool{}
				for _, m := range catalog {
					keep[m.ConnectionID+"/"+m.Model] = true
				}
			}
			refs, err := agentUseRefs(ctx, tx, in.ID, keep)
			if err != nil {
				return err
			}
			if len(refs) > 0 {
				return &ErrInUse{Refs: refs}
			}
		}
		mj, _ := json.Marshal(in.Models)
		limits := domain.AgentLimits{}
		if in.Limits != nil {
			limits = *in.Limits
		}
		lj, _ := json.Marshal(limits)
		ej, _ := json.Marshal(in.Env)
		aj, _ := json.Marshal(in.Args)
		var dj []byte
		if in.DefaultModel != nil {
			dj, _ = json.Marshal(in.DefaultModel)
		}
		if !exists {
			created = true
			enabled := true
			if in.Enabled != nil {
				enabled = *in.Enabled
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO agents (id, name, adapter, command, capabilities, models, default_model, env, args, secrets, enabled, updated_by, limits)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
				in.ID, in.Name, in.Adapter, in.Command, in.Capabilities, mj, dj, ej, aj, in.Secrets, enabled, actorID, lj); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `
				UPDATE agents SET name=CASE WHEN $14 THEN name ELSE $2 END, adapter=$3, command=$4, capabilities=$5, models=$6,
					default_model=CASE WHEN $15 THEN default_model ELSE $7 END, env=$8, args=$9,
					secrets=$10, enabled=COALESCE($11, enabled), updated_at=now(), updated_by=$12,
					limits=CASE WHEN $16 THEN limits ELSE $13 END WHERE id=$1`,
			in.ID, in.Name, in.Adapter, in.Command, in.Capabilities, mj, dj, ej, aj, in.Secrets, in.Enabled, actorID, lj,
			in.KeepName, in.KeepDefaultModel, in.KeepLimits); err != nil {
			return err
		}
		out, err = scanAgent(tx.QueryRow(ctx, `SELECT `+agentCols+` FROM agents a LEFT JOIN users u ON u.id=a.updated_by WHERE a.id=$1`, in.ID))
		if err != nil {
			return err
		}
		if err := fillCatalogModels(ctx, tx, &out); err != nil {
			return err
		}
		if err := syncRunnersForAgent(ctx, tx, out); err != nil {
			return err
		}
		typ, text := "agent.updated", "изменён профиль агента "+in.ID
		if created {
			typ, text = "agent.created", "создан профиль агента "+in.ID
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: typ, Text: text,
			Payload: map[string]any{"agent_id": in.ID, "adapter": in.Adapter, "models": len(in.Models), "secrets": in.Secrets, "enabled": out.Enabled},
		})
		return err
	})
	return out, created, err
}

// checkBinding — привязка ссылается на включённое подключение и видимую модель.
func checkBinding(ctx context.Context, tx pgx.Tx, m domain.AgentModelRef, field string) error {
	if m.ConnectionID == "" || m.Model == "" {
		return &FieldError{Field: field, Msg: "укажите подключение и модель"}
	}
	var raw []byte
	var enabled bool
	err := tx.QueryRow(ctx, `SELECT models, enabled FROM model_connections WHERE id=$1 FOR SHARE`, m.ConnectionID).Scan(&raw, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return &FieldError{Field: field, Msg: "подключение " + m.ConnectionID + " не найдено"}
	}
	if err != nil {
		return err
	}
	if !enabled {
		return &FieldError{Field: field, Msg: "подключение " + m.ConnectionID + " отключено"}
	}
	var models []domain.ModelEntry
	_ = json.Unmarshal(raw, &models)
	for _, e := range models {
		if e.ID == m.Model && !e.Hidden && !e.Missing {
			return nil
		}
	}
	return &FieldError{Field: field, Msg: "модели " + m.Model + " нет в списке подключения " + m.ConnectionID}
}

// bindingModels — идентификаторы моделей привязок без повторов, по порядку.
func bindingModels(a domain.AgentProfile) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range a.Models {
		if !seen[m.Model] && !m.Unavailable {
			seen[m.Model] = true
			out = append(out, m.Model)
		}
	}
	return out
}

// syncRunnersForAgent — runner'ы с агентом профиля (или один runner):
// включённый профиль задаёт модели из привязок (если они есть) и добавляет
// свои capabilities к объявленным; отключённый возвращает объявленное.
func syncRunnersForAgent(ctx context.Context, tx pgx.Tx, a domain.AgentProfile, onlyRunner ...string) error {
	where, args := `agent=$1`, []any{a.ID}
	if len(onlyRunner) > 0 {
		where, args = `id=$2 AND agent=$1`, []any{a.ID, onlyRunner[0]}
	}
	if !a.Enabled {
		_, err := tx.Exec(ctx, `UPDATE runners SET catalog=false, models=declared_models, model=COALESCE(declared_models[1], ''),
			capabilities=declared_capabilities WHERE `+where, args...)
		return err
	}
	caps := a.Capabilities
	if caps == nil {
		caps = []string{}
	}
	models := bindingModels(a)
	if len(models) == 0 {
		_, err := tx.Exec(ctx, `UPDATE runners SET catalog=true, models=declared_models, model=COALESCE(declared_models[1], ''),
			capabilities=(SELECT COALESCE(array_agg(DISTINCT c), '{}') FROM unnest(declared_capabilities || $`+fmt.Sprint(len(args)+1)+`::text[]) c) WHERE `+where, append(args, caps)...)
		return err
	}
	n := len(args)
	_, err := tx.Exec(ctx, `UPDATE runners SET catalog=true, models=$`+fmt.Sprint(n+1)+`, model=$`+fmt.Sprint(n+2)+`,
		capabilities=(SELECT COALESCE(array_agg(DISTINCT c), '{}') FROM unnest(declared_capabilities || $`+fmt.Sprint(n+3)+`::text[]) c) WHERE `+where,
		append(args, models, models[0], caps)...)
	return err
}

func (s *Store) ListAgents(ctx context.Context) ([]domain.AgentProfile, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+agentCols+` FROM agents a LEFT JOIN users u ON u.id=a.updated_by ORDER BY a.preset DESC, a.name, a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AgentProfile{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if err := fillCatalogModels(ctx, s.Pool, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) GetAgent(ctx context.Context, id string) (domain.AgentProfile, error) {
	a, err := scanAgent(s.Pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents a LEFT JOIN users u ON u.id=a.updated_by WHERE a.id=$1`, id))
	if err != nil {
		return a, nf(err)
	}
	return a, fillCatalogModels(ctx, s.Pool, &a)
}

// rowsQuerier — пул или транзакция.
type rowsQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// catalogModels — видимые модели включённых подключений (не скрытые и не
// пропавшие), по порядку подключений и их списков.
func catalogModels(ctx context.Context, q rowsQuerier) ([]domain.AgentModelRef, error) {
	rows, err := q.Query(ctx, `SELECT id, models FROM model_connections WHERE enabled ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AgentModelRef{}
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var models []domain.ModelEntry
		_ = json.Unmarshal(raw, &models)
		for _, m := range models {
			if !m.Hidden && !m.Missing {
				out = append(out, domain.AgentModelRef{ConnectionID: id, Model: m.ID})
			}
		}
	}
	return out, rows.Err()
}

// fillCatalogModels — у агента Rivet модели вычисляются из каталога
// подключений; модель по умолчанию, пропавшая из каталога, сбрасывается к
// первой доступной.
func fillCatalogModels(ctx context.Context, q rowsQuerier, a *domain.AgentProfile) error {
	if a.Adapter != AdapterNative {
		return nil
	}
	models, err := catalogModels(ctx, q)
	if err != nil {
		return err
	}
	a.Models = models
	if a.DefaultModel != nil {
		for _, m := range models {
			if m.ConnectionID == a.DefaultModel.ConnectionID && m.Model == a.DefaultModel.Model {
				return nil
			}
		}
		a.DefaultModel = nil
	}
	if len(models) > 0 {
		d := models[0]
		a.DefaultModel = &d
	}
	return nil
}

// ExternalAgent — агент вне каталога, объявленный runner'ами.
type ExternalAgent struct {
	ID      string   `json:"id"`
	Models  []string `json:"models"`
	Runners int      `json:"runners"`
}

// ExternalAgents — агенты зарегистрированных runner'ов, которых нет в каталоге
// или чей профиль отключён.
func (s *Store) ExternalAgents(ctx context.Context) ([]ExternalAgent, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT r.agent, array_agg(DISTINCT m) FILTER (WHERE m IS NOT NULL), count(DISTINCT r.id)
		FROM runners r LEFT JOIN LATERAL unnest(r.declared_models) m ON true
		WHERE NOT EXISTS (SELECT 1 FROM agents a WHERE a.id = r.agent AND a.enabled)
		GROUP BY r.agent ORDER BY r.agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExternalAgent{}
	for rows.Next() {
		var e ExternalAgent
		if err := rows.Scan(&e.ID, &e.Models, &e.Runners); err != nil {
			return nil, err
		}
		if e.Models == nil {
			e.Models = []string{}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteAgent удаляет профиль; предустановленный и профиль со ссылками
// (runner'ы, переопределения проектов) не удаляется.
func (s *Store) DeleteAgent(ctx context.Context, id, actorLogin string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var preset bool
		if err := tx.QueryRow(ctx, `SELECT preset FROM agents WHERE id=$1 FOR UPDATE`, id).Scan(&preset); err != nil {
			return nf(err)
		}
		if preset {
			return &FieldError{Field: "id", Msg: "предустановленный профиль нельзя удалить, только отключить"}
		}
		var refs []Ref
		rows, err := tx.Query(ctx, `SELECT id FROM runners WHERE agent=$1 ORDER BY id`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var rid string
			if err := rows.Scan(&rid); err != nil {
				rows.Close()
				return err
			}
			refs = append(refs, Ref{Kind: "runner", ID: rid})
		}
		rows.Close()
		// Действующие версии политик: последняя по номеру у проекта и у
		// установки; ссылка — переопределение модели или участник процесса.
		prows, err := tx.Query(ctx, `
			SELECT COALESCE(project_id::text, 'installation'),
			       COALESCE(content->'agent_models' ? $1, false)
			       OR EXISTS (SELECT 1 FROM jsonb_array_elements(COALESCE(content->'process'->'steps', '[]'::jsonb)) st,
			                                jsonb_array_elements(COALESCE(st->'participants', '[]'::jsonb)) p
			                  WHERE p->'agent'->>'kind' = $1)
			FROM (SELECT DISTINCT ON (scope, project_id) scope, project_id, content FROM policy_versions
			      ORDER BY scope, project_id, version DESC) v`, id)
		if err != nil {
			return err
		}
		for prows.Next() {
			var pid string
			var uses bool
			if err := prows.Scan(&pid, &uses); err != nil {
				prows.Close()
				return err
			}
			if uses {
				refs = append(refs, Ref{Kind: "project", ID: pid})
			}
		}
		prows.Close()
		team, err := agentUseRefs(ctx, tx, id, nil)
		if err != nil {
			return err
		}
		if refs = append(refs, team...); len(refs) > 0 {
			return &ErrInUse{Refs: refs}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM agents WHERE id=$1`, id); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "agent.deleted",
			Text: "удалён профиль агента " + id, Payload: map[string]any{"agent_id": id},
		})
		return err
	})
}

// agentRefsForConnection — профили, привязанные к подключению (удаление подключения).
func agentRefsForConnection(ctx context.Context, q interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}, connID string) ([]Ref, error) {
	rows, err := q.Query(ctx, `SELECT id FROM agents WHERE EXISTS (SELECT 1 FROM jsonb_array_elements(models) m WHERE m->>'connection_id' = $1) ORDER BY id`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []Ref
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		refs = append(refs, Ref{Kind: "agent", ID: id})
	}
	return refs, rows.Err()
}

// ─── участники процесса и каталог ────────────────────────────────────────

// ValidateProcessAgents проверяет участников-агентов процесса по каталогу
// (спека agents «Участник процесса из каталога»): профиль из каталога —
// модель из привязок или пустая; агент вне каталога — объявлен runner'ом,
// модель из объявленных.
func (s *Store) ValidateProcessAgents(ctx context.Context, p *policy.Process) error {
	if p == nil {
		return nil
	}
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return err
	}
	byID := map[string]domain.AgentProfile{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	external, err := s.ExternalAgents(ctx)
	if err != nil {
		return err
	}
	extBy := map[string]ExternalAgent{}
	for _, e := range external {
		extBy[e.ID] = e
	}
	for _, st := range p.Steps {
		for i, part := range st.Participants {
			if part.Agent == nil || part.Agent.Kind == "" {
				continue
			}
			field := fmt.Sprintf("participants[%d].agent", i)
			// Отключённый профиль не ограничивает: агент работает как вне
			// каталога, на объявленных runner'ами моделях.
			if a, ok := byID[part.Agent.Kind]; ok && a.Enabled {
				// Профиль без привязок модели не ограничивает: runner'ы
				// работают на объявленных моделях.
				if bound := bindingModels(a); part.Agent.Model != "" && len(bound) > 0 && !containsStr(bound, part.Agent.Model) {
					return &policy.ProcessError{Step: st.ID, Field: field + ".model", Msg: "модель " + part.Agent.Model + " не привязана к агенту " + a.ID}
				}
				continue
			}
			// Агентом может быть только агент Rivet (add-native-agent): типы
			// устаревших runner'ов вне каталога не принимаются.
			e, ok := extBy[part.Agent.Kind]
			if !ok || part.Agent.Kind != AgentRivet {
				return &policy.ProcessError{Step: st.ID, Field: field + ".kind", Msg: "агентом может быть только агент Rivet, указан " + part.Agent.Kind}
			}
			if part.Agent.Model != "" && !containsStr(e.Models, part.Agent.Model) {
				return &policy.ProcessError{Step: st.ID, Field: field + ".model", Msg: "модель " + part.Agent.Model + " не объявлена runner'ами агента " + e.ID}
			}
		}
	}
	return nil
}

// ValidateAgentModels проверяет переопределения моделей агентов проекта.
func (s *Store) ValidateAgentModels(ctx context.Context, am map[string]policy.AgentModel) error {
	for id, m := range am {
		a, err := s.GetAgent(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return &policy.ProcessError{Field: "agent_models." + id, Msg: "профиля агента " + id + " нет в каталоге"}
		}
		if err != nil {
			return err
		}
		found := false
		for _, b := range a.Models {
			if b.ConnectionID == m.ConnectionID && b.Model == m.Model {
				found = true
			}
		}
		if !found {
			return &policy.ProcessError{Field: "agent_models." + id, Msg: "модель " + m.ConnectionID + "/" + m.Model + " не привязана к агенту " + id}
		}
	}
	return nil
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ─── окружение назначения ────────────────────────────────────────────────

// ResolveAgentModel — действующая модель агента: явная модель участника,
// иначе переопределение проекта, иначе модель по умолчанию профиля.
// Возвращает привязку (подключение и модель); ok=false — агент вне каталога.
func (s *Store) ResolveAgentModel(ctx context.Context, agentID, explicit string, override *policy.AgentModel) (domain.AgentProfile, domain.AgentModelRef, bool, error) {
	a, err := s.GetAgent(ctx, agentID)
	if errors.Is(err, ErrNotFound) {
		return domain.AgentProfile{}, domain.AgentModelRef{}, false, nil
	}
	if err != nil {
		return a, domain.AgentModelRef{}, false, err
	}
	return a, ResolveAgentModelIn(a, explicit, override), true, nil
}

// ResolveAgentModelIn — ResolveAgentModel по уже прочитанному профилю:
// явная модель, иначе пара переопределения, иначе модель по умолчанию.
func ResolveAgentModelIn(a domain.AgentProfile, explicit string, override *policy.AgentModel) domain.AgentModelRef {
	if explicit != "" {
		for _, b := range a.Models {
			if b.Model == explicit {
				return b
			}
		}
		// Модель есть у runner'а, но не в привязках: подключения не знаем.
		return domain.AgentModelRef{Model: explicit}
	}
	if override != nil {
		for _, b := range a.Models {
			if b.ConnectionID == override.ConnectionID && b.Model == override.Model {
				return b
			}
		}
	}
	if a.DefaultModel != nil {
		return *a.DefaultModel
	}
	return domain.AgentModelRef{}
}

// SetRunAssignment фиксирует, с чем стадия ушла runner'у: модель (для
// консоли и usage), подключение и признак, что ключ каталога передан в
// назначении (по ним разбирается отказ провайдера во время стадии).
func (s *Store) SetRunAssignment(ctx context.Context, runID int64, model, connectionID string, catalogKey bool) error {
	// Назначенная модель — отдельно от явной модели участника: она не
	// становится требованием при повторном назначении.
	_, err := s.Pool.Exec(ctx, `UPDATE task_step_runs SET assigned_model=$2, assigned_connection=$3, catalog_key=$4 WHERE id=$1`,
		runID, model, connectionID, catalogKey)
	return err
}

// ModelEndpoint — подключение модели для агентского цикла Rivet в
// назначении (Assignment.endpoint): адрес, модель, окно и цены из каталога,
// лимиты профиля, ключ — только по режиму секретов.
type ModelEndpoint struct {
	API           string
	BaseURL       string
	Model         string
	ConnectionID  string
	ContextWindow int64
	// MaxOutputTokens — предел ответа на ход, ModelMaxOutput — предельный
	// выход модели; 0 — по умолчанию цикла.
	MaxOutputTokens int64
	ModelMaxOutput  int64
	HasPrices       bool
	InputPrice      float64 // USD за миллион токенов
	OutputPrice     float64
	// CachedInputPrice — цена чтения из кеша; 0 — цены кеша нет, цикл
	// считает кешированный вход по цене входа.
	CachedInputPrice float64
	Limits           domain.AgentLimits
	Key              string
	Headers          map[string]string
	SecretsIncluded  bool
	// SecretNames — что доставлено (для события без значений).
	SecretNames []string
}

// BuildModelEndpoint собирает подключение агента Rivet по привязке. Ключ и
// секретные заголовки включаются, только если includeSecrets (решение по
// режиму профиля и каналу runner'а принимает вызывающий).
func (s *Store) BuildModelEndpoint(ctx context.Context, a domain.AgentProfile, b domain.AgentModelRef, box *secretbox.Box, includeSecrets bool) (ModelEndpoint, error) {
	if b.ConnectionID == "" {
		return ModelEndpoint{Model: b.Model, Limits: a.Limits}, nil
	}
	cl, c, err := s.ConnectionClient(ctx, b.ConnectionID, box)
	if err != nil && !errors.Is(err, ErrSecret) && !errors.Is(err, secretbox.ErrNoKey) {
		return ModelEndpoint{Model: b.Model, ConnectionID: b.ConnectionID, Limits: a.Limits}, err
	}
	return ModelEndpointFrom(a, b, cl, c, err == nil, includeSecrets), nil
}

// ModelEndpointFrom — подключение агента Rivet из уже прочитанного
// подключения каталога (ConnectionClient): проверка и сборка назначения
// идут по одному чтению. secretsRead — ключ и секретные заголовки
// расшифрованы; includeSecrets — отдавать ли их runner'у.
func ModelEndpointFrom(a domain.AgentProfile, b domain.AgentModelRef, cl llm.Client, c domain.ModelConnection, secretsRead, includeSecrets bool) ModelEndpoint {
	ep := ModelEndpoint{Model: b.Model, ConnectionID: b.ConnectionID, Limits: a.Limits}
	if b.ConnectionID == "" {
		return ep
	}
	err := error(nil)
	if !secretsRead {
		err = ErrSecret
	}
	ep.API, ep.BaseURL = c.API, c.BaseURL
	if ep.BaseURL == "" {
		ep.BaseURL = cl.BaseURL
	}
	for _, m := range c.Models {
		if m.ID != b.Model {
			continue
		}
		if m.ContextWindow != nil {
			ep.ContextWindow = *m.ContextWindow
		}
		if m.MaxOutputTokens != nil {
			ep.MaxOutputTokens = *m.MaxOutputTokens
		}
		if m.ModelMaxOutput != nil {
			ep.ModelMaxOutput = *m.ModelMaxOutput
		}
		// Цены в каталоге — микродоллары за миллион токенов.
		if m.InputPrice != nil || m.OutputPrice != nil {
			ep.HasPrices = true
			if m.InputPrice != nil {
				ep.InputPrice = float64(*m.InputPrice) / 1e6
			}
			if m.OutputPrice != nil {
				ep.OutputPrice = float64(*m.OutputPrice) / 1e6
			}
			if m.CachedInputPrice != nil {
				ep.CachedInputPrice = float64(*m.CachedInputPrice) / 1e6
			}
		}
	}
	if includeSecrets && err == nil {
		secret := map[string]bool{}
		for _, h := range c.Headers {
			if h.Secret {
				secret[h.Name] = true
			}
		}
		if cl.Key != "" {
			ep.Key = cl.Key
			ep.SecretNames = append(ep.SecretNames, "key")
		}
		ep.Headers = map[string]string{}
		for k, v := range cl.Headers {
			ep.Headers[k] = v
			if secret[k] {
				ep.SecretNames = append(ep.SecretNames, "header:"+k)
			}
		}
		ep.SecretsIncluded = len(ep.SecretNames) > 0
	} else if err == nil {
		// Без секретов уходят только несекретные заголовки.
		secret := map[string]bool{}
		for _, h := range c.Headers {
			if h.Secret {
				secret[h.Name] = true
			}
		}
		ep.Headers = map[string]string{}
		for k, v := range cl.Headers {
			if !secret[k] {
				ep.Headers[k] = v
			}
		}
	}
	sort.Strings(ep.SecretNames)
	return ep
}
