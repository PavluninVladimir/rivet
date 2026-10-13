// Package architecture — описание архитектуры проекта в его репозитории
// (спека backend/architecture): слои TOGAF на подмножестве ArchiMate,
// бизнес-процессы в BPMN, viewpoint'ы, изменения с дельтой и разрывами.
// Пакет разбирает и проверяет описание детерминированно, без модели,
// рисует Mermaid-представления и применяет изменения. От store,
// оркестратора и протокола он не зависит: им пользуются CLI и runner.
package architecture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"
)

// ErrAbsent — в каталоге нет описания архитектуры. Это не ошибка проекта:
// описание необязательно.
var ErrAbsent = errors.New("описания архитектуры нет")

// Model — разобранное описание: текущее состояние или целевое состояние
// изменения.
type Model struct {
	Manifest   Manifest
	viewpoints []Viewpoint
	docs       map[string]*Doc     // по идентификатору viewpoint'а
	processes  map[string]*Process // по идентификатору элемента process
	records    map[string]*Record  // все записи по id, первая при дублях
}

// Doc — документ viewpoint'а.
type Doc struct {
	vp      Viewpoint
	file    string
	present bool // файл есть или документ получен из дельты
	value   any
	records []*Record
	origins map[string]string // id → файл записи, если она пришла из дельты
}

// Record — запись документа: элемент слоя или запись собственного
// viewpoint'а.
type Record struct {
	ID    string
	File  string // где запись описана: документ модели или файл дельты
	Index int    // позиция в массиве записей документа
	Value map[string]any
	// Element заполнен у встроенных слоёв.
	Element *Element
}

// Viewpoints возвращает встроенные и собственные viewpoint'ы описания.
func (m *Model) Viewpoints() []Viewpoint { return m.viewpoints }

// Viewpoint возвращает viewpoint по идентификатору.
func (m *Model) Viewpoint(id string) Viewpoint {
	for _, vp := range m.viewpoints {
		if vp.ID() == id {
			return vp
		}
	}
	return nil
}

// Elements возвращает элементы слоёв в порядке слоёв и документа.
func (m *Model) Elements() []Element {
	var out []Element
	for _, vp := range m.viewpoints {
		if !vp.Builtin() {
			continue
		}
		for _, rec := range m.docs[vp.ID()].records {
			out = append(out, *rec.Element)
		}
	}
	return out
}

// Element возвращает элемент слоя по идентификатору.
func (m *Model) Element(id string) (Element, bool) {
	if e := m.element(id); e != nil {
		return *e, true
	}
	return Element{}, false
}

// Process возвращает процесс по идентификатору элемента process.
func (m *Model) Process(id string) *Process { return m.processes[id] }

func (m *Model) element(id string) *Element {
	if rec := m.records[id]; rec != nil {
		return rec.Element
	}
	return nil
}

func (m *Model) index() {
	m.records = map[string]*Record{}
	for _, vp := range m.viewpoints {
		for _, rec := range m.docs[vp.ID()].records {
			if _, dup := m.records[rec.ID]; !dup {
				m.records[rec.ID] = rec
			}
		}
	}
}

// load разбирает описание: манифест, viewpoint'ы, документы и файлы
// процессов. Семантику проверяет check. nil без ошибки означает, что
// описание дальше манифеста не разбирается, причина — в reporter.
func load(fsys fs.FS, r *reporter) (*Model, error) {
	raw, err := fs.ReadFile(fsys, ManifestFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrAbsent
	}
	if err != nil {
		return nil, err
	}
	_, v, err := parseYAML(raw)
	if err != nil {
		r.errorf("yaml.parse", ManifestFile, "", "файл не разбирается: %v", err)
		return nil, nil
	}
	// Версия формата проверяется до всего остального: в чужой версии у
	// файлов может быть другой смысл, и дефекты по ним только запутают.
	obj, _ := v.(map[string]any)
	if format, _ := obj["format"].(string); format != Format {
		r.errorf("format.unsupported", ManifestFile, "/format",
			"неподдерживаемая версия формата %q, поддерживается %s", format, Format)
		return nil, nil
	}
	reportSchema(r, manifestSchema, v, func(p []string) (string, string) { return ManifestFile, formatPointer(p) })
	m := &Model{docs: map[string]*Doc{}, processes: map[string]*Process{}}
	decodeValue(v, &m.Manifest)

	m.viewpoints = BuiltinViewpoints()
	for _, c := range loadCustomViewpoints(fsys, m.Manifest.Viewpoints, r) {
		m.viewpoints = append(m.viewpoints, c)
	}
	for _, vp := range m.viewpoints {
		d, err := readDoc(fsys, vp, r)
		if err != nil {
			return nil, err
		}
		m.docs[vp.ID()] = d
	}
	m.index()
	for _, rec := range m.records {
		if rec.Element == nil || rec.Element.Type != TypeProcess || !idPattern.MatchString(rec.ID) {
			continue
		}
		file := processFile(rec.ID)
		raw, err := fs.ReadFile(fsys, file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		m.processes[rec.ID] = newProcess(rec.ID, file, raw)
	}
	m.reportUnreferenced(fsys, r)
	return m, nil
}

func loadCustomViewpoints(fsys fs.FS, ids []string, r *reporter) []*customViewpoint {
	var out []*customViewpoint
	seen := map[string]bool{}
	for i, id := range ids {
		loc := fmt.Sprintf("/viewpoints/%d", i)
		switch {
		case isBuiltin(id):
			r.errorf("viewpoint.builtin-conflict", ManifestFile, loc, "viewpoint %s совпадает со встроенным", id)
			continue
		case seen[id]:
			r.errorf("viewpoint.duplicate", ManifestFile, loc, "viewpoint %s объявлен повторно", id)
			continue
		case !viewpointIDPattern.MatchString(id):
			r.errorf("viewpoint.id", ManifestFile, loc, "идентификатор viewpoint'а %q не подходит под шаблон %s", id, viewpointIDPattern)
			continue
		}
		seen[id] = true
		file := ViewpointsDir + "/" + id + ".yaml"
		raw, err := fs.ReadFile(fsys, file)
		if err != nil {
			r.errorf("viewpoint.missing", ManifestFile, loc, "нет файла %s с определением viewpoint'а", file)
			continue
		}
		_, v, err := parseYAML(raw)
		if err != nil {
			r.errorf("yaml.parse", file, "", "файл не разбирается: %v", err)
			continue
		}
		reportSchema(r, viewpointSchema, v, func(p []string) (string, string) { return file, formatPointer(p) })
		c := &customViewpoint{}
		decodeValue(v, &c.def)
		if c.def.ID != id {
			r.errorf("viewpoint.id-mismatch", file, "/id", "id %q не совпадает с именем в манифесте %s", c.def.ID, id)
			c.def.ID = id
		}
		ptr, err := parsePointer(c.def.Items)
		if err != nil || len(ptr) == 0 {
			// Без пути к записям документ не адресуется дельтой: такой
			// viewpoint не подключается, дефект уже в отчёте схемы или здесь.
			if err != nil {
				r.errorf("viewpoint.items", file, "/items", "%v", err)
			}
			continue
		}
		c.ptr = ptr
		if c.def.Schema != nil {
			sch, err := compileSchema("viewpoints/"+id+".json", c.def.Schema)
			if err != nil {
				r.errorf("viewpoint.schema", file, "/schema", "JSON Schema не компилируется: %v", err)
			} else {
				c.sch = sch
			}
			scan := refPaths(c.def.Schema)
			c.refs = scan.paths
			for _, at := range scan.conditional {
				r.errorf("viewpoint.ref-conditional", file, "/schema",
					"x-rivet-ref по пути %s стоит в условной ветке схемы (if/then/else, anyOf, oneOf, not, contains, dependentSchemas): "+
						"пометь поле там, где схема применяется безусловно", at)
			}
			for _, ref := range scan.unsupported {
				r.errorf("viewpoint.ref-unsupported", file, "/schema",
					"%s: поддерживаются только локальные ссылки #/<JSON Pointer> и #<$anchor>, иначе пометки x-rivet-ref не проверить", ref)
			}
		}
		if c.def.Render.Kind == "graph" {
			refs := c.refFields()
			for j, f := range c.def.Render.Edges {
				if !refs[f] {
					r.errorf("viewpoint.render", file, fmt.Sprintf("/render/edges/%d", j),
						"поле %s не помечено в схеме записи как ссылка x-rivet-ref", f)
				}
			}
		}
		out = append(out, c)
	}
	return out
}

func readDoc(fsys fs.FS, vp Viewpoint, r *reporter) (*Doc, error) {
	d := &Doc{vp: vp, file: vp.File(), value: map[string]any{}}
	raw, err := fs.ReadFile(fsys, d.file)
	if errors.Is(err, fs.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	d.present = true
	_, v, err := parseYAML(raw)
	if err != nil {
		r.errorf("yaml.parse", d.file, "", "файл не разбирается: %v", err)
		return d, nil
	}
	d.value = v
	d.extract(r)
	return d, nil
}

// extract строит записи по массиву документа. Записи без строкового id не
// адресуются: у встроенных слоёв это ловит схема, у собственных — здесь.
func (d *Doc) extract(r *reporter) {
	d.records = nil
	at, ok := valueAt(d.value, d.vp.items())
	if !ok || at == nil {
		return
	}
	arr, ok := at.([]any)
	if !ok {
		if r != nil && !d.vp.Builtin() {
			r.errorf("doc.items", d.file, formatPointer(d.vp.items()), "по пути записей ожидается массив")
		}
		return
	}
	for i, item := range arr {
		obj, ok := item.(map[string]any)
		id, _ := obj["id"].(string)
		if !ok || id == "" {
			if r != nil && !d.vp.Builtin() {
				r.errorf("doc.item-id", d.file, formatPointer(append(slices.Clone(d.vp.items()), strconv.Itoa(i))),
					"у записи нет строкового поля id")
			}
			continue
		}
		rec := &Record{ID: id, File: d.file, Index: i, Value: obj}
		if f, ok := d.origins[id]; ok {
			rec.File = f
		}
		if d.vp.Builtin() {
			rec.Element = elementFrom(obj)
		}
		d.records = append(d.records, rec)
	}
}

// place сопоставляет месту в документе файл и location: внутри записи —
// её id и путь от неё, иначе JSON Pointer от корня документа.
func (d *Doc) place(p []string) (string, string) {
	items := d.vp.items()
	if len(p) > len(items) && slices.Equal(p[:len(items)], items) {
		if i, err := strconv.Atoi(p[len(items)]); err == nil {
			for _, rec := range d.records {
				if rec.Index != i {
					continue
				}
				loc := rec.ID
				if rest := p[len(items)+1:]; len(rest) > 0 {
					loc += formatPointer(rest)
				}
				return rec.File, loc
			}
		}
	}
	return d.file, formatPointer(p)
}

func (d *Doc) record(id string) *Record {
	for _, rec := range d.records {
		if rec.ID == id {
			return rec
		}
	}
	return nil
}

// elementFrom читает элемент из записи без отказов: несоответствие типов
// уже отражено дефектами схемы.
func elementFrom(v map[string]any) *Element {
	e := &Element{}
	e.ID, _ = v["id"].(string)
	e.Type, _ = v["type"].(string)
	e.Name, _ = v["name"].(string)
	e.Description, _ = v["description"].(string)
	if p, ok := v["props"].(map[string]any); ok && len(p) > 0 {
		e.Props = p
	}
	rels, _ := v["relations"].([]any)
	for _, x := range rels {
		obj, ok := x.(map[string]any)
		if !ok {
			continue
		}
		var rel Relation
		rel.Type, _ = obj["type"].(string)
		rel.To, _ = obj["to"].(string)
		rel.Mode, _ = obj["mode"].(string)
		e.Relations = append(e.Relations, rel)
	}
	return e
}

// decodeValue раскладывает JSON-значение в структуру. Ошибки типов
// игнорируются: их уже нашла схема, а разбор берёт всё, что подходит.
func decodeValue(v any, out any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, out)
}

// reportUnreferenced предупреждает о файлах, которые описание не читает:
// чаще всего это опечатка в имени или забытый элемент модели.
func (m *Model) reportUnreferenced(fsys fs.FS, r *reporter) {
	known := map[string]bool{}
	for _, vp := range m.viewpoints {
		known[vp.File()] = true
		if !vp.Builtin() {
			known[ViewpointsDir+"/"+vp.ID()+".yaml"] = true
		}
	}
	for id := range m.processes {
		known[processFile(id)] = true
	}
	for _, rec := range m.records {
		if rec.Element != nil && rec.Element.Type == TypeProcess {
			known[processFile(rec.ID)] = true
		}
	}
	for _, dir := range []struct{ name, ext string }{{ModelDir, ".yaml"}, {ProcessesDir, ".bpmn"}, {ViewpointsDir, ".yaml"}} {
		entries, err := fs.ReadDir(fsys, dir.name)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			file := path.Join(dir.name, name)
			if !known[file] {
				r.warnf("file.unreferenced", file, "", "файл не относится ни к одному viewpoint'у или процессу описания")
			}
		}
	}
}

// checkOptions — уточнения проверки целевой модели изменения.
type checkOptions struct {
	// removed — удаляемые изменением элементы. Ссылки на них
	// сообщает правило delta.removed-in-use, а не ref.unresolved.
	removed map[string]bool
}

// check — семантическая проверка разобранного описания.
func (m *Model) check(r *reporter, opt checkOptions) {
	seen := map[string]bool{}
	for _, p := range m.Manifest.Principles {
		if p.ID == "" {
			continue
		}
		if seen[p.ID] {
			r.errorf("principle.duplicate", ManifestFile, p.ID, "принцип %s объявлен повторно", p.ID)
		}
		seen[p.ID] = true
	}
	owners := map[string]*Record{}
	for _, vp := range m.viewpoints {
		d := m.docs[vp.ID()]
		if sch := vp.schema(); sch != nil && d.present {
			reportSchema(r, sch, d.value, d.place)
		}
		for _, rec := range d.records {
			if !idPattern.MatchString(rec.ID) {
				r.errorf("id.pattern", rec.File, rec.ID, "идентификатор %q не подходит под шаблон %s", rec.ID, idPattern)
			}
			if prev, dup := owners[rec.ID]; dup {
				r.errorf("id.duplicate", rec.File, rec.ID, "идентификатор %s уже занят в %s", rec.ID, prev.File)
				continue
			}
			owners[rec.ID] = rec
		}
	}
	for _, vp := range m.viewpoints {
		vp.check(m, m.docs[vp.ID()], r, opt)
	}
}

func (m *Model) checkRelation(rec *Record, rel Relation, r *reporter, opt checkOptions) {
	e := rec.Element
	if rel.Type == "" || rel.To == "" {
		return
	}
	if _, ok := relationRules[rel.Type]; !ok {
		r.errorf("relation.type-unknown", rec.File, e.ID, "неизвестный тип связи %q", rel.Type)
		return
	}
	if rel.Mode != "" && rel.Type != RelAccess {
		r.errorf("relation.mode", rec.File, e.ID, "режим %s задаётся только у связи access, а не у %s", rel.Mode, rel.Type)
	}
	if rel.To == e.ID {
		r.errorf("relation.self", rec.File, e.ID, "связь %s ведёт в сам элемент", rel.Type)
		return
	}
	if opt.removed[rel.To] {
		return
	}
	target := m.element(rel.To)
	if target == nil {
		r.errorf("ref.unresolved", rec.File, e.ID, "связь %s ссылается на несуществующий элемент %s", rel.Type, rel.To)
		return
	}
	if typeLayer(e.Type) == "" || typeLayer(target.Type) == "" {
		return
	}
	if !relationAllowed(rel.Type, e.Type, target.Type) {
		r.errorf("relation.not-allowed", rec.File, e.ID, "связь %s недопустима между %s и %s", rel.Type, e.Type, target.Type)
	}
}

// Validate проверяет описание и все активные изменения в нём. ErrAbsent
// означает, что описания нет.
func Validate(fsys fs.FS) (Defects, error) {
	r := newReporter()
	m, err := load(fsys, r)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return r.result(), nil
	}
	m.check(r, checkOptions{})
	ids, err := ActiveChanges(fsys)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if _, err := loadChange(fsys, m, id, r); err != nil {
			return nil, err
		}
	}
	// Каталог с неподходящим именем не считается изменением. Молча его
	// пропускать нельзя: автор решит, что изменение проверено.
	entries, _ := fs.ReadDir(fsys, ChangesDir)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() && name != path.Base(ArchiveDir) && !strings.HasPrefix(name, ".") && !ValidChangeID(name) {
			r.errorf("change.id", ChangesDir+"/"+name, "", "имя каталога изменения не подходит под шаблон %s", changeIDPattern)
		}
	}
	return r.result(), nil
}

// Load разбирает и проверяет текущее описание.
func Load(fsys fs.FS) (*Model, Defects, error) {
	r := newReporter()
	m, err := load(fsys, r)
	if err != nil {
		return nil, nil, err
	}
	if m != nil {
		m.check(r, checkOptions{})
	}
	return m, r.result(), nil
}

// ActiveChanges возвращает идентификаторы неприменённых изменений.
func ActiveChanges(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ChangesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != path.Base(ArchiveDir) && ValidChangeID(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}
