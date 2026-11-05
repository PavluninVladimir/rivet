package architecture

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
)

// Снимок архитектуры для control plane (спека backend/project-architecture
// «Снимок архитектуры проекта»): описание читается через API хостинга по
// SHA, где листинга каталогов нет. Поэтому файлы берутся только по путям,
// которые выводятся из манифеста и модели, а изменения загружаются по
// известным идентификаторам.

// Статусы снимка, которые определяет пакет. Статус error (описание не
// прочитано) ставит вызывающий по ошибке BuildSnapshot.
const (
	SnapshotAbsent  = "absent"
	SnapshotOK      = "ok"
	SnapshotInvalid = "invalid"
)

// SystemInfo — описываемая система из манифеста.
type SystemInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SnapshotViews — схемы C4 снимка по уровням.
type SnapshotViews struct {
	Context    View            `json:"context"`
	Containers []SystemView    `json:"containers"`
	Components []ContainerView `json:"components"`
	Deployment *View           `json:"deployment"`
}

// SnapshotData — данные корректного описания (api-contract ArchitectureData).
type SnapshotData struct {
	System     SystemInfo     `json:"system"`
	Principles []Principle    `json:"principles"`
	Elements   []C4Element    `json:"elements"`
	Views      SnapshotViews  `json:"views"`
	HLD        string         `json:"hld"`
	DSL        string         `json:"dsl"`
	ERD        ERD            `json:"erd"`
	Scenarios  []ScenarioView `json:"scenarios"`
	Decisions  []Decision     `json:"decisions"`
	// Links — элементы, упомянутые в решениях и сценариях: id → номера
	// решений и идентификаторы сценариев. Только элементы со связями.
	Links map[string]ElementLinks `json:"links"`
}

// ElementLinks — решения и сценарии, в которых упомянут элемент.
type ElementLinks struct {
	Decisions []int    `json:"decisions"`
	Scenarios []string `json:"scenarios"`
}

// Лимиты чтения снимка: больше сценариев и решений описание через API
// хостинга не читает, снимок получает статус error с причиной.
const (
	SnapshotMaxScenarios = 100
	SnapshotMaxDecisions = 300
)

// Snapshot — результат чтения описания: статус, дефекты проверки и данные.
// Data заполнено только при статусе ok. При ok в Defects могут остаться
// предупреждения.
type Snapshot struct {
	Status  string
	Defects Defects
	Data    *SnapshotData
}

// SnapshotOptions — что вызывающий знает об описании сверх файлов.
type SnapshotOptions struct {
	// Changes — идентификаторы изменений в работе. nil: список неизвестен,
	// и раздел изменений в HLD говорит об этом прямо. Пустой список: изменений нет.
	Changes []string
	// Dir — каталог описания от корня репозитория: префикс путей файлов
	// решений. Пусто — DefaultDir.
	Dir string
}

// BuildSnapshot читает описание без листинга каталогов, проверяет его и
// строит модель C4, схемы, HLD, Structurizr DSL, ERD, сценарии и журнал
// решений тем же кодом, что генерация представлений. Ошибка означает, что
// описание не прочитано. Превышение лимитов чтения — ошибка ErrLimit.
func BuildSnapshot(fsys fs.FS, opt SnapshotOptions) (*Snapshot, error) {
	r := newReporter()
	m, err := loadWith(fsys, r, loadOptions{maxScenarios: SnapshotMaxScenarios, maxDecisions: SnapshotMaxDecisions})
	if errors.Is(err, ErrAbsent) {
		return &Snapshot{Status: SnapshotAbsent, Defects: Defects{}}, nil
	}
	if err != nil {
		return nil, err
	}
	if m == nil {
		return &Snapshot{Status: SnapshotInvalid, Defects: r.result()}, nil
	}
	m.check(r, checkOptions{})
	ds := r.result()
	if ds.HasErrors() {
		return &Snapshot{Status: SnapshotInvalid, Defects: ds}, nil
	}
	ch := hldChanges{known: opt.Changes != nil}
	ids := slices.Clone(opt.Changes)
	slices.Sort(ids)
	for _, id := range slices.Compact(ids) {
		if !ValidChangeID(id) {
			continue
		}
		cr := newReporter()
		x, err := loadChangeOpt(fsys, m, id, cr, true)
		if err != nil {
			return nil, err
		}
		if x != nil && !cr.result().HasErrors() {
			ch.list = append(ch.list, x)
		}
	}
	c := m.C4()
	data := &SnapshotData{
		System:     SystemInfo{Name: c.byID[DescribedSystemID].Name, Description: c.byID[DescribedSystemID].Description},
		Principles: slices.Clone(m.Manifest.Principles),
		Elements:   slices.Clone(c.Elements),
		Views: SnapshotViews{
			Context:    c.contextScene().view(nil),
			Containers: []SystemView{},
			Components: []ContainerView{},
		},
		HLD: renderHLD(m, c, ch),
		DSL: renderDSL(c),
	}
	if data.Principles == nil {
		data.Principles = []Principle{}
	}
	for i := range data.Elements {
		data.Elements[i].Alias = c.MermaidAlias(data.Elements[i].ID)
	}
	for _, id := range c.systemsWithContainers() {
		data.Views.Containers = append(data.Views.Containers, SystemView{SystemID: id, View: c.containerScene(id).view(nil)})
	}
	for _, id := range c.containersWithComponents() {
		data.Views.Components = append(data.Views.Components, ContainerView{ContainerID: id, View: c.componentScene(id).view(nil)})
	}
	if text, ok := c.deployment(); ok {
		data.Views.Deployment = &View{Title: "Развёртывание: " + data.System.Name, Mermaid: text}
	}
	m.snapshotDocs(c, data, opt.Dir)
	return &Snapshot{Status: SnapshotOK, Defects: ds, Data: data}, nil
}

// snapshotDocs заполняет ERD, сценарии, решения и индекс связей.
func (m *Model) snapshotDocs(c *C4Model, data *SnapshotData, dir string) {
	if dir == "" {
		dir = DefaultDir
	}
	prefix := path.Clean(dir) + "/"
	data.ERD = m.BuildERD(c)
	data.Scenarios = []ScenarioView{}
	data.Decisions = []Decision{}
	data.Links = map[string]ElementLinks{}
	link := func(id string, fn func(*ElementLinks)) {
		l := data.Links[id]
		fn(&l)
		data.Links[id] = l
	}
	for _, sc := range m.scenarios {
		data.Scenarios = append(data.Scenarios, m.scenarioView(sc, c))
		for _, p := range scenarioParticipants(sc) {
			link(p, func(l *ElementLinks) { l.Scenarios = appendUnique(l.Scenarios, sc.ID) })
		}
		if sc.Process != "" {
			link(sc.Process, func(l *ElementLinks) { l.Scenarios = appendUnique(l.Scenarios, sc.ID) })
		}
	}
	for _, d := range m.decisions {
		data.Decisions = append(data.Decisions, decisionView(d, prefix))
		for _, id := range d.Elements {
			link(id, func(l *ElementLinks) {
				if !slices.Contains(l.Decisions, d.Number) {
					l.Decisions = append(l.Decisions, d.Number)
				}
			})
		}
	}
	for id, l := range data.Links {
		if l.Decisions == nil {
			l.Decisions = []int{}
		}
		if l.Scenarios == nil {
			l.Scenarios = []string{}
		}
		slices.Sort(l.Decisions)
		slices.Sort(l.Scenarios)
		data.Links[id] = l
	}
}

// LoadChangeNoList загружает изменение по известному идентификатору без
// листинга каталогов: дельты по списку viewpoint'ов описания, целевые
// процессы по ссылкам разрывов. Проверки, которым нужен листинг (лишние
// файлы дельт и процессов), в этом режиме не выполняются.
func LoadChangeNoList(fsys fs.FS, id string) (*Change, Defects, error) {
	if !ValidChangeID(id) {
		return nil, nil, fmt.Errorf("%q: %s", id, ChangeIDProblem(id))
	}
	r := newReporter()
	cur, err := load(fsys, r)
	if err != nil {
		return nil, nil, err
	}
	if cur == nil {
		return nil, r.result(), nil
	}
	ch, err := loadChangeOpt(fsys, cur, id, r, true)
	if err != nil {
		return nil, nil, err
	}
	return ch, r.result(), nil
}
