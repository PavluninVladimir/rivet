package architecture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sort"
	"strings"
)

// generatedMark открывает каждый файл views/: каталог целиком
// генерируется, ручные правки в нём перезаписываются.
const generatedMark = "<!-- Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается. -->\n\n"

// maxDiagramNodes — сколько элементов слоя рисуется одной диаграммой.
// Большие диаграммы GitHub рисует плохо, поэтому слой делится по типам.
const maxDiagramNodes = 60

// typeOrder — порядок подграфов по типам: слои, внутри слоя как в таблице
// типов.
var typeOrder = func() []string {
	var out []string
	for _, l := range builtinLayers {
		out = append(out, layerTypes[l.id]...)
	}
	return out
}()

const classDefs = `  classDef added fill:#e6f4ea,stroke:#1e8e3e,color:#0d652d
  classDef modified fill:#fef7e0,stroke:#e37400,color:#7a4100
  classDef removed fill:#fce8e6,stroke:#d93025,color:#a50e0e,stroke-dasharray: 5 5
  classDef ext fill:#f1f3f4,stroke:#9aa0a6,color:#5f6368,stroke-dasharray: 3 3
`

// diagram — диаграмма Mermaid flowchart. Идентификаторы узлов выводятся
// из ключей детерминированно, поэтому вывод побайтно стабилен.
type diagram struct {
	b    strings.Builder
	ids  map[string]string
	used map[string]bool
}

func newDiagram() *diagram {
	d := &diagram{ids: map[string]string{}, used: map[string]bool{}}
	d.b.WriteString("```mermaid\nflowchart LR\n")
	return d
}

// id возвращает идентификатор узла Mermaid для ключа. Префикс n_ не даёт
// совпасть с ключевыми словами Mermaid (end, graph, subgraph).
func (d *diagram) id(key string) string {
	if id, ok := d.ids[key]; ok {
		return id
	}
	var b strings.Builder
	b.WriteString("n_")
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	id := b.String()
	for n := 2; d.used[id]; n++ {
		id = fmt.Sprintf("%s_%d", b.String(), n)
	}
	d.used[id] = true
	d.ids[key] = id
	return id
}

func (d *diagram) line(indent int, format string, args ...any) {
	d.b.WriteString(strings.Repeat("  ", indent))
	fmt.Fprintf(&d.b, format, args...)
	d.b.WriteByte('\n')
}

func (d *diagram) String() string {
	return d.b.String() + "```\n"
}

// mlabel — подпись Mermaid в кавычках.
func mlabel(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		s = " "
	}
	return `"` + strings.NewReplacer(`"`, "#quot;", "<", "#lt;", ">", "#gt;", "|", "#124;").Replace(s) + `"`
}

// cell — значение ячейки Markdown-таблицы.
func cell(s string) string {
	return strings.ReplaceAll(strings.Join(strings.Fields(s), " "), "|", `\|`)
}

func code(ids []string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = "`" + id + "`"
	}
	return strings.Join(out, ", ")
}

func elementLabel(e *Element) string {
	if e.Name != "" {
		return e.Name
	}
	return e.ID
}

// graphEdge — ребро диаграммы элементов.
type graphEdge struct {
	from, to, label string
	dotted          bool
}

func relationLabel(rel Relation) string {
	if rel.Mode != "" {
		return rel.Type + ": " + rel.Mode
	}
	return rel.Type
}

// writeElements рисует элементы подграфами по типам. Концы рёбер вне focus
// рисуются серым пунктиром как соседи.
func writeElements(d *diagram, focus []*Element, lookup func(string) *Element, edges []graphEdge, classes map[string]string) {
	in := map[string]bool{}
	byType := map[string][]*Element{}
	for _, e := range focus {
		in[e.ID] = true
		byType[e.Type] = append(byType[e.Type], e)
	}
	types := slices.Clone(typeOrder)
	for _, t := range sortedKeys(byType) {
		if !slices.Contains(types, t) {
			types = append(types, t)
		}
	}
	for _, t := range types {
		list := byType[t]
		if len(list) == 0 {
			continue
		}
		title := typeTitles[t]
		if title == "" {
			title = t
		}
		d.line(1, "subgraph %s[%s]", d.id("type:"+t), mlabel(title))
		for _, e := range list {
			d.line(2, "%s[%s]", d.id(e.ID), mlabel(elementLabel(e)))
		}
		d.line(1, "end")
	}
	var ext []string
	for _, e := range edges {
		for _, id := range []string{e.from, e.to} {
			if !in[id] && !slices.Contains(ext, id) {
				ext = append(ext, id)
			}
		}
	}
	sort.Strings(ext)
	for _, id := range ext {
		label := id
		if e := lookup(id); e != nil {
			label = elementLabel(e)
		}
		d.line(1, "%s[%s]", d.id(id), mlabel(label))
	}
	sort.SliceStable(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.from != b.from {
			return a.from < b.from
		}
		if a.to != b.to {
			return a.to < b.to
		}
		return a.label < b.label
	})
	for _, e := range edges {
		arrow := "-->"
		switch {
		case e.dotted:
			arrow = "-.->"
		case e.label == RelAssociation:
			arrow = "---"
		}
		d.line(1, "%s %s|%s| %s", d.id(e.from), arrow, mlabel(e.label), d.id(e.to))
	}
	d.b.WriteString(classDefs)
	byClass := map[string][]string{}
	for _, id := range ext {
		byClass["ext"] = append(byClass["ext"], d.id(id))
	}
	for _, e := range focus {
		if c := classes[e.ID]; c != "" {
			byClass[c] = append(byClass[c], d.id(e.ID))
		}
	}
	for _, c := range sortedKeys(byClass) {
		d.line(1, "class %s %s", strings.Join(byClass[c], ","), c)
	}
}

// relationEdges — связи модели, у которых хотя бы один конец в focus.
func relationEdges(m *Model, focus map[string]bool) []graphEdge {
	var out []graphEdge
	for _, e := range m.Elements() {
		for _, rel := range e.Relations {
			if (focus[e.ID] || focus[rel.To]) && m.element(rel.To) != nil {
				out = append(out, graphEdge{from: e.ID, to: rel.To, label: relationLabel(rel)})
			}
		}
	}
	return out
}

func docElements(d *Doc) []*Element {
	out := make([]*Element, 0, len(d.records))
	for _, rec := range d.records {
		out = append(out, rec.Element)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func renderLayer(m *Model, l *layer, d *Doc) string {
	var b strings.Builder
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "# %s\n\n", l.title)
	elems := docElements(d)
	if len(elems) == 0 {
		b.WriteString("Элементов нет.\n")
		return b.String()
	}
	groups := [][]*Element{elems}
	split := len(elems) > maxDiagramNodes
	if split {
		groups = nil
		for _, t := range layerTypes[l.id] {
			var g []*Element
			for _, e := range elems {
				if e.Type == t {
					g = append(g, e)
				}
			}
			if len(g) > 0 {
				groups = append(groups, g)
			}
		}
	}
	for _, g := range groups {
		if split {
			fmt.Fprintf(&b, "## %s\n\n", typeTitles[g[0].Type])
		}
		focus := map[string]bool{}
		for _, e := range g {
			focus[e.ID] = true
		}
		dg := newDiagram()
		writeElements(dg, g, m.element, relationEdges(m, focus), nil)
		b.WriteString(dg.String())
		b.WriteString("\n")
	}
	b.WriteString("| Элемент | Тип | Название | Описание |\n|---|---|---|---|\n")
	for _, e := range elems {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", e.ID, e.Type, cell(e.Name), cell(e.Description))
	}
	return b.String()
}

func nodeShape(d *diagram, n *FlowNode) string {
	id := d.id("node:" + n.ID)
	switch n.Kind {
	case "startEvent", "intermediateCatchEvent", "intermediateThrowEvent":
		return fmt.Sprintf("%s((%s))", id, mlabel(n.Name))
	case "endEvent":
		return fmt.Sprintf("%s(((%s)))", id, mlabel(n.Name))
	case "exclusiveGateway":
		return fmt.Sprintf("%s{%s}", id, mlabel(cmp(n.Name, "X")))
	case "parallelGateway":
		return fmt.Sprintf("%s{%s}", id, mlabel(cmp(n.Name, "+")))
	}
	if !n.Supported {
		return fmt.Sprintf("%s[[%s]]", id, mlabel(cmp(n.Name, n.ID)))
	}
	return fmt.Sprintf("%s[%s]", id, mlabel(cmp(n.Name, n.ID)))
}

func cmp(s, fallback string) string {
	if s != "" {
		return s
	}
	return fallback
}

// processDiagram рисует процесс: дорожки — подграфы, события — круги,
// задачи — прямоугольники, шлюзы — ромбы.
func processDiagram(p *Process) string {
	d := newDiagram()
	placed := map[string]bool{}
	var lane func(l Lane, indent int)
	lane = func(l Lane, indent int) {
		d.line(indent, "subgraph %s[%s]", d.id("lane:"+l.ID), mlabel(cmp(l.Name, l.ID)))
		for _, ref := range l.Nodes {
			if n := p.node(ref); n != nil && !placed[ref] {
				placed[ref] = true
				d.line(indent+1, "%s", nodeShape(d, n))
			}
		}
		for _, c := range l.Lanes {
			lane(c, indent+1)
		}
		d.line(indent, "end")
	}
	for _, l := range p.Lanes {
		lane(l, 1)
	}
	for i := range p.Nodes {
		if n := &p.Nodes[i]; !placed[n.ID] {
			d.line(1, "%s", nodeShape(d, n))
		}
	}
	for _, f := range p.Flows {
		if p.node(f.Source) == nil || p.node(f.Target) == nil {
			continue
		}
		if f.Name != "" {
			d.line(1, "%s -->|%s| %s", d.id("node:"+f.Source), mlabel(f.Name), d.id("node:"+f.Target))
		} else {
			d.line(1, "%s --> %s", d.id("node:"+f.Source), d.id("node:"+f.Target))
		}
	}
	return d.String()
}

func renderProcess(e *Element, p *Process) string {
	var b strings.Builder
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "# Процесс: %s (`%s`)\n\n", elementLabel(e), e.ID)
	if e.Description != "" {
		b.WriteString(strings.TrimSpace(e.Description) + "\n\n")
	}
	writeProcessBody(&b, p)
	return b.String()
}

func writeProcessBody(b *strings.Builder, p *Process) {
	b.WriteString(processDiagram(p))
	var lanes [][2]string
	var walk func([]Lane)
	walk = func(ls []Lane) {
		for _, l := range ls {
			if l.Ref != "" {
				lanes = append(lanes, [2]string{cmp(l.Name, l.ID), l.Ref})
			}
			walk(l.Lanes)
		}
	}
	walk(p.Lanes)
	var tasks [][2]string
	for _, n := range p.Nodes {
		if n.Ref != "" && n.Kind == "serviceTask" {
			tasks = append(tasks, [2]string{cmp(n.Name, n.ID), n.Ref})
		}
	}
	for _, t := range []struct {
		head string
		rows [][2]string
	}{{"Дорожка", lanes}, {"Сервисная задача", tasks}} {
		if len(t.rows) == 0 {
			continue
		}
		fmt.Fprintf(b, "\n| %s | Элемент модели |\n|---|---|\n", t.head)
		for _, r := range t.rows {
			fmt.Fprintf(b, "| %s | `%s` |\n", cell(r[0]), r[1])
		}
	}
}

func cellValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []any:
		parts := make([]string, len(x))
		for i, val := range x {
			parts[i] = cellValue(val)
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		raw, _ := json.Marshal(x)
		return string(raw)
	}
	return fmt.Sprint(v)
}

func renderCustom(m *Model, c *customViewpoint, d *Doc) string {
	var b strings.Builder
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "# %s\n\n", c.def.Title)
	recs := slices.Clone(d.records)
	sort.Slice(recs, func(i, j int) bool { return recs[i].ID < recs[j].ID })
	if len(recs) == 0 {
		b.WriteString("Записей нет.\n")
		return b.String()
	}
	label := cmp(c.def.Render.Label, "id")
	columns := c.def.Render.Columns
	if c.def.Render.Kind == "graph" {
		dg := newDiagram()
		for _, rec := range recs {
			dg.line(1, "%s[%s]", dg.id("item:"+rec.ID), mlabel(c.recordTitle(rec)))
		}
		var targets []string
		type edge struct{ from, to, label string }
		var edges []edge
		for _, ref := range c.references(d) {
			if ref.From == "" || !slices.Contains(c.def.Render.Edges, ref.Label) || m.element(ref.To) == nil {
				continue
			}
			edges = append(edges, edge{ref.From, ref.To, ref.Label})
			if !slices.Contains(targets, ref.To) {
				targets = append(targets, ref.To)
			}
		}
		sort.Strings(targets)
		for _, id := range targets {
			dg.line(1, "%s[%s]", dg.id(id), mlabel(elementLabel(m.element(id))))
		}
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].from != edges[j].from {
				return edges[i].from < edges[j].from
			}
			if edges[i].to != edges[j].to {
				return edges[i].to < edges[j].to
			}
			return edges[i].label < edges[j].label
		})
		for _, e := range edges {
			dg.line(1, "%s -->|%s| %s", dg.id("item:"+e.from), mlabel(e.label), dg.id(e.to))
		}
		dg.b.WriteString(classDefs)
		if len(targets) > 0 {
			ids := make([]string, len(targets))
			for i, id := range targets {
				ids[i] = dg.id(id)
			}
			dg.line(1, "class %s ext", strings.Join(ids, ","))
		}
		b.WriteString(dg.String())
		b.WriteString("\n")
		if len(columns) == 0 {
			columns = []string{"id"}
			if label != "id" {
				columns = append(columns, label)
			}
		}
	}
	if len(columns) == 0 {
		keys := map[string]bool{}
		for _, rec := range recs {
			for k := range rec.Value {
				if k != "id" {
					keys[k] = true
				}
			}
		}
		columns = append([]string{"id"}, sortedKeys(keys)...)
	}
	b.WriteString("| " + strings.Join(columns, " | ") + " |\n|" + strings.Repeat("---|", len(columns)) + "\n")
	for _, rec := range recs {
		row := make([]string, len(columns))
		for i, col := range columns {
			if col == "id" {
				row[i] = "`" + rec.ID + "`"
			} else {
				row[i] = cell(cellValue(rec.Value[col]))
			}
		}
		b.WriteString("| " + strings.Join(row, " | ") + " |\n")
	}
	return b.String()
}

func renderChange(ch *Change) string {
	var b strings.Builder
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "# Изменение: %s (`%s`)\n\n", cmp(ch.Info.Title, ch.Info.ID), ch.Info.ID)
	if ch.Info.Epic != "" {
		fmt.Fprintf(&b, "Epic: `%s`\n\n", ch.Info.Epic)
	}
	fmt.Fprintf(&b, "## Видение\n\n%s\n\n", demoteHeadings(strings.TrimSpace(ch.Vision)))

	b.WriteString("## Целевая модель\n\n")
	classes := map[string]string{}
	var focus []*Element
	var dotted []graphEdge
	for _, d := range ch.Deltas {
		if !d.Viewpoint.Builtin() {
			continue
		}
		for _, rec := range d.Added {
			if e := ch.Target.element(rec.ID); e != nil {
				focus = append(focus, e)
				classes[rec.ID] = "added"
			}
		}
		for _, rec := range d.Modified {
			if e := ch.Target.element(rec.ID); e != nil {
				focus = append(focus, e)
				classes[rec.ID] = "modified"
			}
		}
		for _, rm := range d.Removed {
			if e := ch.Current.element(rm.ID); e != nil {
				focus = append(focus, e)
				classes[rm.ID] = "removed"
				for _, rel := range e.Relations {
					if ch.Target.element(rel.To) != nil || ch.Current.element(rel.To) != nil {
						dotted = append(dotted, graphEdge{from: e.ID, to: rel.To, label: relationLabel(rel), dotted: true})
					}
				}
			}
		}
	}
	if len(focus) == 0 {
		b.WriteString("Дельта не меняет элементы слоёв.\n\n")
	} else {
		sort.Slice(focus, func(i, j int) bool { return focus[i].ID < focus[j].ID })
		in := map[string]bool{}
		for _, e := range focus {
			in[e.ID] = true
		}
		edges := append(relationEdges(ch.Target, in), dotted...)
		lookup := func(id string) *Element {
			if e := ch.Target.element(id); e != nil {
				return e
			}
			return ch.Current.element(id)
		}
		b.WriteString("Зелёным отмечены добавленные элементы, жёлтым изменённые, красным пунктиром удалённые. " +
			"Серым пунктиром показаны соседние элементы без изменений.\n\n")
		dg := newDiagram()
		writeElements(dg, focus, lookup, edges, classes)
		b.WriteString(dg.String())
		b.WriteString("\n")
	}

	b.WriteString("## Разрывы\n\n")
	gaps := slices.Clone(ch.Gaps)
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].ID < gaps[j].ID })
	b.WriteString("| Разрыв | Вид | Название | Элементы | Процессы |\n|---|---|---|---|---|\n")
	for _, g := range gaps {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", g.ID, g.Kind, cell(g.Title), code(g.Elements), code(g.Processes))
	}

	b.WriteString("\n## Записи дельты\n\n| Запись | Viewpoint | Изменение | Описание |\n|---|---|---|---|\n")
	type row struct{ id, vp, kind, text string }
	var rows []row
	for _, d := range ch.Deltas {
		name := d.Viewpoint.recordTitle
		for _, rec := range d.Added {
			rows = append(rows, row{rec.ID, d.Viewpoint.ID(), "добавлен", name(rec)})
		}
		for _, rec := range d.Modified {
			rows = append(rows, row{rec.ID, d.Viewpoint.ID(), "изменён", name(rec)})
		}
		for _, rm := range d.Removed {
			rows = append(rows, row{rm.ID, d.Viewpoint.ID(), "удалён", rm.Reason})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].id < rows[j].id })
	for _, r := range rows {
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", r.id, r.vp, r.kind, cell(r.text))
	}

	if len(ch.Processes) > 0 {
		b.WriteString("\n## Целевые процессы\n")
		for _, id := range sortedKeys(ch.Processes) {
			title := id
			if e := ch.Target.element(id); e != nil {
				title = elementLabel(e)
			}
			fmt.Fprintf(&b, "\n### %s (`%s`)\n\n", title, id)
			writeProcessBody(&b, ch.Processes[id])
		}
	}
	return b.String()
}

// demoteHeadings опускает заголовки Markdown на уровень ниже, чтобы
// видение встало внутрь раздела «Видение». Блоки кода не трогаются.
func demoteHeadings(md string) string {
	lines := strings.Split(md, "\n")
	fence := false
	for i, l := range lines {
		if strings.HasPrefix(l, "```") || strings.HasPrefix(l, "~~~") {
			fence = !fence
			continue
		}
		if !fence && strings.HasPrefix(l, "#") {
			lines[i] = "#" + l
		}
	}
	return strings.Join(lines, "\n")
}

func renderIndex(m *Model, changes []*Change) string {
	var b strings.Builder
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "# Архитектура: %s\n\n", m.Manifest.System)
	if len(m.Manifest.Principles) > 0 {
		b.WriteString("## Принципы\n\n| Принцип | Формулировка | Обоснование |\n|---|---|---|\n")
		for _, p := range m.Manifest.Principles {
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", p.ID, cell(p.Title), cell(p.Rationale))
		}
		b.WriteString("\n")
	}
	b.WriteString("## Viewpoint'ы\n\n")
	for _, vp := range m.viewpoints {
		fmt.Fprintf(&b, "- [%s](%s.md): записей %d\n", vp.Title(), vp.ID(), len(m.docs[vp.ID()].records))
	}
	if procs := processElements(m); len(procs) > 0 {
		b.WriteString("\n## Процессы\n\n")
		for _, e := range procs {
			fmt.Fprintf(&b, "- [%s](processes/%s.md)\n", elementLabel(e), e.ID)
		}
	}
	if len(changes) > 0 {
		b.WriteString("\n## Изменения\n\n")
		for _, ch := range changes {
			fmt.Fprintf(&b, "- [%s](changes/%s.md)\n", cmp(ch.Info.Title, ch.Info.ID), ch.Info.ID)
		}
	}
	return b.String()
}

func processElements(m *Model) []*Element {
	var out []*Element
	for _, e := range m.Elements() {
		if e.Type == TypeProcess && m.processes[e.ID] != nil {
			out = append(out, m.element(e.ID))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Views строит представления описания и активных изменений: путь от
// корня описания → содержимое. При ошибках в текущем описании
// представлений нет. Изменение с ошибками пропускается, его дефекты
// возвращаются.
func Views(fsys fs.FS) (map[string][]byte, Defects, error) {
	r := newReporter()
	m, err := load(fsys, r)
	if err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, r.result(), nil
	}
	m.check(r, checkOptions{})
	if r.result().HasErrors() {
		return nil, r.result(), nil
	}
	ids, err := ActiveChanges(fsys)
	if err != nil {
		return nil, nil, err
	}
	var changes []*Change
	for _, id := range ids {
		cr := newReporter()
		ch, err := loadChange(fsys, m, id, cr)
		if err != nil {
			return nil, nil, err
		}
		ds := cr.result()
		r.merge(ds)
		if ch != nil && !ds.HasErrors() {
			changes = append(changes, ch)
		}
	}
	out := map[string][]byte{}
	out[ViewsDir+"/README.md"] = []byte(renderIndex(m, changes))
	for _, vp := range m.viewpoints {
		out[ViewsDir+"/"+vp.ID()+".md"] = []byte(vp.render(m, m.docs[vp.ID()]))
	}
	for _, e := range processElements(m) {
		out[ViewsDir+"/processes/"+e.ID+".md"] = []byte(renderProcess(e, m.processes[e.ID]))
	}
	for _, ch := range changes {
		out[ViewsDir+"/changes/"+ch.Info.ID+".md"] = []byte(renderChange(ch))
	}
	return out, r.result(), nil
}

// existingViews возвращает файлы в views/, кроме скрытых.
func existingViews(fsys fs.FS) ([]string, error) {
	var out []string
	err := fs.WalkDir(fsys, ViewsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			out = append(out, p)
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return out, err
}

// CheckViews сверяет представления на диске с описанием. Устаревшие,
// недостающие и лишние файлы возвращаются дефектами views.stale.
func CheckViews(fsys fs.FS) (Defects, error) {
	gen, ds, err := Views(fsys)
	if err != nil {
		return nil, err
	}
	r := newReporter()
	r.merge(ds)
	if gen == nil {
		return r.result(), nil
	}
	for _, p := range sortedKeys(gen) {
		raw, err := fs.ReadFile(fsys, p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			r.errorf("views.stale", p, "", "представления нет, запусти rivet architecture views")
		case err != nil:
			return nil, err
		case !bytes.Equal(raw, gen[p]):
			r.errorf("views.stale", p, "", "представление устарело, запусти rivet architecture views")
		}
	}
	existing, err := existingViews(fsys)
	if err != nil {
		return nil, err
	}
	for _, p := range existing {
		if _, ok := gen[p]; !ok {
			r.errorf("views.stale", p, "", "лишний файл: views/ целиком создаётся генератором")
		}
	}
	return r.result(), nil
}

// WriteViews перегенерирует views/ в каталоге описания: пишет
// представления и удаляет лишние файлы.
func WriteViews(dir string) (Defects, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrAbsent
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return writeViews(root)
}

func writeViews(root *os.Root) (Defects, error) {
	fsys := root.FS()
	gen, ds, err := Views(fsys)
	if err != nil || gen == nil {
		return ds, err
	}
	for _, p := range sortedKeys(gen) {
		if err := writeFile(root, p, gen[p]); err != nil {
			return nil, err
		}
	}
	existing, err := existingViews(fsys)
	if err != nil {
		return nil, err
	}
	for _, p := range existing {
		if _, ok := gen[p]; !ok {
			if err := root.Remove(p); err != nil {
				return nil, err
			}
		}
	}
	return ds, nil
}
