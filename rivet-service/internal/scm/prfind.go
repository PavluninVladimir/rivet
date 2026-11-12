package scm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PRFinder — поиск открытого PR ветки задачи (fix-pr-without-test-step).
// Создание PR повторяется без дубля: если прошлый запрос PR создал, а ответ
// потерялся (таймаут, рестарт rivetd между созданием и записью), хостинг
// отвечает «уже есть», и найденный PR записывается задаче. Подходит только
// PR из ветки самого репозитория (не из форка) в базовую ветку base;
// несколько подходящих — ErrAmbiguousPR, выбирать среди них нельзя.
type PRFinder interface {
	OpenPR(ctx context.Context, repo, branch, base string) (PR, bool, error)
}

// Адаптеры хостингов ищут открытый PR ветки.
var (
	_ PRFinder = (*GitHub)(nil)
	_ PRFinder = (*GitLab)(nil)
	_ PRFinder = (*Gitea)(nil)
)

// ErrAmbiguousPR — подходящих открытых PR ветки несколько.
var ErrAmbiguousPR = errors.New("открытых PR ветки несколько")

// OpenPR — открытый PR из ветки branch репозитория repo в base.
func (g *GitHub) OpenPR(ctx context.Context, repo, branch, base string) (PR, bool, error) {
	owner, _, _ := strings.Cut(repo, "/")
	raw, code, err := g.do(ctx, "GET", "/repos/"+repo+"/pulls?state=open&head="+url.QueryEscape(owner+":"+branch)+
		"&base="+url.QueryEscape(base), nil, "")
	if err != nil {
		return PR{}, false, err
	}
	if code != http.StatusOK {
		return PR{}, false, statusErr("github list PR", code, raw)
	}
	var out []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		Head    struct {
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return PR{}, false, err
	}
	var found []PR
	for _, p := range out {
		// Фильтры запроса проверяются и по ответу: PR из форка или в другую
		// базовую ветку задаче не подходит.
		if p.Head.Ref != branch || !strings.EqualFold(p.Head.Repo.FullName, repo) || p.Base.Ref != base {
			continue
		}
		found = append(found, PR{Number: p.Number, URL: p.HTMLURL})
	}
	return onePR(found)
}

// OpenPR — открытый merge request из ветки branch самого проекта в base.
func (g *GitLab) OpenPR(ctx context.Context, repo, branch, base string) (PR, bool, error) {
	raw, code, err := g.do(ctx, "GET", "/projects/"+projectID(repo)+"/merge_requests?state=opened&source_branch="+url.QueryEscape(branch)+
		"&target_branch="+url.QueryEscape(base), nil)
	if err != nil {
		return PR{}, false, err
	}
	if code != http.StatusOK {
		return PR{}, false, statusErr("gitlab list MR", code, raw)
	}
	var out []struct {
		IID             int    `json:"iid"`
		WebURL          string `json:"web_url"`
		SourceBranch    string `json:"source_branch"`
		TargetBranch    string `json:"target_branch"`
		SourceProjectID int64  `json:"source_project_id"`
		TargetProjectID int64  `json:"target_project_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return PR{}, false, err
	}
	var found []PR
	for _, m := range out {
		// Список MR проекта включает MR из форков: подходит только MR из
		// ветки самого проекта в базовую ветку задачи.
		if m.SourceProjectID == 0 || m.SourceProjectID != m.TargetProjectID || m.SourceBranch != branch || m.TargetBranch != base {
			continue
		}
		found = append(found, PR{Number: m.IID, URL: m.WebURL})
	}
	return onePR(found)
}

func onePR(found []PR) (PR, bool, error) {
	switch len(found) {
	case 0:
		return PR{}, false, nil
	case 1:
		return found[0], true, nil
	}
	return PR{}, false, ErrAmbiguousPR
}

// ErrPRExists — хостинг отказал в создании PR, потому что открытый PR ветки
// уже есть: GitHub отвечает 422 «A pull request already exists», GitLab 409
// «Another open merge request already exists for this source branch».
// Адаптер узнаёт отказ по коду ответа и разобранному телу, а не по тексту
// ошибки. Только на такой отказ стоит искать существующий PR.
var ErrPRExists = errors.New("открытый PR ветки уже есть")

// hostMessages — тексты ошибки в JSON-ответе хостинга: message (строка у
// GitHub, строка или список строк у GitLab) и errors[] (объекты с message
// у GitHub или строки). Тело с отступами и переводами строк разбирается
// так же, как однострочное.
func hostMessages(raw []byte) []string {
	var body struct {
		Message json.RawMessage   `json:"message"`
		Errors  []json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	var out []string
	add := func(r json.RawMessage) {
		var one string
		var many []string
		var obj struct {
			Message string `json:"message"`
		}
		switch {
		case json.Unmarshal(r, &one) == nil:
			out = append(out, one)
		case json.Unmarshal(r, &many) == nil:
			out = append(out, many...)
		case json.Unmarshal(r, &obj) == nil:
			out = append(out, obj.Message)
		}
	}
	if len(body.Message) > 0 {
		add(body.Message)
	}
	for _, e := range body.Errors {
		add(e)
	}
	return out
}

// saysAlreadyExists — в ответе хостинга сказано, что объект уже есть.
func saysAlreadyExists(raw []byte) bool {
	for _, m := range hostMessages(raw) {
		if strings.Contains(strings.ToLower(m), "already exists") {
			return true
		}
	}
	return false
}

// RateLimitError — хостинг отказал по лимиту запросов (это ErrRateLimited):
// After — пауза из заголовка Retry-After, 0 — хостинг паузу не назвал.
type RateLimitError struct {
	After time.Duration
	Msg   string
}

func (e *RateLimitError) Error() string { return e.Msg }

// Unwrap — RateLimitError это ErrRateLimited.
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

// RetryAfter — пауза, которую хостинг просил выдержать перед повтором.
func RetryAfter(err error) time.Duration {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl.After
	}
	return 0
}

// retryAfter — заголовок Retry-After: секунды или дата HTTP.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

// hostStatusRe — первый код ответа хостинга в тексте ошибки адаптера
// («github create PR: 502: …», в том числе с префиксом вызывающего).
var hostStatusRe = regexp.MustCompile(`: (\d{3}): `)

// Transient — сбой хостинга, который обычно проходит сам: ответ 5xx или
// 429, лимит запросов (в том числе вторичный лимит GitHub с ответом 403),
// обрыв соединения, таймаут. Отказы 4xx (в ветке нет изменений, нет прав,
// PR уже есть) повтор не исправит.
func Transient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRateLimited) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	if m := hostStatusRe.FindStringSubmatch(err.Error()); m != nil {
		code, _ := strconv.Atoi(m[1])
		return code >= 500 || code == http.StatusTooManyRequests
	}
	return false
}
