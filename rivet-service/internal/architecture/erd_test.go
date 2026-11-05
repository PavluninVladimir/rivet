package architecture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// crmData — слой данных и хранилище для тестов ERD: контакт ссылается на
// компанию, сделка на пользователя.
const crmData = `elements:
  - id: data.contact
    type: data-object
    name: Контакт
    attributes:
      - {name: id, type: uuid, pk: true}
      - {name: company_id, type: uuid, fk: {to: data.company, attribute: id}}
      - {name: email, type: string, unique: true}
  - id: data.company
    type: data-object
    name: Компания
    attributes:
      - {name: id, type: uuid, pk: true}
      - {name: inn, type: string}
  - id: data.note
    type: data-object
    name: Заметка
`

const crmTech = `elements:
  - id: tech.postgres
    type: system-software
    name: PostgreSQL
    relations:
      - {type: access, to: data.contact, mode: readwrite}
      - {type: access, to: data.company, mode: readwrite}
`

// Сценарий «Сущности с ключами».
func TestERDKeysAndDerivedRelation(t *testing.T) {
	fsys := minimalFS(map[string]string{"model/data.yaml": crmData, "model/technology.yaml": crmTech})
	m, ds, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	erd := m.BuildERD(m.C4())
	var contact ERDEntity
	for _, e := range erd.Entities {
		if e.ID == "data.contact" {
			contact = e
		}
	}
	want := []ERDRelation{{To: "data.company", Cardinality: CardManyToOne, Label: "company_id", Derived: true}}
	if !reflect.DeepEqual(contact.Relations, want) {
		t.Errorf("связи контакта: %+v", contact.Relations)
	}
	if contact.Attributes[0].Name != "id" || !reflect.DeepEqual(contact.Stores, []string{"tech.postgres"}) {
		t.Errorf("сущность %+v", contact)
	}
	if erd.All == nil || len(erd.Stores) != 1 || erd.Stores[0].StoreID != "tech.postgres" {
		t.Fatalf("ERD %+v", erd)
	}
}

// Правила erd.* на каждый сценарий требования «Модель данных сущностей».
func TestERDRules(t *testing.T) {
	cases := []struct {
		name, data, app, rule, loc string
	}{
		{"нет атрибута fk", `elements:
  - {id: data.deal, type: data-object, name: Сделка, attributes: [{name: id, type: uuid, pk: true}, {name: owner_id, type: uuid, fk: {to: data.user, attribute: uuid}}]}
  - {id: data.user, type: data-object, name: Пользователь, attributes: [{name: id, type: uuid, pk: true}]}
`, "", "erd.fk-attribute", "data.deal"},
		{"нет объекта fk", `elements:
  - {id: data.deal, type: data-object, name: Сделка, attributes: [{name: id, type: uuid, pk: true}, {name: x, type: uuid, fk: {to: data.nope, attribute: id}}]}
`, "", "erd.fk-entity", "data.deal"},
		{"нет pk", `elements:
  - {id: data.deal, type: data-object, name: Сделка, attributes: [{name: title, type: string}]}
`, "", "erd.no-pk", "data.deal"},
		{"pk nullable", `elements:
  - {id: data.deal, type: data-object, name: Сделка, attributes: [{name: id, type: uuid, pk: true, nullable: true}]}
`, "", "erd.pk-nullable", "data.deal"},
		{"повтор атрибута", `elements:
  - {id: data.deal, type: data-object, name: Сделка, attributes: [{name: id, type: uuid, pk: true}, {name: id, type: int}]}
`, "", "erd.attribute-duplicate", "data.deal"},
		{"атрибуты не у объекта данных", "", `elements:
  - {id: app.api, type: component, name: API, attributes: [{name: id, type: uuid, pk: true}]}
`, "erd.attributes-type", "app.api"},
		{"кардинальность не у association", `elements:
  - {id: data.deal, type: data-object, name: Сделка}
`, `elements:
  - {id: app.api, type: component, name: API, relations: [{type: access, to: data.deal, cardinality: one-to-many}]}
`, "erd.cardinality-type", "app.api"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{}
			if c.data != "" {
				files["model/data.yaml"] = c.data
			}
			if c.app != "" {
				files["model/application.yaml"] = c.app
			}
			ds := mustValidate(t, minimalFS(files))
			file := "model/data.yaml"
			if c.app != "" && strings.HasPrefix(c.loc, "app.") {
				file = "model/application.yaml"
			}
			find(t, ds, c.rule, file, c.loc)
		})
	}
}

// Сценарии «ERD базы данных» и «Объекты без хранилища».
func TestERDViews(t *testing.T) {
	gen := mustViews(t, minimalFS(map[string]string{"model/data.yaml": crmData, "model/technology.yaml": crmTech}))
	store := gen["views/erd/tech.postgres.md"]
	mustContain(t, "ERD хранилища", store, "erDiagram\n", `n_data_contact["Контакт"] {`, "uuid company_id FK",
		"string email UK", `n_data_contact }o--|| n_data_company : "company_id"`)
	mustNotContain(t, "ERD хранилища", store, "Заметка")
	all := gen["views/erd/all.md"]
	mustContain(t, "общая ERD", all, `n_data_note["Заметка"] {`, "[PostgreSQL](tech.postgres.md)")
	mustContain(t, "HLD", gen["views/hld.md"], "ERD: [общая схема](erd/all.md), по хранилищам: [PostgreSQL](erd/tech.postgres.md)", "erDiagram")
	mustContain(t, "README", gen["views/README.md"], "## Данные (ERD)")
}

// Сценарий «Стабильность»: перестановка атрибутов и объектов.
func TestERDStableUnderPermutation(t *testing.T) {
	base := mustViews(t, minimalFS(map[string]string{"model/data.yaml": crmData, "model/technology.yaml": crmTech}))
	permuted := `elements:
  - id: data.note
    type: data-object
    name: Заметка
  - id: data.company
    type: data-object
    name: Компания
    attributes:
      - {name: inn, type: string}
      - {name: id, type: uuid, pk: true}
  - id: data.contact
    type: data-object
    name: Контакт
    attributes:
      - {name: email, type: string, unique: true}
      - {name: company_id, type: uuid, fk: {to: data.company, attribute: id}}
      - {name: id, type: uuid, pk: true}
`
	gen := mustViews(t, minimalFS(map[string]string{"model/data.yaml": permuted, "model/technology.yaml": crmTech}))
	for _, p := range []string{"views/erd/all.md", "views/erd/tech.postgres.md"} {
		if gen[p] != base[p] {
			t.Errorf("%s меняется от перестановки:\n%s\n---\n%s", p, base[p], gen[p])
		}
	}
}

// Явная связь перекрывает выведенную, межхранилищная связь подписана.
func TestERDExplicitAndForeign(t *testing.T) {
	data := crmData + `  - id: data.deal
    type: data-object
    name: Сделка
    attributes:
      - {name: id, type: uuid, pk: true}
      - {name: contact_id, type: uuid, fk: {to: data.contact, attribute: id}}
    relations:
      - {type: association, to: data.contact, cardinality: many-to-one, label: клиент}
`
	tech := crmTech + `  - id: tech.redis
    type: system-software
    name: Redis
    props: {c4: database}
    relations:
      - {type: access, to: data.deal, mode: readwrite}
`
	m, ds, err := Load(minimalFS(map[string]string{"model/data.yaml": data, "model/technology.yaml": tech}))
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	c := m.C4()
	for _, e := range buildERD(m, c) {
		if e.ID == "data.deal" && (len(e.Relations) != 1 || e.Relations[0].Derived || e.Relations[0].Label != "клиент") {
			t.Errorf("явная связь должна перекрыть выведенную: %+v", e.Relations)
		}
	}
	gen := mustViews(t, minimalFS(map[string]string{"model/data.yaml": data, "model/technology.yaml": tech}))
	mustContain(t, "ERD redis", gen["views/erd/tech.redis.md"], `n_data_contact["Контакт · PostgreSQL"]`, `n_data_deal }o--|| n_data_contact : "клиент"`)
}

const scenarioModel = `elements:
  - {id: app.web, type: component, name: Консоль, relations: [{type: flow, to: app.api}, {type: serving, to: biz.manager}]}
  - {id: app.api, type: component, name: API, relations: [{type: access, to: data.contact, mode: write}]}
`

func scenarioFS(scenarios map[string]string, index ...string) fstest.MapFS {
	manifest := "format: rivet.architecture/v1\nsystem: CRM\n"
	if len(index) > 0 {
		manifest += "scenarios: [" + strings.Join(index, ", ") + "]\n"
	}
	m := fstest.MapFS{
		ManifestFile:              {Data: []byte(manifest)},
		"model/application.yaml":  {Data: []byte(scenarioModel)},
		"model/business.yaml":     {Data: []byte("elements:\n  - {id: biz.manager, type: actor, name: Менеджер}\n  - {id: biz.sale, type: process, name: Продажа}\n")},
		"model/data.yaml":         {Data: []byte("elements:\n  - {id: data.contact, type: data-object, name: Контакт}\n")},
		"model/technology.yaml":   {Data: []byte("elements:\n  - {id: tech.db, type: system-software, name: База, relations: [{type: access, to: data.contact}]}\n")},
		"processes/biz.sale.bpmn": {Data: []byte(minimalBPMN("biz.sale"))},
	}
	for id, text := range scenarios {
		m["scenarios/"+id+".yaml"] = &fstest.MapFile{Data: []byte(text)}
	}
	return m
}

// minimalBPMN — корректный процесс из одной задачи.
func minimalBPMN(id string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL" id="d">
  <process id="` + id + `">
    <startEvent id="s"/>
    <task id="t" name="Работа"/>
    <endEvent id="e"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="t"/>
    <sequenceFlow id="f2" sourceRef="t" targetRef="e"/>
  </process>
</definitions>
`
}

const loginScenario = `id: sc.login
title: Вход
process: biz.sale
steps:
  - {from: biz.manager, to: app.web, message: вводит пароль, kind: sync}
  - {from: app.web, to: app.api, message: POST /login, kind: sync}
  - alt:
      - when: пароль неверный
        steps:
          - {from: app.api, to: app.web, message: "401", kind: return}
      - when: иначе
        steps:
          - {from: app.api, to: tech.db, message: запись сессии, kind: async}
          - {from: app.api, to: app.web, message: "200", kind: return}
  - loop:
      when: каждые 5 минут
      steps:
        - {from: app.web, to: app.web, message: обновляет токен, kind: self}
  - opt:
      when: включены уведомления
      steps:
        - {from: app.web, to: biz.manager, message: показывает уведомление, kind: return}
`

// Сценарии «Сценарий входа» и «Сценарий на GitHub».
func TestScenarioView(t *testing.T) {
	fsys := scenarioFS(map[string]string{"sc.login": loginScenario}, "sc.login")
	noErrors(t, mustValidate(t, fsys))
	if ds := mustValidate(t, fsys); len(ds) != 0 {
		t.Fatalf("ожидали чистую проверку:\n%s", dump(ds))
	}
	gen := mustViews(t, fsys)
	v := gen["views/scenarios/sc.login.md"]
	mustContain(t, "сценарий", v, "sequenceDiagram\n", "actor n_biz_manager as Менеджер", "participant n_app_web as Консоль",
		"n_biz_manager->>n_app_web: вводит пароль", "  alt пароль неверный\n", "  else иначе\n",
		"n_app_api-)n_tech_db: запись сессии", "n_app_api-->>n_app_web: 401", "  loop каждые 5 минут\n",
		"n_app_web->>n_app_web: обновляет токен", "  opt включены уведомления\n", "  end\n",
		"Процесс: [Продажа](../processes/biz.sale.md)")
	mustContain(t, "HLD", gen["views/hld.md"], "## Ключевые сценарии", "[scenarios/sc.login.md](scenarios/sc.login.md)")
}

// Правила scenario.* по сценариям требования «Сценарии взаимодействия».
func TestScenarioRules(t *testing.T) {
	file := "scenarios/sc.x.yaml"
	cases := []struct {
		name, text, rule, loc string
		warn                  bool
	}{
		{"неизвестный участник", "id: sc.x\ntitle: X\nsteps:\n  - {from: app.web, to: app.billing, message: m, kind: sync}\n",
			"scenario.unknown-participant", "/steps/0/to", false},
		{"тип участника", "id: sc.x\ntitle: X\nsteps:\n  - {from: app.web, to: data.contact, message: m, kind: sync}\n",
			"scenario.participant-type", "/steps/0/to", false},
		{"нет процесса", "id: sc.x\ntitle: X\nprocess: biz.nope\nsteps:\n  - {from: app.web, to: app.api, message: m, kind: sync}\n",
			"scenario.process", "/process", false},
		{"пустой сценарий", "id: sc.x\ntitle: X\nsteps: []\n", "scenario.empty", "/steps", false},
		{"пустой блок", "id: sc.x\ntitle: X\nsteps:\n  - opt: {when: y, steps: []}\n", "scenario.empty", "/steps/0/opt", false},
		{"self", "id: sc.x\ntitle: X\nsteps:\n  - {from: app.web, to: app.api, message: m, kind: self}\n",
			"scenario.self", "/steps/0", false},
		{"глубина", "id: sc.x\ntitle: X\nsteps:\n  - opt:\n      steps:\n        - opt:\n            steps:\n              - opt:\n                  steps:\n                    - opt:\n                        steps:\n                          - opt:\n                              steps:\n                                - {from: app.web, to: app.api, message: m, kind: sync}\n",
			"scenario.depth", "/steps/0/opt/steps/0/opt/steps/0/opt/steps/0/opt/steps/0", false},
		{"id не совпадает", "id: sc.y\ntitle: X\nsteps:\n  - {from: app.web, to: app.api, message: m, kind: sync}\n",
			"scenario.id", "/id", false},
		{"нет связи в модели", "id: sc.x\ntitle: X\nsteps:\n  - {from: app.web, to: tech.db, message: m, kind: sync}\n",
			"scenario.no-relation", "/steps/0", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ds := mustValidate(t, scenarioFS(map[string]string{"sc.x": c.text}, "sc.x"))
			d := find(t, ds, c.rule, file, c.loc)
			if (d.Severity == SeverityWarning) != c.warn {
				t.Errorf("важность %s", d.Severity)
			}
			if c.warn {
				noErrors(t, ds)
			}
		})
	}
	t.Run("нет файла", func(t *testing.T) {
		find(t, mustValidate(t, scenarioFS(nil, "sc.x")), "scenario.missing", ManifestFile, "/scenarios/0")
	})
	t.Run("файл вне индекса", func(t *testing.T) {
		ds := mustValidate(t, scenarioFS(map[string]string{"sc.x": loginScenario}))
		if d := find(t, ds, "scenario.unreferenced", file, ""); d.Severity != SeverityWarning {
			t.Errorf("важность %s", d.Severity)
		}
	})
	t.Run("связь через C4-предка", func(t *testing.T) {
		model := scenarioModel + "  - {id: app.api-auth, type: component, name: Авторизация}\n"
		fsys := scenarioFS(map[string]string{"sc.x": "id: sc.x\ntitle: X\nsteps:\n  - {from: app.web, to: app.api-auth, message: m, kind: sync}\n"}, "sc.x")
		fsys["model/application.yaml"] = &fstest.MapFile{Data: []byte(strings.Replace(model,
			"{id: app.api, type: component, name: API, relations: [", "{id: app.api, type: component, name: API, relations: [{type: composition, to: app.api-auth}, ", 1))}
		if ds := mustValidate(t, fsys); len(ds) != 0 {
			t.Fatalf("шаг к компоненту контейнера со связью не должен давать предупреждений:\n%s", dump(ds))
		}
	})
}

func adrDoc(n int, status string, extra string) string {
	return fmt.Sprintf("---\nnumber: %d\ntitle: Решение %d\nstatus: %s\ndate: 2026-11-04\n%s---\n\n## Контекст\n\nк\n\n## Решение\n\nр\n\n## Последствия\n\nп\n", n, n, status, extra)
}

func adrFS(files map[string]string, index ...string) fstest.MapFS {
	manifest := "format: rivet.architecture/v1\nsystem: CRM\n"
	if len(index) > 0 {
		manifest += "decisions: [" + strings.Join(index, ", ") + "]\n"
	}
	m := fstest.MapFS{
		ManifestFile:             {Data: []byte(manifest)},
		"model/application.yaml": {Data: []byte("elements:\n  - {id: app.api, type: component, name: API}\n")},
	}
	for name, text := range files {
		m["adr/"+name+".md"] = &fstest.MapFile{Data: []byte(text)}
	}
	return m
}

// Сценарии «Корректный журнал», «Дыра в нумерации», «Невзаимная замена» и
// остальные правила adr.*.
func TestADRRules(t *testing.T) {
	ok := adrFS(map[string]string{
		"0001-a": adrDoc(1, "superseded", "superseded_by: 3\nelements: [app.api]\n"),
		"0002-b": adrDoc(2, "accepted", ""),
		"0003-c": adrDoc(3, "accepted", "supersedes: [1]\n"),
	}, "0001-a", "0002-b", "0003-c")
	if ds := mustValidate(t, ok); len(ds) != 0 {
		t.Fatalf("корректный журнал:\n%s", dump(ds))
	}
	cases := []struct {
		name  string
		files map[string]string
		index []string
		rule  string
		file  string
		loc   string
	}{
		{"дыра", map[string]string{"0001-a": adrDoc(1, "accepted", ""), "0003-c": adrDoc(3, "accepted", "")},
			[]string{"0001-a", "0003-c"}, "adr.numbering", ManifestFile, "/decisions"},
		{"невзаимная замена", map[string]string{"0001-a": adrDoc(1, "accepted", ""), "0002-b": adrDoc(2, "accepted", "supersedes: [1]\n")},
			[]string{"0001-a", "0002-b"}, "adr.supersede-mutual", "adr/0002-b.md", "/supersedes/0"},
		{"superseded без ссылки", map[string]string{"0001-a": adrDoc(1, "superseded", "")},
			[]string{"0001-a"}, "adr.superseded-by", "adr/0001-a.md", "/superseded_by"},
		{"ссылка не у superseded", map[string]string{"0001-a": adrDoc(1, "accepted", "superseded_by: 2\n"), "0002-b": adrDoc(2, "accepted", "supersedes: [1]\n")},
			[]string{"0001-a", "0002-b"}, "adr.superseded-by", "adr/0001-a.md", "/superseded_by"},
		{"замена несуществующего", map[string]string{"0001-a": adrDoc(1, "accepted", "supersedes: [7]\n")},
			[]string{"0001-a"}, "adr.supersede-target", "adr/0001-a.md", "/supersedes/0"},
		{"статус", map[string]string{"0001-a": adrDoc(1, "approved", "")}, []string{"0001-a"}, "adr.status", "adr/0001-a.md", "/status"},
		{"дата", map[string]string{"0001-a": strings.Replace(adrDoc(1, "accepted", ""), "2026-11-04", "04.11.2026", 1)},
			[]string{"0001-a"}, "adr.date", "adr/0001-a.md", "/date"},
		{"раздел", map[string]string{"0001-a": strings.Replace(adrDoc(1, "accepted", ""), "## Последствия", "## Итог", 1)},
			[]string{"0001-a"}, "adr.section", "adr/0001-a.md", ""},
		{"элемент", map[string]string{"0001-a": adrDoc(1, "accepted", "elements: [app.nope]\n")},
			[]string{"0001-a"}, "adr.element", "adr/0001-a.md", "/elements/0"},
		{"номер в имени", map[string]string{"0001-a": adrDoc(2, "accepted", "")}, []string{"0001-a"}, "adr.number-mismatch", "adr/0001-a.md", "/number"},
		{"имя", map[string]string{}, []string{"1-a"}, "adr.name", ManifestFile, "/decisions/0"},
		{"нет файла", map[string]string{}, []string{"0001-a"}, "adr.missing", ManifestFile, "/decisions/0"},
		{"нет front matter", map[string]string{"0001-a": "## Контекст\n"}, []string{"0001-a"}, "adr.front-matter", "adr/0001-a.md", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			find(t, mustValidate(t, adrFS(c.files, c.index...)), c.rule, c.file, c.loc)
		})
	}
	t.Run("английские заголовки", func(t *testing.T) {
		en := strings.NewReplacer("## Контекст", "## Context", "## Решение", "## Decision", "## Последствия", "## Consequences").Replace(adrDoc(1, "accepted", ""))
		if ds := mustValidate(t, adrFS(map[string]string{"0001-a": en}, "0001-a")); len(ds) != 0 {
			t.Fatalf("английские заголовки:\n%s", dump(ds))
		}
	})
	t.Run("решение вне индекса", func(t *testing.T) {
		ds := mustValidate(t, adrFS(map[string]string{"0005-cache": adrDoc(5, "accepted", "")}))
		if d := find(t, ds, "adr.unreferenced", "adr/0005-cache.md", ""); d.Severity != SeverityWarning {
			t.Errorf("важность %s", d.Severity)
		}
	})
}

// Сценарий «Принятые решения в HLD» и реестр.
func TestADRRegistryAndHLD(t *testing.T) {
	fsys := adrFS(map[string]string{
		"0001-a": adrDoc(1, "accepted", ""),
		"0002-b": adrDoc(2, "accepted", ""),
		"0003-c": adrDoc(3, "rejected", ""),
	}, "0001-a", "0002-b", "0003-c")
	gen := mustViews(t, fsys)
	mustContain(t, "HLD", gen["views/hld.md"], "## Архитектурные решения", "[0001. Решение 1](../adr/0001-a.md)", "[0002. Решение 2](../adr/0002-b.md)")
	mustNotContain(t, "HLD", gen["views/hld.md"], "Решение 3")
	mustContain(t, "реестр", gen["views/adr.md"], "| 0003 | [Решение 3](../adr/0003-c.md) | отклонено |", "| 0001 |", "| 0002 |")
}

// changeFiles — изменение c4-эталона: решение 0004 заменяет 0001, целевой
// сценарий sc.archive покрыт разрывом.
func changeFiles(decision string, gapScenarios string) map[string]string {
	return map[string]string{
		"changes/to-s3/change.yaml": "id: to-s3\ntitle: Логи сборок в S3\nscenarios: [sc.archive]\ndecisions: [" + decision + "]\n",
		"changes/to-s3/vision.md":   "Логи сборок переезжают в S3.\n",
		"changes/to-s3/gap.yaml":    "gaps:\n  - id: gap.archive\n    title: Архивация логов\n    kind: add\n    description: Воркер пишет логи в S3\n    scenarios: [" + gapScenarios + "]\n",
		"changes/to-s3/scenarios/sc.archive.yaml": "id: sc.archive\ntitle: Архивация лога\nsteps:\n" +
			"  - {from: app.worker, to: tech.postgres, message: читает лог, kind: sync}\n",
		"changes/to-s3/adr/" + decision + ".md": "---\nnumber: 4\ntitle: Логи в S3\nstatus: accepted\ndate: 2026-11-04\nsupersedes: [1]\n---\n\n## Контекст\n\nЛоги растут.\n\n## Решение\n\nS3.\n\n## Последствия\n\nДешевле.\n",
	}
}

// Сценарии «Решение в изменении», «Номер уже занят», «Сценарий без разрыва».
func TestChangeScenariosAndDecisions(t *testing.T) {
	fsys := dirFS(t, c4Dir, changeFiles("0004-logs-s3", "sc.archive"))
	ch, ds, err := LoadChange(fsys, "to-s3")
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	var first *Decision
	for _, d := range ch.Target.decisions {
		if d.Number == 1 {
			first = d
		}
	}
	if first == nil || first.Status != ADRSuperseded || first.SupersededBy == nil || *first.SupersededBy != 4 {
		t.Errorf("в целевом журнале 0001 должно быть заменено 0004: %+v", first)
	}
	if !slices.Contains(ch.Target.Manifest.Decisions, "0004-logs-s3") || !slices.Contains(ch.Target.Manifest.Scenarios, "sc.archive") {
		t.Errorf("индекс целевой модели %v %v", ch.Target.Manifest.Decisions, ch.Target.Manifest.Scenarios)
	}
	gen := mustViews(t, fsys)
	mustContain(t, "изменение", gen["views/changes/to-s3.md"], "## Целевые сценарии", "## Новые решения", "| 0004 | Логи в S3 | принято | 0001 |")
	mustContain(t, "целевые сценарии", gen["views/changes/to-s3-scenarios.md"], "sequenceDiagram", "n_app_worker->>n_tech_postgres: читает лог")

	busy := changeFiles("0003-logs-s3", "sc.archive")
	busy["changes/to-s3/adr/0003-logs-s3.md"] = strings.Replace(busy["changes/to-s3/adr/0003-logs-s3.md"], "number: 4", "number: 3", 1)
	_, ds, err = LoadChange(dirFS(t, c4Dir, busy), "to-s3")
	if err != nil {
		t.Fatal(err)
	}
	find(t, ds, "change.adr-numbering", "changes/to-s3/adr/0003-logs-s3.md", "/number")

	_, ds, err = LoadChange(dirFS(t, c4Dir, changeFiles("0004-logs-s3", "")), "to-s3")
	if err != nil {
		t.Fatal(err)
	}
	find(t, ds, "change.scenario-uncovered", "changes/to-s3/scenarios/sc.archive.yaml", "")
}

// Применение: сценарий и решение переносятся, у 0001 правится front matter,
// индекс манифеста дополняется. Занятый номер — отказ без записи.
func TestApplyScenariosAndDecisions(t *testing.T) {
	dir := t.TempDir()
	copyDir(t, c4Dir, dir)
	writeMapFS(t, dir, fstest.MapFS(func() fstest.MapFS {
		m := fstest.MapFS{}
		for p, s := range changeFiles("0004-logs-s3", "sc.archive") {
			m[p] = &fstest.MapFile{Data: []byte(s)}
		}
		return m
	}()))
	before := readFile(t, filepath.Join(dir, "adr/0001-postgres-history.md"))
	res, err := Apply(dir, "to-s3", applyDate)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || res.Defects.HasErrors() {
		t.Fatalf("изменение не применено:\n%s", dump(res.Defects))
	}
	after := readFile(t, filepath.Join(dir, "adr/0001-postgres-history.md"))
	mustContain(t, "0001 после применения", after, "status: superseded\n", "superseded_by: 4\n", "elements: [tech.postgres, app.api, app.worker]\n")
	if body := before[strings.Index(before, "## Контекст"):]; !strings.HasSuffix(after, body) {
		t.Errorf("тело решения изменилось:\n%s", after)
	}
	manifest := readFile(t, filepath.Join(dir, ManifestFile))
	mustContain(t, "манифест", manifest, "  - sc.archive\n", "  - 0004-logs-s3\n", "  - 0003-github-webhooks\n")
	for _, p := range []string{"adr/0004-logs-s3.md", "scenarios/sc.archive.yaml", "views/scenarios/sc.archive.md"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("нет %s после применения", p)
		}
	}
	ds, err := CheckViews(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Errorf("описание после применения:\n%s", dump(ds))
	}

	// Номер занят в текущем журнале: применения нет, файлы не тронуты.
	dir2 := t.TempDir()
	copyDir(t, c4Dir, dir2)
	busy := changeFiles("0003-logs-s3", "sc.archive")
	busy["changes/to-s3/adr/0003-logs-s3.md"] = strings.Replace(busy["changes/to-s3/adr/0003-logs-s3.md"], "number: 4", "number: 3", 1)
	m := fstest.MapFS{}
	for p, s := range busy {
		m[p] = &fstest.MapFile{Data: []byte(s)}
	}
	writeMapFS(t, dir2, m)
	snap := snapshot(t, dir2)
	res, err = Apply(dir2, "to-s3", applyDate)
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied {
		t.Fatal("изменение с занятым номером не должно применяться")
	}
	if !reflect.DeepEqual(snapshot(t, dir2), snap) {
		t.Error("отказ применения изменил файлы")
	}
}

// Сценарий «Решения и сценарии после merge» в снимке без листинга.
func TestSnapshotDocs(t *testing.T) {
	var reads []string
	fsys := noListFS{fsys: dirFS(t, c4Dir, map[string]string{"adr/0009-stray.md": adrDoc(9, "accepted", "")}), reads: &reads}
	s, err := BuildSnapshot(fsys, SnapshotOptions{Dir: "docs/architecture"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != SnapshotOK {
		t.Fatalf("снимок %s:\n%s", s.Status, dump(s.Defects))
	}
	d := s.Data
	if d.ERD.All == nil || len(d.ERD.Stores) != 1 || d.ERD.Stores[0].StoreID != "tech.postgres" || len(d.ERD.Entities) != 3 {
		t.Errorf("ERD %+v", d.ERD)
	}
	if len(d.Scenarios) != 2 || d.Scenarios[0].ID != "sc.build-run" {
		t.Fatalf("сценарии %+v", d.Scenarios)
	}
	c := mustModel(t, c4Dir).C4()
	for _, p := range d.Scenarios[1].Participants {
		if p.Alias != c.MermaidAlias(p.ID) {
			t.Errorf("псевдоним %s: %s, на C4 %s", p.ID, p.Alias, c.MermaidAlias(p.ID))
		}
	}
	if len(d.Decisions) != 3 || d.Decisions[0].Path != "docs/architecture/adr/0001-postgres-history.md" ||
		strings.HasPrefix(d.Decisions[0].Body, "---") || !strings.HasPrefix(d.Decisions[0].Body, "## Контекст") {
		t.Errorf("решения %+v", d.Decisions[0])
	}
	if l := d.Links["app.api"]; !reflect.DeepEqual(l.Decisions, []int{1, 3}) || !reflect.DeepEqual(l.Scenarios, []string{"sc.build-run", "sc.build-trigger"}) {
		t.Errorf("связи app.api %+v", l)
	}
	for _, p := range reads {
		if strings.Contains(p, "0009-stray") {
			t.Error("снимок прочитал решение вне индекса")
		}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "JSON снимка", string(raw), `"superseded_by":3`, `"superseded_by":null`, `"fk":null`, `"derived":true`, `"process":null`)
}

// Сценарий «Слишком большой журнал»: лимит без чтения файлов.
func TestSnapshotLimit(t *testing.T) {
	var names []string
	for i := 1; i <= SnapshotMaxDecisions+1; i++ {
		names = append(names, fmt.Sprintf("%04d-d", i))
	}
	_, err := BuildSnapshot(adrFS(nil, names...), SnapshotOptions{})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("ожидали ErrLimit, получили %v", err)
	}
}

func TestChangeIDScenariosSuffix(t *testing.T) {
	if ValidChangeID("foo-scenarios") || !strings.Contains(ChangeIDProblem("foo-scenarios"), "-scenarios") {
		t.Error("id с суффиксом -scenarios должен отвергаться")
	}
}

// patchFrontMatter сохраняет тело и комментарии.
func TestPatchFrontMatter(t *testing.T) {
	raw := "---\nnumber: 1  # первый\ntitle: А\nstatus: accepted\ndate: 2026-11-04\n---\n\n## Контекст\n\nтекст --- с тире\n"
	out, err := patchFrontMatter([]byte(raw), map[string]string{"status": ADRSuperseded}, map[string]int{"superseded_by": 2})
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "front matter", string(out), "number: 1 # первый\n", "status: superseded\n", "superseded_by: 2\n", "\n## Контекст\n\nтекст --- с тире\n")
}

func mustModel(t *testing.T, dir string) *Model {
	t.Helper()
	m, ds, err := Load(os.DirFS(dir))
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, ds)
	return m
}
