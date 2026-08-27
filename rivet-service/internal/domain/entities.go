package domain

import (
	"encoding/json"
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
	// PolicySource — откуда берётся политика проекта: собственное
	// хранилище (правка из консоли) или файл в доверенной ветке
	// репозитория (спека access-policy «Хранение политик»).
	PolicySource string
	// PolicyFileID — версия файла политики, из которой создана последняя
	// версия политики проекта: по ней видно, что содержимое не менялось.
	PolicyFileID string
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
	// TokenBudget — бюджет Epic в токенах; nil — без бюджета (спека
	// orchestration «Бюджет Epic»). При превышении расхода планировщик
	// перестаёт назначать стадии задачам Epic.
	TokenBudget *int64
	// SourceKey — ключ источника у импортированной истории (спека
	// domain-model «Импорт истории проекта»); пусто у живых Epic'ов.
	// История не назначается runner'ам и не входит в оценки и бюджеты.
	SourceKey string
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
	// Процесс (спека backend/process): текущий шаг, вход на него (ok —
	// с начала, changes — исправление), снимок разрешённого процесса и хэш
	// версии политики, отказы по шагам, причина ожидания runner'а.
	StepID         string          `json:"Step"`
	StepEntry      string          `json:"StepEntry"`
	Process        json.RawMessage `json:"-"`
	ProcessHash    string          `json:"ProcessHash"`
	StepRejections map[string]int  `json:"StepAttempts"`
	WaitReason     string          `json:"WaitReason"`
	StepGen        int             `json:"-"`
	Created        time.Time
	Updated        time.Time
}

type Runner struct {
	ID    string
	Agent string
	// Model — модель по умолчанию (первая в Models); Models — все
	// поддерживаемые (спека runners «Регистрация runner'а», протокол v11).
	Model  string
	Models []string
	// Stages — стадии протокола, которые runner исполняет (add-process-editor);
	// пусто — четыре стадии без PROMPT.
	Stages       []string
	Host         string
	Capabilities []string
	Status       RunnerStatus
	TaskID       string
	CtxPct       *int // nil — заполненность контекста неизвестна
	Draining     bool
	LastSeen     time.Time
	// Adapter — способ подключения агента (claude-code, wrap); Depth —
	// глубина данных адаптера, сессии runner'а создаются с ней
	// (спека agent-integration «Уровни глубины данных»).
	Adapter string
	Depth   SessionDepth
	// Catalog — агент runner'а есть в каталоге профилей (add-agent-profiles):
	// модели и capabilities приходят из профиля. Secure — канал runner'а
	// защищён (TLS или loopback): секреты подключений можно доставлять.
	Catalog              bool
	Secure               bool
	ProfileName          string
	DeclaredModels       []string
	DeclaredCapabilities []string
	// Protocol — версия протокола runner'а при регистрации: полям v12
	// (окружение профиля) нет смысла ехать runner'у v11.
	Protocol string
	// ContextChannel — адаптер доводит контекст от Rivet до работающего
	// агента (спека agent-integration «Обратный канал контекста»);
	// без него система контекст этому runner'у не шлёт.
	ContextChannel bool
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
	// Files — затронутые сессией файлы: nil — недоступно для способа
	// подключения, пустой список — полная глубина без файлов.
	Files []string
	// Prompt — запрос сессии (для scheduler — снимок названия и описания
	// задачи, для user — промпт человека); Outcome — итог (результат стадии
	// или вопрос blocked); LastStep — текст последнего шага (team-visibility).
	Prompt   string
	Outcome  string
	LastStep string
	// Private — содержимое сессии видно только автору (DriverID); команда
	// видит факт (спека team-visibility «Видимость по умолчанию и приватность»).
	Private bool
	// PolicyHash — версия политики, доставленная стадии: итог стадии
	// привязан к ней независимо от того, чем стадия закончилась
	// (спека access-policy «Доставка политик runner'ам»).
	PolicyHash string
	// StepID и Participant — шаг процесса и участник, чью сессию это
	// (спека process); пусто у сессий до процесса.
	StepID      string
	Participant string
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
	ExecType  string // ssh | k8s | pipeline
	Trigger   string // auto | manual
	Config    EnvConfig
	Paused    bool
	// RunnerCaps — какие capability нужны runner'у публикации помимо
	// deploy: доступ к кластеру и к закрытому периметру даёт окружение
	// конкретных runner'ов (спека deployment «Собственная способность
	// деплоить»). Пусто — подойдёт любой deploy-runner.
	RunnerCaps []string
	Created    time.Time
}

// Типы исполнения окружения (спека deployment «Окружение как сущность»).
const (
	// ExecSSH — собственная доставка: команды на Linux-хосте по ssh
	// (пустой Host — локально на deploy-runner'е).
	ExecSSH = "ssh"
	// ExecPipeline — доставку выполняет пайплайн хостинга; Rivet его
	// триггерит, наблюдает и забирает результат как свой этап Deploy.
	ExecPipeline = "pipeline"
	// ExecK8s — собственная доставка в кластер: манифесты или helm-чарт.
	// Команды собирает control plane, исполняет deploy-runner.
	ExecK8s = "k8s"
	// ExecGitOps — версия меняется коммитом в репозиторий конфигурации,
	// выкат делает контроллер кластера; Rivet ждёт синхронизации.
	ExecGitOps = "gitops"
)

// EnvConfig — конфигурация исполнения окружения. Для ssh значимы Host и
// команды, для внешнего пайплайна — Pipeline, Ref и Vars.
type EnvConfig struct {
	Host      string `json:"host,omitempty"`
	DeployCmd string `json:"deploy_cmd,omitempty"`
	VerifyCmd string `json:"verify_cmd,omitempty"`
	VerifyURL string `json:"verify_url,omitempty"`
	// Pipeline — идентификатор пайплайна хостинга (файл workflow у GitHub
	// Actions; у GitLab CI необязателен), Ref — ветка запуска (пусто —
	// базовая ветка проекта), Vars — переменные прогона.
	Pipeline string            `json:"pipeline,omitempty"`
	Ref      string            `json:"ref,omitempty"`
	Vars     map[string]string `json:"vars,omitempty"`
	// Kubernetes: Namespace и либо каталог манифестов (Manifests) с
	// объектом для проверки готовности (Workload), либо чарт (Chart) с
	// релизом (Release) и значениями (Values).
	Namespace string            `json:"namespace,omitempty"`
	Manifests string            `json:"manifests,omitempty"`
	Workload  string            `json:"workload,omitempty"`
	Chart     string            `json:"chart,omitempty"`
	Release   string            `json:"release,omitempty"`
	Values    map[string]string `json:"values,omitempty"`
	// GitOps: репозиторий конфигурации (пусто — репозиторий проекта),
	// файл, куда пишется версия, и ключ YAML внутри него (пусто — файл
	// целиком). Ветка коммита берётся из Ref.
	Repo string `json:"repo,omitempty"`
	File string `json:"file,omitempty"`
	Key  string `json:"key,omitempty"`
}

// yamlKeyRe — путь ключа YAML: точки между сегментами, сегмент — буквы,
// цифры, дефис и подчёркивание.
var yamlKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// repoPathSlugRe — owner/name репозитория конфигурации.
var repoPathSlugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)+$`)

// k8sNameRe — имя объекта Kubernetes (RFC 1123): строчные буквы, цифры и
// дефис. Значение уезжает в команду deploy-runner'а, поэтому проверяется
// форматом, а не экранированием «на всякий случай».
var k8sNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// k8sWorkloadRe — объект для kubectl rollout status: тип/имя.
var k8sWorkloadRe = regexp.MustCompile(`^[a-z]+/[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)

// repoPathRe — путь внутри рабочей копии: без метасимволов shell,
// без ведущего дефиса и без выхода наверх.
var repoPathRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// envHostRe — [user@]hostname[:port]; ведущий «-» запрещён отдельно
// (аргумент ssh не должен читаться как опция).
var envHostRe = regexp.MustCompile(`^[A-Za-z0-9._-]+(@[A-Za-z0-9._-]+)?(:[0-9]{1,5})?$`)

// Validate — валидация конфигурации окружения (спека deployment «Окружение
// как сущность»): доставка и Verify обязательны, verify_url — только
// http/https без userinfo, host — безопасный аргумент ssh.
func (c EnvConfig) Validate(execType string) error {
	if execType == ExecGitOps {
		return c.validateGitOps()
	}
	if execType == ExecK8s {
		return c.validateK8s()
	}
	if execType == ExecPipeline {
		// Доставку выполняет хостинг: команды здесь исполнять негде, а
		// Verify делает control plane проверкой URL.
		if c.DeployCmd != "" || c.Host != "" {
			return errors.New("у окружения с внешним пайплайном нет команды доставки и хоста")
		}
		if strings.TrimSpace(c.VerifyCmd) != "" {
			return errors.New("verify_cmd недоступен для внешнего пайплайна: Verify — проверка verify_url")
		}
		if strings.TrimSpace(c.VerifyURL) == "" {
			return errors.New("нужен verify_url: этап Verify обязателен")
		}
		if strings.ContainsAny(c.Pipeline, " \t\n") {
			return errors.New("pipeline: ожидается идентификатор пайплайна хостинга")
		}
		if c.Ref != "" && (strings.HasPrefix(c.Ref, "-") || strings.ContainsAny(c.Ref, " \t\n")) {
			return errors.New("ref: ожидается имя ветки или тега")
		}
		for k := range c.Vars {
			if k == "" || strings.ContainsAny(k, " \t\n=") {
				return errors.New("vars: имя переменной без пробелов и «=»")
			}
			// RIVET_* задаёт система: версию и режим прогона решает Rivet,
			// а не конфигурация окружения.
			if strings.HasPrefix(k, "RIVET_") {
				return errors.New("vars: имена RIVET_* занимает система")
			}
		}
		return c.validateVerifyURL()
	}
	if strings.TrimSpace(c.DeployCmd) == "" {
		return errors.New("нужна команда доставки deploy_cmd")
	}
	if strings.TrimSpace(c.VerifyCmd) == "" && strings.TrimSpace(c.VerifyURL) == "" {
		return errors.New("нужен этап Verify: verify_cmd и/или verify_url")
	}
	if c.VerifyCmd != "" && strings.TrimSpace(c.VerifyCmd) == "" {
		return errors.New("verify_cmd: пустая команда")
	}
	if err := c.validateVerifyURL(); err != nil {
		return err
	}
	if c.Host != "" && (strings.HasPrefix(c.Host, "-") || !envHostRe.MatchString(c.Host)) {
		return errors.New("host: ожидается [user@]hostname[:port]")
	}
	return nil
}

// validateGitOps — конфигурация GitOps: коммит версии делает control
// plane, поэтому ни хоста, ни команд здесь нет, а ждать синхронизацию
// без адреса окружения нечем.
func (c EnvConfig) validateGitOps() error {
	if c.Host != "" || c.DeployCmd != "" {
		return errors.New("у окружения GitOps нет хоста и команды доставки: версия меняется коммитом")
	}
	if strings.TrimSpace(c.VerifyCmd) != "" {
		return errors.New("verify_cmd недоступен для GitOps: выполнять команду негде")
	}
	if strings.TrimSpace(c.VerifyURL) == "" {
		return errors.New("нужен verify_url: по нему видно, что окружение приняло версию")
	}
	if c.File == "" {
		return errors.New("нужен файл, в который пишется версия")
	}
	if err := validRepoPath(c.File, "file"); err != nil {
		return err
	}
	if c.Repo != "" && !repoPathSlugRe.MatchString(c.Repo) {
		return errors.New("repo: ожидается owner/name репозитория конфигурации")
	}
	if c.Ref != "" && (strings.HasPrefix(c.Ref, "-") || strings.ContainsAny(c.Ref, " \t\n")) {
		return errors.New("ref: ожидается имя ветки")
	}
	if c.Key != "" && !yamlKeyRe.MatchString(c.Key) {
		return errors.New("key: ожидается путь ключа YAML, например image.tag")
	}
	return c.validateVerifyURL()
}

// validateK8s — конфигурация кластера: либо манифесты, либо чарт; имена и
// пути проверяются форматом, потому что уезжают в команду runner'а.
func (c EnvConfig) validateK8s() error {
	if c.Host != "" {
		return errors.New("у окружения Kubernetes нет хоста: доступ к кластеру даёт окружение runner'а")
	}
	if c.DeployCmd != "" {
		// Команду доставки собирает система из параметров кластера: своя
		// команда обошла бы их проверку. Для произвольных команд есть тип
		// окружения «Linux-хост».
		return errors.New("у окружения Kubernetes нет собственной команды доставки: её собирает Rivet")
	}
	if (c.Manifests == "") == (c.Chart == "") {
		return errors.New("нужно ровно одно: каталог манифестов или helm-чарт")
	}
	if c.Namespace != "" && !k8sNameRe.MatchString(c.Namespace) {
		return errors.New("namespace: ожидается имя Kubernetes (строчные буквы, цифры, дефис)")
	}
	if c.Manifests != "" {
		if err := validRepoPath(c.Manifests, "manifests"); err != nil {
			return err
		}
		if c.Workload != "" && !k8sWorkloadRe.MatchString(c.Workload) {
			return errors.New("workload: ожидается тип/имя, например deployment/api")
		}
		// Verify обязателен: без объекта выката и без своей проверки
		// публикация считалась бы успешной сразу после apply.
		if c.Workload == "" && c.VerifyCmd == "" && c.VerifyURL == "" {
			return errors.New("нужен объект выката (workload) либо своя проверка: этап Verify обязателен")
		}
	}
	if c.Chart != "" {
		if err := validRepoPath(c.Chart, "chart"); err != nil {
			return err
		}
		if !k8sNameRe.MatchString(c.Release) {
			return errors.New("release: ожидается имя релиза helm (строчные буквы, цифры, дефис)")
		}
	}
	for k, v := range c.Values {
		if k == "" || strings.ContainsAny(k, " \t\n=") || strings.ContainsAny(v, " \t\n'\"`$;&|<>") {
			return errors.New("values: ключ без пробелов и «=», значение без подстановок shell")
		}
	}
	if c.VerifyCmd != "" && strings.TrimSpace(c.VerifyCmd) == "" {
		return errors.New("verify_cmd: пустая команда")
	}
	return c.validateVerifyURL()
}

// validRepoPath — путь внутри рабочей копии: без метасимволов, без выхода
// за корень и без ведущего дефиса (иначе команда прочитает его как опцию).
func validRepoPath(p, field string) error {
	if strings.HasPrefix(p, "-") || strings.HasPrefix(p, "/") || !repoPathRe.MatchString(p) {
		return errors.New(field + ": ожидается путь от корня репозитория без подстановок shell")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return errors.New(field + ": путь не должен выходить за корень репозитория")
		}
	}
	return nil
}

func (c EnvConfig) validateVerifyURL() error {
	if c.VerifyURL == "" {
		return nil
	}
	u, err := url.Parse(c.VerifyURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("verify_url: ожидается http(s)-URL")
	}
	if u.User != nil {
		return errors.New("verify_url: userinfo в URL запрещён")
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
	// ExternalRunID и ExternalURL — прогон внешнего пайплайна, если
	// доставку выполняет хостинг (спека deployment «Дирижирование
	// внешними системами доставки»).
	ExternalRunID string
	ExternalURL   string
	Created       time.Time
	Started       *time.Time
	Ended         *time.Time
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
	// AttPolicyEngine — движок политик не дал решения: автоматика проекта
	// остановлена (fail-closed, спека access-policy «Движок недоступен»).
	AttPolicyEngine AttentionReason = "POLICY_ENGINE"
	// AttPRClosed — PR задачи закрыт на хостинге без merge: решение
	// (повторить, отменить) за человеком (спека scm-integration).
	AttPRClosed AttentionReason = "PR_CLOSED"
	// AttPolicySource — политика проекта в репозитории не читается или не
	// проходит валидацию: действует последняя валидная версия
	// (спека access-policy «Битый файл политики»).
	AttPolicySource AttentionReason = "POLICY_SOURCE"
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

// ModelEntry — модель подключения (спека model-connections «Список моделей
// подключения»): обнаруженная у провайдера или заданная вручную. Цены в
// микродолларах за миллион токенов.
type ModelEntry struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	InputPrice    *int64 `json:"input_price,omitempty"`
	OutputPrice   *int64 `json:"output_price,omitempty"`
	ContextWindow *int64 `json:"context_window,omitempty"`
	Source        string `json:"source"` // discovered | manual
	Hidden        bool   `json:"hidden"`
	Missing       bool   `json:"missing"`
}

// ConnHeader — дополнительный HTTP-заголовок подключения; у секретного
// значение наружу не отдаётся.
type ConnHeader struct {
	Name   string `json:"name"`
	Value  string `json:"value,omitempty"`
	Secret bool   `json:"secret"`
}

// ModelConnection — подключение к провайдеру, агрегатору или локальному
// серверу моделей (спека backend/model-connections). Ключ наружу — префиксом.
type ModelConnection struct {
	ID          string
	Name        string
	Kind        string // vendor | aggregator | local
	API         string // anthropic | openai
	BaseURL     string
	KeyPrefix   string
	HasKey      bool
	Headers     []ConnHeader
	Models      []ModelEntry
	Enabled     bool
	State       LLMProviderState
	CheckDetail string
	CheckedAt   *time.Time
	UpdatedAt   time.Time
	UpdatedBy   string // логин администратора
}

// AgentModelRef — привязка модели к агенту: подключение и модель из его списка.
type AgentModelRef struct {
	ConnectionID string `json:"connection_id"`
	Model        string `json:"model"`
	Unavailable  bool   `json:"unavailable,omitempty"`
}

// EnvVar — переменная окружения шаблона агента с подстановками.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// AgentProfile — профиль агента (спека backend/agents «Каталог агентов»).
type AgentProfile struct {
	ID           string
	Name         string
	Adapter      string // claude-code | wrap
	Command      string
	Capabilities []string
	Models       []AgentModelRef
	DefaultModel *AgentModelRef
	Env          []EnvVar
	Args         []string
	Secrets      string // never | secure | always
	Enabled      bool
	Preset       bool
	Runners      int
	UpdatedAt    time.Time
	UpdatedBy    string
}

// PlannerModel — выбранная модель декомпозиции: подключение и модель.
type PlannerModel struct {
	ConnectionID string `json:"connection_id"`
	Model        string `json:"model"`
}
