package architecture

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

// ApplyResult — итог применения изменения.
type ApplyResult struct {
	Applied bool
	// Archive — путь архива изменения относительно корня описания.
	Archive string
	// Defects при Applied=false — причины отказа: конфликты с текущей
	// моделью и ошибки изменения. После применения — дефекты других
	// изменений, чьи представления не построились.
	Defects Defects
}

// Apply применяет изменение к описанию в каталоге dir: элементы added
// добавляются, modified заменяются, removed удаляются, целевые процессы
// заменяют или дополняют текущие, папка изменения переезжает в
// changes/archive/<дата>-<id>/, представления перегенерируются.
//
// Сначала строится и проверяется целевая модель, затем в памяти
// готовятся все файлы. При любом дефекте с важностью error не пишется
// ни один файл.
func Apply(dir, id string, now time.Time) (ApplyResult, error) {
	if !ValidChangeID(id) {
		return ApplyResult{}, fmt.Errorf("идентификатор изменения %q не подходит под шаблон %s", id, changeIDPattern)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ApplyResult{}, ErrAbsent
		}
		return ApplyResult{}, err
	}
	defer root.Close()
	fsys := root.FS()
	ch, ds, err := LoadChange(fsys, id)
	if err != nil {
		return ApplyResult{}, err
	}
	if ch == nil || ds.HasErrors() {
		return ApplyResult{Defects: ds}, nil
	}
	archive := ArchiveDir + "/" + now.Format("2006-01-02") + "-" + id
	// Всё, что может сорвать применение после записи модели, проверяется
	// заранее.
	if fi, err := fs.Stat(fsys, ArchiveDir); err == nil && !fi.IsDir() {
		return ApplyResult{Defects: Defects{{SeverityError, "apply.archive-dir", ArchiveDir, "", "changes/archive должен быть каталогом"}}}, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ApplyResult{}, err
	}
	switch _, err := fs.Stat(fsys, archive); {
	case err == nil:
		return ApplyResult{Defects: Defects{{SeverityError, "apply.archive-exists", archive, "", "архив изменения уже существует"}}}, nil
	case !errors.Is(err, fs.ErrNotExist):
		return ApplyResult{}, err
	}

	writes := map[string][]byte{}
	for _, d := range ch.Deltas {
		content, err := patchDoc(fsys, d)
		if err != nil {
			return ApplyResult{}, fmt.Errorf("%s: %w", d.Viewpoint.File(), err)
		}
		writes[d.Viewpoint.File()] = content
	}
	for pid, p := range ch.Processes {
		writes[processFile(pid)] = p.Raw
	}
	var deletes []string
	for _, d := range ch.Deltas {
		for _, rm := range d.Removed {
			if e := ch.Current.element(rm.ID); e == nil || e.Type != TypeProcess {
				continue
			}
			if _, err := fs.Stat(fsys, processFile(rm.ID)); err == nil {
				deletes = append(deletes, processFile(rm.ID))
			}
		}
	}

	// Запись с откатом: при сбое файловой системы описание возвращается
	// в исходное состояние, а не остаётся применённым наполовину.
	undo, err := backupFiles(fsys, append(sortedKeys(writes), deletes...))
	if err != nil {
		return ApplyResult{}, err
	}
	fail := func(err error) (ApplyResult, error) {
		if rerr := undo.restore(root); rerr != nil {
			return ApplyResult{}, fmt.Errorf("%w; откат не удался: %v", err, rerr)
		}
		return ApplyResult{}, err
	}
	for _, p := range sortedKeys(writes) {
		if err := writeFile(root, p, writes[p]); err != nil {
			return fail(err)
		}
	}
	for _, p := range deletes {
		if err := root.Remove(p); err != nil {
			return fail(err)
		}
	}
	if err := root.MkdirAll(ArchiveDir, 0o755); err != nil {
		return fail(err)
	}
	if err := root.Rename(ch.Dir, archive); err != nil {
		return fail(err)
	}
	// Модель уже применена и согласована. Сбой представлений не отменяет
	// применение: views/ перерисовывается командой views.
	vds, err := writeViews(root)
	return ApplyResult{Applied: true, Archive: archive, Defects: vds}, err
}

// savedFile — исходное содержимое файла. existed=false значит, что
// файла не было и при откате его нужно удалить.
type savedFile struct {
	path    string
	data    []byte
	existed bool
}

// backup — исходное содержимое файлов для отката.
type backup []savedFile

func backupFiles(fsys fs.FS, paths []string) (backup, error) {
	var b backup
	for _, p := range paths {
		raw, err := fs.ReadFile(fsys, p)
		switch {
		case err == nil:
			b = append(b, savedFile{p, raw, true})
		case errors.Is(err, fs.ErrNotExist):
			b = append(b, savedFile{p, nil, false})
		default:
			return nil, err
		}
	}
	return b, nil
}

func (b backup) restore(root *os.Root) error {
	var first error
	for i := len(b) - 1; i >= 0; i-- {
		f := b[i]
		var err error
		if f.existed {
			err = writeFile(root, f.path, f.data)
		} else if rerr := root.Remove(f.path); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			err = rerr
		}
		if err != nil && first == nil {
			first = err
		}
	}
	return first
}

// patchDoc правит документ viewpoint'а на уровне узлов YAML: записи
// добавляются в конец массива, заменяются и удаляются на месте.
// Комментарии и порядок нетронутых записей сохраняются, дифф в PR
// остаётся минимальным.
func patchDoc(fsys fs.FS, d *Delta) ([]byte, error) {
	raw, err := fs.ReadFile(fsys, d.Viewpoint.File())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	doc, _, err := parseYAML(raw)
	if err != nil {
		return nil, err
	}
	if doc == nil || len(doc.Content) == 0 {
		doc = &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	seq, err := ensureSequence(doc.Content[0], d.Viewpoint.items())
	if err != nil {
		return nil, err
	}

	draw, err := fs.ReadFile(fsys, d.File)
	if err != nil {
		return nil, err
	}
	ddoc, _, err := parseYAML(draw)
	if err != nil {
		return nil, err
	}
	added := sectionItems(ddoc, "added")
	modified := map[string]*yaml.Node{}
	for _, n := range sectionItems(ddoc, "modified") {
		modified[nodeID(n)] = n
	}
	removed := map[string]bool{}
	for _, rm := range d.Removed {
		removed[rm.ID] = true
	}

	wasEmpty := len(seq.Content) == 0
	content := make([]*yaml.Node, 0, len(seq.Content)+len(added))
	for _, item := range seq.Content {
		id := nodeID(item)
		if removed[id] {
			continue
		}
		if n := modified[id]; n != nil {
			content = append(content, n)
			continue
		}
		content = append(content, item)
	}
	content = append(content, added...)
	seq.Content = content
	if wasEmpty && len(content) > 0 {
		// `elements: []` после добавления записей пишется блоком, а не
		// одной строкой.
		seq.Style = 0
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ensureSequence находит массив записей по пути, создавая недостающие
// ключи. Пустое значение (`elements:`) становится пустым массивом.
func ensureSequence(root *yaml.Node, tokens []string) (*yaml.Node, error) {
	cur := root
	for i, tok := range tokens {
		last := i == len(tokens)-1
		var val *yaml.Node
		switch cur.Kind {
		case yaml.MappingNode:
			for j := 0; j+1 < len(cur.Content); j += 2 {
				if cur.Content[j].Value == tok {
					val = cur.Content[j+1]
					break
				}
			}
			if val == nil {
				val = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				if last {
					val = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
				}
				cur.Content = append(cur.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: tok}, val)
			}
		case yaml.SequenceNode:
			idx, err := strconv.Atoi(tok)
			if err != nil || idx < 0 || idx >= len(cur.Content) {
				return nil, fmt.Errorf("путь к записям не найден в документе: %s", formatPointer(tokens))
			}
			val = cur.Content[idx]
		default:
			return nil, fmt.Errorf("путь к записям не найден в документе: %s", formatPointer(tokens))
		}
		if last && val.Kind == yaml.ScalarNode && val.ShortTag() == "!!null" {
			val.Kind, val.Tag, val.Value, val.Style = yaml.SequenceNode, "!!seq", "", 0
		}
		cur = val
	}
	if cur.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("по пути %s ожидается массив записей", formatPointer(tokens))
	}
	return cur, nil
}

func sectionItems(doc *yaml.Node, section string) []*yaml.Node {
	if doc == nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}
	m := doc.Content[0]
	for j := 0; j+1 < len(m.Content); j += 2 {
		if m.Content[j].Value == section && m.Content[j+1].Kind == yaml.SequenceNode {
			return m.Content[j+1].Content
		}
	}
	return nil
}

func nodeID(n *yaml.Node) string {
	if n.Kind != yaml.MappingNode {
		return ""
	}
	for j := 0; j+1 < len(n.Content); j += 2 {
		if n.Content[j].Value == "id" {
			return n.Content[j+1].Value
		}
	}
	return ""
}

// Init создаёт пустое описание в каталоге dir: манифест, пустые слои,
// каталоги процессов и изменений и представления.
func Init(dir, system string) error {
	manifest, err := yaml.Marshal(struct {
		Format     string   `yaml:"format"`
		System     string   `yaml:"system"`
		Principles []any    `yaml:"principles"`
		Viewpoints []string `yaml:"viewpoints"`
	}{Format, system, []any{}, []string{}})
	if err != nil {
		return err
	}
	files := map[string][]byte{
		ManifestFile:                   manifest,
		ProcessesDir + "/.gitkeep":     nil,
		ChangesDir + "/.gitkeep":       nil,
		ModelDir + "/business.yaml":    []byte("elements: []\n"),
		ModelDir + "/application.yaml": []byte("elements: []\n"),
		ModelDir + "/data.yaml":        []byte("elements: []\n"),
		ModelDir + "/technology.yaml":  []byte("elements: []\n"),
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	// Ничего чужого не перезаписываем: описание без манифеста — тоже чьи-то
	// файлы.
	for _, p := range sortedKeys(files) {
		if _, err := root.Lstat(p); err == nil {
			return fmt.Errorf("в %s уже есть %s, описание не создаётся", dir, p)
		}
	}
	for _, p := range sortedKeys(files) {
		if err := writeFile(root, p, files[p]); err != nil {
			return err
		}
	}
	ds, err := writeViews(root)
	if err != nil {
		return err
	}
	if ds.HasErrors() {
		return fmt.Errorf("созданное описание не проходит проверку: %v", ds[0])
	}
	return nil
}
