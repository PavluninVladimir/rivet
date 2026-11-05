package architecture

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

// Применение сценариев и решений изменения: файлы переносятся как есть,
// у заменённых решений правится front matter, индекс манифеста
// дополняется. Всё готовится в памяти до записи, как и дельта слоёв.

// docWrites готовит файлы сценариев, решений и манифеста. Конфликт с
// файлом, которого нет в индексе текущего описания, возвращается дефектом.
func docWrites(fsys fs.FS, ch *Change, writes map[string][]byte) (Defects, error) {
	if len(ch.Scenarios) == 0 && len(ch.Decisions) == 0 {
		return nil, nil
	}
	var ds Defects
	// Файл, которого нет в индексе текущего описания, изменение не
	// перезаписывает: это чужой или забытый файл, его судьбу решает человек.
	exists := func(target string) (bool, error) {
		switch _, err := fs.Stat(fsys, target); {
		case err == nil:
			return true, nil
		case errors.Is(err, fs.ErrNotExist):
			return false, nil
		default:
			return false, err
		}
	}
	indexedScenarios := map[string]bool{}
	for _, id := range ch.Current.Manifest.Scenarios {
		indexedScenarios[id] = true
	}
	for _, sc := range ch.Scenarios {
		target := scenarioFile("", sc.ID)
		if !indexedScenarios[sc.ID] {
			found, err := exists(target)
			if err != nil {
				return nil, err
			}
			if found {
				ds = append(ds, Defect{SeverityError, "apply.scenario-exists", target, "",
					"файл сценария уже есть в описании, хотя его нет в индексе: изменение не применяется"})
				continue
			}
		}
		writes[target] = sc.Raw
	}
	indexed := map[string]bool{}
	for _, name := range ch.Current.Manifest.Decisions {
		indexed[name] = true
	}
	for _, d := range ch.Decisions {
		target := adrFile("", d.Name)
		if !indexed[d.Name] {
			found, err := exists(target)
			if err != nil {
				return nil, err
			}
			if found {
				ds = append(ds, Defect{SeverityError, "apply.adr-exists", target, "",
					"файл решения уже есть в описании, хотя его нет в индексе: изменение не применяется"})
				continue
			}
		}
		writes[target] = d.Raw
	}
	cur := map[int]*Decision{}
	for _, d := range ch.Current.decisions {
		cur[d.Number] = d
	}
	for _, d := range ch.Decisions {
		for _, n := range d.Supersedes {
			old := cur[n]
			if old == nil {
				continue
			}
			src := old.Raw
			if prev, ok := writes[old.File]; ok {
				src = prev
			}
			patched, err := patchFrontMatter(src, map[string]string{"status": ADRSuperseded}, map[string]int{"superseded_by": d.Number})
			if err != nil {
				return nil, fmt.Errorf("%s: %w", old.File, err)
			}
			writes[old.File] = patched
		}
	}
	raw, err := fs.ReadFile(fsys, ManifestFile)
	if err != nil {
		return nil, err
	}
	manifest, err := patchManifestIndex(raw, map[string][]string{
		"scenarios": ch.Info.Scenarios,
		"decisions": ch.Info.Decisions,
	})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ManifestFile, err)
	}
	writes[ManifestFile] = manifest
	return ds, nil
}

// patchFrontMatter меняет поля front matter решения на уровне узлов YAML.
// Тело файла остаётся побайтно прежним, front matter пишется с переводом
// строки исходного файла.
func patchFrontMatter(raw []byte, strs map[string]string, ints map[string]int) ([]byte, error) {
	fm, body, eol, ok := splitFrontMatter(raw)
	if !ok {
		return nil, errors.New("нет front matter")
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(fm, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	}
	m := doc.Content[0]
	if m.Kind != yaml.MappingNode {
		return nil, errors.New("front matter должен быть объектом")
	}
	set := func(key, tag, value string) {
		for j := 0; j+1 < len(m.Content); j += 2 {
			if m.Content[j].Value == key {
				v := m.Content[j+1]
				*v = yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value, LineComment: v.LineComment}
				return
			}
		}
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value})
	}
	for _, k := range sortedKeys(strs) {
		set(k, "!!str", strs[k])
	}
	for _, k := range sortedKeys(ints) {
		set(k, "!!int", strconv.Itoa(ints[k]))
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	head := "---\n" + buf.String() + "---\n"
	if eol != "\n" {
		head = strings.ReplaceAll(head, "\n", eol)
	}
	return append([]byte(head), body...), nil
}

// patchManifestIndex дописывает в списки манифеста новые значения в конец,
// создавая ключи при необходимости. Комментарии и порядок сохраняются.
func patchManifestIndex(raw []byte, add map[string][]string) ([]byte, error) {
	doc, _, err := parseYAML(raw)
	if err != nil {
		return nil, err
	}
	if doc == nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("манифест должен быть объектом")
	}
	root := doc.Content[0]
	for _, key := range sortedKeys(add) {
		values := add[key]
		if len(values) == 0 {
			continue
		}
		seq, err := ensureSequence(root, []string{key})
		if err != nil {
			return nil, err
		}
		have := map[string]bool{}
		for _, n := range seq.Content {
			have[n.Value] = true
		}
		for _, v := range values {
			if have[v] {
				continue
			}
			have[v] = true
			seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
		}
		if seq.Style == yaml.FlowStyle {
			seq.Style = 0
		}
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
