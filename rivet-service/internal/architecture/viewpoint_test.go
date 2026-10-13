package architecture

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Сценарий «Собственный viewpoint проекта».
func TestCustomViewpointSecurity(t *testing.T) {
	m, ds, err := Load(shopFS(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	vp := m.Viewpoint("security")
	if vp == nil || vp.Builtin() || vp.Title() != "Угрозы и меры защиты" || !strings.Contains(vp.Prompt(), "targets") {
		t.Fatalf("viewpoint security: %+v", vp)
	}
	if got := len(m.Viewpoints()); got != 5 {
		t.Errorf("viewpoint'ов %d, ожидали 4 встроенных и security", got)
	}

	ds = mustValidate(t, shopFS(t, map[string]string{
		"model/security.yaml": "threats:\n  - {id: sec.card-leak, title: Утечка, targets: [app.storefront, app.ghost]}\n",
	}))
	d := find(t, ds, "ref.unresolved", "model/security.yaml", "sec.card-leak/targets/1")
	if !strings.Contains(d.Message, "app.ghost") {
		t.Errorf("сообщение: %s", d.Message)
	}
}

// Сценарий «Документ не соответствует схеме».
func TestCustomViewpointSchemaViolation(t *testing.T) {
	ds := mustValidate(t, shopFS(t, map[string]string{
		"model/security.yaml": "threats:\n  - {id: sec.card-leak, severity: extreme, targets: [app.storefront]}\n",
	}))
	for _, c := range []struct{ loc, keyword string }{
		{"sec.card-leak", "required"},
		{"sec.card-leak/severity", "enum"},
	} {
		d := find(t, ds, "schema.violation", "model/security.yaml", c.loc)
		if !strings.Contains(d.Message, c.keyword) {
			t.Errorf("%s: сообщение без правила %s: %s", c.loc, c.keyword, d.Message)
		}
	}
}

// Сценарий «Конфликт идентификатора».
func TestCustomViewpointBuiltinConflict(t *testing.T) {
	ds := mustValidate(t, shopFS(t, map[string]string{
		ManifestFile: "format: rivet.architecture/v1\nsystem: Магазин\nviewpoints: [security, application]\n",
	}))
	d := find(t, ds, "viewpoint.builtin-conflict", ManifestFile, "/viewpoints/1")
	if !strings.Contains(d.Message, "application") {
		t.Errorf("сообщение: %s", d.Message)
	}
}

func TestCustomViewpointDefinitionErrors(t *testing.T) {
	const def = "id: risk\ntitle: Риски\nprompt: Опиши риски\nitems: /risks\n"
	cases := []struct {
		name, src, rule, loc, msg string
	}{
		{"нет файла", "", "viewpoint.missing", "/viewpoints/0", "viewpoints/risk.yaml"},
		{"ребро не ссылка", def + "schema: {type: object}\nrender: {kind: graph, edges: [title]}\n",
			"viewpoint.render", "/render/edges/0", "x-rivet-ref"},
		{"схема не компилируется", def + "schema: {type: 5}\nrender: {kind: table}\n",
			"viewpoint.schema", "/schema", ""},
		{"внешняя схема", def + "schema: {$ref: 'file:///etc/passwd'}\nrender: {kind: table}\n",
			"viewpoint.schema", "/schema", "внешние ресурсы"},
		{"чужой id", "id: other\ntitle: Риски\nprompt: x\nitems: /risks\nschema: true\nrender: {kind: table}\n",
			"viewpoint.id-mismatch", "/id", ""},
		{"неизвестный способ отрисовки", def + "schema: true\nrender: {kind: chart}\n",
			"schema.violation", "/render/kind", "enum"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{}
			if c.src != "" {
				files["viewpoints/risk.yaml"] = c.src
			}
			fsys := minimalFS(files)
			fsys[ManifestFile].Data = []byte("format: rivet.architecture/v1\nsystem: T\nviewpoints: [risk]\n")
			ds := mustValidate(t, fsys)
			file := "viewpoints/risk.yaml"
			if c.rule == "viewpoint.missing" {
				file = ManifestFile
			}
			d := find(t, ds, c.rule, file, c.loc)
			if !strings.Contains(d.Message, c.msg) {
				t.Errorf("сообщение %q не содержит %q", d.Message, c.msg)
			}
		})
	}
}

func TestCustomViewpointItems(t *testing.T) {
	fsys := minimalFS(map[string]string{
		"viewpoints/risk.yaml": "id: risk\ntitle: Риски\nprompt: x\nitems: /risks\nschema: true\nrender: {kind: table}\n",
		"model/risk.yaml":      "risks:\n  - {id: risk.a, title: A}\n  - {title: без id}\n",
	})
	fsys[ManifestFile].Data = []byte("format: rivet.architecture/v1\nsystem: T\nviewpoints: [risk]\n")
	ds := mustValidate(t, fsys)
	find(t, ds, "doc.item-id", "model/risk.yaml", "/risks/1")

	fsys["model/risk.yaml"].Data = []byte("risks: {a: 1}\n")
	ds = mustValidate(t, fsys)
	find(t, ds, "doc.items", "model/risk.yaml", "/risks")
}

func TestRefPaths(t *testing.T) {
	_, schema, err := parseYAML([]byte(`
type: object
properties:
  threats:
    type: array
    items: {$ref: '#/$defs/threat'}
  owners:
    type: object
    additionalProperties: {type: string, x-rivet-ref: true}
$defs:
  threat:
    type: object
    properties:
      targets: {type: array, items: {type: string, x-rivet-ref: true}}
      main: {anyOf: [{type: string, x-rivet-ref: true}, {type: "null"}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	scan := refPaths(schema)
	if got := pathStrings(scan.paths); !reflect.DeepEqual(got, []string{"owners/*", "threats/*/targets/*"}) {
		t.Errorf("пути %v", got)
	}
	if !reflect.DeepEqual(scan.conditional, []string{"/threats/*/main"}) {
		t.Errorf("пометки в условных ветках %v", scan.conditional)
	}
}

func pathStrings(paths [][]refStep) []string {
	var out []string
	for _, p := range paths {
		parts := make([]string, len(p))
		for i, st := range p {
			parts[i] = st.String()
		}
		out = append(out, strings.Join(parts, "/"))
	}
	return out
}

// Ссылкой считается ровно то, что помечает схема: позиция prefixItems,
// свойства по шаблону, остальные свойства объекта.
func TestRefPathsPrecise(t *testing.T) {
	_, schema, err := parseYAML([]byte(`
type: object
properties:
  pair:
    type: array
    prefixItems: [{type: string, x-rivet-ref: true}, {type: string}]
    items: {type: string, x-rivet-ref: true}
  links:
    type: object
    properties:
      note: {type: string}
    patternProperties:
      "^app-": {type: string, x-rivet-ref: true}
    additionalProperties: {type: string, x-rivet-ref: true}
`))
	if err != nil {
		t.Fatal(err)
	}
	paths := refPaths(schema).paths
	if got := pathStrings(paths); !reflect.DeepEqual(got, []string{"links/~^app-", "links/*", "pair/[0]", "pair/[2:]"}) {
		t.Errorf("пути %v", got)
	}
	_, doc, _ := parseYAML([]byte(`
pair: [app.a, просто текст, app.c]
links: {note: заметка, app-main: app.d, other: app.e}
`))
	var refs []string
	for _, p := range paths {
		for _, hit := range collectRefs(doc, p) {
			refs = append(refs, formatPointer(hit.path)+"="+hit.value)
		}
	}
	want := []string{"/links/app-main=app.d", "/links/other=app.e", "/pair/0=app.a", "/pair/2=app.c"}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("ссылки %v, ожидали %v", refs, want)
	}
}

func TestBuiltinViewpoints(t *testing.T) {
	var ids, titles []string
	for _, vp := range BuiltinViewpoints() {
		ids = append(ids, vp.ID())
		titles = append(titles, vp.Title())
		if !vp.Builtin() || vp.Prompt() == "" || vp.File() != "model/"+vp.ID()+".yaml" {
			t.Errorf("viewpoint %s: %+v", vp.ID(), vp)
		}
	}
	if !reflect.DeepEqual(ids, []string{"business", "application", "data", "technology"}) ||
		!reflect.DeepEqual(titles, []string{"Бизнес", "Приложения", "Данные", "Технологии"}) {
		t.Errorf("встроенные: %v %v", ids, titles)
	}
}

// Разные additionalProperties в ветках allOf — разные пути, а одно поле
// под двумя путями даёт одну ссылку.
func TestRefPathsAllOfAdditional(t *testing.T) {
	_, schema, _ := parseYAML([]byte(`
allOf:
  - {properties: {a: {}}, additionalProperties: {type: string, x-rivet-ref: true}}
  - {properties: {b: {}}, additionalProperties: {type: string, x-rivet-ref: true}}
`))
	paths := refPaths(schema).paths
	if len(paths) != 2 {
		t.Fatalf("путей %d, ожидали 2", len(paths))
	}
	c := &customViewpoint{ptr: []string{"items"}, refs: paths}
	d := &Doc{vp: c, file: "model/x.yaml", value: map[string]any{"a": "app.a", "b": "app.b", "c": "app.c"}}
	var got []string
	for _, ref := range c.references(d) {
		got = append(got, ref.Location+"="+ref.To)
	}
	sort.Strings(got)
	if want := []string{"/a=app.a", "/b=app.b", "/c=app.c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ссылки %v, ожидали %v", got, want)
	}
}

func TestCustomViewpointConditionalRef(t *testing.T) {
	fsys := minimalFS(map[string]string{
		"viewpoints/risk.yaml": `id: risk
title: Риски
prompt: x
items: /risks
schema:
  type: object
  properties:
    risks:
      type: array
      items:
        type: object
        if: {properties: {kind: {const: linked}}}
        then: {properties: {value: {type: string, x-rivet-ref: true}}}
render: {kind: table}
`,
	})
	fsys[ManifestFile].Data = []byte("format: rivet.architecture/v1\nsystem: T\nviewpoints: [risk]\n")
	ds := mustValidate(t, fsys)
	d := find(t, ds, "viewpoint.ref-conditional", "viewpoints/risk.yaml", "/schema")
	if !strings.Contains(d.Message, "/risks/*/value") {
		t.Errorf("сообщение: %s", d.Message)
	}
}

// Имена с запятыми и слэшами не склеивают разные пути.
func TestRefPathsNoCollisions(t *testing.T) {
	_, schema, _ := parseYAML([]byte(`
properties:
  a/b: {type: string, x-rivet-ref: true}
  a: {properties: {b: {type: string, x-rivet-ref: true}}}
allOf:
  - {properties: {a: {}, b: {}}, additionalProperties: {x-rivet-ref: true}}
  - {properties: {"a,b": {}}, additionalProperties: {x-rivet-ref: true}}
`))
	if n := len(refPaths(schema).paths); n != 4 {
		t.Errorf("путей %d, ожидали 4", n)
	}
}

// Пометки находятся и через $anchor, условные ветки через $anchor тоже
// ловятся, а нелокальные ссылки отклоняются.
func TestRefPathsAnchorsAndUnsupported(t *testing.T) {
	_, schema, _ := parseYAML([]byte(`
$defs:
  target: {$anchor: target, type: string, x-rivet-ref: true}
  cond: {$anchor: cond, type: string, x-rivet-ref: true}
properties:
  x: {$ref: '#target'}
  y: {if: {$ref: '#cond'}}
  z: {$ref: 'item.json'}
  w: {$dynamicRef: '#meta'}
`))
	scan := refPaths(schema)
	if got := pathStrings(scan.paths); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("пути %v", got)
	}
	if !reflect.DeepEqual(scan.conditional, []string{"/y"}) {
		t.Errorf("условные %v", scan.conditional)
	}
	if !reflect.DeepEqual(scan.unsupported, []string{"$dynamicRef #meta", "$ref item.json"}) {
		t.Errorf("неподдерживаемые %v", scan.unsupported)
	}
}

// Каждое условное ключевое слово отклоняет пометку.
func TestRefPathsConditionalKeywords(t *testing.T) {
	const ref = "{type: string, x-rivet-ref: true}"
	for kw, src := range map[string]string{
		"if":               "if: " + ref,
		"then":             "then: " + ref,
		"else":             "else: " + ref,
		"not":              "not: " + ref,
		"anyOf":            "anyOf: [" + ref + "]",
		"oneOf":            "oneOf: [" + ref + "]",
		"contains":         "contains: " + ref,
		"dependentSchemas": "dependentSchemas: {a: " + ref + "}",
	} {
		_, schema, err := parseYAML([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if scan := refPaths(schema); len(scan.paths) != 0 || len(scan.conditional) != 1 {
			t.Errorf("%s: пути %v, условные %v", kw, pathStrings(scan.paths), scan.conditional)
		}
	}
}

// Якорь внутри данных схемы не затеняет настоящий, повторный якорь
// неоднозначен.
func TestSchemaAnchorsSkipData(t *testing.T) {
	_, schema, _ := parseYAML([]byte(`
default: {$anchor: target}
properties:
  x: {$anchor: target, type: string, x-rivet-ref: true}
  y: {$ref: '#target'}
  a: {$anchor: twice}
  b: {$anchor: twice}
  c: {$ref: '#twice'}
`))
	scan := refPaths(schema)
	if got := pathStrings(scan.paths); !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Errorf("пути %v", got)
	}
	if !reflect.DeepEqual(scan.unsupported, []string{"$ref #twice"}) {
		t.Errorf("неподдерживаемые %v", scan.unsupported)
	}
}
