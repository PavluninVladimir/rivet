// Подключение репозитория к проекту (api-contract add-repo-onboarding).
package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
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
	// StateReason и StateMessage — причина нерабочего состояния: код
	// (bad_token, no_access, insufficient_scope, not_found) и текст для
	// человека; у рабочего подключения пустые (fix-credential-state).
	StateReason  string      `json:"state_reason"`
	StateMessage string      `json:"state_message"`
	CheckedAt    *time.Time  `json:"checked_at"`
	Webhook      webhookView `json:"webhook"`
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
// Провайдер по хосту угадывается только для известных облачных инстансов
// (github.com, gitlab.com, gitea.com, codeberg.org), иначе его обязан
// указать пользователь (design, решение 3). У Gitea облачного инстанса по
// умолчанию нет: без URL инстанса подключение не разобрать.
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
			return "", "", "", errors.New("неизвестный провайдер: ожидается github, gitlab или gitea")
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
		case scm.ProviderGitea:
			return "", "", "", errors.New("для Gitea укажите URL инстанса")
		default:
			baseURL = "https://fake.local"
		}
	}
	// Gitea: инстанс может жить в подкаталоге — в режиме «создать» он
	// берётся из адреса инстанса целиком. Подкаталог в адресе репозитория
	// находит resolveRepo, спросив сам инстанс.
	if provider == scm.ProviderGitea && in.RepoURL == "" && in.BaseURL != "" {
		if baseURL, err = scm.NormalizeInstanceURL(in.BaseURL); err != nil {
			return "", "", "", err
		}
	}
	return provider, baseURL, repoPath, nil
}

// resolvedRepo — разобранный адрес подключения.
type resolvedRepo struct {
	provider scm.Provider
	baseURL  string
	repoPath string
	// hint — пояснение к возможному отказу проверки: инстанс Gitea по
	// адресу не опознан, взят запасной вариант.
	hint string
}

// failed дополняет отказ проверки подключения пояснением разбора адреса.
func (rr resolvedRepo) failed(res probeView) probeView {
	if res.OK || rr.hint == "" {
		return res
	}
	if msg := strings.TrimRight(res.Message, ". "); msg != "" {
		res.Message = msg + ". " + rr.hint
	} else {
		res.Message = rr.hint
	}
	return res
}

// resolveRepo — resolve с поиском подкаталога инстанса Gitea: в адресе
// репозитория он неотличим от owner/name и хвоста страницы, поэтому
// инстанс спрашивается сам (scm.LocateGitea). Установка без реальных
// хостингов (fake) наружу не ходит: owner/name — первые два сегмента.
func (s *Server) resolveRepo(ctx context.Context, in probeInput) (resolvedRepo, error) {
	provider, baseURL, repoPath, err := in.resolve()
	rr := resolvedRepo{provider: provider, baseURL: baseURL, repoPath: repoPath}
	if err != nil || provider != scm.ProviderGitea || repoPath == "" {
		return rr, err
	}
	if s.adapters().Force {
		rr.repoPath, err = scm.GiteaRepoPath(repoPath)
		return rr, err
	}
	var found bool
	rr.baseURL, rr.repoPath, found, err = scm.LocateGitea(ctx, baseURL, repoPath)
	if !found {
		rr.hint = scm.GiteaNotLocated
	}
	return rr, err
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
	rr, err := s.resolveRepo(r.Context(), in)
	if err != nil {
		unprocessable(w, err.Error())
		return
	}
	res := s.probe(r, s.effectiveProvider(rr.provider), rr.baseURL, rr.repoPath, in.Token)
	writeJSON(w, http.StatusOK, rr.failed(res))
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
		v.StateReason, v.StateMessage = c.StateReason, c.StateMessage
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
	if !s.requireOwner(w, r, r.PathValue("id")) {
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
	s.refreshArchitecture(p.ID, true)
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
	s.registerWebhookCtx(r.Context(), p, token)
}

func (s *Server) registerWebhookCtx(ctx context.Context, p domain.Project, token string) bool {
	ok, err := s.registerWebhookErr(ctx, p, token)
	if err != nil {
		slog.Warn("регистрация webhook не удалась", "project", p.ID, "err", err)
	}
	return ok
}

// registerWebhookErr — регистрация с ошибкой хостинга наружу: переподписка
// при старте отличает лимит запросов от отсутствия прав.
func (s *Server) registerWebhookErr(ctx context.Context, p domain.Project, token string) (bool, error) {
	if s.PublicURL == "" || token == "" {
		return false, nil
	}
	adapter, err := s.adapters().For(scm.Provider(p.Provider), p.BaseURL, token)
	if err != nil {
		return false, err
	}
	ok, err := adapter.RegisterWebhook(ctx, p.RepoPath, s.webhookURL(p.Provider), p.WebhookSecret)
	if err != nil {
		return false, err
	}
	if !ok {
		slog.Info("webhook не зарегистрирован: нет прав, нужна ручная настройка", "project", p.ID)
		return false, nil
	}
	if err := s.St.SetWebhookRegistered(ctx, p.ID, true); err != nil {
		slog.Error("отметка регистрации webhook", "project", p.ID, "err", err)
	}
	if err := s.St.SetWebhookEvents(ctx, p.ID, scm.HookEventsVersion); err != nil {
		slog.Error("версия подписки webhook", "project", p.ID, "err", err)
	}
	return true, nil
}

// resubscribePause — пауза между проектами переподписки. У существующего
// хука один мутирующий запрос (GET, затем PATCH), а мутирующие запросы
// GitHub ограничивает отдельно (около 80 в минуту): пауза 2 с держит
// переподписку не выше 30 в минуту.
var resubscribePause = 2 * time.Second

// ResubscribeWebhooks переподписывает хуки, заведённые со старым набором
// событий (до подписки на push, add-architecture-c4). Зовётся при старте
// rivetd в фоне: сбой одного проекта не мешает остальным, а лимит
// запросов хостинга останавливает проход до следующего старта.
func (s *Server) ResubscribeWebhooks(ctx context.Context) {
	if s.PublicURL == "" {
		return
	}
	projects, err := s.St.ProjectsToResubscribe(ctx, scm.HookEventsVersion)
	if err != nil {
		slog.Error("переподписка webhook", "err", err)
		return
	}
	for i, p := range projects {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(resubscribePause):
			}
		}
		token, err := s.St.ProjectToken(ctx, p.ID, s.Secrets)
		if err != nil || token == "" {
			continue
		}
		ok, err := s.registerWebhookErr(ctx, p, token)
		switch {
		case errors.Is(err, scm.ErrRateLimited):
			slog.Warn("переподписка webhook остановлена лимитом запросов хостинга", "project", p.ID, "left", len(projects)-i)
			return
		case err != nil:
			slog.Warn("переподписка webhook не удалась", "project", p.ID, "err", err)
		case ok:
			slog.Info("webhook переподписан", "project", p.ID)
		}
	}
}
