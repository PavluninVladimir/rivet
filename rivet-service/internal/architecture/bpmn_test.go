package architecture

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func jsonNum(s string) json.Number { return json.Number(s) }

// bpmnDoc оборачивает тело процесса в definitions.
func bpmnDoc(body string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<bpmn:definitions xmlns:bpmn="http://www.omg.org/spec/BPMN/20100524/MODEL" xmlns:rivet="urn:rivet:architecture:1" id="d" targetNamespace="urn:test">
  <bpmn:process id="p">` + body + `
  </bpmn:process>
</bpmn:definitions>
`
}

// processModel — модель с ролями, компонентом и процессом biz.order.
const processModel = `elements:
  - {id: biz.buyer, type: role, name: Покупатель}
  - {id: biz.clerk, type: role, name: Кладовщик}
  - {id: biz.order, type: process, name: Оформление заказа}
`

func processFS(body string) map[string]string {
	return map[string]string{
		"model/business.yaml":      processModel,
		"model/application.yaml":   "elements:\n  - {id: app.orders, type: component, name: Заказы}\n",
		"processes/biz.order.bpmn": bpmnDoc(body),
	}
}

const orderProcessFile = "processes/biz.order.bpmn"

// Сценарий «Корректный процесс».
const correctProcess = `
    <bpmn:laneSet id="ls">
      <bpmn:lane id="lane-buyer" name="Покупатель" rivet:ref="biz.buyer">
        <bpmn:flowNodeRef>start</bpmn:flowNodeRef>
        <bpmn:flowNodeRef>pick</bpmn:flowNodeRef>
      </bpmn:lane>
      <bpmn:lane id="lane-clerk" name="Кладовщик" rivet:ref="biz.clerk">
        <bpmn:flowNodeRef>stock</bpmn:flowNodeRef>
        <bpmn:flowNodeRef>gw</bpmn:flowNodeRef>
        <bpmn:flowNodeRef>end</bpmn:flowNodeRef>
      </bpmn:lane>
    </bpmn:laneSet>
    <bpmn:startEvent id="start" name="Начало"/>
    <bpmn:userTask id="pick" name="Выбрать товар"/>
    <bpmn:serviceTask id="stock" name="Проверить остаток" rivet:ref="app.orders"/>
    <bpmn:exclusiveGateway id="gw" name="Есть на складе?"/>
    <bpmn:endEvent id="end" name="Готово"/>
    <bpmn:sequenceFlow id="f1" sourceRef="start" targetRef="pick"/>
    <bpmn:sequenceFlow id="f2" sourceRef="pick" targetRef="stock"/>
    <bpmn:sequenceFlow id="f3" sourceRef="stock" targetRef="gw"/>
    <bpmn:sequenceFlow id="f4" name="да" sourceRef="gw" targetRef="end"/>
    <bpmn:sequenceFlow id="f5" name="нет" sourceRef="gw" targetRef="pick"/>`

func TestProcessCorrect(t *testing.T) {
	ds := mustValidate(t, minimalFS(processFS(correctProcess)))
	if len(ds) != 0 {
		t.Fatalf("дефекты:\n%s", dump(ds))
	}
}

// Сценарий «Тупиковая ветка».
func TestProcessDeadEnd(t *testing.T) {
	body := strings.Replace(correctProcess, `<bpmn:sequenceFlow id="f5" name="нет" sourceRef="gw" targetRef="pick"/>`,
		`<bpmn:sequenceFlow id="f5" name="нет" sourceRef="gw" targetRef="lost"/>
    <bpmn:task id="lost" name="Ждать поставку"/>`, 1)
	ds := mustValidate(t, minimalFS(processFS(body)))
	d := find(t, ds, "bpmn.dead-end", orderProcessFile, "lost")
	if !strings.Contains(d.Message, "biz.order") || !strings.Contains(d.Message, "Ждать поставку") {
		t.Errorf("сообщение без процесса или задачи: %s", d.Message)
	}
	if rs := rules(ds); len(rs) != 1 {
		t.Errorf("лишние дефекты:\n%s", dump(ds))
	}
}

// Сценарий «Дорожка ссылается на неизвестную роль».
func TestProcessLaneUnknownRole(t *testing.T) {
	body := strings.Replace(correctProcess, `rivet:ref="biz.clerk"`, `rivet:ref="biz.ghost"`, 1)
	ds := mustValidate(t, minimalFS(processFS(body)))
	d := find(t, ds, "bpmn.ref-unresolved", orderProcessFile, "lane-clerk")
	if !strings.Contains(d.Message, "biz.ghost") {
		t.Errorf("сообщение: %s", d.Message)
	}
}

func TestProcessRefTypes(t *testing.T) {
	body := strings.Replace(correctProcess, `rivet:ref="biz.clerk"`, `rivet:ref="app.orders"`, 1)
	body = strings.Replace(body, `rivet:ref="app.orders"/>`, `rivet:ref="biz.buyer"/>`, 1)
	ds := mustValidate(t, minimalFS(processFS(body)))
	find(t, ds, "bpmn.ref-type", orderProcessFile, "lane-clerk")
	find(t, ds, "bpmn.ref-type", orderProcessFile, "stock")
}

// Сценарий «Элемент вне подмножества»: подпроцесс и граничное событие дают
// предупреждения, а граничное событие достижимо через свою активность.
func TestProcessUnsupportedElements(t *testing.T) {
	body := `
    <bpmn:startEvent id="start"/>
    <bpmn:subProcess id="sub" name="Сборка"><bpmn:startEvent id="inner"/></bpmn:subProcess>
    <bpmn:boundaryEvent id="timeout" attachedToRef="sub"/>
    <bpmn:textAnnotation id="note"><bpmn:text>заметка</bpmn:text></bpmn:textAnnotation>
    <bpmn:endEvent id="end"/>
    <bpmn:endEvent id="late"/>
    <bpmn:sequenceFlow id="f1" sourceRef="start" targetRef="sub"/>
    <bpmn:sequenceFlow id="f2" sourceRef="sub" targetRef="end"/>
    <bpmn:sequenceFlow id="f3" sourceRef="timeout" targetRef="late"/>`
	ds := mustValidate(t, minimalFS(processFS(body)))
	if ds.HasErrors() {
		t.Fatalf("ошибок быть не должно:\n%s", dump(ds))
	}
	for _, loc := range []string{"sub", "timeout", "note"} {
		if d := find(t, ds, "bpmn.unsupported", orderProcessFile, loc); d.Severity != SeverityWarning {
			t.Errorf("%s: важность %s", loc, d.Severity)
		}
	}
}

// Сценарий «Процесс без файла».
func TestProcessFileMissing(t *testing.T) {
	files := processFS(correctProcess)
	delete(files, "processes/biz.order.bpmn")
	ds := mustValidate(t, minimalFS(files))
	d := find(t, ds, "process.file-missing", "model/business.yaml", "biz.order")
	if !strings.Contains(d.Message, "processes/biz.order.bpmn") {
		t.Errorf("сообщение без ожидаемого пути: %s", d.Message)
	}
}

func TestProcessStructureRules(t *testing.T) {
	cases := []struct {
		name, body, rule, loc string
	}{
		{"нет начала", `<bpmn:task id="a"/><bpmn:endEvent id="e"/><bpmn:sequenceFlow id="f" sourceRef="a" targetRef="e"/>`,
			"bpmn.no-start", ""},
		{"нет конца", `<bpmn:startEvent id="s"/><bpmn:task id="a"/><bpmn:sequenceFlow id="f" sourceRef="s" targetRef="a"/>`,
			"bpmn.no-end", ""},
		{"поток в никуда", `<bpmn:startEvent id="s"/><bpmn:endEvent id="e"/><bpmn:sequenceFlow id="f" sourceRef="s" targetRef="x"/>`,
			"bpmn.flow-ref", "f"},
		{"недостижимый узел", `<bpmn:startEvent id="s"/><bpmn:task id="orphan"/><bpmn:endEvent id="e"/>
			<bpmn:sequenceFlow id="f1" sourceRef="s" targetRef="e"/><bpmn:sequenceFlow id="f2" sourceRef="orphan" targetRef="e"/>`,
			"bpmn.unreachable", "orphan"},
		{"шлюз без ветвления", `<bpmn:startEvent id="s"/><bpmn:exclusiveGateway id="g"/><bpmn:endEvent id="e"/>
			<bpmn:sequenceFlow id="f1" sourceRef="s" targetRef="g"/><bpmn:sequenceFlow id="f2" sourceRef="g" targetRef="e"/>`,
			"bpmn.gateway-split", "g"},
		{"дорожка на чужой узел", `<bpmn:laneSet id="ls"><bpmn:lane id="l"><bpmn:flowNodeRef>ghost</bpmn:flowNodeRef></bpmn:lane></bpmn:laneSet>
			<bpmn:startEvent id="s"/><bpmn:endEvent id="e"/><bpmn:sequenceFlow id="f" sourceRef="s" targetRef="e"/>`,
			"bpmn.lane-ref", "l"},
		{"повтор id", `<bpmn:startEvent id="s"/><bpmn:task id="s"/><bpmn:endEvent id="e"/><bpmn:sequenceFlow id="f" sourceRef="s" targetRef="e"/>`,
			"bpmn.duplicate-id", "s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ds := mustValidate(t, minimalFS(processFS(c.body)))
			find(t, ds, c.rule, orderProcessFile, c.loc)
		})
	}
}

func TestProcessParseErrors(t *testing.T) {
	cases := map[string]struct{ src, rule string }{
		"битый XML":      {"<bpmn:definitions", "bpmn.parse"},
		"не BPMN":        {`<definitions xmlns="urn:other"><process id="p"/></definitions>`, "bpmn.parse"},
		"нет процесса":   {`<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"/>`, "bpmn.no-process"},
		"пустой процесс": {bpmnDoc(""), "bpmn.no-start"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			files := processFS("")
			files["processes/biz.order.bpmn"] = c.src
			ds := mustValidate(t, minimalFS(files))
			find(t, ds, c.rule, orderProcessFile, "")
		})
	}
}

// Пространство имён BPMN по умолчанию, без префикса bpmn:.
func TestProcessDefaultNamespace(t *testing.T) {
	files := processFS("")
	files["processes/biz.order.bpmn"] = `<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" xmlns:rivet="urn:rivet:architecture:1">
  <process id="p">
    <startEvent id="s"/><serviceTask id="t" rivet:ref="app.orders"/><endEvent id="e"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="t"/><sequenceFlow id="f2" sourceRef="t" targetRef="e"/>
  </process>
</definitions>`
	ds := mustValidate(t, minimalFS(files))
	if len(ds) != 0 {
		t.Fatalf("дефекты:\n%s", dump(ds))
	}
}

func camundaFS(t *testing.T, edit func(string) string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("testdata/bpmn/camunda-checkout.bpmn")
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"model/business.yaml":         "elements:\n  - {id: biz.buyer, type: role, name: Покупатель}\n  - {id: biz.checkout, type: process, name: Оформление заказа}\n",
		"model/application.yaml":      "elements:\n  - {id: app.order-api, type: application-service, name: API заказов}\n",
		"processes/biz.checkout.bpmn": edit(string(raw)),
	}
}

// Файл в том виде, в каком его сохраняет Camunda Modeler: пул, DI,
// расширения camunda, incoming/outgoing и условия на потоках.
func TestCamundaModelerFile(t *testing.T) {
	m, ds, err := Load(minimalFS(camundaFS(t, func(s string) string { return s })))
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Fatalf("дефекты:\n%s", dump(ds))
	}
	p := m.Process("biz.checkout")
	if p.Name != "Оформление заказа" || len(p.Nodes) != 6 || len(p.Flows) != 5 || len(p.Lanes) != 2 {
		t.Fatalf("разбор: имя %q, узлов %d, потоков %d, дорожек %d", p.Name, len(p.Nodes), len(p.Flows), len(p.Lanes))
	}
	if n := p.node("Activity_order"); n == nil || n.Ref != "app.order-api" || n.Kind != "serviceTask" {
		t.Errorf("сервисная задача: %+v", n)
	}
}

func TestCamundaModelerBroken(t *testing.T) {
	files := camundaFS(t, func(s string) string {
		// Ветку «нет» удалили на холсте, а конечное событие осталось.
		return strings.Replace(s, `<bpmn:sequenceFlow id="Flow_no" name="нет" sourceRef="Gateway_paid" targetRef="Event_failed" />`, "", 1)
	})
	ds := mustValidate(t, minimalFS(files))
	file := "processes/biz.checkout.bpmn"
	find(t, ds, "bpmn.unreachable", file, "Event_failed")
	find(t, ds, "bpmn.gateway-split", file, "Gateway_paid")
}
