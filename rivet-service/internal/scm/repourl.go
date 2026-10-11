package scm

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Разбор URL репозитория (design add-repo-onboarding, решение 3):
// из адреса извлекаются корень инстанса и путь репозитория, провайдер по
// хосту угадывается только для github.com и gitlab.com — по URL self-hosted
// GitLab и Gitea неотличимы, и молча угадать значит подключить проект
// к чужому API.

// Provider — тип хостинга.
type Provider string

const (
	ProviderGitHub Provider = "github"
	ProviderGitLab Provider = "gitlab"
	// ProviderFake — провайдер e2e-стендов (RIVET_SCM=fake).
	ProviderFake Provider = "fake"
)

// ValidProvider — известен ли провайдер.
func ValidProvider(p string) bool {
	switch Provider(p) {
	case ProviderGitHub, ProviderGitLab, ProviderFake:
		return true
	}
	return false
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
