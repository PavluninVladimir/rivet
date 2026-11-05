package architecture

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// Тесты по находкам ревью ERD, сценариев и ADR: экранирование, порядок
// замены решений, история в журнале, конфликты применения, CRLF,
// псевдонимы и зарезервированные имена.

const erdEscapeDir = "testdata/golden/erd-escape"

// erdEscapeData — объекты данных со спецсимволами: ключевые слова Mermaid в
// именах и типах, %, обратная косая черта, кавычки, пустое имя, | и ` в
// имени атрибута. BT заменяется на обратную кавычку.
var erdEscapeData = strings.ReplaceAll(`elements:
  - id: data.order
    type: data-object
    name: 'Заказ "100%" \ #1; [черновик]'
    attributes:
      - {name: PK, type: uuid, pk: true}
      - {name: fk, type: Fk}
      - {name: Uk, type: uk, unique: true}
      - {name: "50% скидка", type: "decimal(10,2)", description: 'путь C:\tmp, а %% не комментарий'}
      - {name: "a|b", type: string}
      - {name: "xBTy", type: string}
  - id: data.line
    type: data-object
    name: " "
    attributes:
      - {name: id, type: int, pk: true}
      - {name: order_pk, type: uuid, fk: {to: data.order, attribute: PK}}
  - id: data.tag
    type: data-object
    name: Метка
    attributes:
      - {name: id, type: int, pk: true}
    relations:
      - {type: association, to: data.order, cardinality: many-to-many}
`, "BT", "`")

const erdEscapeTech = `elements:
  - id: tech.pg
    type: system-software
    name: "PostgreSQL [main]"
    props: {c4: database}
    relations:
      - {type: access, to: data.order, mode: readwrite}
      - {type: access, to: data.line, mode: readwrite}
`

// Находка 1: ERD со спецсимволами разбирается Mermaid (golden прогоняется
// через mmdc), находка 8: таблицы и ссылки не ломаются.
func TestERDEscapeGolden(t *testing.T) {
	fsys := minimalFS(map[string]string{"model/data.yaml": erdEscapeData, "model/technology.yaml": erdEscapeTech})
	noErrors(t, mustValidate(t, fsys))
	gen := mustViews(t, fsys)
	files := map[string]string{
		"all.md":     gen["views/erd/all.md"],
		"tech.pg.md": gen["views/erd/tech.pg.md"],
	}
	for name, got := range files {
		golden := filepath.Join(erdEscapeDir, name)
		if *update {
			if err := os.MkdirAll(erdEscapeDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if want := readFile(t, golden); got != want {
			t.Errorf("%s отличается от golden:\n--- ожидали\n%s\n--- получили\n%s", name, want, got)
		}
	}
	all := files["all.md"]
	mustContain(t, "токены", all, "uuid PK_ PK ", "Fk_ fk_ ", "uk_ Uk_ UK ", `имя: PK`, `тип: Fk`,
		`n_data_line["data.line"]`, `n_data_order["Заказ '100％' ∖ №1, [черновик]"]`, "C:∖tmp, а ％％ не комментарий",
		`n_data_tag }o--o{ n_data_order : ""`)
	mustContain(t, "таблица", all, "| `a\\|b` | string |", "| `` x`y `` | string |", "FK → `data.order.PK`",
		`[PostgreSQL \[main\]](tech.pg.md)`)
	mustContain(t, "чужая сущность", files["tech.pg.md"], `n_data_tag["Метка · вне хранилищ"]`)
	mustContain(t, "README", gen["views/README.md"], `- Хранилище: [PostgreSQL \[main\]](erd/tech.pg.md)`)
	mustContain(t, "HLD", gen["views/hld.md"], `[PostgreSQL \[main\]](erd/tech.pg.md)`)
	for name, text := range files {
		start := strings.Index(text, "```mermaid\n")
		end := strings.Index(text[start+1:], "```")
		if start < 0 || end < 0 {
			t.Fatalf("%s: нет схемы", name)
		}
		if diagram := text[start : start+1+end]; strings.ContainsAny(diagram, "%\\") {
			t.Errorf("%s: в схеме остался %% или обратная косая черта:\n%s", name, diagram)
		}
	}
}

// Находка 8: квадратные скобки в названиях сценария и решения не рвут
// ссылки, участники в таблице HLD экранированы.
func TestDocsLinkEscape(t *testing.T) {
	sc := strings.Replace(loginScenario, "title: Вход", "title: Вход [beta]", 1)
	fsys := scenarioFS(map[string]string{"sc.login": sc}, "sc.login")
	fsys["model/business.yaml"] = &fstest.MapFile{Data: []byte("elements:\n  - {id: biz.manager, type: actor, name: Менеджер | продажи}\n  - {id: biz.sale, type: process, name: Продажа}\n")}
	fsys[ManifestFile] = &fstest.MapFile{Data: []byte("format: rivet.architecture/v1\nsystem: CRM\nscenarios: [sc.login]\ndecisions: [0001-a]\n")}
	fsys["adr/0001-a.md"] = &fstest.MapFile{Data: []byte(strings.Replace(adrDoc(1, "accepted", ""), "title: Решение 1", "title: Решение [1]", 1))}
	gen := mustViews(t, fsys)
	mustContain(t, "README", gen["views/README.md"], `- [Вход \[beta\]](scenarios/sc.login.md)`)
	mustContain(t, "HLD", gen["views/hld.md"], `- [0001. Решение \[1\]](../adr/0001-a.md)`,
		"| Вход [beta] (`sc.login`) | Менеджер \\| продажи, Консоль, API, База | [scenarios/sc.login.md](scenarios/sc.login.md) |")
	mustNotContain(t, "HLD", gen["views/hld.md"], `\\|`)
	mustContain(t, "реестр", gen["views/adr.md"], `[Решение \[1\]](../adr/0001-a.md)`)
}

// Находка 2: решение заменяет только более ранние и заменяется только
// более поздним, цикл замены не проходит проверку.
func TestADRSupersedeOrder(t *testing.T) {
	cycle := adrFS(map[string]string{
		"0001-a": adrDoc(1, "superseded", "superseded_by: 2\nsupersedes: [2]\n"),
		"0002-b": adrDoc(2, "superseded", "superseded_by: 1\nsupersedes: [1]\n"),
	}, "0001-a", "0002-b")
	ds := mustValidate(t, cycle)
	find(t, ds, "adr.supersede-target", "adr/0001-a.md", "/supersedes/0")
	find(t, ds, "adr.supersede-target", "adr/0002-b.md", "/superseded_by")

	later := adrFS(map[string]string{
		"0001-a": adrDoc(1, "accepted", "supersedes: [2]\n"),
		"0002-b": adrDoc(2, "superseded", "superseded_by: 1\n"),
	}, "0001-a", "0002-b")
	ds = mustValidate(t, later)
	d := find(t, ds, "adr.supersede-target", "adr/0001-a.md", "/supersedes/0")
	if !strings.Contains(d.Message, "меньшим номером") {
		t.Errorf("сообщение: %s", d.Message)
	}
	find(t, ds, "adr.supersede-target", "adr/0002-b.md", "/superseded_by")
}

// Находка 3: неактивное решение с удалённым элементом даёт предупреждение,
// принятое и предложенное — ошибку.
func TestADRElementHistoric(t *testing.T) {
	for _, status := range []string{ADRSuperseded, ADRDeprecated, ADRRejected} {
		extra := "elements: [app.gone]\n"
		files := map[string]string{"0001-a": adrDoc(1, status, extra)}
		index := []string{"0001-a"}
		if status == ADRSuperseded {
			files["0001-a"] = adrDoc(1, status, extra+"superseded_by: 2\n")
			files["0002-b"] = adrDoc(2, ADRAccepted, "supersedes: [1]\n")
			index = append(index, "0002-b")
		}
		ds := mustValidate(t, adrFS(files, index...))
		if d := find(t, ds, "adr.element", "adr/0001-a.md", "/elements/0"); d.Severity != SeverityWarning {
			t.Errorf("%s: важность %s", status, d.Severity)
		}
		noErrors(t, ds)
	}
	for _, status := range []string{ADRProposed, ADRAccepted} {
		ds := mustValidate(t, adrFS(map[string]string{"0001-a": adrDoc(1, status, "elements: [app.gone]\n")}, "0001-a"))
		if d := find(t, ds, "adr.element", "adr/0001-a.md", "/elements/0"); d.Severity != SeverityError {
			t.Errorf("%s: важность %s", status, d.Severity)
		}
	}
}

// postgresChange — изменение демо-CRM «Переход на PostgreSQL»: JSON-файл
// удаляется, решение 0003 заменяет 0001 «Хранилище в JSON-файле».
func postgresChange(t *testing.T, supersede bool) map[string]string {
	t.Helper()
	adr := "---\nnumber: 3\ntitle: Переход на PostgreSQL\nstatus: accepted\ndate: 2026-11-04\nelements: [tech.postgres, app.api-storage]\n"
	if supersede {
		adr += "supersedes: [1]\n"
	}
	adr += "---\n\n## Контекст\n\nКонтактов стало больше, файл не выдерживает параллельную запись.\n\n## Решение\n\nPostgreSQL.\n\n## Последствия\n\nНужен сервер базы.\n"
	scenario := readFile(t, filepath.Join(crmDir, "scenarios/sc.contact-create.yaml"))
	scenario = strings.NewReplacer("tech.json-file", "tech.postgres", "запись во временный файл и rename", "INSERT в contacts").Replace(scenario)
	return map[string]string{
		"changes/to-postgres/change.yaml": "id: to-postgres\ntitle: Переход на PostgreSQL\nscenarios: [sc.contact-create]\ndecisions: [0003-postgres]\n",
		"changes/to-postgres/vision.md":   "Контакты переезжают из JSON-файла в PostgreSQL.\n",
		"changes/to-postgres/gap.yaml": "gaps:\n  - id: gap.postgres\n    title: Хранилище в PostgreSQL\n    kind: change\n" +
			"    description: JSON-файл заменяется базой PostgreSQL\n    elements: [tech.postgres, tech.json-file, tech.host]\n    scenarios: [sc.contact-create]\n",
		"changes/to-postgres/delta/technology.yaml": `added:
  - id: tech.postgres
    type: system-software
    name: PostgreSQL
    props: {c4: database, technology: "PostgreSQL 17"}
    relations:
      - {type: access, to: data.contact, mode: readwrite}
modified:
  - id: tech.host
    type: node
    name: Сервер
    props: {technology: "Node.js 22"}
    relations:
      - {type: serving, to: app.api}
      - {type: assignment, to: tech.postgres}
removed:
  - id: tech.json-file
    reason: Контакты переезжают в PostgreSQL
`,
		"changes/to-postgres/scenarios/sc.contact-create.yaml": scenario,
		"changes/to-postgres/adr/0003-postgres.md":             adr,
	}
}

// Находка 3, сценарий «Переход на PostgreSQL»: заменённое решение о
// JSON-файле не мешает удалить JSON-файл.
func TestChangeRemovesElementOfSupersededADR(t *testing.T) {
	ch, ds, err := LoadChange(dirFS(t, crmDir, postgresChange(t, true)), "to-postgres")
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	if d := find(t, ds, "adr.element", "adr/0001-json-storage.md", "/elements/0"); d.Severity != SeverityWarning {
		t.Errorf("важность %s", d.Severity)
	}
	var first *Decision
	for _, d := range ch.Target.decisions {
		if d.Number == 1 {
			first = d
		}
	}
	if first == nil || first.Status != ADRSuperseded {
		t.Fatalf("0001 в целевом журнале: %+v", first)
	}

	// Без замены решение 0001 остаётся принятым и держит элемент.
	_, ds, err = LoadChange(dirFS(t, crmDir, postgresChange(t, false)), "to-postgres")
	if err != nil {
		t.Fatal(err)
	}
	if d := find(t, ds, "adr.element", "adr/0001-json-storage.md", "/elements/0"); d.Severity != SeverityError {
		t.Errorf("важность %s", d.Severity)
	}
}

// Находка 4: сценарий, файл которого уже лежит вне индекса, не
// перезаписывается.
func TestApplyScenarioExists(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, c4Dir, dir)
	m := fstest.MapFS{"scenarios/sc.archive.yaml": {Data: []byte("id: sc.archive\ntitle: Чужой\nsteps:\n  - {from: app.api, to: app.worker, message: m, kind: sync}\n")}}
	for p, s := range changeFiles("0004-logs-s3", "sc.archive") {
		m[p] = &fstest.MapFile{Data: []byte(s)}
	}
	writeMapFS(t, dir, m)
	before := snapshot(t, dir)
	res, err := Apply(dir, "to-s3", applyDate)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Fatal("изменение не должно применяться поверх сценария вне индекса")
	}
	find(t, res.Defects, "apply.scenario-exists", "scenarios/sc.archive.yaml", "")
	if !reflect.DeepEqual(snapshot(t, dir), before) {
		t.Error("отказ применения изменил файлы")
	}
}

// Задача 1.9: golden на дифф файлов применения сценариев и решений.
func TestApplyDocsDiffGolden(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, c4Dir, dir)
	m := fstest.MapFS{}
	for p, s := range changeFiles("0004-logs-s3", "sc.archive") {
		m[p] = &fstest.MapFile{Data: []byte(s)}
	}
	writeMapFS(t, dir, m)
	before := snapshot(t, dir)
	res, err := Apply(dir, "to-s3", applyDate)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied {
		t.Fatalf("изменение не применено:\n%s", dump(res.Defects))
	}
	got := fileDiff(before, snapshot(t, dir))
	golden := "testdata/golden/apply-docs.diff"
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := readFile(t, golden); got != want {
		t.Errorf("дифф применения отличается от golden %s:\n%s", golden, got)
	}
}

// fileDiff — детерминированный перечень изменений каталога: удалённые
// файлы, добавленные и изменённые с новым содержимым.
func fileDiff(before, after map[string]string) string {
	paths := map[string]bool{}
	for p := range before {
		paths[p] = true
	}
	for p := range after {
		paths[p] = true
	}
	list := make([]string, 0, len(paths))
	for p := range paths {
		list = append(list, p)
	}
	sort.Strings(list)
	var b strings.Builder
	for _, p := range list {
		old, hadOld := before[p]
		cur, hasCur := after[p]
		switch {
		case !hasCur:
			b.WriteString("=== удалён " + p + "\n")
		case !hadOld:
			b.WriteString("=== добавлен " + p + "\n" + cur + "\n")
		case old != cur:
			b.WriteString("=== изменён " + p + "\n" + cur + "\n")
		}
	}
	return b.String()
}

// Находка 5: тело решения с CRLF сохраняется побайтно, front matter пишется
// с тем же переводом строки, разделы находятся.
func TestADRCRLF(t *testing.T) {
	raw := strings.ReplaceAll(adrDoc(1, "accepted", "elements: [app.api]\n"), "\n", "\r\n")
	ds := mustValidate(t, adrFS(map[string]string{"0001-a": raw}, "0001-a"))
	if len(ds) != 0 {
		t.Fatalf("решение с CRLF:\n%s", dump(ds))
	}
	out, err := patchFrontMatter([]byte(raw), map[string]string{"status": ADRSuperseded}, map[string]int{"superseded_by": 2})
	if err != nil {
		t.Fatal(err)
	}
	body := raw[strings.Index(raw, "\r\n## Контекст"):]
	if !strings.HasSuffix(string(out), body) {
		t.Errorf("тело изменилось:\n%q", out)
	}
	if strings.Count(string(out), "\n") != strings.Count(string(out), "\r\n") {
		t.Errorf("в файле смешаны переводы строк:\n%q", out)
	}
	mustContain(t, "front matter", string(out), "---\r\nnumber: 1\r\n", "status: superseded\r\n", "superseded_by: 2\r\n---\r\n")
}

// Находка 6: участники вне C4 не получают псевдоним элемента C4 и не
// совпадают между собой.
func TestScenarioAliasesOutsideC4(t *testing.T) {
	model := scenarioModel + "  - {id: app.a-b, type: component, name: Внутри C4}\n" +
		"  - {id: app.a.b, type: component, name: Вне C4, props: {c4: none}}\n"
	steps := "id: sc.x\ntitle: X\nsteps:\n" +
		"  - {from: app.a-b, to: app.a.b, message: m1, kind: sync}\n" +
		"  - {from: app.a.b, to: tech.n-1, message: m2, kind: sync}\n" +
		"  - {from: tech.n-1, to: tech.n.1, message: m3, kind: sync}\n"
	fsys := scenarioFS(map[string]string{"sc.x": steps}, "sc.x")
	fsys["model/application.yaml"] = &fstest.MapFile{Data: []byte(model)}
	fsys["model/technology.yaml"] = &fstest.MapFile{Data: []byte("elements:\n" +
		"  - {id: tech.db, type: system-software, name: База, relations: [{type: access, to: data.contact}]}\n" +
		"  - {id: tech.n-1, type: node, name: Узел один}\n  - {id: tech.n.1, type: node, name: Узел два}\n")}
	m, ds, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	c := m.C4()
	v := m.scenarioView(m.scenarios[0], c)
	seen := map[string]string{}
	for _, p := range v.Participants {
		if other, ok := seen[p.Alias]; ok {
			t.Errorf("псевдоним %s у %s и %s", p.Alias, other, p.ID)
		}
		seen[p.Alias] = p.ID
		if c.Element(p.ID) != nil && p.Alias != c.MermaidAlias(p.ID) {
			t.Errorf("%s: псевдоним %s, на C4 %s", p.ID, p.Alias, c.MermaidAlias(p.ID))
		}
		if !strings.Contains(v.View.Mermaid, " "+p.Alias+" as ") {
			t.Errorf("на схеме нет участника %s", p.Alias)
		}
	}
	if len(seen) != 4 {
		t.Errorf("участников %d: %+v", len(seen), v.Participants)
	}
}

// Находка 7: прямая связь C4-предка участника считается связью шага.
func TestScenarioRelationViaAncestor(t *testing.T) {
	model := strings.Replace(scenarioModel,
		"{id: app.api, type: component, name: API, relations: [",
		"{id: app.api, type: component, name: API, relations: [{type: composition, to: app.api-auth}, {type: association, to: app.billing}, ", 1) +
		"  - {id: app.api-auth, type: component, name: Авторизация}\n  - {id: app.billing, type: component, name: Биллинг}\n"
	fsys := scenarioFS(map[string]string{"sc.x": "id: sc.x\ntitle: X\nsteps:\n  - {from: app.api-auth, to: app.billing, message: m, kind: sync}\n"}, "sc.x")
	fsys["model/application.yaml"] = &fstest.MapFile{Data: []byte(model)}
	if ds := mustValidate(t, fsys); len(ds) != 0 {
		t.Fatalf("связь контейнера должна покрывать шаг его компонента:\n%s", dump(ds))
	}

	// Соседние компоненты одного контейнера composition не связывает.
	sibling := strings.Replace(model, "{type: composition, to: app.api-auth}", "{type: composition, to: app.api-auth}, {type: composition, to: app.api-log}", 1) +
		"  - {id: app.api-log, type: component, name: Журнал}\n"
	fsys = scenarioFS(map[string]string{"sc.x": "id: sc.x\ntitle: X\nsteps:\n  - {from: app.api-auth, to: app.api-log, message: m, kind: sync}\n"}, "sc.x")
	fsys["model/application.yaml"] = &fstest.MapFile{Data: []byte(sibling)}
	if d := find(t, mustValidate(t, fsys), "scenario.no-relation", "scenarios/sc.x.yaml", "/steps/0"); d.Severity != SeverityWarning {
		t.Errorf("важность %s", d.Severity)
	}
}

// Находка 9: имена README, hld и adr заняты представлениями Rivet.
func TestCustomViewpointReservedNames(t *testing.T) {
	ds := mustValidate(t, shopFS(t, map[string]string{
		ManifestFile: "format: rivet.architecture/v1\nsystem: Магазин\nviewpoints: [security, hld, readme, adr]\n",
	}))
	for i := 1; i <= 3; i++ {
		d := find(t, ds, "viewpoint.builtin-conflict", ManifestFile, fmt.Sprintf("/viewpoints/%d", i))
		if !strings.Contains(d.Message, "views/hld.md") {
			t.Errorf("сообщение: %s", d.Message)
		}
	}
}
