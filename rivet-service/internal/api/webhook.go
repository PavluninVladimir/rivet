package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/orchestrator"
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

// Виды событий хостинга, на которые реагирует конвейер (спека
// scm-integration «События хостинга в конвейере»).
const (
	hookMerge    = "merge"     // PR/MR смержен человеком
	hookPRClosed = "pr_closed" // PR/MR закрыт без merge
	hookChecks   = "checks"    // внешние проверки завершились
	hookReview   = "review"    // review человека на хостинге
)

// hostingEvent — то общее, что конвейеру нужно от события любого хостинга.
// Пустой Kind — событие нас не интересует.
type hostingEvent struct {
	Kind      string
	RepoPath  string // owner/name
	Branch    string // ветка-источник PR/MR или проверок
	URL       string
	MergeSHA  string
	Actor     string // кто смержил, закрыл или оставил review
	Name      string // имя набора проверок
	Body      string // текст review
	State     string // review: approved | changes_requested | commented
	OK        bool   // checks: проверки прошли
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
	s.handleHostingEvent(w, r, string(scm.ProviderGitHub), body, ev, verifyGitHubSignature)
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
	s.handleHostingEvent(w, r, string(scm.ProviderGitLab), body, ev, verifyGitLabToken)
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

// handleHostingEvent — общая часть: выбор проекта, проверка подлинности,
// связывание с задачей и реакция конвейера по виду события.
func (s *Server) handleHostingEvent(w http.ResponseWriter, r *http.Request, provider string,
	body []byte, ev hostingEvent, verify func(*http.Request, []byte, string) bool) {

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
	if ev.Kind == "" {
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
	if epic.ProjectID != project.ID {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	// URL сравнивается только у событий про сам PR: у проверок и review
	// в URL приезжает прогон CI или комментарий, а не адрес PR.
	if ev.Kind == hookMerge || ev.Kind == hookPRClosed {
		if task.PRURL != "" && ev.URL != "" && ev.URL != task.PRURL {
			writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}
	}
	switch ev.Kind {
	case hookMerge:
		s.hostingMerge(w, r, project, task, ev)
	case hookPRClosed:
		if task.Status == domain.TaskDone {
			// Хостинг закрывает PR и после merge: задача уже завершена.
			writeJSON(w, http.StatusOK, map[string]string{"status": "already done"})
			return
		}
		if err := s.Engine.OnPRClosed(r.Context(), task, ev.Actor, ev.URL); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "noted"})
	case hookChecks:
		reacted, err := s.Engine.OnExternalChecks(r.Context(), task, ev.OK, ev.Name, ev.URL)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": statusOf(reacted)})
	case hookReview:
		reacted, err := s.Engine.OnExternalReview(r.Context(), task, ev.State, ev.Actor, ev.Body, ev.URL)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": statusOf(reacted)})
	}
}

// statusOf — ответ webhook'у: изменили конвейер или только записали событие.
func statusOf(reacted orchestrator.ExternalReacted) string {
	if reacted {
		return "done"
	}
	return "noted"
}

// hostingMerge — PR смержен человеком на хостинге: задача завершается и
// запускаются автопубликации, как при merge кнопкой.
func (s *Server) hostingMerge(w http.ResponseWriter, r *http.Request,
	project domain.Project, task domain.Task, ev hostingEvent) {

	if task.Status == domain.TaskDone {
		writeJSON(w, http.StatusOK, map[string]string{"status": "already done"})
		return
	}
	if err := s.St.CompleteTask(r.Context(), task.ID,
		store.EventInput{ActorKind: domain.ActorUser, ActorID: ev.Actor,
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
		if err := s.Engine.EnqueueAutoDeploys(r.Context(), project.ID, ev.MergeSHA); err != nil {
			writeErr(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "done"})
}

func parseGitHubEvent(r *http.Request, body []byte) hostingEvent {
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
			User struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"pull_request"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		Review struct {
			State   string `json:"state"`
			Body    string `json:"body"`
			HTMLURL string `json:"html_url"`
			User    struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"review"`
		CheckSuite struct {
			HeadBranch string `json:"head_branch"`
			Conclusion string `json:"conclusion"`
			Status     string `json:"status"`
			App        struct {
				Name string `json:"name"`
			} `json:"app"`
		} `json:"check_suite"`
		WorkflowRun struct {
			Name       string `json:"name"`
			HeadBranch string `json:"head_branch"`
			Conclusion string `json:"conclusion"`
			Status     string `json:"status"`
			HTMLURL    string `json:"html_url"`
		} `json:"workflow_run"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return hostingEvent{Malformed: true}
	}
	ev := hostingEvent{RepoPath: payload.Repository.FullName}
	switch r.Header.Get("X-GitHub-Event") {
	case "pull_request":
		if payload.Action != "closed" {
			return ev
		}
		ev.Branch, ev.URL = payload.PullRequest.Head.Ref, payload.PullRequest.HTMLURL
		ev.MergeSHA = payload.PullRequest.MergeCommitSHA
		if payload.PullRequest.Merged {
			ev.Kind, ev.Actor = hookMerge, payload.PullRequest.MergedBy.Login
			return ev
		}
		ev.Kind, ev.Actor = hookPRClosed, payload.Sender.Login
	case "pull_request_review":
		if payload.Action != "submitted" {
			return ev
		}
		ev.Kind = hookReview
		ev.Branch, ev.URL = payload.PullRequest.Head.Ref, payload.Review.HTMLURL
		ev.Actor, ev.Body = payload.Review.User.Login, payload.Review.Body
		ev.State = reviewState(payload.Review.State)
	case "workflow_run":
		if payload.WorkflowRun.Status != "completed" {
			return ev
		}
		ok, decisive := checksOutcome(payload.WorkflowRun.Conclusion)
		if !decisive {
			return ev
		}
		ev.Kind, ev.Branch, ev.OK = hookChecks, payload.WorkflowRun.HeadBranch, ok
		ev.Name, ev.URL = payload.WorkflowRun.Name, payload.WorkflowRun.HTMLURL
	case "check_suite":
		if payload.CheckSuite.Status != "completed" {
			return ev
		}
		ok, decisive := checksOutcome(payload.CheckSuite.Conclusion)
		if !decisive {
			return ev
		}
		ev.Kind, ev.Branch, ev.OK = hookChecks, payload.CheckSuite.HeadBranch, ok
		ev.Name = payload.CheckSuite.App.Name
	}
	return ev
}

// checksOutcome — итог набора проверок GitHub. Отменённый или пропущенный
// прогон вердикта не даёт: возвращать из-за него задачу в работу нельзя,
// и писать «проверки прошли» тоже неправда.
func checksOutcome(conclusion string) (ok, decisive bool) {
	switch strings.ToLower(conclusion) {
	case "success", "neutral":
		return true, true
	case "failure", "timed_out", "action_required", "startup_failure":
		return false, true
	}
	return false, false
}

// reviewState нормализует состояние review GitHub (APPROVED,
// CHANGES_REQUESTED, COMMENTED) в значения контракта.
func reviewState(s string) string {
	switch strings.ToLower(s) {
	case "approved":
		return "approved"
	case "changes_requested":
		return "changes_requested"
	}
	return "commented"
}

func parseGitLabEvent(r *http.Request, body []byte) hostingEvent {
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
			// Pipeline Hook: ветка и статус пайплайна.
			Ref    string `json:"ref"`
			Status string `json:"status"`
			// Note Hook: текст комментария и к чему он относится.
			Note         string `json:"note"`
			NoteableType string `json:"noteable_type"`
		} `json:"object_attributes"`
		MergeRequest struct {
			SourceBranch string `json:"source_branch"`
			URL          string `json:"url"`
		} `json:"merge_request"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return hostingEvent{Malformed: true}
	}
	a := payload.ObjectAttributes
	ev := hostingEvent{RepoPath: payload.Project.PathWithNamespace, Actor: payload.User.Username}
	switch r.Header.Get("X-Gitlab-Event") {
	case "Merge Request Hook":
		ev.Branch, ev.URL, ev.MergeSHA = a.SourceBranch, a.URL, a.MergeCommit
		switch {
		case a.Action == "merge" || a.State == "merged":
			ev.Kind = hookMerge
		case a.Action == "close" || a.State == "closed":
			ev.Kind = hookPRClosed
		case a.Action == "approved":
			ev.Kind, ev.State = hookReview, "approved"
		case a.Action == "unapproved":
			// В GitLab запрос изменений выражается снятием одобрения:
			// отдельного changes_requested в модели MR нет.
			ev.Kind, ev.State = hookReview, "changes_requested"
		}
	case "Pipeline Hook":
		switch a.Status {
		case "success", "failed":
			ev.Kind, ev.Branch, ev.URL = hookChecks, a.Ref, a.URL
			ev.OK = a.Status == "success"
			ev.Name = "pipeline"
		}
	case "Note Hook":
		if a.NoteableType != "MergeRequest" {
			return ev
		}
		ev.Kind, ev.State = hookReview, "commented"
		ev.Branch, ev.Body = payload.MergeRequest.SourceBranch, a.Note
		ev.URL = a.URL
	}
	return ev
}
