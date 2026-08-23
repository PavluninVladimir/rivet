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
)

// GitLab — адаптер GitLab REST API v4, облачный и self-hosted. MR прячется
// за теми же операциями, что PR у GitHub: конвейер различий не видит
// (спека scm-integration «Протокол SCM-адаптера»).
type GitLab struct {
	Token  string
	Client *http.Client
	Base   string // корень инстанса, например https://gitlab.com
}

func NewGitLab(baseURL, token string) *GitLab {
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}
	return &GitLab{Token: token, Client: httpClient(), Base: strings.TrimSuffix(baseURL, "/")}
}

// projectID — путь репозитория в виде, который принимает API GitLab
// (вложенные группы кодируются целиком).
func projectID(repo string) string { return url.PathEscape(repo) }

func (g *GitLab) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.Base+"/api/v4"+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("PRIVATE-TOKEN", g.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	return raw, resp.StatusCode, err
}

// CreatePR создаёт merge request.
func (g *GitLab) CreatePR(ctx context.Context, repo, branch, base, title, body string) (PR, error) {
	raw, code, err := g.do(ctx, "POST", "/projects/"+projectID(repo)+"/merge_requests", map[string]any{
		"source_branch": branch, "target_branch": base, "title": title, "description": body,
	})
	if err != nil {
		return PR{}, err
	}
	if code != http.StatusCreated {
		return PR{}, fmt.Errorf("gitlab create MR: %d: %s", code, tailMsg(raw))
	}
	var out struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return PR{}, err
	}
	return PR{Number: out.IID, URL: out.WebURL}, nil
}

// Diff возвращает изменения MR в формате, пригодном для review-агента.
func (g *GitLab) Diff(ctx context.Context, repo string, number int) (string, error) {
	raw, code, err := g.do(ctx, "GET",
		fmt.Sprintf("/projects/%s/merge_requests/%d/changes", projectID(repo), number), nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("gitlab MR changes: %d: %s", code, tailMsg(raw))
	}
	var out struct {
		Changes []struct {
			OldPath string `json:"old_path"`
			NewPath string `json:"new_path"`
			Diff    string `json:"diff"`
		} `json:"changes"`
		// Overflow — GitLab упёрся в diff limits и отдал часть изменений.
		Overflow bool `json:"overflow"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, c := range out.Changes {
		fmt.Fprintf(&b, "diff --git a/%s b/%s\n%s\n", c.OldPath, c.NewPath, c.Diff)
	}
	if out.Overflow {
		return b.String(), ErrDiffTruncated
	}
	return b.String(), nil
}

// Merge выполняет merge MR и возвращает sha merge-коммита.
func (g *GitLab) Merge(ctx context.Context, repo string, number int) (string, error) {
	raw, code, err := g.do(ctx, "PUT",
		fmt.Sprintf("/projects/%s/merge_requests/%d/merge", projectID(repo), number), map[string]any{})
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("gitlab merge: %d: %s", code, tailMsg(raw))
	}
	var out struct {
		SHA       string `json:"sha"`
		MergeSHA  string `json:"merge_commit_sha"`
		SquashSHA string `json:"squash_commit_sha"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	for _, sha := range []string{out.MergeSHA, out.SquashSHA, out.SHA} {
		if sha != "" {
			return sha, nil
		}
	}
	return "", nil
}

// HeadSHA — вершина ветки.
func (g *GitLab) HeadSHA(ctx context.Context, repo, branch string) (string, error) {
	raw, code, err := g.do(ctx, "GET",
		"/projects/"+projectID(repo)+"/repository/branches/"+url.PathEscape(branch), nil)
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("gitlab branch: %d: %s", code, tailMsg(raw))
	}
	var out struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.Commit.ID, nil
}

// Probe проверяет токен и доступ к проекту GitLab с правами на push и MR.
func (g *GitLab) Probe(ctx context.Context, repo string) ProbeResult {
	raw, code, err := g.do(ctx, "GET", "/user", nil)
	if err != nil {
		return probeFail(ReasonUnreachable, "не удалось связаться с инстансом: "+err.Error())
	}
	if code != http.StatusOK {
		return probeReason(code, raw)
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := json.Unmarshal(raw, &me); err != nil {
		return probeFail(ReasonUnreachable, "неожиданный ответ инстанса")
	}
	if repo == "" {
		return ProbeResult{OK: true, TokenOwner: me.Username}
	}

	raw, code, err = g.do(ctx, "GET", "/projects/"+projectID(repo), nil)
	if err != nil {
		return probeFail(ReasonUnreachable, "не удалось связаться с инстансом: "+err.Error())
	}
	if code != http.StatusOK {
		out := probeReason(code, raw)
		out.TokenOwner = me.Username
		return out
	}
	var info struct {
		PathWithNamespace string `json:"path_with_namespace"`
		DefaultBranch     string `json:"default_branch"`
		Permissions       struct {
			ProjectAccess *struct {
				AccessLevel int `json:"access_level"`
			} `json:"project_access"`
			GroupAccess *struct {
				AccessLevel int `json:"access_level"`
			} `json:"group_access"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return probeFail(ReasonUnreachable, "неожиданный ответ инстанса")
	}
	// 30 — developer: минимальный уровень, дающий push в ветку и MR.
	const developer = 30
	level := 0
	if a := info.Permissions.ProjectAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	if a := info.Permissions.GroupAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	res := ProbeResult{
		TokenOwner: me.Username, RepoPath: info.PathWithNamespace, DefaultBranch: info.DefaultBranch,
		CanPush: level >= developer, CanMergeRequest: level >= developer,
	}
	if !res.CanPush {
		res.Reason = ReasonNoScope
		res.Message = "уровень доступа ниже developer: нужны права на push и merge request"
		return res
	}
	res.OK = true
	return res
}

// CreateRepo создаёт проект GitLab с начальным коммитом.
func (g *GitLab) CreateRepo(ctx context.Context, in NewRepo) (RepoInfo, error) {
	body := map[string]any{
		"name":                   in.Name,
		"path":                   in.Name,
		"initialize_with_readme": true,
		"visibility":             "public",
	}
	if in.Private {
		body["visibility"] = "private"
	}
	if in.Owner != "" {
		id, err := g.namespaceID(ctx, in.Owner)
		if err != nil {
			return RepoInfo{}, err
		}
		if id != 0 {
			body["namespace_id"] = id
		}
	}
	raw, code, err := g.do(ctx, "POST", "/projects", body)
	if err != nil {
		return RepoInfo{}, err
	}
	if code == http.StatusBadRequest && strings.Contains(string(raw), "has already been taken") {
		return RepoInfo{}, ErrRepoExists
	}
	if code != http.StatusCreated {
		return RepoInfo{}, fmt.Errorf("gitlab create project: %d: %s", code, tailMsg(raw))
	}
	var out struct {
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
		DefaultBranch     string `json:"default_branch"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return RepoInfo{}, err
	}
	if out.DefaultBranch == "" {
		out.DefaultBranch = "main"
	}
	return RepoInfo{Path: out.PathWithNamespace, WebURL: out.WebURL, DefaultBranch: out.DefaultBranch}, nil
}

// namespaceID ищет группу или пользователя владельца; 0 — личное
// пространство токена (GitLab подставит его сам).
func (g *GitLab) namespaceID(ctx context.Context, owner string) (int, error) {
	raw, code, err := g.do(ctx, "GET", "/namespaces?search="+url.QueryEscape(owner), nil)
	if err != nil {
		return 0, err
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("gitlab namespaces: %d: %s", code, tailMsg(raw))
	}
	var list []struct {
		ID       int    `json:"id"`
		FullPath string `json:"full_path"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return 0, err
	}
	for _, n := range list {
		if strings.EqualFold(n.FullPath, owner) {
			return n.ID, nil
		}
	}
	return 0, fmt.Errorf("владелец %q не найден среди доступных пространств имён", owner)
}

// RegisterWebhook подписывает проект на события MR; секрет проекта уезжает
// в X-Gitlab-Token. Существующий хук с тем же URL обновляется, а не
// дублируется: иначе у старого остался бы прежний токен.
func (g *GitLab) RegisterWebhook(ctx context.Context, repo, hookURL, secret string) (bool, error) {
	body := map[string]any{
		"url": hookURL, "token": secret, "merge_requests_events": true, "push_events": false,
	}
	raw, code, err := g.do(ctx, "GET", "/projects/"+projectID(repo)+"/hooks", nil)
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusOK:
		var hooks []struct {
			ID  int64  `json:"id"`
			URL string `json:"url"`
		}
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return false, err
		}
		for _, h := range hooks {
			if h.URL != hookURL {
				continue
			}
			raw, code, err := g.do(ctx, "PUT",
				fmt.Sprintf("/projects/%s/hooks/%d", projectID(repo), h.ID), body)
			if err != nil {
				return false, err
			}
			if code == http.StatusOK {
				return true, nil
			}
			if code == http.StatusForbidden || code == http.StatusNotFound {
				return false, nil
			}
			return false, fmt.Errorf("gitlab webhook update: %d: %s", code, tailMsg(raw))
		}
	case http.StatusForbidden, http.StatusNotFound:
		return false, nil // нет прав администрировать проект — не блокируем
	default:
		return false, fmt.Errorf("gitlab hooks: %d: %s", code, tailMsg(raw))
	}

	raw, code, err = g.do(ctx, "POST", "/projects/"+projectID(repo)+"/hooks", body)
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusCreated, http.StatusOK:
		return true, nil
	case http.StatusForbidden, http.StatusNotFound:
		return false, nil
	}
	return false, fmt.Errorf("gitlab webhook: %d: %s", code, tailMsg(raw))
}
