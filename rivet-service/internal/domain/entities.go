package domain

import "time"

type Project struct {
	ID      string
	Name    string
	Repo    string // owner/name на GitHub
	Checks  []Check
	Created time.Time
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
	RunnerID     string
	Branch       string
	PRURL        string
	BlockReason  string
	BlockedBy    string // id задачи-первопричины при каскадной блокировке
	Created      time.Time
	Updated      time.Time
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
	Tokens        int64
	Started       time.Time
	Ended         *time.Time
}

// User — учётная запись человека (спека domain-model «Пользователи и членство
// в проекте»). Login неизменяем: по нему атрибутируются события.
type User struct {
	ID       string
	Login    string
	Name     string
	Admin    bool
	Disabled bool
	Created  time.Time
}

// Member — участник проекта.
type Member struct {
	Login string
	Name  string
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
}

type AttentionReason string

const (
	AttBlocked     AttentionReason = "BLOCKED"
	AttReviewLimit AttentionReason = "REVIEW_LIMIT"
	AttTestFailed  AttentionReason = "TEST_FAILED"
	AttRunnerLost  AttentionReason = "RUNNER_LOST"
)

type Attention struct {
	ID        string
	ProjectID string
	TaskID    string
	Reason    AttentionReason
	Message   string
	Status    string // open | claimed | resolved
	ClaimedBy string
	Created   time.Time
}
