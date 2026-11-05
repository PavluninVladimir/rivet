package architecture

import (
	"fmt"
	"sort"
	"strings"
)

// Экспорт модели C4 в Structurizr DSL (спека «Экспорт в Structurizr DSL»):
// полная модель для внешних инструментов. Идентификаторы DSL выводятся из
// идентификаторов элементов: всё, кроме строчных букв и цифр, заменяется
// на «_». Редкое совпадение (app.a-b и app.a.b) разводится суффиксом _2 по
// порядку id, поэтому вывод однозначен и детерминирован. Описываемая
// система получает идентификатор system: у элементов в идентификаторе
// всегда есть «_» от точки, и с ней они не совпадают.

// dslEnvironment — окружение развёртывания в DSL.
const dslEnvironment = "Основное"

// dslIdentifiers — идентификаторы DSL всех элементов модели C4.
func dslIdentifiers(c *C4Model) map[string]string {
	out := map[string]string{DescribedSystemID: DescribedSystemID}
	used := map[string]bool{DescribedSystemID: true}
	ids := make([]string, 0, len(c.Elements))
	for _, e := range c.Elements {
		if e.ID != DescribedSystemID {
			ids = append(ids, e.ID)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		var b strings.Builder
		for _, r := range id {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
			} else {
				b.WriteByte('_')
			}
		}
		v := b.String()
		for n := 2; used[v]; n++ {
			v = fmt.Sprintf("%s_%d", b.String(), n)
		}
		used[v] = true
		out[id] = v
	}
	return out
}

// uniqueNames — имена группы элементов одной области DSL. Structurizr не
// открывает модель, где в одной области (люди и системы верхнего уровня,
// контейнеры системы, компоненты контейнера, соседние узлы развёртывания)
// два элемента с одним именем. Каждому элементу группы совпадений
// дописывается « (<id>)». Сравнение без учёта регистра и пробелов.
func uniqueNames(ids []string, name func(string) string) map[string]string {
	count := map[string]int{}
	key := func(id string) string { return strings.ToLower(strings.Join(strings.Fields(name(id)), " ")) }
	for _, id := range ids {
		count[key(id)]++
	}
	out := map[string]string{}
	for _, id := range ids {
		out[id] = name(id)
		if count[key(id)] > 1 {
			out[id] = name(id) + " (" + id + ")"
		}
	}
	return out
}

// dslNames — имена элементов C4 в DSL, уникальные в своей области.
func dslNames(c *C4Model) map[string]string {
	groups := map[string][]string{}
	for _, e := range c.Elements {
		scope := "top"
		if e.Kind != C4Person && e.Kind != C4System {
			scope = "in:" + e.Parent
		}
		groups[scope] = append(groups[scope], e.ID)
	}
	out := map[string]string{}
	for _, ids := range groups {
		for id, n := range uniqueNames(ids, func(id string) string { return c.byID[id].Name }) {
			out[id] = n
		}
	}
	return out
}

// dslq — строка DSL в кавычках.
func dslq(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func dslTags(e *C4Element) []string {
	var tags []string
	switch e.Kind {
	case C4Database:
		tags = append(tags, "Database")
	case C4Queue:
		tags = append(tags, "Queue")
	}
	if e.External {
		tags = append(tags, "External")
	}
	return tags
}

type dslWriter struct {
	b     strings.Builder
	ids   map[string]string
	names map[string]string
}

func (w *dslWriter) line(indent int, format string, args ...any) {
	w.b.WriteString(strings.Repeat("    ", indent))
	fmt.Fprintf(&w.b, format, args...)
	w.b.WriteByte('\n')
}

// element пишет объявление элемента, вложенные элементы и закрывает блок.
func (w *dslWriter) element(c *C4Model, e *C4Element, indent int) {
	var decl string
	tags := strings.Join(dslTags(e), ",")
	name := w.names[e.ID]
	switch e.Kind {
	case C4Person:
		decl = fmt.Sprintf("person %s %s %s", dslq(name), dslq(e.Description), dslq(tags))
	case C4System:
		decl = fmt.Sprintf("softwareSystem %s %s %s", dslq(name), dslq(e.Description), dslq(tags))
	case C4Component:
		decl = fmt.Sprintf("component %s %s %s %s", dslq(name), dslq(e.Description), dslq(e.Technology), dslq(tags))
	default:
		decl = fmt.Sprintf("container %s %s %s %s", dslq(name), dslq(e.Description), dslq(e.Technology), dslq(tags))
	}
	children := c.children(e.ID)
	if e.Kind == C4Person {
		children = nil
	}
	sortC4(children)
	if len(children) == 0 {
		w.line(indent, "%s = %s", w.ids[e.ID], decl)
		return
	}
	w.line(indent, "%s = %s {", w.ids[e.ID], decl)
	for _, ch := range children {
		w.element(c, ch, indent+1)
	}
	w.line(indent, "}")
}

func viewBlock(w *dslWriter, header string) {
	w.line(2, "%s {", header)
	w.line(3, "include *")
	w.line(3, "autoLayout lr")
	w.line(2, "}")
}

// renderDSL пишет workspace.dsl: модель, связи, окружение развёртывания и
// виды контекста, контейнеров, компонентов и развёртывания.
func renderDSL(c *C4Model) string {
	w := &dslWriter{ids: dslIdentifiers(c), names: dslNames(c)}
	sys := c.byID[DescribedSystemID]
	w.b.WriteString("// Сгенерировано командой rivet architecture views. Не редактируй вручную: файл перезаписывается.\n")
	w.line(0, "workspace %s %s {", dslq(sys.Name), dslq(sys.Description))
	w.line(1, "!identifiers flat")
	w.b.WriteString("\n")
	w.line(1, "model {")
	var top []*C4Element
	for i := range c.Elements {
		if e := &c.Elements[i]; e.Kind == C4Person || e.Kind == C4System {
			top = append(top, e)
		}
	}
	sortC4(top)
	for _, e := range top {
		w.element(c, e, 2)
	}
	if len(c.Rels) > 0 {
		w.b.WriteString("\n")
		for _, r := range c.Rels {
			w.line(2, "%s -> %s %s", w.ids[r.From], w.ids[r.To], dslq(r.Label()))
		}
	}
	if t, ok := c.deployTree(); ok {
		byID := map[string]*Element{}
		for _, h := range t.hosts {
			byID[h.ID] = h
		}
		w.b.WriteString("\n")
		w.line(2, "deploymentEnvironment %s {", dslq(dslEnvironment))
		hostName := func(id string) string { return elementLabel(byID[id]) }
		var walk func(id, name string, indent int)
		walk = func(id, name string, indent int) {
			h := byID[id]
			w.line(indent, "deploymentNode %s %s %s {", dslq(name), dslq(h.Description), dslq(hostType(h)))
			names := uniqueNames(t.children[id], hostName)
			for _, ch := range t.children[id] {
				walk(ch, names[ch], indent+1)
			}
			// Экземпляры те же, что на схеме Mermaid: контейнеры и базы
			// данных как containerInstance, внешняя система на узле как
			// softwareSystemInstance.
			for _, inst := range t.instances[id] {
				if c.byID[inst].Kind == C4System {
					w.line(indent+1, "softwareSystemInstance %s", w.ids[inst])
				} else {
					w.line(indent+1, "containerInstance %s", w.ids[inst])
				}
			}
			w.line(indent, "}")
		}
		names := uniqueNames(t.roots, hostName)
		for _, id := range t.roots {
			walk(id, names[id], 3)
		}
		w.line(2, "}")
	}
	w.line(1, "}")
	w.b.WriteString("\n")
	w.line(1, "views {")
	viewBlock(w, fmt.Sprintf("systemContext %s %s", w.ids[DescribedSystemID], dslq("context")))
	for _, id := range c.systemsWithContainers() {
		viewBlock(w, fmt.Sprintf("container %s %s", w.ids[id], dslq("containers-"+w.ids[id])))
	}
	for _, id := range c.containersWithComponents() {
		viewBlock(w, fmt.Sprintf("component %s %s", w.ids[id], dslq("components-"+w.ids[id])))
	}
	if _, ok := c.deployTree(); ok {
		viewBlock(w, fmt.Sprintf("deployment * %s %s", dslq(dslEnvironment), dslq("deployment")))
	}
	w.b.WriteString("\n")
	w.line(2, "styles {")
	for _, s := range [][2]string{
		{"Person", "shape Person"},
		{"Database", "shape Cylinder"},
		{"Queue", "shape Pipe"},
		{"External", "background #8a8a8a"},
	} {
		w.line(3, "element %s {", dslq(s[0]))
		w.line(4, "%s", s[1])
		w.line(3, "}")
	}
	w.line(2, "}")
	w.line(1, "}")
	w.line(0, "}")
	return w.b.String()
}
