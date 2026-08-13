package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// githubWebhook принимает события хостинга (спека backend/scm-integration):
// ручной merge PR человеком корректно завершает задачу. Каждый запрос обязан
// нести валидную подпись HMAC-SHA256 (X-Hub-Signature-256); без настроенного
// секрета приём событий выключен (fail-closed).
func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	if s.WebhookSecret == "" {
		writeJSON(w, http.StatusForbidden,
			map[string]apiError{"error": {Code: "webhook_disabled", Message: "секрет webhook не настроен"}})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		unprocessable(w, "невалидный payload")
		return
	}
	mac := hmac.New(sha256.New, []byte(s.WebhookSecret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if got := r.Header.Get("X-Hub-Signature-256"); !hmac.Equal([]byte(got), []byte(want)) {
		writeJSON(w, http.StatusUnauthorized,
			map[string]apiError{"error": {Code: "bad_signature", Message: "невалидная подпись webhook"}})
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	var payload struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		PullRequest struct {
			Merged         bool   `json:"merged"`
			HTMLURL        string `json:"html_url"`
			MergeCommitSHA string `json:"merge_commit_sha"`
			Head           struct {
				Ref string `json:"ref"`
			} `json:"head"`
			MergedBy struct {
				Login string `json:"login"`
			} `json:"merged_by"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
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
	// Подпись подтверждает подлинность, но не принадлежность: событие обязано
	// прийти из репозитория проекта задачи и (если PR создавали мы) про тот же PR.
	epic, err := s.St.GetEpic(r.Context(), task.EpicID)
	if err != nil {
		writeErr(w, err)
		return
	}
	project, err := s.St.GetProject(r.Context(), epic.ProjectID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if payload.Repository.FullName != project.Repo ||
		(task.PRURL != "" && payload.PullRequest.HTMLURL != task.PRURL) {
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
	// Внешний merge запускает автопубликации так же, как merge кнопкой
	// (спека deployment «Режимы запуска»).
	if sha := payload.PullRequest.MergeCommitSHA; sha != "" {
		if err := s.St.EnqueueAutoDeployments(r.Context(), project.ID, sha); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "done"})
}
