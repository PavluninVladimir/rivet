package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

// Приём событий хостингов (спека backend/scm-integration «Аутентичность
// webhook»): ручной merge PR/MR человеком корректно завершает задачу.
//
// Порядок проверки: из тела достаётся репозиторий и находится
// проект-кандидат — это ещё не доверие, а только выбор ключа; затем тело
// проверяется секретом этого проекта. Проект без своего секрета проверяется
// секретом установки. Нет ни того, ни другого — приём выключен (fail-closed).

// mergeEvent — то общее, что конвейеру нужно от события любого хостинга.
type mergeEvent struct {
	RepoPath  string // owner/name
	Branch    string // ветка-источник PR/MR
	URL       string
	MergeSHA  string
	MergedBy  string
	IsMerge   bool // событие вообще про состоявшийся merge
	Malformed bool
}

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := readWebhookBody(w, r)
	if !ok {
		return
	}
	ev := parseGitHubEvent(r, body)
	if ev.Malformed {
		unprocessable(w, "невалидный payload")
		return
	}
	s.handleMergeEvent(w, r, string(scm.ProviderGitHub), body, ev, verifyGitHubSignature)
}

// gitlabWebhook — тот же конвейер для GitLab: вместо подписи HMAC общий
// токен в X-Gitlab-Token (api-contract add-repo-onboarding).
func (s *Server) gitlabWebhook(w http.ResponseWriter, r *http.Request) {
	body, ok := readWebhookBody(w, r)
	if !ok {
		return
	}
	ev := parseGitLabEvent(r, body)
	if ev.Malformed {
		unprocessable(w, "невалидный payload")
		return
	}
	s.handleMergeEvent(w, r, string(scm.ProviderGitLab), body, ev, verifyGitLabToken)
}

func readWebhookBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		unprocessable(w, "невалидный payload")
		return nil, false
	}
	return body, true
}

func verifyGitHubSignature(r *http.Request, body []byte, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(r.Header.Get("X-Hub-Signature-256")), []byte(want))
}

func verifyGitLabToken(r *http.Request, _ []byte, secret string) bool {
	got := r.Header.Get("X-Gitlab-Token")
	return subtle.ConstantTimeCompare([]byte(got), []byte(secret)) == 1
}

// handleMergeEvent — общая часть: выбор проекта, проверка подлинности,
// перевод задачи в done и запуск автопубликаций.
func (s *Server) handleMergeEvent(w http.ResponseWriter, r *http.Request, provider string,
	body []byte, ev mergeEvent, verify func(*http.Request, []byte, string) bool) {

	if ev.RepoPath == "" {
		// Событие без репозитория: не с чем сопоставить ключ проверки.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	candidates, err := s.St.ProjectsByRepo(r.Context(), provider, ev.RepoPath)
	if err != nil {
		writeErr(w, err)
		return
	}
	if len(candidates) == 0 {
		// Репозиторий не наш: событие не подтверждено, существование
		// проектов не раскрываем.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	// Один и тот же путь может быть у проектов на разных инстансах, а
	// событие инстанс не называет: кандидата выбирает подпись. Секрет
	// проекта, иначе секрет установки; нет ни одного — приём выключен.
	var project domain.Project
	matched, anySecret := false, false
	for _, c := range candidates {
		secret := c.WebhookSecret
		if secret == "" {
			secret = s.WebhookSecret
		}
		if secret == "" {
			continue
		}
		anySecret = true
		if verify(r, body, secret) {
			project, matched = c, true
			break
		}
	}
	if !anySecret {
		writeJSON(w, http.StatusForbidden,
			map[string]apiError{"error": {Code: "webhook_disabled", Message: "секрет webhook не настроен"}})
		return
	}
	if !matched {
		writeJSON(w, http.StatusUnauthorized,
			map[string]apiError{"error": {Code: "bad_signature", Message: "невалидная подпись webhook"}})
		return
	}
	// Подлинность проверена до всего остального; дальше решаем, интересует
	// ли нас это событие (спека: проверяется каждое входящее событие).
	if !ev.IsMerge {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	task, err := s.St.TaskByBranch(r.Context(), ev.Branch)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	// Подпись подтверждает подлинность, но не принадлежность: задача обязана
	// быть из этого же проекта и (если PR создавали мы) про тот же PR.
	epic, err := s.St.GetEpic(r.Context(), task.EpicID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if epic.ProjectID != project.ID || (task.PRURL != "" && ev.URL != "" && ev.URL != task.PRURL) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if task.Status == domain.TaskDone {
		writeJSON(w, http.StatusOK, map[string]string{"status": "already done"})
		return
	}
	if err := s.St.TransitionWithRunnerRelease(r.Context(), task.ID, domain.TaskDone,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: ev.MergedBy,
			Type: "task.status", Text: "PR смержен вручную на хостинге",
			Payload: map[string]any{"status": "done", "pr": ev.URL}}); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.St.RecomputeEpic(r.Context(), task.EpicID); err != nil {
		writeErr(w, err)
		return
	}
	// Внешний merge запускает автопубликации так же, как merge кнопкой
	// (спека deployment «Режимы запуска»).
	if ev.MergeSHA != "" {
		if err := s.St.EnqueueAutoDeployments(r.Context(), project.ID, ev.MergeSHA); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "done"})
}

func parseGitHubEvent(r *http.Request, body []byte) mergeEvent {
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
		return mergeEvent{Malformed: true}
	}
	return mergeEvent{
		RepoPath: payload.Repository.FullName,
		Branch:   payload.PullRequest.Head.Ref,
		URL:      payload.PullRequest.HTMLURL,
		MergeSHA: payload.PullRequest.MergeCommitSHA,
		MergedBy: payload.PullRequest.MergedBy.Login,
		IsMerge: r.Header.Get("X-GitHub-Event") == "pull_request" &&
			payload.Action == "closed" && payload.PullRequest.Merged,
	}
}

func parseGitLabEvent(r *http.Request, body []byte) mergeEvent {
	var payload struct {
		ObjectKind string `json:"object_kind"`
		User       struct {
			Username string `json:"username"`
		} `json:"user"`
		Project struct {
			PathWithNamespace string `json:"path_with_namespace"`
		} `json:"project"`
		ObjectAttributes struct {
			Action       string `json:"action"`
			State        string `json:"state"`
			SourceBranch string `json:"source_branch"`
			URL          string `json:"url"`
			MergeCommit  string `json:"merge_commit_sha"`
		} `json:"object_attributes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return mergeEvent{Malformed: true}
	}
	a := payload.ObjectAttributes
	return mergeEvent{
		RepoPath: payload.Project.PathWithNamespace,
		Branch:   a.SourceBranch,
		URL:      a.URL,
		MergeSHA: a.MergeCommit,
		MergedBy: payload.User.Username,
		IsMerge: r.Header.Get("X-Gitlab-Event") == "Merge Request Hook" &&
			(a.Action == "merge" || a.State == "merged"),
	}
}
