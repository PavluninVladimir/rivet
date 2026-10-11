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
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/secretbox"
)

// Каталог агентов (add-agent-profiles, спека backend/agents): профили с
// адаптером, командой, привязками моделей из каталога подключений, шаблоном
// окружения и режимом доставки секретов. Runner'ы с агентом из каталога
// получают модели из привязок; окружение назначения собирается по шаблону.

var agentIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Подстановки шаблона окружения и аргументов.
var placeholderRe = regexp.MustCompile(`\{\{\s*([a-z_]+)(?::([A-Za-z0-9-]+))?\s*\}\}`)

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
}

const agentCols = `a.id, a.name, a.adapter, a.command, a.capabilities, a.models, a.default_model, a.env, a.args, a.secrets, a.enabled, a.preset, a.updated_at, COALESCE(u.login, ''),
	(SELECT count(*) FROM runners r WHERE r.agent = a.id)`

func scanAgent(row pgx.Row) (domain.AgentProfile, error) {
	var a domain.AgentProfile
	var models, def, env, args []byte
	err := row.Scan(&a.ID, &a.Name, &a.Adapter, &a.Command, &a.Capabilities, &models, &def, &env, &args,
		&a.Secrets, &a.Enabled, &a.Preset, &a.UpdatedAt, &a.UpdatedBy, &a.Runners)
	if err != nil {
		return a, err
	}
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

// validatePlaceholders — все подстановки известны: key, base_url, model,
// connection_id, header:Имя.
var anyPlaceholderRe = regexp.MustCompile(`\{\{[^}]*\}\}`)

func validatePlaceholders(field, v string) error {
	if len(anyPlaceholderRe.FindAllString(v, -1)) != len(placeholderRe.FindAllString(v, -1)) {
		return &FieldError{Field: field, Msg: "некорректная подстановка: допустимы {{key}}, {{base_url}}, {{model}}, {{connection_id}}, {{header:Имя}}"}
	}
	for _, m := range placeholderRe.FindAllStringSubmatch(v, -1) {
		switch m[1] {
		case "key", "base_url", "model", "connection_id":
			if m[2] != "" {
				return &FieldError{Field: field, Msg: "подстановка {{" + m[1] + "}} без аргумента"}
			}
		case "header":
			if m[2] == "" {
				return &FieldError{Field: field, Msg: "подстановка {{header:Имя}} требует имя заголовка"}
			}
		default:
			return &FieldError{Field: field, Msg: "неизвестная подстановка {{" + m[1] + "}}: допустимы key, base_url, model, connection_id, header:Имя"}
		}
	}
	return nil
}

func validateAgent(in AgentInput) error {
	if !agentIDRe.MatchString(in.ID) {
		return &FieldError{Field: "id", Msg: "идентификатор: латиница, цифры, дефис, до 63 символов"}
	}
	if strings.TrimSpace(in.Name) == "" {
		return &FieldError{Field: "name", Msg: "укажите название"}
	}
	switch in.Adapter {
	case "claude-code", "wrap":
	default:
		return &FieldError{Field: "adapter", Msg: "адаптер: claude-code или wrap"}
	}
	if strings.ContainsAny(in.Command, "\r\n") {
		return &FieldError{Field: "command", Msg: "команда в одну строку"}
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
	envRe := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	for i, e := range in.Env {
		if !envRe.MatchString(e.Name) {
			return &FieldError{Field: fmt.Sprintf("env[%d].name", i), Msg: "имя переменной: буквы, цифры, подчёркивание"}
		}
		if err := validatePlaceholders(fmt.Sprintf("env[%d].value", i), e.Value); err != nil {
			return err
		}
	}
	for i, a := range in.Args {
		if err := validatePlaceholders(fmt.Sprintf("args[%d]", i), a); err != nil {
			return err
		}
		for _, m := range placeholderRe.FindAllStringSubmatch(a, -1) {
			if m[1] == "key" || m[1] == "header" {
				return &FieldError{Field: fmt.Sprintf("args[%d]", i), Msg: "секреты в аргументах недопустимы: аргументы видны в списке процессов, используйте переменные окружения"}
			}
		}
	}
	return nil
}

// UpsertAgent создаёт или меняет профиль: привязки проверяются по каталогу
// подключений, runner'ы с этим агентом получают модели из привязок.
func (s *Store) UpsertAgent(ctx context.Context, in AgentInput, actorID, actorLogin string) (domain.AgentProfile, bool, error) {
	in.Name, in.Command = strings.TrimSpace(in.Name), strings.TrimSpace(in.Command)
	if in.Secrets == "" {
		in.Secrets = "secure"
	}
	if err := validateAgent(in); err != nil {
		return domain.AgentProfile{}, false, err
	}
	var out domain.AgentProfile
	created := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Привязки: подключение включено, модель в списке, не скрыта и не пропала.
		seen := map[string]bool{}
		for i, m := range in.Models {
			key := m.ConnectionID + "/" + m.Model
			if seen[key] {
				return &FieldError{Field: fmt.Sprintf("models[%d]", i), Msg: "привязка повторяется"}
			}
			seen[key] = true
			if err := checkBinding(ctx, tx, m, fmt.Sprintf("models[%d]", i)); err != nil {
				return err
			}
			in.Models[i].Unavailable = false
		}
		if in.DefaultModel != nil {
			if !seen[in.DefaultModel.ConnectionID+"/"+in.DefaultModel.Model] {
				return &FieldError{Field: "default_model", Msg: "модель по умолчанию должна быть одной из привязок"}
			}
		} else if len(in.Models) > 0 {
			d := in.Models[0]
			in.DefaultModel = &d
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM agents WHERE id=$1 FOR UPDATE`, in.ID).Scan(&exists); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if in.Models == nil {
			in.Models = []domain.AgentModelRef{}
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
		mj, _ := json.Marshal(in.Models)
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
				INSERT INTO agents (id, name, adapter, command, capabilities, models, default_model, env, args, secrets, enabled, updated_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
				in.ID, in.Name, in.Adapter, in.Command, in.Capabilities, mj, dj, ej, aj, in.Secrets, enabled, actorID); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `
				UPDATE agents SET name=$2, adapter=$3, command=$4, capabilities=$5, models=$6, default_model=$7, env=$8, args=$9,
					secrets=$10, enabled=COALESCE($11, enabled), updated_at=now(), updated_by=$12 WHERE id=$1`,
			in.ID, in.Name, in.Adapter, in.Command, in.Capabilities, mj, dj, ej, aj, in.Secrets, in.Enabled, actorID); err != nil {
			return err
		}
		var err error
		out, err = scanAgent(tx.QueryRow(ctx, `SELECT `+agentCols+` FROM agents a LEFT JOIN users u ON u.id=a.updated_by WHERE a.id=$1`, in.ID))
		if err != nil {
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
	return out, rows.Err()
}

func (s *Store) GetAgent(ctx context.Context, id string) (domain.AgentProfile, error) {
	a, err := scanAgent(s.Pool.QueryRow(ctx, `SELECT `+agentCols+` FROM agents a LEFT JOIN users u ON u.id=a.updated_by WHERE a.id=$1`, id))
	return a, nf(err)
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
		if len(refs) > 0 {
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
			e, ok := extBy[part.Agent.Kind]
			if !ok {
				return &policy.ProcessError{Step: st.ID, Field: field + ".kind", Msg: "агента " + part.Agent.Kind + " нет ни в каталоге, ни среди зарегистрированных runner'ов"}
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

// AgentLaunch — что runner получает для запуска агента: модель, окружение и
// аргументы по шаблону профиля, команда обёртки, подключение модели.
type AgentLaunch struct {
	Model        string
	ConnectionID string
	Env          map[string]string
	Args         []string
	Command      string
	// SecretNames — переменные с секретами; SecretValues — их значения для
	// маскирования; Secrets — режим профиля.
	SecretNames  []string
	SecretValues []string
	Secrets      string
	Catalog      bool
}

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
	if explicit != "" {
		for _, b := range a.Models {
			if b.Model == explicit {
				return a, b, true, nil
			}
		}
		// Модель есть у runner'а, но не в привязках: подключения не знаем.
		return a, domain.AgentModelRef{Model: explicit}, true, nil
	}
	if override != nil {
		for _, b := range a.Models {
			if b.ConnectionID == override.ConnectionID && b.Model == override.Model {
				return a, b, true, nil
			}
		}
	}
	if a.DefaultModel != nil {
		return a, *a.DefaultModel, true, nil
	}
	return a, domain.AgentModelRef{}, true, nil
}

// BuildAgentLaunch собирает окружение и аргументы по шаблону профиля и
// подключению привязки. Секреты подставляются всегда (решение о доставке
// принимает вызывающий по режиму и каналу): при отказе в доставке их
// переменные выбрасываются целиком.
func (s *Store) BuildAgentLaunch(ctx context.Context, a domain.AgentProfile, b domain.AgentModelRef, box *secretbox.Box, includeSecrets bool) (AgentLaunch, error) {
	l := AgentLaunch{Model: b.Model, ConnectionID: b.ConnectionID, Env: map[string]string{}, Args: []string{}, Command: a.Command, Secrets: a.Secrets, Catalog: true}
	key, baseURL := "", ""
	headers := map[string]string{}
	secretHeaders := map[string]bool{}
	if b.ConnectionID != "" {
		cl, c, err := s.ConnectionClient(ctx, b.ConnectionID, box)
		if err != nil && !errors.Is(err, ErrSecret) && !errors.Is(err, secretbox.ErrNoKey) {
			return l, err
		}
		if err == nil {
			key, baseURL, headers = cl.Key, cl.BaseURL, cl.Headers
		} else {
			baseURL = c.BaseURL
		}
		for _, h := range c.Headers {
			if h.Secret {
				secretHeaders[h.Name] = true
			}
		}
	}
	subst := func(v string) (string, bool) {
		secret := false
		out := placeholderRe.ReplaceAllStringFunc(v, func(m string) string {
			sm := placeholderRe.FindStringSubmatch(m)
			switch sm[1] {
			case "key":
				secret = true
				return key
			case "base_url":
				return baseURL
			case "model":
				return b.Model
			case "connection_id":
				return b.ConnectionID
			case "header":
				if secretHeaders[sm[2]] {
					secret = true
				}
				return headers[sm[2]]
			}
			return ""
		})
		return out, secret
	}
	for _, e := range a.Env {
		v, secret := subst(e.Value)
		if secret {
			if !includeSecrets {
				continue
			}
			l.SecretNames = append(l.SecretNames, e.Name)
			if v != "" {
				l.SecretValues = append(l.SecretValues, v)
			}
		}
		if v == "" && strings.Contains(e.Value, "{{") {
			continue // подстановка пустая: переменную не задаём
		}
		l.Env[e.Name] = v
	}
	for _, arg := range a.Args {
		v, secret := subst(arg)
		if secret && !includeSecrets {
			continue
		}
		if secret && v != "" {
			l.SecretValues = append(l.SecretValues, v)
		}
		l.Args = append(l.Args, v)
	}
	sort.Strings(l.SecretNames)
	return l, nil
}

// SetRunModel фиксирует модель, выбранную при назначении (для консоли и usage).
func (s *Store) SetRunModel(ctx context.Context, runID int64, model string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE task_step_runs SET model=$2 WHERE id=$1 AND model=''`, runID, model)
	return err
}
