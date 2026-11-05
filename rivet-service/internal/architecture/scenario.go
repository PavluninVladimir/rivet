package architecture

import (
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Сценарии взаимодействия (спеки «Сценарии взаимодействия» и
// «Sequence-представления»): файлы scenarios/<id>.yaml по индексу
// манифеста, участники — элементы модели и описываемая система, шаги и
// блоки alt/opt/loop.

// Виды шагов сценария.
const (
	StepSync   = "sync"
	StepAsync  = "async"
	StepReturn = "return"
	StepSelf   = "self"
)

// maxScenarioDepth — предельная вложенность блоков: глубже схема перестаёт
// читаться.
const maxScenarioDepth = 4

// scenarioParticipantTypes — какие элементы модели могут быть участниками.
var scenarioParticipantTypes = []string{TypeActor, TypeRole, TypeComponent, TypeSystemSoftware, TypeNode}

// Scenario — сценарий взаимодействия.
type Scenario struct {
	ID           string         `json:"id"`
	Title        string         `json:"title"`
	Description  string         `json:"description,omitempty"`
	Process      string         `json:"process,omitempty"`
	Participants []string       `json:"participants,omitempty"`
	Steps        []ScenarioStep `json:"steps"`
	// File — путь файла сценария от корня описания.
	File string `json:"-"`
	// Raw — исходный текст файла: применение изменения переносит его как есть.
	Raw []byte `json:"-"`
}

// ScenarioStep — шаг сценария или блок: заполнено одно из сообщения
// (From, To, Message, Kind), Alt, Opt или Loop.
type ScenarioStep struct {
	From    string           `json:"from,omitempty"`
	To      string           `json:"to,omitempty"`
	Message string           `json:"message,omitempty"`
	Kind    string           `json:"kind,omitempty"`
	Alt     []ScenarioBranch `json:"alt,omitempty"`
	Opt     *ScenarioBranch  `json:"opt,omitempty"`
	Loop    *ScenarioBranch  `json:"loop,omitempty"`
}

// ScenarioBranch — ветка блока: условие и шаги.
type ScenarioBranch struct {
	When  string         `json:"when,omitempty"`
	Steps []ScenarioStep `json:"steps"`
}

func (s ScenarioStep) isMessage() bool { return s.Alt == nil && s.Opt == nil && s.Loop == nil }

func scenarioFile(dir, id string) string { return dir + ScenariosDir + "/" + id + ".yaml" }

// loadScenarios читает сценарии из индекса. dir — префикс каталога (пусто
// для описания, changes/<id>/ для изменения). Отсутствие файла — дефект,
// другие ошибки чтения возвращаются вызывающему.
func loadScenarios(fsys fs.FS, ids []string, dir, indexFile, indexPtr string, r *reporter) ([]*Scenario, error) {
	var out []*Scenario
	seen := map[string]bool{}
	for i, id := range ids {
		loc := fmt.Sprintf("%s/%d", indexPtr, i)
		switch {
		case !idPattern.MatchString(id):
			r.errorf("scenario.id", indexFile, loc, "идентификатор сценария %q не подходит под шаблон %s", id, idPattern)
			continue
		case seen[id]:
			r.errorf("scenario.id", indexFile, loc, "сценарий %s указан в индексе повторно", id)
			continue
		}
		seen[id] = true
		file := scenarioFile(dir, id)
		raw, err := fs.ReadFile(fsys, file)
		if errors.Is(err, fs.ErrNotExist) {
			r.errorf("scenario.missing", indexFile, loc, "нет файла сценария %s", file)
			continue
		}
		if err != nil {
			return nil, err
		}
		_, v, err := parseYAML(raw)
		if err != nil {
			r.errorf("yaml.parse", file, "", "файл не разбирается: %v", err)
			continue
		}
		reportSchema(r, scenarioSchema, v, func(p []string) (string, string) { return file, formatPointer(p) })
		sc := &Scenario{}
		decodeValue(v, sc)
		if sc.ID != id {
			r.errorf("scenario.id", file, "/id", "id %q не совпадает с именем в индексе %s", sc.ID, id)
		}
		sc.ID, sc.File, sc.Raw = id, file, raw
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// participantName — название участника для схемы и таблиц.
func (m *Model) participantName(id string) string {
	if id == DescribedSystemID {
		return cmp(strings.TrimSpace(m.Manifest.System), "Система")
	}
	if e := m.element(id); e != nil {
		return elementLabel(e)
	}
	return id
}

func (m *Model) participantPerson(id string) bool {
	e := m.element(id)
	return e != nil && (e.Type == TypeActor || e.Type == TypeRole)
}

// scenarioParticipants — участники в порядке списка participants, затем
// по первому появлению в шагах.
func scenarioParticipants(sc *Scenario) []string {
	var out []string
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, p := range sc.Participants {
		add(p)
	}
	var walk func([]ScenarioStep)
	walk = func(steps []ScenarioStep) {
		for _, s := range steps {
			if s.isMessage() {
				add(s.From)
				add(s.To)
			}
			for _, b := range s.Alt {
				walk(b.Steps)
			}
			if s.Opt != nil {
				walk(s.Opt.Steps)
			}
			if s.Loop != nil {
				walk(s.Loop.Steps)
			}
		}
	}
	walk(sc.Steps)
	return out
}

// checkScenarios — правила scenario.* по сценариям модели.
func (m *Model) checkScenarios(r *reporter, opt checkOptions) {
	if len(m.scenarios) == 0 {
		return
	}
	c := m.C4()
	for _, sc := range m.scenarios {
		m.checkScenario(sc, c, r, opt)
	}
}

func (m *Model) checkScenario(sc *Scenario, c *C4Model, r *reporter, opt checkOptions) {
	valid := map[string]bool{}
	ref := func(id, loc string) bool {
		if id == "" {
			return false
		}
		if id == DescribedSystemID {
			return true
		}
		if opt.removed[id] {
			r.errorf("scenario.unknown-participant", sc.File, loc, "участник %s удаляется изменением", id)
			return false
		}
		e := m.element(id)
		if e == nil {
			r.errorf("scenario.unknown-participant", sc.File, loc, "участника %s нет в модели", id)
			return false
		}
		allowed := false
		for _, t := range scenarioParticipantTypes {
			if e.Type == t {
				allowed = true
			}
		}
		if !allowed {
			r.errorf("scenario.participant-type", sc.File, loc,
				"участник %s типа %s: участниками бывают actor, role, component, system-software, node и система", id, e.Type)
			return false
		}
		return true
	}
	for i, p := range sc.Participants {
		valid[p] = ref(p, fmt.Sprintf("/participants/%d", i))
	}
	if sc.Process != "" {
		if e := m.element(sc.Process); e == nil || e.Type != TypeProcess || opt.removed[sc.Process] {
			r.errorf("scenario.process", sc.File, "/process", "процесса %s нет в модели", sc.Process)
		}
	}
	if len(sc.Steps) == 0 {
		r.errorf("scenario.empty", sc.File, "/steps", "в сценарии нет шагов")
	}
	check := func(id, loc string) bool {
		if v, ok := valid[id]; ok {
			return v
		}
		valid[id] = ref(id, loc)
		return valid[id]
	}
	var walk func(steps []ScenarioStep, ptr string, depth int)
	walk = func(steps []ScenarioStep, ptr string, depth int) {
		for i, s := range steps {
			loc := ptr + "/" + strconv.Itoa(i)
			if s.isMessage() {
				okFrom := check(s.From, loc+"/from")
				okTo := check(s.To, loc+"/to")
				if s.Kind == StepSelf && s.From != s.To {
					r.errorf("scenario.self", sc.File, loc, "шаг вида self должен идти от участника к нему же, а идёт от %s к %s", s.From, s.To)
					continue
				}
				if okFrom && okTo && s.From != s.To && !m.participantsRelated(c, s.From, s.To) {
					r.warnf("scenario.no-relation", sc.File, loc,
						"между %s и %s нет связи в модели: проверь, что шаг описан верно, или добавь связь", s.From, s.To)
				}
				continue
			}
			if depth >= maxScenarioDepth {
				r.errorf("scenario.depth", sc.File, loc, "блоки вложены глубже %d уровней", maxScenarioDepth)
				continue
			}
			var branches []ScenarioBranch
			var key string
			switch {
			case s.Alt != nil:
				branches, key = s.Alt, "alt"
				if len(s.Alt) == 0 {
					r.errorf("scenario.empty", sc.File, loc+"/alt", "в блоке alt нет веток")
				}
			case s.Opt != nil:
				branches, key = []ScenarioBranch{*s.Opt}, "opt"
			default:
				branches, key = []ScenarioBranch{*s.Loop}, "loop"
			}
			for j, b := range branches {
				bp := loc + "/" + key
				if key == "alt" {
					bp += "/" + strconv.Itoa(j)
				}
				if len(b.Steps) == 0 {
					r.errorf("scenario.empty", sc.File, bp, "в блоке %s нет шагов", key)
					continue
				}
				walk(b.Steps, bp+"/steps", depth+1)
			}
		}
	}
	walk(sc.Steps, "/steps", 0)
}

// participantsRelated — есть ли между участниками связь в модели в любую
// сторону: прямая связь элементов или связь C4, концы которой совпадают с
// участниками, лежат внутри них или содержат их по иерархии C4 (компонент
// поднимается к контейнеру со связью). Прямые связи учитываются и у
// C4-предков участников. Структурная связь предка со своим же потомком
// после подъёма не считается: иначе два компонента одного контейнера
// считались бы связанными через его composition.
func (m *Model) participantsRelated(c *C4Model, a, b string) bool {
	covers := func(x, y string) bool { return x == y || c.isAncestor(x, y) || c.isAncestor(y, x) }
	direct := func(x, y string) bool {
		e := m.element(x)
		if e == nil {
			return false
		}
		for _, rel := range e.Relations {
			if rel.To == y {
				return true
			}
		}
		return false
	}
	for i, x := range c.lineage(a) {
		for j, y := range c.lineage(b) {
			if (i > 0 || j > 0) && covers(x, y) {
				continue
			}
			if direct(x, y) || direct(y, x) {
				return true
			}
		}
	}
	for _, rel := range c.Rels {
		if (covers(a, rel.From) && covers(b, rel.To)) || (covers(b, rel.From) && covers(a, rel.To)) {
			return true
		}
	}
	return false
}

// seqText — текст в sequenceDiagram: одна строка без символов, которые
// Mermaid читает как разделитель, сущность или комментарий и директиву
// (%%, в том числе %%{init}%%).
func seqText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.NewReplacer(";", ",", "#", "№", "%", "％", "<", "‹", ">", "›").Replace(s)
}

func seqArrow(kind string) string {
	switch kind {
	case StepAsync:
		return "-)"
	case StepReturn:
		return "-->>"
	}
	return "->>"
}

// scenarioAliases — псевдонимы участников сценария. Элементы C4 получают
// свои псевдонимы с C4-схем, остальные участники свободные, по
// отсортированным идентификаторам, без совпадений с C4 и между собой.
func scenarioAliases(sc *Scenario, c *C4Model) *diagram {
	d := c.aliasDiagram()
	ids := slices.Clone(scenarioParticipants(sc))
	slices.Sort(ids)
	for _, p := range ids {
		d.id(p)
	}
	return d
}

// sequenceDiagram — текст Mermaid sequenceDiagram сценария. Псевдонимы
// участников общие с C4-схемами.
func (m *Model) sequenceDiagram(sc *Scenario, c *C4Model) string {
	alias := scenarioAliases(sc, c)
	var b strings.Builder
	b.WriteString("sequenceDiagram\n")
	for _, p := range scenarioParticipants(sc) {
		kind := "participant"
		if m.participantPerson(p) {
			kind = "actor"
		}
		fmt.Fprintf(&b, "  %s %s as %s\n", kind, alias.id(p), seqText(m.participantName(p)))
	}
	var walk func(steps []ScenarioStep, indent int)
	walk = func(steps []ScenarioStep, indent int) {
		pad := strings.Repeat("  ", indent)
		for _, s := range steps {
			if s.isMessage() {
				fmt.Fprintf(&b, "%s%s%s%s: %s\n", pad, alias.id(s.From), seqArrow(s.Kind), alias.id(s.To), seqText(s.Message))
				continue
			}
			switch {
			case s.Alt != nil:
				for j, br := range s.Alt {
					word := "alt"
					if j > 0 {
						word = "else"
					}
					fmt.Fprintf(&b, "%s%s %s\n", pad, word, seqText(cmp(br.When, "условие")))
					walk(br.Steps, indent+1)
				}
			case s.Opt != nil:
				fmt.Fprintf(&b, "%sopt %s\n", pad, seqText(cmp(s.Opt.When, "условие")))
				walk(s.Opt.Steps, indent+1)
			case s.Loop != nil:
				fmt.Fprintf(&b, "%sloop %s\n", pad, seqText(cmp(s.Loop.When, "повтор")))
				walk(s.Loop.Steps, indent+1)
			}
			fmt.Fprintf(&b, "%send\n", pad)
		}
	}
	walk(sc.Steps, 1)
	return b.String()
}

// ScenarioView — сценарий для снимка (api-contract ScenarioView).
type ScenarioView struct {
	ID           string                `json:"id"`
	Title        string                `json:"title"`
	Description  string                `json:"description"`
	Process      *string               `json:"process"`
	Participants []ScenarioParticipant `json:"participants"`
	View         View                  `json:"view"`
}

// ScenarioParticipant — участник сценария в снимке.
type ScenarioParticipant struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Alias string `json:"alias"`
	Kind  string `json:"kind"` // person или participant
}

func (m *Model) scenarioView(sc *Scenario, c *C4Model) ScenarioView {
	v := ScenarioView{
		ID: sc.ID, Title: sc.Title, Description: strings.TrimSpace(sc.Description),
		Participants: []ScenarioParticipant{},
		View:         View{Title: "Сценарий: " + sc.Title, Mermaid: m.sequenceDiagram(sc, c)},
	}
	if sc.Process != "" {
		p := sc.Process
		v.Process = &p
	}
	alias := scenarioAliases(sc, c)
	for _, p := range scenarioParticipants(sc) {
		kind := "participant"
		if m.participantPerson(p) {
			kind = "person"
		}
		v.Participants = append(v.Participants, ScenarioParticipant{ID: p, Name: m.participantName(p), Alias: alias.id(p), Kind: kind})
	}
	return v
}

// Scenarios возвращает сценарии описания по идентификатору.
func (m *Model) Scenarios() []*Scenario { return m.scenarios }

// renderScenario — документ views/scenarios/<id>.md. up — путь от
// документа к корню views/ для ссылок на процессы.
func (m *Model) renderScenario(sc *Scenario, c *C4Model, up string) string {
	var b strings.Builder
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "# Сценарий: %s\n\n", sc.Title)
	if d := strings.TrimSpace(sc.Description); d != "" {
		b.WriteString(d + "\n\n")
	}
	if sc.Process != "" {
		name := m.participantName(sc.Process)
		if m.processes[sc.Process] != nil {
			fmt.Fprintf(&b, "Процесс: [%s](%sprocesses/%s.md).\n\n", linkText(name), up, sc.Process)
		} else {
			fmt.Fprintf(&b, "Процесс: %s (`%s`).\n\n", name, sc.Process)
		}
	}
	b.WriteString(fenced(m.sequenceDiagram(sc, c)))
	b.WriteString("\n| Участник | Элемент | Вид |\n|---|---|---|\n")
	for _, p := range scenarioParticipants(sc) {
		kind := "участник"
		if m.participantPerson(p) {
			kind = "человек"
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s |\n", cell(m.participantName(p)), p, kind)
	}
	return b.String()
}
