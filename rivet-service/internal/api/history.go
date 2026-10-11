package api

import (
	"errors"
	"net/http"

	"github.com/PavluninVladimir/rivet/internal/history"
)

// POST /projects/{id}/history — импорт истории проекта из манифеста
// (спека domain-model «Импорт истории проекта»): выполненные Epic'и и
// задачи с исходными датами и PR. Только владелец: импорт задаёт даты и
// статусы в обход конвейера.
func (s *Server) importHistory(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if !s.requireOwner(w, r, projectID) {
		return
	}
	var m history.Manifest
	if err := decodeLarge(r, &m); err != nil {
		unprocessable(w, "невалидный JSON")
		return
	}
	res, err := s.St.ImportHistory(r.Context(), projectID, m, user(r))
	if err != nil {
		if errors.Is(err, history.ErrInvalid) {
			unprocessable(w, err.Error())
			return
		}
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
