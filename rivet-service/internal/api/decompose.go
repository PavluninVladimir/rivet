package api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/planner"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// decompose — POST /api/v1/epics/{id}/decompose: модель строит план,
// детерминированный код валидирует, план создаётся в статусе planned
// и ждёт просмотра и запуска человеком (спека backend/epic-decomposition).
func (s *Server) decompose(w http.ResponseWriter, r *http.Request) {
	// Слой доступа раньше конфигурации: чужой epic — 404 до любых деталей.
	epic, err := s.St.EpicForViewer(r.Context(), r.PathValue("id"), currentUser(r).ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Модель не настроена или ключ отклонён — машиночитаемый 503, не сбой
	// (спека epic-decomposition, api-contract).
	pl, st := s.plannerStatus()
	switch {
	case st.State == "invalid":
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": apiError{
			Code: "planner_invalid", Message: "модель декомпозиции недоступна: " + st.Detail},
			"connection_id": st.ConnectionID})
		return
	case pl == nil:
		writeJSON(w, http.StatusServiceUnavailable, map[string]apiError{"error": {
			Code: "no_planner", Message: "модель для декомпозиции не настроена: задайте ключ в разделе «Управление приложением» или в окружении установки"}})
		return
	}
	if epic.Status != domain.EpicPlanned {
		writeErr(w, domain.ErrBadTransition{Entity: "epic", From: string(epic.Status), To: "decompose"})
		return
	}
	if existing, err := s.St.ListEpicTasks(r.Context(), epic.ID); err != nil {
		writeErr(w, err)
		return
	} else if len(existing) > 0 {
		unprocessable(w, "у Epic уже есть задачи — декомпозиция выполняется на пустой план")
		return
	}
	project, err := s.St.GetProject(r.Context(), epic.ProjectID)
	if err != nil {
		writeErr(w, err)
		return
	}

	// План строит агент-Тимлид команды (add-agent-roles): инструкция
	// шаблона и дополнение агента дополняют задание. Без Тимлида в команде —
	// инструкция шаблона; отключённый шаблон — прежнее задание.
	instruction := ""
	rolePayload := map[string]any{"model": st.Model, "source": string(st.Source)}
	var leadAgent *domain.TeamAgent
	if lead, err := s.St.GetRole(r.Context(), domain.RoleLead); err == nil && lead.Enabled {
		instruction = lead.Instruction
		rolePayload["role"], rolePayload["title"] = lead.ID, lead.Title
		if agent, ok, err := s.St.FirstTeamAgent(r.Context(), domain.RoleLead); err != nil {
			writeErr(w, err)
			return
		} else if ok {
			if extra := strings.TrimSpace(agent.InstructionExtra); extra != "" {
				instruction += "\n" + extra
			}
			rolePayload["agent"] = map[string]any{"id": agent.ID, "name": agent.Name}
			leadAgent = &agent
		}
	} else {
		rolePayload["role_delivered"] = false
	}
	// Старт декомпозиции виден всем зрителям: Тимлид «составляет план».
	startPayload := map[string]any{"epic_id": epic.ID}
	if a, ok := rolePayload["agent"]; ok {
		startPayload["agent"] = a
	}
	if _, err := s.St.AppendEvent(r.Context(), store.EventInput{
		ActorKind: domain.ActorSystem, Type: "epic.decomposing",
		ProjectID: project.ID, EpicID: epic.ID,
		Text: decomposingText(rolePayload), Payload: startPayload,
	}); err != nil {
		writeErr(w, err)
		return
	}
	done := s.startDecomposition(leadAgent, epic)
	defer done()
	failed := func(reason string) {
		payload := map[string]any{"epic_id": epic.ID, "reason": reason}
		if a, ok := rolePayload["agent"]; ok {
			payload["agent"] = a
		}
		// Событие провала пишется и при обрыве запроса клиентом: иначе
		// консоль осталась бы с «составляет план».
		if _, err := s.St.AppendEvent(context.WithoutCancel(r.Context()), store.EventInput{
			ActorKind: domain.ActorSystem, Type: "epic.plan_failed",
			ProjectID: project.ID, EpicID: epic.ID,
			Text: "план не составлен: " + reason, Payload: payload,
		}); err != nil {
			slog.Error("событие провала декомпозиции", "epic", epic.ID, "err", err)
		}
	}
	plan, err := pl.DecomposeAs(r.Context(), epic, project, instruction)
	if err != nil {
		failed(err.Error())
		unprocessable(w, "план не принят: "+err.Error())
		return
	}
	tasks, err := planner.Materialize(r.Context(), s.St, epic.ID, plan)
	if err != nil {
		failed(err.Error())
		writeErr(w, err)
		return
	}
	if _, err := s.St.AppendEvent(r.Context(), store.EventInput{
		ActorKind: domain.ActorSystem, Type: "epic.decomposed",
		ProjectID: project.ID, EpicID: epic.ID,
		Text:    decomposedText(rolePayload),
		Payload: rolePayload,
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tasks": tasks})
}

// decomposedText — текст события декомпозиции с автором плана.
func decomposedText(p map[string]any) string {
	t, _ := p["title"].(string)
	if t == "" {
		return "план построен и ожидает проверки человеком"
	}
	if a, ok := p["agent"].(map[string]any); ok {
		if name, _ := a["name"].(string); name != "" {
			t += " " + name
		}
	}
	return "план составил " + t + ": ожидает проверки человеком"
}

// decomposingText — текст события старта декомпозиции.
func decomposingText(p map[string]any) string {
	t, _ := p["title"].(string)
	if t == "" {
		return "декомпозиция Epic начата"
	}
	if a, ok := p["agent"].(map[string]any); ok {
		if name, _ := a["name"].(string); name != "" {
			t += " " + name
		}
	}
	return t + " составляет план"
}

// decomposition — идущие декомпозиции агента-Тимлида по одному Epic:
// число параллельных запусков и порядок последнего старта.
type decomposition struct {
	EpicID    string
	ProjectID string
	n         int
	seq       int64
}

// startDecomposition отмечает агента-Тимлида занятым планом Epic до
// возврата done. Параллельные декомпозиции считаются по Epic: done снимает
// только свою.
func (s *Server) startDecomposition(agent *domain.TeamAgent, epic domain.Epic) (done func()) {
	if agent == nil {
		return func() {}
	}
	s.decompMu.Lock()
	if s.decomposing == nil {
		s.decomposing = map[string]map[string]*decomposition{}
	}
	byEpic := s.decomposing[agent.ID]
	if byEpic == nil {
		byEpic = map[string]*decomposition{}
		s.decomposing[agent.ID] = byEpic
	}
	d := byEpic[epic.ID]
	if d == nil {
		d = &decomposition{EpicID: epic.ID, ProjectID: epic.ProjectID}
		byEpic[epic.ID] = d
	}
	s.decompSeq++
	d.n, d.seq = d.n+1, s.decompSeq
	s.decompMu.Unlock()
	return func() {
		s.decompMu.Lock()
		defer s.decompMu.Unlock()
		if d.n--; d.n <= 0 {
			delete(byEpic, epic.ID)
		}
		if len(byEpic) == 0 {
			delete(s.decomposing, agent.ID)
		}
	}
}

// decompositionOf — последняя начатая из идущих декомпозиций агента.
func (s *Server) decompositionOf(agentID string) (decomposition, bool) {
	s.decompMu.Lock()
	defer s.decompMu.Unlock()
	var best *decomposition
	for _, d := range s.decomposing[agentID] {
		if best == nil || d.seq > best.seq {
			best = d
		}
	}
	if best == nil {
		return decomposition{}, false
	}
	return *best, true
}
