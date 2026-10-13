package architecture

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func TestParseYAMLStrict(t *testing.T) {
	bad := map[string]string{
		"якорь":           "a: &x 1\nb: 2\n",
		"ссылка":          "a: &x 1\nb: *x\n",
		"два документа":   "a: 1\n---\nb: 2\n",
		"повторный ключ":  "a: 1\na: 2\n",
		"ключ слияния":    "base: {a: 1}\nother:\n  <<: {b: 2}\n",
		"числовой ключ":   "1: a\n",
		"чужой тег":       "a: !custom x\n",
		"бинарные данные": "a: !!binary aGVsbG8=\n",
		"синтаксис":       "a: [1, 2\n",
	}
	for name, src := range bad {
		if _, _, err := parseYAML([]byte(src)); err == nil {
			t.Errorf("%s: разбор должен отказать", name)
		}
	}
	_, v, err := parseYAML([]byte("s: текст\ni: 3\nf: 1.5\nb: true\nn: null\nd: 2026-10-13\nl: [a, 1]\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"s": "текст", "i": jsonNum("3"), "f": jsonNum("1.5"), "b": true, "n": nil,
		"d": "2026-10-13", "l": []any{"a", jsonNum("1")}}
	if !reflect.DeepEqual(v, want) {
		t.Errorf("значение %#v, ожидали %#v", v, want)
	}
	if _, v, err := parseYAML(nil); err != nil || !reflect.DeepEqual(v, map[string]any{}) {
		t.Errorf("пустой файл: %#v, %v", v, err)
	}
}

func TestAbsentDescription(t *testing.T) {
	if _, err := Validate(fstest.MapFS{}); !errors.Is(err, ErrAbsent) {
		t.Fatalf("ожидали ErrAbsent, получили %v", err)
	}
}

// Сценарий «Минимальное описание».
func TestMinimalDescription(t *testing.T) {
	fsys := minimalFS(map[string]string{
		"model/business.yaml":    "elements: []\n",
		"model/application.yaml": "elements: []\n",
	})
	m, ds, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Fatalf("дефекты в минимальном описании:\n%s", dump(ds))
	}
	if n := len(m.Elements()); n != 0 {
		t.Errorf("элементов %d, ожидали 0", n)
	}
}

// Сценарий «Неизвестная версия формата»: остальные файлы не разбираются.
func TestUnsupportedFormat(t *testing.T) {
	fsys := fstest.MapFS{
		ManifestFile:          {Data: []byte("format: rivet.architecture/v2\nsystem: Тест\n")},
		"model/business.yaml": {Data: []byte("elements: [{id: X}]\n")},
	}
	ds := mustValidate(t, fsys)
	if len(ds) != 1 {
		t.Fatalf("ожидали один дефект, получили:\n%s", dump(ds))
	}
	d := find(t, ds, "format.unsupported", ManifestFile, "/format")
	if !strings.Contains(d.Message, "неподдерживаемая версия формата") {
		t.Errorf("сообщение: %s", d.Message)
	}
}

// Сценарий «Принципы архитектуры»: дубли идентификаторов отклоняются.
func TestPrinciples(t *testing.T) {
	fsys := fstest.MapFS{ManifestFile: {Data: []byte(`format: rivet.architecture/v1
system: Тест
principles:
  - {id: p.one, title: Первый}
  - {id: p.two, title: Второй, rationale: Потому что}
  - {id: p.one, title: Первый ещё раз}
`)}}
	m, ds, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Manifest.Principles) != 3 || m.Manifest.Principles[1].Rationale != "Потому что" {
		t.Errorf("принципы: %+v", m.Manifest.Principles)
	}
	find(t, ds, "principle.duplicate", ManifestFile, "p.one")
}

func TestManifestStrict(t *testing.T) {
	fsys := fstest.MapFS{ManifestFile: {Data: []byte("format: rivet.architecture/v1\nsytem: Тест\n")}}
	ds := mustValidate(t, fsys)
	if !ds.HasErrors() {
		t.Fatal("опечатка в ключе манифеста должна быть ошибкой")
	}
	for _, d := range ds {
		if d.Rule != "schema.violation" || d.File != ManifestFile {
			t.Errorf("неожиданный дефект %s", d)
		}
	}
}

func TestElementRules(t *testing.T) {
	cases := []struct {
		name, file, src string
		rule, at, loc   string
		msg             string
	}{
		{"шаблон id", "model/application.yaml", "elements:\n  - {id: App, type: component, name: A}\n",
			"id.pattern", "model/application.yaml", "App", ""},
		{"неизвестный тип", "model/application.yaml", "elements:\n  - {id: app.a, type: microservice, name: A}\n",
			"element.type-unknown", "model/application.yaml", "app.a", `"microservice"`},
		// Сценарий «Элемент не в своём слое».
		{"не свой слой", "model/business.yaml", "elements:\n  - {id: app.a, type: component, name: A}\n",
			"element.layer", "model/business.yaml", "app.a", "model/application.yaml"},
		// Сценарий «Ссылка на несуществующий элемент».
		{"неразрешённая ссылка", "model/application.yaml",
			"elements:\n  - {id: app.a, type: component, name: A, relations: [{type: flow, to: app.missing}]}\n",
			"ref.unresolved", "model/application.yaml", "app.a", "app.missing"},
		{"неизвестная связь", "model/application.yaml",
			"elements:\n  - {id: app.a, type: component, name: A, relations: [{type: uses, to: app.a}]}\n",
			"relation.type-unknown", "model/application.yaml", "app.a", `"uses"`},
		{"режим не у access", "model/application.yaml",
			"elements:\n  - {id: app.a, type: component, name: A}\n  - {id: app.b, type: component, name: B, relations: [{type: flow, to: app.a, mode: read}]}\n",
			"relation.mode", "model/application.yaml", "app.b", ""},
		{"связь в себя", "model/application.yaml",
			"elements:\n  - {id: app.a, type: component, name: A, relations: [{type: flow, to: app.a}]}\n",
			"relation.self", "model/application.yaml", "app.a", ""},
		{"лишнее поле", "model/application.yaml", "elements:\n  - {id: app.a, type: component, name: A, relatons: []}\n",
			"schema.violation", "model/application.yaml", "app.a", "additionalProperties"},
		{"нет названия", "model/application.yaml", "elements:\n  - {id: app.a, type: component}\n",
			"schema.violation", "model/application.yaml", "app.a", "required"},
		{"неверный режим", "model/data.yaml",
			"elements:\n  - {id: data.a, type: data-object, name: A}\n  - {id: data.b, type: data-object, name: B, relations: [{type: association, to: data.a, mode: all}]}\n",
			"schema.violation", "model/data.yaml", "data.b/relations/0/mode", "enum"},
		{"пустой тип", "model/application.yaml", "elements:\n  - {id: app.a, type: \"\", name: A}\n",
			"schema.violation", "model/application.yaml", "app.a/type", "minLength"},
		{"пустая цель связи", "model/application.yaml",
			"elements:\n  - {id: app.a, type: component, name: A, relations: [{type: flow, to: \"\"}]}\n",
			"schema.violation", "model/application.yaml", "app.a/relations/0/to", "minLength"},
		{"пустой id", "model/application.yaml", "elements:\n  - {id: \"\", type: component, name: A}\n",
			"schema.violation", "model/application.yaml", "/elements/0/id", "minLength"},
		{"якорь YAML", "model/data.yaml", "elements:\n  - &a {id: data.a, type: data-object, name: A}\n",
			"yaml.parse", "model/data.yaml", "", "якоря"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ds := mustValidate(t, minimalFS(map[string]string{c.file: c.src}))
			d := find(t, ds, c.rule, c.at, c.loc)
			if d.Severity != SeverityError {
				t.Errorf("важность %s", d.Severity)
			}
			if !strings.Contains(d.Message, c.msg) {
				t.Errorf("сообщение %q не содержит %q", d.Message, c.msg)
			}
		})
	}
}

func TestDuplicateIDAcrossLayers(t *testing.T) {
	ds := mustValidate(t, minimalFS(map[string]string{
		"model/application.yaml": "elements:\n  - {id: x.a, type: component, name: A}\n",
		"model/data.yaml":        "elements:\n  - {id: x.a, type: data-object, name: A}\n",
	}))
	d := find(t, ds, "id.duplicate", "model/data.yaml", "x.a")
	if !strings.Contains(d.Message, "model/application.yaml") {
		t.Errorf("сообщение не называет первый файл: %s", d.Message)
	}
}

// Сценарий «Корректная модель».
func TestCorrectModel(t *testing.T) {
	ds := mustValidate(t, minimalFS(map[string]string{
		"model/application.yaml": `elements:
  - id: app.rivetd
    type: component
    name: rivetd
    relations:
      - {type: serving, to: app.web}
  - id: app.web
    type: component
    name: Консоль
`,
		"model/technology.yaml": `elements:
  - id: tech.postgres
    type: node
    name: PostgreSQL
    relations:
      - {type: serving, to: app.rivetd}
`,
	}))
	if len(ds) != 0 {
		t.Fatalf("дефекты:\n%s", dump(ds))
	}
}

// Сценарий «Недопустимая связь».
func TestRelationNotAllowed(t *testing.T) {
	ds := mustValidate(t, minimalFS(map[string]string{
		"model/business.yaml":   "elements:\n  - {id: biz.user, type: actor, name: Пользователь, relations: [{type: realization, to: tech.vm}]}\n",
		"model/technology.yaml": "elements:\n  - {id: tech.vm, type: node, name: ВМ}\n",
	}))
	d := find(t, ds, "relation.not-allowed", "model/business.yaml", "biz.user")
	if d.Message != "связь realization недопустима между actor и node" {
		t.Errorf("сообщение: %s", d.Message)
	}
}

func TestRelationTable(t *testing.T) {
	allowed := [][3]string{
		{RelAssignment, TypeActor, TypeRole},
		{RelAssignment, TypeRole, TypeProcess},
		{RelAssignment, TypeNode, TypeSystemSoftware},
		{RelRealization, TypeComponent, TypeApplicationService},
		{RelRealization, TypeDataObject, TypeBusinessObject},
		{RelServing, TypeComponent, TypeComponent},
		{RelServing, TypeInterface, TypeRole},
		{RelServing, TypeSystemSoftware, TypeNode},
		{RelAccess, TypeSystemSoftware, TypeDataObject},
		{RelFlow, TypeProcess, TypeProcess},
		{RelTriggering, TypeComponent, TypeComponent},
		{RelComposition, TypeActor, TypeBusinessObject},
		{RelAssociation, TypeActor, TypeNode},
	}
	for _, c := range allowed {
		if !relationAllowed(c[0], c[1], c[2]) {
			t.Errorf("%s %s → %s должна быть допустима", c[0], c[1], c[2])
		}
	}
	denied := [][3]string{
		{RelRealization, TypeActor, TypeNode},
		{RelComposition, TypeComponent, TypeNode},
		{RelAccess, TypeNode, TypeDataObject},
		{RelFlow, TypeProcess, TypeComponent},
		{RelServing, TypeRole, TypeActor},
		{"uses", TypeComponent, TypeComponent},
	}
	for _, c := range denied {
		if relationAllowed(c[0], c[1], c[2]) {
			t.Errorf("%s %s → %s должна быть недопустима", c[0], c[1], c[2])
		}
	}
}

// Сценарий «Несколько дефектов сразу».
func TestManyDefectsAtOnce(t *testing.T) {
	ds := mustValidate(t, minimalFS(map[string]string{
		"model/business.yaml": `elements:
  - {id: biz.user, type: actor, name: Пользователь, relations: [{type: realization, to: tech.vm}, {type: assignment, to: biz.nobody}]}
`,
		"model/application.yaml": "elements:\n  - {id: app.a, type: component, name: A, relations: [{type: flow, to: app.ghost}]}\n",
		"model/technology.yaml":  "elements:\n  - {id: tech.vm, type: node, name: ВМ}\n",
	}))
	if got := rules(ds); !reflect.DeepEqual(got, []string{"ref.unresolved", "ref.unresolved", "relation.not-allowed"}) {
		t.Fatalf("правила %v:\n%s", got, dump(ds))
	}
	if errs, _ := ds.Count(); errs != 3 {
		t.Errorf("ошибок %d", errs)
	}
}

// Сценарий «Только предупреждения».
func TestOnlyWarnings(t *testing.T) {
	ds := mustValidate(t, minimalFS(map[string]string{
		"model/business.yaml": "elements:\n  - {id: biz.p, type: process, name: Процесс}\n",
		"processes/biz.p.bpmn": bpmnDoc(`
    <bpmn:startEvent id="s"/>
    <bpmn:subProcess id="sub"/>
    <bpmn:endEvent id="e"/>
    <bpmn:sequenceFlow id="f1" sourceRef="s" targetRef="sub"/>
    <bpmn:sequenceFlow id="f2" sourceRef="sub" targetRef="e"/>`),
	}))
	if ds.HasErrors() || len(ds) != 1 || ds[0].Severity != SeverityWarning || ds[0].Rule != "bpmn.unsupported" {
		t.Fatalf("ожидали одно предупреждение:\n%s", dump(ds))
	}
}

func TestUnreferencedFiles(t *testing.T) {
	ds := mustValidate(t, minimalFS(map[string]string{
		"model/extra.yaml":     "a: 1\n",
		"processes/lost.bpmn":  bpmnDoc(""),
		"viewpoints/old.yaml":  "id: old\n",
		"processes/.gitkeep":   "",
		"model/business.yaml":  "elements: []\n",
		"model/.hidden.yaml":   "",
		"views/application.md": "x",
	}))
	for _, f := range []string{"model/extra.yaml", "processes/lost.bpmn", "viewpoints/old.yaml"} {
		if d := find(t, ds, "file.unreferenced", f, ""); d.Severity != SeverityWarning {
			t.Errorf("%s: важность %s", f, d.Severity)
		}
	}
	if len(ds) != 3 {
		t.Errorf("лишние дефекты:\n%s", dump(ds))
	}
}

// Сценарий «Детерминированная проверка»: одинаковый вход, одинаковый
// результат в стабильном порядке.
func TestDeterministic(t *testing.T) {
	fsys := shopFS(t, map[string]string{
		"model/application.yaml": "elements:\n  - {id: app.z, type: component, name: Z, relations: [{type: flow, to: app.q}, {type: flow, to: app.p}]}\n",
	})
	first := mustValidate(t, fsys)
	for range 20 {
		if again := mustValidate(t, fsys); !reflect.DeepEqual(first, again) {
			t.Fatalf("результат отличается:\n%s\nпротив\n%s", dump(first), dump(again))
		}
	}
}

func TestShopValid(t *testing.T) {
	m, ds, err := Load(shopFS(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Fatalf("эталонное описание с дефектами:\n%s", dump(ds))
	}
	if n := len(m.Elements()); n != 16 {
		t.Errorf("элементов %d", n)
	}
	if e, ok := m.Element("app.orders"); !ok || e.Props["replicas"] != jsonNum("3") {
		t.Errorf("app.orders: %+v", e)
	}
	if ds := mustValidate(t, shopFS(t, nil)); len(ds) != 0 {
		t.Fatalf("эталонное описание с изменениями не проходит проверку:\n%s", dump(ds))
	}
}
