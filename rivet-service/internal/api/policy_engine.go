package api

import (
	"net/http"

	"github.com/PavluninVladimir/rivet/internal/policy"
)

// Движок политик в клиентском API (change add-policy-engine, спека
// access-policy «Точки принуждения»): мутации людей, управляемые политикой,
// проходят через движок. Права выдаёт код — движок может только запретить,
// поэтому проверка стоит после guard'ов роли и членства.

// Действия мутаций для движка.
const (
	actionPolicyWrite = "policy.write"
	actionTaskMerge   = "task.merge"
	actionRunnerAdmin = "runner.admin"
	actionEpicStatus  = "epic.status"
)

// policyBlocks — движок запретил мутацию (или не дал решения): ответ уже
// записан, обработчику остаётся выйти. Ошибка движка — 503: для мутаций
// людей отсутствие решения тоже запрет, но это состояние установки, а не
// вина запроса.
func (s *Server) policyBlocks(w http.ResponseWriter, r *http.Request, action, projectID string) bool {
	d, err := s.policyEngine().Decide(r.Context(), policy.PointMutation, map[string]any{
		"action":     action,
		"project_id": projectID,
		"actor":      map[string]any{"kind": "user", "login": user(r), "admin": currentUser(r).Admin},
	})
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]apiError{"error": {
			Code: "policy_unavailable", Message: "движок политик не дал решения: " + err.Error()}})
		return true
	}
	if !d.Allow {
		reason := d.Reason
		if reason == "" {
			reason = "запрещено политикой"
		}
		writeJSON(w, http.StatusForbidden, map[string]apiError{"error": {
			Code: "policy_denied", Message: "действие запрещено политикой: " + reason}})
		return true
	}
	return false
}

// policyEngine — движок установки или встроенный по умолчанию (тесты и
// внутренние потребители; rivetd задаёт движок явно).
func (s *Server) policyEngine() policy.Engine {
	if s.Policy != nil {
		return s.Policy
	}
	return policy.Default()
}

// policyExternal — пресеты управляются вне Rivet: локальная правка политики
// в этом режиме отклоняется (спека access-policy «Внешний контур политик»).
func (s *Server) policyExternal(w http.ResponseWriter) bool {
	if s.policyEngine().Mode() != policy.ModeExternal {
		return false
	}
	writeJSON(w, http.StatusConflict, map[string]apiError{"error": {
		Code: "policy_external", Message: "политики управляются вне Rivet: установка работает с внешним движком"}})
	return true
}

// engineView — режим и состояние движка для состояния установки и вкладки
// «Политики».
type engineView struct {
	Mode   string `json:"mode"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

func (s *Server) engineView(r *http.Request) engineView {
	eng := s.policyEngine()
	v := engineView{Mode: eng.Mode(), State: "ok"}
	switch v.Mode {
	case policy.ModeExternal:
		v.Detail = "внешний OPA установки"
	default:
		v.Detail = "встроенный движок"
	}
	if err := eng.Health(r.Context()); err != nil {
		v.State, v.Detail = "degraded", err.Error()
	}
	return v
}
