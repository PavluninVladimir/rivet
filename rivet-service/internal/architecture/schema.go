package architecture

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Схемы файлов описания. Строгость разбора (неизвестные поля — ошибка)
// задаётся в них через additionalProperties: false, как в политике.
//
//go:embed schemas/*.json
var schemaFiles embed.FS

var (
	manifestSchema  = mustBuiltinSchema("manifest.json")
	layerSchema     = mustBuiltinSchema("layer.json")
	viewpointSchema = mustBuiltinSchema("viewpoint.json")
	deltaSchema     = mustBuiltinSchema("delta.json")
	gapSchema       = mustBuiltinSchema("gap.json")
	changeSchema    = mustBuiltinSchema("change.json")
)

// schemaBase — адрес, под которым схемы регистрируются в компиляторе.
// По нему ничего не скачивается: загрузчик внешних ресурсов отключён.
const schemaBase = "https://schemas.rivet.dev/architecture/"

// noLoader запрещает схемам тянуть внешние ресурсы. Собственный viewpoint
// пишет проект, и $ref на файл или URL не должен читать ничего за
// пределами описания.
type noLoader struct{}

func (noLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("внешние ресурсы схем не загружаются: %s", url)
}

func newCompiler() *jsonschema.Compiler {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseLoader(noLoader{})
	return c
}

func mustBuiltinSchema(name string) *jsonschema.Schema {
	raw, err := schemaFiles.ReadFile("schemas/" + name)
	if err != nil {
		panic(err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		panic(fmt.Sprintf("схема %s: %v", name, err))
	}
	sch, err := compileSchema(name, doc)
	if err != nil {
		panic(fmt.Sprintf("схема %s: %v", name, err))
	}
	return sch
}

func compileSchema(name string, doc any) (*jsonschema.Schema, error) {
	c := newCompiler()
	url := schemaBase + name
	if err := c.AddResource(url, doc); err != nil {
		return nil, err
	}
	return c.Compile(url)
}

var schemaPrinter = message.NewPrinter(language.English)

// schemaViolation — нарушение схемы: место в документе и правило.
type schemaViolation struct {
	path    []string
	keyword string
	message string
}

// validateSchema проверяет значение по схеме и возвращает листовые
// нарушения: они точнее сводных и указывают конкретное поле.
func validateSchema(sch *jsonschema.Schema, v any) []schemaViolation {
	err := sch.Validate(v)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []schemaViolation{{message: err.Error()}}
	}
	var out []schemaViolation
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			kw := ""
			if e.ErrorKind != nil {
				kw = strings.Join(e.ErrorKind.KeywordPath(), "/")
			}
			msg := ""
			if e.ErrorKind != nil {
				msg = e.ErrorKind.LocalizedString(schemaPrinter)
			}
			out = append(out, schemaViolation{path: e.InstanceLocation, keyword: kw, message: msg})
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

// reportSchema переводит нарушения схемы в дефекты. place сопоставляет
// месту в документе файл и location: у записей это их идентификатор.
func reportSchema(r *reporter, sch *jsonschema.Schema, v any, place func(path []string) (string, string)) {
	for _, viol := range validateSchema(sch, v) {
		file, loc := place(viol.path)
		r.errorf("schema.violation", file, loc, "нарушено правило схемы %s: %s", viol.keyword, viol.message)
	}
}

// stepKind — вид шага пути к ссылочному полю.
type stepKind int

const (
	stepKey     stepKind = iota // свойство с именем key
	stepIndex                   // элемент массива на позиции n (prefixItems)
	stepItems                   // элементы массива с позиции n и дальше (items)
	stepPattern                 // свойства, подходящие под re (patternProperties)
	stepOther                   // свойства вне properties и patternProperties (additionalProperties)
)

// refStep — шаг пути к ссылочному полю. Шаги повторяют семантику
// ключевых слов схемы, чтобы ссылкой считалось ровно то, что схема
// помечает.
type refStep struct {
	kind  stepKind
	key   string
	n     int
	re    *regexp.Regexp
	known map[string]bool
	res   []*regexp.Regexp
}

func (s refStep) String() string {
	switch s.kind {
	case stepKey:
		return s.key
	case stepIndex:
		return fmt.Sprintf("[%d]", s.n)
	case stepItems:
		if s.n == 0 {
			return "*"
		}
		return fmt.Sprintf("[%d:]", s.n)
	case stepPattern:
		return "~" + s.re.String()
	}
	return "*"
}

func (s refStep) matchIndex(i int) bool {
	switch s.kind {
	case stepIndex:
		return i == s.n
	case stepItems:
		return i >= s.n
	}
	return false
}

func (s refStep) matchKey(k string) bool {
	switch s.kind {
	case stepKey:
		return k == s.key
	case stepPattern:
		return s.re.MatchString(k)
	case stepOther:
		if s.known[k] {
			return false
		}
		for _, re := range s.res {
			if re.MatchString(k) {
				return false
			}
		}
		return true
	}
	return false
}

// matchToken сопоставляет шаг с токеном JSON Pointer.
func (s refStep) matchToken(tok string) bool {
	if i, err := strconv.Atoi(tok); err == nil && i >= 0 && s.matchIndex(i) {
		return true
	}
	return s.matchKey(tok)
}

// id — ключ шага для сравнения путей. В отличие от String учитывает
// исключения additionalProperties и кодируется JSON'ом, чтобы имена с
// запятыми и слэшами не склеивали разные пути.
func (s refStep) id() string {
	v := []any{s.kind, s.key, s.n}
	if s.re != nil {
		v = append(v, s.re.String())
	}
	if s.kind == stepOther {
		res := make([]string, len(s.res))
		for i, re := range s.res {
			res[i] = re.String()
		}
		v = append(v, sortedKeys(s.known), res)
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// refScan — результат поиска пометок x-rivet-ref в схеме.
type refScan struct {
	paths [][]refStep
	// conditional — пометки в условных ветках, пути для сообщения.
	conditional []string
	// unsupported — $ref, по которым пометки не найти: их цель
	// определяется не JSON Pointer'ом и не $anchor.
	unsupported []string
}

// refPaths находит в JSON Schema строки, помеченные x-rivet-ref: true, и
// возвращает пути к ним в документе. Обходятся properties, prefixItems,
// items, patternProperties, additionalProperties, allOf и локальные $ref:
// эти ключевые слова применяются к документу безусловно.
//
// Пометка внутри условных веток (if/then/else, anyOf, oneOf, not,
// contains, dependentSchemas) возвращается вторым результатом: применима
// ли ветка, зависит от значения, и детерминированно понять, ссылка ли это,
// нельзя. Такой viewpoint отклоняется при разборе.
func refPaths(schema any) refScan {
	var out refScan
	seen := map[string]bool{}
	anchors := schemaAnchors(schema)
	var walk func(s any, path []refStep, cond bool, depth int)
	walk = func(s any, path []refStep, cond bool, depth int) {
		m, ok := s.(map[string]any)
		if !ok || depth > 32 {
			return
		}
		if b, _ := m["x-rivet-ref"].(bool); b {
			keys := make([]string, len(path))
			names := make([]string, len(path))
			for i, st := range path {
				keys[i], names[i] = st.id(), st.String()
			}
			raw, _ := json.Marshal(keys)
			switch key := string(raw); {
			case cond:
				out.conditional = append(out.conditional, "/"+strings.Join(names, "/"))
			case !seen[key]:
				seen[key] = true
				out.paths = append(out.paths, slices.Clone(path))
			}
		}
		with := func(st refStep) []refStep {
			return append(slices.Clone(path), st)
		}
		props, _ := m["properties"].(map[string]any)
		known := map[string]bool{}
		for _, k := range sortedKeys(props) {
			known[k] = true
			walk(props[k], with(refStep{kind: stepKey, key: k}), cond, depth+1)
		}
		prefix := 0
		if list, ok := m["prefixItems"].([]any); ok {
			prefix = len(list)
			for i, sub := range list {
				walk(sub, with(refStep{kind: stepIndex, n: i}), cond, depth+1)
			}
		}
		walk(m["items"], with(refStep{kind: stepItems, n: prefix}), cond, depth+1)
		walk(m["contains"], with(refStep{kind: stepItems}), true, depth+1)
		var res []*regexp.Regexp
		if pp, ok := m["patternProperties"].(map[string]any); ok {
			for _, k := range sortedKeys(pp) {
				re, err := regexp.Compile(k)
				if err != nil {
					continue // некорректный шаблон отклонит компилятор схемы
				}
				res = append(res, re)
				walk(pp[k], with(refStep{kind: stepPattern, re: re}), cond, depth+1)
			}
		}
		walk(m["additionalProperties"], with(refStep{kind: stepOther, known: known, res: res}), cond, depth+1)
		if list, ok := m["allOf"].([]any); ok {
			for _, sub := range list {
				walk(sub, path, cond, depth+1)
			}
		}
		for _, kw := range []string{"anyOf", "oneOf"} {
			if list, ok := m[kw].([]any); ok {
				for _, sub := range list {
					walk(sub, path, true, depth+1)
				}
			}
		}
		for _, kw := range []string{"if", "then", "else", "not"} {
			walk(m[kw], path, true, depth+1)
		}
		if ds, ok := m["dependentSchemas"].(map[string]any); ok {
			for _, k := range sortedKeys(ds) {
				walk(ds[k], path, true, depth+1)
			}
		}
		if ref, ok := m["$ref"].(string); ok {
			if target := resolveLocalRef(schema, anchors, ref); target != nil {
				walk(target, path, cond, depth+1)
			} else {
				out.unsupported = append(out.unsupported, "$ref "+ref)
			}
		}
		if ref, ok := m["$dynamicRef"].(string); ok {
			out.unsupported = append(out.unsupported, "$dynamicRef "+ref)
		}
	}
	walk(schema, nil, false, 0)
	return out
}

// resolveLocalRef находит цель локального $ref: JSON Pointer (#/$defs/x)
// или якорь (#name). nil — цель так не находится.
func resolveLocalRef(schema any, anchors map[string]any, ref string) any {
	frag, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return nil
	}
	if frag == "" || strings.HasPrefix(frag, "/") {
		toks, err := parsePointer(frag)
		if err != nil {
			return nil
		}
		target, ok := valueAt(schema, toks)
		if !ok {
			return nil
		}
		return target
	}
	return anchors[frag]
}

// schemaAnchors собирает $anchor схемы. Значения const, default, enum и
// examples — данные, а не схемы, и не обходятся. Повторный якорь
// неоднозначен: ссылка на него считается неподдерживаемой.
func schemaAnchors(schema any) map[string]any {
	out := map[string]any{}
	dup := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if name, ok := x["$anchor"].(string); ok {
				if _, seen := out[name]; seen {
					dup[name] = true
				}
				out[name] = x
			}
			for _, k := range sortedKeys(x) {
				switch k {
				case "const", "default", "enum", "examples":
					continue
				}
				walk(x[k])
			}
		case []any:
			for _, item := range x {
				walk(item)
			}
		}
	}
	walk(schema)
	for name := range dup {
		delete(out, name)
	}
	return out
}

// refHit — найденная в документе ссылка на элемент модели.
type refHit struct {
	path  []string
	value string
}

// collectRefs собирает строки документа по пути из refPaths.
func collectRefs(doc any, steps []refStep) []refHit {
	var out []refHit
	var walk func(v any, rest []refStep, path []string)
	walk = func(v any, rest []refStep, path []string) {
		if len(rest) == 0 {
			if s, ok := v.(string); ok {
				out = append(out, refHit{path: slices.Clone(path), value: s})
			}
			return
		}
		st := rest[0]
		switch x := v.(type) {
		case []any:
			for i, val := range x {
				if st.matchIndex(i) {
					walk(val, rest[1:], append(path, strconv.Itoa(i)))
				}
			}
		case map[string]any:
			for _, k := range sortedKeys(x) {
				if st.matchKey(k) {
					walk(x[k], rest[1:], append(path, k))
				}
			}
		}
	}
	walk(doc, steps, nil)
	return out
}

// parsePointer разбирает JSON Pointer (RFC 6901). Пустая строка — корень.
func parsePointer(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("JSON Pointer %q должен начинаться с /", p)
	}
	parts := strings.Split(p[1:], "/")
	for i, s := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

func formatPointer(tokens []string) string {
	if len(tokens) == 0 {
		return "/"
	}
	var b strings.Builder
	for _, t := range tokens {
		b.WriteByte('/')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(t, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

// valueAt возвращает значение по пути в JSON-значении.
func valueAt(v any, tokens []string) (any, bool) {
	for _, t := range tokens {
		switch x := v.(type) {
		case map[string]any:
			val, ok := x[t]
			if !ok {
				return nil, false
			}
			v = val
		case []any:
			i, err := strconv.Atoi(t)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}

// setAt записывает значение по пути: недостающие объекты создаются,
// элементы массивов адресуются индексом. false — путь несовместим с
// документом.
func setAt(root map[string]any, tokens []string, val any) bool {
	var cur any = root
	for i, t := range tokens {
		last := i == len(tokens)-1
		switch x := cur.(type) {
		case map[string]any:
			if last {
				x[t] = val
				return true
			}
			next := x[t]
			if next == nil {
				next = map[string]any{}
				x[t] = next
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(t)
			if err != nil || idx < 0 || idx >= len(x) {
				return false
			}
			if last {
				x[idx] = val
				return true
			}
			cur = x[idx]
		default:
			return false
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
