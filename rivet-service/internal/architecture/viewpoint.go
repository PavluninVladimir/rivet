package architecture

import (
	"regexp"
	"slices"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Viewpoint определяет часть архитектуры: какой документ её описывает,
// как его проверять, какое задание получает агент и как его рисовать.
// Встроенные viewpoint'ы — слои модели, собственные проект задаёт файлом
// viewpoints/<id>.yaml. Остальной код работает с интерфейсом и не
// различает их.
type Viewpoint interface {
	ID() string
	Title() string
	// Prompt — задание агенту: что описывать в документе viewpoint'а.
	Prompt() string
	Builtin() bool
	// File — путь документа viewpoint'а в описании.
	File() string
	// items — путь к массиву записей документа. Записи адресуются по id,
	// по нему же их меняет дельта.
	items() []string
	// schema — JSON Schema документа, nil если схема не скомпилировалась.
	schema() *jsonschema.Schema
	// check — семантические правила документа в контексте всей модели.
	check(m *Model, d *Doc, r *reporter, opt checkOptions)
	// references — ссылки документа на элементы модели.
	references(d *Doc) []modelRef
	// recordTitle — подпись записи в таблицах и на диаграммах.
	recordTitle(rec *Record) string
	// render — Markdown-представление документа.
	render(m *Model, d *Doc) string
}

// modelRef — ссылка записи документа на элемент модели.
type modelRef struct {
	From, To string // id записи и id элемента
	Label    string // тип связи или имя поля
	File     string
	Location string
}

// layer — встроенный viewpoint слоя TOGAF.
type layer struct {
	id, title, prompt string
}

var builtinLayers = []*layer{
	{LayerBusiness, "Бизнес", "Опиши бизнес-слой: акторов (actor) и их роли (role), бизнес-сервисы " +
		"(business-service), процессы (process) и бизнес-объекты (business-object). Каждый процесс опиши " +
		"в BPMN 2.0 в файле processes/<id процесса>.bpmn: дорожки (lane) ссылаются на роли и акторов " +
		"атрибутом rivet:ref, сервисные задачи (serviceTask) на компоненты и сервисы приложений."},
	{LayerApplication, "Приложения", "Опиши слой приложений: компоненты (component), интерфейсы " +
		"(interface) и сервисы приложений (application-service). Покажи, какие процессы и роли они " +
		"обслуживают (serving, realization), как компоненты передают друг другу данные и управление " +
		"(flow, triggering) и к каким данным обращаются (access с mode)."},
	{LayerData, "Данные", "Опиши слой данных: объекты данных (data-object), какие бизнес-объекты они " +
		"реализуют (realization) и какие компоненты и системное ПО к ним обращаются."},
	{LayerTechnology, "Технологии", "Опиши слой технологий: узлы (node) и системное ПО " +
		"(system-software), на которых работают компоненты, и связи serving и assignment между ними."},
}

// BuiltinViewpoints возвращает встроенные viewpoint'ы в порядке слоёв.
func BuiltinViewpoints() []Viewpoint {
	out := make([]Viewpoint, len(builtinLayers))
	for i, l := range builtinLayers {
		out[i] = l
	}
	return out
}

func isBuiltin(id string) bool {
	return slices.ContainsFunc(builtinLayers, func(l *layer) bool { return l.id == id })
}

func (l *layer) ID() string                 { return l.id }
func (l *layer) Title() string              { return l.title }
func (l *layer) Prompt() string             { return l.prompt }
func (l *layer) Builtin() bool              { return true }
func (l *layer) File() string               { return ModelDir + "/" + l.id + ".yaml" }
func (l *layer) items() []string            { return []string{"elements"} }
func (l *layer) schema() *jsonschema.Schema { return layerSchema }

func (l *layer) check(m *Model, d *Doc, r *reporter, opt checkOptions) {
	for _, rec := range d.records {
		e := rec.Element
		if e.Type != "" {
			switch want := typeLayer(e.Type); want {
			case "":
				r.errorf("element.type-unknown", rec.File, e.ID, "неизвестный тип элемента %q", e.Type)
			case l.id:
			default:
				r.errorf("element.layer", rec.File, e.ID, "элемент типа %s должен лежать в %s/%s.yaml", e.Type, ModelDir, want)
			}
		}
		for _, rel := range e.Relations {
			m.checkRelation(rec, rel, r, opt)
		}
		if e.Type == TypeProcess {
			if p := m.processes[e.ID]; p == nil {
				r.errorf("process.file-missing", rec.File, e.ID, "нет файла процесса %s", processFile(e.ID))
			} else {
				p.check(m, r, opt)
			}
		}
	}
}

func (l *layer) references(d *Doc) []modelRef {
	var out []modelRef
	for _, rec := range d.records {
		for _, rel := range rec.Element.Relations {
			if rel.To != "" {
				out = append(out, modelRef{From: rec.ID, To: rel.To, Label: rel.Type, File: rec.File, Location: rec.ID})
			}
		}
	}
	return out
}

func (l *layer) recordTitle(rec *Record) string { return elementLabel(rec.Element) }

func (l *layer) render(m *Model, d *Doc) string { return renderLayer(m, l, d) }

func processFile(id string) string { return ProcessesDir + "/" + id + ".bpmn" }

// viewpointIDPattern — идентификатор собственного viewpoint'а: из него
// строятся имена файлов.
var viewpointIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// viewpointDef — файл viewpoints/<id>.yaml.
type viewpointDef struct {
	ID     string    `json:"id"`
	Title  string    `json:"title"`
	Prompt string    `json:"prompt"`
	Items  string    `json:"items"`
	Schema any       `json:"schema"`
	Render renderDef `json:"render"`
}

// renderDef — способ отрисовки собственного viewpoint'а.
type renderDef struct {
	Kind    string   `json:"kind"`
	Label   string   `json:"label,omitempty"`
	Edges   []string `json:"edges,omitempty"`
	Columns []string `json:"columns,omitempty"`
}

// customViewpoint — обёртка над собственным viewpoint'ом проекта.
type customViewpoint struct {
	def  viewpointDef
	ptr  []string
	sch  *jsonschema.Schema
	refs [][]refStep // пути полей x-rivet-ref в документе
}

func (c *customViewpoint) ID() string                 { return c.def.ID }
func (c *customViewpoint) Title() string              { return c.def.Title }
func (c *customViewpoint) Prompt() string             { return c.def.Prompt }
func (c *customViewpoint) Builtin() bool              { return false }
func (c *customViewpoint) File() string               { return ModelDir + "/" + c.def.ID + ".yaml" }
func (c *customViewpoint) items() []string            { return c.ptr }
func (c *customViewpoint) schema() *jsonschema.Schema { return c.sch }

// check разрешает ссылки на основную модель. Соответствие схеме
// проверяет общий проход по документам.
func (c *customViewpoint) check(m *Model, d *Doc, r *reporter, opt checkOptions) {
	for _, ref := range c.references(d) {
		if opt.removed[ref.To] || m.element(ref.To) != nil {
			continue
		}
		r.errorf("ref.unresolved", ref.File, ref.Location, "ссылка на несуществующий элемент модели %s", ref.To)
	}
}

// references собирает строки документа по путям x-rivet-ref. Ссылка вне
// записи (например, в шапке документа) получает пустой From.
func (c *customViewpoint) references(d *Doc) []modelRef {
	var out []modelRef
	seen := map[string]bool{}
	for _, pat := range c.refs {
		for _, hit := range collectRefs(d.value, pat) {
			// Одно поле может подходить под несколько путей схемы.
			if at := formatPointer(hit.path); seen[at] {
				continue
			} else {
				seen[at] = true
			}
			file, loc := d.place(hit.path)
			from := ""
			if !strings.HasPrefix(loc, "/") {
				from = strings.SplitN(loc, "/", 2)[0]
			}
			label := ""
			if n := len(c.ptr); len(hit.path) > n+1 {
				label = hit.path[n+1]
			}
			out = append(out, modelRef{From: from, To: hit.value, Label: label, File: file, Location: loc})
		}
	}
	return out
}

// recordTitle берёт поле подписи из render.label, иначе id.
func (c *customViewpoint) recordTitle(rec *Record) string {
	field := c.def.Render.Label
	if field == "" {
		field = "id"
	}
	if s := cellValue(rec.Value[field]); s != "" {
		return s
	}
	return rec.ID
}

func (c *customViewpoint) render(m *Model, d *Doc) string { return renderCustom(m, c, d) }

// refFields — поля записи, помеченные в схеме как ссылки: строка или
// массив строк прямо в записи.
func (c *customViewpoint) refFields() map[string]bool {
	out := map[string]bool{}
	n := len(c.ptr)
	for _, p := range c.refs {
		if len(p) < n+2 {
			continue
		}
		match := true
		for i, tok := range c.ptr {
			if !p[i].matchToken(tok) {
				match = false
				break
			}
		}
		if !match || p[n].kind != stepItems || p[n].n != 0 || p[n+1].kind != stepKey {
			continue
		}
		rest := p[n+2:]
		if len(rest) == 0 || (len(rest) == 1 && rest[0].kind == stepItems && rest[0].n == 0) {
			out[p[n+1].key] = true
		}
	}
	return out
}
