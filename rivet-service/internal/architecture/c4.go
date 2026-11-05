package architecture

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
)

// Нотация C4 строится из той же модели, что и слои TOGAF (спека
// backend/architecture «Нотация C4 поверх модели»): отдельной модели нет,
// вид элемента выводится из типа и связей composition, а свойства c4,
// technology и external уточняют вывод.

// Виды элементов C4.
const (
	C4Person    = "person"
	C4System    = "system"
	C4Container = "container"
	C4Database  = "database"
	C4Queue     = "queue"
	C4Component = "component"
	C4None      = "none"
)

// DescribedSystemID — идентификатор описываемой системы в модели C4. В
// идентификаторах элементов модели всегда есть точка, поэтому он с ними не
// пересекается.
const DescribedSystemID = "system"

var c4Kinds = []string{C4Person, C4System, C4Container, C4Database, C4Queue, C4Component, C4None}

// c4AllowedKinds — допустимые значения свойства c4 по типу элемента.
// Остальным типам допустимо только none: в C4 они не рисуются.
var c4AllowedKinds = map[string][]string{
	TypeActor:          {C4Person, C4None},
	TypeRole:           {C4Person, C4None},
	TypeComponent:      {C4System, C4Container, C4Database, C4Queue, C4Component, C4None},
	TypeSystemSoftware: {C4Container, C4Database, C4Queue, C4None},
}

func c4Allowed(typ string) []string {
	if list, ok := c4AllowedKinds[typ]; ok {
		return list
	}
	return []string{C4None}
}

// containerKind — вид уровня контейнеров: контейнер, база данных, очередь.
func containerKind(kind string) bool {
	return kind == C4Container || kind == C4Database || kind == C4Queue
}

// C4Element — элемент модели C4: человек, программная система, контейнер
// (база данных, очередь) или компонент. Parent — система для контейнера и
// контейнер для компонента, у людей и систем пусто.
type C4Element struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Technology  string `json:"technology"`
	External    bool   `json:"external"`
	Parent      string `json:"parent"`
	// Alias — псевдоним элемента на схемах Mermaid модели. Заполняется в
	// снимке, чтобы клиент находил элемент в SVG схемы.
	Alias string `json:"alias,omitempty"`
}

// MarshalJSON отдаёт пустого родителя как null (api-contract C4Element).
func (e C4Element) MarshalJSON() ([]byte, error) {
	type plain C4Element
	var parent *string
	if e.Parent != "" {
		p := e.Parent
		parent = &p
	}
	return json.Marshal(struct {
		plain
		Parent *string `json:"parent"`
	}{plain(e), parent})
}

// C4Rel — связь C4 между самыми детальными элементами. Подписи
// отсортированы и уникальны.
type C4Rel struct {
	From   string   `json:"from"`
	To     string   `json:"to"`
	Labels []string `json:"labels"`
}

// Label — подписи связи через запятую.
func (r C4Rel) Label() string { return strings.Join(r.Labels, ", ") }

// C4Model — модель C4 описания: элементы и связи между ними на самом
// детальном уровне. Уровни схем получаются подъёмом концов связей.
type C4Model struct {
	// Elements — описываемая система первой, затем остальные по id.
	Elements []C4Element
	Rels     []C4Rel

	byID    map[string]*C4Element
	storers map[string][]string // объект данных → базы данных, которые его хранят
	src     *Model
	aliases map[string]string // псевдонимы Mermaid по ключам схем, строятся при первом обращении
}

// Element возвращает элемент C4 по идентификатору или nil.
func (c *C4Model) Element(id string) *C4Element { return c.byID[id] }

// systemOf — система элемента: сама система, родитель контейнера,
// система контейнера компонента. Для людей пусто.
func (c *C4Model) systemOf(id string) string {
	e := c.byID[id]
	switch {
	case e == nil:
		return ""
	case e.Kind == C4System:
		return id
	case containerKind(e.Kind):
		return e.Parent
	case e.Kind == C4Component:
		return c.systemOf(e.Parent)
	}
	return ""
}

// children — элементы с родителем parent по id.
func (c *C4Model) children(parent string) []*C4Element {
	var out []*C4Element
	for i := range c.Elements {
		if c.Elements[i].Parent == parent {
			out = append(out, &c.Elements[i])
		}
	}
	return out
}

// systemsWithContainers — системы, у которых есть контейнеры: для каждой
// строится схема L2.
func (c *C4Model) systemsWithContainers() []string {
	var out []string
	for _, e := range c.Elements {
		if e.Kind == C4System && len(c.children(e.ID)) > 0 {
			out = append(out, e.ID)
		}
	}
	return out
}

// containersWithComponents — контейнеры, у которых есть компоненты: для
// каждого строится схема L3.
func (c *C4Model) containersWithComponents() []string {
	var out []string
	for _, e := range c.Elements {
		if containerKind(e.Kind) && len(c.children(e.ID)) > 0 {
			out = append(out, e.ID)
		}
	}
	return out
}

// isAncestor — a является предком b по иерархии C4.
func (c *C4Model) isAncestor(a, b string) bool {
	seen := map[string]bool{}
	for e := c.byID[b]; e != nil && e.Parent != "" && !seen[e.ID]; e = c.byID[e.Parent] {
		seen[e.ID] = true
		if e.Parent == a {
			return true
		}
	}
	return false
}

// lineage — идентификатор и его C4-предки от ближнего к дальнему. Ключ
// вне C4 — только он сам.
func (c *C4Model) lineage(id string) []string {
	out := []string{id}
	seen := map[string]bool{id: true}
	for e := c.byID[id]; e != nil && e.Parent != "" && !seen[e.Parent]; e = c.byID[e.Parent] {
		seen[e.Parent] = true
		out = append(out, e.Parent)
	}
	return out
}

// c4Props — явные свойства C4 элемента после проверки типов.
type c4Props struct {
	kind        string // пусто, если не задано или задано с ошибкой
	technology  string
	external    bool
	hasExternal bool
}

// c4Info — выведенный вид элемента. Пустой kind: элемент в C4 не рисуется.
type c4Info struct {
	kind     string
	parent   string
	external bool
}

type c4Builder struct {
	m       *Model
	r       *reporter // nil: вывод без отчёта о дефектах
	ids     []string
	recs    map[string]*Record
	props   map[string]c4Props
	parents map[string][]string // компонент → компоненты-родители по composition
	owners  map[string][]string // интерфейс или сервис → компоненты-владельцы
	info    map[string]*c4Info
	state   map[string]int // 1 — вывод идёт, 2 — готов
}

// deriveC4 строит модель C4. Если r задан, в него пишутся дефекты правил
// c4.*: ошибки только при явных свойствах, предупреждения о неоднозначном
// выводе.
func deriveC4(m *Model, r *reporter) *C4Model {
	b := &c4Builder{m: m, r: r, recs: map[string]*Record{}, props: map[string]c4Props{},
		parents: map[string][]string{}, owners: map[string][]string{}, info: map[string]*c4Info{}, state: map[string]int{}}
	for id, rec := range m.records {
		if rec.Element != nil && rec.Element.ID != "" {
			b.recs[id] = rec
			b.ids = append(b.ids, id)
		}
	}
	sort.Strings(b.ids)
	for _, id := range b.ids {
		b.props[id] = b.readProps(b.recs[id])
	}
	for _, id := range b.ids {
		e := b.recs[id].Element
		if e.Type != TypeComponent {
			continue
		}
		for _, rel := range e.Relations {
			t := b.element(rel.To)
			if t == nil || rel.To == id {
				continue
			}
			switch {
			case rel.Type == RelComposition && t.Type == TypeComponent:
				b.parents[rel.To] = appendUnique(b.parents[rel.To], id)
			case (rel.Type == RelComposition || rel.Type == RelRealization) &&
				(t.Type == TypeInterface || t.Type == TypeApplicationService):
				b.owners[rel.To] = appendUnique(b.owners[rel.To], id)
			}
		}
	}
	for _, list := range b.parents {
		sort.Strings(list)
	}
	for _, list := range b.owners {
		sort.Strings(list)
	}
	for _, id := range b.ids {
		b.resolve(id)
	}
	return b.build()
}

func appendUnique(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

func (b *c4Builder) element(id string) *Element {
	if rec := b.recs[id]; rec != nil {
		return rec.Element
	}
	return nil
}

func (b *c4Builder) errorf(rule string, rec *Record, format string, args ...any) {
	if b.r != nil {
		b.r.errorf(rule, rec.File, rec.ID, format, args...)
	}
}

func (b *c4Builder) warnf(rule string, rec *Record, format string, args ...any) {
	if b.r != nil {
		b.r.warnf(rule, rec.File, rec.ID, format, args...)
	}
}

// readProps проверяет зарезервированные свойства (правила c4.prop-type,
// c4.kind-unknown, c4.kind-type) и возвращает годные значения.
func (b *c4Builder) readProps(rec *Record) c4Props {
	e := rec.Element
	var p c4Props
	if v, ok := e.Props[PropC4]; ok {
		s, isStr := v.(string)
		switch {
		case !isStr:
			b.errorf("c4.prop-type", rec, "свойство %s должно быть строкой", PropC4)
		case !slices.Contains(c4Kinds, s):
			b.errorf("c4.kind-unknown", rec, "неизвестный вид C4 %q, допустимы: %s", s, strings.Join(c4Kinds, ", "))
		case !slices.Contains(c4Allowed(e.Type), s):
			b.errorf("c4.kind-type", rec, "вид C4 %s недопустим для элемента типа %s, допустимы: %s",
				s, e.Type, strings.Join(c4Allowed(e.Type), ", "))
		default:
			p.kind = s
		}
	}
	if v, ok := e.Props[PropTechnology]; ok {
		if s, isStr := v.(string); isStr {
			p.technology = strings.Join(strings.Fields(s), " ")
		} else {
			b.errorf("c4.prop-type", rec, "свойство %s должно быть строкой", PropTechnology)
		}
	}
	if v, ok := e.Props[PropExternal]; ok {
		if x, isBool := v.(bool); isBool {
			p.external, p.hasExternal = x, true
		} else {
			b.errorf("c4.prop-type", rec, "свойство %s должно быть логическим значением true или false", PropExternal)
		}
	}
	return p
}

// drawnAncestor — ближайший рисуемый предок компонента по выбранным
// родителям (первый по id). Цикл composition обрывается.
func (b *c4Builder) drawnAncestor(id string) (string, *c4Info) {
	seen := map[string]bool{}
	for id != "" && !seen[id] {
		seen[id] = true
		if inf := b.resolve(id); inf.kind != "" {
			return id, inf
		}
		ps := b.parents[id]
		if len(ps) == 0 {
			return "", nil
		}
		id = ps[0]
	}
	return "", nil
}

// resolve выводит вид элемента. Компоненты выводятся рекурсивно от
// родителей, цикл composition даёт элемент без родителя.
func (b *c4Builder) resolve(id string) *c4Info {
	if inf := b.info[id]; inf != nil && b.state[id] == 2 {
		return inf
	}
	if b.state[id] == 1 {
		return &c4Info{}
	}
	b.state[id] = 1
	inf := b.classify(id)
	b.info[id] = inf
	b.state[id] = 2
	return inf
}

func (b *c4Builder) classify(id string) *c4Info {
	rec := b.recs[id]
	e := rec.Element
	p := b.props[id]
	ext := p.hasExternal && p.external
	switch e.Type {
	case TypeActor, TypeRole:
		if p.kind == C4None {
			return &c4Info{}
		}
		return &c4Info{kind: C4Person, external: ext}
	case TypeSystemSoftware:
		switch {
		case p.kind == C4None:
			return &c4Info{}
		case containerKind(p.kind):
			return &c4Info{kind: p.kind, parent: DescribedSystemID, external: ext}
		case ext:
			return &c4Info{kind: C4System, external: true}
		case b.storesData(e):
			return &c4Info{kind: C4Database, parent: DescribedSystemID}
		}
		return &c4Info{}
	case TypeComponent:
		return b.classifyComponent(rec, p, ext)
	}
	return &c4Info{}
}

// storesData — системное ПО обращается к объекту данных: это база данных.
func (b *c4Builder) storesData(e *Element) bool {
	for _, rel := range e.Relations {
		if t := b.element(rel.To); rel.Type == RelAccess && t != nil && t.Type == TypeDataObject {
			return true
		}
	}
	return false
}

func (b *c4Builder) classifyComponent(rec *Record, p c4Props, ext bool) *c4Info {
	ps := b.parents[rec.ID]
	parent := ""
	if len(ps) > 0 {
		parent = ps[0]
	}
	if ext && parent != "" {
		b.errorf("c4.external-nested", rec, "свойство %s задаётся только у элемента без родителя, а %s входит в composition %s",
			PropExternal, rec.ID, strings.Join(ps, ", "))
	}
	switch {
	case p.kind == C4None:
		return &c4Info{}
	case p.kind == C4System:
		return &c4Info{kind: C4System, external: ext}
	case containerKind(p.kind):
		if len(ps) > 1 {
			b.errorf("c4.container-parent", rec, "контейнер должен входить ровно в одну систему, а входит в composition %s",
				strings.Join(ps, ", "))
		}
		if parent == "" {
			return &c4Info{kind: p.kind, parent: DescribedSystemID, external: ext}
		}
		aid, ainf := b.drawnAncestor(parent)
		if ainf == nil || ainf.kind != C4System {
			b.errorf("c4.container-parent", rec, "контейнер должен входить в программную систему, а %s ею не является", parent)
			return &c4Info{kind: p.kind, parent: DescribedSystemID, external: ext}
		}
		return &c4Info{kind: p.kind, parent: aid, external: ext}
	case p.kind == C4Component:
		if len(ps) != 1 {
			msg := "не входит в composition ни одного контейнера"
			if len(ps) > 1 {
				msg = "входит в composition нескольких компонентов: " + strings.Join(ps, ", ")
			}
			b.errorf("c4.component-parent", rec, "компонент должен входить ровно в один контейнер, а %s", msg)
		}
		if parent != "" {
			aid, ainf := b.drawnAncestor(parent)
			switch {
			case ainf != nil && containerKind(ainf.kind):
				return &c4Info{kind: C4Component, parent: aid}
			case ainf != nil && ainf.kind == C4Component:
				return &c4Info{kind: C4Component, parent: ainf.parent}
			}
			if len(ps) == 1 {
				b.errorf("c4.component-parent", rec, "компонент должен входить в контейнер, а %s им не является", parent)
			}
		}
		return &c4Info{kind: C4Container, parent: DescribedSystemID}
	}
	// Вывод по умолчанию.
	if len(ps) > 1 {
		b.warnf("c4.ambiguous-parent", rec, "компонент входит в composition нескольких компонентов (%s), для C4 выбран %s",
			strings.Join(ps, ", "), parent)
	}
	if parent == "" {
		if ext {
			return &c4Info{kind: C4System, external: true}
		}
		return &c4Info{kind: C4Container, parent: DescribedSystemID}
	}
	aid, ainf := b.drawnAncestor(parent)
	switch {
	case ainf == nil:
		return &c4Info{kind: C4Container, parent: DescribedSystemID}
	case ainf.kind == C4System:
		return &c4Info{kind: C4Container, parent: aid}
	case containerKind(ainf.kind):
		return &c4Info{kind: C4Component, parent: aid}
	case ainf.kind == C4Component:
		return &c4Info{kind: C4Component, parent: ainf.parent}
	}
	return &c4Info{kind: C4Container, parent: DescribedSystemID}
}

// owner — рисуемый элемент, к которому относятся связи элемента: сам
// элемент, а у интерфейса и сервиса приложения — компонент-владелец
// (composition) или реализующий (realization), первый рисуемый по id.
func (b *c4Builder) owner(id string) string {
	if inf := b.info[id]; inf != nil && inf.kind != "" {
		return id
	}
	e := b.element(id)
	if e == nil || (e.Type != TypeInterface && e.Type != TypeApplicationService) {
		return ""
	}
	for _, c := range b.owners[id] {
		if inf := b.info[c]; inf != nil && inf.kind != "" {
			return c
		}
	}
	return ""
}

func accessLabel(mode, data string) string {
	switch mode {
	case "read":
		return "читает " + data
	case "write":
		return "пишет " + data
	case "readwrite":
		return "читает и пишет " + data
	}
	return "обращается к " + data
}

func (b *c4Builder) build() *C4Model {
	c := &C4Model{byID: map[string]*C4Element{}, storers: map[string][]string{}, src: b.m}
	c.Elements = append(c.Elements, C4Element{
		ID: DescribedSystemID, Kind: C4System,
		Name:        cmp(strings.TrimSpace(b.m.Manifest.System), "Система"),
		Description: strings.Join(strings.Fields(b.m.Manifest.Description), " "),
	})
	for _, id := range b.ids {
		inf := b.info[id]
		if inf == nil || inf.kind == "" {
			continue
		}
		e := b.recs[id].Element
		c.Elements = append(c.Elements, C4Element{
			ID: id, Kind: inf.kind, Name: elementLabel(e),
			Description: strings.Join(strings.Fields(e.Description), " "),
			Technology:  b.props[id].technology, External: inf.external, Parent: inf.parent,
		})
	}
	for i := range c.Elements {
		c.byID[c.Elements[i].ID] = &c.Elements[i]
	}
	// Базы данных по объектам данных: связь access компонента к объекту
	// превращается в связь к базе, которая этот объект хранит.
	for _, id := range b.ids {
		if el := c.byID[id]; el == nil || el.Kind != C4Database {
			continue
		}
		for _, rel := range b.recs[id].Element.Relations {
			if t := b.element(rel.To); rel.Type == RelAccess && t != nil && t.Type == TypeDataObject {
				c.storers[rel.To] = appendUnique(c.storers[rel.To], id)
			}
		}
	}
	for _, list := range c.storers {
		sort.Strings(list)
	}
	rels := map[[2]string]map[string]bool{}
	add := func(from, to, label string) {
		if from == "" || to == "" || from == to || c.isAncestor(from, to) || c.isAncestor(to, from) {
			return
		}
		k := [2]string{from, to}
		if rels[k] == nil {
			rels[k] = map[string]bool{}
		}
		rels[k][label] = true
	}
	for _, id := range b.ids {
		e := b.recs[id].Element
		for _, rel := range e.Relations {
			switch rel.Type {
			case RelServing:
				add(b.owner(rel.To), b.owner(id), "использует")
			case RelFlow:
				add(b.owner(id), b.owner(rel.To), "передаёт")
			case RelTriggering:
				add(b.owner(id), b.owner(rel.To), "вызывает")
			case RelAccess:
				t := b.element(rel.To)
				if t == nil || t.Type != TypeDataObject {
					continue
				}
				from := b.owner(id)
				for _, db := range c.storers[rel.To] {
					add(from, db, accessLabel(rel.Mode, elementLabel(t)))
				}
			}
		}
	}
	keys := make([][2]string, 0, len(rels))
	for k := range rels {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		c.Rels = append(c.Rels, C4Rel{From: k[0], To: k[1], Labels: sortedKeys(rels[k])})
	}
	return c
}

// checkC4 — правила согласованности C4 (спека «Согласованность C4»).
func (m *Model) checkC4(r *reporter) { deriveC4(m, r) }

// C4 выводит модель C4 из корректного описания.
func (m *Model) C4() *C4Model { return deriveC4(m, nil) }

// lift поднимает концы связей до видимых на уровне элементов, отбрасывает
// петли и склеивает повторы.
func (c *C4Model) lift(visible func(string) string) []C4Rel {
	merged := map[[2]string]map[string]bool{}
	for _, rel := range c.Rels {
		from, to := visible(rel.From), visible(rel.To)
		if from == "" || to == "" || from == to {
			continue
		}
		k := [2]string{from, to}
		if merged[k] == nil {
			merged[k] = map[string]bool{}
		}
		for _, l := range rel.Labels {
			merged[k][l] = true
		}
	}
	keys := make([][2]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	out := make([]C4Rel, 0, len(keys))
	for _, k := range keys {
		out = append(out, C4Rel{From: k[0], To: k[1], Labels: sortedKeys(merged[k])})
	}
	return out
}

// contextVisible — уровень L1: люди и системы, контейнеры и компоненты
// поднимаются к своей системе.
func (c *C4Model) contextVisible(id string) string {
	e := c.byID[id]
	switch {
	case e == nil:
		return ""
	case e.Kind == C4Person || e.Kind == C4System:
		return id
	}
	return c.systemOf(id)
}

// containerVisible — уровень L2 системы sys: её контейнеры видны, их
// компоненты поднимаются к контейнеру, чужие элементы к своей системе.
func (c *C4Model) containerVisible(sys string) func(string) string {
	return func(id string) string {
		e := c.byID[id]
		switch {
		case e == nil || id == sys:
			return ""
		case e.Kind == C4Person || e.Kind == C4System:
			return id
		case containerKind(e.Kind):
			if e.Parent == sys {
				return id
			}
			return e.Parent
		case e.Kind == C4Component:
			if c.systemOf(e.Parent) == sys {
				return e.Parent
			}
			return c.systemOf(e.Parent)
		}
		return ""
	}
}

// componentVisible — уровень L3 контейнера cont: его компоненты видны,
// соседние контейнеры той же системы видны целиком, остальное как на L2.
func (c *C4Model) componentVisible(cont string) func(string) string {
	sys := c.systemOf(cont)
	l2 := c.containerVisible(sys)
	return func(id string) string {
		e := c.byID[id]
		switch {
		case e == nil || id == cont:
			return ""
		case e.Kind == C4Component && e.Parent == cont:
			return id
		}
		v := l2(id)
		if v == cont {
			return ""
		}
		return v
	}
}
