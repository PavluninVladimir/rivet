package architecture

import (
	"strings"
	"testing"
	"testing/fstest"
)

const refunds = "changes/refunds"

func loadRefunds(t *testing.T, fsys fstest.MapFS) (*Change, Defects) {
	t.Helper()
	ch, ds, err := LoadChange(fsys, "refunds")
	if err != nil {
		t.Fatal(err)
	}
	return ch, ds
}

func TestRefundsChange(t *testing.T) {
	ch, ds := loadRefunds(t, shopFS(t, nil))
	if len(ds) != 0 {
		t.Fatalf("дефекты:\n%s", dump(ds))
	}
	if ch.Info.Title != "Возвраты заказов" || len(ch.Gaps) != 3 || len(ch.Deltas) != 4 {
		t.Fatalf("изменение: %+v, разрывов %d, дельт %d", ch.Info, len(ch.Gaps), len(ch.Deltas))
	}
	tgt := ch.Target
	if _, ok := tgt.Element("app.mail-refunds"); ok {
		t.Error("удалённый элемент остался в целевой модели")
	}
	if _, ok := ch.Current.Element("app.mail-refunds"); !ok {
		t.Error("текущая модель изменилась")
	}
	if e, _ := tgt.Element("app.storefront"); len(e.Relations) != 3 {
		t.Errorf("app.storefront в целевой модели: %+v", e)
	}
	if p := tgt.Process("biz.refund"); p == nil || p.File != refunds+"/processes/biz.refund.bpmn" {
		t.Errorf("целевой процесс: %+v", p)
	}
	if p := tgt.Process("biz.checkout"); p == nil || p.File != "processes/biz.checkout.bpmn" {
		t.Errorf("нетронутый процесс должен остаться текущим: %+v", p)
	}
}

// Сценарий «Корректное изменение»: компонент и интерфейс добавлены,
// процесс изменён, два разрыва покрывают три записи.
func TestCorrectChangeScenario(t *testing.T) {
	newCheckout := strings.Replace(mustRead(t, shopDir+"/processes/biz.checkout.bpmn"),
		`<bpmn:userTask id="cart" name="Проверить корзину"/>`, `<bpmn:userTask id="cart" name="Проверить корзину и промокод"/>`, 1)
	fsys := fstest.MapFS{}
	for p, f := range shopFS(t, nil) {
		if !strings.HasPrefix(p, refunds) {
			fsys[p] = f
		}
	}
	add := map[string]string{
		"changes/promo/change.yaml": "id: promo\ntitle: Промокоды\n",
		"changes/promo/vision.md":   "Покупатель вводит промокод при оформлении заказа.\n",
		"changes/promo/delta/application.yaml": `added:
  - {id: app.promo, type: component, name: Сервис промокодов, relations: [{type: serving, to: app.storefront}]}
  - {id: app.promo-form, type: interface, name: Поле промокода, relations: [{type: serving, to: biz.buyer}]}
`,
		"changes/promo/processes/biz.checkout.bpmn": newCheckout,
		"changes/promo/gap.yaml": `gaps:
  - {id: gap.promo, title: Нет промокодов, kind: add, description: Скидки дают только менеджеры, elements: [app.promo, app.promo-form]}
  - {id: gap.checkout, title: Оформление не знает о промокоде, kind: change, description: Шаг проверки корзины, processes: [biz.checkout]}
`,
	}
	for p, c := range add {
		fsys[p] = &fstest.MapFile{Data: []byte(c)}
	}
	ch, ds, err := LoadChange(fsys, "promo")
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Fatalf("дефекты:\n%s", dump(ds))
	}
	if _, ok := ch.Target.Element("app.promo-form"); !ok {
		t.Error("нет нового интерфейса в целевой модели")
	}
	if p := ch.Target.Process("biz.checkout"); p.node("cart").Name != "Проверить корзину и промокод" {
		t.Errorf("целевая модель содержит старую версию процесса")
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	f, ok := shopFS(t, nil)[strings.TrimPrefix(p, shopDir+"/")]
	if !ok {
		t.Fatalf("нет файла %s", p)
	}
	return string(f.Data)
}

func TestChangeRules(t *testing.T) {
	gap := mustRead(t, shopDir+"/"+refunds+"/gap.yaml")
	app := mustRead(t, shopDir+"/"+refunds+"/delta/application.yaml")
	cases := []struct {
		name     string
		files    map[string]string
		rule, at string
		loc, msg string
	}{
		// Сценарий «Запись дельты без разрыва».
		{"запись без разрыва", map[string]string{refunds + "/gap.yaml": strings.Replace(gap, "app.refund-form, ", "", 1)},
			"delta.uncovered", refunds + "/delta/application.yaml", "app.refund-form", "изменение элемента не покрыто разрывом"},
		// Сценарий «Изменение несуществующего элемента».
		{"изменение несуществующего", map[string]string{refunds + "/delta/application.yaml": strings.Replace(app, "id: app.storefront", "id: app.ghost", 1)},
			"delta.target-missing", refunds + "/delta/application.yaml", "app.ghost", "нет в текущей модели"},
		{"элемент в другом слое", map[string]string{refunds + "/delta/data.yaml": "removed:\n  - {id: biz.order, reason: x}\n"},
			"delta.target-missing", refunds + "/delta/data.yaml", "biz.order", "model/business.yaml"},
		{"добавление существующего", map[string]string{refunds + "/delta/data.yaml": "added:\n  - {id: data.order, type: data-object, name: Дубль}\n"},
			"delta.added-exists", refunds + "/delta/data.yaml", "data.order", "model/data.yaml"},
		{"двойная запись", map[string]string{refunds + "/delta/technology.yaml": "removed:\n  - {id: tech.k8s, reason: x}\nmodified:\n  - {id: tech.k8s, type: node, name: K8s}\n"},
			"delta.duplicate", refunds + "/delta/technology.yaml", "tech.k8s", ""},
		{"разрыв вне изменения", map[string]string{refunds + "/gap.yaml": strings.Replace(gap, "elements: [app.mail-refunds]", "elements: [app.mail-refunds, app.web-ui]", 1)},
			"gap.ref-outside", refunds + "/gap.yaml", "gap.mail-refunds", "app.web-ui"},
		{"разрыв на чужой процесс", map[string]string{refunds + "/gap.yaml": strings.Replace(gap, "processes: [biz.refund]", "processes: [biz.refund, biz.checkout]", 1)},
			"gap.ref-outside", refunds + "/gap.yaml", "gap.refund-process", "biz.checkout"},
		{"процесс без разрыва", map[string]string{refunds + "/gap.yaml": strings.Replace(gap, "    processes: [biz.refund]\n", "", 1)},
			"process.uncovered", refunds + "/processes/biz.refund.bpmn", "", "biz.refund"},
		{"разрыв без ссылок", map[string]string{refunds + "/gap.yaml": gap + "  - {id: gap.empty, title: Пусто, kind: change, description: x}\n"},
			"gap.no-refs", refunds + "/gap.yaml", "gap.empty", ""},
		{"повтор разрыва", map[string]string{refunds + "/gap.yaml": gap + "  - {id: gap.mail-refunds, title: Ещё, kind: change, description: x, elements: [tech.k8s]}\n"},
			"gap.duplicate", refunds + "/gap.yaml", "gap.mail-refunds", ""},
		{"лишний процесс", map[string]string{refunds + "/processes/biz.ghost.bpmn": bpmnDoc("")},
			"process.orphan", refunds + "/processes/biz.ghost.bpmn", "", ""},
		{"чужая дельта", map[string]string{refunds + "/delta/infra.yaml": "added: []\n"},
			"delta.viewpoint-unknown", refunds + "/delta/infra.yaml", "", ""},
		{"нет видения", map[string]string{refunds + "/vision.md": ""},
			"change.vision", refunds + "/vision.md", "", ""},
		{"нет разрывов", map[string]string{refunds + "/gap.yaml": ""},
			"gap.missing", refunds + "/gap.yaml", "", ""},
		{"чужой id", map[string]string{refunds + "/change.yaml": "id: other\ntitle: Возвраты\n"},
			"change.id-mismatch", refunds + "/change.yaml", "/id", ""},
		{"удаление без причины", map[string]string{refunds + "/delta/application.yaml": strings.Replace(app, "    reason: Возвраты переезжают в сервис возвратов\n", "", 1)},
			"schema.violation", refunds + "/delta/application.yaml", "/removed/0", "required"},
		// Целевая модель проверяется полностью: ошибка новой записи
		// указывает на файл дельты.
		{"ссылка новой записи", map[string]string{refunds + "/delta/application.yaml": strings.Replace(app, "to: biz.refund}", "to: biz.refundz}", 1)},
			"ref.unresolved", refunds + "/delta/application.yaml", "app.refunds", "biz.refundz"},
		{"новый процесс без BPMN", map[string]string{refunds + "/processes/biz.refund.bpmn": ""},
			"process.file-missing", refunds + "/delta/business.yaml", "biz.refund", "processes/biz.refund.bpmn"},
		{"схема собственного viewpoint'а в дельте", map[string]string{refunds + "/delta/security.yaml": "added:\n  - {id: sec.x, targets: [app.refunds]}\n"},
			"schema.violation", refunds + "/delta/security.yaml", "sec.x", "title"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			over := map[string]string{}
			for p, content := range c.files {
				over[p] = content
			}
			_, ds := loadRefunds(t, shopFS(t, over))
			d := find(t, ds, c.rule, c.at, c.loc)
			if !strings.Contains(d.Message, c.msg) {
				t.Errorf("сообщение %q не содержит %q", d.Message, c.msg)
			}
		})
	}
}

// Сценарий «Удаление используемого элемента».
func TestChangeRemovedInUse(t *testing.T) {
	_, ds := loadRefunds(t, shopFS(t, map[string]string{
		refunds + "/delta/data.yaml": "removed:\n  - {id: data.order, reason: Заказы переезжают}\n",
		refunds + "/gap.yaml": mustRead(t, shopDir+"/"+refunds+"/gap.yaml") +
			"  - {id: gap.orders, title: Заказы, kind: remove, description: x, elements: [data.order]}\n",
	}))
	d := find(t, ds, "delta.removed-in-use", refunds+"/delta/data.yaml", "data.order")
	for _, user := range []string{"app.orders", "app.refunds", "tech.postgres"} {
		if !strings.Contains(d.Message, user) {
			t.Errorf("сообщение не называет %s: %s", user, d.Message)
		}
	}
	for _, x := range ds {
		if x.Rule == "ref.unresolved" {
			t.Errorf("ссылки на удаляемый элемент сообщаются только правилом delta.removed-in-use: %s", x)
		}
	}
}

func TestChangeEmpty(t *testing.T) {
	fsys := shopFS(t, nil)
	for p := range fsys {
		if strings.HasPrefix(p, refunds+"/delta/") || strings.HasPrefix(p, refunds+"/processes/") {
			delete(fsys, p)
		}
	}
	fsys[refunds+"/gap.yaml"] = &fstest.MapFile{Data: []byte("gaps: []\n")}
	_, ds := loadRefunds(t, fsys)
	find(t, ds, "change.empty", refunds+"/change.yaml", "")
}

// Ошибка текущей модели, которую изменение исправляет, не мешает проверке
// изменения. Проверка всего описания её по-прежнему видит.
func TestChangeFixesCurrentError(t *testing.T) {
	app := mustRead(t, shopDir+"/model/application.yaml")
	fsys := shopFS(t, map[string]string{
		"model/application.yaml": strings.Replace(app, "- {type: flow, to: app.orders}", "- {type: flow, to: app.orderz}", 1),
	})
	if _, ds := loadRefunds(t, fsys); ds.HasErrors() {
		t.Fatalf("изменение заменяет app.storefront целиком и исправляет ссылку:\n%s", dump(ds))
	}
	ds := mustValidate(t, fsys)
	find(t, ds, "ref.unresolved", "model/application.yaml", "app.storefront")
}

func TestChangeMissing(t *testing.T) {
	_, ds, err := LoadChange(shopFS(t, nil), "nope")
	if err != nil {
		t.Fatal(err)
	}
	find(t, ds, "change.missing", "changes/nope/change.yaml", "")
	if _, _, err := LoadChange(shopFS(t, nil), "../model"); err == nil {
		t.Error("идентификатор с путём должен отклоняться")
	}
}

func TestActiveChanges(t *testing.T) {
	fsys := shopFS(t, map[string]string{
		"changes/archive/2026-01-01-old/change.yaml": "id: old\n",
		"changes/.gitkeep":                           "x",
	})
	ids, err := ActiveChanges(fsys)
	if err != nil || len(ids) != 1 || ids[0] != "refunds" {
		t.Fatalf("активные изменения %v, %v", ids, err)
	}
}

func TestBadChangeDirName(t *testing.T) {
	ds := mustValidate(t, shopFS(t, map[string]string{"changes/My_Change/change.yaml": "id: x\n"}))
	find(t, ds, "change.id", "changes/My_Change", "")
}

// riskFS — собственный viewpoint с записями внутри массива групп и
// ссылкой в шапке документа.
func riskFS(change map[string]string) fstest.MapFS {
	files := map[string]string{
		"viewpoints/risk.yaml": `id: risk
title: Риски
prompt: Опиши риски
items: /groups/1/risks
schema:
  type: object
  properties:
    owner: {type: string, x-rivet-ref: true}
    groups:
      type: array
      items:
        type: object
        properties:
          name: {type: string}
          risks:
            type: array
            items:
              type: object
              required: [id, title]
              properties:
                id: {type: string}
                title: {type: string}
render: {kind: table}
`,
		"model/risk.yaml": `owner: app.old
groups:
  - name: Первая
    risks: []
  - name: Вторая
    risks:
      - {id: risk.a, title: A}
`,
		"model/application.yaml": "elements:\n  - {id: app.old, type: component, name: Старый}\n  - {id: app.new, type: component, name: Новый}\n",
	}
	for p, c := range change {
		files[p] = c
	}
	fsys := minimalFS(files)
	fsys[ManifestFile] = &fstest.MapFile{Data: []byte("format: rivet.architecture/v1\nsystem: T\nviewpoints: [risk]\n")}
	return fsys
}

// Ссылка из шапки документа собственного viewpoint'а тоже блокирует
// удаление элемента.
func TestChangeRemovedInUseByHeader(t *testing.T) {
	fsys := riskFS(map[string]string{
		"changes/drop/change.yaml":            "id: drop\ntitle: Удаление\n",
		"changes/drop/vision.md":              "Убрать старый компонент.\n",
		"changes/drop/delta/application.yaml": "removed:\n  - {id: app.old, reason: Не нужен}\n",
		"changes/drop/gap.yaml":               "gaps:\n  - {id: gap.drop, title: Удаление, kind: remove, description: x, elements: [app.old]}\n",
	})
	_, ds, err := LoadChange(fsys, "drop")
	if err != nil {
		t.Fatal(err)
	}
	d := find(t, ds, "delta.removed-in-use", "changes/drop/delta/application.yaml", "app.old")
	if !strings.Contains(d.Message, "model/risk.yaml /owner") {
		t.Errorf("сообщение не называет ссылку из шапки: %s", d.Message)
	}
}

// Записи по пути через массив: целевая модель сохраняет остальные группы.
func TestChangeItemsThroughArray(t *testing.T) {
	fsys := riskFS(map[string]string{
		"changes/add/change.yaml":     "id: add\ntitle: Риск\n",
		"changes/add/vision.md":       "Добавить риск.\n",
		"changes/add/delta/risk.yaml": "added:\n  - {id: risk.b, title: B}\n",
		"changes/add/gap.yaml":        "gaps:\n  - {id: gap.risk, title: Риск, kind: add, description: x, elements: [risk.b]}\n",
	})
	ch, ds, err := LoadChange(fsys, "add")
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	groups, _ := valueAt(ch.Target.docs["risk"].value, []string{"groups"})
	if list, _ := groups.([]any); len(list) != 2 {
		t.Fatalf("группы целевой модели: %#v", groups)
	}
	if recs := ch.Target.docs["risk"].records; len(recs) != 2 || recs[1].ID != "risk.b" {
		t.Errorf("записи: %+v", recs)
	}
}

// rivet:ref у обычной задачи игнорируется проверкой процесса и не мешает
// удалению элемента.
func TestChangeIgnoredBPMNRefDoesNotBlockRemoval(t *testing.T) {
	fsys := minimalFS(map[string]string{
		"model/business.yaml":    "elements:\n  - {id: biz.p, type: process, name: П}\n",
		"model/application.yaml": "elements:\n  - {id: app.old, type: component, name: Старый}\n",
		"processes/biz.p.bpmn": bpmnDoc(`<bpmn:startEvent id="s"/><bpmn:userTask id="t" rivet:ref="app.old"/><bpmn:endEvent id="e"/>
    <bpmn:sequenceFlow id="f1" sourceRef="s" targetRef="t"/><bpmn:sequenceFlow id="f2" sourceRef="t" targetRef="e"/>`),
		"changes/drop/change.yaml":            "id: drop\ntitle: Удаление\n",
		"changes/drop/vision.md":              "Убрать.\n",
		"changes/drop/delta/application.yaml": "removed:\n  - {id: app.old, reason: Не нужен}\n",
		"changes/drop/gap.yaml":               "gaps:\n  - {id: gap.drop, title: Удаление, kind: remove, description: x, elements: [app.old]}\n",
	})
	_, ds, err := LoadChange(fsys, "drop")
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	find(t, ds, "bpmn.ref-ignored", "processes/biz.p.bpmn", "t")
}

// Путь к записям через массив, которого нет в документе: дельта не
// применяется, проверка сообщает doc.items.
func TestChangeItemsPathMissing(t *testing.T) {
	fsys := riskFS(map[string]string{
		"model/risk.yaml":             "owner: app.new\ngroups:\n  - name: Единственная\n    risks: []\n",
		"changes/add/change.yaml":     "id: add\ntitle: Риск\n",
		"changes/add/vision.md":       "Добавить риск.\n",
		"changes/add/delta/risk.yaml": "added:\n  - {id: risk.b, title: B}\n",
		"changes/add/gap.yaml":        "gaps:\n  - {id: gap.risk, title: Риск, kind: add, description: x, elements: [risk.b]}\n",
	})
	_, ds, err := LoadChange(fsys, "add")
	if err != nil {
		t.Fatal(err)
	}
	find(t, ds, "doc.items", "model/risk.yaml", "/groups/1/risks")
}
