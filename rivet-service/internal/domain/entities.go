package domain

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Project — проект с подключённым репозиторием (спека domain-model
// «Репозиторий проекта»): провайдера и инстанс хранит сам проект, а не
// конфигурация установки.
type Project struct {
	ID      string
	Name    string
	Checks  []Check
	Created time.Time

	Provider      string // github | gitlab | fake
	BaseURL       string // корень инстанса, например https://github.com
	RepoPath      string // owner/name; у GitLab возможны вложенные группы
	DefaultBranch string
	// CredentialID — учётные данные проекта; пусто у проектов, работающих
	// на глобальном токене установки (созданы до add-repo-onboarding).
	CredentialID  string
	WebhookSecret string
	// WebhookRegistered — подписку на события создала система (иначе её
	// настраивают на хостинге руками).
	WebhookRegistered bool
}

// Repo — путь репозитория. Метод сохраняет привычное имя для мест, где
// нужен только owner/name (промпты планировщика, сверка webhook).
func (p Project) Repo() string { return p.RepoPath }

// WebURL — адрес репозитория на хостинге.
func (p Project) WebURL() string {
	if p.BaseURL == "" || p.RepoPath == "" {
		return ""
	}
	return p.BaseURL + "/" + p.RepoPath
}

// ScmCredential — учётные данные хостинга. Секрет наружу не отдаётся:
// только владелец и префикс (спека scm-integration «Учётные данные хостинга»).
type ScmCredential struct {
	ID          string
	Provider    string
	BaseURL     string
	Owner       string
	TokenPrefix string
	State       string // ok | invalid | unchecked
	CheckedAt   *time.Time
	Created     time.Time
}

// Check — команда этапа testing из конфига проекта.
type Check struct {
	Name string `json:"name"`
	Cmd  string `json:"cmd"`
}

type Epic struct {
	ID        string
	ProjectID string
	Title     string
	Goal      string
	Status    EpicStatus
	Created   time.Time
}

type Criterion struct {
	Text string `json:"text"`
	OK   bool   `json:"ok"`
}

type Task struct {
	ID           string
	EpicID       string
	Num          int64 // ветка agent/task-<Num>
	Title        string
	Description  string
	Status       TaskStatus
	Estimate     int
	Capabilities []string
	Criteria     []Criterion
	Deps         []string
	AttemptUsed  int
	AttemptLimit int
	// ReviewRejections — отказы review с последнего решения человека;
	// лимит из политики проекта (спека orchestration «Лимит попыток»).
	ReviewRejections int
	RunnerID         string
	Branch           string
	PRURL            string
	BlockReason      string
	BlockedBy        string // id задачи-первопричины при каскадной блокировке
	Created          time.Time
	Updated          time.Time
}

type Runner struct {
	ID           string
	Agent        string
	Model        string
	Host         string
	Capabilities []string
	Status       RunnerStatus
	TaskID       string
	CtxPct       *int // nil — заполненность контекста неизвестна
	Draining     bool
	LastSeen     time.Time
}

// HasCapabilities — подбор по требованиям задачи (спека orchestration).
func (r Runner) HasCapabilities(required []string) bool {
	have := map[string]bool{}
	for _, c := range r.Capabilities {
		have[c] = true
	}
	for _, c := range required {
		if !have[c] {
			return false
		}
	}
	return true
}

type SessionDepth string

const (
	DepthFull    SessionDepth = "full"
	DepthPartial SessionDepth = "partial"
	DepthMinimal SessionDepth = "minimal"
)

type Session struct {
	ID            string
	TaskID        string
	Attempt       int
	DriverKind    string // user | scheduler
	DriverID      string
	Agent         string
	Model         string
	Depth         SessionDepth
	Scope         string
	TranscriptRef string
	// Tokens — итог токенов сессии; nil = источник не сообщил (не ноль),
	// колонка nullable с миграции 0004 (спека observability «Учёт usage»).
	Tokens  *int64
	Started time.Time
	Ended   *time.Time
}

// Environment — окружение публикации проекта (спека backend/deployment).
// Config несёт несекретные параметры исполнения и Verify.
type Environment struct {
	ID        string
	ProjectID string
	Name      string
	ExecType  string // ssh
	Trigger   string // auto | manual
	Config    EnvConfig
	Paused    bool
	Created   time.Time
}

// EnvConfig — конфигурация исполнения ssh-окружения. Пустой Host —
// команды исполняются локально на deploy-runner'е (e2e, деплой «на себя»).
type EnvConfig struct {
	Host      string `json:"host,omitempty"`
	DeployCmd string `json:"deploy_cmd"`
	VerifyCmd string `json:"verify_cmd,omitempty"`
	VerifyURL string `json:"verify_url,omitempty"`
}

// envHostRe — [user@]hostname[:port]; ведущий «-» запрещён отдельно
// (аргумент ssh не должен читаться как опция).
var envHostRe = regexp.MustCompile(`^[A-Za-z0-9._-]+(@[A-Za-z0-9._-]+)?(:[0-9]{1,5})?$`)

// Validate — валидация конфигурации окружения (спека deployment «Окружение
// как сущность»): доставка и Verify обязательны, verify_url — только
// http/https без userinfo, host — безопасный аргумент ssh.
func (c EnvConfig) Validate() error {
	if strings.TrimSpace(c.DeployCmd) == "" {
		return errors.New("нужна команда доставки deploy_cmd")
	}
	if strings.TrimSpace(c.VerifyCmd) == "" && strings.TrimSpace(c.VerifyURL) == "" {
		return errors.New("нужен этап Verify: verify_cmd и/или verify_url")
	}
	if c.VerifyCmd != "" && strings.TrimSpace(c.VerifyCmd) == "" {
		return errors.New("verify_cmd: пустая команда")
	}
	if c.VerifyURL != "" {
		u, err := url.Parse(c.VerifyURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("verify_url: ожидается http(s)-URL")
		}
		if u.User != nil {
			return errors.New("verify_url: userinfo в URL запрещён")
		}
	}
	if c.Host != "" && (strings.HasPrefix(c.Host, "-") || !envHostRe.MatchString(c.Host)) {
		return errors.New("host: ожидается [user@]hostname[:port]")
	}
	return nil
}

// Deployment — публикация окружения. Created — постановка в очередь,
// Started — взята runner'ом, Ended — финал.
type Deployment struct {
	ID        string
	EnvID     string
	Version   string
	Status    string // queued | deploying | verifying | done | failed | rolled_back
	Initiator string // login участника или auto
	RunnerID  string
	Detail    string
	// Rollback — публикация в фазе отката к предыдущей версии
	// (durable: переживает рестарт control plane).
	Rollback bool
	LogRef   string
	Created  time.Time
	Started  *time.Time
	Ended    *time.Time
}

// loginRe — формат login: URL-safe (login живёт в путях API и event log).
var loginRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

// ValidLogin — допустим ли login: латиница/цифры/._-, до 64 символов.
func ValidLogin(login string) bool { return loginRe.MatchString(login) }

// MinPasswordLen — нижняя граница длины пароля на всех путях его задания
// (спека access-policy, design add-user-management).
const MinPasswordLen = 8

// User — учётная запись человека (спека domain-model «Пользователи и членство
// в проекте»). Login неизменяем: по нему атрибутируются события.
type User struct {
	ID       string
	Login    string
	Name     string
	Admin    bool
	Disabled bool
	Created  time.Time
	// MustChangePassword — пароль сброшен администратором: вход работает,
	// остальное API закрыто до смены пароля.
	MustChangePassword bool
}

// Роли участника проекта: owner меняет настройки проекта, member работает
// с задачами (спека domain-model «Пользователи и членство в проекте»).
const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

// ValidRole — допустима ли роль участника.
func ValidRole(role string) bool { return role == RoleOwner || role == RoleMember }

// Member — участник проекта.
type Member struct {
	Login string
	Name  string
	Role  string
	Added time.Time
}

// AccessToken — метаданные PAT; секрет существует только в момент создания.
type AccessToken struct {
	ID        string
	Name      string
	Prefix    string
	Created   time.Time
	ExpiresAt *time.Time
	LastUsed  *time.Time
}

type ActorKind string

const (
	ActorRunner    ActorKind = "runner"
	ActorScheduler ActorKind = "scheduler"
	ActorSystem    ActorKind = "system"
	ActorUser      ActorKind = "user"
)

type Event struct {
	ID        int64
	TS        time.Time
	ActorKind ActorKind
	ActorID   string
	Type      string
	ProjectID string
	EpicID    string
	TaskID    string
	Text      string
	// Payload — структурированные данные события (например, статус публикации
	// для deploy.status); nil, если событие их не несёт.
	Payload map[string]any
}

type AttentionReason string

const (
	AttBlocked      AttentionReason = "BLOCKED"
	AttReviewLimit  AttentionReason = "REVIEW_LIMIT"
	AttTestFailed   AttentionReason = "TEST_FAILED"
	AttRunnerLost   AttentionReason = "RUNNER_LOST"
	AttDeployFailed AttentionReason = "DEPLOY_FAILED"
	// AttPolicyChange — PR меняет файлы политики (.rivet/): авто-merge
	// заблокирован метаправилом, нужен человек (спека access-policy).
	AttPolicyChange AttentionReason = "POLICY_CHANGE"
)

type Attention struct {
	ID        string
	ProjectID string
	// Предмет эскалации: задача или публикация (ровно одно из двух,
	// CHECK attention_subject в 0006).
	TaskID       string
	DeploymentID string
	Reason       AttentionReason
	Message      string
	Status       string // open | claimed | resolved
	ClaimedBy    string
	Created      time.Time
}

// RunnerToken — токен регистрации runner'ов (спека runners «Токены
// регистрации runner'ов»): общий секрет установки, секрет существует
// только в момент создания.
type RunnerToken struct {
	ID        string
	Name      string
	Prefix    string
	CreatedBy string // логин администратора
	Created   time.Time
	ExpiresAt *time.Time
	LastUsed  *time.Time
	RevokedAt *time.Time
}

// LLMProviderState — результат последней проверки ключа у провайдера.
type LLMProviderState string

const (
	LLMStateOK        LLMProviderState = "ok"
	LLMStateInvalid   LLMProviderState = "invalid"
	LLMStateUnchecked LLMProviderState = "unchecked"
)

// LLMProvider — провайдер модели декомпозиции с ключом в базе (спека
// epic-decomposition «Настройка модели для декомпозиции»). Секрет наружу
// не отдаётся — только префикс.
type LLMProvider struct {
	Provider    string
	KeyPrefix   string
	Model       string
	Active      bool
	State       LLMProviderState
	CheckDetail string
	CheckedAt   *time.Time
	UpdatedAt   time.Time
	UpdatedBy   string // логин администратора
}
