package architecture

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ERD (спеки «Модель данных сущностей» и «ERD-представления»): атрибуты
// объектов данных и связи association с кардинальностью. Внешний ключ без
// явной связи даёт выведенную связь «многие к одному». Хранилище для ERD
// берётся из C4: база данных и объекты, к которым у неё есть access.

// erdDir — каталог ERD-представлений внутри views/. Не data/: шаблон
// data/ часто стоит в .gitignore проектов (каталог с данными приложения),
// и схемы молча не попали бы в репозиторий.
const erdDir = "erd"

// Attribute — атрибут объекта данных.
type Attribute struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	PK          bool   `json:"pk"`
	FK          *FKRef `json:"fk"`
	Nullable    bool   `json:"nullable"`
	Unique      bool   `json:"unique"`
	Description string `json:"description"`
}

// FKRef — внешний ключ: объект данных и его атрибут.
type FKRef struct {
	To        string `json:"to"`
	Attribute string `json:"attribute"`
}

// Кардинальности связи association между объектами данных.
const (
	CardOneToOne   = "one-to-one"
	CardOneToMany  = "one-to-many"
	CardManyToOne  = "many-to-one"
	CardManyToMany = "many-to-many"
)

// ERD — данные описания для снимка: общая схема, схемы хранилищ и
// структура сущностей для навигации.
type ERD struct {
	All      *View       `json:"all"`
	Stores   []ERDStore  `json:"stores"`
	Entities []ERDEntity `json:"entities"`
}

// ERDStore — схема одного хранилища.
type ERDStore struct {
	StoreID string `json:"store_id"`
	Title   string `json:"title"`
	View    View   `json:"view"`
}

// ERDEntity — сущность ERD: объект данных с атрибутами и связями.
type ERDEntity struct {
	ID         string        `json:"id"`
	Name       string        `json:"name"`
	Stores     []string      `json:"stores"`
	Attributes []Attribute   `json:"attributes"`
	Relations  []ERDRelation `json:"relations"`
}

// ERDRelation — связь сущности с другой. Derived — выведена из внешнего
// ключа, а не задана связью association.
type ERDRelation struct {
	To          string `json:"to"`
	Cardinality string `json:"cardinality"`
	Label       string `json:"label"`
	Derived     bool   `json:"derived"`
	// nullable — выведенная связь по внешнему ключу, который может быть
	// пустым: на схеме «многие к нулю или одному».
	nullable bool
}

// checkERD — правила erd.* по элементам модели.
func (m *Model) checkERD(r *reporter, opt checkOptions) {
	for _, vp := range m.viewpoints {
		if !vp.Builtin() {
			continue
		}
		for _, rec := range m.docs[vp.ID()].records {
			e := rec.Element
			if len(e.Attributes) > 0 && e.Type != TypeDataObject {
				r.errorf("erd.attributes-type", rec.File, e.ID,
					"атрибуты задаются только у объекта данных (data-object), а не у %s", e.Type)
			}
			for _, rel := range e.Relations {
				if rel.Cardinality == "" && rel.Label == "" {
					continue
				}
				t := m.element(rel.To)
				if rel.Type != RelAssociation || e.Type != TypeDataObject || t == nil || t.Type != TypeDataObject {
					r.errorf("erd.cardinality-type", rec.File, e.ID,
						"кардинальность и подпись задаются только у связи association между объектами данных (связь %s к %s)", rel.Type, rel.To)
				}
			}
			if e.Type != TypeDataObject || len(e.Attributes) == 0 {
				continue
			}
			seen := map[string]bool{}
			hasPK := false
			for _, a := range e.Attributes {
				if a.Name == "" {
					continue
				}
				if seen[a.Name] {
					r.errorf("erd.attribute-duplicate", rec.File, e.ID, "атрибут %s объявлен повторно", a.Name)
				}
				seen[a.Name] = true
				if a.PK {
					hasPK = true
					if a.Nullable {
						r.errorf("erd.pk-nullable", rec.File, e.ID, "первичный ключ %s не может быть nullable", a.Name)
					}
				}
				if a.FK == nil || a.FK.To == "" {
					continue
				}
				if opt.removed[a.FK.To] {
					r.errorf("erd.fk-entity", rec.File, e.ID,
						"внешний ключ %s ссылается на удаляемый объект данных %s", a.Name, a.FK.To)
					continue
				}
				t := m.element(a.FK.To)
				if t == nil || t.Type != TypeDataObject {
					r.errorf("erd.fk-entity", rec.File, e.ID,
						"внешний ключ %s ссылается на %s, а такого объекта данных нет", a.Name, a.FK.To)
					continue
				}
				if !hasAttribute(t, a.FK.Attribute) {
					r.errorf("erd.fk-attribute", rec.File, e.ID,
						"внешний ключ %s ссылается на %s.%s, а у объекта данных %s нет атрибута %s",
						a.Name, a.FK.To, a.FK.Attribute, a.FK.To, a.FK.Attribute)
				}
			}
			if !hasPK {
				r.errorf("erd.no-pk", rec.File, e.ID, "у объекта данных %s есть атрибуты, но ни один не отмечен pk", e.ID)
			}
		}
	}
}

func hasAttribute(e *Element, name string) bool {
	for _, a := range e.Attributes {
		if a.Name == name {
			return true
		}
	}
	return false
}

// sortedAttributes — атрибуты в стабильном порядке: первичные ключи, затем
// остальные, внутри по имени. Перестановка в файле схему не меняет.
func sortedAttributes(list []Attribute) []Attribute {
	out := make([]Attribute, len(list))
	copy(out, list)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PK != out[j].PK {
			return out[i].PK
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// buildERD собирает сущности: объекты данных по идентификатору, их
// атрибуты и связи. Явная связь с кардинальностью перекрывает выведенную
// по внешнему ключу для той же пары объектов.
func buildERD(m *Model, c *C4Model) []ERDEntity {
	var objects []*Element
	for _, e := range m.Elements() {
		if e.Type == TypeDataObject {
			objects = append(objects, m.element(e.ID))
		}
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].ID < objects[j].ID })
	isObject := map[string]bool{}
	for _, o := range objects {
		isObject[o.ID] = true
	}
	explicit := map[[2]string]bool{}
	pair := func(a, b string) [2]string {
		if a > b {
			a, b = b, a
		}
		return [2]string{a, b}
	}
	for _, o := range objects {
		for _, rel := range o.Relations {
			if rel.Type == RelAssociation && rel.Cardinality != "" && isObject[rel.To] && rel.To != o.ID {
				explicit[pair(o.ID, rel.To)] = true
			}
		}
	}
	out := make([]ERDEntity, 0, len(objects))
	for _, o := range objects {
		ent := ERDEntity{
			ID: o.ID, Name: cmp(strings.Join(strings.Fields(o.Name), " "), o.ID),
			Stores:     append([]string{}, c.storers[o.ID]...),
			Attributes: sortedAttributes(o.Attributes),
			Relations:  []ERDRelation{},
		}
		if ent.Attributes == nil {
			ent.Attributes = []Attribute{}
		}
		seen := map[string]bool{}
		for _, rel := range o.Relations {
			if rel.Type != RelAssociation || rel.Cardinality == "" || !isObject[rel.To] || rel.To == o.ID {
				continue
			}
			k := rel.To + "\x00" + rel.Cardinality + "\x00" + rel.Label
			if !seen[k] {
				seen[k] = true
				ent.Relations = append(ent.Relations, ERDRelation{To: rel.To, Cardinality: rel.Cardinality, Label: rel.Label})
			}
		}
		for _, a := range ent.Attributes {
			if a.FK == nil || !isObject[a.FK.To] || explicit[pair(o.ID, a.FK.To)] {
				continue
			}
			k := a.FK.To + "\x00fk\x00" + a.Name
			if !seen[k] {
				seen[k] = true
				ent.Relations = append(ent.Relations, ERDRelation{
					To: a.FK.To, Cardinality: CardManyToOne, Label: a.Name, Derived: true, nullable: a.Nullable,
				})
			}
		}
		sort.SliceStable(ent.Relations, func(i, j int) bool {
			a, b := ent.Relations[i], ent.Relations[j]
			if a.To != b.To {
				return a.To < b.To
			}
			return a.Label < b.Label
		})
		out = append(out, ent)
	}
	return out
}

var erdTokenBad = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// erdToken приводит тип или имя атрибута к токену Mermaid erDiagram:
// латиница, цифры и подчёркивание, первый символ не цифра. Слова PK, FK и
// UK в любом регистре Mermaid читает как ключ атрибута, поэтому к ним
// добавляется подчёркивание.
func erdToken(s, fallback string) string {
	t := strings.Trim(erdTokenBad.ReplaceAllString(s, "_"), "_")
	if t == "" {
		return fallback
	}
	if t[0] >= '0' && t[0] <= '9' {
		t = "_" + t
	}
	switch strings.ToLower(t) {
	case "pk", "fk", "uk":
		t += "_"
	}
	return t
}

// erdText — текст в кавычках erDiagram: одна строка без кавычек и символов,
// которые Mermaid читает как комментарий (%%), сущность (#;) или escape.
// Пустой после нормализации текст заменяется на fallback.
func erdText(s, fallback string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.NewReplacer(`"`, "'", "#", "№", ";", ",", "%", "％", `\`, "∖", "<", "‹", ">", "›").Replace(s)
	if s == "" {
		return fallback
	}
	return s
}

// erdArrow — запись связи erDiagram от первой сущности ко второй.
func erdArrow(rel ERDRelation) string {
	switch rel.Cardinality {
	case CardOneToOne:
		return "||--||"
	case CardOneToMany:
		return "||--o{"
	case CardManyToMany:
		return "}o--o{"
	}
	if rel.nullable {
		return "}o--o|"
	}
	return "}o--||"
}

// erdDiagram — текст Mermaid erDiagram по сущностям focus. Сущности вне
// focus, с которыми есть связи, рисуются без атрибутов с подписью foreign.
func erdDiagram(ents []ERDEntity, focus map[string]bool, foreign func(*ERDEntity) string) string {
	byID := map[string]*ERDEntity{}
	keys := map[string]bool{}
	for i := range ents {
		byID[ents[i].ID] = &ents[i]
		keys[ents[i].ID] = true
	}
	ids := &diagram{ids: map[string]string{}, used: map[string]bool{}}
	for _, k := range sortedKeys(keys) {
		ids.id(k)
	}
	type edge struct {
		from string
		rel  ERDRelation
	}
	var edges []edge
	extra := map[string]bool{}
	for _, e := range ents {
		for _, rel := range e.Relations {
			if !focus[e.ID] && !focus[rel.To] {
				continue
			}
			edges = append(edges, edge{e.ID, rel})
			for _, x := range []string{e.ID, rel.To} {
				if !focus[x] {
					extra[x] = true
				}
			}
		}
	}
	var b strings.Builder
	b.WriteString("erDiagram\n")
	for _, e := range ents {
		if !focus[e.ID] {
			continue
		}
		fmt.Fprintf(&b, "  %s[\"%s\"]", ids.id(e.ID), erdText(e.Name, e.ID))
		if len(e.Attributes) == 0 {
			b.WriteString(" {\n  }\n")
			continue
		}
		b.WriteString(" {\n")
		for _, a := range e.Attributes {
			typ := erdToken(a.Type, "any")
			name := erdToken(a.Name, "attr")
			var keysList []string
			if a.PK {
				keysList = append(keysList, "PK")
			}
			if a.FK != nil {
				keysList = append(keysList, "FK")
			}
			if a.Unique {
				keysList = append(keysList, "UK")
			}
			var notes []string
			if d := strings.TrimSpace(a.Description); d != "" {
				notes = append(notes, d)
			}
			if name != a.Name {
				notes = append(notes, "имя: "+a.Name)
			}
			if typ != a.Type {
				notes = append(notes, "тип: "+a.Type)
			}
			if a.FK != nil {
				notes = append(notes, "→ "+a.FK.To+"."+a.FK.Attribute)
			}
			if a.Nullable {
				notes = append(notes, "может быть пустым")
			}
			fmt.Fprintf(&b, "    %s %s", typ, name)
			if len(keysList) > 0 {
				b.WriteString(" " + strings.Join(keysList, ", "))
			}
			if len(notes) > 0 {
				fmt.Fprintf(&b, " \"%s\"", erdText(strings.Join(notes, ", "), ""))
			}
			b.WriteString("\n")
		}
		b.WriteString("  }\n")
	}
	for _, x := range sortedKeys(extra) {
		e := byID[x]
		fmt.Fprintf(&b, "  %s[\"%s\"] {\n  }\n", ids.id(x), erdText(e.Name+" · "+foreign(e), e.ID))
	}
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].from != edges[j].from {
			return edges[i].from < edges[j].from
		}
		if edges[i].rel.To != edges[j].rel.To {
			return edges[i].rel.To < edges[j].rel.To
		}
		return edges[i].rel.Label < edges[j].rel.Label
	})
	for _, ed := range edges {
		fmt.Fprintf(&b, "  %s %s %s : \"%s\"\n", ids.id(ed.from), erdArrow(ed.rel), ids.id(ed.rel.To), erdText(ed.rel.Label, ""))
	}
	return b.String()
}

// erdStores — хранилища, у которых есть объекты данных, по идентификатору.
func erdStores(ents []ERDEntity) []string {
	set := map[string]bool{}
	for _, e := range ents {
		for _, s := range e.Stores {
			set[s] = true
		}
	}
	return sortedKeys(set)
}

func storeNames(c *C4Model, ids []string) string {
	if len(ids) == 0 {
		return "вне хранилищ"
	}
	var names []string
	for _, id := range ids {
		if e := c.byID[id]; e != nil {
			names = append(names, e.Name)
		} else {
			names = append(names, id)
		}
	}
	return strings.Join(names, ", ")
}

// erdAllView и erdStoreView — общая схема и схема хранилища.
func erdAllView(c *C4Model, ents []ERDEntity) View {
	focus := map[string]bool{}
	for _, e := range ents {
		focus[e.ID] = true
	}
	return View{Title: "Данные: " + c.byID[DescribedSystemID].Name, Mermaid: erdDiagram(ents, focus, nil)}
}

func erdStoreView(c *C4Model, ents []ERDEntity, store string) View {
	focus := map[string]bool{}
	for _, e := range ents {
		for _, s := range e.Stores {
			if s == store {
				focus[e.ID] = true
			}
		}
	}
	foreign := func(e *ERDEntity) string { return storeNames(c, e.Stores) }
	return View{Title: "Данные: " + c.byID[store].Name, Mermaid: erdDiagram(ents, focus, foreign)}
}

// BuildERD — данные ERD описания для снимка. All равно nil без объектов
// данных.
func (m *Model) BuildERD(c *C4Model) ERD {
	ents := buildERD(m, c)
	out := ERD{Stores: []ERDStore{}, Entities: ents}
	if len(ents) == 0 {
		return out
	}
	all := erdAllView(c, ents)
	out.All = &all
	for _, s := range erdStores(ents) {
		out.Stores = append(out.Stores, ERDStore{StoreID: s, Title: c.byID[s].Name, View: erdStoreView(c, ents, s)})
	}
	return out
}

// erdFiles — представления views/erd/: общая схема и схемы хранилищ.
func erdFiles(m *Model, c *C4Model) map[string]string {
	ents := buildERD(m, c)
	if len(ents) == 0 {
		return nil
	}
	stores := erdStores(ents)
	out := map[string]string{}
	var all strings.Builder
	all.WriteString(generatedMark)
	fmt.Fprintf(&all, "# Данные: %s\n\nОбщая схема всех объектов данных описания.", c.byID[DescribedSystemID].Name)
	if len(stores) > 0 {
		var links []string
		for _, s := range stores {
			links = append(links, fmt.Sprintf("[%s](%s.md)", linkText(c.byID[s].Name), s))
		}
		all.WriteString(" По хранилищам: " + strings.Join(links, ", ") + ".")
	}
	all.WriteString("\n\n")
	all.WriteString(fenced(erdAllView(c, ents).Mermaid))
	writeEntityTables(&all, ents, nil, c)
	out[ViewsDir+"/"+erdDir+"/all.md"] = all.String()
	for _, s := range stores {
		var b strings.Builder
		b.WriteString(generatedMark)
		fmt.Fprintf(&b, "# Данные: %s (`%s`)\n\nОбъекты данных хранилища и их связи. [Общая схема](all.md).\n\n", c.byID[s].Name, s)
		b.WriteString(fenced(erdStoreView(c, ents, s).Mermaid))
		only := map[string]bool{}
		for _, e := range ents {
			for _, x := range e.Stores {
				if x == s {
					only[e.ID] = true
				}
			}
		}
		writeEntityTables(&b, ents, only, c)
		out[ViewsDir+"/"+erdDir+"/"+s+".md"] = b.String()
	}
	return out
}

// writeEntityTables — таблицы атрибутов сущностей. only=nil — все.
func writeEntityTables(b *strings.Builder, ents []ERDEntity, only map[string]bool, c *C4Model) {
	for _, e := range ents {
		if only != nil && !only[e.ID] {
			continue
		}
		fmt.Fprintf(b, "\n## %s (`%s`)\n\nХранится в: %s.\n\n", e.Name, e.ID, storeNames(c, e.Stores))
		if len(e.Attributes) == 0 {
			b.WriteString("Атрибуты не описаны.\n")
			continue
		}
		b.WriteString("| Атрибут | Тип | Ключ | Пусто | Описание |\n|---|---|---|---|---|\n")
		for _, a := range e.Attributes {
			var k []string
			if a.PK {
				k = append(k, "PK")
			}
			if a.FK != nil {
				k = append(k, "FK → "+codeCell(a.FK.To+"."+a.FK.Attribute))
			}
			if a.Unique {
				k = append(k, "UK")
			}
			null := "нет"
			if a.Nullable {
				null = "да"
			}
			fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", codeCell(a.Name), cell(a.Type), strings.Join(k, ", "), null, cell(a.Description))
		}
	}
}
