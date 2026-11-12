// Package scm — единый интерфейс git-хостинга (спека backend/scm-integration).
// Конвейер не знает различий PR/MR — их прячет адаптер.
package scm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
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

// ErrFileNotFound — файла нет в репозитории на этой ветке.
var ErrFileNotFound = errors.New("файла нет в репозитории")

// ErrSymlink — по пути лежит символическая ссылка: содержимое цели не
// читается (снимок архитектуры пропускает такой файл с дефектом).
var ErrSymlink = errors.New("символическая ссылка")

// ErrRefNotFound — версии, из которой читается файл (ветки или коммита),
// нет на хостинге, или недоступен сам проект (GitLab отвечает на оба
// одинаково). Это не ErrFileNotFound: политика из репозитория при нём
// сообщает о недоступной доверенной ветке, а снимок не принимает его за
// отсутствие описания.
var ErrRefNotFound = errors.New("версии (ветки или коммита) нет в репозитории")

// ErrBranchNotFound — ветки нет в репозитории, в том числе в пустом
// репозитории без коммитов. Отличается от сбоя хостинга: снимок
// архитектуры по нему понимает, что описывать пока нечего.
var ErrBranchNotFound = errors.New("ветки нет в репозитории")

// ErrMergeConflict — хостинг отказал в merge из-за конфликта PR/MR с
// базовой веткой (спека scm-integration «Распознавание конфликта при
// merge»). Адаптер возвращает её, только когда причина установлена точно:
// защита ветки, обязательные проверки и неизвестный ответ — обычная ошибка.
var ErrMergeConflict = errors.New("PR конфликтует с базовой веткой")

// ErrUnauthorized — хостинг не принял учётные данные (401): токен отозван,
// просрочен или неверен (спека scm-integration «Учётные данные хостинга»).
var ErrUnauthorized = errors.New("хостинг не принимает учётные данные")

// ErrForbidden — хостинг отказал в доступе (403, не лимит запросов). Сам по
// себе отказ неоднозначен: у токена отобрали доступ к репозиторию или ему
// не хватает прав на одну операцию. Различает их проверка подключения.
var ErrForbidden = errors.New("хостинг отказал в доступе")

// ErrNotFound — хостинг не нашёл объект операции (404). На приватный
// репозиторий, к которому у токена отобрали доступ, GitHub отвечает именно
// так, поэтому 404 на операции без собственного смысла 404 сверяется с
// проверкой подключения. Операции, у которых 404 значит «нет ветки» или
// «нет файла», разбирают его сами и сюда не попадают.
var ErrNotFound = errors.New("хостинг не нашёл объект")

// AccessRefused — ответ хостинга мог означать, что учётные данные не
// работают (401, 403, 404). Решает проверка подключения.
func AccessRefused(err error) bool {
	return errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrForbidden) || errors.Is(err, ErrNotFound)
}

// hostError — неуспешный ответ хостинга. Текст прежний («github create PR:
// 403: …»), а отказ по авторизации узнаётся через errors.Is.
type hostError struct {
	msg  string
	kind error
}

func (e *hostError) Error() string { return e.msg }
func (e *hostError) Unwrap() error { return e.kind }

// statusErr — ошибка операции op по неуспешному коду ответа хостинга.
func statusErr(op string, code int, raw []byte) error {
	msg := fmt.Sprintf("%s: %d: %s", op, code, clip(raw))
	switch code {
	case http.StatusUnauthorized:
		return &hostError{msg: msg, kind: ErrUnauthorized}
	case http.StatusForbidden:
		return &hostError{msg: msg, kind: ErrForbidden}
	case http.StatusNotFound:
		return &hostError{msg: msg, kind: ErrNotFound}
	}
	return errors.New(msg)
}

// MaxResponseBytes — лимит чтения тела ответа хостинга.
const MaxResponseBytes = 4 << 20

// clip — хвост тела ответа для текста ошибки: целиком он не нужен, а
// многомегабайтная страница в сообщении бесполезна.
func clip(raw []byte) string {
	const n = 500
	if len(raw) <= n {
		return string(raw)
	}
	return string(raw[:n]) + "…"
}

type Adapter interface {
	// CreatePR создаёт pull request из ветки задачи в базовую ветку.
	CreatePR(ctx context.Context, repo, branch, base, title, body string) (PR, error)
	// Diff возвращает diff PR (контекст для review-агента).
	Diff(ctx context.Context, repo string, number int) (string, error)
	// Merge выполняет merge PR и возвращает sha merge-коммита
	// (версия для автопубликаций, спека backend/deployment).
	Merge(ctx context.Context, repo string, number int) (string, error)
	// HeadSHA — текущий HEAD ветки (версия для ручного запуска публикации,
	// снимок архитектуры проекта). ErrBranchNotFound — ветки нет.
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
	// TriggerPipeline запускает пайплайн доставки хостинга (спека
	// deployment «Дирижирование внешними системами доставки»): pipeline —
	// идентификатор пайплайна (файл workflow у GitHub Actions), ref —
	// ветка или тег запуска, vars — переменные прогона. Пустой RunID в
	// ответе означает «прогон ещё не виден»: его найдёт PipelineRun.
	TriggerPipeline(ctx context.Context, repo, pipeline, ref string, vars map[string]string) (PipelineRun, error)
	// PipelineRun — состояние прогона. Пустой runID — прогон ещё не
	// найден: адаптер ищет свежий прогон пайплайна на ветке, начавшийся
	// не раньше since.
	PipelineRun(ctx context.Context, repo, pipeline, ref, runID string, since time.Time) (PipelineRun, error)
	// ReadFile читает файл репозитория на ветке или коммите: ref — имя
	// ветки или SHA (спека scm-integration «Запись файла поверх разных
	// хостингов», снимок архитектуры по SHA). Возвращает содержимое и
	// идентификатор версии файла для атомарной записи. ErrFileNotFound —
	// файла нет, ErrRefNotFound — нет самой версии.
	ReadFile(ctx context.Context, repo, ref, path string) (FileContent, error)
	// WriteFile сохраняет файл коммитом на ветке; prevID — идентификатор
	// прочитанной версии (пусто для нового файла).
	WriteFile(ctx context.Context, repo, ref, path, prevID, content, message string) (Commit, error)
	// BranchProtected — защищена ли ветка правилами хостинга (спека
	// access-policy: без защиты доверенной ветки git-провайдер политики
	// не включается). Нет прав — false без ошибки.
	BranchProtected(ctx context.Context, repo, branch string) (bool, error)
}

// FileContent — файл репозитория: содержимое и идентификатор версии
// (sha блоба у GitHub, last_commit_id у GitLab) для атомарной записи.
type FileContent struct {
	Content string
	FileID  string
}

// Commit — созданный коммит: sha и адрес для человека.
type Commit struct {
	SHA string
	URL string
}

// Состояния прогона внешнего пайплайна.
const (
	PipelineStarting = "starting" // запущен, но прогон ещё не найден
	PipelineRunning  = "running"
	PipelineSuccess  = "success"
	PipelineFailed   = "failed"
)

// PipelineRun — прогон пайплайна доставки: идентификатор для опроса,
// адрес для человека и состояние.
type PipelineRun struct {
	RunID string
	URL   string
	State string
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
		// Fake стенда видит токен проекта: так отзыв токена отличается от
		// его замены (RIVET_FAKE_SCM_REVOKED).
		if fk, ok := f.Fallback.(*Fake); ok && token != "" {
			return fk.ForToken(token), nil
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
	case ProviderGitea:
		if baseURL == "" {
			return nil, errors.New("для Gitea нужен URL инстанса")
		}
		a = NewGitea(baseURL, token)
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

// branchMissing — 404 хостинга означает именно отсутствие ветки, а не
// недоступный репозиторий: GitHub отвечает «Branch not found», GitLab
// «404 Branch Not Found». Для репозитория оба пишут другое.
func branchMissing(raw []byte) bool {
	return strings.Contains(hostMessage(raw), "branch not found")
}

// refMissing — 404 на чтение файла означает отсутствие версии (ветки,
// коммита) или проекта, а не файла: GitHub пишет «No commit found for the
// ref …», GitLab «404 Commit Not Found» или «404 Project Not Found». Для
// отсутствующего файла оба пишут другое. Недоступный репозиторий GitHub
// отвечает так же, как на отсутствующий файл, и отличить его нельзя.
func refMissing(raw []byte) bool {
	msg := hostMessage(raw)
	for _, s := range []string{"no commit found", "commit not found", "project not found", "branch not found", "ref not found"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// hostMessage — поле message ответа хостинга в нижнем регистре.
func hostMessage(raw []byte) string {
	var out struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &out) != nil {
		return ""
	}
	return strings.ToLower(out.Message)
}

// probeFail — короткий конструктор отказа.
func probeFail(reason, msg string) ProbeResult {
	return ProbeResult{Reason: reason, Message: msg}
}
