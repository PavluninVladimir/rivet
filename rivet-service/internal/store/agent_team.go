package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
)

// Команда агентов (add-agent-roles, спека agent-roles «Команда агентов»):
// именованные агенты из шаблонов ролей. Агент ведёт одну сессию
// одновременно; его модель — тройка «профиль, подключение, модель» из
// привязок профиля или (пусто) модель шаблона. Модель агента-тимлида —
// настройка планировщика.

// TeamAgentInput — что сохраняет администратор. Enabled nil — не менять
// (при создании — включён); Position nil — в конец команды.
type TeamAgentInput struct {
	ID               string
	Name             string
	RoleID           string
	Agent            string
	ConnectionID     string
	Model            string
	InstructionExtra string
	Enabled          *bool
	Position         *int
	// ExpectedUpdatedAt — версия, которую видел клиент (изменение).
	ExpectedUpdatedAt *time.Time
}

var teamAgentIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// teamCols — агент команды с текущей работой (открытый запуск агента).
const teamCols = `a.id, a.name, a.role_id, a.agent, a.connection_id, a.model, a.instruction_extra, a.enabled, a.position,
	a.updated_at, COALESCE(u.login, ''),
	COALESCE(w.task_id::text, ''), COALESCE(w.num, 0), COALESCE(w.project_id::text, ''), COALESCE(w.epic_id::text, ''),
	COALESCE(w.step_kind, ''), COALESCE(w.step_entry, ''), COALESCE(w.runner_id, ''), COALESCE(w.runner_name, '')`

const teamFrom = `team_agents a LEFT JOIN users u ON u.id = a.updated_by
	LEFT JOIN LATERAL (
		SELECT r.task_id, t.num, e.project_id, t.epic_id, r.step_kind, t.step_entry, r.runner_id,
		       COALESCE(NULLIF(x.display_name, ''), x.id) AS runner_name
		FROM task_step_runs r JOIN tasks t ON t.id = r.task_id JOIN epics e ON e.id = t.epic_id
		LEFT JOIN runners x ON x.id = r.runner_id
		WHERE r.agent_id = a.id AND r.verdict IS NULL
		ORDER BY r.id DESC LIMIT 1) w ON true`

func scanTeamAgent(row pgx.Row) (domain.TeamAgent, error) {
	var a domain.TeamAgent
	err := row.Scan(&a.ID, &a.Name, &a.RoleID, &a.Agent, &a.ConnectionID, &a.Model, &a.InstructionExtra, &a.Enabled, &a.Position,
		&a.UpdatedAt, &a.UpdatedBy,
		&a.TaskID, &a.TaskNum, &a.ProjectID, &a.EpicID, &a.StepKind, &a.StepEntry, &a.RunnerID, &a.RunnerName)
	return a, err
}

// withPlannerAgentModel — агенту-тимлиду модель планировщика.
func (s *Store) withPlannerAgentModel(ctx context.Context, a *domain.TeamAgent) error {
	if a.RoleID != domain.RoleLead {
		return nil
	}
	pm, ok, err := s.PlannerModel(ctx)
	if err != nil {
		return err
	}
	a.Agent, a.ConnectionID, a.Model = "", "", ""
	if ok {
		a.ConnectionID, a.Model = pm.ConnectionID, pm.Model
	}
	return nil
}

// ListTeam — команда в порядке позиций с текущей работой агентов.
func (s *Store) ListTeam(ctx context.Context) ([]domain.TeamAgent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+teamCols+` FROM `+teamFrom+` ORDER BY a.position, a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.TeamAgent{}
	for rows.Next() {
		a, err := scanTeamAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.withPlannerAgentModel(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetTeamAgent — агент команды по идентификатору.
func (s *Store) GetTeamAgent(ctx context.Context, id string) (domain.TeamAgent, error) {
	a, err := scanTeamAgent(s.Pool.QueryRow(ctx, `SELECT `+teamCols+` FROM `+teamFrom+` WHERE a.id=$1`, id))
	if err != nil {
		return a, nf(err)
	}
	return a, s.withPlannerAgentModel(ctx, &a)
}

// FirstTeamAgent — первый включённый агент роли по порядку команды
// (Тимлид для декомпозиции); ok=false — включённых агентов роли нет.
func (s *Store) FirstTeamAgent(ctx context.Context, roleID string) (domain.TeamAgent, bool, error) {
	a, err := scanTeamAgent(s.Pool.QueryRow(ctx, `SELECT `+teamCols+` FROM `+teamFrom+`
		WHERE a.role_id=$1 AND a.enabled ORDER BY a.position, a.id LIMIT 1`, roleID))
	if errors.Is(err, pgx.ErrNoRows) {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	return a, true, s.withPlannerAgentModel(ctx, &a)
}

// RoleHasTeam — есть ли в команде включённые агенты роли.
func (s *Store) RoleHasTeam(ctx context.Context, roleID string) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM team_agents WHERE role_id=$1 AND enabled)`, roleID).Scan(&ok)
	return ok, err
}

// checkAgentModel — тройка модели агента: профиль из каталога, привязка в
// его моделях, подключение включено, модель видима. Пустая тройка — модель
// шаблона; без профиля — модель включённого подключения.
func checkAgentModel(ctx context.Context, tx pgx.Tx, agent, connID, model, field string) error {
	if agent == "" && connID == "" && model == "" {
		return nil
	}
	if (connID == "") != (model == "") {
		return &FieldError{Field: field, Msg: "укажите подключение и модель вместе"}
	}
	if agent != "" {
		a, err := scanAgent(tx.QueryRow(ctx, `SELECT `+agentCols+` FROM agents a LEFT JOIN users u ON u.id=a.updated_by WHERE a.id=$1`, agent))
		if errors.Is(err, pgx.ErrNoRows) {
			return &FieldError{Field: field, Msg: "профиля агента " + agent + " нет в каталоге"}
		}
		if err != nil {
			return err
		}
		// Модели агента Rivet — вычисляемые (каталог подключений), в строке
		// профиля их нет.
		if err := fillCatalogModels(ctx, tx, &a); err != nil {
			return err
		}
		if !a.Enabled {
			return &FieldError{Field: field, Msg: "профиль агента " + agent + " отключён"}
		}
		if model == "" {
			return nil
		}
		found := false
		for _, b := range a.Models {
			if b.ConnectionID == connID && b.Model == model {
				found = true
			}
		}
		if !found {
			return &FieldError{Field: field, Msg: "модель " + connID + "/" + model + " не привязана к агенту " + agent}
		}
	}
	return checkBinding(ctx, tx, domain.AgentModelRef{ConnectionID: connID, Model: model}, field)
}

// CreateTeamAgent добавляет агента из шаблона; занятый идентификатор —
// ErrExists.
func (s *Store) CreateTeamAgent(ctx context.Context, in TeamAgentInput, actorID, actorLogin string) (domain.TeamAgent, error) {
	a, _, err := s.saveTeamAgent(ctx, in, saveCreate, actorID, actorLogin)
	return a, err
}

// UpdateTeamAgent меняет агента: нет агента — ErrNotFound, версия не
// совпала — ErrStale.
func (s *Store) UpdateTeamAgent(ctx context.Context, in TeamAgentInput, actorID, actorLogin string) (domain.TeamAgent, error) {
	a, _, err := s.saveTeamAgent(ctx, in, saveUpdate, actorID, actorLogin)
	return a, err
}

// UpsertTeamAgent — создание или изменение (внутренние вызовы и тесты).
func (s *Store) UpsertTeamAgent(ctx context.Context, in TeamAgentInput, actorID, actorLogin string) (domain.TeamAgent, bool, error) {
	return s.saveTeamAgent(ctx, in, saveAuto, actorID, actorLogin)
}

// saveTeamAgent сохраняет агента. Смена модели пишется в событие с
// прежним и новым значением; модель Тимлида уходит в настройку
// планировщика, только если тело явно несёт другую модель.
func (s *Store) saveTeamAgent(ctx context.Context, in TeamAgentInput, mode int, actorID, actorLogin string) (domain.TeamAgent, bool, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.InstructionExtra = strings.TrimSpace(in.InstructionExtra)
	in.Agent = strings.TrimSpace(in.Agent)
	if !teamAgentIDRe.MatchString(in.ID) {
		return domain.TeamAgent{}, false, &FieldError{Field: "id", Msg: "идентификатор: латиница, цифры, дефис, до 63 символов"}
	}
	if n := utf8.RuneCountInString(in.Name); n < 1 || n > 40 || strings.ContainsFunc(in.Name, isControl) {
		return domain.TeamAgent{}, false, &FieldError{Field: "name", Msg: "имя от 1 до 40 символов в одну строку"}
	}
	if utf8.RuneCountInString(in.InstructionExtra) > 4000 {
		return domain.TeamAgent{}, false, &FieldError{Field: "instruction_extra", Msg: "дополнение инструкции до 4000 символов"}
	}
	var before domain.TeamAgent
	created := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var capability, roleAgent string
		var roleEnabled bool
		err := tx.QueryRow(ctx, `SELECT capability, enabled, agent FROM agent_roles WHERE id=$1`, in.RoleID).Scan(&capability, &roleEnabled, &roleAgent)
		if errors.Is(err, pgx.ErrNoRows) {
			return &FieldError{Field: "role_id", Msg: "шаблона роли " + in.RoleID + " нет"}
		}
		if err != nil {
			return err
		}
		// Тимлид и Аналитик работают на control plane: профиль им не нужен,
		// модель Тимлида — настройка планировщика.
		if capability == "" && in.Agent != "" {
			return &FieldError{Field: "model", Msg: "роль выполняет control plane: профиль агента не задаётся"}
		}
		// Без своего профиля действует профиль шаблона: модель проверяется
		// по его привязкам.
		effAgent := in.Agent
		if effAgent == "" {
			effAgent = roleAgent
		}
		if err := checkAgentModel(ctx, tx, effAgent, in.ConnectionID, in.Model, "model"); err != nil {
			return err
		}
		before, err = scanTeamAgent(tx.QueryRow(ctx, `SELECT `+teamCols+` FROM `+teamFrom+` WHERE a.id=$1 FOR UPDATE OF a`, in.ID))
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		switch {
		case mode == saveCreate && exists:
			return ErrExists
		case mode == saveUpdate && !exists:
			return ErrNotFound
		case exists && mode != saveCreate && !sameVersion(before.UpdatedAt, in.ExpectedUpdatedAt):
			return ErrStale
		}
		enabled := true
		if exists {
			enabled = before.Enabled
		}
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		agent, connID, model := in.Agent, in.ConnectionID, in.Model
		if in.RoleID == domain.RoleLead {
			agent, connID, model = "", "", ""
		}
		if !exists {
			created = true
			pos := 0
			if in.Position != nil {
				pos = *in.Position
			} else if err := tx.QueryRow(ctx, `SELECT COALESCE(max(position), 0) + 10 FROM team_agents`).Scan(&pos); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO team_agents (id, name, role_id, agent, connection_id, model, instruction_extra, enabled, position, updated_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
				in.ID, in.Name, in.RoleID, agent, connID, model, in.InstructionExtra, enabled, pos, nullUUID(actorID)); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `
				UPDATE team_agents SET name=$2, role_id=$3, agent=$4, connection_id=$5, model=$6, instruction_extra=$7,
					enabled=$8, position=COALESCE($9, position), updated_at=now(), updated_by=$10 WHERE id=$1`,
			in.ID, in.Name, in.RoleID, agent, connID, model, in.InstructionExtra, enabled, in.Position, nullUUID(actorID)); err != nil {
			return err
		}
		typ, text := "team.agent_updated", "изменён агент команды «"+in.Name+"»"
		if created {
			typ, text = "team.agent_created", "в команду добавлен агент «"+in.Name+"»"
		}
		payload := map[string]any{"id": in.ID, "name": in.Name, "role": in.RoleID, "enabled": enabled,
			"model": modelTriple(agent, connID, model)}
		if !created {
			payload["before"] = map[string]any{"name": before.Name, "role": before.RoleID, "enabled": before.Enabled,
				"model": modelTriple(before.Agent, before.ConnectionID, before.Model)}
		}
		_, err = appendEvent(ctx, tx, EventInput{ActorKind: domain.ActorUser, ActorID: actorLogin, Type: typ, Text: text, Payload: payload})
		return err
	})
	if err != nil {
		return domain.TeamAgent{}, false, err
	}
	if in.RoleID == domain.RoleLead && in.ConnectionID != "" && in.Model != "" {
		if err := s.setPlannerIfChanged(ctx, in.ConnectionID, in.Model, actorID, actorLogin); err != nil {
			return domain.TeamAgent{}, created, err
		}
	}
	out, err := s.GetTeamAgent(ctx, in.ID)
	return out, created, err
}

// modelTriple — тройка модели для событий; пустая — «как в шаблоне».
func modelTriple(agent, connID, model string) string {
	if agent == "" && connID == "" && model == "" {
		return ""
	}
	return strings.TrimPrefix(agent+" / "+connID+" / "+model, " / ")
}

// DeleteTeamAgent удаляет агента. Сессию, которую он ведёт, это не
// прерывает: запуск остаётся без агента и доводится до конца.
func (s *Store) DeleteTeamAgent(ctx context.Context, id, actorLogin string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var name, role string
		if err := tx.QueryRow(ctx, `SELECT name, role_id FROM team_agents WHERE id=$1 FOR UPDATE`, id).Scan(&name, &role); err != nil {
			return nf(err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM team_agents WHERE id=$1`, id); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "team.agent_deleted",
			Text: "из команды удалён агент «" + name + "»", Payload: map[string]any{"id": id, "name": name, "role": role},
		})
		return err
	})
}

// ModelChoice — доступная тройка модели для агента команды: привязка
// включённого профиля через включённое подключение и признак, подключён ли
// сейчас runner с этим агентом.
type ModelChoice struct {
	Agent        string `json:"agent"`
	AgentName    string `json:"agent_name"`
	ConnectionID string `json:"connection_id"`
	Model        string `json:"model"`
	RunnerOnline bool   `json:"runner_online"`
}

// ModelChoices — все тройки «профиль, подключение, модель» включённых
// профилей с доступными привязками.
func (s *Store) ModelChoices(ctx context.Context) ([]ModelChoice, error) {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	online := map[string]bool{}
	rows, err := s.Pool.Query(ctx, `SELECT DISTINCT agent FROM runners WHERE status <> 'offline'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return nil, err
		}
		online[a] = true
	}
	rows.Close()
	// Доступные модели: включённое подключение, модель в списке, не скрыта
	// и не пропала (те же правила, что у checkBinding).
	visible := map[string]bool{}
	crows, err := s.Pool.Query(ctx, `SELECT id, models FROM model_connections WHERE enabled`)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var id string
		var raw []byte
		if err := crows.Scan(&id, &raw); err != nil {
			crows.Close()
			return nil, err
		}
		var models []domain.ModelEntry
		_ = json.Unmarshal(raw, &models)
		for _, m := range models {
			if !m.Hidden && !m.Missing {
				visible[id+"/"+m.ID] = true
			}
		}
	}
	crows.Close()
	out := []ModelChoice{}
	for _, a := range agents {
		if !a.Enabled {
			continue
		}
		for _, b := range a.Models {
			if b.Unavailable || !visible[b.ConnectionID+"/"+b.Model] {
				continue
			}
			out = append(out, ModelChoice{Agent: a.ID, AgentName: a.Name, ConnectionID: b.ConnectionID, Model: b.Model, RunnerOnline: online[a.ID]})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return fmt.Sprint(out[i].ConnectionID, "/", out[i].Model) < fmt.Sprint(out[j].ConnectionID, "/", out[j].Model)
	})
	return out, nil
}

// TeamWait — запуск с ролью, которому не достаётся агент команды: все
// агенты роли заняты (Busy) или свободным агентам роли не подходит ни один
// подключённый runner.
type TeamWait struct {
	Run       StepRun
	RoleTitle string
	Busy      bool
}

// WaitingTeamRuns — ожидающие запуски ролей с командой без причины
// ожидания у задачи (спека agent-roles «Назначение агенту команды»).
// Проекты и Epic'и, исключённые из назначений в этом проходе (бюджет,
// недоступный движок политик, ожидание зависимостей Epic'а), пропускаются:
// их задачи стоят не из-за агентов, а свою причину консоль показывает на
// Epic'е и проекте.
// excludedAgents — агенты без рабочей модели в этом проходе
// (fix-agent-without-connection): для запусков без явной модели они не
// считаются свободными.
func (s *Store) WaitingTeamRuns(ctx context.Context, excludedProjects, excludedEpics, excludedAgents []string) ([]TeamWait, error) {
	if excludedProjects == nil {
		excludedProjects = []string{}
	}
	if excludedEpics == nil {
		excludedEpics = []string{}
	}
	if excludedAgents == nil {
		excludedAgents = []string{}
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+runCols+`, ar.title,
			NOT EXISTS (SELECT 1 FROM team_agents ag WHERE ag.role_id = r.role AND ag.enabled
				AND (r.model <> '' OR ag.id <> ALL($3::text[]))
				AND NOT EXISTS (SELECT 1 FROM task_step_runs o WHERE o.agent_id = ag.id AND o.verdict IS NULL)) AS busy
		FROM task_step_runs r
		JOIN tasks t ON t.id = r.task_id
		JOIN epics e ON e.id = t.epic_id AND e.status = 'running'
			AND e.project_id <> ALL($1::uuid[]) AND e.id <> ALL($2::uuid[])
		JOIN agent_roles ar ON ar.id = r.role AND ar.enabled AND ar.capability <> ''
		WHERE r.runner_id IS NULL AND r.verdict IS NULL AND t.wait_reason = '' AND `+agentRunsOnly+`
		  AND EXISTS (SELECT 1 FROM team_agents ta WHERE ta.role_id = r.role AND ta.enabled)
		  -- ни одному свободному агенту роли не подходит ни один подключённый
		  -- runner (занятый подходящий runner — обычное ожидание без причины)
		  AND NOT EXISTS (
			SELECT 1 FROM team_agents ag JOIN runners x ON x.status <> 'offline' AND NOT x.draining AND `+agentRunner("x")+`
			WHERE ag.role_id = r.role AND ag.enabled
			  AND (r.model <> '' OR ag.id <> ALL($3::text[]))
			  AND NOT EXISTS (SELECT 1 FROM task_step_runs o WHERE o.agent_id = ag.id AND o.verdict IS NULL)
			  AND x.capabilities @> (r.capabilities || CASE WHEN r.step_kind IN ('code','test','prompt') THEN t.capabilities ELSE '{}'::text[] END)
			  AND x.agent = COALESCE(NULLIF(ag.agent, ''), NULLIF(r.role_agent, ''), x.agent)
			  AND (ag.model = '' OR ag.model = ANY(x.models) OR x.model = ag.model)
			  AND (r.agent_kind = '' OR x.agent = r.agent_kind)
			  AND (r.model = '' OR r.model = ANY(x.models))
			  AND (r.step_kind <> 'prompt' OR 'PROMPT' = ANY(x.stages)))
		ORDER BY r.id`, excludedProjects, excludedEpics, excludedAgents)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamWait
	for rows.Next() {
		var w TeamWait
		r := &w.Run
		if err := rows.Scan(&r.ID, &r.TaskID, &r.StepID, &r.StepKind, &r.Pass, &r.Participant, &r.AgentKind, &r.Model,
			&r.Capabilities, &r.UserLogin, &r.UserRole, &r.RunnerID, &r.SessionID, &r.Verdict, &r.VerdictBy,
			&r.Detail, &r.CreatedAt, &r.FinishedAt, &r.Role, &r.PrefModel, &r.AgentID, &r.AgentName,
			&r.RoleAgent, &r.PrefAgent, &r.AgentVerdict, &r.AssignedModel, &r.AssignedConnection, &r.CatalogKey,
			&w.RoleTitle, &w.Busy); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
