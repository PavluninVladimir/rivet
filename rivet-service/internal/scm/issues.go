package scm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// IssueReader is optional: only GitHub projects can enable issue intake.
type IssueReader interface {
	ListIssues(context.Context, string, string, int, string) (IssuePage, error)
}

type Issue struct {
	Number      int
	Title, Body string
}

type IssuePage struct {
	Issues      []Issue
	NextPage    int
	ETag        string
	NotModified bool
}

var ErrIssuesUnsupported = errors.New("GitHub Issues недоступны для этого адаптера")

// ListIssues never follows response URLs: pagination stays on the configured API.
// GitHub lists PRs as issues, so the label and PR exclusion are also checked locally.
func (g *GitHub) ListIssues(ctx context.Context, repo, label string, page int, etag string) (IssuePage, error) {
	var out IssuePage
	if page < 1 {
		page = 1
	}
	q := url.Values{"state": {"open"}, "labels": {label}, "sort": {"created"}, "direction": {"asc"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.Base+"/repos/"+repo+"/issues?"+q.Encode(), nil)
	if err != nil {
		return out, errors.New("не удалось создать запрос GitHub Issues")
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if etag != "" && page == 1 {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return out, errors.New("GitHub Issues: соединение недоступно")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil || len(raw) > MaxResponseBytes {
		return out, errors.New("GitHub Issues: ответ не прочитан или превышает лимит")
	}
	if rateLimited(resp.StatusCode, resp.Header, raw) || resp.StatusCode == http.StatusTooManyRequests {
		return out, &RateLimitError{After: retryAfter(resp.Header), Msg: "GitHub Issues: лимит запросов"}
	}
	switch resp.StatusCode {
	case http.StatusNotModified:
		if etag == "" {
			return out, errors.New("GitHub Issues: неожиданный 304")
		}
		out.NotModified = true
		out.ETag = etag
		return out, nil
	case http.StatusUnauthorized:
		return out, ErrUnauthorized
	case http.StatusForbidden:
		return out, ErrForbidden
	case http.StatusNotFound:
		return out, ErrNotFound
	case http.StatusOK:
	default:
		return out, fmt.Errorf("GitHub Issues: HTTP %d", resp.StatusCode)
	}
	var items []struct {
		Number int             `json:"number"`
		Title  string          `json:"title"`
		Body   string          `json:"body"`
		State  string          `json:"state"`
		PR     json.RawMessage `json:"pull_request"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return out, errors.New("GitHub Issues: неверный ответ")
	}
	for _, it := range items {
		if it.Number <= 0 || it.State != "open" || (len(it.PR) > 0 && string(it.PR) != "null") {
			continue
		}
		for _, l := range it.Labels {
			if strings.EqualFold(l.Name, label) {
				out.Issues = append(out.Issues, Issue{Number: it.Number, Title: it.Title, Body: it.Body})
				break
			}
		}
	}
	if strings.Contains(resp.Header.Get("Link"), `rel="next"`) {
		out.NextPage = page + 1
	}
	// At exactly 100 items, a new issue can appear only on page 2 while
	// page 1's representation (and validator) remains unchanged.
	if page == 1 && out.NextPage == 0 && len(items) < 100 {
		out.ETag = resp.Header.Get("ETag")
	}
	return out, nil
}
