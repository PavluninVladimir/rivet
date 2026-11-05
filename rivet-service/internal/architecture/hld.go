package architecture

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Обзорный документ HLD (спека «Обзорный документ HLD»): собирается из
// описания шаблоном, без модели, поэтому стабилен и проверяется вместе с
// остальными представлениями. Пустой раздел явно говорит, чего в описании
// нет.

// hldChanges — изменения в работе для раздела HLD. known=false: список
// неизвестен, потому что описание прочитано без листинга каталогов.
type hldChanges struct {
	list  []*Change
	known bool
}

func renderHLD(m *Model, c *C4Model, ch hldChanges) string {
	var b strings.Builder
	sys := c.byID[DescribedSystemID]
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "# HLD: %s\n\n", sys.Name)
	b.WriteString("Обзор архитектуры верхнего уровня. Документ собран из описания архитектуры, " +
		"подробности в [C4-схемах](c4/context.md) и [представлениях слоёв](README.md).\n\n")

	b.WriteString("## Назначение и принципы\n\n")
	if sys.Description != "" {
		b.WriteString(sys.Description + "\n\n")
	} else {
		b.WriteString("Назначение системы не описано: заполни поле `description` в `architecture.yaml`.\n\n")
	}
	if len(m.Manifest.Principles) > 0 {
		b.WriteString("| Принцип | Формулировка | Обоснование |\n|---|---|---|\n")
		for _, p := range m.Manifest.Principles {
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", p.ID, cell(p.Title), cell(p.Rationale))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("Принципы архитектуры не заданы.\n\n")
	}

	b.WriteString("## Контекст (L1)\n\n")
	b.WriteString(fenced(c.contextScene().render(nil)))
	b.WriteString("\nПодробнее: [c4/context.md](c4/context.md).\n\n")

	b.WriteString("## Контейнеры (L2)\n\n")
	if conts := c.children(DescribedSystemID); len(conts) > 0 {
		b.WriteString(fenced(c.containerScene(DescribedSystemID).render(nil)))
		sortC4(conts)
		b.WriteString("\n| Контейнер | Вид | Технология | Описание |\n|---|---|---|---|\n")
		for _, e := range conts {
			fmt.Fprintf(&b, "| %s (`%s`) | %s | %s | %s |\n", cell(e.Name), e.ID, kindTitle(e), cell(e.Technology), cell(e.Description))
		}
		b.WriteString("\n")
	} else {
		b.WriteString("В описании нет контейнеров: компоненты слоя приложений не описаны.\n\n")
	}

	b.WriteString("## Ключевые бизнес-процессы\n\n")
	if procs := processElements(m); len(procs) > 0 {
		b.WriteString("| Процесс | Участвуют | Представление |\n|---|---|---|\n")
		for _, e := range procs {
			fmt.Fprintf(&b, "| %s (`%s`) | %s | [processes/%s.md](processes/%s.md) |\n",
				cell(elementLabel(e)), e.ID, cell(strings.Join(processParticipants(m, e.ID), ", ")), e.ID, e.ID)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("В описании нет бизнес-процессов.\n\n")
	}

	b.WriteString("## Данные\n\n")
	writeDataSection(&b, m, c)
	writeERDSection(&b, m, c)

	b.WriteString("## Развёртывание\n\n")
	if text, ok := c.deployment(); ok {
		b.WriteString(fenced(text))
		b.WriteString("\nПодробнее: [c4/deployment.md](c4/deployment.md).\n\n")
	} else {
		b.WriteString("Развёртывание не описано: в слое технологий нет узлов.\n\n")
	}

	b.WriteString("## Внешние интеграции\n\n")
	writeExternalSection(&b, c)

	b.WriteString("## Ключевые сценарии\n\n")
	if len(m.scenarios) > 0 {
		b.WriteString("| Сценарий | Участники | Представление |\n|---|---|---|\n")
		for _, sc := range m.scenarios {
			// Имена экранируются по одному, поэтому склеиваются без listOr:
			// повторный cell превратил бы \| в \\| и снова разорвал ячейку.
			var names []string
			for _, p := range scenarioParticipants(sc) {
				names = append(names, cell(m.participantName(p)))
			}
			participants := "не указаны"
			if len(names) > 0 {
				participants = strings.Join(names, ", ")
			}
			fmt.Fprintf(&b, "| %s (`%s`) | %s | [scenarios/%s.md](scenarios/%s.md) |\n",
				cell(sc.Title), sc.ID, participants, sc.ID, sc.ID)
		}
		b.WriteString("\n")
	} else {
		b.WriteString("Ключевых сценариев в описании нет.\n\n")
	}

	b.WriteString("## Архитектурные решения\n\n")
	var accepted []*Decision
	for _, d := range m.decisions {
		if d.Status == ADRAccepted {
			accepted = append(accepted, d)
		}
	}
	switch {
	case len(accepted) > 0:
		b.WriteString("Принятые решения. Полный журнал со всеми статусами: [adr.md](adr.md).\n\n")
		for _, d := range accepted {
			fmt.Fprintf(&b, "- [%04d. %s](../%s)\n", d.Number, linkText(d.Title), d.File)
		}
		b.WriteString("\n")
	case len(m.decisions) > 0:
		b.WriteString("Принятых решений нет. Журнал со всеми статусами: [adr.md](adr.md).\n\n")
	default:
		b.WriteString("Архитектурных решений в описании нет.\n\n")
	}

	b.WriteString("## Изменения архитектуры в работе\n\n")
	switch {
	case !ch.known:
		b.WriteString("Список изменений в работе здесь не строится: описание прочитано без листинга каталога `changes/`.\n")
	case len(ch.list) == 0:
		b.WriteString("Изменений в работе нет.\n")
	default:
		for _, x := range ch.list {
			fmt.Fprintf(&b, "- [%s](changes/%s.md), целевые схемы C4: [changes/%s-c4.md](changes/%s-c4.md)\n",
				linkText(cmp(x.Info.Title, x.Info.ID)), x.Info.ID, x.Info.ID, x.Info.ID)
		}
	}
	return b.String()
}

// processParticipants — роли и акторы процесса: дорожки BPMN и связи
// assignment к процессу, по названиям.
func processParticipants(m *Model, id string) []string {
	ids := map[string]bool{}
	if p := m.processes[id]; p != nil {
		var walk func([]Lane)
		walk = func(ls []Lane) {
			for _, l := range ls {
				if l.Ref != "" && m.element(l.Ref) != nil {
					ids[l.Ref] = true
				}
				walk(l.Lanes)
			}
		}
		walk(p.Lanes)
	}
	for _, e := range m.Elements() {
		if e.Type != TypeActor && e.Type != TypeRole {
			continue
		}
		for _, rel := range e.Relations {
			if rel.Type == RelAssignment && rel.To == id {
				ids[e.ID] = true
			}
		}
	}
	var names []string
	for _, x := range sortedKeys(ids) {
		names = appendUnique(names, elementLabel(m.element(x)))
	}
	if len(names) == 0 {
		return []string{"не указаны"}
	}
	return names
}

func writeDataSection(b *strings.Builder, m *Model, c *C4Model) {
	var objects []*Element
	for _, e := range m.Elements() {
		if e.Type == TypeDataObject {
			objects = append(objects, m.element(e.ID))
		}
	}
	if len(objects) == 0 {
		b.WriteString("В описании нет объектов данных.\n\n")
		return
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].ID < objects[j].ID })
	b.WriteString("| Объект данных | Читают | Пишут | Хранится в |\n|---|---|---|---|\n")
	for _, o := range objects {
		stores := c.storers[o.ID]
		var readers, writers []string
		for _, e := range m.Elements() {
			if slices.Contains(stores, e.ID) {
				continue
			}
			for _, rel := range e.Relations {
				if rel.Type != RelAccess || rel.To != o.ID {
					continue
				}
				name := cmp(e.Name, e.ID)
				if rel.Mode != "write" {
					readers = appendUnique(readers, name)
				}
				if rel.Mode == "write" || rel.Mode == "readwrite" {
					writers = appendUnique(writers, name)
				}
			}
		}
		var where []string
		for _, s := range stores {
			where = append(where, c.byID[s].Name)
		}
		sort.Strings(readers)
		sort.Strings(writers)
		fmt.Fprintf(b, "| %s (`%s`) | %s | %s | %s |\n", cell(elementLabel(o)), o.ID,
			listOr(readers, "нет"), listOr(writers, "нет"), listOr(where, "не указано"))
	}
	b.WriteString("\n")
}

// hldERDLimit — общая ERD показывается в HLD, только если сущностей не
// больше этого числа, иначе только ссылка.
const hldERDLimit = 15

// writeERDSection — ссылки раздела «Данные» на ERD хранилищ и общая схема.
func writeERDSection(b *strings.Builder, m *Model, c *C4Model) {
	ents := buildERD(m, c)
	if len(ents) == 0 {
		return
	}
	var links []string
	for _, s := range erdStores(ents) {
		links = append(links, fmt.Sprintf("[%s](erd/%s.md)", linkText(c.byID[s].Name), s))
	}
	b.WriteString("ERD: [общая схема](erd/all.md)")
	if len(links) > 0 {
		b.WriteString(", по хранилищам: " + strings.Join(links, ", "))
	}
	b.WriteString(".\n\n")
	if len(ents) <= hldERDLimit {
		b.WriteString(fenced(erdAllView(c, ents).Mermaid))
		b.WriteString("\n")
	}
}

func listOr(list []string, empty string) string {
	if len(list) == 0 {
		return empty
	}
	return cell(strings.Join(list, ", "))
}

func writeExternalSection(b *strings.Builder, c *C4Model) {
	var ext []*C4Element
	for i := range c.Elements {
		if e := &c.Elements[i]; e.Kind == C4System && e.External {
			ext = append(ext, e)
		}
	}
	if len(ext) == 0 {
		b.WriteString("Внешних систем в описании нет.\n\n")
		return
	}
	rels := c.lift(c.contextVisible)
	b.WriteString("| Внешняя система | Описание | Связи |\n|---|---|---|\n")
	for _, e := range ext {
		var links []string
		for _, r := range rels {
			if r.From == e.ID || r.To == e.ID {
				links = append(links, fmt.Sprintf("%s → %s: %s", c.byID[r.From].Name, c.byID[r.To].Name, r.Label()))
			}
		}
		fmt.Fprintf(b, "| %s (`%s`) | %s | %s |\n", cell(e.Name), e.ID, cell(e.Description), listOr(links, "связей нет"))
	}
	b.WriteString("\n")
}
