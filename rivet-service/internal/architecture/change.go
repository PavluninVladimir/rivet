package architecture

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// Change — изменение архитектуры вместе с целевой моделью.
type Change struct {
	Info   ChangeInfo
	Dir    string // changes/<id>
	Vision string
	Deltas []*Delta // в порядке viewpoint'ов
	Gaps   []Gap
	// Processes — целевые процессы изменения по идентификатору.
	Processes map[string]*Process
	Current   *Model
	Target    *Model
}

// Delta — дельта одного viewpoint'а.
type Delta struct {
	Viewpoint Viewpoint
	File      string
	Added     []*Record
	Modified  []*Record
	Removed   []Removal
}

// Removal — удаляемая запись.
type Removal struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

func (ch *Change) delta(vp string) *Delta {
	for _, d := range ch.Deltas {
		if d.Viewpoint.ID() == vp {
			return d
		}
	}
	return nil
}

// touched возвращает записи, которые затрагивает дельта: id → файл дельты.
func (ch *Change) touched() map[string]string {
	out := map[string]string{}
	for _, d := range ch.Deltas {
		for _, rec := range append(append([]*Record(nil), d.Added...), d.Modified...) {
			out[rec.ID] = d.File
		}
		for _, rm := range d.Removed {
			out[rm.ID] = d.File
		}
	}
	return out
}

func (ch *Change) removed() map[string]bool {
	out := map[string]bool{}
	for _, d := range ch.Deltas {
		for _, rm := range d.Removed {
			out[rm.ID] = true
		}
	}
	return out
}

// LoadChange разбирает изменение и проверяет его вместе с текущей моделью.
// Дефекты включают ошибки разбора текущего описания, самого изменения и
// полной проверки целевой модели.
func LoadChange(fsys fs.FS, id string) (*Change, Defects, error) {
	if !ValidChangeID(id) {
		return nil, nil, fmt.Errorf("идентификатор изменения %q не подходит под шаблон %s", id, changeIDPattern)
	}
	r := newReporter()
	cur, err := load(fsys, r)
	if err != nil {
		return nil, nil, err
	}
	if cur == nil {
		return nil, r.result(), nil
	}
	ch, err := loadChange(fsys, cur, id, r)
	if err != nil {
		return nil, nil, err
	}
	return ch, r.result(), nil
}

// ValidateChange проверяет одно изменение.
func ValidateChange(fsys fs.FS, id string) (Defects, error) {
	_, ds, err := LoadChange(fsys, id)
	return ds, err
}

func loadChange(fsys fs.FS, cur *Model, id string, r *reporter) (*Change, error) {
	dir := ChangesDir + "/" + id
	infoFile := dir + "/change.yaml"
	raw, err := fs.ReadFile(fsys, infoFile)
	if errors.Is(err, fs.ErrNotExist) {
		r.errorf("change.missing", infoFile, "", "нет файла изменения %s", infoFile)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ch := &Change{Dir: dir, Current: cur, Processes: map[string]*Process{}}
	if _, v, err := parseYAML(raw); err != nil {
		r.errorf("yaml.parse", infoFile, "", "файл не разбирается: %v", err)
	} else {
		reportSchema(r, changeSchema, v, func(p []string) (string, string) { return infoFile, formatPointer(p) })
		decodeValue(v, &ch.Info)
	}
	if ch.Info.ID != "" && ch.Info.ID != id {
		r.errorf("change.id-mismatch", infoFile, "/id", "id %q не совпадает с каталогом изменения %s", ch.Info.ID, id)
	}
	ch.Info.ID = id

	vision, err := fs.ReadFile(fsys, dir+"/vision.md")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ch.Vision = string(vision)
	if strings.TrimSpace(ch.Vision) == "" {
		r.errorf("change.vision", dir+"/vision.md", "", "нет видения изменения: файл vision.md пуст или отсутствует")
	}

	if err := ch.loadDeltas(fsys, r); err != nil {
		return nil, err
	}
	ch.checkApplicable(r)
	if err := ch.buildTarget(fsys, r); err != nil {
		return nil, err
	}
	if err := ch.loadGaps(fsys, r); err != nil {
		return nil, err
	}
	ch.checkCoverage(r)
	ch.checkRemovedInUse(r)
	ch.Target.check(r, checkOptions{removed: ch.removed()})
	return ch, nil
}

func (ch *Change) loadDeltas(fsys fs.FS, r *reporter) error {
	known := map[string]bool{}
	for _, vp := range ch.Current.viewpoints {
		file := ch.Dir + "/delta/" + vp.ID() + ".yaml"
		known[file] = true
		raw, err := fs.ReadFile(fsys, file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		_, v, err := parseYAML(raw)
		if err != nil {
			r.errorf("yaml.parse", file, "", "файл не разбирается: %v", err)
			continue
		}
		reportSchema(r, deltaSchema, v, func(p []string) (string, string) { return file, formatPointer(p) })
		d := &Delta{Viewpoint: vp, File: file}
		obj, _ := v.(map[string]any)
		records := func(section string) []*Record {
			var out []*Record
			list, _ := obj[section].([]any)
			for i, item := range list {
				m, _ := item.(map[string]any)
				id, _ := m["id"].(string)
				if id == "" {
					continue
				}
				rec := &Record{ID: id, File: file, Index: i, Value: m}
				if vp.Builtin() {
					rec.Element = elementFrom(m)
				}
				out = append(out, rec)
			}
			return out
		}
		d.Added = records("added")
		d.Modified = records("modified")
		var removed struct {
			Removed []Removal `json:"removed"`
		}
		decodeValue(obj, &removed)
		for _, rm := range removed.Removed {
			if rm.ID != "" {
				d.Removed = append(d.Removed, rm)
			}
		}
		ch.Deltas = append(ch.Deltas, d)
	}
	entries, err := fs.ReadDir(fsys, ch.Dir+"/delta")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		file := ch.Dir + "/delta/" + e.Name()
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || known[file] {
			continue
		}
		r.errorf("delta.viewpoint-unknown", file, "", "дельта не относится ни к одному viewpoint'у описания")
	}
	return nil
}

// checkApplicable проверяет, что дельта применима к текущей модели.
// Нарушения здесь — конфликты: при них изменение не применяется.
func (ch *Change) checkApplicable(r *reporter) {
	cur := ch.Current
	seen := map[string]string{}
	mark := func(id, file string) {
		if prev, dup := seen[id]; dup {
			r.errorf("delta.duplicate", file, id, "элемент %s уже меняется в %s", id, prev)
			return
		}
		seen[id] = file
	}
	for _, d := range ch.Deltas {
		doc := cur.docs[d.Viewpoint.ID()]
		for _, rec := range d.Added {
			mark(rec.ID, d.File)
			if prev := cur.records[rec.ID]; prev != nil {
				r.errorf("delta.added-exists", d.File, rec.ID, "элемент %s уже есть в текущей модели (%s)", rec.ID, prev.File)
			}
		}
		exists := func(id string) {
			if doc.record(id) != nil {
				return
			}
			if prev := cur.records[id]; prev != nil {
				r.errorf("delta.target-missing", d.File, id, "элемент %s лежит в %s, а не в %s", id, prev.File, doc.file)
				return
			}
			r.errorf("delta.target-missing", d.File, id, "элемента %s нет в текущей модели", id)
		}
		for _, rec := range d.Modified {
			mark(rec.ID, d.File)
			exists(rec.ID)
		}
		for _, rm := range d.Removed {
			mark(rm.ID, d.File)
			exists(rm.ID)
		}
	}
}

// buildTarget строит целевую модель: текущая модель с применённой дельтой
// и целевыми процессами.
func (ch *Change) buildTarget(fsys fs.FS, r *reporter) error {
	cur := ch.Current
	t := &Model{Manifest: cur.Manifest, viewpoints: cur.viewpoints, docs: map[string]*Doc{}, processes: map[string]*Process{}}
	for _, vp := range cur.viewpoints {
		cd := cur.docs[vp.ID()]
		d := ch.delta(vp.ID())
		if d == nil {
			t.docs[vp.ID()] = cd
			continue
		}
		nd := &Doc{vp: vp, file: cd.file, present: true, origins: map[string]string{}}
		value, ok := deepCopy(cd.value).(map[string]any)
		if !ok {
			value = map[string]any{}
		}
		at, _ := valueAt(value, vp.items())
		arr, _ := at.([]any)
		removed := map[string]bool{}
		for _, rm := range d.Removed {
			removed[rm.ID] = true
		}
		modified := map[string]*Record{}
		for _, rec := range d.Modified {
			modified[rec.ID] = rec
		}
		out := make([]any, 0, len(arr)+len(d.Added))
		for _, item := range arr {
			if obj, ok := item.(map[string]any); ok {
				id, _ := obj["id"].(string)
				if removed[id] {
					continue
				}
				if rec := modified[id]; rec != nil {
					out = append(out, deepCopy(rec.Value))
					nd.origins[id] = d.File
					continue
				}
			}
			out = append(out, item)
		}
		for _, rec := range d.Added {
			out = append(out, deepCopy(rec.Value))
			nd.origins[rec.ID] = d.File
		}
		if !setAt(value, vp.items(), out) {
			r.errorf("doc.items", cd.file, formatPointer(vp.items()), "путь к записям не найден в документе, дельта не применяется")
			t.docs[vp.ID()] = cd
			continue
		}
		nd.value = value
		nd.extract(nil)
		t.docs[vp.ID()] = nd
	}
	t.index()
	for id, rec := range t.records {
		if rec.Element == nil || rec.Element.Type != TypeProcess || !idPattern.MatchString(id) {
			continue
		}
		file := ch.Dir + "/" + processFile(id)
		raw, err := fs.ReadFile(fsys, file)
		switch {
		case err == nil:
			p := newProcess(id, file, raw)
			ch.Processes[id] = p
			t.processes[id] = p
		case errors.Is(err, fs.ErrNotExist):
			if p := cur.processes[id]; p != nil {
				t.processes[id] = p
			}
		default:
			return err
		}
	}
	entries, err := fs.ReadDir(fsys, ch.Dir+"/"+ProcessesDir)
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			pid := strings.TrimSuffix(name, ".bpmn")
			if path.Ext(name) == ".bpmn" && ch.Processes[pid] != nil {
				continue
			}
			r.errorf("process.orphan", ch.Dir+"/"+ProcessesDir+"/"+name, "",
				"целевой процесс не соответствует ни одному элементу process целевой модели")
		}
	}
	ch.Target = t
	return nil
}

func (ch *Change) loadGaps(fsys fs.FS, r *reporter) error {
	file := ch.Dir + "/gap.yaml"
	raw, err := fs.ReadFile(fsys, file)
	if errors.Is(err, fs.ErrNotExist) {
		r.errorf("gap.missing", file, "", "нет файла разрывов gap.yaml")
		return nil
	}
	if err != nil {
		return err
	}
	_, v, err := parseYAML(raw)
	if err != nil {
		r.errorf("yaml.parse", file, "", "файл не разбирается: %v", err)
		return nil
	}
	var doc struct {
		Gaps []Gap `json:"gaps"`
	}
	decodeValue(v, &doc)
	ch.Gaps = doc.Gaps
	reportSchema(r, gapSchema, v, func(p []string) (string, string) {
		if len(p) >= 2 && p[0] == "gaps" {
			if list, ok := v.(map[string]any)["gaps"].([]any); ok {
				var i int
				if _, err := fmt.Sscan(p[1], &i); err == nil && i >= 0 && i < len(list) {
					if g, _ := list[i].(map[string]any); g != nil {
						if id, _ := g["id"].(string); id != "" {
							return file, id + formatPointerTail(p[2:])
						}
					}
				}
			}
		}
		return file, formatPointer(p)
	})
	seen := map[string]bool{}
	for _, g := range ch.Gaps {
		if g.ID == "" {
			continue
		}
		if !idPattern.MatchString(g.ID) {
			r.errorf("id.pattern", file, g.ID, "идентификатор %q не подходит под шаблон %s", g.ID, idPattern)
		}
		if seen[g.ID] {
			r.errorf("gap.duplicate", file, g.ID, "разрыв %s объявлен повторно", g.ID)
		}
		seen[g.ID] = true
	}
	return nil
}

func formatPointerTail(p []string) string {
	if len(p) == 0 {
		return ""
	}
	return formatPointer(p)
}

// checkCoverage сверяет дельту и разрывы в обе стороны: каждая запись
// дельты и каждый целевой процесс покрыты разрывом, а разрыв ссылается
// только на то, что изменение затрагивает.
func (ch *Change) checkCoverage(r *reporter) {
	file := ch.Dir + "/gap.yaml"
	touched := ch.touched()
	covered, coveredProc := map[string]bool{}, map[string]bool{}
	for _, g := range ch.Gaps {
		if g.ID == "" {
			continue
		}
		if len(g.Elements)+len(g.Processes) == 0 {
			r.errorf("gap.no-refs", file, g.ID, "разрыв не ссылается ни на элементы, ни на процессы")
		}
		for _, id := range g.Elements {
			if _, ok := touched[id]; !ok {
				r.errorf("gap.ref-outside", file, g.ID, "разрыв ссылается на %s, который изменение не затрагивает", id)
				continue
			}
			covered[id] = true
		}
		for _, pid := range g.Processes {
			if ch.Processes[pid] == nil {
				r.errorf("gap.ref-outside", file, g.ID, "разрыв ссылается на процесс %s, у которого нет целевой версии в %s/%s",
					pid, ch.Dir, ProcessesDir)
				continue
			}
			coveredProc[pid] = true
		}
	}
	for _, id := range sortedKeys(touched) {
		if !covered[id] {
			r.errorf("delta.uncovered", touched[id], id, "изменение элемента не покрыто разрывом")
		}
	}
	for _, pid := range sortedKeys(ch.Processes) {
		if !coveredProc[pid] {
			r.errorf("process.uncovered", ch.Processes[pid].File, "", "целевой процесс %s не покрыт разрывом", pid)
		}
	}
	if len(touched) == 0 && len(ch.Processes) == 0 {
		r.errorf("change.empty", ch.Dir+"/change.yaml", "", "изменение ничего не меняет: нет ни дельты, ни целевых процессов")
	}
}

// checkRemovedInUse отклоняет удаление элемента, на который в целевой
// модели остаются ссылки: связи, дорожки и задачи процессов, записи
// собственных viewpoint'ов.
func (ch *Change) checkRemovedInUse(r *reporter) {
	t := ch.Target
	users := map[string]map[string]bool{}
	use := func(target, user string) {
		if users[target] == nil {
			users[target] = map[string]bool{}
		}
		users[target][user] = true
	}
	for _, vp := range t.viewpoints {
		for _, ref := range vp.references(t.docs[vp.ID()]) {
			if ref.From != "" {
				use(ref.To, ref.From)
			} else {
				// Ссылка вне записей, например в шапке документа.
				use(ref.To, ref.File+" "+ref.Location)
			}
		}
	}
	for id, p := range t.processes {
		for _, ref := range p.refs() {
			use(ref, "процесс "+id)
		}
	}
	for _, d := range ch.Deltas {
		for _, rm := range d.Removed {
			if len(users[rm.ID]) == 0 {
				continue
			}
			r.errorf("delta.removed-in-use", d.File, rm.ID, "элемент удаляется, но на него ссылаются: %s",
				strings.Join(sortedKeys(users[rm.ID]), ", "))
		}
	}
}
