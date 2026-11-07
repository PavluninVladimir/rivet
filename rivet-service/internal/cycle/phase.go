// Package cycle — цикл разработки проекта из восьми фаз (change
// add-sdlc-cycle, спека backend/sdlc-cycle). Фаза не хранится: пакет
// вычисляет её из состояния сущностей и собирает агрегат цикла. Пакет
// чистый: ни базы, ни HTTP. Store читает строки, оркестратор зовёт те же
// функции при записи событий перехода, поэтому правила живут в одном месте.
package cycle

import "time"

// Phase — идентификатор фазы цикла.
type Phase string

// Фазы в фиксированном порядке спеки.
const (
	Discovery    Phase = "discovery"
	Architecture Phase = "architecture"
	Planning     Phase = "planning"
	Development  Phase = "development"
	Testing      Phase = "testing"
	Review       Phase = "review"
	Release      Phase = "release"
	Operations   Phase = "operations"
)

// PhaseInfo — описание фазы: название и шаблон роли, который её ведёт.
type PhaseInfo struct {
	ID    Phase
	Title string
	Role  string
}

var phases = []PhaseInfo{
	{Discovery, "Исследование", "analyst"},
	{Architecture, "Архитектура", "architect"},
	{Planning, "Планирование", "lead"},
	{Development, "Разработка", "developer"},
	{Testing, "Тестирование", "tester"},
	{Review, "Ревью", "reviewer"},
	{Release, "Релиз", "devops"},
	{Operations, "Сопровождение", "sre"},
}

// Phases — восемь фаз в порядке спеки (копия, вызывающий может менять).
func Phases() []PhaseInfo { return append([]PhaseInfo(nil), phases...) }

// Info — описание фазы по идентификатору.
func Info(p Phase) (PhaseInfo, bool) {
	for _, ph := range phases {
		if ph.ID == p {
			return ph, true
		}
	}
	return PhaseInfo{}, false
}

// ItemState — состояние элемента внутри фазы.
type ItemState string

const (
	StateActive     ItemState = "active"
	StateWaiting    ItemState = "waiting"
	StateNeedsHuman ItemState = "needs_human"
)

// Отметки элемента.
const (
	BadgeBug             = "bug"
	BadgeAwaitingRelease = "awaiting_release"
	BadgeRollback        = "rollback"
)

// Placement — куда сущность попадает в цикле. Нулевая фаза означает, что
// сущность в активные элементы не входит (завершена или вне цикла).
type Placement struct {
	Phase Phase
	State ItemState
	Badge string
}

// In — сущность входит в активные элементы какой-то фазы.
func (p Placement) In() bool { return p.Phase != "" }

func at(ph Phase, needsHuman bool, base ItemState) Placement {
	if needsHuman {
		return Placement{Phase: ph, State: StateNeedsHuman}
	}
	return Placement{Phase: ph, State: base}
}

// ─── задачи ──────────────────────────────────────────────────────────────

// Виды задач и Epic'ов (поле Kind из add-architecture-c4 и этого change'а).
const (
	KindWork         = "work"
	KindBug          = "bug"
	KindArchitecture = "architecture"
	KindMaintenance  = "maintenance"
)

// Step — шаг снимка процесса задачи: идентификатор и тип, в порядке процесса.
type Step struct {
	ID   string
	Kind string // code | test | review | merge | deploy | prompt
}

// Task — то, что нужно знать о задаче для отнесения к фазе.
type Task struct {
	ID    string
	Num   int
	Title string
	// EpicID — Epic задачи (ссылка консоли на дашборд).
	EpicID string
	// Kind — work, bug или architecture; пусто читается как work.
	Kind string
	// Status — статус задачи domain: queued, ready, running, testing,
	// review, fixing, blocked, failed, done, cancelled.
	Status string
	// StepID — текущий шаг по снимку процесса задачи.
	StepID string
	// Process — шаги снимка процесса задачи в порядке процесса.
	Process []Step
	// AwaitingMerge — задача ждёт подтверждения merge человеком.
	AwaitingMerge bool
	// EpicRunning — Epic задачи запущен (задачи ждут runner'а или зависимостей).
	EpicRunning bool
	// NeedsHuman — заблокирована, эскалирована или исчерпала попытки.
	NeedsHuman bool
	// MergedAt — время merge, nil у несмерженных.
	MergedAt *time.Time
	// Since — вход в текущую фазу (для сортировки элементов).
	Since time.Time
}

// ReleaseCut — граница релиза проекта: есть ли окружение с автозапуском и
// когда создана его последняя успешная публикация. Задача входит в
// публикацию, если её merge был не позже создания публикации (полуинтервал
// состава релиза (прошлая, эта], design решение 4).
type ReleaseCut struct {
	AutoEnv     bool
	LastCreated *time.Time
}

// AwaitingRelease — смерженная задача ещё не вошла в успешную публикацию
// окружения с автозапуском.
func (c ReleaseCut) AwaitingRelease(mergedAt *time.Time) bool {
	if !c.AutoEnv || mergedAt == nil {
		return false
	}
	return c.LastCreated == nil || mergedAt.After(*c.LastCreated)
}

// phaseOfStepKind — фаза типа шага; ok=false для prompt (решает сосед) и
// неизвестных типов.
func phaseOfStepKind(kind string) (Phase, bool) {
	switch kind {
	case "code":
		return Development, true
	case "test":
		return Testing, true
	case "review", "merge":
		return Review, true
	case "deploy":
		return Release, true
	}
	return "", false
}

// stepPhase — фаза шага задачи: по типу шага, а шаг prompt относится к фазе
// ближайшего предыдущего шага другого типа, без такого шага к разработке.
func stepPhase(t Task) (Phase, bool) {
	idx := -1
	for i, s := range t.Process {
		if s.ID == t.StepID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return "", false
	}
	if ph, ok := phaseOfStepKind(t.Process[idx].Kind); ok {
		return ph, true
	}
	if t.Process[idx].Kind != "prompt" {
		return "", false
	}
	for j := idx - 1; j >= 0; j-- {
		if ph, ok := phaseOfStepKind(t.Process[j].Kind); ok {
			return ph, true
		}
	}
	return Development, true
}

// statusPhase — запасное отнесение по статусу, если шаг не найден в снимке
// (задача без снимка процесса или снимок прежней версии).
func statusPhase(status string) Phase {
	switch status {
	case "testing":
		return Testing
	case "review":
		return Review
	}
	return Development
}

// PhaseOfTask относит задачу к фазе по правилам спеки «Отнесение к фазе».
func PhaseOfTask(t Task, cut ReleaseCut) Placement {
	badge := ""
	if t.Kind == KindBug {
		badge = BadgeBug
	}
	with := func(p Placement) Placement {
		if p.Badge == "" {
			p.Badge = badge
		}
		return p
	}
	switch t.Status {
	case "cancelled":
		return Placement{}
	case "done":
		// Релиза ждут задачи с точкой merge, как в составе релиза
		// (ops.Composition). У задачи описания архитектуры её нет
		// (add-architecture-c4), сюда она не доходит.
		if cut.AwaitingRelease(t.MergedAt) {
			return Placement{Phase: Release, State: StateWaiting, Badge: BadgeAwaitingRelease}
		}
		return Placement{}
	}
	if t.Kind == KindArchitecture {
		return with(at(Architecture, t.NeedsHuman, StateActive))
	}
	if t.AwaitingMerge {
		return with(at(Review, t.NeedsHuman, StateWaiting))
	}
	switch t.Status {
	case "queued", "ready":
		if t.NeedsHuman {
			return with(at(Development, true, StateWaiting))
		}
		if !t.EpicRunning {
			// Задачи не запущенного Epic'а принадлежат планированию через Epic.
			return Placement{}
		}
		return with(Placement{Phase: Development, State: StateWaiting})
	}
	ph, ok := stepPhase(t)
	if !ok {
		ph = statusPhase(t.Status)
	}
	return with(at(ph, t.NeedsHuman || t.Status == "blocked" || t.Status == "failed", StateActive))
}

// ─── Epic'и, публикации, инциденты, исследование ────────────────────────

// Epic — то, что нужно знать об Epic'е.
type Epic struct {
	ID    string
	Title string
	// Kind — work, architecture или maintenance; пусто читается как work.
	Kind string
	// Status — planning, planned, exploring, running, paused, done, archived.
	Status     string
	NeedsHuman bool
	Since      time.Time
}

// PhaseOfEpic: Epic в исследовании архитектуры относится к архитектуре,
// Epic вида work без утверждённого плана к планированию. План в работе у
// Тимлида активен, готовый план ждёт человека.
func PhaseOfEpic(e Epic) Placement {
	if e.Status == "exploring" {
		return at(Architecture, e.NeedsHuman, StateActive)
	}
	if e.Kind != "" && e.Kind != KindWork {
		return Placement{}
	}
	switch e.Status {
	case "planning":
		return at(Planning, e.NeedsHuman, StateActive)
	case "planned":
		return Placement{Phase: Planning, State: StateNeedsHuman}
	}
	return Placement{}
}

// Deployment — публикация окружения.
type Deployment struct {
	ID     string
	Title  string
	Status string // queued | deploying | verifying | done | failed | rolled_back
	// Rollback — публикация в фазе отката.
	Rollback bool
	Since    time.Time
}

// PhaseOfDeployment: идущие публикации и откат относятся к релизу.
func PhaseOfDeployment(d Deployment) Placement {
	switch d.Status {
	case "queued":
		return Placement{Phase: Release, State: StateWaiting}
	case "deploying", "verifying":
		p := Placement{Phase: Release, State: StateActive}
		if d.Rollback {
			p.Badge = BadgeRollback
		}
		return p
	}
	return Placement{}
}

// Incident — инцидент сопровождения.
type Incident struct {
	ID     string
	Title  string
	EnvID  string
	Status string // open | mitigated | resolved
	// NeedsHuman — по инциденту открыта эскалация (лимит сопровождения).
	NeedsHuman bool
	Since      time.Time
}

// PhaseOfIncident: открытые инциденты активны в сопровождении, смягчённые
// ждут подтверждающих проверок.
func PhaseOfIncident(i Incident) Placement {
	switch i.Status {
	case "open":
		return at(Operations, i.NeedsHuman, StateActive)
	case "mitigated":
		return at(Operations, i.NeedsHuman, StateWaiting)
	}
	return Placement{}
}

// Material — интервью или отзыв пользователя.
type Material struct {
	ID    string
	Title string
	Kind  string // interview | feedback
	Since time.Time
}

// LastRun — последняя готовая версия исследования, то есть последняя
// версия с результатом анализа (ready, approved или superseded после неё):
// какие материалы она учла. nil-множество означает, что готовых версий ещё
// не было. Идущий анализ материалы не учитывает: если он упадёт, они снова
// ждут анализа.
type LastRun struct {
	Included map[string]bool
}

// PhaseOfMaterial: материал, не учтённый последней готовой версией
// исследования, ждёт анализа.
func PhaseOfMaterial(m Material, last LastRun) Placement {
	if last.Included[m.ID] {
		return Placement{}
	}
	return Placement{Phase: Discovery, State: StateWaiting}
}

// Analysis — версия исследования: идущий анализ или бэклог на утверждении.
type Analysis struct {
	ID    string
	Title string
	// Status — статус версии (discovery_runs): running — идёт анализ,
	// ready — бэклог ждёт утверждения владельцем. Остальные статусы
	// (failed, approved, superseded) в цикл не входят.
	Status string
	Since  time.Time
}

// PhaseOfAnalysis: идущий анализ активен, бэклог на утверждении ждёт человека.
func PhaseOfAnalysis(a Analysis) Placement {
	switch a.Status {
	case "running":
		return Placement{Phase: Discovery, State: StateActive}
	case "ready":
		return Placement{Phase: Discovery, State: StateNeedsHuman}
	}
	return Placement{}
}

// EnvMonitor — окружение под наблюдением.
type EnvMonitor struct {
	EnvID string
	Name  string
	State string // ok | fail | unknown | disabled
	Since time.Time
}

// PhaseOfEnv: окружение с непройденной последней проверкой относится к
// сопровождению. Если у окружения есть активный инцидент, элементом служит
// инцидент, окружение не дублирует его (решает агрегат).
func PhaseOfEnv(e EnvMonitor) Placement {
	if e.State == "fail" {
		return Placement{Phase: Operations, State: StateActive}
	}
	return Placement{}
}

// ─── переходы ────────────────────────────────────────────────────────────

// Transition — фазы до и после изменения сущности для полей событий
// `phase_from` и `phase_to`. Пустая строка означает «вне цикла» (null в
// JSON). changed=false, если фаза не изменилась: тогда поля в событие не
// пишутся.
func Transition(before, after Placement) (from, to Phase, changed bool) {
	return before.Phase, after.Phase, before.Phase != after.Phase
}

// TaskTransition — Transition для задачи до и после изменения.
func TaskTransition(before, after Task, cut ReleaseCut) (from, to Phase, changed bool) {
	return Transition(PhaseOfTask(before, cut), PhaseOfTask(after, cut))
}

// PhasePtr — фаза для JSON-поля события: nil вместо пустой строки.
func PhasePtr(p Phase) *string {
	if p == "" {
		return nil
	}
	s := string(p)
	return &s
}
