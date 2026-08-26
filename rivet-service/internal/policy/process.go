package policy

import (
	"fmt"
	"regexp"
	"strings"
)

// Процесс задачи проекта как документ (change add-process-model, спека
// backend/process): шаги, участники, переходы и лимиты. Документ живёт в
// политике (раздел process), движок исполняет его разрешённую форму.

// Типы шагов.
const (
	StepCode   = "code"
	StepTest   = "test"
	StepReview = "review"
	StepMerge  = "merge"
	StepDeploy = "deploy"
)

// Режимы работы участников и правила агрегации вердиктов.
const (
	ModeParallel   = "parallel"
	ModeSequential = "sequential"
	RequireAll     = "all"
	RequireAny     = "any"
)

// Исходы шага и вердикты участников.
const (
	OutcomeOk      = "ok"
	OutcomeChanges = "changes"
	OutcomeFail    = "fail"
	OutcomeBlocked = "blocked"
)

// Цели переходов помимо идентификаторов шагов.
const (
	TargetEscalate = "escalate"
	TargetDone     = "done"
)

// Process — документ процесса: упорядоченный список шагов.
type Process struct {
	Steps []Step `json:"steps"`
}

// Step — шаг процесса. Нулевые поля означают значения по умолчанию, их
// раскрывает Resolve.
type Step struct {
	ID           string        `json:"id"`
	Kind         string        `json:"kind"`
	Title        string        `json:"title,omitempty"`
	Enabled      *bool         `json:"enabled,omitempty"`
	Capabilities []string      `json:"capabilities,omitempty"`
	Participants []Participant `json:"participants,omitempty"`
	Mode         string        `json:"mode,omitempty"`
	Require      string        `json:"require,omitempty"`
	Attempts     *int          `json:"attempts,omitempty"`
	On           *Transitions  `json:"on,omitempty"`
}

// Participant — участник шага: агент или человек (ровно одно поле).
type Participant struct {
	Agent *AgentRef `json:"agent,omitempty"`
	User  *UserRef  `json:"user,omitempty"`
}

// AgentRef — агент участника: тип и модель; пустые — любой runner с
// capabilities шага.
type AgentRef struct {
	Kind  string `json:"kind,omitempty"`
	Model string `json:"model,omitempty"`
}

// UserRef — человек участника: логин или роль проекта. Схема принимается,
// исполнение появится в следующем изменении (add-process-humans).
type UserRef struct {
	Login string `json:"login,omitempty"`
	Role  string `json:"role,omitempty"`
}

// Transitions — переходы по исходу шага: идентификатор шага, escalate или done.
type Transitions struct {
	Ok      string `json:"ok,omitempty"`
	Changes string `json:"changes,omitempty"`
	Fail    string `json:"fail,omitempty"`
}

// ProcessError — ошибка валидации с привязкой к шагу и полю (api-contract:
// 422 с step и field).
type ProcessError struct {
	Step  string
	Field string
	Msg   string
}

func (e *ProcessError) Error() string {
	where := ""
	if e.Step != "" {
		where = "шаг " + e.Step
		if e.Field != "" {
			where += ", поле " + e.Field
		}
		where += ": "
	}
	return ErrInvalid.Error() + ": " + where + e.Msg
}

func (e *ProcessError) Unwrap() error { return ErrInvalid }

func perr(step, field, format string, args ...any) error {
	return &ProcessError{Step: step, Field: field, Msg: fmt.Sprintf(format, args...)}
}

// DefaultProcess — процесс установки по умолчанию, воспроизводящий
// стандартный конвейер: code → test → review → merge → deploy с одним
// агентом любого типа на шаге.
func DefaultProcess() Process {
	anyAgent := []Participant{{Agent: &AgentRef{}}}
	return Process{Steps: []Step{
		{ID: StepCode, Kind: StepCode, Title: "Реализация", Participants: anyAgent},
		{ID: StepTest, Kind: StepTest, Title: "Проверки", Participants: anyAgent},
		{ID: StepReview, Kind: StepReview, Title: "Review", Participants: anyAgent},
		{ID: StepMerge, Kind: StepMerge, Title: "Merge"},
		{ID: StepDeploy, Kind: StepDeploy, Title: "Публикация"},
	}}
}

var stepIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func validKind(k string) bool {
	switch k {
	case StepCode, StepTest, StepReview, StepMerge, StepDeploy:
		return true
	}
	return false
}

// hasParticipants — шаг исполняется участниками (агентами); merge и deploy
// исполняет control plane.
func hasParticipants(kind string) bool {
	return kind == StepCode || kind == StepTest || kind == StepReview
}

func (s Step) enabled() bool { return s.Enabled == nil || *s.Enabled }

// Validate проверяет документ: идентификаторы, типы, участники, режимы,
// лимиты и переходы. Участники-люди отклоняются до их поддержки.
func (p Process) Validate() error {
	if len(p.Steps) == 0 {
		return perr("", "steps", "в процессе нет ни одного шага")
	}
	byID := map[string]Step{}
	merges := 0
	for _, s := range p.Steps {
		if !stepIDRe.MatchString(s.ID) {
			return perr(s.ID, "id", "идентификатор шага должен быть из [a-z0-9-] длиной до 32 символов")
		}
		if _, dup := byID[s.ID]; dup {
			return perr(s.ID, "id", "идентификатор шага повторяется")
		}
		byID[s.ID] = s
		if !validKind(s.Kind) {
			return perr(s.ID, "kind", "неизвестный тип шага %q (допустимы code, test, review, merge, deploy)", s.Kind)
		}
		if strings.ContainsFunc(s.Title, promptBreaking) {
			return perr(s.ID, "title", "название содержит управляющий символ")
		}
		for _, c := range s.Capabilities {
			if strings.TrimSpace(c) == "" || strings.ContainsFunc(c, promptBreaking) {
				return perr(s.ID, "capabilities", "некорректная capability %q", c)
			}
		}
		if s.Mode != "" && s.Mode != ModeParallel && s.Mode != ModeSequential {
			return perr(s.ID, "mode", "режим %q: допустимы parallel и sequential", s.Mode)
		}
		if s.Require != "" && s.Require != RequireAll && s.Require != RequireAny {
			return perr(s.ID, "require", "правило %q: допустимы all и any", s.Require)
		}
		if s.Attempts != nil && *s.Attempts < 1 {
			return perr(s.ID, "attempts", "лимит проходов должен быть не меньше 1")
		}
		if hasParticipants(s.Kind) {
			if s.enabled() && len(s.Participants) == 0 {
				return perr(s.ID, "participants", "у включённого шага должен быть хотя бы один участник")
			}
			for i, part := range s.Participants {
				if err := part.validate(s.ID, i); err != nil {
					return err
				}
			}
		} else if len(s.Participants) > 0 {
			return perr(s.ID, "participants", "у шага типа %s участников нет: его исполняет control plane", s.Kind)
		}
		if s.Kind == StepMerge && s.enabled() {
			merges++
		}
	}
	if merges != 1 {
		return perr("", "steps", "в процессе должен быть ровно один включённый шаг merge, сейчас %d", merges)
	}
	for _, s := range p.Steps {
		if s.On == nil {
			continue
		}
		if err := p.validateTarget(byID, s, "ok", s.On.Ok); err != nil {
			return err
		}
		if s.Kind == StepMerge || s.Kind == StepDeploy {
			if s.On.Changes != "" || s.On.Fail != "" {
				return perr(s.ID, "on", "у шага типа %s допустим только переход ok", s.Kind)
			}
			continue
		}
		if err := p.validateTarget(byID, s, "changes", s.On.Changes); err != nil {
			return err
		}
		if s.On.Changes != "" && s.On.Changes != TargetEscalate && s.On.Changes != TargetDone && byID[s.On.Changes].Kind != StepCode {
			return perr(s.ID, "on.changes", "переход changes ведёт на шаг %q типа %s, а исправление возможно только шагом code", s.On.Changes, byID[s.On.Changes].Kind)
		}
		if err := p.validateTarget(byID, s, "fail", s.On.Fail); err != nil {
			return err
		}
	}
	return nil
}

func (p Process) validateTarget(byID map[string]Step, s Step, name, target string) error {
	if target == "" || target == TargetEscalate || target == TargetDone {
		return nil
	}
	t, ok := byID[target]
	if !ok {
		return perr(s.ID, "on."+name, "переход %s ведёт на несуществующий шаг %q", name, target)
	}
	if !t.enabled() {
		return perr(s.ID, "on."+name, "переход %s ведёт на отключённый шаг %q", name, target)
	}
	return nil
}

func (part Participant) validate(stepID string, i int) error {
	field := fmt.Sprintf("participants[%d]", i)
	switch {
	case part.Agent != nil && part.User != nil:
		return perr(stepID, field, "участник задаётся либо агентом, либо человеком")
	case part.User != nil:
		return perr(stepID, field, "участники-люди появятся в следующем изменении (add-process-humans)")
	case part.Agent == nil:
		return perr(stepID, field, "у участника не указан ни агент, ни человек")
	}
	for name, v := range map[string]string{"kind": part.Agent.Kind, "model": part.Agent.Model} {
		if strings.ContainsFunc(v, promptBreaking) || strings.ContainsAny(v, " \t") {
			return perr(stepID, field+"."+name, "значение %q содержит недопустимые символы", v)
		}
	}
	return nil
}

// ─── разрешённая форма ───────────────────────────────────────────────────

// Resolved — процесс с раскрытыми значениями по умолчанию и только
// включёнными шагами: его исполняет движок и хранит задача как снимок.
type Resolved struct {
	Steps []ResolvedStep `json:"steps"`
	// Skipped — отключённые шаги в порядке документа (для событий о пропуске).
	Skipped []string `json:"skipped,omitempty"`
}

// ResolvedStep — шаг с заполненными полями.
type ResolvedStep struct {
	ID           string                `json:"id"`
	Kind         string                `json:"kind"`
	Title        string                `json:"title"`
	Capabilities []string              `json:"capabilities"`
	Participants []ResolvedParticipant `json:"participants"`
	Mode         string                `json:"mode"`
	Require      string                `json:"require"`
	Attempts     int                   `json:"attempts"`
	On           Transitions           `json:"on"`
}

// ResolvedParticipant — участник с порядковым идентификатором (p1, p2, …).
type ResolvedParticipant struct {
	ID    string   `json:"id"`
	Agent AgentRef `json:"agent"`
}

// Resolve раскрывает значения по умолчанию: переходы (ok — следующий
// включённый шаг, changes — ближайший предыдущий включённый шаг code,
// fail — escalate), лимиты из пресетов (review — лимит отказов review,
// остальные — лимит попыток), режим parallel, правило all, capabilities по
// типу (review → review). Документ должен быть валиден.
func Resolve(p Process, presets Presets) Resolved {
	var out Resolved
	var enabled []Step
	for _, s := range p.Steps {
		if s.enabled() {
			enabled = append(enabled, s)
		} else {
			out.Skipped = append(out.Skipped, s.ID)
		}
	}
	for i, s := range enabled {
		rs := ResolvedStep{ID: s.ID, Kind: s.Kind, Title: s.Title, Mode: s.Mode, Require: s.Require}
		if rs.Title == "" {
			rs.Title = defaultTitle(s.Kind)
		}
		if rs.Mode == "" {
			rs.Mode = ModeParallel
		}
		if rs.Require == "" {
			rs.Require = RequireAll
		}
		rs.Capabilities = append([]string{}, s.Capabilities...)
		if len(rs.Capabilities) == 0 && s.Kind == StepReview {
			rs.Capabilities = []string{"review"}
		}
		if s.Attempts != nil {
			rs.Attempts = *s.Attempts
		} else if s.Kind == StepReview {
			rs.Attempts = presets.ReviewLimit
		} else {
			rs.Attempts = presets.AttemptLimit
		}
		if rs.Attempts < 1 {
			rs.Attempts = 1
		}
		for j, part := range s.Participants {
			rp := ResolvedParticipant{ID: fmt.Sprintf("p%d", j+1)}
			if part.Agent != nil {
				rp.Agent = *part.Agent
			}
			rs.Participants = append(rs.Participants, rp)
		}
		if rs.Participants == nil {
			rs.Participants = []ResolvedParticipant{}
		}
		if s.On != nil {
			rs.On = *s.On
		}
		if rs.On.Ok == "" {
			if i+1 < len(enabled) {
				rs.On.Ok = enabled[i+1].ID
			} else {
				rs.On.Ok = TargetDone
			}
		}
		if hasParticipants(s.Kind) {
			if rs.On.Changes == "" {
				rs.On.Changes = nearestCode(enabled, i)
			}
			if rs.On.Fail == "" {
				rs.On.Fail = TargetEscalate
			}
		}
		out.Steps = append(out.Steps, rs)
	}
	return out
}

// nearestCode — ближайший предыдущий (включая сам) включённый шаг code;
// без него исправлять нечем — эскалация.
func nearestCode(enabled []Step, i int) string {
	for j := i; j >= 0; j-- {
		if enabled[j].Kind == StepCode {
			return enabled[j].ID
		}
	}
	return TargetEscalate
}

func defaultTitle(kind string) string {
	switch kind {
	case StepCode:
		return "Реализация"
	case StepTest:
		return "Проверки"
	case StepReview:
		return "Review"
	case StepMerge:
		return "Merge"
	case StepDeploy:
		return "Публикация"
	}
	return kind
}

// Step возвращает шаг по идентификатору; ok=false — шага нет (например,
// снимок задачи из другой версии процесса).
func (r Resolved) Step(id string) (ResolvedStep, bool) {
	for _, s := range r.Steps {
		if s.ID == id {
			return s, true
		}
	}
	return ResolvedStep{}, false
}

// First — первый включённый шаг.
func (r Resolved) First() (ResolvedStep, bool) {
	if len(r.Steps) == 0 {
		return ResolvedStep{}, false
	}
	return r.Steps[0], true
}

// HasKind — есть ли в процессе включённый шаг такого типа.
func (r Resolved) HasKind(kind string) bool {
	for _, s := range r.Steps {
		if s.Kind == kind {
			return true
		}
	}
	return false
}

// Participant — участник шага по идентификатору.
func (s ResolvedStep) Participant(id string) (ResolvedParticipant, bool) {
	for _, p := range s.Participants {
		if p.ID == id {
			return p, true
		}
	}
	return ResolvedParticipant{}, false
}

// Target — цель перехода по исходу; blocked переходом не является.
func (s ResolvedStep) Target(outcome string) string {
	switch outcome {
	case OutcomeOk:
		return s.On.Ok
	case OutcomeChanges:
		return s.On.Changes
	case OutcomeFail:
		return s.On.Fail
	}
	return ""
}
