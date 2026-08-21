package api

import (
	"net/http"

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
		writeJSON(w, http.StatusServiceUnavailable, map[string]apiError{"error": {
			Code: "planner_invalid", Message: "ключ модели отклонён провайдером: " + st.Detail}})
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

	plan, err := pl.Decompose(r.Context(), epic, project)
	if err != nil {
		unprocessable(w, "план не принят: "+err.Error())
		return
	}
	tasks, err := planner.Materialize(r.Context(), s.St, epic.ID, plan)
	if err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.St.AppendEvent(r.Context(), store.EventInput{
		ActorKind: domain.ActorSystem, Type: "epic.decomposed",
		ProjectID: project.ID, EpicID: epic.ID,
		Text: "план построен и ожидает проверки человеком",
	}); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"tasks": tasks})
}
