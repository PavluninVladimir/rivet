package api

import (
	"encoding/json"
	"net/http"

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
		writeErr(w, err)
		return
	}
	// Пресеты — из сохранённой версии, а не повторным чтением активной:
	// параллельный PUT не должен подменить их в ответе.
	var p policy.Presets
	if err := json.Unmarshal(v.Content, &p); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": v, "presets": p.Normalize()})
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
}

func (s *Server) writeProjectPolicy(w http.ResponseWriter, r *http.Request, eff store.EffectivePolicy) {
	writeJSON(w, http.StatusOK, projectPolicyView{
		Effective: eff.Presets, EffectiveHash: eff.Hash, Overrides: eff.Overrides,
		Version: eff.Project, InstallationVersion: eff.Installation,
		Engine: s.engineView(r),
	})
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
	var in policy.Overrides
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидный JSON")
		return
	}
	v, err := s.St.SaveProjectPolicy(r.Context(), r.PathValue("id"), in, user(r))
	if err != nil {
		writeErr(w, err)
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
