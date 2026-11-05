package architecture

import (
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// Сценарии и решения в изменении архитектуры (спека «Сценарии и решения в
// изменении архитектуры»): целевые сценарии в changes/<id>/scenarios/,
// новые решения в changes/<id>/adr/, оба списка в change.yaml.

// loadDocs читает целевые сценарии и новые решения изменения. noList:
// файлы вне индекса изменения не ищутся.
func (ch *Change) loadDocs(fsys fs.FS, r *reporter, noList bool) error {
	infoFile := ch.Dir + "/change.yaml"
	var err error
	if ch.Scenarios, err = loadScenarios(fsys, ch.Info.Scenarios, ch.Dir+"/", infoFile, "/scenarios", r); err != nil {
		return err
	}
	if ch.Decisions, err = loadDecisions(fsys, ch.Info.Decisions, ch.Dir+"/", infoFile, "/decisions", r); err != nil {
		return err
	}
	if noList {
		return nil
	}
	known := map[string]bool{}
	for _, id := range ch.Info.Scenarios {
		known[scenarioFile(ch.Dir+"/", id)] = true
	}
	for _, name := range ch.Info.Decisions {
		known[adrFile(ch.Dir+"/", name)] = true
	}
	for _, dir := range []struct{ name, rule, msg string }{
		{ScenariosDir, "scenario.unreferenced", "сценарий не указан в поле scenarios файла change.yaml и не читается"},
		{ADRDir, "adr.unreferenced", "решение не указано в поле decisions файла change.yaml и не читается"},
	} {
		entries, err := fs.ReadDir(fsys, ch.Dir+"/"+dir.name)
		if err != nil {
			continue
		}
		for _, e := range entries {
			file := path.Join(ch.Dir, dir.name, e.Name())
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") || known[file] {
				continue
			}
			r.warnf(dir.rule, file, "", "%s", dir.msg)
		}
	}
	return nil
}

// targetDocs строит сценарии и решения целевой модели: сценарии изменения
// заменяют одноимённые текущие или добавляются, новые решения дописываются
// в журнал, а заменяемые ими получают статус superseded.
func (ch *Change) targetDocs(t *Model) {
	cur := ch.Current
	byID := map[string]*Scenario{}
	for _, sc := range cur.scenarios {
		byID[sc.ID] = sc
	}
	for _, sc := range ch.Scenarios {
		byID[sc.ID] = sc
	}
	t.scenarios = nil
	for _, id := range sortedKeys(byID) {
		t.scenarios = append(t.scenarios, byID[id])
	}
	t.Manifest.Scenarios = mergeIndex(cur.Manifest.Scenarios, ch.Info.Scenarios)

	t.decisions = nil
	superseded := map[int]int{}
	for _, d := range ch.Decisions {
		for _, n := range d.Supersedes {
			superseded[n] = d.Number
		}
	}
	for _, d := range cur.decisions {
		x := *d
		if by, ok := superseded[d.Number]; ok {
			b := by
			x.Status, x.SupersededBy = ADRSuperseded, &b
		}
		t.decisions = append(t.decisions, &x)
	}
	t.decisions = append(t.decisions, ch.Decisions...)
	sortDecisions(t.decisions)
	t.Manifest.Decisions = mergeIndex(cur.Manifest.Decisions, ch.Info.Decisions)
}

// mergeIndex — индекс манифеста после изменения: текущие записи в прежнем
// порядке, новые в конце.
func mergeIndex(cur, add []string) []string {
	out := append([]string{}, cur...)
	for _, x := range add {
		if !containsString(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// checkDocs — покрытие целевых сценариев разрывами и нумерация новых
// решений продолжением журнала.
func (ch *Change) checkDocs(r *reporter) {
	file := ch.Dir + "/gap.yaml"
	inChange := map[string]bool{}
	for _, sc := range ch.Scenarios {
		inChange[sc.ID] = true
	}
	covered := map[string]bool{}
	for _, g := range ch.Gaps {
		for _, sid := range g.Scenarios {
			if !inChange[sid] {
				r.errorf("gap.ref-outside", file, g.ID, "разрыв ссылается на сценарий %s, у которого нет целевой версии в %s/%s",
					sid, ch.Dir, ScenariosDir)
				continue
			}
			covered[sid] = true
		}
	}
	for _, sc := range ch.Scenarios {
		if !covered[sc.ID] {
			r.errorf("change.scenario-uncovered", sc.File, "", "целевой сценарий %s не покрыт разрывом", sc.ID)
		}
	}
	max := 0
	used := map[int]string{}
	for _, d := range ch.Current.decisions {
		used[d.Number] = d.File
		if d.Number > max {
			max = d.Number
		}
	}
	next := max + 1
	for _, d := range ch.Decisions {
		switch {
		case d.Number <= 0:
			continue
		case used[d.Number] != "":
			r.errorf("change.adr-numbering", d.File, "/number", "номер %d уже занят в журнале: %s", d.Number, used[d.Number])
		case d.Number != next:
			r.errorf("change.adr-numbering", d.File, "/number",
				"новые решения нумеруются продолжением журнала: ожидался номер %d, а указан %d", next, d.Number)
		}
		if d.Number >= next {
			next = d.Number + 1
		}
	}
}

// renderChangeDocs — разделы изменения о целевых сценариях и новых
// решениях. Пусто, если изменение их не несёт.
func renderChangeDocs(b *strings.Builder, ch *Change) {
	if len(ch.Scenarios) > 0 {
		fmt.Fprintf(b, "\n## Целевые сценарии\n\nСхемы: [%s%s.md](%s%s.md).\n\n", ch.Info.ID, changeScenariosSuffix, ch.Info.ID, changeScenariosSuffix)
		cur := map[string]bool{}
		for _, sc := range ch.Current.scenarios {
			cur[sc.ID] = true
		}
		for _, sc := range ch.Scenarios {
			kind := "новый"
			if cur[sc.ID] {
				kind = "заменяет текущий"
			}
			fmt.Fprintf(b, "- %s (`%s`): %s\n", sc.Title, sc.ID, kind)
		}
	}
	if len(ch.Decisions) > 0 {
		b.WriteString("\n## Новые решения\n\n| № | Решение | Статус | Заменяет |\n|---|---|---|---|\n")
		for _, d := range ch.Decisions {
			var repl []string
			for _, n := range d.Supersedes {
				repl = append(repl, fmt.Sprintf("%04d", n))
			}
			fmt.Fprintf(b, "| %04d | %s | %s | %s |\n", d.Number, cell(d.Title), adrStatusTitle(d.Status), listOr(repl, ""))
		}
	}
}

// renderChangeScenarios — views/changes/<id>-scenarios.md: целевые
// сценарии на модели после изменения.
func renderChangeScenarios(ch *Change) string {
	t := ch.Target
	c := t.C4()
	var b strings.Builder
	b.WriteString(generatedMark)
	fmt.Fprintf(&b, "# Целевые сценарии: %s (`%s`)\n\n[Изменение](%s.md).\n", cmp(ch.Info.Title, ch.Info.ID), ch.Info.ID, ch.Info.ID)
	for _, sc := range ch.Scenarios {
		fmt.Fprintf(&b, "\n## %s (`%s`)\n\n", sc.Title, sc.ID)
		if d := strings.TrimSpace(sc.Description); d != "" {
			b.WriteString(d + "\n\n")
		}
		b.WriteString(fenced(t.sequenceDiagram(sc, c)))
	}
	return b.String()
}
