package scm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GitHub — адаптер GitHub REST API v3 от имени бот-идентичности (токен).
type GitHub struct {
	Token  string
	Client *http.Client
	Base   string // для тестов; по умолчанию https://api.github.com
}

func NewGitHub(token string) *GitHub {
	return &GitHub{Token: token, Client: &http.Client{Timeout: 30 * time.Second}, Base: "https://api.github.com"}
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
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
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
