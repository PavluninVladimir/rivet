package scm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Разбор URL репозитория (design add-repo-onboarding, решение 3):
// из адреса извлекаются корень инстанса и путь репозитория, провайдер по
// хосту угадывается только для известных облачных инстансов (github.com,
// gitlab.com, gitea.com, codeberg.org) — по URL self-hosted GitLab и Gitea
// неотличимы, и молча угадать значит подключить проект к чужому API.

// Provider — тип хостинга.
type Provider string

const (
	ProviderGitHub Provider = "github"
	ProviderGitLab Provider = "gitlab"
	// ProviderGitea — Gitea и Forgejo: API у них общий (add-gitea-adapter).
	ProviderGitea Provider = "gitea"
	// ProviderFake — провайдер e2e-стендов (RIVET_SCM=fake).
	ProviderFake Provider = "fake"
)

// ValidProvider — известен ли провайдер.
func ValidProvider(p string) bool {
	switch Provider(p) {
	case ProviderGitHub, ProviderGitLab, ProviderGitea, ProviderFake:
		return true
	}
	return false
}

// giteaPathSegments — сегменты пути адреса Gitea, которые нужны поиску
// инстанса: owner/name за самым глубоким проверяемым префиксом, то есть
// первые giteaLocatePrefixes+1. Остальное — хвост страницы (ветка вида
// feature/x/y и путь файла), он не разбирается и длину адреса не
// ограничивает. В нужных сегментах пустые, «.» и «..» отклоняются.
func giteaPathSegments(path string) ([]string, error) {
	need := giteaLocatePrefixes + 1
	segments := strings.SplitN(strings.Trim(path, "/"), "/", need+1)
	if len(segments) > need {
		segments = segments[:need]
	}
	for _, s := range segments {
		if s == "" || s == "." || s == ".." {
			return nil, errors.New("путь репозитория Gitea должен быть вида owner/name")
		}
	}
	if len(segments) < 2 {
		return nil, errors.New("путь репозитория Gitea должен быть вида owner/name")
	}
	return segments, nil
}

// Поиск подкаталога инстанса: сколько префиксов пути проверяется (реальный
// подкаталог — один, редко два сегмента), сколько ждать одного ответа и
// весь поиск. Переменные для тестов.
var (
	giteaLocatePrefixes = 4
	giteaLocateTimeout  = 5 * time.Second
	giteaLocateTotal    = 10 * time.Second
)

// giteaSigninOnly — ответ Gitea на API без токена при REQUIRE_SIGNIN_VIEW.
const giteaSigninOnly = "Only signed in user is allowed to call APIs."

// GiteaNotLocated — пояснение к отказу проверки подключения, когда инстанс
// по адресу не опознан и взят запасной вариант: дело может быть не в
// токене или репозитории, а в поиске инстанса.
const GiteaNotLocated = "Gitea по этому адресу не опознан: если инстанс в подкаталоге, проверьте адрес и доступ без токена к /api/v1/version"

// LocateGitea находит в адресе репозитория Gitea корень инстанса и
// owner/name. Подкаталог (ROOT_URL за прокси под путём) по словам адреса не
// угадать: и разделов страницы много, и репозиторий может называться wiki
// или src. Поэтому инстанс спрашивается сам: от корня вглубь (не больше
// giteaLocatePrefixes префиксов, весь поиск не дольше giteaLocateTotal)
// берётся первый префикс пути, где без токена и без редиректов отвечает
// /api/v1/version. Два сегмента после него — owner/name, остальное — хвост
// страницы. Никто не опознан (не Gitea, нет ответа в срок, API закрыт
// прокси) — корень хоста, owner/name — первые два сегмента и found=false:
// проверка подключения назовёт причину и добавит GiteaNotLocated.
func LocateGitea(ctx context.Context, host, path string) (baseURL, repo string, found bool, err error) {
	segments, err := giteaPathSegments(path)
	if err != nil {
		return "", "", false, err
	}
	host = strings.TrimSuffix(host, "/")
	lctx, cancel := context.WithTimeout(ctx, giteaLocateTotal)
	defer cancel()
	for k := 0; k+2 <= len(segments) && k < giteaLocatePrefixes; k++ {
		cand := host
		if k > 0 {
			cand += "/" + strings.Join(segments[:k], "/")
		}
		if giteaAnswers(lctx, cand) {
			repo, err := GiteaRepoPath(segments[k] + "/" + segments[k+1])
			return cand, repo, err == nil, err
		}
		if lctx.Err() != nil {
			break
		}
	}
	repo, err = GiteaRepoPath(segments[0] + "/" + segments[1])
	return host, repo, false, err
}

// giteaLocateClient — без редиректов: признаётся только ответ самого
// кандидата, а не чужого хоста, куда он перенаправил.
var giteaLocateClient = &http.Client{
	Timeout:       30 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// giteaAnswers — по адресу отвечает API Gitea (или Forgejo): 200 с объектом
// ровно из одного ключа version или 403 «только для вошедших»
// (REQUIRE_SIGNIN_VIEW). Ключи сверяются точно: encoding/json
// сопоставлял бы их без учёта регистра.
func giteaAnswers(ctx context.Context, base string) bool {
	cctx, cancel := context.WithTimeout(ctx, giteaLocateTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, base+"/api/v1/version", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Accept", "application/json")
	resp, err := giteaLocateClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var obj map[string]json.RawMessage
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&obj) != nil {
		return false
	}
	switch resp.StatusCode {
	case http.StatusOK:
		var v string
		raw, ok := obj["version"]
		return ok && len(obj) == 1 && json.Unmarshal(raw, &v) == nil && v != ""
	case http.StatusForbidden:
		var msg string
		raw, ok := obj["message"]
		return ok && json.Unmarshal(raw, &msg) == nil && msg == giteaSigninOnly
	}
	return false
}

// NormalizeInstanceURL — корень инстанса с подкаталогом (Gitea за
// прокси под путём): схема и хост в нижнем регистре, путь без завершающего
// слэша.
func NormalizeInstanceURL(raw string) (string, error) {
	base, err := NormalizeBaseURL(raw)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(strings.TrimSpace(raw))
	if p := strings.Trim(u.Path, "/"); p != "" {
		for _, s := range strings.Split(p, "/") {
			if s == "" || s == "." || s == ".." {
				return "", errors.New("URL инстанса: недопустимый путь")
			}
		}
		base += "/" + p
	}
	return base, nil
}

// GiteaRepoPath — путь репозитория Gitea: ровно owner/name, вложенных
// групп нет. Хвост адреса страницы (/src/branch/main, /pulls/3) отрезается.
func GiteaRepoPath(path string) (string, error) {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) < 2 {
		return "", errors.New("путь репозитория Gitea должен быть вида owner/name")
	}
	owner, name := segments[0], strings.TrimSuffix(segments[1], ".git")
	if owner == "" || name == "" {
		return "", errors.New("путь репозитория Gitea должен быть вида owner/name")
	}
	return owner + "/" + name, nil
}

// RepoRef — разобранный адрес репозитория.
type RepoRef struct {
	Provider Provider // пусто, если по хосту не определяется
	BaseURL  string   // корень инстанса, например https://gitlab.example.com
	Path     string   // owner/name; у GitLab возможны вложенные группы
}

// WebURL — адрес репозитория на хостинге.
func (r RepoRef) WebURL() string { return r.BaseURL + "/" + r.Path }

var errEmptyURL = errors.New("пустой URL репозитория")

// ParseRepoURL разбирает URL репозитория в инстанс и путь. Схема — только
// http/https (http нужен инстансам во внутренней сети). URL с userinfo
// отклоняется: токен передаётся отдельным полем, а не внутри адреса.
func ParseRepoURL(raw string) (RepoRef, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return RepoRef{}, errEmptyURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return RepoRef{}, fmt.Errorf("не разобрать URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return RepoRef{}, errors.New("нужен полный URL со схемой https://")
	default:
		return RepoRef{}, fmt.Errorf("схема %q не поддерживается, нужен https", u.Scheme)
	}
	if u.User != nil {
		return RepoRef{}, errors.New("URL с логином или паролем не принимается: передайте токен отдельным полем")
	}
	if u.Host == "" {
		return RepoRef{}, errors.New("в URL нет хоста")
	}

	path := strings.Trim(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	// Хвосты вида /-/tree/main у GitLab и /tree/main у GitHub: репозиторий —
	// это всё до разделителя.
	if i := strings.Index(path, "/-/"); i >= 0 {
		path = path[:i]
	}
	segments := strings.Split(path, "/")
	for _, s := range segments {
		if s == "" {
			return RepoRef{}, errors.New("путь репозитория должен быть вида owner/name")
		}
	}
	if len(segments) < 2 {
		return RepoRef{}, errors.New("путь репозитория должен быть вида owner/name")
	}

	ref := RepoRef{
		BaseURL: strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host),
		Path:    strings.Join(segments, "/"),
	}
	switch strings.ToLower(u.Hostname()) {
	case "github.com", "www.github.com":
		ref.Provider = ProviderGitHub
	case "gitlab.com", "www.gitlab.com":
		ref.Provider = ProviderGitLab
	case "gitea.com", "codeberg.org":
		ref.Provider = ProviderGitea
	}
	if ref.Provider == ProviderGitea {
		if ref.Path, err = GiteaRepoPath(ref.Path); err != nil {
			return RepoRef{}, err
		}
	}
	return ref, nil
}

// NormalizeBaseURL приводит корень инстанса к каноничному виду (для режима
// «создать репозиторий», где пути ещё нет).
func NormalizeBaseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("нужен URL инстанса")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("не разобрать URL инстанса: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("URL инстанса: нужна схема https")
	}
	if u.User != nil {
		return "", errors.New("URL инстанса с логином или паролем не принимается")
	}
	if u.Host == "" {
		return "", errors.New("в URL инстанса нет хоста")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}
