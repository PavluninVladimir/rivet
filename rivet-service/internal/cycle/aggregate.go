package cycle

import (
	"fmt"
	"sort"
	"time"
)

// Лимиты агрегата (спека «Агрегат цикла»).
const (
	MaxItems     = 10
	MaxArtifacts = 5
)

// Периоды агрегата.
const (
	PeriodToday   = "today"
	Period7d      = "7d"
	Period30d     = "30d"
	DefaultPeriod = Period7d
)

// ValidPeriod — допустимое значение параметра period.
func ValidPeriod(p string) bool { return p == PeriodToday || p == Period7d || p == Period30d }

// PeriodStart — начало периода: today — полночь текущих суток в зоне loc,
// 7d и 30d — скользящее окно от now.
func PeriodStart(period string, now time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	switch period {
	case PeriodToday:
		n := now.In(loc)
		return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, loc), nil
	case Period7d:
		return now.Add(-7 * 24 * time.Hour), nil
	case Period30d:
		return now.Add(-30 * 24 * time.Hour), nil
	}
	return time.Time{}, fmt.Errorf("period: ожидается today, 7d или 30d, получено %q", period)
}

// ─── агенты ──────────────────────────────────────────────────────────────

// Состояния агента фазы.
const (
	AgentWorking  = "working"
	AgentIdle     = "idle"
	AgentDisabled = "disabled"
	AgentNoRunner = "no_runner"
)

// Work — текущая работа агента.
type Work struct {
	Kind   string `json:"kind"` // task | epic | deployment | incident | analysis | release
	ID     string `json:"id"`
	Num    int    `json:"num,omitempty"`
	Title  string `json:"title"`
	Action string `json:"action"`
	// Phase — фаза этой работы (вычисляет вызывающий теми же функциями).
	// Пусто: агент показывается в фазе своей роли.
	Phase Phase `json:"-"`
	// ProjectID — проект работы: работа в другом проекте показывается без
	// названия и ссылки.
	ProjectID string `json:"-"`
}

// Elsewhere — работа агента в другом проекте: агент занят, но что он
// делает, зрителю этого проекта не показывается.
func Elsewhere(action string) *Work {
	return &Work{Kind: "other", Title: "работа в другом проекте", Action: action}
}

// Agent — агент команды на входе агрегата.
type Agent struct {
	ID           string
	Name         string
	RoleID       string
	RoleTitle    string
	Model        string
	ConnectionID string
	Enabled      bool
	// Capability — capability шаблона роли; пусто у шаблонов, которые
	// выполняет control plane (analyst, lead, devops, sre).
	Capability string
	// RunnerOnline — есть online-runner, подходящий профилю и модели агента
	// (считает функция подбора runner'а из add-agent-roles).
	RunnerOnline bool
	RunnerID     string
	Current      *Work
}

// builtinPhase — фаза встроенного шаблона роли.
var builtinPhase = map[string]Phase{}

func init() {
	for _, ph := range phases {
		builtinPhase[ph.Role] = ph.ID
	}
}

// RolePhase — фаза агента без текущей работы: встроенный шаблон — его фаза,
// собственный — по capability (coding → разработка, review → ревью,
// explore → архитектура). ok=false: агент только в общем списке команды.
func RolePhase(roleID, capability string) (Phase, bool) {
	if ph, ok := builtinPhase[roleID]; ok {
		return ph, true
	}
	switch capability {
	case "coding":
		return Development, true
	case "review":
		return Review, true
	case "explore":
		return Architecture, true
	}
	return "", false
}

// AgentState — состояние агента: отключён, занят, без подходящего runner'а
// (только у шаблонов с capability) или свободен.
func AgentState(a Agent) string {
	switch {
	case !a.Enabled:
		return AgentDisabled
	case a.Current != nil:
		return AgentWorking
	case a.Capability != "" && !a.RunnerOnline:
		return AgentNoRunner
	}
	return AgentIdle
}

// agentPhase — фаза агента на кольце. Агент встроенного шаблона всегда в
// фазе своей роли: Разработчик на шаге prompt после review остаётся в
// разработке, а его работа видна в current. Агент собственного шаблона —
// в фазе текущей работы, без неё по capability.
func agentPhase(a Agent) (Phase, bool) {
	if ph, ok := builtinPhase[a.RoleID]; ok {
		return ph, true
	}
	if a.Current != nil && a.Current.Phase != "" {
		return a.Current.Phase, true
	}
	return RolePhase(a.RoleID, a.Capability)
}

// ─── вход и выход агрегата ───────────────────────────────────────────────

// Ref — ссылка артефакта: представление консоли или внешний URL. View:
// epic, task, release, incident — с идентификатором сущности; discovery —
// «Исследование», ID версии исследования или пусто; architecture —
// «Архитектура» проекта; adr — решение в «Архитектуре», ID его номер.
type Ref struct {
	View string `json:"view,omitempty"`
	ID   string `json:"id,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Artifact — артефакт фазы.
type Artifact struct {
	Phase Phase     `json:"-"`
	Kind  string    `json:"kind"` // interview | need | architecture | adr | plan | pr | check | review | release | incident
	ID    string    `json:"id"`
	Title string    `json:"title"`
	At    time.Time `json:"at"`
	Ref   Ref       `json:"ref"`
	// Order — порядок артефактов одного момента: меньше раньше. Потребности
	// одной версии идут по важности, решения одной даты новыми номерами.
	Order int `json:"-"`
}

// LastRelease — последний релиз проекта для сводки.
type LastRelease struct {
	ID        string    `json:"id"`
	EnvID     string    `json:"env_id"`
	EnvName   string    `json:"env_name"`
	Version   string    `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// Input — всё, что store прочитал в одной согласованной транзакции.
type Input struct {
	ProjectID   string
	Period      string
	GeneratedAt time.Time

	Tasks       []Task
	Epics       []Epic
	Deployments []Deployment
	Incidents   []Incident
	Materials   []Material
	LastRun     LastRun
	Analyses    []Analysis
	Envs        []EnvMonitor
	Cut         ReleaseCut

	Agents    []Agent
	Artifacts []Artifact
	// DonePeriod — завершённые за период по фазам (считает store по журналу).
	DonePeriod map[Phase]int
	// Available — есть ли смежный механизм и данные для фаз discovery,
	// architecture и operations. Отсутствие ключа у этих фаз означает «нет
	// данных». У остальных фаз ключ не читается.
	Available map[Phase]bool

	LastRelease *LastRelease
	SpendUSD    *float64
	Tokens      int64
}

// Counters — счётчики фазы.
type Counters struct {
	Active     int `json:"active"`
	Waiting    int `json:"waiting"`
	NeedsHuman int `json:"needs_human"`
	DonePeriod int `json:"done_period"`
}

// Item — активный элемент фазы.
type Item struct {
	Kind  string `json:"kind"` // task | epic | deployment | incident | analysis | material | environment
	ID    string `json:"id"`
	Num   int    `json:"num,omitempty"`
	Title string `json:"title"`
	// EpicID — Epic задачи или сам Epic (переход на дашборд).
	EpicID string    `json:"epic_id,omitempty"`
	State  ItemState `json:"state"`
	Badge  string    `json:"badge,omitempty"`
	Since  time.Time `json:"since"`
}

// AgentView — агент фазы в ответе.
type AgentView struct {
	AgentID      string `json:"agent_id"`
	Name         string `json:"name"`
	RoleID       string `json:"role_id"`
	RoleTitle    string `json:"role_title"`
	Model        string `json:"model,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
	State        string `json:"state"`
	Current      *Work  `json:"current"`
	RunnerID     string `json:"runner_id,omitempty"`
}

// PhaseView — фаза в агрегате.
type PhaseView struct {
	ID           Phase       `json:"id"`
	Title        string      `json:"title"`
	Role         string      `json:"role"`
	NoData       bool        `json:"no_data"`
	Hint         string      `json:"hint,omitempty"`
	Counters     Counters    `json:"counters"`
	TemplateOnly bool        `json:"template_only"`
	Agents       []AgentView `json:"agents"`
	Items        []Item      `json:"items"`
	Artifacts    []Artifact  `json:"artifacts"`
}

// Summary — сводка в центре кольца. AgentsTotal — все агенты команды,
// включая отключённых. IncidentsOpen — активные инциденты: открытые и
// смягчённые.
type Summary struct {
	AgentsTotal   int          `json:"agents_total"`
	AgentsWorking int          `json:"agents_working"`
	IncidentsOpen int          `json:"incidents_open"`
	LastRelease   *LastRelease `json:"last_release"`
	SpendUSD      *float64     `json:"spend_usd"`
	Tokens        int64        `json:"tokens"`
}

// View — агрегат цикла (CycleView из api-contract).
type View struct {
	ProjectID   string      `json:"project_id"`
	Period      string      `json:"period"`
	GeneratedAt time.Time   `json:"generated_at"`
	Summary     Summary     `json:"summary"`
	Phases      []PhaseView `json:"phases"`
}

// optional — фазы, которые зависят от смежных механизмов, и подсказка, что
// включить, когда данных нет.
var optional = map[Phase]string{
	Discovery:    "загрузите интервью пользователей в «Исследование»",
	Architecture: "опишите архитектуру проекта",
	Operations:   "включите наблюдение окружения",
}

// Build собирает агрегат: относит сущности к фазам, считает счётчики,
// сортирует элементы (сначала требующие человека, затем по времени входа в
// фазу) и обрезает до 10 элементов и 5 артефактов. Результат зависит только
// от входа.
func Build(in Input) View {
	period := in.Period
	if period == "" {
		period = DefaultPeriod
	}
	byPhase := map[Phase][]Item{}
	add := func(p Placement, it Item) {
		if !p.In() {
			return
		}
		it.State, it.Badge = p.State, p.Badge
		byPhase[p.Phase] = append(byPhase[p.Phase], it)
	}
	for _, t := range in.Tasks {
		add(PhaseOfTask(t, in.Cut), Item{Kind: "task", ID: t.ID, Num: t.Num, Title: t.Title, EpicID: t.EpicID, Since: t.Since})
	}
	for _, e := range in.Epics {
		add(PhaseOfEpic(e), Item{Kind: "epic", ID: e.ID, Title: e.Title, EpicID: e.ID, Since: e.Since})
	}
	for _, d := range in.Deployments {
		add(PhaseOfDeployment(d), Item{Kind: "deployment", ID: d.ID, Title: d.Title, Since: d.Since})
	}
	envWithIncident := map[string]bool{}
	incidentsOpen := 0
	for _, i := range in.Incidents {
		p := PhaseOfIncident(i)
		if p.In() {
			envWithIncident[i.EnvID] = true
			incidentsOpen++ // открытые и смягчённые
		}
		add(p, Item{Kind: "incident", ID: i.ID, Title: i.Title, Since: i.Since})
	}
	for _, e := range in.Envs {
		if envWithIncident[e.EnvID] {
			continue
		}
		add(PhaseOfEnv(e), Item{Kind: "environment", ID: e.EnvID, Title: e.Name, Since: e.Since})
	}
	for _, m := range in.Materials {
		add(PhaseOfMaterial(m, in.LastRun), Item{Kind: "material", ID: m.ID, Title: m.Title, Since: m.Since})
	}
	for _, a := range in.Analyses {
		add(PhaseOfAnalysis(a), Item{Kind: "analysis", ID: a.ID, Title: a.Title, Since: a.Since})
	}

	agentsBy := map[Phase][]AgentView{}
	hasRole := map[string]bool{}
	sum := Summary{AgentsTotal: len(in.Agents), IncidentsOpen: incidentsOpen, LastRelease: in.LastRelease, SpendUSD: in.SpendUSD, Tokens: in.Tokens}
	for _, a := range in.Agents {
		if a.Enabled {
			hasRole[a.RoleID] = true
		}
		st := AgentState(a)
		if st == AgentWorking {
			sum.AgentsWorking++
		}
		ph, ok := agentPhase(a)
		if !ok {
			continue
		}
		agentsBy[ph] = append(agentsBy[ph], AgentView{
			AgentID: a.ID, Name: a.Name, RoleID: a.RoleID, RoleTitle: a.RoleTitle,
			Model: a.Model, ConnectionID: a.ConnectionID, State: st, Current: a.Current, RunnerID: a.RunnerID,
		})
	}

	artBy := map[Phase][]Artifact{}
	for _, a := range in.Artifacts {
		artBy[a.Phase] = append(artBy[a.Phase], a)
	}

	out := View{ProjectID: in.ProjectID, Period: period, GeneratedAt: in.GeneratedAt, Summary: sum}
	for _, info := range phases {
		pv := PhaseView{
			ID: info.ID, Title: info.Title, Role: info.Role,
			TemplateOnly: !hasRole[info.Role],
			Agents:       agentsBy[info.ID],
			Items:        []Item{},
			Artifacts:    []Artifact{},
		}
		if pv.Agents == nil {
			pv.Agents = []AgentView{}
		}
		sort.SliceStable(pv.Agents, func(i, j int) bool {
			if pv.Agents[i].Name != pv.Agents[j].Name {
				return pv.Agents[i].Name < pv.Agents[j].Name
			}
			return pv.Agents[i].AgentID < pv.Agents[j].AgentID
		})
		items := byPhase[info.ID]
		// «Нет данных» только когда фазе действительно нечего показать:
		// Epic в исследовании, инцидент провала публикации или артефакт
		// видны, даже если смежный механизм ещё не выкачен.
		if hint, opt := optional[info.ID]; opt && !in.Available[info.ID] && len(items) == 0 && in.DonePeriod[info.ID] == 0 && len(artBy[info.ID]) == 0 {
			pv.NoData, pv.Hint = true, hint
			out.Phases = append(out.Phases, pv)
			continue
		}
		for _, it := range items {
			switch it.State {
			case StateActive:
				pv.Counters.Active++
			case StateWaiting:
				pv.Counters.Waiting++
			case StateNeedsHuman:
				pv.Counters.NeedsHuman++
			}
		}
		pv.Counters.DonePeriod = in.DonePeriod[info.ID]
		sortItems(items)
		if len(items) > MaxItems {
			items = items[:MaxItems]
		}
		pv.Items = append(pv.Items, items...)
		arts := artBy[info.ID]
		sort.SliceStable(arts, func(i, j int) bool {
			if !arts[i].At.Equal(arts[j].At) {
				return arts[i].At.After(arts[j].At)
			}
			if arts[i].Order != arts[j].Order {
				return arts[i].Order < arts[j].Order
			}
			if arts[i].Kind != arts[j].Kind {
				return arts[i].Kind < arts[j].Kind
			}
			return arts[i].ID < arts[j].ID
		})
		if len(arts) > MaxArtifacts {
			arts = arts[:MaxArtifacts]
		}
		pv.Artifacts = append(pv.Artifacts, arts...)
		out.Phases = append(out.Phases, pv)
	}
	return out
}

// sortItems: сначала требующие человека, затем по времени входа в фазу
// (дольше всех ждущие первыми), при равенстве по виду и идентификатору —
// порядок не зависит от порядка строк из базы.
func sortItems(items []Item) {
	sort.SliceStable(items, func(i, j int) bool {
		hi, hj := items[i].State == StateNeedsHuman, items[j].State == StateNeedsHuman
		if hi != hj {
			return hi
		}
		if !items[i].Since.Equal(items[j].Since) {
			return items[i].Since.Before(items[j].Since)
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		return items[i].ID < items[j].ID
	})
}
