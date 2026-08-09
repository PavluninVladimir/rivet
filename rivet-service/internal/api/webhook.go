package api

import (
	"net/http"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// githubWebhook принимает события хостинга (спека backend/scm-integration):
// ручной merge PR человеком корректно завершает задачу.
func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	event := r.Header.Get("X-GitHub-Event")
	var payload struct {
		Action      string `json:"action"`
		PullRequest struct {
			Merged  bool   `json:"merged"`
			HTMLURL string `json:"html_url"`
			Head    struct {
				Ref string `json:"ref"`
			} `json:"head"`
			MergedBy struct {
				Login string `json:"login"`
			} `json:"merged_by"`
		} `json:"pull_request"`
	}
	if err := decode(r, &payload); err != nil {
		unprocessable(w, "невалидный payload")
		return
	}
	if event != "pull_request" || payload.Action != "closed" || !payload.PullRequest.Merged {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	task, err := s.St.TaskByBranch(r.Context(), payload.PullRequest.Head.Ref)
	if err != nil {
		// PR не из наших веток — не наша задача.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if task.Status == domain.TaskDone {
		writeJSON(w, http.StatusOK, map[string]string{"status": "already done"})
		return
	}
	if err := s.St.TransitionWithRunnerRelease(r.Context(), task.ID, domain.TaskDone,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: payload.PullRequest.MergedBy.Login,
			Type: "task.status", Text: "PR смержен вручную на хостинге",
			Payload: map[string]any{"status": "done", "pr": payload.PullRequest.HTMLURL}}); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.St.RecomputeEpic(r.Context(), task.EpicID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "done"})
}
