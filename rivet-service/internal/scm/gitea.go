package scm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Gitea — адаптер Gitea и Forgejo (REST API v1, change add-gitea-adapter).
// Инстанс всегда задаётся адресом: облачного «по умолчанию» у Gitea нет.
// Forgejo — форк Gitea, операции конвейера у них одинаковые. PR прячется за
// теми же операциями, что у GitHub и GitLab (спека scm-integration
// «Протокол SCM-адаптера»).
type Gitea struct {
	Token  string
	Client *http.Client
	Base   string // корень инстанса, например https://gitea.example.com
}

func NewGitea(baseURL, token string) *Gitea {
	return &Gitea{Token: token, Client: httpClient(), Base: strings.TrimSuffix(baseURL, "/")}
}

// giteaMergeWait — сколько ждать, пока Gitea досчитает проверку PR после
// сдвига базовой ветки (merge соседней задачи), прежде чем повторить
// merge. Переменная для тестов.
var giteaMergeWait = 6 * time.Second

// do — запрос к API инстанса. Токен — в заголовке Authorization со схемой
// token: её понимают Gitea и Forgejo всех версий.
func (g *Gitea) do(ctx context.Context, method, path string, body any, accept string) ([]byte, int, http.Header, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, 0, nil, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.Base+"/api/v1"+path, rd)
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Authorization", "token "+g.Token)
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	return raw, resp.StatusCode, resp.Header, err
}

// giteaErr — ошибка операции по неуспешному ответу инстанса. Одна точка на
// адаптер: отказы по авторизации классифицируются здесь же, как у
// остальных адаптеров.
func giteaErr(op string, code int, raw []byte) error {
	return fmt.Errorf("gitea %s: %d: %s", op, code, clip(raw))
}

// giteaRepo — путь репозитория в адресе API.
func giteaRepo(repo string) string { return "/repos/" + escapePath(repo) }

// giteaMissing — разбор 404 содержимого репозитория. Gitea называет
// отсутствующий объект строкой «object does not exist [id: <версия>,
// rel_path: <путь>]» (в поле message у новых версий, в errors у старых).
// Пустой id с путём — нет файла, непустой id — нет самой версии. ok=false —
// ответ без этой строки (нет репозитория или доступа к нему).
func giteaMissing(raw []byte) (id, relPath string, ok bool) {
	m := giteaMissingRe.FindSubmatch(raw)
	if m == nil {
		return "", "", false
	}
	return strings.TrimSpace(string(m[1])), strings.TrimSpace(string(m[2])), true
}

var giteaMissingRe = regexp.MustCompile(`object does not exist \[id: ([^,\]]*), rel_path: ([^\]]*)\]`)

// CreatePR открывает pull request из ветки задачи в базовую ветку. Отказ
// «PR ветки уже есть» (409 «pull request already exists for these
// targets») — ErrPRExists. Своего лимита запросов у Gitea нет, 429 отдаёт
// прокси перед инстансом: RateLimitError с паузой из Retry-After.
func (g *Gitea) CreatePR(ctx context.Context, repo, branch, base, title, body string) (PR, error) {
	raw, code, h, err := g.do(ctx, http.MethodPost, giteaRepo(repo)+"/pulls", map[string]any{
		"head": branch, "base": base, "title": title, "body": body,
	}, "")
	if err != nil {
		return PR{}, err
	}
	switch {
	case code == http.StatusCreated:
	case code == http.StatusTooManyRequests:
		return PR{}, &RateLimitError{After: retryAfter(h),
			Msg: fmt.Sprintf("gitea create PR: %v: %d: %s", ErrRateLimited, code, clip(raw))}
	case code == http.StatusConflict && saysAlreadyExists(raw):
		return PR{}, fmt.Errorf("gitea create PR: %w: %d: %s", ErrPRExists, code, clip(raw))
	default:
		return PR{}, giteaErr("create PR", code, raw)
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

// giteaPRPages — предел страниц открытых PR, которые читает запасной поиск
// OpenPR.
const giteaPRPages = 20

// giteaPRRef — PR в ответе Gitea: номер, ссылка, состояние и ветки с
// репозиториями.
type giteaPRRef struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Head    struct {
		Ref    string `json:"ref"`
		RepoID int64  `json:"repo_id"`
	} `json:"head"`
	Base struct {
		Ref    string `json:"ref"`
		RepoID int64  `json:"repo_id"`
	} `json:"base"`
}

// fits — открытый PR из ветки branch того же репозитория в base: PR из
// форка (другой head.repo_id) и в другую базу не подходят.
func (p giteaPRRef) fits(branch, base string) bool {
	return p.State == "open" && p.Head.Ref == branch && p.Base.Ref == base && p.Head.RepoID != 0 && p.Head.RepoID == p.Base.RepoID
}

// OpenPR — открытый PR из ветки branch самого репозитория в base. Сначала
// запрос PR по базе и ветке (GET /repos/{repo}/pulls/{base}/{head}, есть в
// Gitea 28 и Forgejo 15): он отдаёт один PR, но не обязательно открытый.
// Если он не подошёл, открытые PR читаются постранично от старых к новым,
// повторы на границе страниц отбрасываются по номеру.
func (g *Gitea) OpenPR(ctx context.Context, repo, branch, base string) (PR, bool, error) {
	if !strings.Contains(base, "/") {
		raw, code, _, err := g.do(ctx, http.MethodGet, giteaRepo(repo)+"/pulls/"+neturl.PathEscape(base)+"/"+escapePath(branch), nil, "")
		if err != nil {
			return PR{}, false, err
		}
		switch code {
		case http.StatusOK:
			var p giteaPRRef
			if err := json.Unmarshal(raw, &p); err != nil {
				return PR{}, false, err
			}
			if p.fits(branch, base) {
				return PR{Number: p.Number, URL: p.HTMLURL}, true, nil
			}
		case http.StatusNotFound:
		default:
			return PR{}, false, giteaErr("PR по базе и ветке", code, raw)
		}
	}
	seen := map[int]bool{}
	var found []PR
	total := 0
	for page := 1; ; page++ {
		if page > giteaPRPages {
			if len(found) > 0 {
				break
			}
			return PR{}, false, fmt.Errorf("gitea list PR: просмотрено %d открытых PR на %d страницах, PR ветки %s в %s среди них нет, остальные не читались",
				total, giteaPRPages, branch, base)
		}
		raw, code, _, err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls?state=open&sort=oldest&limit=50&page=%d", giteaRepo(repo), page), nil, "")
		if err != nil {
			return PR{}, false, err
		}
		if code != http.StatusOK {
			return PR{}, false, giteaErr("list PR", code, raw)
		}
		var out []giteaPRRef
		if err := json.Unmarshal(raw, &out); err != nil {
			return PR{}, false, err
		}
		if len(out) == 0 {
			break
		}
		for _, p := range out {
			if seen[p.Number] {
				continue
			}
			seen[p.Number] = true
			total++
			if p.State == "" {
				p.State = "open"
			}
			if p.fits(branch, base) {
				found = append(found, PR{Number: p.Number, URL: p.HTMLURL})
			}
		}
	}
	return onePR(found)
}

// Diff — unified diff PR (контекст review-агента).
func (g *Gitea) Diff(ctx context.Context, repo string, number int) (string, error) {
	raw, code, _, err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d.diff", giteaRepo(repo), number), nil, "text/plain")
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", giteaErr("diff", code, raw)
	}
	if len(raw) >= MaxResponseBytes {
		return string(raw), ErrDiffTruncated
	}
	return string(raw), nil
}

// giteaPull — то, что адаптеру нужно от PR.
type giteaPull struct {
	Number         int    `json:"number"`
	HTMLURL        string `json:"html_url"`
	State          string `json:"state"`
	Mergeable      bool   `json:"mergeable"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	ChangedFiles   int    `json:"changed_files"`
	Head           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func (g *Gitea) pull(ctx context.Context, repo string, number int) (giteaPull, error) {
	raw, code, _, err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/pulls/%d", giteaRepo(repo), number), nil, "")
	if err != nil {
		return giteaPull{}, err
	}
	if code != http.StatusOK {
		return giteaPull{}, giteaErr("PR", code, raw)
	}
	var pr giteaPull
	if err := json.Unmarshal(raw, &pr); err != nil {
		return giteaPull{}, err
	}
	return pr, nil
}

func (g *Gitea) Merge(ctx context.Context, repo string, number int) (string, error) {
	return g.MergeAt(ctx, repo, number, "")
}

// giteaMergeRetries — сколько раз merge повторяется после отказа «PR ещё
// пересчитывается» (405) или «база сдвинулась во время merge» (409
// PushRejected): соседние задачи мержатся одна за другой.
const giteaMergeRetries = 5

// MergeAt — merge PR способом, который выбран в репозитории по умолчанию,
// с проверкой вершины: Gitea отвечает 409 «head out of date», если ветка
// PR ушла дальше headSHA. Пустой headSHA — merge текущей вершины.
//
// Причину отказа 405 «Please try again later» Gitea не называет: так он
// отвечает и на конфликт с базовой веткой, и пока пересчитывает PR после
// сдвига базы. А если база сдвинулась уже во время merge (merge соседней
// задачи), push Gitea отклоняется: 409 «PushRejected … cannot lock ref»
// или «merge push out of date» (giteaMergeRaced). В обоих случаях адаптер ждёт, пока PR снова станет
// мержибельным, и повторяет merge с той же проверкой вершины. Не
// дождался — обычный отказ с пояснением, не конфликт (спека
// scm-integration «Распознавание конфликта при merge»). Конфликтом
// считается только ответ 409 с выводом git о конфликте.
func (g *Gitea) MergeAt(ctx context.Context, repo string, number int, headSHA string) (string, error) {
	style := g.mergeStyle(ctx, repo)
	for attempt := 0; ; attempt++ {
		body := map[string]any{"Do": style, "delete_branch_after_merge": false}
		if headSHA != "" {
			body["head_commit_id"] = headSHA
		}
		raw, code, _, err := g.do(ctx, http.MethodPost, fmt.Sprintf("%s/pulls/%d/merge", giteaRepo(repo), number), body, "")
		if err != nil {
			return "", err
		}
		msg := hostMessage(raw)
		pending := code == http.StatusMethodNotAllowed && strings.Contains(msg, "try again later")
		raced := code == http.StatusConflict && giteaMergeRaced(msg)
		switch {
		case code == http.StatusOK:
			return g.mergedSHA(ctx, repo, number), nil
		case code == http.StatusConflict && headSHA != "" && strings.Contains(msg, "head out of date"):
			return "", fmt.Errorf("gitea merge: %w: %s", ErrHeadMoved, tailMsg(raw))
		case code == http.StatusConflict && giteaMergeConflict(raw):
			return "", fmt.Errorf("gitea merge: %w: %s", ErrMergeConflict, tailMsg(raw))
		case pending || raced:
			if attempt < giteaMergeRetries && g.waitMergeable(ctx, repo, number) {
				continue
			}
			return "", fmt.Errorf("gitea merge: %d: PR не в состоянии merge — конфликт с базовой веткой, проверка PR ещё идёт или база сдвигается соседними merge: %s", code, tailMsg(raw))
		}
		return "", giteaErr("merge", code, raw)
	}
}

// giteaMergeRaced — push результата merge отклонён, потому что база
// сдвинулась (соседний merge): так отвечают Gitea 1.24 и 28.x («cannot lock
// ref»), Forgejo («PushRejected without remote error message») и часть
// версий («merge push out of date»). PushRejected от хука инстанса
// (политика коммитов, LFS) гонкой не считается и не повторяется.
func giteaMergeRaced(msg string) bool {
	return strings.Contains(msg, "merge push out of date") ||
		strings.Contains(msg, "pushrejected") && (strings.Contains(msg, "cannot lock ref") || strings.Contains(msg, "without remote error message"))
}

// giteaMergeConflict — отказ merge с выводом git о конфликте: так Gitea
// отдаёт ошибку слияния во временном клоне (поля StdOut и StdErr).
func giteaMergeConflict(raw []byte) bool {
	var out struct {
		StdOut string `json:"StdOut"`
		StdErr string `json:"StdErr"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return false
	}
	return strings.Contains(out.StdOut, "CONFLICT") || strings.Contains(out.StdErr, "CONFLICT")
}

// waitMergeable ждёт, пока Gitea досчитает PR: true — PR открыт и снова
// мержится.
func (g *Gitea) waitMergeable(ctx context.Context, repo string, number int) bool {
	deadline := time.Now().Add(giteaMergeWait)
	pause := giteaMergeWait / 6
	for {
		// Пауза до первого чтения: сразу после отказа PR может ещё числиться
		// мержибельным, пока Gitea не поставил его на пересчёт.
		select {
		case <-ctx.Done():
			return false
		case <-time.After(pause):
		}
		pr, err := g.pull(ctx, repo, number)
		if err == nil && pr.State == "open" && !pr.Merged && pr.Mergeable {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
	}
}

// mergedSHA — коммит merge: ответ на merge пустой, sha есть у PR. Merge уже
// состоялся, поэтому сбой чтения не делает его неуспешным: версии для
// автопубликации просто не будет.
func (g *Gitea) mergedSHA(ctx context.Context, repo string, number int) string {
	pr, err := g.pull(ctx, repo, number)
	if err != nil {
		return ""
	}
	return pr.MergeCommitSHA
}

// mergeStyle — способ merge по умолчанию в настройках репозитория (merge,
// squash, rebase…); без настройки — обычный merge-коммит.
func (g *Gitea) mergeStyle(ctx context.Context, repo string) string {
	info, err := g.repoInfo(ctx, repo)
	if err != nil || info.DefaultMergeStyle == "" {
		return "merge"
	}
	return info.DefaultMergeStyle
}

// giteaRepoInfo — поля репозитория, нужные адаптеру.
type giteaRepoInfo struct {
	FullName          string `json:"full_name"`
	HTMLURL           string `json:"html_url"`
	DefaultBranch     string `json:"default_branch"`
	DefaultMergeStyle string `json:"default_merge_style"`
	Empty             bool   `json:"empty"`
	Permissions       struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
		Pull  bool `json:"pull"`
	} `json:"permissions"`
}

func (g *Gitea) repoInfo(ctx context.Context, repo string) (giteaRepoInfo, error) {
	raw, code, _, err := g.do(ctx, http.MethodGet, giteaRepo(repo), nil, "")
	if err != nil {
		return giteaRepoInfo{}, err
	}
	if code != http.StatusOK {
		return giteaRepoInfo{}, giteaErr("repo", code, raw)
	}
	var info giteaRepoInfo
	err = json.Unmarshal(raw, &info)
	return info, err
}

// HeadSHA — вершина ветки. На отсутствие ветки и репозитория Gitea отвечает
// одинаковым 404, поэтому при 404 проверяется сам репозиторий: он есть —
// нет ветки (в том числе в пустом репозитории).
func (g *Gitea) HeadSHA(ctx context.Context, repo, branch string) (string, error) {
	raw, code, _, err := g.do(ctx, http.MethodGet, giteaRepo(repo)+"/branches/"+escapePath(branch), nil, "")
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		if _, rerr := g.repoInfo(ctx, repo); rerr == nil {
			return "", fmt.Errorf("gitea branch %s: %w", branch, ErrBranchNotFound)
		}
	}
	if code != http.StatusOK {
		return "", giteaErr("branch", code, raw)
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

// Probe проверяет токен и доступ к репозиторию с правом push (без него нет
// ни push ветки задачи, ни PR).
func (g *Gitea) Probe(ctx context.Context, repo string) ProbeResult {
	raw, code, _, err := g.do(ctx, http.MethodGet, "/user", nil, "")
	if err != nil {
		return probeFail(ReasonUnreachable, "не удалось связаться с инстансом: "+err.Error())
	}
	if code != http.StatusOK {
		return probeReason(code, raw)
	}
	var me struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(raw, &me); err != nil || me.Login == "" {
		return probeFail(ReasonUnreachable, "неожиданный ответ инстанса")
	}
	if repo == "" {
		return ProbeResult{OK: true, TokenOwner: me.Login}
	}
	raw, code, _, err = g.do(ctx, http.MethodGet, giteaRepo(repo), nil, "")
	if err != nil {
		return probeFail(ReasonUnreachable, "не удалось связаться с инстансом: "+err.Error())
	}
	if code != http.StatusOK {
		out := probeReason(code, raw)
		out.TokenOwner = me.Login
		return out
	}
	var info giteaRepoInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return probeFail(ReasonUnreachable, "неожиданный ответ инстанса")
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
	// permissions.push — права пользователя, а не токена: токен Gitea
	// всегда со scope'ами, и read:repository при праве записи у
	// пользователя проходит выше. Scope проверяется запросом, которому
	// нужен write:repository: создание PR с пустым телом. Scope Gitea
	// проверяет раньше тела — без него 403 с названием scope, с ним 422, и
	// PR не создаётся (проверено на Gitea 1.24, 28.x и Forgejo 15).
	raw, code, _, err = g.do(ctx, http.MethodPost, giteaRepo(repo)+"/pulls", map[string]any{}, "")
	if err != nil {
		return probeFail(ReasonUnreachable, "не удалось связаться с инстансом: "+err.Error())
	}
	switch code {
	case http.StatusUnprocessableEntity, http.StatusBadRequest:
		res.OK = true // запись есть, пустое тело отклонено, PR не создан
		return res
	case http.StatusForbidden:
		res.Reason = ReasonNoScope
		res.Message = "у токена нет права на запись (scope write:repository): нужны права на push и создание pull request: " + tailMsg(raw)
	case http.StatusNotFound:
		res.Reason = ReasonNoAccess
		res.Message = "в репозитории выключены pull request'ы: включите их в настройках репозитория"
	case http.StatusLocked:
		res.Reason = ReasonNoAccess
		res.Message = "репозиторий в архиве: push и pull request'ы недоступны"
	default:
		res.Reason = ReasonUnreachable
		res.Message = fmt.Sprintf("неожиданный ответ инстанса на проверку права записи: %d: %s", code, tailMsg(raw))
	}
	res.CanPush, res.CanMergeRequest = false, false
	return res
}

// CreateRepo создаёт репозиторий с начальным коммитом (auto_init) и
// веткой main: без них конвейеру не от чего ответвляться.
func (g *Gitea) CreateRepo(ctx context.Context, in NewRepo) (RepoInfo, error) {
	login, err := g.login(ctx)
	if err != nil {
		return RepoInfo{}, err
	}
	target := "/user/repos"
	if in.Owner != "" && !strings.EqualFold(in.Owner, login) {
		target = "/orgs/" + neturl.PathEscape(in.Owner) + "/repos"
	}
	raw, code, _, err := g.do(ctx, http.MethodPost, target, map[string]any{
		"name": in.Name, "private": in.Private, "auto_init": true,
		"default_branch": "main", "readme": "Default",
	}, "")
	if err != nil {
		return RepoInfo{}, err
	}
	if code == http.StatusConflict {
		return RepoInfo{}, ErrRepoExists
	}
	if code != http.StatusCreated {
		return RepoInfo{}, giteaErr("create repo", code, raw)
	}
	var info giteaRepoInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return RepoInfo{}, err
	}
	if info.DefaultBranch == "" {
		info.DefaultBranch = "main"
	}
	return RepoInfo{Path: info.FullName, WebURL: info.HTMLURL, DefaultBranch: info.DefaultBranch}, nil
}

// giteaHookEvents — события подписки: merge и закрытие PR (pull_request),
// review человека (pull_request_review: одобрение, запрос изменений и
// комментарий review), итоги Actions (workflow_run, есть в новых версиях
// Gitea; старые неизвестное событие пропускают) и push основной ветки для
// снимка архитектуры (спека scm-integration «События хостинга в конвейере»).
var giteaHookEvents = []string{"push", "pull_request", "pull_request_review", "workflow_run"}

// RegisterWebhook подписывает репозиторий на события; секрет проекта
// уходит в конфигурацию хука (подпись X-Gitea-Signature). Хук с тем же
// URL обновляется, а не дублируется: у старого остался бы прежний секрет.
// Push фильтруется основной веткой: пуши веток задач не нужны.
func (g *Gitea) RegisterWebhook(ctx context.Context, repo, hookURL, secret string) (bool, error) {
	filter := ""
	if info, err := g.repoInfo(ctx, repo); err == nil {
		filter = info.DefaultBranch
	}
	body := map[string]any{
		"type": "gitea", "active": true, "events": giteaHookEvents, "branch_filter": filter,
		"config": map[string]string{"url": hookURL, "content_type": "json", "secret": secret},
	}
	id, found, ok, err := g.findHook(ctx, repo, hookURL)
	if err != nil || !ok {
		return false, err // нет прав администрировать репозиторий — не блокируем
	}
	if found {
		// PATCH заменяет и фильтр веток: без него он сбросился бы.
		raw, code, _, err := g.do(ctx, http.MethodPatch, fmt.Sprintf("%s/hooks/%d", giteaRepo(repo), id), body, "")
		if err != nil {
			return false, err
		}
		switch code {
		case http.StatusOK:
			return true, nil
		case http.StatusForbidden, http.StatusNotFound:
			return false, nil
		}
		return false, giteaErr("webhook update", code, raw)
	}
	raw, code, _, err := g.do(ctx, http.MethodPost, giteaRepo(repo)+"/hooks", body, "")
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusCreated, http.StatusOK:
		return true, nil
	case http.StatusForbidden, http.StatusNotFound:
		return false, nil
	}
	return false, giteaErr("webhook", code, raw)
}

// giteaHooksPage — хуков за страницу при поиске своего.
const giteaHooksPage = 50

// findHook ищет хук с нашим URL по всем страницам списка: иначе хук не с
// первой страницы задублировался бы со старым секретом. ok=false — прав на
// хуки нет.
func (g *Gitea) findHook(ctx context.Context, repo, hookURL string) (id int64, found, ok bool, err error) {
	seen := 0
	for page := 1; page <= 50; page++ {
		raw, code, hdr, err := g.do(ctx, http.MethodGet,
			fmt.Sprintf("%s/hooks?limit=%d&page=%d", giteaRepo(repo), giteaHooksPage, page), nil, "")
		if err != nil {
			return 0, false, false, err
		}
		switch code {
		case http.StatusOK:
		case http.StatusForbidden, http.StatusNotFound:
			return 0, false, false, nil
		default:
			return 0, false, false, giteaErr("hooks", code, raw)
		}
		var hooks []struct {
			ID     int64 `json:"id"`
			Config struct {
				URL string `json:"url"`
			} `json:"config"`
		}
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return 0, false, false, err
		}
		for _, h := range hooks {
			if h.Config.URL == hookURL {
				return h.ID, true, true, nil
			}
		}
		// Конец списка — пустая страница: предел страницы у инстанса может
		// быть меньше запрошенного (MAX_RESPONSE_ITEMS), короткая страница
		// ещё не последняя.
		seen += len(hooks)
		total, terr := strconv.Atoi(hdr.Get("X-Total-Count"))
		if len(hooks) == 0 || (terr == nil && seen >= total) {
			break
		}
	}
	return 0, false, true, nil
}

// ─── пайплайны доставки (спека deployment) ───────────────────────────────

// TriggerPipeline запускает workflow Gitea Actions через workflow_dispatch.
// Идентификатор прогона ответ не несёт: его найдёт PipelineRun.
func (g *Gitea) TriggerPipeline(ctx context.Context, repo, pipeline, ref string, vars map[string]string) (PipelineRun, error) {
	if pipeline == "" {
		return PipelineRun{}, fmt.Errorf("gitea: нужен файл workflow (например deploy.yml)")
	}
	inputs := map[string]string{}
	for k, v := range vars {
		inputs[k] = v
	}
	raw, code, _, err := g.do(ctx, http.MethodPost,
		fmt.Sprintf("%s/actions/workflows/%s/dispatches", giteaRepo(repo), neturl.PathEscape(pipeline)),
		map[string]any{"ref": ref, "inputs": inputs}, "")
	if err != nil {
		return PipelineRun{}, err
	}
	if code != http.StatusNoContent && code != http.StatusCreated && code != http.StatusOK {
		return PipelineRun{}, giteaErr("workflow_dispatch", code, raw)
	}
	return PipelineRun{State: PipelineStarting}, nil
}

// PipelineRun — состояние прогона по API Actions, совместимому с GitHub:
// по идентификатору или, пока он неизвестен, свежий прогон workflow на
// ветке, начавшийся не раньше since.
func (g *Gitea) PipelineRun(ctx context.Context, repo, pipeline, ref, runID string, since time.Time) (PipelineRun, error) {
	if runID != "" {
		raw, code, _, err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/actions/runs/%s", giteaRepo(repo), neturl.PathEscape(runID)), nil, "")
		if err != nil {
			return PipelineRun{}, err
		}
		if code != http.StatusOK {
			return PipelineRun{}, giteaErr("run", code, raw)
		}
		var run githubRun
		if err := json.Unmarshal(raw, &run); err != nil {
			return PipelineRun{}, err
		}
		return run.pipeline(), nil
	}
	q := neturl.Values{"event": {"workflow_dispatch"}, "limit": {"10"}}
	if ref != "" {
		q.Set("branch", ref)
	}
	raw, code, _, err := g.do(ctx, http.MethodGet,
		fmt.Sprintf("%s/actions/workflows/%s/runs?%s", giteaRepo(repo), neturl.PathEscape(pipeline), q.Encode()), nil, "")
	if err != nil {
		return PipelineRun{}, err
	}
	if code != http.StatusOK {
		return PipelineRun{}, giteaErr("runs", code, raw)
	}
	var out struct {
		WorkflowRuns []githubRun `json:"workflow_runs"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return PipelineRun{}, err
	}
	// Как у GitHub: прогоны от свежих к старым, чужой прошлый прогон того
	// же workflow на той же ветке в публикацию не попадает.
	for _, run := range out.WorkflowRuns {
		if !run.CreatedAt.IsZero() && run.CreatedAt.Before(since.Add(-15*time.Second)) {
			continue
		}
		return run.pipeline(), nil
	}
	return PipelineRun{State: PipelineStarting}, nil
}

// ─── файлы репозитория (спека scm-integration) ───────────────────────────

// giteaContent — запись contents API.
type giteaContent struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
	SHA      string `json:"sha"`
	Size     int64  `json:"size"`
	Path     string `json:"path"`
	Name     string `json:"name"`
}

// ReadFile читает файл через contents API: содержимое в base64, sha блоба
// нужен для атомарной записи.
func (g *Gitea) ReadFile(ctx context.Context, repo, ref, filePath string) (FileContent, error) {
	u := giteaRepo(repo) + "/contents/" + escapePath(strings.TrimPrefix(filePath, "/"))
	if ref != "" {
		u += "?ref=" + neturl.QueryEscape(ref)
	}
	raw, code, _, err := g.do(ctx, http.MethodGet, u, nil, "")
	if err != nil {
		return FileContent{}, err
	}
	if code == http.StatusNotFound {
		if id, rel, ok := giteaMissing(raw); ok && id == "" && rel != "" {
			return FileContent{}, ErrFileNotFound
		}
		// Нет версии или самого репозитория: как у GitLab, это не
		// «нет файла».
		return FileContent{}, fmt.Errorf("gitea contents %s@%s: %w: %s", filePath, ref, ErrRefNotFound, clip(raw))
	}
	if code != http.StatusOK {
		return FileContent{}, giteaErr("contents", code, raw)
	}
	if t := bytes.TrimLeft(raw, " \t\r\n"); len(t) > 0 && t[0] == '[' {
		return FileContent{}, fmt.Errorf("gitea contents: %s: каталог, а не файл", filePath)
	}
	var out giteaContent
	if err := json.Unmarshal(raw, &out); err != nil {
		return FileContent{}, err
	}
	switch out.Type {
	case "file":
	case "symlink":
		return FileContent{}, fmt.Errorf("gitea contents %s: %w", filePath, ErrSymlink)
	case "dir":
		return FileContent{}, fmt.Errorf("gitea contents: %s: каталог, а не файл", filePath)
	default:
		return FileContent{}, fmt.Errorf("gitea contents: %s: не файл (%s)", filePath, out.Type)
	}
	if out.Path != "" && out.Path != strings.TrimPrefix(filePath, "/") {
		return FileContent{}, fmt.Errorf("gitea contents %s → %s: %w", filePath, out.Path, ErrSymlink)
	}
	content := out.Content
	if content == "" && out.Size > 0 {
		// Большой файл contents API отдаёт без содержимого: блоб читается
		// по его sha, чтобы содержимое не разошлось с FileID.
		if out.Size > MaxResponseBytes {
			return FileContent{}, fmt.Errorf("gitea contents: %s: %d байт, больше лимита чтения %d", filePath, out.Size, MaxResponseBytes)
		}
		if content, err = g.blob(ctx, repo, out.SHA); err != nil {
			return FileContent{}, err
		}
		out.Encoding = "base64"
	}
	if out.Encoding == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(content, "\n", ""))
		if err != nil {
			return FileContent{}, fmt.Errorf("gitea contents: %w", err)
		}
		content = string(decoded)
	}
	if out.Size > 0 && int64(len(content)) != out.Size {
		return FileContent{}, fmt.Errorf("gitea contents: %s: прочитано %d байт из %d", filePath, len(content), out.Size)
	}
	return FileContent{Content: content, FileID: out.SHA}, nil
}

// blob — содержимое блоба (base64) по sha.
func (g *Gitea) blob(ctx context.Context, repo, sha string) (string, error) {
	raw, code, _, err := g.do(ctx, http.MethodGet, giteaRepo(repo)+"/git/blobs/"+neturl.PathEscape(sha), nil, "")
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", giteaErr("blob", code, raw)
	}
	if len(raw) >= MaxResponseBytes {
		return "", fmt.Errorf("gitea blob %s: ответ больше лимита чтения %d байт", sha, MaxResponseBytes)
	}
	var out struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.Content, nil
}

// WriteFile сохраняет файл коммитом: новый — POST, существующий — PUT с sha
// прочитанной версии (конфликт параллельной записи виден сразу).
func (g *Gitea) WriteFile(ctx context.Context, repo, ref, filePath, prevID, content, message string) (Commit, error) {
	body := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString([]byte(content)),
	}
	if ref != "" {
		body["branch"] = ref
	}
	method := http.MethodPost
	if prevID != "" {
		method, body["sha"] = http.MethodPut, prevID
	}
	raw, code, _, err := g.do(ctx, method, giteaRepo(repo)+"/contents/"+escapePath(strings.TrimPrefix(filePath, "/")), body, "")
	if err != nil {
		return Commit{}, err
	}
	if code != http.StatusOK && code != http.StatusCreated {
		return Commit{}, giteaErr("contents write", code, raw)
	}
	var out struct {
		Commit struct {
			SHA     string `json:"sha"`
			HTMLURL string `json:"html_url"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Commit{}, err
	}
	return Commit{SHA: out.Commit.SHA, URL: out.Commit.HTMLURL}, nil
}

// BranchProtected — защищена ли ветка так, что изменения в неё проходят
// только через одобренный PR (спека access-policy: без этого git-провайдер
// политики не включается). У Gitea одобрения действуют только на merge
// PR, а прямой push регулирует отдельный флаг, поэтому ветка защищена,
// если правило требует одобрений и запрещает push: совсем
// (enable_push: false) или всем, кроме белого списка, в котором нет
// бот-идентичности. Gitea применяет к ветке одно правило из подходящих,
// и порядок выбора у версий разный (точное имя, шаблон, priority), поэтому
// условие должно выполняться для каждого подходящего правила. Белый список
// команд проверить нельзя — такое правило защитой не считается. Любая
// неоднозначность — «не защищена» (fail-closed). Нет прав — false без
// ошибки.
func (g *Gitea) BranchProtected(ctx context.Context, repo, branch string) (bool, error) {
	raw, code, _, err := g.do(ctx, http.MethodGet, giteaRepo(repo)+"/branch_protections", nil, "")
	if err != nil {
		return false, err
	}
	switch code {
	case http.StatusNotFound, http.StatusForbidden:
		return false, nil
	case http.StatusOK:
	default:
		return false, giteaErr("branch protections", code, raw)
	}
	var rules []struct {
		RuleName                string   `json:"rule_name"`
		BranchName              string   `json:"branch_name"`
		RequiredApprovals       int      `json:"required_approvals"`
		EnablePush              bool     `json:"enable_push"`
		EnablePushWhitelist     bool     `json:"enable_push_whitelist"`
		PushWhitelistUsernames  []string `json:"push_whitelist_usernames"`
		PushWhitelistTeams      []string `json:"push_whitelist_teams"`
		UnprotectedFilePatterns string   `json:"unprotected_file_patterns"`
	}
	if err := json.Unmarshal(raw, &rules); err != nil {
		return false, err
	}
	matched, login := 0, ""
	for _, r := range rules {
		name := r.RuleName
		if name == "" {
			name = r.BranchName // до появления шаблонов правило называлось веткой
		}
		if !giteaRuleMatches(name, branch) {
			continue
		}
		matched++
		if r.RequiredApprovals <= 0 {
			return false, nil
		}
		// Файлы из unprotected_file_patterns меняются прямым push в обход
		// запрета: путь политики мог попасть туда.
		if strings.TrimSpace(r.UnprotectedFilePatterns) != "" {
			return false, nil
		}
		if !r.EnablePush {
			continue
		}
		if !r.EnablePushWhitelist || len(r.PushWhitelistTeams) > 0 {
			return false, nil
		}
		if login == "" {
			if login, err = g.login(ctx); err != nil {
				return false, err
			}
		}
		for _, u := range r.PushWhitelistUsernames {
			if strings.EqualFold(u, login) {
				return false, nil
			}
		}
	}
	return matched > 0, nil
}

// login — логин владельца токена (бот-идентичность).
func (g *Gitea) login(ctx context.Context) (string, error) {
	raw, code, _, err := g.do(ctx, http.MethodGet, "/user", nil, "")
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", giteaErr("user", code, raw)
	}
	var me struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(raw, &me); err != nil || me.Login == "" {
		return "", fmt.Errorf("gitea user: неожиданный ответ")
	}
	return me.Login, nil
}

// giteaRuleMatches — правило защиты относится к ветке так, как его видит
// Gitea: имя без шаблона — без учёта регистра, шаблон — glob, где * не
// переходит через /. Синтаксис gobwas/glob, которого path.Match не знает
// ({a,b}, \, **, [!…]), считается подходящим к любой ветке: в сомнении
// правило участвует в решении, и мягкое такое правило даёт «не защищена».
func giteaRuleMatches(rule, branch string) bool {
	if !strings.ContainsAny(rule, "*?[{}\\") {
		return strings.EqualFold(rule, branch)
	}
	if strings.ContainsAny(rule, "{}\\") || strings.Contains(rule, "**") || strings.Contains(rule, "[!") {
		return true
	}
	// Шаблон тоже без учёта регистра: лишнее совпадение здесь — ошибка в
	// безопасную сторону.
	ok, err := path.Match(strings.ToLower(rule), strings.ToLower(branch))
	return err != nil || ok
}

// ─── файлы PR и дерево (add-architecture-c4) ─────────────────────────────

// giteaFilesPage — сколько файлов PR просить за страницу: больше 50 Gitea
// по умолчанию не отдаёт (MAX_RESPONSE_ITEMS).
const giteaFilesPage = 50

// PRFiles — файлы PR с одним повтором при расхождении счётчика.
func (g *Gitea) PRFiles(ctx context.Context, repo string, number int) (PRFileList, error) {
	return retryPRFiles(ctx, func() (PRFileList, error) { return g.prFilesOnce(ctx, repo, number) })
}

// prFilesOnce — файлы PR постранично; число записей сверяется с
// changed_files PR, как у GitHub. Конец списка — X-HasMore: false или
// короткая страница (у инстанса может быть свой предел страницы).
func (g *Gitea) prFilesOnce(ctx context.Context, repo string, number int) (PRFileList, error) {
	pr, err := g.pull(ctx, repo, number)
	if err != nil {
		return PRFileList{}, err
	}
	var paths []string
	entries := 0
	for page := 1; page <= prFilesMaxPages; page++ {
		raw, code, hdr, err := g.do(ctx, http.MethodGet,
			fmt.Sprintf("%s/pulls/%d/files?limit=%d&page=%d", giteaRepo(repo), number, giteaFilesPage, page), nil, "")
		if err != nil {
			return PRFileList{}, err
		}
		if code != http.StatusOK {
			return PRFileList{}, giteaErr("PR files", code, raw)
		}
		var files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		}
		if err := json.Unmarshal(raw, &files); err != nil {
			return PRFileList{}, err
		}
		entries += len(files)
		for _, f := range files {
			paths = append(paths, f.Filename, f.PreviousFilename)
		}
		more := strings.EqualFold(hdr.Get("X-HasMore"), "true")
		if hdr.Get("X-HasMore") == "" {
			more = len(files) == giteaFilesPage
		}
		if !more || len(files) == 0 {
			if entries != pr.ChangedFiles {
				return PRFileList{}, fmt.Errorf("gitea PR #%d: %w: файлов %d из %d", number, errCountMismatch, entries, pr.ChangedFiles)
			}
			return PRFileList{Files: sortedUnique(paths), HeadSHA: pr.Head.SHA}, nil
		}
	}
	return PRFileList{}, fmt.Errorf("gitea PR #%d: %w", number, ErrPRFilesTruncated)
}

// PathSHA — sha записи-каталога в содержимом родительского каталога.
func (g *Gitea) PathSHA(ctx context.Context, repo, ref, dir string) (string, error) {
	parent, name := splitDir(dir)
	u := giteaRepo(repo) + "/contents"
	if parent != "." {
		u += "/" + escapePath(parent)
	}
	u += "?ref=" + neturl.QueryEscape(ref)
	raw, code, _, err := g.do(ctx, http.MethodGet, u, nil, "")
	if err != nil {
		return "", err
	}
	if code == http.StatusNotFound {
		return "", nil
	}
	if code != http.StatusOK {
		return "", giteaErr("contents "+parent, code, raw)
	}
	var entries []giteaContent
	if err := json.Unmarshal(raw, &entries); err != nil {
		return "", nil // родитель — файл, а не каталог: каталога описания нет
	}
	for _, e := range entries {
		if e.Name == name && e.Type == "dir" {
			return e.SHA, nil
		}
	}
	return "", nil
}
