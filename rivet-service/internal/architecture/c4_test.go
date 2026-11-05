package architecture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const (
	smallDir = "testdata/small"
	c4Dir    = "testdata/c4"
	// crmDir — эталон демо-CRM: as-is описание заготовки с ERD, сценариями
	// и решениями, его же кладёт fake-агент в задаче описания.
	crmDir = "testdata/crm"
)

// dirFS — описание из каталога testdata в памяти с заменами файлов.
func dirFS(t *testing.T, dir string, overrides map[string]string) fstest.MapFS {
	t.Helper()
	m := fstest.MapFS{}
	err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(os.DirFS(dir), p)
		if err != nil {
			return err
		}
		m[p] = &fstest.MapFile{Data: raw}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for p, content := range overrides {
		if content == "" {
			delete(m, p)
			continue
		}
		m[p] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mustViews(t *testing.T, fsys fs.FS) map[string]string {
	t.Helper()
	gen, ds, err := Views(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if gen == nil || ds.HasErrors() {
		t.Fatalf("представления не построены:\n%s", dump(ds))
	}
	out := map[string]string{}
	for p, b := range gen {
		out[p] = string(b)
	}
	return out
}

func mustContain(t *testing.T, name, text string, parts ...string) {
	t.Helper()
	for _, s := range parts {
		if !strings.Contains(text, s) {
			t.Errorf("в %s нет %q:\n%s", name, s, text)
		}
	}
}

func mustNotContain(t *testing.T, name, text string, parts ...string) {
	t.Helper()
	for _, s := range parts {
		if strings.Contains(text, s) {
			t.Errorf("в %s не должно быть %q:\n%s", name, s, text)
		}
	}
}

// Эталоны C4 — golden-файлы генератора: схемы, DSL и HLD.
func TestC4FixturesUpToDate(t *testing.T) {
	for _, dir := range []string{smallDir, c4Dir, crmDir} {
		if *update {
			if _, err := WriteViews(dir); err != nil {
				t.Fatal(err)
			}
		}
		ds, err := CheckViews(os.DirFS(dir))
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) != 0 {
			t.Fatalf("%s: представления устарели или есть дефекты (go test -update):\n%s", dir, dump(ds))
		}
	}
}

// Сценарий «Описание без свойств C4».
func TestC4DefaultClassification(t *testing.T) {
	fsys := minimalFS(map[string]string{
		"model/business.yaml": "elements:\n  - {id: biz.customer, type: actor, name: Покупатель}\n",
		"model/application.yaml": "elements:\n" +
			"  - {id: app.web, type: component, name: Витрина, relations: [{type: serving, to: biz.customer}]}\n" +
			"  - {id: app.api, type: component, name: API, relations: [{type: composition, to: app.orders}]}\n" +
			"  - {id: app.orders, type: component, name: Заказы}\n",
	})
	m, ds, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	c := m.C4()
	want := map[string][2]string{
		"biz.customer": {C4Person, ""},
		"system":       {C4System, ""},
		"app.web":      {C4Container, DescribedSystemID},
		"app.api":      {C4Container, DescribedSystemID},
		"app.orders":   {C4Component, "app.api"},
	}
	for id, w := range want {
		e := c.Element(id)
		if e == nil || e.Kind != w[0] || e.Parent != w[1] {
			t.Errorf("%s: %+v, ожидали вид %s и родителя %q", id, e, w[0], w[1])
		}
	}
	gen := mustViews(t, fsys)
	mustContain(t, "context.md", gen["views/c4/context.md"], "Rel(n_biz_customer, n_system, \"использует\")")
	mustContain(t, "container/system.md", gen["views/c4/container/system.md"],
		"Rel(n_biz_customer, n_app_web, \"использует\")",
		"Container(n_app_api, \"API\", \"\", \"\")")
	mustContain(t, "component/app.api.md", gen["views/c4/component/app.api.md"],
		"Container_Boundary(n_app_api, \"API\") {", "Component(n_app_orders, \"Заказы\", \"\", \"\")")
}

// Сценарий «Явная внешняя система».
func TestC4ExternalSystem(t *testing.T) {
	gen := mustViews(t, minimalFS(map[string]string{
		"model/application.yaml": "elements:\n" +
			"  - {id: app.api, type: component, name: API, relations: [{type: flow, to: app.github}]}\n" +
			"  - {id: app.github, type: component, name: GitHub, props: {c4: system, external: true}}\n",
	}))
	ctx := gen["views/c4/context.md"]
	mustContain(t, "context.md", ctx, `System_Ext(n_app_github, "GitHub", "")`, `Rel(n_system, n_app_github, "передаёт")`)
	mustContain(t, "container/system.md", gen["views/c4/container/system.md"], `Rel(n_app_api, n_app_github, "передаёт")`)
	mustContain(t, "hld.md", gen["views/hld.md"], "| GitHub (`app.github`) |  | Тест → GitHub: передаёт |")
}

// Сценарий «База данных из слоя технологий».
func TestC4DatabaseFromTechnology(t *testing.T) {
	gen := mustViews(t, minimalFS(map[string]string{
		"model/application.yaml": "elements:\n  - {id: app.api, type: component, name: API, relations: [{type: access, to: data.order, mode: write}]}\n",
		"model/data.yaml":        "elements:\n  - {id: data.order, type: data-object, name: Заказ}\n",
		"model/technology.yaml":  "elements:\n  - {id: tech.postgres, type: system-software, name: PostgreSQL, relations: [{type: access, to: data.order, mode: readwrite}]}\n",
	}))
	mustContain(t, "container/system.md", gen["views/c4/container/system.md"],
		`ContainerDb(n_tech_postgres, "PostgreSQL", "", "")`, `Rel(n_app_api, n_tech_postgres, "пишет Заказ")`)
	if _, ok := gen["views/c4/deployment.md"]; ok {
		t.Error("без узлов схемы развёртывания нет")
	}
}

// Сценарий «Элемент исключён из C4».
func TestC4ExcludedElement(t *testing.T) {
	gen := mustViews(t, minimalFS(map[string]string{
		"model/business.yaml": "elements:\n  - {id: biz.bot, type: role, name: Робот-уборщик, props: {c4: none}}\n",
	}))
	for p, text := range gen {
		if strings.HasPrefix(p, "views/c4/") || p == "views/hld.md" {
			mustNotContain(t, p, text, "Робот-уборщик")
		}
	}
	mustContain(t, "business.md", gen["views/business.md"], "Робот-уборщик")
}

// Сценарии требования «Согласованность C4»: коды правил и строгость
// только при явных свойствах.
func TestC4Rules(t *testing.T) {
	app := func(elems ...string) map[string]string {
		return map[string]string{"model/application.yaml": "elements:\n  - " + strings.Join(elems, "\n  - ") + "\n"}
	}
	cases := []struct {
		name  string
		files map[string]string
		rule  string
		loc   string
		sev   Severity
		msg   string
	}{
		{"неизвестный вид", app(`{id: app.a, type: component, name: A, props: {c4: service}}`),
			"c4.kind-unknown", "app.a", SeverityError, "person, system, container"},
		{"вид не подходит типу", map[string]string{"model/data.yaml": "elements:\n  - {id: data.x, type: data-object, name: X, props: {c4: person}}\n"},
			"c4.kind-type", "data.x", SeverityError, "data-object"},
		{"c4 не строка", app(`{id: app.a, type: component, name: A, props: {c4: 3}}`),
			"c4.prop-type", "app.a", SeverityError, "c4"},
		{"technology не строка", app(`{id: app.a, type: component, name: A, props: {technology: 5}}`),
			"c4.prop-type", "app.a", SeverityError, "technology"},
		{"external не логическое", app(`{id: app.a, type: component, name: A, props: {external: "yes"}}`),
			"c4.prop-type", "app.a", SeverityError, "external"},
		{"external у вложенного", app(`{id: app.a, type: component, name: A, relations: [{type: composition, to: app.b}]}`,
			`{id: app.b, type: component, name: B, props: {external: true}}`),
			"c4.external-nested", "app.b", SeverityError, "app.a"},
		{"контейнер внутри контейнера", app(`{id: app.a, type: component, name: A, relations: [{type: composition, to: app.b}]}`,
			`{id: app.b, type: component, name: B, props: {c4: container}}`),
			"c4.container-parent", "app.b", SeverityError, "app.a"},
		{"контейнер в двух системах", app(`{id: app.s1, type: component, name: S1, props: {c4: system}, relations: [{type: composition, to: app.b}]}`,
			`{id: app.s2, type: component, name: S2, props: {c4: system}, relations: [{type: composition, to: app.b}]}`,
			`{id: app.b, type: component, name: B, props: {c4: container}}`),
			"c4.container-parent", "app.b", SeverityError, "app.s1, app.s2"},
		{"компонент без контейнера", app(`{id: app.a, type: component, name: A, props: {c4: component}}`),
			"c4.component-parent", "app.a", SeverityError, "ни одного"},
		{"компонент в двух контейнерах", app(`{id: app.a, type: component, name: A, relations: [{type: composition, to: app.c}]}`,
			`{id: app.b, type: component, name: B, relations: [{type: composition, to: app.c}]}`,
			`{id: app.c, type: component, name: C, props: {c4: component}}`),
			"c4.component-parent", "app.c", SeverityError, "app.a, app.b"},
		{"два родителя без свойств", app(`{id: app.b, type: component, name: B, relations: [{type: composition, to: app.c}]}`,
			`{id: app.a, type: component, name: A, relations: [{type: composition, to: app.c}]}`,
			`{id: app.c, type: component, name: C}`),
			"c4.ambiguous-parent", "app.c", SeverityWarning, "выбран app.a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds := mustValidate(t, minimalFS(tc.files))
			file := "model/application.yaml"
			for f := range tc.files {
				file = f
			}
			d := find(t, ds, tc.rule, file, tc.loc)
			if d.Severity != tc.sev || !strings.Contains(d.Message, tc.msg) {
				t.Errorf("дефект %s", d)
			}
			if tc.sev == SeverityWarning && ds.HasErrors() {
				t.Errorf("предупреждение не должно делать описание некорректным:\n%s", dump(ds))
			}
		})
	}
	// Неоднозначность без свойств разрешается к родителю с меньшим id.
	m, _, _ := Load(minimalFS(app(`{id: app.b, type: component, name: B, relations: [{type: composition, to: app.c}]}`,
		`{id: app.a, type: component, name: A, relations: [{type: composition, to: app.c}]}`,
		`{id: app.c, type: component, name: C}`)))
	if e := m.C4().Element("app.c"); e == nil || e.Parent != "app.a" {
		t.Errorf("родитель app.c: %+v", e)
	}
}

// Описания, корректные до C4, остаются корректными.
func TestC4ExistingFixturesStayValid(t *testing.T) {
	for _, dir := range []string{shopDir, "testdata/golden/apply-refunds", smallDir, c4Dir} {
		ds := mustValidate(t, os.DirFS(dir))
		for _, d := range ds {
			if strings.HasPrefix(d.Rule, "c4.") {
				t.Errorf("%s: %s", dir, d)
			}
		}
		noErrors(t, ds)
	}
}

// Сценарий «Переход по уровням».
func TestC4Levels(t *testing.T) {
	gen := mustViews(t, minimalFS(map[string]string{
		"model/application.yaml": "elements:\n" +
			"  - {id: app.web, type: component, name: Web, relations: [{type: flow, to: app.api-a}]}\n" +
			"  - {id: app.api, type: component, name: API, relations: [{type: composition, to: app.api-a}, {type: composition, to: app.api-b}]}\n" +
			"  - {id: app.api-a, type: component, name: A, relations: [{type: flow, to: app.worker}]}\n" +
			"  - {id: app.api-b, type: component, name: B}\n" +
			"  - {id: app.worker, type: component, name: Worker}\n",
	}))
	var c4files []string
	for p := range gen {
		if strings.HasPrefix(p, "views/c4/") && strings.HasSuffix(p, ".md") {
			c4files = append(c4files, p)
		}
	}
	slices.Sort(c4files)
	if want := []string{"views/c4/component/app.api.md", "views/c4/container/system.md", "views/c4/context.md"}; !reflect.DeepEqual(c4files, want) {
		t.Errorf("файлы C4 %v", c4files)
	}
	l2 := gen["views/c4/container/system.md"]
	start, end := strings.Index(l2, "System_Boundary("), strings.Index(l2, "  }\n")
	if start < 0 || end < start {
		t.Fatalf("нет границы системы:\n%s", l2)
	}
	mustContain(t, "граница системы", l2[start:end], "n_app_web", "n_app_api", "n_app_worker")
	mustContain(t, "container/system.md", l2, `Rel(n_app_api, n_app_worker, "передаёт")`, `Rel(n_app_web, n_app_api, "передаёт")`,
		"../component/app.api.md")
	l3 := gen["views/c4/component/app.api.md"]
	mustContain(t, "component/app.api.md", l3,
		`Container(n_app_web, "Web", "", "")`, `Rel(n_app_web, n_app_api_a, "передаёт")`, `Rel(n_app_api_a, n_app_worker, "передаёт")`)
	mustContain(t, "README.md", gen["views/README.md"], "[HLD: обзор архитектуры](hld.md)", "(c4/component/app.api.md)", "(c4/workspace.dsl)")
}

// Сценарий «Развёртывание».
func TestC4Deployment(t *testing.T) {
	gen := mustViews(t, os.DirFS(c4Dir))
	dep := gen["views/c4/deployment.md"]
	vm := strings.Index(dep, "Deployment_Node(n_tech_vm,")
	docker := strings.Index(dep, "Deployment_Node(n_tech_docker,")
	api := strings.Index(dep, `Container(n_inst_tech_docker_app_api, "API"`)
	web := strings.Index(dep, `Container(n_inst_tech_docker_app_web, "Консоль"`)
	if vm < 0 || docker < vm || api < docker || web < docker {
		t.Fatalf("вложенность развёртывания:\n%s", dep)
	}
	mustContain(t, "deployment.md", dep, `ContainerDb(n_inst_tech_vm_tech_postgres, "PostgreSQL", "PostgreSQL 16", "")`)
	// Очередь без узла: размещение не описано, ни Mermaid, ни DSL её не
	// размещают.
	mustNotContain(t, "deployment.md", dep, "NATS")
	dsl := gen["views/c4/workspace.dsl"]
	env := dsl[strings.Index(dsl, "deploymentEnvironment"):]
	mustNotContain(t, "deploymentEnvironment", env, "tech_nats")
}

// Mermaid и DSL размещают одно и то же: внешняя система на узле — экземпляр
// и там, и там.
func TestC4DeploymentExternalOnNode(t *testing.T) {
	gen := mustViews(t, minimalFS(map[string]string{
		"model/application.yaml": "elements:\n  - {id: app.api, type: component, name: API}\n",
		"model/technology.yaml": "elements:\n" +
			"  - {id: tech.vm, type: node, name: VM, relations: [{type: assignment, to: tech.s3}, {type: serving, to: app.api}]}\n" +
			"  - {id: tech.s3, type: system-software, name: S3, props: {external: true}}\n",
	}))
	mustContain(t, "deployment.md", gen["views/c4/deployment.md"],
		`System_Ext(n_inst_tech_vm_tech_s3, "S3", "")`, `Container(n_inst_tech_vm_app_api, "API", "", "")`)
	mustContain(t, "workspace.dsl", gen["views/c4/workspace.dsl"], "softwareSystemInstance tech_s3", "containerInstance app_api")
}

// reverseElements переставляет элементы файла слоя в обратном порядке.
func reverseElements(t *testing.T, doc string) string {
	t.Helper()
	parts := strings.Split(doc, "\n  - id: ")
	if len(parts) < 3 {
		t.Fatalf("мало элементов для перестановки:\n%s", doc)
	}
	head, items := parts[0], parts[1:]
	items[len(items)-1] = strings.TrimSuffix(items[len(items)-1], "\n")
	slices.Reverse(items)
	return head + "\n  - id: " + strings.Join(items, "\n  - id: ") + "\n"
}

// Сценарий «Стабильность».
func TestC4StableUnderPermutation(t *testing.T) {
	base := mustViews(t, dirFS(t, c4Dir, nil))
	overrides := map[string]string{}
	for _, f := range []string{"model/application.yaml", "model/technology.yaml", "model/business.yaml"} {
		overrides[f] = reverseElements(t, readFile(t, c4Dir+"/"+f))
	}
	again := mustViews(t, dirFS(t, c4Dir, overrides))
	for p, text := range base {
		if strings.HasPrefix(p, "views/c4/") || p == "views/hld.md" {
			if again[p] != text {
				t.Errorf("%s отличается после перестановки элементов", p)
			}
		}
	}
}

// Сценарий «Открытие в Structurizr»: структура DSL и однозначные идентификаторы.
func TestDSLStructure(t *testing.T) {
	dsl := mustViews(t, os.DirFS(c4Dir))["views/c4/workspace.dsl"]
	mustContain(t, "workspace.dsl", dsl,
		`workspace "Платформа сборок" "Собирает проекты разработчиков по событиям из GitHub и хранит историю сборок" {`,
		`system = softwareSystem "Платформа сборок"`,
		`app_api = container "API" "" "Go, net/http" "" {`,
		`app_api_builds = component "Сборки" "Ставит сборки в очередь и отдаёт их статус" "Go" ""`,
		`tech_postgres = container "PostgreSQL" "" "PostgreSQL 16" "Database"`,
		`tech_nats = container "NATS" "Очередь заданий сборки" "NATS JetStream" "Queue"`,
		`app_github = softwareSystem "GitHub" "Хостинг репозиториев и событий push" "External"`,
		`biz_auditor = person "Аудитор" "Проверяет историю сборок снаружи компании" "External"`,
		`app_api_builds -> app_github "вызывает"`,
		`deploymentNode "Виртуальная машина" "" "Ubuntu 24.04" {`,
		`containerInstance tech_postgres`,
		`component app_api "components-app_api" {`,
		`deployment * "Основное" "deployment" {`)
	mustNotContain(t, "workspace.dsl", dsl, "app_cleanup", "Скрипт очистки")
	if strings.Count(dsl, "{") != strings.Count(dsl, "}") {
		t.Errorf("скобки DSL не сбалансированы:\n%s", dsl)
	}
}

func TestDSLIdentifiersUnique(t *testing.T) {
	m, ds, err := Load(minimalFS(map[string]string{
		"model/application.yaml": "elements:\n" +
			"  - {id: app.a-b, type: component, name: AB1}\n" +
			"  - {id: app.a.b, type: component, name: AB2}\n" +
			"  - {id: app.a-b-2, type: component, name: AB3}\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	ids := dslIdentifiers(m.C4())
	seen := map[string]string{}
	for id, v := range ids {
		if prev, dup := seen[v]; dup {
			t.Errorf("идентификатор %s у %s и %s", v, prev, id)
		}
		seen[v] = id
		for _, r := range v {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
				t.Errorf("идентификатор %q с недопустимым символом", v)
			}
		}
	}
	if ids[DescribedSystemID] != "system" || ids["app.a-b"] != "app_a_b" || len(seen) != 4 {
		t.Errorf("идентификаторы %v", ids)
	}
}

// Сценарии «HLD небольшой системы» и «Нет слоя технологий».
func TestHLDSmallSystem(t *testing.T) {
	gen := mustViews(t, os.DirFS(smallDir))
	hld := gen["views/hld.md"]
	mustContain(t, "hld.md", hld,
		"## Назначение и принципы\n\nПринимает заявки клиентов и ведёт их до закрытия",
		"| `p.single-source` |", "| `p.web-first` |",
		"## Контекст (L1)\n\n```mermaid\nC4Context",
		"## Контейнеры (L2)\n\n```mermaid\nC4Container",
		"| Веб-кабинет (`app.web`) | контейнер | TypeScript, React | Страницы для клиентов |",
		"| PostgreSQL (`tech.db`) | база данных | PostgreSQL 16 |  |",
		"| Подача заявки (`biz.request`) | Клиент | [processes/biz.request.md](processes/biz.request.md) |",
		"| Заявка (`data.request`) | API заявок | API заявок | PostgreSQL |",
		"Развёртывание не описано: в слое технологий нет узлов.",
		"Внешних систем в описании нет.",
		"Изменений в работе нет.")
	if _, ok := gen["views/c4/deployment.md"]; ok {
		t.Error("без узлов deployment.md не создаётся")
	}
}

// HLD минимального описания: все разделы на месте с явными заглушками.
func TestHLDMinimalGolden(t *testing.T) {
	gen := mustViews(t, minimalFS(nil))
	golden := "testdata/golden/hld-minimal.md"
	if *update {
		if err := os.WriteFile(golden, []byte(gen["views/hld.md"]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if want := readFile(t, golden); gen["views/hld.md"] != want {
		t.Errorf("hld.md отличается от golden (go test -update):\n%s", gen["views/hld.md"])
	}
	for _, s := range []string{"Назначение системы не описано", "Принципы архитектуры не заданы", "В описании нет контейнеров",
		"В описании нет бизнес-процессов", "В описании нет объектов данных", "Развёртывание не описано", "Внешних систем в описании нет"} {
		mustContain(t, "hld.md", gen["views/hld.md"], s)
	}
}

// Сценарий «Новый контейнер в изменении».
func TestChangeC4Highlight(t *testing.T) {
	gen := mustViews(t, shopFS(t, nil))
	ch := gen["views/changes/refunds-c4.md"]
	mustContain(t, "refunds-c4.md", ch,
		`Container(n_app_refunds, "Сервис возвратов", "", "")`,
		`UpdateElementStyle(n_app_refunds, $bgColor="#e6f4ea"`,
		`UpdateElementStyle(n_app_storefront, $bgColor="#fef7e0"`,
		`Container(n_app_mail_refunds, "Возвраты по почте"`,
		`UpdateElementStyle(n_app_mail_refunds, $bgColor="#fce8e6"`,
		`Rel(n_app_storefront, n_app_refunds, "передаёт")`,
		"## Легенда", "| `#1e8e3e` | добавлено |")
	mustContain(t, "README.md", gen["views/README.md"], "[changes/refunds-c4.md](changes/refunds-c4.md)")
	mustContain(t, "hld.md", gen["views/hld.md"], "- [Возвраты заказов](changes/refunds.md)")
}

// noListFS — файловая система без листинга каталогов, как чтение через
// API хостинга по SHA.
type noListFS struct {
	fsys  fstest.MapFS
	reads *[]string
}

var errNoList = errors.New("листинг каталогов недоступен")

func (f noListFS) Open(name string) (fs.File, error) {
	file, err := f.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := file.Stat()
	if err != nil || st.IsDir() {
		_ = file.Close()
		return nil, errNoList
	}
	*f.reads = append(*f.reads, name)
	return file, nil
}

func (noListFS) ReadDir(string) ([]fs.DirEntry, error) { return nil, errNoList }

func TestSnapshotWithoutListing(t *testing.T) {
	var reads []string
	fsys := noListFS{fsys: shopFS(t, nil), reads: &reads}
	s, err := BuildSnapshot(fsys, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != SnapshotOK || s.Data == nil || s.Defects.HasErrors() {
		t.Fatalf("снимок %s:\n%s", s.Status, dump(s.Defects))
	}
	d := s.Data
	if d.System.Name != "Интернет-магазин" || len(d.Principles) != 2 || d.Views.Deployment == nil || len(d.Views.Containers) != 1 {
		t.Errorf("данные снимка %+v", d)
	}
	if d.Views.Containers[0].SystemID != DescribedSystemID || !strings.HasPrefix(d.Views.Containers[0].View.Mermaid, "C4Container\n") {
		t.Errorf("схема контейнеров %+v", d.Views.Containers[0])
	}
	mustContain(t, "HLD снимка", d.HLD, "без листинга каталога")
	for _, p := range reads {
		if strings.HasPrefix(p, "views/") || strings.HasPrefix(p, ChangesDir) {
			t.Errorf("снимок без изменений прочитал %s", p)
		}
	}
	raw, err := json.Marshal(d.Elements)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "elements JSON", string(raw), `{"id":"system","kind":"system","name":"Интернет-магазин","description":"","technology":"","external":false,"alias":"n_system","parent":null}`,
		`"id":"app.orders","kind":"container"`, `"parent":"system"`)

	// Известное изменение загружается без листинга, HLD совпадает с views/hld.md.
	s, err = BuildSnapshot(fsys, SnapshotOptions{Changes: []string{"refunds"}})
	if err != nil {
		t.Fatal(err)
	}
	gen := mustViews(t, shopFS(t, nil))
	if s.Data.HLD != gen["views/hld.md"] || s.Data.DSL != gen["views/c4/workspace.dsl"] {
		t.Error("HLD и DSL снимка должны совпадать с представлениями")
	}
	ch, ds, err := LoadChangeNoList(fsys, "refunds")
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	if _, ok := ch.Target.Element("app.refunds"); !ok || ch.Processes["biz.refund"] == nil {
		t.Error("целевая модель изменения без app.refunds или процесса biz.refund")
	}
	cv := BuildChangeC4(ch)
	if len(cv.Containers) != 1 || len(cv.Legend) != 3 || !strings.Contains(cv.Containers[0].View.Mermaid, "UpdateElementStyle(n_app_refunds") {
		t.Errorf("C4 изменения %+v", cv)
	}
}

func TestSnapshotStatuses(t *testing.T) {
	s, err := BuildSnapshot(fstest.MapFS{}, SnapshotOptions{})
	if err != nil || s.Status != SnapshotAbsent || s.Data != nil || len(s.Defects) != 0 {
		t.Errorf("нет описания: %+v %v", s, err)
	}
	s, err = BuildSnapshot(shopFS(t, map[string]string{"model/data.yaml": "elements: [{id: data.x}]\n"}), SnapshotOptions{})
	if err != nil || s.Status != SnapshotInvalid || s.Data != nil || !s.Defects.HasErrors() {
		t.Errorf("сломанное описание: %+v %v", s, err)
	}
}

func TestDescribePrompt(t *testing.T) {
	p := DescribePrompt()
	mustContain(t, "задание describe", p, DescribeMarker, DefaultDir,
		`"$RIVET_RUNNER_BIN" architecture validate`, `"$RIVET_RUNNER_BIN" architecture views`, `"$RIVET_RUNNER_BIN" architecture init`,
		"c4: system", "external: true", "technology", "c4: none", "description")
	// Данные, сценарии и решения (спека «Данные, сценарии и решения в
	// задаче описания»): ссылки на код и индекс манифеста.
	mustContain(t, "задание describe", p, "attributes", "fk", "pk", "scenarios/<id>.yaml", "alt", "loop",
		"adr/NNNN-<slug>.md", "status: accepted", "## Контекст", "ссылайся на файлы репозитория",
		"в поле scenarios манифеста", "в поле decisions манифеста", "Не выдумывай сценарии и решения")
	if !strings.HasPrefix(p, DescribeMarker+"\n") {
		t.Error("задание должно начинаться с маркера")
	}
	if c := DescribeCriteria(); len(c) != 7 || !strings.Contains(c[2], "validate") || !strings.Contains(c[3], "--check") ||
		!strings.Contains(c[4], "ERD") || !strings.Contains(c[5], "сценарии") || !strings.Contains(c[6], "ADR") {
		t.Errorf("критерии %v", c)
	}
}

// Псевдонимы считаются один раз на модель: при совпадении после замены
// символов каждый элемент получает один и тот же псевдоним на всех схемах,
// в MermaidAlias и в снимке.
func TestMermaidAliasConsistent(t *testing.T) {
	fsys := minimalFS(map[string]string{
		"model/application.yaml": "elements:\n" +
			"  - {id: app.a-b, type: component, name: AB1, relations: [{type: flow, to: app.a.b}]}\n" +
			"  - {id: app.a.b, type: component, name: AB2}\n",
	})
	m, ds, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	c := m.C4()
	a1, a2 := c.MermaidAlias("app.a-b"), c.MermaidAlias("app.a.b")
	if a1 != "n_app_a_b" || a2 != "n_app_a_b_2" {
		t.Fatalf("псевдонимы %s %s", a1, a2)
	}
	gen := mustViews(t, fsys)
	for _, p := range []string{"views/c4/container/system.md", "views/hld.md"} {
		mustContain(t, p, gen[p], `Container(n_app_a_b, "AB1"`, `Container(n_app_a_b_2, "AB2"`, `Rel(n_app_a_b, n_app_a_b_2, "передаёт")`)
	}
	s, err := BuildSnapshot(fsys, SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range s.Data.Elements {
		if e.Alias != c.MermaidAlias(e.ID) {
			t.Errorf("псевдоним %s в снимке %q, в модели %q", e.ID, e.Alias, c.MermaidAlias(e.ID))
		}
	}
}

// Заголовок Mermaid C4 без «#» и «;»: на них парсер падает с Lexical error.
func TestC4TitleSanitized(t *testing.T) {
	fsys := minimalFS(nil)
	fsys[ManifestFile] = &fstest.MapFile{Data: []byte("format: rivet.architecture/v1\nsystem: \"Склад #1; основной\"\n")}
	gen := mustViews(t, fsys)
	ctx := gen["views/c4/context.md"]
	mustContain(t, "context.md", ctx, "title Контекст: Склад №1, основной\n")
	for _, line := range strings.Split(ctx, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "title ") && strings.ContainsAny(line, "#;") {
			t.Errorf("в заголовке остались # или ;: %q", line)
		}
	}
}

// Имя каталога изменения с суффиксом -c4 заняло бы файл C4 другого
// изменения: такой id запрещён с понятной причиной.
func TestChangeIDC4SuffixRejected(t *testing.T) {
	if ValidChangeID("refunds-c4") || !ValidChangeID("refunds-c45") || !ValidChangeID("c4") {
		t.Error("ValidChangeID: -c4 в конце запрещён, остальное как раньше")
	}
	files := map[string]string{}
	for p, f := range shopFS(t, nil) {
		if strings.HasPrefix(p, refunds+"/") {
			files["changes/refunds-c4/"+strings.TrimPrefix(p, refunds+"/")] = string(f.Data)
		}
	}
	ds := mustValidate(t, shopFS(t, files))
	d := find(t, ds, "change.id", "changes/refunds-c4", "")
	mustContain(t, "сообщение change.id", d.Message, "не может оканчиваться на -c4", "views/changes/<id>-c4.md")
	if _, _, err := LoadChange(shopFS(t, files), "refunds-c4"); err == nil || !strings.Contains(err.Error(), "-c4") {
		t.Errorf("LoadChange: %v", err)
	}
}

// Принцип в JSON всегда с полем rationale (api-contract ArchitectureData).
func TestPrincipleRationaleAlwaysPresent(t *testing.T) {
	raw, err := json.Marshal(Principle{ID: "p.x", Title: "X"})
	if err != nil || !strings.Contains(string(raw), `"rationale":""`) {
		t.Errorf("%s %v", raw, err)
	}
}

// Имена в одной области Structurizr DSL уникальны, иначе Structurizr не
// откроет файл. В эталоне shop два элемента «Покупатель».
func TestDSLNamesUniqueInScope(t *testing.T) {
	declRe := regexp.MustCompile(`^\s*(?:\S+ = )?(person|softwareSystem|container|component|deploymentNode) "([^"]*)"`)
	for _, fsys := range []fs.FS{shopFS(t, nil), os.DirFS(c4Dir), os.DirFS(smallDir)} {
		dsl := mustViews(t, fsys)["views/c4/workspace.dsl"]
		var stack []string
		seen := map[string]bool{}
		for n, line := range strings.Split(dsl, "\n") {
			trimmed := strings.TrimSpace(line)
			if m := declRe.FindStringSubmatch(line); m != nil {
				scope := strings.Join(stack, "/")
				if m[1] == "person" || m[1] == "softwareSystem" {
					scope = "top"
				}
				key := scope + "|" + strings.ToLower(m[2])
				if seen[key] {
					t.Errorf("строка %d: имя %q повторяется в области %s", n+1, m[2], scope)
				}
				seen[key] = true
			}
			switch {
			case strings.HasSuffix(trimmed, "{"):
				stack = append(stack, fmt.Sprint(n))
			case trimmed == "}":
				stack = stack[:len(stack)-1]
			}
		}
	}
	dsl := mustViews(t, shopFS(t, nil))["views/c4/workspace.dsl"]
	mustContain(t, "workspace.dsl", dsl, `biz_buyer = person "Покупатель (biz.buyer)"`, `biz_customer = person "Покупатель (biz.customer)"`)
}

// gap.yaml читается один раз, целевые процессы только по ссылкам разрывов.
func TestLoadChangeNoListReadsOnce(t *testing.T) {
	var reads []string
	fsys := noListFS{fsys: shopFS(t, nil), reads: &reads}
	if _, ds, err := LoadChangeNoList(fsys, "refunds"); err != nil || ds.HasErrors() {
		t.Fatalf("%v\n%s", err, dump(ds))
	}
	count := map[string]int{}
	for _, p := range reads {
		count[p]++
	}
	if count[refunds+"/gap.yaml"] != 1 {
		t.Errorf("gap.yaml прочитан %d раз", count[refunds+"/gap.yaml"])
	}
	for p := range count {
		if strings.HasPrefix(p, refunds+"/processes/") && p != refunds+"/processes/biz.refund.bpmn" {
			t.Errorf("прочитан процесс вне разрывов: %s", p)
		}
	}
}

// failFS отдаёт сбой (не «нет файла») для одного пути, как недоступный
// хостинг при чтении по API.
type failFS struct {
	fstest.MapFS
	path string
}

var errHosting = errors.New("хостинг ответил 502")

func (f failFS) Open(name string) (fs.File, error) {
	if name == f.path {
		return nil, errHosting
	}
	return f.MapFS.Open(name)
}

func (f failFS) ReadFile(name string) ([]byte, error) {
	if name == f.path {
		return nil, errHosting
	}
	return f.MapFS.ReadFile(name)
}

// Сбой чтения определения viewpoint'а — ошибка чтения, а не дефект
// viewpoint.missing: снимок получает error, а не ложный invalid.
func TestViewpointReadFailureIsError(t *testing.T) {
	fsys := failFS{MapFS: shopFS(t, nil), path: "viewpoints/security.yaml"}
	if _, err := BuildSnapshot(fsys, SnapshotOptions{}); !errors.Is(err, errHosting) {
		t.Errorf("BuildSnapshot: %v", err)
	}
	if _, err := Validate(fsys); !errors.Is(err, errHosting) {
		t.Errorf("Validate: %v", err)
	}
	ds := mustValidate(t, shopFS(t, map[string]string{"viewpoints/security.yaml": ""}))
	find(t, ds, "viewpoint.missing", ManifestFile, "/viewpoints/0")
}
