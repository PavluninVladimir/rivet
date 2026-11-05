package architecture

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Схемы Mermaid C4 (спека «C4-представления»): контекст L1, контейнеры L2
// по системам, компоненты L3 по контейнерам, развёртывание. Псевдонимы
// строит тот же помощник, что у flowchart, поэтому вывод побайтно стабилен.

// View — схема C4: заголовок и исходный текст Mermaid без ограждения
// блока кода (api-contract View).
type View struct {
	Title   string `json:"title"`
	Mermaid string `json:"mermaid"`
}

// SystemView — схема контейнеров системы.
type SystemView struct {
	SystemID string `json:"system_id"`
	View     View   `json:"view"`
}

// ContainerView — схема компонентов контейнера.
type ContainerView struct {
	ContainerID string `json:"container_id"`
	View        View   `json:"view"`
}

// LegendItem — стиль подсветки изменения на целевых C4-схемах.
type LegendItem struct {
	State string `json:"state"`
	Color string `json:"color"`
	Title string `json:"title"`
}

// c4Styles — цвета подсветки, те же, что classDef в схемах слоёв.
var c4Styles = map[string]struct{ bg, font, border string }{
	"added":    {"#e6f4ea", "#0d652d", "#1e8e3e"},
	"modified": {"#fef7e0", "#7a4100", "#e37400"},
	"removed":  {"#fce8e6", "#a50e0e", "#d93025"},
}

// C4Legend — легенда подсветки изменений в порядке показа.
func C4Legend() []LegendItem {
	return []LegendItem{
		{State: "added", Color: c4Styles["added"].border, Title: "добавлено"},
		{State: "modified", Color: c4Styles["modified"].border, Title: "изменено"},
		{State: "removed", Color: c4Styles["removed"].border, Title: "удалено, оставлено на схеме для наглядности"},
	}
}

// MermaidAlias — псевдоним элемента на всех схемах Mermaid модели: префикс
// n_, строчные буквы и цифры, остальные символы заменены на «_». Консоль по
// нему находит элемент в SVG. Псевдонимы считаются один раз на модель по
// отсортированным ключам, поэтому совпадение (app.a-b и app.a.b) получает
// суффикс _2 одинаково на каждой схеме.
func (c *C4Model) MermaidAlias(id string) string { return c.alias(id) }

// alias — псевдоним ключа схемы: идентификатор элемента C4, хоста
// развёртывания или экземпляра на хосте (instanceKey). Ключу вне таблицы
// достаётся псевдоним, не занятый таблицей. Схема с несколькими такими
// ключами строит псевдонимы через aliasDiagram, чтобы они не совпали между
// собой.
func (c *C4Model) alias(key string) string {
	if a, ok := c.aliasTable()[key]; ok {
		return a
	}
	return c.aliasDiagram().id(key)
}

// aliasDiagram — таблица псевдонимов схемы с уже занятыми псевдонимами C4.
// Ключи C4 получают свои псевдонимы, остальные ключи свободные, без
// совпадений с C4 и между собой.
func (c *C4Model) aliasDiagram() *diagram {
	d := &diagram{ids: map[string]string{}, used: map[string]bool{}}
	for k, a := range c.aliasTable() {
		d.ids[k] = a
		d.used[a] = true
	}
	return d
}

func (c *C4Model) aliasTable() map[string]string {
	if c.aliases == nil {
		keys := map[string]bool{}
		for _, e := range c.Elements {
			keys[e.ID] = true
		}
		if t, ok := c.deployTree(); ok {
			for _, h := range t.hosts {
				keys[h.ID] = true
				for _, inst := range t.instances[h.ID] {
					keys[instanceKey(h.ID, inst)] = true
				}
			}
		}
		d := &diagram{ids: map[string]string{}, used: map[string]bool{}}
		for _, k := range sortedKeys(keys) {
			d.id(k)
		}
		c.aliases = d.ids
	}
	return c.aliases
}

func instanceKey(host, id string) string { return "inst:" + host + ":" + id }

// c4Diagram — схема Mermaid C4 с псевдонимами модели.
type c4Diagram struct {
	b     strings.Builder
	alias func(string) string
}

func newC4Diagram(c *C4Model, kind, title string) *c4Diagram {
	d := &c4Diagram{alias: c.alias}
	d.b.WriteString(kind + "\n")
	d.line(1, "title %s", c4Title(title))
	return d
}

// c4Title — заголовок схемы. Парсер Mermaid C4 читает title до конца строки
// и спотыкается о «#» и «;» (Lexical error), поэтому они заменяются.
func c4Title(s string) string {
	s = strings.NewReplacer("#", "№", ";", ",", "%", "％", "<", "‹", ">", "›").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

func (d *c4Diagram) line(indent int, format string, args ...any) {
	d.b.WriteString(strings.Repeat("  ", indent))
	fmt.Fprintf(&d.b, format, args...)
	d.b.WriteByte('\n')
}

func (d *c4Diagram) id(key string) string { return d.alias(key) }

func (d *c4Diagram) text() string { return d.b.String() }

// c4q — строковый аргумент Mermaid C4. Экранирования кавычек парсер не
// знает, поэтому двойные кавычки заменяются одинарными. Знак процента
// заменяется полноширинным, как в ERD: директива %%{init}%% из имени
// элемента не должна доходить до схемы.
func c4q(s string) string {
	s = strings.NewReplacer(`"`, "'", "%", "％", "<", "‹", ">", "›").Replace(strings.Join(strings.Fields(s), " "))
	return `"` + s + `"`
}

func c4Func(e *C4Element) string {
	fn := map[string]string{
		C4Person: "Person", C4System: "System", C4Container: "Container",
		C4Database: "ContainerDb", C4Queue: "ContainerQueue", C4Component: "Component",
	}[e.Kind]
	if e.External {
		fn += "_Ext"
	}
	return fn
}

func (d *c4Diagram) element(indent int, e *C4Element) { d.elementAs(indent, e, e.ID) }

// elementAs рисует элемент под псевдонимом ключа: экземпляр на хосте
// развёртывания получает свой псевдоним.
func (d *c4Diagram) elementAs(indent int, e *C4Element, key string) {
	if e.Kind == C4Person || e.Kind == C4System {
		d.line(indent, "%s(%s, %s, %s)", c4Func(e), d.id(key), c4q(e.Name), c4q(e.Description))
		return
	}
	d.line(indent, "%s(%s, %s, %s, %s)", c4Func(e), d.id(key), c4q(e.Name), c4q(e.Technology), c4q(e.Description))
}

// kindTitle — вид элемента для таблиц.
func kindTitle(e *C4Element) string {
	t := map[string]string{
		C4Person: "человек", C4System: "система", C4Container: "контейнер",
		C4Database: "база данных", C4Queue: "очередь", C4Component: "компонент",
	}[e.Kind]
	if e.External {
		t += ", внешний"
	}
	return t
}

func kindRank(e *C4Element) int {
	switch {
	case e.Kind == C4Person:
		return 0
	case e.Kind == C4System && e.ID == DescribedSystemID:
		return 1
	case e.Kind == C4System:
		return 2
	case containerKind(e.Kind):
		return 3
	}
	return 4
}

func sortC4(list []*C4Element) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ra, rb := kindRank(a), kindRank(b); ra != rb {
			return ra < rb
		}
		return a.ID < b.ID
	})
}

// c4Scene — содержимое одной схемы: граница (система или контейнер) с
// элементами внутри, элементы снаружи и поднятые связи.
type c4Scene struct {
	model    *C4Model
	kind     string
	title    string
	boundary *C4Element
	inside   []*C4Element
	outside  []*C4Element
	rels     []C4Rel
}

// scene собирает схему. Со своей границей (L2, L3) на схеме остаются
// только связи, у которых хотя бы один конец внутри границы, и их соседи.
// Без границы (L1) показываются все люди, системы и связи между ними.
func (c *C4Model) scene(kind, title string, boundary *C4Element, inside []*C4Element, rels []C4Rel, all bool) c4Scene {
	s := c4Scene{model: c, kind: kind, title: title, boundary: boundary, inside: inside}
	in := map[string]bool{}
	for _, e := range inside {
		in[e.ID] = true
	}
	for _, r := range rels {
		if boundary == nil || in[r.From] || in[r.To] {
			s.rels = append(s.rels, r)
		}
	}
	rels = s.rels
	seen := map[string]bool{}
	addOut := func(id string) {
		if e := c.byID[id]; e != nil && !in[id] && !seen[id] && (boundary == nil || id != boundary.ID) {
			seen[id] = true
			s.outside = append(s.outside, e)
		}
	}
	if all {
		for i := range c.Elements {
			if e := &c.Elements[i]; e.Kind == C4Person || e.Kind == C4System {
				addOut(e.ID)
			}
		}
	}
	for _, r := range rels {
		addOut(r.From)
		addOut(r.To)
	}
	sortC4(s.inside)
	sortC4(s.outside)
	return s
}

func (c *C4Model) contextScene() c4Scene {
	return c.scene("C4Context", "Контекст: "+c.byID[DescribedSystemID].Name, nil, nil, c.lift(c.contextVisible), true)
}

func (c *C4Model) containerScene(sys string) c4Scene {
	b := c.byID[sys]
	return c.scene("C4Container", "Контейнеры: "+b.Name, b, c.children(sys), c.lift(c.containerVisible(sys)), false)
}

func (c *C4Model) componentScene(cont string) c4Scene {
	b := c.byID[cont]
	return c.scene("C4Component", "Компоненты: "+b.Name, b, c.children(cont), c.lift(c.componentVisible(cont)), false)
}

// render рисует схему. classes подсвечивает элементы изменения.
func (s c4Scene) render(classes map[string]string) string {
	d := newC4Diagram(s.model, s.kind, s.title)
	var shown []*C4Element
	for _, e := range s.outside {
		if e.Kind == C4Person {
			d.element(1, e)
			shown = append(shown, e)
		}
	}
	if s.boundary != nil {
		fn := "System_Boundary"
		if s.boundary.Kind != C4System {
			fn = "Container_Boundary"
		}
		d.line(1, "%s(%s, %s) {", fn, d.id(s.boundary.ID), c4q(s.boundary.Name))
		for _, e := range s.inside {
			d.element(2, e)
			shown = append(shown, e)
		}
		d.line(1, "}")
	}
	for _, e := range s.outside {
		if e.Kind != C4Person {
			d.element(1, e)
			shown = append(shown, e)
		}
	}
	for _, r := range s.rels {
		d.line(1, "Rel(%s, %s, %s)", d.id(r.From), d.id(r.To), c4q(r.Label()))
	}
	sort.Slice(shown, func(i, j int) bool { return shown[i].ID < shown[j].ID })
	for _, e := range shown {
		if st, ok := c4Styles[classes[e.ID]]; ok {
			d.line(1, `UpdateElementStyle(%s, $bgColor="%s", $fontColor="%s", $borderColor="%s")`, d.id(e.ID), st.bg, st.font, st.border)
		}
	}
	return d.text()
}

func (s c4Scene) view(classes map[string]string) View {
	return View{Title: s.title, Mermaid: s.render(classes)}
}

func fenced(text string) string { return "```mermaid\n" + text + "```\n" }

// elementsTable — таблица элементов схемы: вид, технология, описание.
func elementsTable(b *strings.Builder, list []*C4Element) {
	if len(list) == 0 {
		return
	}
	sorted := slices.Clone(list)
	sortC4(sorted)
	b.WriteString("\n| Элемент | Название | Вид | Технология | Описание |\n|---|---|---|---|---|\n")
	for _, e := range sorted {
		fmt.Fprintf(b, "| `%s` | %s | %s | %s | %s |\n", e.ID, cell(e.Name), kindTitle(e), cell(e.Technology), cell(e.Description))
	}
}

func (s c4Scene) elements() []*C4Element {
	out := append(slices.Clone(s.outside), s.inside...)
	if s.boundary != nil {
		out = append(out, s.boundary)
	}
	return out
}

// renderC4File — Markdown-файл схемы в views/c4/.
func renderC4File(s c4Scene, links []string) string {
	var b strings.Builder
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "# %s\n\n", s.title)
	for _, l := range links {
		b.WriteString(l + "\n")
	}
	if len(links) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(fenced(s.render(nil)))
	elementsTable(&b, s.elements())
	return b.String()
}

// c4Files — схемы описания для views/: путь от корня описания → текст.
func (c *C4Model) c4Files() map[string]string {
	out := map[string]string{}
	sys := c.byID[DescribedSystemID]
	var links []string
	for _, id := range c.systemsWithContainers() {
		links = append(links, fmt.Sprintf("- Контейнеры: [%s](container/%s.md)", linkText(c.byID[id].Name), id))
	}
	if _, ok := c.deployment(); ok {
		links = append(links, "- [Развёртывание](deployment.md)")
	}
	out[ViewsDir+"/c4/context.md"] = renderC4File(c.contextScene(), links)
	for _, id := range c.systemsWithContainers() {
		links := []string{"- [Контекст](../context.md)"}
		for _, ch := range c.children(id) {
			if len(c.children(ch.ID)) > 0 {
				links = append(links, fmt.Sprintf("- Компоненты: [%s](../component/%s.md)", linkText(ch.Name), ch.ID))
			}
		}
		out[ViewsDir+"/c4/container/"+id+".md"] = renderC4File(c.containerScene(id), links)
	}
	for _, id := range c.containersWithComponents() {
		links := []string{fmt.Sprintf("- Контейнеры: [%s](../container/%s.md)", linkText(c.byID[c.systemOf(id)].Name), c.systemOf(id))}
		out[ViewsDir+"/c4/component/"+id+".md"] = renderC4File(c.componentScene(id), links)
	}
	if text, ok := c.deployment(); ok {
		var b strings.Builder
		b.WriteString(generatedMark)
		fmt.Fprintf(&b, "# Развёртывание: %s\n\n- [Контекст](context.md)\n\n", sys.Name)
		b.WriteString(fenced(text))
		out[ViewsDir+"/c4/deployment.md"] = b.String()
	}
	return out
}

// ─── развёртывание ───────────────────────────────────────────────────────

// deployTree — дерево развёртывания по слою технологий: узлы и системное
// ПО, которое не является элементом C4, вкладываются по composition и
// assignment. Контейнер размещается там, где его обслуживает (serving)
// узел или системное ПО. Элемент C4 из слоя технологий (база данных,
// внешняя система) размещается экземпляром внутри своего узла. Элемент C4
// без узла на схеме развёртывания не показывается: его размещение не
// описано. Mermaid и Structurizr DSL строятся из одного дерева и
// показывают одно и то же.
type deployTree struct {
	hosts     []*Element          // узлы и системное ПО вне C4 по id
	children  map[string][]string // хост → дочерние хосты
	instances map[string][]string // хост → элементы C4 на нём
	roots     []string            // хосты без родителя
}

func (c *C4Model) deployTree() (*deployTree, bool) {
	m := c.src
	t := &deployTree{children: map[string][]string{}, instances: map[string][]string{}}
	isHost := map[string]bool{}
	hasNode := false
	var techs []*Element
	for _, e := range m.Elements() {
		if e.Type != TypeNode && e.Type != TypeSystemSoftware {
			continue
		}
		e := m.element(e.ID)
		techs = append(techs, e)
		if e.Type == TypeNode {
			hasNode = true
		}
		if c.byID[e.ID] == nil {
			isHost[e.ID] = true
		}
	}
	if !hasNode {
		return nil, false
	}
	sort.Slice(techs, func(i, j int) bool { return techs[i].ID < techs[j].ID })
	parent := map[string]string{}
	for _, h := range techs {
		if !isHost[h.ID] {
			continue
		}
		t.hosts = append(t.hosts, h)
		for _, rel := range h.Relations {
			to := m.element(rel.To)
			if to == nil || rel.To == h.ID {
				continue
			}
			switch {
			case (rel.Type == RelComposition || rel.Type == RelAssignment) && (to.Type == TypeNode || to.Type == TypeSystemSoftware):
				// Первый по id родитель: хосты обходятся по возрастанию id.
				if _, ok := parent[rel.To]; !ok {
					parent[rel.To] = h.ID
				}
			case rel.Type == RelServing:
				if v := c.containerLevel(rel.To); v != "" {
					t.instances[h.ID] = appendUnique(t.instances[h.ID], v)
				}
			}
		}
	}
	for _, e := range techs {
		p, ok := parent[e.ID]
		switch {
		case isHost[e.ID] && ok && !t.descends(parent, p, e.ID):
			t.children[p] = append(t.children[p], e.ID)
		case isHost[e.ID]:
			t.roots = append(t.roots, e.ID)
		case ok:
			t.instances[p] = appendUnique(t.instances[p], e.ID)
		}
	}
	for _, list := range t.instances {
		sort.Strings(list)
	}
	return t, true
}

// descends — по цепочке родителей от p встречается id (цикл вложенности).
func (t *deployTree) descends(parent map[string]string, p, id string) bool {
	seen := map[string]bool{}
	for p != "" && !seen[p] {
		if p == id {
			return true
		}
		seen[p] = true
		p = parent[p]
	}
	return false
}

// containerLevel — элемент уровня контейнеров для размещения.
func (c *C4Model) containerLevel(id string) string {
	e := c.byID[id]
	switch {
	case e == nil:
		return ""
	case containerKind(e.Kind):
		return id
	case e.Kind == C4Component:
		return c.containerLevel(e.Parent)
	}
	return ""
}

func hostType(e *Element) string {
	if t, ok := e.Props[PropTechnology].(string); ok && strings.TrimSpace(t) != "" {
		return t
	}
	if e.Type == TypeNode {
		return "узел"
	}
	return "системное ПО"
}

// deployment — схема развёртывания; ok=false, если узлов нет.
func (c *C4Model) deployment() (string, bool) {
	t, ok := c.deployTree()
	if !ok {
		return "", false
	}
	d := newC4Diagram(c, "C4Deployment", "Развёртывание: "+c.byID[DescribedSystemID].Name)
	byID := map[string]*Element{}
	for _, h := range t.hosts {
		byID[h.ID] = h
	}
	var walk func(id string, indent int)
	walk = func(id string, indent int) {
		h := byID[id]
		d.line(indent, "Deployment_Node(%s, %s, %s, %s) {", d.id(id), c4q(elementLabel(h)), c4q(hostType(h)), c4q(h.Description))
		for _, ch := range t.children[id] {
			walk(ch, indent+1)
		}
		for _, inst := range t.instances[id] {
			d.elementAs(indent+1, c.byID[inst], instanceKey(id, inst))
		}
		d.line(indent, "}")
	}
	for _, id := range t.roots {
		walk(id, 1)
	}
	return d.text(), true
}

// ─── C4 изменения ────────────────────────────────────────────────────────

// ChangeC4 — целевые C4-схемы изменения с подсветкой (api-contract
// ArchitectureChangeC4 без полей снимка).
type ChangeC4 struct {
	Context    View            `json:"context"`
	Containers []SystemView    `json:"containers"`
	Components []ContainerView `json:"components"`
	Legend     []LegendItem    `json:"legend"`
}

// changeScenes — целевая модель C4 изменения с удалёнными элементами,
// оставленными для наглядности, и подсветка по элементам дельты.
func changeScenes(ch *Change) (*C4Model, map[string]string) {
	target, cur := deriveC4(ch.Target, nil), deriveC4(ch.Current, nil)
	classes := map[string]string{}
	var removed []string
	for _, d := range ch.Deltas {
		if !d.Viewpoint.Builtin() {
			continue
		}
		for _, rec := range d.Added {
			if target.byID[rec.ID] != nil {
				classes[rec.ID] = "added"
			}
		}
		for _, rec := range d.Modified {
			if target.byID[rec.ID] != nil {
				classes[rec.ID] = "modified"
			}
		}
		for _, rm := range d.Removed {
			if cur.byID[rm.ID] != nil && target.byID[rm.ID] == nil {
				classes[rm.ID] = "removed"
				removed = append(removed, rm.ID)
			}
		}
	}
	if len(removed) == 0 {
		return target, classes
	}
	sort.Strings(removed)
	disp := &C4Model{Elements: slices.Clone(target.Elements), Rels: slices.Clone(target.Rels), storers: target.storers, src: ch.Target}
	for _, id := range removed {
		disp.Elements = append(disp.Elements, *cur.byID[id])
	}
	disp.reindex()
	for _, r := range cur.Rels {
		if (classes[r.From] == "removed" || classes[r.To] == "removed") && disp.byID[r.From] != nil && disp.byID[r.To] != nil {
			disp.Rels = append(disp.Rels, r)
		}
	}
	sort.SliceStable(disp.Rels, func(i, j int) bool {
		if disp.Rels[i].From != disp.Rels[j].From {
			return disp.Rels[i].From < disp.Rels[j].From
		}
		return disp.Rels[i].To < disp.Rels[j].To
	})
	return disp, classes
}

func (c *C4Model) reindex() {
	sort.SliceStable(c.Elements, func(i, j int) bool {
		a, b := c.Elements[i], c.Elements[j]
		if (a.ID == DescribedSystemID) != (b.ID == DescribedSystemID) {
			return a.ID == DescribedSystemID
		}
		return a.ID < b.ID
	})
	c.byID = map[string]*C4Element{}
	c.aliases = nil
	for i := range c.Elements {
		c.byID[c.Elements[i].ID] = &c.Elements[i]
	}
}

// touchedContainers — контейнеры, которые сами изменены или чьи компоненты
// изменены: для них строятся схемы компонентов.
func touchedContainers(c *C4Model, classes map[string]string) []string {
	var out []string
	for _, id := range c.containersWithComponents() {
		touched := classes[id] != ""
		for _, ch := range c.children(id) {
			touched = touched || classes[ch.ID] != ""
		}
		if touched {
			out = append(out, id)
		}
	}
	return out
}

// BuildChangeC4 строит целевые C4-схемы корректного изменения.
func BuildChangeC4(ch *Change) *ChangeC4 {
	c, classes := changeScenes(ch)
	out := &ChangeC4{Context: c.contextScene().view(classes), Containers: []SystemView{}, Components: []ContainerView{}, Legend: C4Legend()}
	for _, id := range c.systemsWithContainers() {
		out.Containers = append(out.Containers, SystemView{SystemID: id, View: c.containerScene(id).view(classes)})
	}
	for _, id := range touchedContainers(c, classes) {
		out.Components = append(out.Components, ContainerView{ContainerID: id, View: c.componentScene(id).view(classes)})
	}
	return out
}

func renderChangeC4(ch *Change) string {
	v := BuildChangeC4(ch)
	var b strings.Builder
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "# C4 изменения: %s (`%s`)\n\n", cmp(ch.Info.Title, ch.Info.ID), ch.Info.ID)
	fmt.Fprintf(&b, "Целевая архитектура после изменения. Описание изменения: [%s.md](%s.md).\n\n", ch.Info.ID, ch.Info.ID)
	fmt.Fprintf(&b, "## %s\n\n%s\n", v.Context.Title, fenced(v.Context.Mermaid))
	for _, s := range v.Containers {
		fmt.Fprintf(&b, "## %s\n\n%s\n", s.View.Title, fenced(s.View.Mermaid))
	}
	for _, s := range v.Components {
		fmt.Fprintf(&b, "## %s\n\n%s\n", s.View.Title, fenced(s.View.Mermaid))
	}
	b.WriteString("## Легенда\n\n| Цвет рамки | Состояние |\n|---|---|\n")
	for _, l := range v.Legend {
		fmt.Fprintf(&b, "| `%s` | %s |\n", l.Color, l.Title)
	}
	return b.String()
}
