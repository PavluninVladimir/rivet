package architecture

import (
	"regexp"
	"slices"
	"strings"
)

// Format — поддерживаемая версия формата описания.
const Format = "rivet.architecture/v1"

// Пути внутри описания. Все файлы выводятся из манифеста и модели, поэтому
// описание читается без обхода каталога.
const (
	ManifestFile  = "architecture.yaml"
	ModelDir      = "model"
	ProcessesDir  = "processes"
	ViewpointsDir = "viewpoints"
	ViewsDir      = "views"
	ScenariosDir  = "scenarios"
	ADRDir        = "adr"
	ChangesDir    = "changes"
	ArchiveDir    = "changes/archive"
)

// DefaultDir — каталог описания относительно корня репозитория.
const DefaultDir = "docs/architecture"

// Manifest — манифест описания: версия формата, система и её назначение,
// принципы (фаза Preliminary) и собственные viewpoint'ы проекта.
type Manifest struct {
	Format string `json:"format"`
	System string `json:"system"`
	// Description — назначение описываемой системы: текст HLD и описание
	// системы на схеме контекста C4.
	Description string      `json:"description,omitempty"`
	Principles  []Principle `json:"principles,omitempty"`
	Viewpoints  []string    `json:"viewpoints,omitempty"`
	// Scenarios — индекс сценариев взаимодействия: идентификаторы файлов
	// scenarios/<id>.yaml. Описание читает только перечисленные.
	Scenarios []string `json:"scenarios,omitempty"`
	// Decisions — индекс архитектурных решений: имена файлов adr/<имя>.md
	// без расширения, вида NNNN-<slug>.
	Decisions []string `json:"decisions,omitempty"`
}

// Зарезервированные свойства элемента (props), которые уточняют вывод C4
// из модели. Остальные свойства остаются свободными.
const (
	// PropC4 — вид элемента в C4: person, system, container, database,
	// queue, component или none.
	PropC4 = "c4"
	// PropTechnology — строка технологии на C4-схемах.
	PropTechnology = "technology"
	// PropExternal — внешний человек или внешняя система (логическое).
	PropExternal = "external"
)

// Principle — принцип архитектуры проекта.
type Principle struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Rationale string `json:"rationale"`
}

// Element — элемент модели. Связи хранятся у источника, поэтому запись
// дельты по элементу несёт и его исходящие связи.
type Element struct {
	ID          string         `json:"id"`
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Props       map[string]any `json:"props,omitempty"`
	// Attributes — атрибуты объекта данных для ERD (только data-object).
	Attributes []Attribute `json:"attributes,omitempty"`
	Relations  []Relation  `json:"relations,omitempty"`
}

// Relation — исходящая связь элемента. Cardinality и Label задаются только
// у association между объектами данных: они нужны ERD.
type Relation struct {
	Type        string `json:"type"`
	To          string `json:"to"`
	Mode        string `json:"mode,omitempty"`
	Cardinality string `json:"cardinality,omitempty"`
	Label       string `json:"label,omitempty"`
}

// Gap — разрыв между текущим и целевым состоянием.
type Gap struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Kind        string   `json:"kind"`
	Description string   `json:"description"`
	Elements    []string `json:"elements,omitempty"`
	Processes   []string `json:"processes,omitempty"`
	// Scenarios — целевые сценарии изменения, которые покрывает разрыв.
	Scenarios []string `json:"scenarios,omitempty"`
}

// ChangeInfo — паспорт изменения из change.yaml.
type ChangeInfo struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Epic  string `json:"epic,omitempty"`
	// Scenarios — целевые сценарии изменения: changes/<id>/scenarios/<sid>.yaml.
	Scenarios []string `json:"scenarios,omitempty"`
	// Decisions — новые решения изменения: changes/<id>/adr/<имя>.md.
	Decisions []string `json:"decisions,omitempty"`
}

// idPattern — шаблон идентификаторов элементов, записей собственных
// viewpoint'ов и разрывов. Точка обязательна: префикс (biz., app., gap.)
// отделяет пространство имён и не даёт спутать идентификатор со словом.
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*(\.[a-z0-9-]+)+$`)

// changeIDPattern — шаблон идентификатора изменения: это имя каталога,
// поэтому без точек и слэшей.
var changeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// changeC4Suffix и changeScenariosSuffix — суффиксы файлов целевых C4-схем
// и сценариев изменения (views/changes/<id>-c4.md, <id>-scenarios.md).
// Изменение с таким суффиксом в id заняло бы имя файла другого изменения,
// поэтому такой id запрещён.
const (
	changeC4Suffix        = "-c4"
	changeScenariosSuffix = "-scenarios"
)

// ValidChangeID сообщает, годится ли строка как идентификатор изменения.
func ValidChangeID(id string) bool {
	return ChangeIDProblem(id) == ""
}

// ChangeIDProblem — почему идентификатор изменения не годится: текст для
// человека, его выводят и проверка, и CLI. Для годного id — пустая строка.
func ChangeIDProblem(id string) string {
	switch {
	case !changeIDPattern.MatchString(id):
		return "идентификатор изменения: только строчные латинские буквы, цифры и дефис, первый символ буква или цифра (шаблон " +
			changeIDPattern.String() + ")"
	case strings.HasSuffix(id, changeC4Suffix):
		return "идентификатор изменения не может оканчиваться на " + changeC4Suffix +
			": это имя занимает файл целевых C4-схем views/changes/<id>-c4.md"
	case strings.HasSuffix(id, changeScenariosSuffix):
		return "идентификатор изменения не может оканчиваться на " + changeScenariosSuffix +
			": это имя занимает файл целевых сценариев views/changes/<id>-scenarios.md"
	}
	return ""
}

// Слои модели. Идентификатор слоя совпадает с идентификатором встроенного
// viewpoint'а и именем файла model/<id>.yaml.
const (
	LayerBusiness    = "business"
	LayerApplication = "application"
	LayerData        = "data"
	LayerTechnology  = "technology"
)

// Типы элементов (подмножество ArchiMate 3.2).
const (
	TypeActor              = "actor"
	TypeRole               = "role"
	TypeBusinessService    = "business-service"
	TypeProcess            = "process"
	TypeBusinessObject     = "business-object"
	TypeComponent          = "component"
	TypeInterface          = "interface"
	TypeApplicationService = "application-service"
	TypeDataObject         = "data-object"
	TypeNode               = "node"
	TypeSystemSoftware     = "system-software"
)

// layerTypes — допустимые типы элементов по слоям в порядке отрисовки.
var layerTypes = map[string][]string{
	LayerBusiness:    {TypeActor, TypeRole, TypeBusinessService, TypeProcess, TypeBusinessObject},
	LayerApplication: {TypeComponent, TypeInterface, TypeApplicationService},
	LayerData:        {TypeDataObject},
	LayerTechnology:  {TypeNode, TypeSystemSoftware},
}

// typeLayer возвращает слой типа элемента или пустую строку.
func typeLayer(t string) string {
	for layer, types := range layerTypes {
		if slices.Contains(types, t) {
			return layer
		}
	}
	return ""
}

var typeTitles = map[string]string{
	TypeActor:              "Акторы",
	TypeRole:               "Роли",
	TypeBusinessService:    "Бизнес-сервисы",
	TypeProcess:            "Процессы",
	TypeBusinessObject:     "Бизнес-объекты",
	TypeComponent:          "Компоненты",
	TypeInterface:          "Интерфейсы",
	TypeApplicationService: "Сервисы приложений",
	TypeDataObject:         "Объекты данных",
	TypeNode:               "Узлы",
	TypeSystemSoftware:     "Системное ПО",
}

// Типы связей.
const (
	RelComposition = "composition"
	RelAssignment  = "assignment"
	RelRealization = "realization"
	RelServing     = "serving"
	RelAccess      = "access"
	RelFlow        = "flow"
	RelTriggering  = "triggering"
	RelAssociation = "association"
)

// relationRule — допустимые пары типов для связи.
type relationRule struct {
	anyPair   bool // association: любые элементы
	sameLayer bool // composition: элементы одного слоя
	pairs     []relationPair
}

type relationPair struct {
	from, to []string
}

// relationRules — таблица допустимых связей, упрощённые таблицы ArchiMate.
// Это данные, а не ветвления кода: таблица расширяется вместе с версией
// формата.
var relationRules = map[string]relationRule{
	RelComposition: {sameLayer: true},
	RelAssociation: {anyPair: true},
	RelAssignment: {pairs: []relationPair{
		{[]string{TypeActor}, []string{TypeRole}},
		{[]string{TypeActor, TypeRole}, []string{TypeProcess}},
		{[]string{TypeNode}, []string{TypeSystemSoftware}},
	}},
	RelRealization: {pairs: []relationPair{
		{[]string{TypeProcess}, []string{TypeBusinessService}},
		{[]string{TypeComponent}, []string{TypeApplicationService}},
		{[]string{TypeDataObject}, []string{TypeBusinessObject}},
		{[]string{TypeComponent, TypeApplicationService}, []string{TypeProcess}},
	}},
	RelServing: {pairs: []relationPair{
		{[]string{TypeBusinessService}, []string{TypeActor, TypeRole, TypeProcess}},
		{[]string{TypeApplicationService, TypeInterface, TypeComponent}, []string{TypeComponent, TypeProcess, TypeRole, TypeActor}},
		{[]string{TypeNode, TypeSystemSoftware}, []string{TypeComponent, TypeNode, TypeSystemSoftware}},
	}},
	RelAccess: {pairs: []relationPair{
		{[]string{TypeProcess, TypeComponent, TypeApplicationService}, []string{TypeBusinessObject, TypeDataObject}},
		{[]string{TypeSystemSoftware}, []string{TypeDataObject}},
	}},
	RelFlow: {pairs: []relationPair{
		{[]string{TypeProcess}, []string{TypeProcess}},
		{[]string{TypeComponent}, []string{TypeComponent}},
	}},
	RelTriggering: {pairs: []relationPair{
		{[]string{TypeProcess}, []string{TypeProcess}},
		{[]string{TypeComponent}, []string{TypeComponent}},
	}},
}

// relationAllowed сообщает, допустима ли связь между типами. Оба типа
// должны быть известны.
func relationAllowed(rel, from, to string) bool {
	rule, ok := relationRules[rel]
	if !ok {
		return false
	}
	if rule.anyPair {
		return true
	}
	if rule.sameLayer {
		return typeLayer(from) == typeLayer(to)
	}
	for _, p := range rule.pairs {
		if slices.Contains(p.from, from) && slices.Contains(p.to, to) {
			return true
		}
	}
	return false
}

// laneRefTypes и serviceTaskRefTypes — на что могут ссылаться дорожка и
// сервисная задача BPMN.
var (
	laneRefTypes        = []string{TypeRole, TypeActor}
	serviceTaskRefTypes = []string{TypeComponent, TypeApplicationService}
)
