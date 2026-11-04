package ops

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Status — статус инцидента.
type Status string

const (
	StatusOpen      Status = "open"
	StatusMitigated Status = "mitigated"
	StatusResolved  Status = "resolved"
)

// Active — инцидент открыт или смягчён (для частичного уникального индекса
// по отпечатку и для повторов).
func (s Status) Active() bool { return s == StatusOpen || s == StatusMitigated }

// Severity — серьёзность инцидента.
type Severity string

const (
	SeverityMedium Severity = "medium"
	SeverityHigh   Severity = "high"
)

// Resolution — как закрыт инцидент.
type Resolution string

const (
	ResolutionFixed  Resolution = "fixed"
	ResolutionSelf   Resolution = "self"
	ResolutionManual Resolution = "manual"
)

// BugState — состояние задачи-бага инцидента.
type BugState string

const (
	BugNone       BugState = ""
	BugNotStarted BugState = "not_started" // в очереди, ещё не взята в работу
	BugActive     BugState = "active"      // в работе, на проверках или review
	BugDone       BugState = "done"
	// BugCancelled — отменена системой (самоустранение инцидента).
	BugCancelled BugState = "cancelled"
	// BugCancelledByHuman — отменена человеком: автоматически не
	// пересоздаётся, инцидент ждёт решения человека.
	BugCancelledByHuman BugState = "cancelled_by_human"
)

// Unfinished — у инцидента уже есть незавершённая задача-баг.
func (b BugState) Unfinished() bool { return b == BugNotStarted || b == BugActive }

// Limits — лимиты и пороги сопровождения.
type Limits struct {
	// MaxOpenBugs — незавершённых задач-багов из инцидентов на проект.
	MaxOpenBugs int
	// MaxIncidentsPerDay — новых инцидентов за DailyWindow, сверх них без багов.
	MaxIncidentsPerDay int
	// DailyWindow — окно суточного лимита: инцидент, пропущенный по нему,
	// не получает баг раньше, чем через окно после первого появления.
	DailyWindow time.Duration
	// ReopenWindow — повтор после закрытия в этом окне переоткрывает инцидент.
	ReopenWindow time.Duration
	// OKToResolve — успешных проверок подряд для закрытия.
	OKToResolve int
	// FailsForHigh — проваленных проверок подряд для серьёзности high и
	// провалов после смягчения для возврата инцидента в open.
	FailsForHigh int
}

// DefaultLimits — значения из спеки.
func DefaultLimits() Limits {
	return Limits{MaxOpenBugs: 3, MaxIncidentsPerDay: 5, DailyWindow: 24 * time.Hour, ReopenWindow: 24 * time.Hour, OKToResolve: 3, FailsForHigh: 3}
}

// Incident — состояние инцидента, нужное для решений.
type Incident struct {
	ID string
	// Source — monitor или release: инцидент провала публикации закрывается
	// релизом, а не проверками наблюдения. Пусто читается как monitor.
	Source      string
	Status      Status
	Severity    Severity
	Occurrences int
	// OccurrencesAtMitigation — повторы инцидента в момент смягчения. Провалы
	// после смягчения считаются по отпечатку инцидента как разница с
	// Occurrences, а не по окружению: сбои другого инцидента того же
	// окружения его не возвращают. Ноль у смягчённого читается как
	// «смягчён на текущем числе повторов».
	OccurrencesAtMitigation int
	FirstSeen               time.Time
	ResolvedAt              *time.Time
	BugState                BugState
	// SkippedDaily — баг пропущен по суточному лимиту. Отметка липкая: store
	// только выставляет её и не сбрасывает.
	SkippedDaily bool
}

// Failure — проваленная проверка или публикация.
type Failure struct {
	Now time.Time
	// ConsecutiveFail — проваленных проверок подряд по окружению, включая
	// эту (для серьёзности).
	ConsecutiveFail int
}

// failsSinceMitigated — провалы этого инцидента после смягчения, включая
// текущий: все, а не только подряд, чтобы чередование провалов и успехов
// не держало инцидент в mitigated вечно.
func (inc Incident) failsSinceMitigated() int {
	at := inc.OccurrencesAtMitigation
	if at <= 0 {
		at = inc.Occurrences
	}
	return inc.Occurrences + 1 - at
}

// dailyWindowOpen — инцидент пропущен по суточному лимиту, и окно с первого
// появления не истекло. Нулевой FirstSeen считается попаданием в окно:
// лучше не создать баг, чем обойти лимит.
func (inc Incident) dailyWindowOpen(now time.Time, l Limits) bool {
	return inc.SkippedDaily && (inc.FirstSeen.IsZero() || now.Sub(inc.FirstSeen) < l.DailyWindow)
}

// Counts — счётчики проекта на момент решения (запросы store).
type Counts struct {
	// UnfinishedBugs — незавершённые задачи-баги из инцидентов проекта.
	UnfinishedBugs int
	// OpenedLast24h — инциденты проекта, открытые за 24 часа до этого сбоя.
	OpenedLast24h int
	// EscalationOpen — открыта эскалация OPS_LIMIT проекта.
	EscalationOpen bool
}

// Action — что сделать с инцидентом.
type Action string

const (
	ActionNew    Action = "new"
	ActionRepeat Action = "repeat"
	ActionReopen Action = "reopen"
	// ActionRegress — смягчённый инцидент снова падает: исправление не
	// помогло, инцидент возвращается в open.
	ActionRegress Action = "regress"
)

// BugAction — что сделать с задачей-багом.
type BugAction string

const (
	BugActionNone      BugAction = ""           // баг не нужен или ждёт релиза
	BugActionCreate    BugAction = "create"     // создать задачу-баг
	BugActionExists    BugAction = "exists"     // уже есть незавершённая
	BugActionSkipLimit BugAction = "skip_limit" // лимит: инцидент без бага
)

// Decision — решение по сбою.
type Decision struct {
	Action      Action
	Status      Status
	Severity    Severity
	Occurrences int
	Bug         BugAction
	// SkippedDaily — у инцидента есть отметка пропуска по суточному лимиту:
	// новая или унаследованная (липкая). Store выставляет её и никогда не
	// сбрасывает, повторы до конца окна бага не создают.
	SkippedDaily bool
	// Escalate — создать эскалацию OPS_LIMIT (нет открытой).
	Escalate bool
	// NeedsHuman — баг отменён человеком и автоматически не пересоздаётся:
	// инцидент получает отметку «нужен человек» (ручное закрытие или явное
	// «создать баг заново»).
	NeedsHuman bool
	// Describe — запросить описание у Инженера сопровождения: только при
	// открытии и переоткрытии. Повтор и возврат в open берут уже готовое
	// описание, модель не вызывают.
	Describe bool
}

func severityFor(fails int, prev Severity, l Limits) Severity {
	if prev == SeverityHigh || fails >= l.FailsForHigh {
		return SeverityHigh
	}
	return SeverityMedium
}

// bugFor заполняет решение по задаче-багу:
//   - незавершённая уже есть;
//   - отменена человеком: не пересоздаётся, нужен человек;
//   - лимит незавершённых багов;
//   - суточный лимит: для нового инцидента по счётчику, для известного по
//     сохранённой отметке, пока не прошло окно с первого появления;
//   - иначе создать.
//
// Сверх лимита эскалация, если нет открытой.
func bugFor(d *Decision, existing *Incident, f Failure, c Counts, l Limits) {
	state := BugNone
	if existing != nil {
		state = existing.BugState
	}
	switch {
	case state.Unfinished():
		d.Bug = BugActionExists
	case state == BugCancelledByHuman:
		d.Bug, d.NeedsHuman = BugActionNone, true
	case c.UnfinishedBugs >= l.MaxOpenBugs:
		d.Bug, d.Escalate = BugActionSkipLimit, !c.EscalationOpen
	case d.Action == ActionNew && c.OpenedLast24h >= l.MaxIncidentsPerDay,
		existing != nil && existing.dailyWindowOpen(f.Now, l):
		d.Bug, d.SkippedDaily, d.Escalate = BugActionSkipLimit, true, !c.EscalationOpen
	default:
		d.Bug = BugActionCreate
	}
}

// Decide решает судьбу сбоя по существующему инциденту с тем же
// отпечатком (nil, если его нет):
//   - открытый инцидент: повтор. Если у него нет незавершённого бага и
//     баг не ждёт релиза (пропущен по лимиту или отменён системой), баг
//     создаётся по правилам bugFor с готовым описанием;
//   - смягчённый инцидент: повтор, а после FailsForHigh провалов после
//     смягчения (всех, не только подряд) возврат в open: исправление не
//     помогло, новый баг по правилам bugFor. Разовый сбой после выкатки
//     инцидент не возвращает;
//   - закрытый не больше окна назад: переоткрытие;
//   - иначе новый инцидент.
//
// Описание модель пишет только при открытии и переоткрытии.
func Decide(existing *Incident, f Failure, c Counts, l Limits) Decision {
	if existing != nil && existing.Status.Active() {
		d := Decision{
			Action: ActionRepeat, Status: existing.Status,
			Severity:     severityFor(f.ConsecutiveFail, existing.Severity, l),
			Occurrences:  existing.Occurrences + 1,
			SkippedDaily: existing.SkippedDaily,
		}
		switch {
		case existing.Status == StatusMitigated && existing.failsSinceMitigated() >= l.FailsForHigh:
			d.Action, d.Status = ActionRegress, StatusOpen
			bugFor(&d, existing, f, c, l)
		case existing.Status == StatusOpen && existing.BugState != BugDone:
			bugFor(&d, existing, f, c, l)
		}
		return d
	}
	d := Decision{Action: ActionNew, Status: StatusOpen, Severity: severityFor(f.ConsecutiveFail, "", l), Occurrences: 1, Describe: true}
	var prev *Incident
	if existing != nil && existing.ResolvedAt != nil && f.Now.Sub(*existing.ResolvedAt) <= l.ReopenWindow {
		// Переоткрытие — та же строка: отметка суточного лимита остаётся.
		d.Action, d.Occurrences, d.SkippedDaily = ActionReopen, existing.Occurrences+1, existing.SkippedDaily
		prev = existing
	}
	bugFor(&d, prev, f, c, l)
	return d
}

// Transition — смена статуса инцидента.
type Transition struct {
	From, To   Status
	Resolution Resolution
	// CancelBug — отменить не начатую задачу-баг (самоустранение).
	CancelBug bool
	// OccurrencesAtMitigation — при переходе в mitigated число повторов на
	// этот момент: store пишет его в инцидент из перехода, от него Decide
	// считает провалы после смягчения.
	OccurrencesAtMitigation int
}

// selfResolvable — открытый инцидент без бага в работе или ожидающего
// релиза: его можно закрыть как самоустранившийся.
func selfResolvable(b BugState) bool {
	return b == BugNone || b == BugNotStarted || b == BugCancelled || b == BugCancelledByHuman
}

// ReleaseFacts — что известно об успешном релизе окружения инцидента.
type ReleaseFacts struct {
	// BugIncluded — в состав вошла задача-баг инцидента.
	BugIncluded bool
	// NewVersion — релиз создан после провала публикации, и его версия не
	// равна prev_version упавшей публикации (версии, на которую
	// откатились). Ручная выкатка старой версии провал не исправляет.
	NewVersion bool
}

// OnRelease — успешный релиз окружения инцидента.
//   - Инцидент наблюдения открыт и баг в релизе: смягчён, закрытие после
//     подтверждающих проверок.
//   - Инцидент провала публикации: баг в релизе закрывает его как
//     исправленный, минуя смягчение (проверки наблюдения его не закрывают).
//     Релиз новой версии без бага закрывает его как самоустранившийся,
//     если бага в работе нет, не начатый баг отменяется.
func OnRelease(inc Incident, r ReleaseFacts) (Transition, bool) {
	if inc.Source == SourceRelease {
		if !inc.Status.Active() {
			return Transition{}, false
		}
		if r.BugIncluded {
			return Transition{From: inc.Status, To: StatusResolved, Resolution: ResolutionFixed}, true
		}
		if inc.Status == StatusOpen && r.NewVersion && selfResolvable(inc.BugState) {
			return Transition{From: StatusOpen, To: StatusResolved, Resolution: ResolutionSelf, CancelBug: inc.BugState == BugNotStarted}, true
		}
		return Transition{}, false
	}
	if inc.Status != StatusOpen || !r.BugIncluded {
		return Transition{}, false
	}
	return Transition{From: StatusOpen, To: StatusMitigated, OccurrencesAtMitigation: inc.Occurrences}, true
}

// OnCheckOK — успешная проверка наблюдения. consecutiveOK для смягчённого
// инцидента считается после смягчения. Смягчённый закрывается как
// исправленный. Открытый без бага, с не начатым или отменённым багом
// закрывается как самоустранившийся с отменой не начатого. Открытый с
// багом в работе или смерженным, но не выпущенным, ждёт релиза. Инцидент
// провала публикации проверками не закрывается.
func OnCheckOK(inc Incident, consecutiveOK int, l Limits) (Transition, bool) {
	if consecutiveOK < l.OKToResolve || inc.Source == SourceRelease {
		return Transition{}, false
	}
	switch inc.Status {
	case StatusMitigated:
		return Transition{From: StatusMitigated, To: StatusResolved, Resolution: ResolutionFixed}, true
	case StatusOpen:
		if !selfResolvable(inc.BugState) {
			return Transition{}, false
		}
		return Transition{From: StatusOpen, To: StatusResolved, Resolution: ResolutionSelf, CancelBug: inc.BugState == BugNotStarted}, true
	}
	return Transition{}, false
}

// Ошибки ручного закрытия (api-contract: 400 и 409 incident_resolved).
var (
	ErrAlreadyResolved = errors.New("инцидент уже закрыт")
	ErrBadComment      = errors.New("комментарий: от 1 до 500 символов")
)

// ResolveManual — ручное закрытие владельцем с комментарием.
func ResolveManual(inc Incident, comment string) (Transition, error) {
	n := utf8.RuneCountInString(strings.TrimSpace(comment))
	if n == 0 || n > 500 {
		return Transition{}, ErrBadComment
	}
	if inc.Status == StatusResolved {
		return Transition{}, ErrAlreadyResolved
	}
	return Transition{From: inc.Status, To: StatusResolved, Resolution: ResolutionManual}, nil
}
