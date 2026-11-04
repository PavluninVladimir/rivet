package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Шаблоны ролей и команда агентов (add-agent-roles, api-contract): чтение
// всем вошедшим, правка — администратору. Создание и изменение разведены:
// POST создаёт (занятый id — 409), PUT меняет существующее (нет — 404).
// Необязательный updated_at в теле PUT — оптимистическая проверка: запись
// изменили после чтения — 409 stale с актуальной версией.

type roleView struct {
	ID           string                `json:"id"`
	Title        string                `json:"title"`
	Description  string                `json:"description"`
	Instruction  string                `json:"instruction"`
	Color        string                `json:"color"`
	Capability   string                `json:"capability"`
	Agent        string                `json:"agent"`
	ConnectionID string                `json:"connection_id"`
	Model        string                `json:"model"`
	DefaultModel *domain.AgentModelRef `json:"default_model"`
	Enabled      bool                  `json:"enabled"`
	Builtin      bool                  `json:"builtin"`
	ControlPlane bool                  `json:"control_plane"`
	Agents       int                   `json:"agents"`
	UpdatedAt    time.Time             `json:"updated_at"`
	UpdatedBy    string                `json:"updated_by"`
}

func roleDTO(r domain.AgentRole, agents int) roleView {
	v := roleView{
		ID: r.ID, Title: r.Title, Description: r.Description, Instruction: r.Instruction, Color: r.Color,
		Capability: r.Capability, Agent: r.Agent, DefaultModel: r.DefaultModel, Enabled: r.Enabled,
		Builtin: r.Builtin, ControlPlane: r.Capability == "", Agents: agents,
		UpdatedAt: r.UpdatedAt, UpdatedBy: r.UpdatedBy,
	}
	if r.DefaultModel != nil {
		v.ConnectionID, v.Model = r.DefaultModel.ConnectionID, r.DefaultModel.Model
	}
	return v
}

// roleAgents — число агентов команды по шаблонам.
func (s *Server) roleAgents(r *http.Request) (map[string]int, error) {
	team, err := s.St.ListTeam(r.Context())
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, a := range team {
		out[a.RoleID]++
	}
	return out, nil
}

// listRoles — шаблоны ролей любому вошедшему пользователю.
func (s *Server) listRoles(w http.ResponseWriter, r *http.Request) {
	list, err := s.St.ListRoles(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	counts, err := s.roleAgents(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	out := make([]roleView, 0, len(list))
	for _, x := range list {
		out = append(out, roleDTO(x, counts[x.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"roles": out})
}

// roleBody — тело создания и изменения шаблона.
type roleBody struct {
	ID           string                `json:"id"`
	Title        string                `json:"title"`
	Description  string                `json:"description"`
	Instruction  string                `json:"instruction"`
	Color        string                `json:"color"`
	Capability   string                `json:"capability"`
	Agent        string                `json:"agent"`
	ConnectionID string                `json:"connection_id"`
	Model        string                `json:"model"`
	DefaultModel *domain.AgentModelRef `json:"default_model"`
	Enabled      *bool                 `json:"enabled"`
	UpdatedAt    *time.Time            `json:"updated_at"`
}

func (b roleBody) input(id string) (store.RoleInput, error) {
	dm := b.DefaultModel
	if dm == nil && (b.ConnectionID != "" || b.Model != "") {
		if b.ConnectionID == "" || b.Model == "" {
			return store.RoleInput{}, &store.FieldError{Field: "model", Msg: "укажите подключение и модель"}
		}
		dm = &domain.AgentModelRef{ConnectionID: b.ConnectionID, Model: b.Model}
	}
	return store.RoleInput{
		ID: id, Title: b.Title, Description: b.Description, Instruction: b.Instruction, Color: b.Color,
		Capability: b.Capability, Agent: b.Agent, DefaultModel: dm, Enabled: b.Enabled, ExpectedUpdatedAt: b.UpdatedAt,
	}, nil
}

// createRole — POST /system/roles: собственный шаблон.
func (s *Server) createRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var b roleBody
	if err := decode(r, &b); err != nil {
		unprocessable(w, "невалидное тело запроса")
		return
	}
	in, err := b.input(b.ID)
	if err != nil {
		writeRoleErr(w, err, nil)
		return
	}
	u := currentUser(r)
	role, err := s.St.CreateRole(r.Context(), in, u.ID, u.Login)
	if err != nil {
		writeRoleErr(w, err, nil)
		return
	}
	writeJSON(w, http.StatusCreated, roleDTO(role, 0))
}

// updateRole — PUT /system/roles/{id}. Модель Тимлида — модель
// планировщика: после её смены планировщик перечитывается.
func (s *Server) updateRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var b roleBody
	if err := decode(r, &b); err != nil {
		unprocessable(w, "невалидное тело запроса")
		return
	}
	id := r.PathValue("id")
	in, err := b.input(id)
	if err != nil {
		writeRoleErr(w, err, nil)
		return
	}
	u := currentUser(r)
	role, err := s.St.UpdateRole(r.Context(), in, u.ID, u.Login)
	if err != nil {
		writeRoleErr(w, err, func() (any, error) {
			cur, err := s.St.GetRole(r.Context(), id)
			return roleDTO(cur, 0), err
		})
		return
	}
	if role.ID == domain.RoleLead && in.DefaultModel != nil {
		if err := s.ReloadPlanner(r.Context()); err != nil {
			writeErr(w, err)
			return
		}
	}
	counts, err := s.roleAgents(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roleDTO(role, counts[role.ID]))
}

func (s *Server) deleteRole(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := s.St.DeleteRole(r.Context(), r.PathValue("id"), currentUser(r).Login); err != nil {
		writeRoleErr(w, err, nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── команда ─────────────────────────────────────────────────────────────

type teamAgentView struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	RoleID           string    `json:"role_id"`
	RoleTitle        string    `json:"role_title"`
	Color            string    `json:"color"`
	Agent            string    `json:"agent"`
	ConnectionID     string    `json:"connection_id"`
	Model            string    `json:"model"`
	InstructionExtra string    `json:"instruction_extra"`
	Enabled          bool      `json:"enabled"`
	Position         int       `json:"position"`
	UpdatedAt        time.Time `json:"updated_at"`
	UpdatedBy        string    `json:"updated_by"`
	// Текущая работа: busy — ведёт сессию; задача и проект только
	// участникам проекта, runner — всем.
	Busy       bool   `json:"busy"`
	TaskID     string `json:"task_id"`
	TaskNum    int64  `json:"task_num"`
	ProjectID  string `json:"project_id"`
	EpicID     string `json:"epic_id"`
	Step       string `json:"step"`
	StepEntry  string `json:"step_entry"`
	RunnerID   string `json:"runner_id"`
	RunnerName string `json:"runner_name"`
}

func teamAgentDTO(a domain.TeamAgent, roles map[string]domain.AgentRole) teamAgentView {
	role := roles[a.RoleID]
	return teamAgentView{
		ID: a.ID, Name: a.Name, RoleID: a.RoleID, RoleTitle: role.Title, Color: role.Color,
		Agent: a.Agent, ConnectionID: a.ConnectionID, Model: a.Model, InstructionExtra: a.InstructionExtra,
		Enabled: a.Enabled, Position: a.Position, UpdatedAt: a.UpdatedAt, UpdatedBy: a.UpdatedBy,
		Busy: a.TaskID != "", TaskID: a.TaskID, TaskNum: a.TaskNum, ProjectID: a.ProjectID, EpicID: a.EpicID,
		Step: a.StepKind, StepEntry: a.StepEntry, RunnerID: a.RunnerID, RunnerName: a.RunnerName,
	}
}

func (s *Server) rolesByID(r *http.Request) (map[string]domain.AgentRole, error) {
	list, err := s.St.ListRoles(r.Context())
	if err != nil {
		return nil, err
	}
	out := make(map[string]domain.AgentRole, len(list))
	for _, x := range list {
		out[x.ID] = x
	}
	return out, nil
}

// stepDecompose — шаг агента-Тимлида, пока идёт декомпозиция.
const stepDecompose = "decompose"

// listTeam — команда любому вошедшему: задача агента видна только
// участникам её проекта.
func (s *Server) listTeam(w http.ResponseWriter, r *http.Request) {
	team, err := s.St.ListTeam(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	roles, err := s.rolesByID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	member := map[string]bool{}
	out := make([]teamAgentView, 0, len(team))
	for _, a := range team {
		v := teamAgentDTO(a, roles)
		// Тимлид составляет план (декомпозиция идёт в этом процессе rivetd).
		if d, ok := s.decompositionOf(a.ID); ok && !v.Busy {
			v.Busy, v.Step, v.EpicID, v.ProjectID = true, stepDecompose, d.EpicID, d.ProjectID
		}
		if v.ProjectID != "" {
			ok, seen := member[v.ProjectID]
			if !seen {
				ok, err = s.St.IsMember(r.Context(), v.ProjectID, currentUser(r).ID)
				if err != nil {
					writeErr(w, err)
					return
				}
				member[v.ProjectID] = ok
			}
			if !ok {
				v.TaskID, v.TaskNum, v.ProjectID, v.EpicID = "", 0, "", ""
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": out})
}

// teamBody — тело создания и изменения агента команды.
type teamBody struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	RoleID           string     `json:"role_id"`
	Agent            string     `json:"agent"`
	ConnectionID     string     `json:"connection_id"`
	Model            string     `json:"model"`
	InstructionExtra string     `json:"instruction_extra"`
	Enabled          *bool      `json:"enabled"`
	Position         *int       `json:"position"`
	UpdatedAt        *time.Time `json:"updated_at"`
}

func (b teamBody) input(id string) store.TeamAgentInput {
	return store.TeamAgentInput{
		ID: id, Name: b.Name, RoleID: b.RoleID, Agent: b.Agent, ConnectionID: b.ConnectionID, Model: b.Model,
		InstructionExtra: b.InstructionExtra, Enabled: b.Enabled, Position: b.Position, ExpectedUpdatedAt: b.UpdatedAt,
	}
}

// createTeamAgent — POST /system/team: агент из шаблона.
func (s *Server) createTeamAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var b teamBody
	if err := decode(r, &b); err != nil {
		unprocessable(w, "невалидное тело запроса")
		return
	}
	u := currentUser(r)
	a, err := s.St.CreateTeamAgent(r.Context(), b.input(b.ID), u.ID, u.Login)
	if err != nil {
		writeTeamErr(w, err, nil)
		return
	}
	s.writeTeamAgent(w, r, http.StatusCreated, a, b.RoleID == domain.RoleLead && b.Model != "")
}

// updateTeamAgent — PUT /system/team/{id}.
func (s *Server) updateTeamAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var b teamBody
	if err := decode(r, &b); err != nil {
		unprocessable(w, "невалидное тело запроса")
		return
	}
	id := r.PathValue("id")
	u := currentUser(r)
	a, err := s.St.UpdateTeamAgent(r.Context(), b.input(id), u.ID, u.Login)
	if err != nil {
		writeTeamErr(w, err, func() (any, error) {
			cur, err := s.St.GetTeamAgent(r.Context(), id)
			if err != nil {
				return nil, err
			}
			roles, err := s.rolesByID(r)
			return teamAgentDTO(cur, roles), err
		})
		return
	}
	s.writeTeamAgent(w, r, http.StatusOK, a, b.RoleID == domain.RoleLead && b.Model != "")
}

// writeTeamAgent — ответ с агентом; модель Тимлида — планировщик
// перечитывается.
func (s *Server) writeTeamAgent(w http.ResponseWriter, r *http.Request, status int, a domain.TeamAgent, reloadPlanner bool) {
	if reloadPlanner {
		if err := s.ReloadPlanner(r.Context()); err != nil {
			writeErr(w, err)
			return
		}
	}
	roles, err := s.rolesByID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, status, teamAgentDTO(a, roles))
}

func (s *Server) deleteTeamAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := s.St.DeleteTeamAgent(r.Context(), r.PathValue("id"), currentUser(r).Login); err != nil {
		writeTeamErr(w, err, nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// modelChoices — тройки «профиль, подключение, модель» для выбора модели
// агента команды и переопределения роли, с признаком подключённого runner'а.
func (s *Server) modelChoices(w http.ResponseWriter, r *http.Request) {
	list, err := s.St.ModelChoices(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"choices": list})
}

// renameRunner — имя runner'а из консоли; пустая строка снимает его.
func (s *Server) renameRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct {
		DisplayName *string `json:"display_name"`
	}
	if err := decode(r, &in); err != nil || in.DisplayName == nil {
		writeRoleErr(w, &store.FieldError{Field: "display_name", Msg: "укажите имя runner'а"}, nil)
		return
	}
	rn, err := s.St.RenameRunner(r.Context(), r.PathValue("id"), *in.DisplayName, currentUser(r).Login)
	if err != nil {
		writeRoleErr(w, err, nil)
		return
	}
	roles, err := s.St.ListRoles(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	rn.Roles = runnerRoles(rn, roles)
	writeJSON(w, http.StatusOK, rn)
}

// writeRoleErr — ошибки шаблонов и команды. Поле, ссылки и актуальная
// версия лежат в объекте error (их читает консоль), поле и ссылки — ещё и
// рядом, как у остальных каталогов.
func writeRoleErr(w http.ResponseWriter, err error, current func() (any, error)) {
	writeCatalogErr(w, err, current, "role_exists")
}

// writeTeamErr — ошибки команды: занятый идентификатор — agent_exists.
func writeTeamErr(w http.ResponseWriter, err error, current func() (any, error)) {
	writeCatalogErr(w, err, current, "agent_exists")
}

func writeCatalogErr(w http.ResponseWriter, err error, current func() (any, error), existsCode string) {
	var fe *store.FieldError
	var inUse *store.ErrRoleInUse
	switch {
	case errors.As(err, &fe):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": map[string]any{"code": "invalid", "message": fe.Msg, "field": fe.Field}, "field": fe.Field,
		})
	case errors.As(err, &inUse):
		msg := "шаблон используется: уберите его из процессов и команды или оставьте включённым"
		if inUse.Reason == store.RoleInUseProfile {
			msg = "у нового профиля нет моделей агентов команды: переведите этих агентов на модель, которая есть у нового профиля"
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{"code": "role_in_use", "message": msg, "refs": inUse.Refs},
			"refs":  inUse.Refs,
		})
	case errors.Is(err, store.ErrRoleBuiltin):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{"code": "role_builtin", "message": err.Error()},
		})
	case errors.Is(err, store.ErrExists):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{"code": existsCode, "message": "идентификатор уже занят", "field": "id"}, "field": "id",
		})
	case errors.Is(err, store.ErrStale):
		e := map[string]any{"code": "stale", "message": err.Error()}
		if current != nil {
			if cur, cerr := current(); cerr == nil {
				e["current"] = cur
			}
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": e})
	default:
		writeErr(w, err)
	}
}

// runnerRoles — включённые шаблоны, которые runner может выполнять по
// capability (шаблоны control plane runner'у не нужны).
func runnerRoles(rn domain.Runner, roles []domain.AgentRole) []string {
	out := []string{}
	for _, role := range roles {
		if !role.Enabled || role.Capability == "" || !containsCap(rn.Capabilities, role.Capability) {
			continue
		}
		if role.Agent != "" && rn.Agent != role.Agent {
			continue
		}
		out = append(out, role.ID)
	}
	return out
}

func containsCap(caps []string, c string) bool {
	for _, x := range caps {
		if x == c {
			return true
		}
	}
	return false
}
