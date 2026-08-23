// Package scm — единый интерфейс git-хостинга (спека backend/scm-integration).
// Конвейер не знает различий PR/MR — их прячет адаптер.
package scm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

type PR struct {
	Number int
	URL    string
}

// ProbeResult — итог проверки подключения репозитория (спека
// scm-integration «Подключение репозитория к проекту»). Причина отказа
// различается по смыслу: общий «сбой хостинга» пользователю бесполезен.
type ProbeResult struct {
	OK              bool
	Reason          string // пусто при OK; см. константы Reason*
	Message         string
	TokenOwner      string
	RepoPath        string
	DefaultBranch   string
	CanPush         bool
	CanMergeRequest bool
}

// Причины отказа проверки подключения.
const (
	ReasonNotFound    = "not_found"
	ReasonNoAccess    = "no_access"
	ReasonNoScope     = "insufficient_scope"
	ReasonUnreachable = "unreachable"
	ReasonBadToken    = "bad_token"
)

// NewRepo — параметры создания репозитория.
type NewRepo struct {
	Owner   string // личный аккаунт, организация или группа
	Name    string
	Private bool
}

// RepoInfo — созданный или найденный репозиторий.
type RepoInfo struct {
	Path          string
	WebURL        string
	DefaultBranch string
}

// ErrRepoExists — репозиторий с таким именем у владельца уже есть.
var ErrRepoExists = errors.New("репозиторий с таким именем уже существует")

// ErrDiffTruncated — diff PR не поместился в лимит чтения ответа хостинга:
// Diff возвращает начало вместе с этой ошибкой. Для контекста ревьюера
// начало годится, для решений политики по путям — нет (fail-closed).
var ErrDiffTruncated = errors.New("diff PR обрезан: превышен лимит чтения")

// MaxResponseBytes — лимит чтения тела ответа хостинга.
const MaxResponseBytes = 4 << 20

type Adapter interface {
	// CreatePR создаёт pull request из ветки задачи в базовую ветку.
	CreatePR(ctx context.Context, repo, branch, base, title, body string) (PR, error)
	// Diff возвращает diff PR (контекст для review-агента).
	Diff(ctx context.Context, repo string, number int) (string, error)
	// Merge выполняет merge PR и возвращает sha merge-коммита
	// (версия для автопубликаций, спека backend/deployment).
	Merge(ctx context.Context, repo string, number int) (string, error)
	// HeadSHA — текущий HEAD ветки (версия для ручного запуска публикации).
	HeadSHA(ctx context.Context, repo, branch string) (string, error)
	// Probe проверяет доступ к репозиторию и права до создания проекта;
	// пустой repo проверяет только сам токен (режим «создать новый»).
	Probe(ctx context.Context, repo string) ProbeResult
	// CreateRepo создаёт репозиторий с начальным коммитом и базовой веткой:
	// без них конвейер не сможет ни клонировать, ни ответвиться.
	CreateRepo(ctx context.Context, in NewRepo) (RepoInfo, error)
	// RegisterWebhook подписывает хостинг на события проекта. Возвращает
	// false без ошибки, если прав на подписку нет — подключение при этом
	// не блокируется, консоль покажет данные для ручной настройки.
	RegisterWebhook(ctx context.Context, repo, url, secret string) (bool, error)
}

// Factory создаёт адаптеры по проекту: провайдер и инстанс живут у проекта,
// а не в конфигурации установки (design add-repo-onboarding, решение 5).
type Factory struct {
	// Fallback — адаптер для проектов без собственных учётных данных
	// (глобальный токен установки, fake на e2e-стендах).
	Fallback Adapter
	// Force — в установке нет реальных хостингов (RIVET_SCM=fake): любые
	// учётные данные ведут на запасной адаптер, наружу не ходим.
	Force bool

	mu    sync.Mutex
	cache map[string]cachedAdapter
}

type cachedAdapter struct {
	adapter Adapter
	added   time.Time
}

// adapterTTL — отзыв учётных данных не должен ждать рестарта rivetd.
const adapterTTL = 5 * time.Minute

// For возвращает адаптер провайдера для инстанса и токена. Пустой токен —
// запасной адаптер установки (проекты, созданные до этого изменения).
func (f *Factory) For(provider Provider, baseURL, token string) (Adapter, error) {
	if f.Force || token == "" {
		if f.Fallback == nil {
			return nil, errors.New("у проекта нет учётных данных, а запасной адаптер не настроен")
		}
		return f.Fallback, nil
	}
	key := string(provider) + "|" + baseURL + "|" + token
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.cache[key]; ok && time.Since(c.added) < adapterTTL {
		return c.adapter, nil
	}
	var a Adapter
	switch provider {
	case ProviderGitHub:
		a = NewGitHubAt(baseURL, token)
	case ProviderGitLab:
		a = NewGitLab(baseURL, token)
	case ProviderFake:
		a = NewFake()
	default:
		return nil, fmt.Errorf("неизвестный провайдер %q", provider)
	}
	if f.cache == nil {
		f.cache = map[string]cachedAdapter{}
	}
	if len(f.cache) > 256 { // защита от неограниченного роста
		f.cache = map[string]cachedAdapter{}
	}
	f.cache[key] = cachedAdapter{adapter: a, added: time.Now()}
	return a, nil
}

// httpClient — общий клиент адаптеров с таймаутом.
func httpClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// probeFail — короткий конструктор отказа.
func probeFail(reason, msg string) ProbeResult {
	return ProbeResult{Reason: reason, Message: msg}
}
