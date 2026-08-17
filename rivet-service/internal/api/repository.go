// Подключение репозитория к проекту (api-contract add-repo-onboarding).
package api

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/PavluninVladimir/rivet/internal/domain"
	"github.com/PavluninVladimir/rivet/internal/scm"
	"github.com/PavluninVladimir/rivet/internal/store"
)

type probeView struct {
	OK              bool   `json:"ok"`
	Reason          string `json:"reason"`
	Message         string `json:"message"`
	TokenOwner      string `json:"token_owner"`
	RepoPath        string `json:"repo_path"`
	BaseURL         string `json:"base_url"`
	DefaultBranch   string `json:"default_branch"`
	CanPush         bool   `json:"can_push"`
	CanMergeRequest bool   `json:"can_merge_request"`
}

type credentialView struct {
	Owner       string    `json:"owner"`
	TokenPrefix string    `json:"token_prefix"`
	AddedAt     time.Time `json:"added_at"`
}

type webhookView struct {
	Registered bool   `json:"registered"`
	URL        string `json:"url"`
	SecretHint string `json:"secret_hint"`
}

type repositoryView struct {
	Provider      string          `json:"provider"`
	BaseURL       string          `json:"base_url"`
	RepoPath      string          `json:"repo_path"`
	DefaultBranch string          `json:"default_branch"`
	WebURL        string          `json:"web_url"`
	Credential    *credentialView `json:"credential"`
	State         string          `json:"state"`
	CheckedAt     *time.Time      `json:"checked_at"`
	Webhook       webhookView     `json:"webhook"`
}

// probeInput — общий вход проверки подключения и создания проекта.
type probeInput struct {
	Provider string `json:"provider"`
	RepoURL  string `json:"repo_url"`
	BaseURL  string `json:"base_url"`
	RepoPath string `json:"repo_path"`
	Token    string `json:"token"`
}

// resolve приводит вход к провайдеру, инстансу и пути репозитория.
// Провайдер по хосту угадывается только для github.com и gitlab.com,
// иначе его обязан указать пользователь (design, решение 3).
func (in probeInput) resolve() (provider scm.Provider, baseURL, repoPath string, err error) {
	if in.RepoURL != "" {
		ref, perr := scm.ParseRepoURL(in.RepoURL)
		if perr != nil {
			return "", "", "", perr
		}
		provider, baseURL, repoPath = ref.Provider, ref.BaseURL, ref.Path
	} else {
		if in.BaseURL != "" {
			baseURL, err = scm.NormalizeBaseURL(in.BaseURL)
			if err != nil {
				return "", "", "", err
			}
		}
		repoPath = in.RepoPath
	}
	if in.Provider != "" {
		// fake — внутренний провайдер стендов: снаружи его принимать нельзя,
		// иначе любой участник создаст проект, у которого проверка доступа
		// всегда успешна.
		if !scm.ValidProvider(in.Provider) || in.Provider == string(scm.ProviderFake) {
			return "", "", "", errors.New("неизвестный провайдер: ожидается github или gitlab")
		}
		provider = scm.Provider(in.Provider)
	}
	if provider == "" || provider == scm.ProviderFake {
		return "", "", "", errors.New("укажите провайдера: по этому URL он не определяется однозначно")
	}
	if baseURL == "" {
		switch provider {
		case scm.ProviderGitHub:
			baseURL = "https://github.com"
		case scm.ProviderGitLab:
			baseURL = "https://gitlab.com"
		default:
			baseURL = "https://fake.local"
		}
	}
	return provider, baseURL, repoPath, nil
}

// POST /api/v1/scm/probe — проверка подключения до создания проекта.
func (s *Server) scmProbe(w http.ResponseWriter, r *http.Request) {
	var in probeInput
	if err := decode(r, &in); err != nil {
		unprocessable(w, "невалидный JSON")
		return
	}
	if in.Token == "" {
		unprocessable(w, "нужен токен доступа к хостингу")
		return
	}
	provider, baseURL, repoPath, err := in.resolve()
	if err != nil {
		unprocessable(w, err.Error())
		return
	}
	res := s.probe(r, s.effectiveProvider(provider), baseURL, repoPath, in.Token)
	writeJSON(w, http.StatusOK, res)
}

// probe выполняет проверку через адаптер провайдера.
func (s *Server) probe(r *http.Request, provider scm.Provider, baseURL, repoPath, token string) probeView {
	adapter, err := s.adapters().For(provider, baseURL, token)
	if err != nil {
		return probeView{Reason: scm.ReasonUnreachable, Message: err.Error(), BaseURL: baseURL}
	}
	res := adapter.Probe(r.Context(), repoPath)
	if res.RepoPath == "" {
		res.RepoPath = repoPath
	}
	return probeView{
		OK: res.OK, Reason: res.Reason, Message: res.Message, TokenOwner: res.TokenOwner,
		RepoPath: res.RepoPath, BaseURL: baseURL, DefaultBranch: res.DefaultBranch,
		CanPush: res.CanPush, CanMergeRequest: res.CanMergeRequest,
	}
}

// effectiveProvider — в установке без реальных хостингов (RIVET_SCM=fake,
// e2e-стенд) любой выбор пользователя ведёт к fake-провайдеру: иначе
// проект хранил бы github, а работал бы на подставном адаптере.
func (s *Server) effectiveProvider(p scm.Provider) scm.Provider {
	if s.adapters().Force {
		return scm.ProviderFake
	}
	return p
}

func (s *Server) adapters() *scm.Factory {
	if s.Engine != nil && s.Engine.Adapters != nil {
		return s.Engine.Adapters
	}
	return &scm.Factory{}
}

// GET /api/v1/projects/{id}/repository — состояние подключения.
func (s *Server) getRepository(w http.ResponseWriter, r *http.Request) {
	if !s.requireMember(w, r, r.PathValue("id")) {
		return
	}
	p, err := s.St.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.repositoryDTO(r, p))
}

func (s *Server) repositoryDTO(r *http.Request, p domain.Project) repositoryView {
	v := repositoryView{
		Provider: p.Provider, BaseURL: p.BaseURL, RepoPath: p.RepoPath,
		DefaultBranch: p.DefaultBranch, WebURL: p.WebURL(), State: "unchecked",
		Webhook: webhookView{URL: s.webhookURL(p.Provider)},
	}
	if p.WebhookSecret != "" {
		v.Webhook.SecretHint = "секрет создан системой; показывается один раз при подключении"
	}
	if c, err := s.St.ProjectCredential(r.Context(), p.ID); err == nil {
		v.Credential = &credentialView{Owner: c.Owner, TokenPrefix: c.TokenPrefix, AddedAt: c.Created}
		v.State = c.State
		v.CheckedAt = c.CheckedAt
		v.Webhook.Registered = p.WebhookRegistered
	}
	return v
}

func (s *Server) webhookURL(provider string) string {
	if s.PublicURL == "" {
		return ""
	}
	return s.PublicURL + "/api/v1/webhooks/" + provider
}

// PUT /api/v1/projects/{id}/credentials — замена учётных данных проекта.
func (s *Server) putCredentials(w http.ResponseWriter, r *http.Request) {
	if !s.requireMember(w, r, r.PathValue("id")) {
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := decode(r, &in); err != nil || in.Token == "" {
		unprocessable(w, "нужен токен доступа к хостингу")
		return
	}
	if !s.Secrets.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]apiError{"error": {
			Code: "no_secret_key", Message: "ключ шифрования не настроен: учётные данные не сохранить"}})
		return
	}
	p, err := s.St.GetProject(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	res := s.probe(r, scm.Provider(p.Provider), p.BaseURL, p.RepoPath, in.Token)
	if !res.OK {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error": apiError{Code: res.Reason, Message: res.Message}, "probe": res})
		return
	}
	if err := s.St.ReplaceCredential(r.Context(), p.ID, store.NewRepoConnection{
		Provider: p.Provider, BaseURL: p.BaseURL, Token: in.Token, TokenOwner: res.TokenOwner,
	}, s.Secrets); err != nil {
		writeErr(w, err)
		return
	}
	// У проекта, жившего на общем секрете установки, появляются свои
	// учётные данные — значит пора выдать и собственный секрет webhook.
	if _, err := s.St.EnsureWebhookSecret(r.Context(), p.ID); err != nil {
		writeErr(w, err)
		return
	}
	p, err = s.St.GetProject(r.Context(), p.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.registerWebhook(r, p, in.Token)
	s.repoEvent(r, p, "учётные данные хостинга заменены")
	p, err = s.St.GetProject(r.Context(), p.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.repositoryDTO(r, p))
}

// repoEvent — изменение подключения видно участникам проекта в event log.
func (s *Server) repoEvent(r *http.Request, p domain.Project, text string) {
	if _, err := s.St.AppendEvent(r.Context(), store.EventInput{
		ActorKind: domain.ActorUser, ActorID: user(r), Type: "project.repository",
		ProjectID: p.ID, Text: text,
		Payload: map[string]any{"provider": p.Provider, "repo_path": p.RepoPath},
	}); err != nil {
		slog.Error("project repository event", "project", p.ID, "err", err)
	}
}

// registerWebhook подписывает хостинг на события проекта; без прав или без
// внешнего URL установки подключение не блокируется (design, решение 7).
func (s *Server) registerWebhook(r *http.Request, p domain.Project, token string) {
	if s.PublicURL == "" || token == "" {
		return
	}
	adapter, err := s.adapters().For(scm.Provider(p.Provider), p.BaseURL, token)
	if err != nil {
		return
	}
	ok, err := adapter.RegisterWebhook(r.Context(), p.RepoPath, s.webhookURL(p.Provider), p.WebhookSecret)
	if err != nil {
		slog.Warn("регистрация webhook не удалась", "project", p.ID, "err", err)
		return
	}
	if !ok {
		slog.Info("webhook не зарегистрирован: нет прав, нужна ручная настройка", "project", p.ID)
		return
	}
	if err := s.St.SetWebhookRegistered(r.Context(), p.ID, true); err != nil {
		slog.Error("отметка регистрации webhook", "project", p.ID, "err", err)
	}
}
