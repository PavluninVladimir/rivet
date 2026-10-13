package architecture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"

	yaml "go.yaml.in/yaml/v3"
)

// parseYAML разбирает файл описания в узел YAML и в JSON-значение
// (map[string]any, []any, string, json.Number, bool, nil). Узел нужен
// применению изменения, значение — проверке по JSON Schema.
//
// Разбор строгий: один документ в файле, без якорей, ссылок, ключей
// слияния, повторных ключей и нестандартных тегов. Такие конструкции
// ломают правку узлов при применении изменения и делают дифф в PR
// нечитаемым, поэтому отклоняются сразу. Пустой файл равен пустому
// объекту.
func parseYAML(raw []byte) (*yaml.Node, any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, map[string]any{}, nil
		}
		return nil, nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("строка %d: в файле больше одного документа YAML", extra.Line)
	}
	v, err := yamlValue(&doc)
	if err != nil {
		return nil, nil, err
	}
	if v == nil {
		v = map[string]any{}
	}
	return &doc, v, nil
}

func yamlValue(n *yaml.Node) (any, error) {
	if n.Anchor != "" {
		return nil, fmt.Errorf("строка %d: якоря YAML (&%s) не поддерживаются", n.Line, n.Anchor)
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, nil
		}
		return yamlValue(n.Content[0])
	case yaml.AliasNode:
		return nil, fmt.Errorf("строка %d: ссылки YAML (*%s) не поддерживаются", n.Line, n.Value)
	case yaml.MappingNode:
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, val := n.Content[i], n.Content[i+1]
			if k.Kind != yaml.ScalarNode || k.ShortTag() == "!!merge" {
				return nil, fmt.Errorf("строка %d: ключи слияния и составные ключи YAML не поддерживаются", k.Line)
			}
			if k.ShortTag() != "!!str" {
				return nil, fmt.Errorf("строка %d: ключ %q должен быть строкой", k.Line, k.Value)
			}
			if _, dup := out[k.Value]; dup {
				return nil, fmt.Errorf("строка %d: ключ %q повторяется", k.Line, k.Value)
			}
			v, err := yamlValue(val)
			if err != nil {
				return nil, err
			}
			out[k.Value] = v
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, c := range n.Content {
			v, err := yamlValue(c)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		return scalarValue(n)
	}
	return nil, fmt.Errorf("строка %d: неожиданный узел YAML", n.Line)
}

func scalarValue(n *yaml.Node) (any, error) {
	switch n.ShortTag() {
	case "!!str":
		return n.Value, nil
	case "!!timestamp":
		// Дата без кавычек остаётся строкой: описание не работает с
		// временем, а потеря исходного написания испортила бы дифф.
		return n.Value, nil
	case "!!null":
		return nil, nil
	case "!!bool":
		var b bool
		if err := n.Decode(&b); err != nil {
			return nil, fmt.Errorf("строка %d: %v", n.Line, err)
		}
		return b, nil
	case "!!int":
		var i int64
		if err := n.Decode(&i); err != nil {
			return nil, fmt.Errorf("строка %d: %v", n.Line, err)
		}
		return json.Number(strconv.FormatInt(i, 10)), nil
	case "!!float":
		var f float64
		if err := n.Decode(&f); err != nil {
			return nil, fmt.Errorf("строка %d: %v", n.Line, err)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, fmt.Errorf("строка %d: значение %s не поддерживается", n.Line, n.Value)
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64)), nil
	}
	return nil, fmt.Errorf("строка %d: тег YAML %s не поддерживается", n.Line, n.Tag)
}

// deepCopy копирует JSON-значение, чтобы целевая модель изменения не
// делила массивы и объекты с текущей.
func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = deepCopy(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = deepCopy(val)
		}
		return out
	}
	return v
}
