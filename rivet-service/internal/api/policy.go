package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/policy"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Политики конвейера пресетами (change add-policy-presets, api-contract):
// пресеты установки меняет администратор, переопределения проекта — owner,
// участник читает действующую политику. У автоматики идентичности в API
// нет, поэтому «агент не может изменить политику» выполняется конструкцией.

func (s *Server) getInstallationPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	p, v, err := s.St.InstallationPolicy(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": v, "presets": p, "engine": s.engineView(r)})
}

func (s *Server) putInstallationPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.policyExternal(w) || s.policyBlocks(w, r, actionPolicyWrite, "") {
		return
	}
	var in policy.Presets
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидный JSON")
		return
	}
	v, err := s.St.SaveInstallationPolicy(r.Context(), in, user(r))
	if err != nil {
		writeProcessErr(w, err)
		return
	}
	// Пресеты — из сохранённой версии, а не повторным чтением активной:
	// параллельный PUT не должен подменить их в ответе.
	var p policy.Presets
	if err := json.Unmarshal(v.Content, &p); err != nil {
		writeErr(w, err)
		return
	}
	// Проекты, чьи процессы не соответствуют новым ограничениям
	// (спека process «Ограничение ужесточили позже»): информативно.
	// Политика уже сохранена: сбой подсчёта нарушений не делает ответ
	// ошибкой, список остаётся пустым, причина в логе.
	violations, err := s.St.LockViolations(r.Context(), p.Locks())
	if err != nil {
		slog.Error("нарушения ограничений процесса", "err", err)
		violations = []store.LockViolation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": v, "presets": p.Normalize(), "violations": violations})
}

func (s *Server) listInstallationPolicyVersions(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	out, err := s.St.ListPolicyVersions(r.Context(), store.PolicyScopeInstallation, "")
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// projectPolicyView — форма GET/PUT /projects/{id}/policy.
type projectPolicyView struct {
	Effective           policy.Presets       `json:"effective"`
	EffectiveHash       string               `json:"effective_hash"`
	Overrides           policy.Overrides     `json:"overrides"`
	Version             *store.PolicyVersion `json:"version"`
	InstallationVersion *store.PolicyVersion `json:"installation_version"`
	// Engine — режим движка: в external правка пресетов проекта тоже
	// отключена, и консоль должна это показать, а не ловить 409.
	Engine engineView `json:"engine"`
	// Source — источник политики проекта: собственное хранилище или файл
	// доверенной ветки репозитория (api-contract add-policy-git-provider).
	Source policySourceView `json:"source"`
	// ProcessSource — чей процесс действует: project или installation
	// (api-contract add-process-model).
	ProcessSource string `json:"process_source"`
	// AgentModels — действующие модели по умолчанию профилей агентов для
	// проекта (add-agent-profiles): установка (профиль) и переопределение.
	AgentModels map[string]agentModelView `json:"agent_models"`
}

// agentModelView — модель агента для проекта: значение установки из
// профиля, действующее значение и его источник.
type agentModelView struct {
	Name         string                 `json:"name"`
	Installation *domain.AgentModelRef  `json:"installation"`
	Effective    *domain.AgentModelRef  `json:"effective"`
	Source       string                 `json:"source"` // installation | project
	Models       []domain.AgentModelRef `json:"models"`
}

// sourceLabel — человекочитаемое имя источника политики.
func sourceLabel(kind string) string {
	if kind == policy.SourceGit {
		return "репозиторий проекта (" + policy.PolicyFile + ")"
	}
	return "хранилище Rivet"
}

// policySourceView — источник политики проекта для консоли.
type policySourceView struct {
	Kind string `json:"kind"`
	File string `json:"file"`
	Ref  string `json:"ref"`
}

func (s *Server) writeProjectPolicy(w http.ResponseWriter, r *http.Request, eff store.EffectivePolicy) {
	processSource := "installation"
	if eff.Overrides.Process != nil {
		processSource = "project"
	}
	view := projectPolicyView{
		Effective: eff.Presets, EffectiveHash: eff.Hash, Overrides: eff.Overrides,
		ProcessSource: processSource,
		Version:       eff.Project, InstallationVersion: eff.Installation,
		Engine: s.engineView(r),
		Source: policySourceView{Kind: policy.SourceStore},
	}
	view.AgentModels = map[string]agentModelView{}
	if agents, err := s.St.ListAgents(r.Context()); err == nil {
		for _, a := range agents {
			if !a.Enabled {
				continue
			}
			v := agentModelView{Name: a.Name, Installation: a.DefaultModel, Effective: a.DefaultModel, Source: "installation", Models: a.Models}
			if ov, ok := eff.Presets.AgentModels[a.ID]; ok {
				m := domain.AgentModelRef{ConnectionID: ov.ConnectionID, Model: ov.Model}
				v.Effective, v.Source = &m, "project"
			}
			view.AgentModels[a.ID] = v
		}
	}
	if p, err := s.St.GetProject(r.Context(), r.PathValue("id")); err == nil && p.PolicySource == policy.SourceGit {
		view.Source = policySourceView{Kind: policy.SourceGit, File: policy.PolicyFile, Ref: p.DefaultBranch}
	}
	writeJSON(w, http.StatusOK, view)
}

// PUT /projects/{id}/policy/source — источник политики проекта: правка из
// консоли или файл доверенной ветки. Включение git-провайдера требует
// защищённой ветки: без неё политику меняет любой push (спека
// access-policy «Защита от самоослабления»).
func (s *Server) putProjectPolicySource(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireOwner(w, r, projectID) {
		return
	}
	var in struct {
		Kind string `json:"kind"`
	}
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидный JSON")
		return
	}
	if in.Kind != policy.SourceStore && in.Kind != policy.SourceGit {
		unprocessable(w, "kind: ожидается store или git")
		return
	}
	// Смена источника — такая же правка политики, как и правка значений:
	// во внешнем режиме её нет, и движок может её запретить.
	if s.policyExternal(w) || s.policyBlocks(w, r, actionPolicyWrite, projectID) {
		return
	}
	p, err := s.St.GetProject(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if in.Kind == policy.SourceGit {
		adapter, err := s.Engine.SCMFor(r.Context(), p)
		if err != nil {
			writeErr(w, err)
			return
		}
		ref := p.DefaultBranch
		protected, err := adapter.BranchProtected(r.Context(), p.Repo(), ref)
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]apiError{"error": {
				Code: "conflict", Message: "хостинг не ответил о защите ветки " + ref + ": " + err.Error()}})
			return
		}
		if !protected {
			writeJSON(w, http.StatusConflict, map[string]apiError{"error": {
				Code: "conflict", Message: "ветка " + ref + " не защищена на хостинге: политику мог бы изменить любой push без ревью"}})
			return
		}
	}
	if err := s.St.SetProjectPolicySource(r.Context(), projectID, in.Kind); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.St.AppendEvent(r.Context(), store.EventInput{
		ActorKind: domain.ActorUser, ActorID: user(r), Type: "policy.source",
		ProjectID: projectID,
		Text:      "источник политики проекта: " + sourceLabel(in.Kind),
		Payload:   map[string]any{"kind": in.Kind, "file": policy.PolicyFile, "ref": p.DefaultBranch},
	}); err != nil {
		writeErr(w, err)
		return
	}
	eff, err := s.St.EffectivePolicy(r.Context(), projectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.writeProjectPolicy(w, r, eff)
}

func (s *Server) getProjectPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireMember(w, r, r.PathValue("id")) {
		return
	}
	eff, err := s.St.EffectivePolicy(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.writeProjectPolicy(w, r, eff)
}

func (s *Server) putProjectPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireOwner(w, r, r.PathValue("id")) {
		return
	}
	if s.policyExternal(w) || s.policyBlocks(w, r, actionPolicyWrite, r.PathValue("id")) {
		return
	}
	// Политика из репозитория меняется коммитом: двух источников правды
	// быть не должно (спека access-policy «Политика проекта из репозитория»).
	// Ошибка чтения проекта — отказ, а не пропуск проверки: иначе сбой
	// базы открывал бы обход запрета.
	p, err := s.St.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	if p.PolicySource == policy.SourceGit {
		writeJSON(w, http.StatusConflict, map[string]apiError{"error": {
			Code: "policy_from_git", Message: "политика проекта хранится в репозитории: меняйте её коммитом в " + policy.PolicyFile}})
		return
	}
	var raw json.RawMessage
	if err := decode(r, &raw); err != nil {
		unprocessable(w, "невалидный JSON")
		return
	}
	var in policy.Overrides
	if err := json.Unmarshal(raw, &in); err != nil {
		unprocessable(w, "невалидный JSON")
		return
	}
	// Тело без ключа process не трогает процесс проекта (api-contract):
	// старые клиенты правят пресеты, не сбрасывая процесс на установку.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err == nil {
		if _, has := keys["process"]; !has {
			cur, err := s.St.EffectivePolicy(r.Context(), r.PathValue("id"))
			if err != nil {
				// Не прочитали текущий процесс — не рискуем сбросить его молча.
				writeErr(w, err)
				return
			}
			in.Process = cur.Overrides.Process
		}
	}
	v, err := s.St.SaveProjectPolicy(r.Context(), r.PathValue("id"), in, user(r))
	if err != nil {
		writeProcessErr(w, err)
		return
	}
	// Ответ — из сохранённой версии: параллельный PUT не подменит её.
	eff, err := s.St.PolicyFromProjectVersion(r.Context(), v)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.writeProjectPolicy(w, r, eff)
}

func (s *Server) listProjectPolicyVersions(w http.ResponseWriter, r *http.Request) {
	if !s.requireMember(w, r, r.PathValue("id")) {
		return
	}
	out, err := s.St.ListPolicyVersions(r.Context(), store.PolicyScopeProject, r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// writeProcessErr — ошибка валидации процесса с привязкой к шагу и полю
// (api-contract add-process-model: 422 с step и field), иначе как writeErr.
func writeProcessErr(w http.ResponseWriter, err error) {
	var pe *policy.ProcessError
	if errors.As(err, &pe) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": apiError{Code: "invalid", Message: pe.Error()},
			"step":  pe.Step, "field": pe.Field,
		})
		return
	}
	writeErr(w, err)
}
