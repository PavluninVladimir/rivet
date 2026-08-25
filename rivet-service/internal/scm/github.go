package scm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GitHub — адаптер GitHub REST API v3 от имени бот-идентичности (токен).
type GitHub struct {
	Token  string
	Client *http.Client
	Base   string // API-хост; по умолчанию https://api.github.com
	Web    string // корень инстанса для ссылок; по умолчанию https://github.com
}

func NewGitHub(token string) *GitHub {
	return &GitHub{Token: token, Client: &http.Client{Timeout: 30 * time.Second},
		Base: "https://api.github.com", Web: "https://github.com"}
}

func (g *GitHub) do(ctx context.Context, method, path string, body any, accept string) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.Base+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if accept == "" {
		accept = "application/vnd.github+json"
	}
	req.Header.Set("Accept", accept)
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	return raw, resp.StatusCode, err
}

func (g *GitHub) CreatePR(ctx context.Context, repo, branch, base, title, body string) (PR, error) {
	raw, code, err := g.do(ctx, "POST", "/repos/"+repo+"/pulls", map[string]any{
		"title": title, "head": branch, "base": base, "body": body,
	}, "")
	if err != nil {
		return PR{}, err
	}
	if code != http.StatusCreated {
		return PR{}, fmt.Errorf("github create PR: %d: %s", code, raw)
	}
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return PR{}, err
	}
	return PR{Number: out.Number, URL: out.HTMLURL}, nil
}

func (g *GitHub) Diff(ctx context.Context, repo string, number int) (string, error) {
	raw, code, err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/pulls/%d", repo, number),
		nil, "application/vnd.github.diff")
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("github diff: %d: %s", code, raw)
	}
	// Тело режется лимитом молча: сигнализируем обрезку, чтобы решения по
	// путям PR не принимались по части diff'а.
	if len(raw) >= MaxResponseBytes {
		return string(raw), ErrDiffTruncated
	}
	return string(raw), nil
}

func (g *GitHub) Merge(ctx context.Context, repo string, number int) (string, error) {
	raw, code, err := g.do(ctx, "PUT", fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, number),
		map[string]any{"merge_method": "squash"}, "")
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("github merge: %d: %s", code, raw)
	}
	var out struct {
		SHA string `json:"sha"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

// HeadSHA — sha вершины ветки через branches API.
func (g *GitHub) HeadSHA(ctx context.Context, repo, branch string) (string, error) {
	raw, code, err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/branches/%s", repo, branch), nil, "")
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("github branch: %d: %s", code, raw)
	}
	var out struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.Commit.SHA, nil
}

// NewGitHubAt — адаптер для инстанса по URL: github.com ходит в
// api.github.com, GitHub Enterprise — в <base>/api/v3 (спека
// scm-integration «Первая волна адаптеров»).
func NewGitHubAt(baseURL, token string) *GitHub {
	api := "https://api.github.com"
	switch baseURL {
	case "", "https://github.com", "http://github.com":
	default:
		api = strings.TrimSuffix(baseURL, "/") + "/api/v3"
	}
	return &GitHub{Token: token, Client: httpClient(), Base: api, Web: webBase(baseURL)}
}

func webBase(baseURL string) string {
	if baseURL == "" {
		return "https://github.com"
	}
	return strings.TrimSuffix(baseURL, "/")
}

// probeReason переводит код ответа хостинга в причину отказа.
func probeReason(code int, raw []byte) ProbeResult {
	switch code {
	case http.StatusUnauthorized:
		return probeFail(ReasonBadToken, "токен не принят хостингом")
	case http.StatusNotFound:
		// GitHub отвечает 404 и на приватный репозиторий без доступа:
		// различить «нет» и «не видно» нельзя, поэтому формулировка общая.
		return probeFail(ReasonNotFound, "репозиторий не найден или недоступен по этому токену")
	case http.StatusForbidden:
		return probeFail(ReasonNoAccess, "доступ запрещён: "+tailMsg(raw))
	}
	return probeFail(ReasonUnreachable, fmt.Sprintf("хостинг ответил %d: %s", code, tailMsg(raw)))
}

func tailMsg(raw []byte) string {
	s := string(raw)
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// Probe проверяет токен и (если repo задан) доступ к репозиторию с правами.
func (g *GitHub) Probe(ctx context.Context, repo string) ProbeResult {
	raw, code, err := g.do(ctx, "GET", "/user", nil, "")
	if err != nil {
		return probeFail(ReasonUnreachable, "не удалось связаться с хостингом: "+err.Error())
	}
	if code != http.StatusOK {
		return probeReason(code, raw)
	}
	var me struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(raw, &me); err != nil {
		return probeFail(ReasonUnreachable, "неожиданный ответ хостинга")
	}
	if repo == "" {
		return ProbeResult{OK: true, TokenOwner: me.Login}
	}

	raw, code, err = g.do(ctx, "GET", "/repos/"+repo, nil, "")
	if err != nil {
		return probeFail(ReasonUnreachable, "не удалось связаться с хостингом: "+err.Error())
	}
	if code != http.StatusOK {
		out := probeReason(code, raw)
		out.TokenOwner = me.Login
		return out
	}
	var info struct {
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Permissions   struct {
			Push bool `json:"push"`
			Pull bool `json:"pull"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return probeFail(ReasonUnreachable, "неожиданный ответ хостинга")
	}
	res := ProbeResult{
		TokenOwner: me.Login, RepoPath: info.FullName, DefaultBranch: info.DefaultBranch,
		CanPush: info.Permissions.Push, CanMergeRequest: info.Permissions.Push,
	}
	if !res.CanPush {
		res.Reason = ReasonNoScope
		res.Message = "токен даёт только чтение: нужны права на push и создание pull request"
		return res
	}
	res.OK = true
	return res
}

// CreateRepo создаёт репозиторий с начальным коммитом (auto_init): без него
// нет базовой ветки, и конвейеру не от чего ответвляться.
func (g *GitHub) CreateRepo(ctx context.Context, in NewRepo) (RepoInfo, error) {
	path := "/user/repos"
	me, err := g.login(ctx)
	if err != nil {
		return RepoInfo{}, err
	}
	if in.Owner != "" && !strings.EqualFold(in.Owner, me) {
		path = "/orgs/" + in.Owner + "/repos"
	}
	raw, code, err := g.do(ctx, "POST", path, map[string]any{
		"name": in.Name, "private": in.Private, "auto_init": true,
	}, "")
	if err != nil {
		return RepoInfo{}, err
	}
	if code == http.StatusUnprocessableEntity && strings.Contains(string(raw), "already exists") {
		return RepoInfo{}, ErrRepoExists
	}
	if code != http.StatusCreated {
		return RepoInfo{}, fmt.Errorf("github create repo: %d: %s", code, tailMsg(raw))
	}
	var out struct {
		FullName      string `json:"full_name"`
		HTMLURL       string `json:"html_url"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return RepoInfo{}, err
	}
	if out.DefaultBranch == "" {
		out.DefaultBranch = "main"
	}
	return RepoInfo{Path: out.FullName, WebURL: out.HTMLURL, DefaultBranch: out.DefaultBranch}, nil
}

func (g *GitHub) login(ctx context.Context) (string, error) {
	raw, code, err := g.do(ctx, "GET", "/user", nil, "")
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("github user: %d: %s", code, tailMsg(raw))
	}
	var me struct {
		Login string `json:"login"`
	}
	err = json.Unmarshal(raw, &me)
	return me.Login, err
}

// RegisterWebhook подписывает репозиторий на события задач и merge.
// Если хук с таким URL уже есть, он обновляется: у старого хука остался
// прежний секрет, и «успешная» регистрация без обновления означала бы,
// что события начнут отклоняться по подписи.
func (g *GitHub) RegisterWebhook(ctx context.Context, repo, url, secret string) (bool, error) {
	config := map[string]any{
		"url": url, "content_type": "json", "secret": secret, "insecure_ssl": "0",
	}
	raw, code, err := g.do(ctx, "POST", "/repos/"+repo+"/hooks", map[string]any{
		"name": "web", "active": true, "events": []string{"pull_request"}, "config": config,
	}, "")
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusCreated, http.StatusOK:
		return true, nil
	case http.StatusForbidden, http.StatusNotFound:
		// Нет прав администрировать репозиторий — подключение не блокируем.
		return false, nil
	case http.StatusUnprocessableEntity:
		// 422 бывает и «хук уже есть», и «невалидный URL».
		if !strings.Contains(strings.ToLower(string(raw)), "already exists") {
			return false, fmt.Errorf("github webhook: %d: %s", code, tailMsg(raw))
		}
		return g.updateWebhook(ctx, repo, url, config)
	}
	return false, fmt.Errorf("github webhook: %d: %s", code, tailMsg(raw))
}

// updateWebhook находит хук с нашим URL и переписывает его конфигурацию
// (в первую очередь секрет).
func (g *GitHub) updateWebhook(ctx context.Context, repo, url string, config map[string]any) (bool, error) {
	raw, code, err := g.do(ctx, "GET", "/repos/"+repo+"/hooks", nil, "")
	if err != nil {
		return false, err
	}
	if code == http.StatusForbidden || code == http.StatusNotFound {
		return false, nil
	}
	if code != http.StatusOK {
		return false, fmt.Errorf("github hooks: %d: %s", code, tailMsg(raw))
	}
	var hooks []struct {
		ID     int64 `json:"id"`
		Config struct {
			URL string `json:"url"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return false, err
	}
	for _, h := range hooks {
		if h.Config.URL != url {
			continue
		}
		raw, code, err := g.do(ctx, "PATCH", fmt.Sprintf("/repos/%s/hooks/%d", repo, h.ID),
			map[string]any{"active": true, "events": []string{"pull_request"}, "config": config}, "")
		if err != nil {
			return false, err
		}
		if code == http.StatusOK {
			return true, nil
		}
		if code == http.StatusForbidden || code == http.StatusNotFound {
			return false, nil
		}
		return false, fmt.Errorf("github webhook update: %d: %s", code, tailMsg(raw))
	}
	// Хук с нашим URL не нашёлся: считать регистрацию выполненной нельзя.
	return false, nil
}

// ─── пайплайны доставки (спека deployment) ───────────────────────────────

// TriggerPipeline запускает workflow GitHub Actions через workflow_dispatch.
// Ответ API пустой: идентификатор прогона придётся искать отдельно, поэтому
// возвращается состояние «запускается».
func (g *GitHub) TriggerPipeline(ctx context.Context, repo, pipeline, ref string, vars map[string]string) (PipelineRun, error) {
	if pipeline == "" {
		return PipelineRun{}, fmt.Errorf("github: нужен файл workflow (например deploy.yml)")
	}
	inputs := map[string]any{}
	for k, v := range vars {
		inputs[k] = v
	}
	raw, code, err := g.do(ctx, "POST",
		fmt.Sprintf("/repos/%s/actions/workflows/%s/dispatches", repo, url.PathEscape(pipeline)),
		map[string]any{"ref": ref, "inputs": inputs}, "")
	if err != nil {
		return PipelineRun{}, err
	}
	if code != http.StatusNoContent && code != http.StatusCreated {
		return PipelineRun{}, fmt.Errorf("github workflow_dispatch: %d: %s", code, clip(raw))
	}
	return PipelineRun{State: PipelineStarting}, nil
}

// PipelineRun — состояние прогона. Без известного идентификатора ищется
// свежий прогон этого workflow на ветке, начавшийся не раньше since:
// workflow_dispatch идентификатор не возвращает.
func (g *GitHub) PipelineRun(ctx context.Context, repo, pipeline, ref, runID string, since time.Time) (PipelineRun, error) {
	if runID != "" {
		raw, code, err := g.do(ctx, "GET", fmt.Sprintf("/repos/%s/actions/runs/%s", repo, url.PathEscape(runID)), nil, "")
		if err != nil {
			return PipelineRun{}, err
		}
		if code != http.StatusOK {
			return PipelineRun{}, fmt.Errorf("github run: %d: %s", code, clip(raw))
		}
		var run githubRun
		if err := json.Unmarshal(raw, &run); err != nil {
			return PipelineRun{}, err
		}
		return run.pipeline(), nil
	}
	path := fmt.Sprintf("/repos/%s/actions/workflows/%s/runs?event=workflow_dispatch&per_page=10",
		repo, url.PathEscape(pipeline))
	if ref != "" {
		path += "&branch=" + url.QueryEscape(ref)
	}
	raw, code, err := g.do(ctx, "GET", path, nil, "")
	if err != nil {
		return PipelineRun{}, err
	}
	if code != http.StatusOK {
		return PipelineRun{}, fmt.Errorf("github runs: %d: %s", code, clip(raw))
	}
	var out struct {
		WorkflowRuns []githubRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return PipelineRun{}, err
	}
	// Прогоны приходят от свежих к старым: берём первый, начавшийся не
	// раньше запуска публикации. workflow_dispatch идентификатор не
	// возвращает, поэтому окно узкое — чужой прошлый прогон того же
	// workflow на той же ветке не должен попасть в публикацию.
	for _, run := range out.WorkflowRuns {
		if !run.CreatedAt.IsZero() && run.CreatedAt.Before(since.Add(-15*time.Second)) {
			continue
		}
		return run.pipeline(), nil
	}
	return PipelineRun{State: PipelineStarting}, nil
}

type githubRun struct {
	ID         int64     `json:"id"`
	HTMLURL    string    `json:"html_url"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	CreatedAt  time.Time `json:"created_at"`
}

func (r githubRun) pipeline() PipelineRun {
	p := PipelineRun{RunID: fmt.Sprintf("%d", r.ID), URL: r.HTMLURL, State: PipelineRunning}
	if r.Status != "completed" {
		return p
	}
	switch strings.ToLower(r.Conclusion) {
	case "success", "neutral":
		p.State = PipelineSuccess
	default:
		p.State = PipelineFailed
	}
	return p
}
