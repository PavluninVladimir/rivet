package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Каталог ролей агентов (add-agent-roles, спека backend/agent-roles): кем
// агент выступает в процессе. Роль даёт агенту инструкцию, capability и
// модель по умолчанию. Модель роли lead — настройка планировщика
// (planner_model): на неё завязаны проверки ссылок подключений, поэтому в
// строке lead она не хранится, а подставляется при чтении.

// RoleInput — что сохраняет администратор. Enabled nil — не менять
// (при создании — включена).
type RoleInput struct {
	ID           string
	Title        string
	Description  string
	Instruction  string
	Color        string
	Capability   string
	Agent        string
	DefaultModel *domain.AgentModelRef
	Enabled      *bool
	// ExpectedUpdatedAt — версия, которую видел клиент (изменение): при
	// расхождении сохранение отклоняется ErrStale.
	ExpectedUpdatedAt *time.Time
}

// ErrRoleBuiltin — встроенную роль нельзя удалить, только отключить.
var ErrRoleBuiltin = errors.New("встроенную роль нельзя удалить, только отключить")

const roleCols = `r.id, r.title, r.description, r.instruction, r.color, r.capability, r.agent, r.default_model,
	r.enabled, r.builtin, r.position, r.updated_at, COALESCE(u.login, '')`

func scanRole(row pgx.Row) (domain.AgentRole, error) {
	var r domain.AgentRole
	var dm []byte
	err := row.Scan(&r.ID, &r.Title, &r.Description, &r.Instruction, &r.Color, &r.Capability, &r.Agent, &dm,
		&r.Enabled, &r.Builtin, &r.Position, &r.UpdatedAt, &r.UpdatedBy)
	if err != nil {
		return r, err
	}
	if len(dm) > 0 && string(dm) != "null" {
		var m domain.AgentModelRef
		if json.Unmarshal(dm, &m) == nil && m.Model != "" {
			r.DefaultModel = &m
		}
	}
	return r, nil
}

// withPlannerModel подставляет роли lead модель планировщика.
func (s *Store) withPlannerModel(ctx context.Context, r *domain.AgentRole) error {
	if r.ID != domain.RoleLead {
		return nil
	}
	pm, ok, err := s.PlannerModel(ctx)
	if err != nil {
		return err
	}
	r.DefaultModel = nil
	if ok {
		r.DefaultModel = &domain.AgentModelRef{ConnectionID: pm.ConnectionID, Model: pm.Model}
	}
	return nil
}

// ListRoles — каталог ролей: встроенные в порядке конвейера, затем
// собственные по идентификатору.
func (s *Store) ListRoles(ctx context.Context) ([]domain.AgentRole, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+roleCols+` FROM agent_roles r LEFT JOIN users u ON u.id=r.updated_by
		ORDER BY r.position, r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AgentRole{}
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if err := s.withPlannerModel(ctx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetRole — роль по идентификатору.
func (s *Store) GetRole(ctx context.Context, id string) (domain.AgentRole, error) {
	r, err := scanRole(s.Pool.QueryRow(ctx, `SELECT `+roleCols+` FROM agent_roles r LEFT JOIN users u ON u.id=r.updated_by WHERE r.id=$1`, id))
	if err != nil {
		return r, nf(err)
	}
	return r, s.withPlannerModel(ctx, &r)
}

func validateRole(in RoleInput) error {
	if !policy.ValidRoleID(in.ID) {
		return &FieldError{Field: "id", Msg: "идентификатор: латиница, цифры, дефис, от 2 до 32 символов, начинается с буквы"}
	}
	if n := utf8.RuneCountInString(in.Title); n < 1 || n > 40 {
		return &FieldError{Field: "title", Msg: "название от 1 до 40 символов"}
	}
	if strings.ContainsFunc(in.Title, isControl) {
		return &FieldError{Field: "title", Msg: "название в одну строку"}
	}
	if utf8.RuneCountInString(in.Description) > 500 {
		return &FieldError{Field: "description", Msg: "описание до 500 символов"}
	}
	if strings.TrimSpace(in.Instruction) == "" {
		return &FieldError{Field: "instruction", Msg: "укажите инструкцию агенту"}
	}
	if utf8.RuneCountInString(in.Instruction) > 8000 {
		return &FieldError{Field: "instruction", Msg: "инструкция до 8000 символов"}
	}
	if !containsStr(domain.RoleColors, in.Color) {
		return &FieldError{Field: "color", Msg: "цвет из палитры: " + strings.Join(domain.RoleColors, ", ")}
	}
	if !containsStr(domain.RoleCapabilities, in.Capability) {
		return &FieldError{Field: "capability", Msg: "capability: coding, review, explore или пусто для роли control plane"}
	}
	// Тимлид и Аналитик работают на control plane (запрос к модели): runner'у
	// им нечего делать. Остальным встроенным ролям без capability не найти
	// runner'а; собственная роль без capability — роль control plane.
	cp := domain.ControlPlaneRole(in.ID)
	if cp && in.Capability != "" {
		return &FieldError{Field: "capability", Msg: "роль выполняет control plane: capability не задаётся"}
	}
	if !cp && in.Capability == "" && isBuiltinRunnerRole(in.ID) {
		return &FieldError{Field: "capability", Msg: "укажите capability: coding, review или explore"}
	}
	if in.Capability == "" && in.Agent != "" {
		return &FieldError{Field: "agent", Msg: "роль control plane не запускает агента на runner'е: профиль не задаётся"}
	}
	return nil
}

// isBuiltinRunnerRole — встроенная роль, которую выполняет runner.
func isBuiltinRunnerRole(id string) bool {
	switch id {
	case domain.RoleArchitect, domain.RoleDeveloper, domain.RoleTester, domain.RoleReviewer:
		return true
	}
	return false
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || r == '\u2028' || r == '\u2029' }

// ErrExists — запись с таким идентификатором уже есть (создание).
var ErrExists = errors.New("запись с таким идентификатором уже есть")

// ErrStale — запись изменили после того, как её прочитал клиент
// (необязательная оптимистическая проверка по updated_at).
var ErrStale = errors.New("запись изменена в другом месте: обновите и повторите")

// Режимы сохранения: создать или изменить; авто — что получится.
const (
	saveAuto = iota
	saveCreate
	saveUpdate
)

// CreateRole создаёт собственный шаблон роли; занятый идентификатор —
// ErrExists.
func (s *Store) CreateRole(ctx context.Context, in RoleInput, actorID, actorLogin string) (domain.AgentRole, error) {
	r, _, err := s.saveRole(ctx, in, saveCreate, actorID, actorLogin)
	return r, err
}

// UpdateRole меняет шаблон (кроме идентификатора): нет шаблона —
// ErrNotFound, ExpectedUpdatedAt не совпал с текущим — ErrStale.
func (s *Store) UpdateRole(ctx context.Context, in RoleInput, actorID, actorLogin string) (domain.AgentRole, error) {
	r, _, err := s.saveRole(ctx, in, saveUpdate, actorID, actorLogin)
	return r, err
}

// UpsertRole — создание или изменение (внутренние вызовы и тесты).
func (s *Store) UpsertRole(ctx context.Context, in RoleInput, actorID, actorLogin string) (domain.AgentRole, bool, error) {
	return s.saveRole(ctx, in, saveAuto, actorID, actorLogin)
}

// sameVersion — версия записи совпадает с ожидаемой клиентом (точность БД —
// микросекунды).
func sameVersion(cur time.Time, expected *time.Time) bool {
	return expected == nil || cur.Truncate(time.Microsecond).Equal(expected.Truncate(time.Microsecond))
}

// saveRole сохраняет шаблон. Модель шаблона — привязка его профиля; у
// шаблона без профиля — модель включённого подключения. Модель Тимлида —
// настройка планировщика: меняется, только если тело явно несёт другую
// модель. Выключение шаблона, на который ссылаются процессы, отклоняется.
func (s *Store) saveRole(ctx context.Context, in RoleInput, mode int, actorID, actorLogin string) (domain.AgentRole, bool, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Description = strings.TrimSpace(in.Description)
	in.Instruction = strings.TrimSpace(in.Instruction)
	in.Agent = strings.TrimSpace(in.Agent)
	if in.Color == "" {
		in.Color = "slate"
	}
	if err := validateRole(in); err != nil {
		return domain.AgentRole{}, false, err
	}
	created := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockRoles(ctx, tx); err != nil {
			return err
		}
		var exists, wasEnabled bool
		var updatedAt time.Time
		var wasCapability, wasAgent string
		err := tx.QueryRow(ctx, `SELECT true, enabled, updated_at, capability, agent FROM agent_roles WHERE id=$1 FOR UPDATE`, in.ID).
			Scan(&exists, &wasEnabled, &updatedAt, &wasCapability, &wasAgent)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		switch {
		case mode == saveCreate && exists:
			return ErrExists
		case mode == saveUpdate && !exists:
			return ErrNotFound
		case exists && mode != saveCreate && !sameVersion(updatedAt, in.ExpectedUpdatedAt):
			return ErrStale
		}
		// Модель проверяется тройкой «профиль, подключение, модель»; модель
		// Тимлида — теми же правилами, что у настройки планировщика: запись
		// настройки после транзакции шаблона не должна отказать.
		if in.Agent != "" || in.DefaultModel != nil {
			connID, model := "", ""
			if in.DefaultModel != nil {
				connID, model = in.DefaultModel.ConnectionID, in.DefaultModel.Model
			}
			if err := checkAgentModel(ctx, tx, in.Agent, connID, model, "model"); err != nil {
				var fe *FieldError
				if in.DefaultModel == nil && errors.As(err, &fe) {
					fe.Field = "agent"
				}
				return err
			}
		}
		// Выключение шаблона с действующими ссылками оставило бы процессы с
		// участником, который молча потерял роль.
		enabled := true
		if exists {
			enabled = wasEnabled
		}
		if in.Enabled != nil {
			enabled = *in.Enabled
		}
		// Выключение шаблона или перевод его на control plane (пустая
		// capability) при живых ссылках оставил бы участников и агентов
		// без работающей роли.
		if exists && wasEnabled && (!enabled || (wasCapability != "" && in.Capability == "")) {
			refs, err := roleRefs(ctx, tx, in.ID)
			if err != nil {
				return err
			}
			if len(refs) > 0 {
				return &ErrRoleInUse{Refs: refs}
			}
		}
		// Смена профиля шаблона: агенты команды без своего профиля берут
		// профиль шаблона, их модель должна остаться привязкой нового.
		if exists && in.Agent != wasAgent && in.Agent != "" {
			refs, err := templateProfileRefs(ctx, tx, in.ID, in.Agent)
			if err != nil {
				return err
			}
			if len(refs) > 0 {
				return &ErrRoleInUse{Refs: refs, Reason: RoleInUseProfile}
			}
		}
		var dm []byte
		if in.DefaultModel != nil && in.ID != domain.RoleLead {
			dm, _ = json.Marshal(in.DefaultModel)
		}
		if !exists {
			created = true
			if _, err := tx.Exec(ctx, `
				INSERT INTO agent_roles (id, title, description, instruction, color, capability, agent, default_model, enabled, updated_by, position)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,(SELECT COALESCE(max(position), 0) + 10 FROM agent_roles))`,
				in.ID, in.Title, in.Description, in.Instruction, in.Color, in.Capability, in.Agent, dm, enabled, nullUUID(actorID)); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `
				UPDATE agent_roles SET title=$2, description=$3, instruction=$4, color=$5, capability=$6, agent=$7,
					default_model=$8, enabled=$9, updated_at=now(), updated_by=$10 WHERE id=$1`,
			in.ID, in.Title, in.Description, in.Instruction, in.Color, in.Capability, in.Agent, dm, enabled, nullUUID(actorID)); err != nil {
			return err
		}
		typ, text := "role.updated", "изменён шаблон роли «"+in.Title+"»"
		if created {
			typ, text = "role.created", "создан шаблон роли «"+in.Title+"»"
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: typ, Text: text,
			Payload: map[string]any{"id": in.ID, "enabled": enabled, "capability": in.Capability},
		})
		return err
	})
	if err != nil {
		return domain.AgentRole{}, false, err
	}
	// Модель Тимлида — настройка планировщика: своя транзакция с той же
	// блокировкой, что у удаления подключения и выбора модели. Меняется,
	// только если тело явно несёт другую модель: обычное сохранение формы не
	// должно откатывать модель, выбранную в другом месте.
	if in.ID == domain.RoleLead && in.DefaultModel != nil {
		if err := s.setPlannerIfChanged(ctx, in.DefaultModel.ConnectionID, in.DefaultModel.Model, actorID, actorLogin); err != nil {
			return domain.AgentRole{}, created, err
		}
	}
	out, err := s.GetRole(ctx, in.ID)
	return out, created, err
}

// setPlannerIfChanged выбирает модель планировщика, если она отличается от
// текущей.
func (s *Store) setPlannerIfChanged(ctx context.Context, connID, model, actorID, actorLogin string) error {
	cur, ok, err := s.PlannerModel(ctx)
	if err != nil {
		return err
	}
	if ok && cur.ConnectionID == connID && cur.Model == model {
		return nil
	}
	return s.SetPlannerModel(ctx, &domain.PlannerModel{ConnectionID: connID, Model: model}, actorID, actorLogin)
}

// nullUUID — пустой идентификатор пользователя как NULL (вызовы без актора).
func nullUUID(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

// DeleteRole удаляет собственную роль; встроенную — только отключить,
// роль со ссылками из процессов не удаляется.
func (s *Store) DeleteRole(ctx context.Context, id, actorLogin string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockRoles(ctx, tx); err != nil {
			return err
		}
		var builtin bool
		var title string
		if err := tx.QueryRow(ctx, `SELECT builtin, title FROM agent_roles WHERE id=$1 FOR UPDATE`, id).Scan(&builtin, &title); err != nil {
			return nf(err)
		}
		if builtin {
			return ErrRoleBuiltin
		}
		refs, err := roleRefs(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(refs) > 0 {
			return &ErrRoleInUse{Refs: refs}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM agent_roles WHERE id=$1`, id); err != nil {
			return err
		}
		_, err = appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "role.deleted",
			Text: "удалена роль «" + title + "»", Payload: map[string]any{"id": id},
		})
		return err
	})
}

// RoleRef — ссылка на шаблон роли (отказ удаления или выключения): шаг
// процесса установки или проекта (kind participant), переопределение модели
// роли в проекте (kind role_model, шага нет) или агент команды (kind agent).
type RoleRef struct {
	Scope       string `json:"scope"`
	Kind        string `json:"kind"`
	ProjectID   string `json:"project_id,omitempty"`
	ProjectName string `json:"project_name,omitempty"`
	Step        string `json:"step"`
	AgentID     string `json:"agent_id,omitempty"`
	AgentName   string `json:"agent_name,omitempty"`
}

// ErrRoleInUse — на шаблон ссылаются процессы или агенты команды.
type ErrRoleInUse struct {
	Refs []RoleRef
	// Reason — почему правка отклонена: пусто — шаблон используется
	// процессами, переопределениями или командой; RoleInUseProfile — смена
	// профиля шаблона без моделей его агентов команды.
	Reason string
}

// RoleInUseProfile — отказ смены профиля шаблона: модели агентов команды без
// своего профиля нет в привязках нового.
const RoleInUseProfile = "profile"

func (e *ErrRoleInUse) Error() string { return "шаблон роли используется" }

// lockRoles — общий замок шаблонов ролей: правка шаблона и сохранение
// политики с ролями сериализуются, проверка ссылок не проскакивает между
// чтением и записью.
func lockRoles(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('agent_roles'))`)
	return err
}

// roleRefs — ссылки на шаблон: шаги процессов (явных и по умолчанию с учётом
// флага тестировщика) в действующих политиках установки и проектов,
// переопределения модели роли в проектах и агенты команды по шаблону.
func roleRefs(ctx context.Context, tx pgx.Tx, id string) ([]RoleRef, error) {
	rows, err := tx.Query(ctx, `
		SELECT v.scope, COALESCE(v.project_id::text, ''), COALESCE(p.name, ''), v.content
		FROM (SELECT DISTINCT ON (scope, project_id) scope, project_id, content FROM policy_versions
		      ORDER BY scope, project_id, version DESC) v
		LEFT JOIN projects p ON p.id = v.project_id
		ORDER BY v.scope, p.name, v.project_id`)
	if err != nil {
		return nil, err
	}
	type version struct {
		scope, projectID, projectName string
		raw                           []byte
	}
	var versions []version
	hasInstallation := false
	for rows.Next() {
		var v version
		if err := rows.Scan(&v.scope, &v.projectID, &v.projectName, &v.raw); err != nil {
			rows.Close()
			return nil, err
		}
		if v.scope == PolicyScopeInstallation {
			hasInstallation = true
		}
		versions = append(versions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Процесс по умолчанию проекта (свой флаг тестировщика) действует,
	// только если у установки нет явного процесса: иначе проект наследует
	// явный процесс установки, и флаг на него не влияет.
	instExplicit := false
	for _, v := range versions {
		if v.scope != PolicyScopeInstallation {
			continue
		}
		var pr policy.Presets
		if err := json.Unmarshal(v.raw, &pr); err != nil {
			return nil, err
		}
		instExplicit = pr.Process != nil
	}
	var refs []RoleRef
	steps := func(scope, pid, pname string, doc policy.Process) {
		for _, st := range doc.Steps {
			for _, part := range st.Participants {
				if part.Agent != nil && part.Agent.Role == id {
					refs = append(refs, RoleRef{Scope: scope, Kind: "participant", ProjectID: pid, ProjectName: pname, Step: st.ID})
					break
				}
			}
		}
	}
	// Установка без своей версии работает на процессе по умолчанию.
	if !hasInstallation {
		steps(PolicyScopeInstallation, "", "", policy.DefaultProcess())
	}
	for _, v := range versions {
		if v.scope == PolicyScopeInstallation {
			var pr policy.Presets
			if err := json.Unmarshal(v.raw, &pr); err != nil {
				return nil, err
			}
			doc := policy.DefaultProcessFor(pr.TesterEnabled())
			if pr.Process != nil {
				doc = *pr.Process
			}
			steps(PolicyScopeInstallation, "", "", doc)
			continue
		}
		var o policy.Overrides
		if err := json.Unmarshal(v.raw, &o); err != nil {
			return nil, err
		}
		switch {
		case o.Process != nil:
			steps(PolicyScopeProject, v.projectID, v.projectName, *o.Process)
		case o.TesterAgent != nil && !instExplicit:
			// Свой флаг тестировщика — свой процесс по умолчанию.
			steps(PolicyScopeProject, v.projectID, v.projectName, policy.DefaultProcessFor(*o.TesterAgent))
		}
		if _, ok := o.RoleModels[id]; ok {
			refs = append(refs, RoleRef{Scope: PolicyScopeProject, Kind: "role_model", ProjectID: v.projectID, ProjectName: v.projectName})
		}
	}
	arows, err := tx.Query(ctx, `SELECT id, name FROM team_agents WHERE role_id=$1 ORDER BY position, id`, id)
	if err != nil {
		return nil, err
	}
	defer arows.Close()
	for arows.Next() {
		var aid, name string
		if err := arows.Scan(&aid, &name); err != nil {
			return nil, err
		}
		refs = append(refs, RoleRef{Scope: PolicyScopeInstallation, Kind: "agent", AgentID: aid, AgentName: name})
	}
	return refs, arows.Err()
}

// validateProcessRolesTx — роли участников процесса по шаблонам внутри
// транзакции сохранения политики (под lockRoles).
func validateProcessRolesTx(ctx context.Context, tx pgx.Tx, p *policy.Process) error {
	if p == nil {
		return nil
	}
	for _, st := range p.Steps {
		for i, part := range st.Participants {
			if part.Agent == nil || part.Agent.Role == "" {
				continue
			}
			field := fmt.Sprintf("participants[%d].agent.role", i)
			var enabled bool
			var capability string
			err := tx.QueryRow(ctx, `SELECT enabled, capability FROM agent_roles WHERE id=$1`, part.Agent.Role).Scan(&enabled, &capability)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return &policy.ProcessError{Step: st.ID, Field: field, Msg: "роли " + part.Agent.Role + " нет в каталоге"}
			case err != nil:
				return err
			case !enabled:
				return &policy.ProcessError{Step: st.ID, Field: field, Msg: "роль " + part.Agent.Role + " отключена"}
			case capability == "":
				return &policy.ProcessError{Step: st.ID, Field: field, Msg: "роль " + part.Agent.Role + " выполняет control plane и не может быть участником шага"}
			}
		}
	}
	return nil
}

// ValidateProcessRoles проверяет роли участников процесса по каталогу:
// роль есть и включена (спека process «Участники шага»).
func (s *Store) ValidateProcessRoles(ctx context.Context, p *policy.Process) error {
	if p == nil {
		return nil
	}
	roles, err := s.ListRoles(ctx)
	if err != nil {
		return err
	}
	byID := map[string]domain.AgentRole{}
	for _, r := range roles {
		byID[r.ID] = r
	}
	for _, st := range p.Steps {
		for i, part := range st.Participants {
			if part.Agent == nil || part.Agent.Role == "" {
				continue
			}
			field := fmt.Sprintf("participants[%d].agent.role", i)
			r, ok := byID[part.Agent.Role]
			switch {
			case !ok:
				return &policy.ProcessError{Step: st.ID, Field: field, Msg: "роли " + part.Agent.Role + " нет в каталоге"}
			case !r.Enabled:
				return &policy.ProcessError{Step: st.ID, Field: field, Msg: "роль " + part.Agent.Role + " отключена"}
			case r.Capability == "":
				return &policy.ProcessError{Step: st.ID, Field: field, Msg: "роль " + part.Agent.Role + " выполняет control plane и не может быть участником шага"}
			}
		}
	}
	return nil
}

// ValidateRoleModels проверяет переопределения моделей ролей проекта: роль
// есть, тройка «профиль, подключение, модель» — привязка профиля (профиль
// по умолчанию — профиль шаблона) через включённое подключение.
func (s *Store) ValidateRoleModels(ctx context.Context, rm map[string]policy.RoleModel) error {
	for id, m := range rm {
		field := "role_models." + id + ".model"
		r, err := s.GetRole(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return &policy.ProcessError{Field: "role_models." + id, Msg: "роли " + id + " нет в каталоге"}
		}
		if err != nil {
			return err
		}
		if !r.Enabled {
			return &policy.ProcessError{Field: "role_models." + id, Msg: "роль " + id + " отключена"}
		}
		if r.Capability == "" {
			return &policy.ProcessError{Field: "role_models." + id, Msg: "роль " + id + " выполняет control plane: модель задаётся настройкой планировщика"}
		}
		agent := m.Agent
		if agent == "" {
			agent = r.Agent
		}
		if err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			return checkAgentModel(ctx, tx, agent, m.ConnectionID, m.Model, field)
		}); err != nil {
			var fe *FieldError
			if errors.As(err, &fe) {
				return &policy.ProcessError{Field: field, Msg: fe.Msg}
			}
			return err
		}
	}
	return nil
}

// RoleBinding — роль участника, разрешённая по шаблону на входе в шаг
// (add-agent-roles): capability и профиль шаблона, предпочтения проекта
// (профиль и модель из переопределения роли, иначе модель шаблона), есть ли
// в команде агенты роли. Пустая Role — участник без роли или шаблон
// отключён: поведение как до появления ролей. Team — стадию ведёт свободный
// агент команды; без агентов участник работает по шаблону через capability.
type RoleBinding struct {
	Role       string
	Capability string
	Agent      string
	PrefAgent  string
	PrefModel  string
	Team       bool
}

// ResolveRole — привязка роли участника для проекта: шаблон (включённый,
// не control plane). Профиль шаблона обязателен только без своего профиля
// агента команды; профиль и модель переопределения проекта — предпочтение,
// иначе предпочтение — модель шаблона.
func ResolveRole(r domain.AgentRole, ok bool, override *policy.RoleModel, team bool) RoleBinding {
	if !ok || !r.Enabled || r.Capability == "" {
		return RoleBinding{}
	}
	b := RoleBinding{Role: r.ID, Capability: r.Capability, Agent: r.Agent, Team: team}
	switch {
	case override != nil:
		b.PrefModel, b.PrefAgent = override.Model, override.Agent
	case r.DefaultModel != nil:
		b.PrefModel = r.DefaultModel.Model
	}
	return b
}

// RenameRunner задаёт отображаемое имя runner'а из консоли (спека
// agent-roles «Имя агента»): имя закрепляется и регистрацией не
// перезаписывается. Пустое имя снимает закрепление: действует имя из
// регистрации.
func (s *Store) RenameRunner(ctx context.Context, id, name, actorLogin string) (domain.Runner, error) {
	name = strings.TrimSpace(name)
	if name != "" && (utf8.RuneCountInString(name) > 40 || strings.ContainsFunc(name, isControl)) {
		return domain.Runner{}, &FieldError{Field: "display_name", Msg: "имя от 1 до 40 символов в одну строку"}
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var tag string
		if name == "" {
			tag = `UPDATE runners SET display_name=registered_name, display_name_locked=false WHERE id=$1 RETURNING display_name`
		} else {
			tag = `UPDATE runners SET display_name=$2, display_name_locked=true WHERE id=$1 RETURNING display_name`
		}
		var shown string
		args := []any{id}
		if name != "" {
			args = append(args, name)
		}
		if err := tx.QueryRow(ctx, tag, args...).Scan(&shown); err != nil {
			return nf(err)
		}
		text := "runner " + id + " переименован: " + shown
		if shown == "" {
			text = "у runner'а " + id + " снято имя из консоли"
		}
		_, err := appendEvent(ctx, tx, EventInput{
			ActorKind: domain.ActorUser, ActorID: actorLogin, Type: "runner.renamed", Text: text,
			Payload: map[string]any{"runner": id, "display_name": shown},
		})
		return err
	})
	if err != nil {
		return domain.Runner{}, err
	}
	return s.GetRunner(ctx, id)
}

// templateProfileRefs — агенты шаблона без своего профиля, чья модель не
// привязана к новому профилю шаблона.
func templateProfileRefs(ctx context.Context, tx pgx.Tx, roleID, agentID string) ([]RoleRef, error) {
	a, err := scanAgent(tx.QueryRow(ctx, `SELECT `+agentCols+` FROM agents a LEFT JOIN users u ON u.id=a.updated_by WHERE a.id=$1`, agentID))
	if err != nil {
		return nil, nf(err)
	}
	bound := map[string]bool{}
	for _, b := range a.Models {
		bound[b.ConnectionID+"/"+b.Model] = true
	}
	rows, err := tx.Query(ctx, `SELECT id, name, connection_id, model FROM team_agents
		WHERE role_id=$1 AND agent='' AND model <> '' ORDER BY position, id`, roleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []RoleRef
	for rows.Next() {
		var id, name, conn, model string
		if err := rows.Scan(&id, &name, &conn, &model); err != nil {
			return nil, err
		}
		if len(a.Models) > 0 && !bound[conn+"/"+model] {
			refs = append(refs, RoleRef{Scope: PolicyScopeInstallation, Kind: "agent", AgentID: id, AgentName: name})
		}
	}
	return refs, rows.Err()
}
