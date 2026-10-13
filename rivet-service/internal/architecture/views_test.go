package architecture

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// Представления эталонного описания — golden-файлы генератора.
func TestShopViewsUpToDate(t *testing.T) {
	if *update {
		if _, err := WriteViews(shopDir); err != nil {
			t.Fatal(err)
		}
	}
	ds, err := CheckViews(os.DirFS(shopDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Fatalf("представления эталона устарели (go test -update):\n%s", dump(ds))
	}
}

func TestViewsDeterministic(t *testing.T) {
	first, _, err := Views(shopFS(t, nil))
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		again, _, _ := Views(shopFS(t, nil))
		if !reflect.DeepEqual(first, again) {
			t.Fatal("представления отличаются между запусками")
		}
	}
	want := []string{"views/README.md", "views/application.md", "views/business.md", "views/changes/refunds.md",
		"views/data.md", "views/processes/biz.checkout.md", "views/processes/biz.delivery.md", "views/security.md",
		"views/technology.md"}
	if got := sortedKeys(first); !reflect.DeepEqual(got, want) {
		t.Errorf("файлы %v", got)
	}
}

// Сценарий «Представления на GitHub».
func TestViewsLayer(t *testing.T) {
	gen, _, _ := Views(shopFS(t, map[string]string{
		"model/application.yaml": strings.Replace(mustRead(t, shopDir+"/model/application.yaml"),
			"name: Веб-интерфейс витрины", `name: Веб-интерфейс "Витрина" <beta>`, 1),
	}))
	app := string(gen["views/application.md"])
	for _, s := range []string{
		"```mermaid\nflowchart LR\n",
		`subgraph n_type_component["Компоненты"]`,
		`n_app_web_ui["Веб-интерфейс #quot;Витрина#quot; #lt;beta#gt;"]`,
		`n_app_orders -->|"access: write"| n_data_order`,
		"| `app.orders` | component | Сервис заказов |  |",
	} {
		if !strings.Contains(app, s) {
			t.Errorf("в views/application.md нет %q:\n%s", s, app)
		}
	}
}

// Сценарий «Подсветка изменения».
func TestViewsChangeHighlight(t *testing.T) {
	gen, _, _ := Views(shopFS(t, nil))
	ch := string(gen["views/changes/refunds.md"])
	for _, s := range []string{
		"class n_app_refund_form,n_app_refunds,n_biz_refund added",
		"class n_app_storefront,n_biz_manager,n_tech_k8s modified",
		"class n_app_mail_refunds removed",
		`n_app_mail_refunds -.->|"access: read"| n_data_order`,
		"| `gap.mail-refunds` | remove | Разбор писем о возвратах больше не нужен | `app.mail-refunds` |  |",
		"### Цель",
		"### Возврат заказа (`biz.refund`)",
	} {
		if !strings.Contains(ch, s) {
			t.Errorf("в представлении изменения нет %q:\n%s", s, ch)
		}
	}
}

// Сценарий «Устаревшие представления».
func TestViewsStale(t *testing.T) {
	fsys := shopFS(t, map[string]string{
		"model/data.yaml": "elements:\n  - {id: data.order, type: data-object, name: Заказ в базе}\n",
		"views/manual.md": "заметка",
	})
	ds, err := CheckViews(fsys)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"views/data.md", "views/application.md", "views/manual.md"} {
		find(t, ds, "views.stale", f, "")
	}
	if !ds.HasErrors() {
		t.Error("устаревшие представления должны быть ошибкой")
	}
	delete(fsys, "views/processes/biz.delivery.md")
	ds, _ = CheckViews(fsys)
	if d := find(t, ds, "views.stale", "views/processes/biz.delivery.md", ""); !strings.Contains(d.Message, "нет") {
		t.Errorf("сообщение: %s", d.Message)
	}
}

func TestViewsSkipInvalidChange(t *testing.T) {
	gen, ds, err := Views(shopFS(t, map[string]string{refunds + "/vision.md": ""}))
	if err != nil {
		t.Fatal(err)
	}
	find(t, ds, "change.vision", refunds+"/vision.md", "")
	if _, ok := gen["views/changes/refunds.md"]; ok {
		t.Error("представление некорректного изменения не строится")
	}
	if _, ok := gen["views/application.md"]; !ok {
		t.Error("представления текущей модели строятся и при сломанном изменении")
	}
	gen, ds, _ = Views(shopFS(t, map[string]string{"model/data.yaml": "elements: [{id: data.x}]\n"}))
	if gen != nil || !ds.HasErrors() {
		t.Error("при ошибках текущего описания представлений нет")
	}
}

func TestViewsSplitLargeLayer(t *testing.T) {
	var b strings.Builder
	b.WriteString("elements:\n")
	for i := range 40 {
		fmt.Fprintf(&b, "  - {id: app.c%02d, type: component, name: C%d}\n", i, i)
	}
	for i := range 25 {
		fmt.Fprintf(&b, "  - {id: app.i%02d, type: interface, name: I%d, relations: [{type: serving, to: app.c00}]}\n", i, i)
	}
	gen, ds, err := Views(minimalFS(map[string]string{"model/application.yaml": b.String()}))
	if err != nil || ds.HasErrors() {
		t.Fatalf("%v\n%s", err, dump(ds))
	}
	app := string(gen["views/application.md"])
	if strings.Count(app, "```mermaid") != 2 || !strings.Contains(app, "## Компоненты") || !strings.Contains(app, "## Интерфейсы") {
		t.Errorf("большой слой не разделён по типам:\n%s", app)
	}
}

func TestViewsCustomTable(t *testing.T) {
	fsys := minimalFS(map[string]string{
		"viewpoints/risk.yaml": "id: risk\ntitle: Риски\nprompt: x\nitems: /risks\nschema: true\nrender: {kind: table}\n",
		"model/risk.yaml":      "risks:\n  - {id: risk.b, title: Б, level: 2, tags: [x, y]}\n  - {id: risk.a, title: \"A | B\"}\n",
	})
	fsys[ManifestFile] = &fstest.MapFile{Data: []byte("format: rivet.architecture/v1\nsystem: T\nviewpoints: [risk]\n")}
	gen, ds, err := Views(fsys)
	if err != nil || ds.HasErrors() {
		t.Fatalf("%v\n%s", err, dump(ds))
	}
	want := "| id | level | tags | title |\n|---|---|---|---|\n| `risk.a` |  |  | A \\| B |\n| `risk.b` | 2 | x, y | Б |\n"
	if got := string(gen["views/risk.md"]); !strings.HasSuffix(got, want) {
		t.Errorf("таблица:\n%s", got)
	}
}

func TestDemoteHeadings(t *testing.T) {
	in := "## Цель\nтекст\n```\n# не заголовок\n```\n# Рамки"
	want := "### Цель\nтекст\n```\n# не заголовок\n```\n## Рамки"
	if got := demoteHeadings(in); got != want {
		t.Errorf("получили\n%s", got)
	}
}

func TestMermaidLabelEscaping(t *testing.T) {
	if got := mlabel("A | \"B\" <c>\n d"); got != `"A #124; #quot;B#quot; #lt;c#gt; d"` {
		t.Errorf("подпись %s", got)
	}
	if got := mlabel(""); got != `" "` {
		t.Errorf("пустая подпись %s", got)
	}
}
