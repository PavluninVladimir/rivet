// Package overview — обзор проекта для руководителя (change
// add-project-lead-view, спека backend/project-overview): прогноз и статус
// риска Epic'ов, решения, которые ждут людей, работа команды агентов,
// деньги, поставки и еженедельный отчёт.
//
// Пакет чистый: ни базы, ни HTTP. Store собирает Input одним согласованным
// снимком, Build считает Overview, Report рендерит Markdown. Все формулы
// детерминированы и описаны в спеке, модель в расчёте не участвует.
package overview

import (
	"time"

	"github.com/PavluninVladimir/rivet/internal/signals"
)

// Статусы задач и Epic'ов в терминах домена (строки, чтобы пакет не зависел
// от domain и store).
const (
	TaskQueued    = "queued"
	TaskReady     = "ready"
	TaskRunning   = "running"
	TaskTesting   = "testing"
	TaskReview    = "review"
	TaskFixing    = "fixing"
	TaskBlocked   = "blocked"
	TaskFailed    = "failed"
	TaskDone      = "done"
	TaskCancelled = "cancelled"

	EpicPlanned  = "planned"
	EpicRunning  = "running"
	EpicPaused   = "paused"
	EpicDone     = "done"
	EpicArchived = "archived"
)

// Роли шаблонов, по которым считаются возвраты (add-agent-roles).
const (
	RoleTester   = "tester"
	RoleReviewer = "reviewer"
)

// Вид шага процесса и вердикты, нужные расчёту качества.
const (
	StepKindReview = "review"
	VerdictChanges = "changes"
)

// Thresholds — пороги расчёта (design, решение 4). Отдаются в ответе, чтобы
// расчёт можно было перепроверить.
type Thresholds struct {
	VelocityWindowDays int `json:"velocity_window_days"`
	VelocityMinTasks   int `json:"velocity_min_tasks"`
	StalledMinutes     int `json:"stalled_minutes"`
	NoCapacityMinutes  int `json:"no_capacity_minutes"`
	StuckMinutes       int `json:"stuck_minutes"`
	LastAttemptLeft    int `json:"last_attempt_left"`
}

// DefaultThresholds — константы системы из спеки.
func DefaultThresholds() Thresholds {
	return Thresholds{
		VelocityWindowDays: 14, VelocityMinTasks: 3,
		StalledMinutes:    int(signals.StalledEpic / time.Minute),
		NoCapacityMinutes: int(signals.LongWait / time.Minute),
		StuckMinutes:      int(signals.SilentTask / time.Minute),
		LastAttemptLeft:   signals.LastAttemptLeft,
	}
}

// ─── вход ────────────────────────────────────────────────────────────────

// Input — согласованный снимок данных проекта на момент Now. Store отдаёт
// всё, что нужно разделам: задачи и Epic'и проекта целиком, сессии и usage
// с запасом по окнам (сутки, 7 суток, месяц, окно темпа, период).
//
// Для Report снимок приводится к моменту среза min(Period.To, Now) (asOf),
// поэтому store дополнительно отдаёт: записи usage за всю жизнь Epic'ов и
// задач отчёта (расход пересчитывается из записей до среза, поля Spend не
// используются) и в Deployments последнюю выкатку каждого окружения до
// начала периода. MySteps отчёт не использует.
//
// Известны только на момент формирования: попытки, LastEventAt и
// WaitingRunnerSince задач, отмена задачи, Estimate, пауза, возобновление
// и архивация Epic'а, DueAt, TokenBudget, BudgetExhausted,
// MonthlyBudgetUSD и список MergeWaits. Без Live отчёт не считает
// last_attempt, stalled, no_capacity, budget_exhausted, списки застрявших
// задач и задач на последней попытке и показывает ожидания merge как «нет
// данных». Остальные значения из этого списка берутся как есть.
type Input struct {
	ProjectID   string
	ProjectName string
	Now         time.Time
	Period      Period
	Location    *time.Location
	// Live — отчёт «текущий»: API ставит его, когда to не задан или to ≥
	// now, где now — тот же момент, от которого считается to по умолчанию.
	// Обзор (Build) всегда текущий и поле не читает.
	Live bool
	// Thresholds — нулевое значение означает DefaultThresholds.
	Thresholds Thresholds

	Roles          []Role
	Epics          []Epic
	Tasks          []Task
	Sessions       []Session
	Usage          []UsageRecord
	StepRuns       []StepRun
	MergeConflicts []MergeConflict
	Escalations    []Escalation
	MergeWaits     []MergeWait
	MySteps        []MyStep
	Environments   []Environment
	Deployments    []Deployment

	// DailyTokenBudget — дневной бюджет токенов политики проекта.
	DailyTokenBudget *int64
	// MonthlyBudgetUSD — плановый бюджет месяца (только для прогноза).
	MonthlyBudgetUSD *float64
}

// Role — шаблон роли из каталога, в порядке каталога.
type Role struct {
	ID    string
	Title string
	Color string
}

// Epic — Epic проекта. Spend — весь расход Epic'а за всё время.
type Epic struct {
	ID        string
	Title     string
	Status    string
	CreatedAt time.Time
	// PlannedAt — момент, когда план появился (для ожидания утверждения).
	PlannedAt *time.Time
	StartedAt *time.Time
	DoneAt    *time.Time
	DueAt     *time.Time
	// TokenBudget — бюджет Epic'а в токенах.
	TokenBudget *int64
	// BudgetExhausted — Epic остановлен исчерпанием бюджета.
	BudgetExhausted bool
	PauseReason     string
	Spend           Totals
	// Kind — вид Epic'а; пусто — work. Системные Epic'и («Архитектура
	// проекта», «Сопровождение») в активные и прогноз не входят.
	Kind string
}

// System — системный Epic: живёт вне плана, прогноза у него нет.
func (e Epic) System() bool { return e.Kind != "" && e.Kind != "work" }

// Task — задача проекта. Estimate — оценка в единицах планировщика.
type Task struct {
	ID           string
	EpicID       string
	Num          int
	Title        string
	Status       string
	Estimate     int
	CreatedAt    time.Time
	StartedAt    *time.Time
	DoneAt       *time.Time
	AttemptUsed  int
	AttemptLimit int
	// LastEventAt — время последнего события задачи.
	LastEventAt *time.Time
	// WaitingRunnerSince — с какого момента готовая задача ждёт runner'а.
	WaitingRunnerSince *time.Time
	// Spend — весь расход задачи (для средней стоимости задачи).
	Spend Totals
}

// Session — сессия стадии. Human — сессия человека, в работу конвейера не
// входит. End nil — сессия ещё идёт.
type Session struct {
	ID        string
	TaskID    string
	EpicID    string
	Human     bool
	RoleID    string
	AgentID   string
	AgentName string
	Start     time.Time
	End       *time.Time
}

// UsageRecord — запись usage. CostKnown — стоимость известна (запись с
// нулевой ценой у модели без прайса считается «без стоимости»).
type UsageRecord struct {
	TS        time.Time
	EpicID    string
	TaskID    string
	RoleID    string
	Model     string
	TokensIn  int64
	TokensOut int64
	CostUSD   float64
	CostKnown bool
	DurationS int64
}

// StepRun — запуск шага с вердиктом. At — момент вердикта.
type StepRun struct {
	TaskID   string
	StepKind string
	RoleID   string
	Verdict  string
	At       time.Time
}

// MergeConflict — событие task.merge_conflict.
type MergeConflict struct {
	TaskID    string
	At        time.Time
	Exhausted bool
}

// Escalation — эскалация (attention). Status: open | claimed | resolved.
type Escalation struct {
	ID         string
	TaskID     string
	EpicID     string
	TaskNum    int
	Title      string
	Reason     string
	Status     string
	ClaimedBy  string
	OpenedAt   time.Time
	ResolvedAt *time.Time
}

// MergeWait — задача ждёт подтверждения merge человеком.
type MergeWait struct {
	TaskID  string
	EpicID  string
	TaskNum int
	Title   string
	Since   time.Time
}

// MyStep — запуск шага, адресованный текущему пользователю.
type MyStep struct {
	RunID   string
	TaskID  string
	EpicID  string
	TaskNum int
	Title   string
	Since   time.Time
}

// Environment — окружение проекта с последней выкаткой.
type Environment struct {
	ID   string
	Name string
	Last *Deployment
}

// Deployment — выкатка (форма api-contract).
type Deployment struct {
	ID        string     `json:"id"`
	EnvID     string     `json:"env_id"`
	Version   string     `json:"version"`
	Status    string     `json:"status"`
	Initiator string     `json:"initiator"`
	CreatedAt time.Time  `json:"created_at"`
	EndedAt   *time.Time `json:"ended_at"`
	Rollback  bool       `json:"rollback"`
}

// ─── выход (api-contract) ────────────────────────────────────────────────

// Overview — ответ GET /projects/{id}/overview.
type Overview struct {
	ProjectID  string         `json:"project_id"`
	Period     Period         `json:"period"`
	ComputedAt time.Time      `json:"computed_at"`
	Thresholds Thresholds     `json:"thresholds"`
	Summary    Summary        `json:"summary"`
	Epics      []EpicOverview `json:"epics"`
	Decisions  Decisions      `json:"decisions"`
	Team       Team           `json:"team"`
	Money      Money          `json:"money"`
	Delivery   Delivery       `json:"delivery"`
}

// Summary — полоса показателей.
type Summary struct {
	ActiveEpics         int      `json:"active_epics"`
	TasksDone           int      `json:"tasks_done"`
	ThroughputPerDay    PerDay   `json:"throughput_per_day"`
	Spend               Amount   `json:"spend"`
	OpenDecisions       int      `json:"open_decisions"`
	FirstPassReviewRate *float64 `json:"first_pass_review_rate"`
}

// PerDay — значение в пересчёте на сутки.
type PerDay struct {
	Tasks float64 `json:"tasks"`
	Units float64 `json:"units"`
}

// EpicOverview — Epic в обзоре.
type EpicOverview struct {
	ID                   string     `json:"id"`
	Title                string     `json:"title"`
	Status               string     `json:"status"`
	Progress             Progress   `json:"progress"`
	DueAt                *time.Time `json:"due_at"`
	Forecast             Forecast   `json:"forecast"`
	Risk                 Risk       `json:"risk"`
	Spend                Amount     `json:"spend"`
	TokenBudget          *int64     `json:"token_budget"`
	BudgetForecastTokens *int64     `json:"budget_forecast_tokens"`
	// System — системный Epic: виден в списке с пометкой, в число
	// активных и в прогноз не входит.
	System bool `json:"system"`
}

// Progress — объём W (TotalUnits) и сделанное.
type Progress struct {
	DoneUnits  float64 `json:"done_units"`
	TotalUnits float64 `json:"total_units"`
	DoneTasks  int     `json:"done_tasks"`
	TotalTasks int     `json:"total_tasks"`
}

// Источники темпа.
const (
	SourceProject = "project"
	SourceEpic    = "epic"
)

// Forecast — прогноз завершения с входными величинами.
type Forecast struct {
	Available            bool       `json:"available"`
	At                   *time.Time `json:"at"`
	RemainingUnits       float64    `json:"remaining_units"`
	VelocityUnitsPerHour *float64   `json:"velocity_units_per_hour"`
	VelocitySource       *string    `json:"velocity_source"`
	Reason               string     `json:"reason,omitempty"`
}

// Статусы риска.
const (
	RiskOnTrack    = "on_track"
	RiskAtRisk     = "at_risk"
	RiskNeedsHuman = "needs_human"
	RiskDone       = "done"
)

// Коды причин риска в порядке важности.
const (
	ReasonEscalation      = "escalation"
	ReasonBudgetExhausted = "budget_exhausted"
	ReasonLate            = "late"
	ReasonBudgetForecast  = "budget_forecast"
	ReasonLastAttempt     = "last_attempt"
	ReasonStalled         = "stalled"
	ReasonNoCapacity      = "no_capacity"
)

// Risk — статус и причины.
type Risk struct {
	Status  string   `json:"status"`
	Reasons []Reason `json:"reasons"`
}

// Reason — причина риска со ссылкой на задачу или Epic.
type Reason struct {
	Code   string  `json:"code"`
	Text   string  `json:"text"`
	TaskID *string `json:"task_id"`
	EpicID string  `json:"epic_id"`
}

// Виды решений.
const (
	DecisionPlanApproval      = "plan_approval"
	DecisionEscalation        = "escalation"
	DecisionMergeConfirmation = "merge_confirmation"
	DecisionMyStep            = "my_step"
)

// Decisions — то, что ждёт людей.
type Decisions struct {
	Counts DecisionCounts `json:"counts"`
	Items  []Decision     `json:"items"`
}

// DecisionCounts — счётчики по видам.
type DecisionCounts struct {
	PlanApproval      int `json:"plan_approval"`
	Escalation        int `json:"escalation"`
	MergeConfirmation int `json:"merge_confirmation"`
	MyStep            int `json:"my_step"`
}

// Decision — пункт «Нужны решения».
type Decision struct {
	Kind           string    `json:"kind"`
	Title          string    `json:"title"`
	Since          time.Time `json:"since"`
	WaitingSeconds int64     `json:"waiting_seconds"`
	EpicID         *string   `json:"epic_id"`
	TaskID         *string   `json:"task_id"`
	TaskNum        *int      `json:"task_num"`
	RunID          *string   `json:"run_id"`
	AttentionID    *string   `json:"attention_id"`
	Reason         *string   `json:"reason"`
	ClaimedBy      *string   `json:"claimed_by"`
}

// Team — работа команды агентов.
type Team struct {
	Roles      []RoleLoad `json:"roles"`
	Throughput Throughput `json:"throughput"`
	Quality    Quality    `json:"quality"`
}

// RoleLoad — загрузка роли. Role "" — агенты без роли.
type RoleLoad struct {
	Role        string  `json:"role"`
	Title       string  `json:"title"`
	Color       string  `json:"color"`
	BusyNow     int     `json:"busy_now"`
	AvgBusy     float64 `json:"avg_busy"`
	Sessions    int     `json:"sessions"`
	BusySeconds int64   `json:"busy_seconds"`
}

// Throughput — пропускная способность за период.
type Throughput struct {
	TasksDone int     `json:"tasks_done"`
	UnitsDone float64 `json:"units_done"`
	PerDay    PerDay  `json:"per_day"`
}

// Quality — качество за период.
type Quality struct {
	FirstPassReviewRate *float64           `json:"first_pass_review_rate"`
	TesterReturns       int                `json:"tester_returns"`
	ReviewerReturns     int                `json:"reviewer_returns"`
	MergeConflicts      MergeConflictStats `json:"merge_conflicts"`
	Escalations         []ReasonCount      `json:"escalations"`
}

// MergeConflictStats — задачи, вернувшиеся агенту из-за конфликта, и
// сколько из них затем завершены.
type MergeConflictStats struct {
	Returned int `json:"returned"`
	Done     int `json:"done"`
}

// ReasonCount — число эскалаций по причине.
type ReasonCount struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// Money — деньги проекта.
type Money struct {
	Today              Amount       `json:"today"`
	Last7d             Amount       `json:"last_7d"`
	MonthToDate        Amount       `json:"month_to_date"`
	Period             Amount       `json:"period"`
	ByRole             []Share      `json:"by_role"`
	ByModel            []Share      `json:"by_model"`
	AvgCostPerTask     *float64     `json:"avg_cost_per_task"`
	DailyTokenBudget   *DailyBudget `json:"daily_token_budget"`
	MonthlyBudgetUSD   *float64     `json:"monthly_budget_usd"`
	MonthForecastUSD   *float64     `json:"month_forecast_usd"`
	MonthOverBudgetUSD *float64     `json:"month_over_budget_usd"`
}

// Share — доля группы в расходе за период.
type Share struct {
	Key    string `json:"key"`
	Title  string `json:"title"`
	Amount Amount `json:"amount"`
	// Share — доля от итога: по деньгам, если стоимость известна, иначе по
	// токенам. nil — у группы нет стоимости при денежном разрезе.
	Share *float64 `json:"share"`
}

// DailyBudget — дневной бюджет токенов против расхода за сегодня.
type DailyBudget struct {
	Budget    int64 `json:"budget"`
	UsedToday int64 `json:"used_today"`
}

// Amount — расход. nil — данных нет (а не ноль).
type Amount struct {
	TokensIn    *int64   `json:"tokens_in"`
	TokensOut   *int64   `json:"tokens_out"`
	CostUSD     *float64 `json:"cost_usd"`
	DurationS   int64    `json:"duration_s"`
	PartialCost bool     `json:"partial_cost"`
}

// Delivery — поставки и риски.
type Delivery struct {
	Environments []EnvStatus   `json:"environments"`
	Deployments  []Deployment  `json:"deployments"`
	Stuck        []StuckTask   `json:"stuck"`
	LastAttempt  []AttemptTask `json:"last_attempt"`
	BudgetRisks  []BudgetRisk  `json:"budget_risks"`
}

// EnvStatus — окружение и его последняя выкатка.
type EnvStatus struct {
	ID   string      `json:"id"`
	Name string      `json:"name"`
	Last *Deployment `json:"last"`
}

// StuckTask — застрявшая задача.
type StuckTask struct {
	TaskID        string     `json:"task_id"`
	TaskNum       int        `json:"task_num"`
	Title         string     `json:"title"`
	Status        string     `json:"status"`
	LastEventAt   *time.Time `json:"last_event_at"`
	WaitingRunner bool       `json:"waiting_runner"`
}

// AttemptTask — задача на последней попытке.
type AttemptTask struct {
	TaskID       string `json:"task_id"`
	TaskNum      int    `json:"task_num"`
	Title        string `json:"title"`
	AttemptUsed  int    `json:"attempt_used"`
	AttemptLimit int    `json:"attempt_limit"`
}

// BudgetRisk — Epic с прогнозом расхода выше бюджета.
type BudgetRisk struct {
	EpicID               string `json:"epic_id"`
	Title                string `json:"title"`
	TokenBudget          int64  `json:"token_budget"`
	BudgetForecastTokens int64  `json:"budget_forecast_tokens"`
}
