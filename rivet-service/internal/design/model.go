// Package design implements the versioned, data-only design document format.
// Neither imports nor comments can supply CSS, HTML, scripts or network URLs.
package design

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const Schema = "rivet.design/v1"
const MaxDocumentBytes = 2 << 20
const MaxPatchBytes = 1 << 20
const MaxAssetBytes = 10 << 20
const MaxNodes = 2000

type Error struct {
	Code            string `json:"code"`
	Path            string `json:"path,omitempty"`
	Message         string `json:"message"`
	CurrentRevision int    `json:"current_revision,omitempty"`
}

func (e *Error) Error() string             { return e.Code + ": " + e.Message }
func Invalid(code, path, msg string) error { return &Error{Code: code, Path: path, Message: msg} }

type Diagnostic struct {
	Code    string `json:"code"`
	Path    string `json:"path"`
	Message string `json:"message"`
}
type BaseRef struct {
	DocumentID string `json:"document_id"`
	Revision   int    `json:"revision"`
	ContentSHA string `json:"content_sha"`
}
type Properties map[string]json.RawMessage
type Token struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}
type Variant struct {
	State string     `json:"state"`
	Props Properties `json:"props"`
}
type Node struct {
	ID          string     `json:"id"`
	Type        string     `json:"type"`
	Props       Properties `json:"props"`
	Children    []Node     `json:"children"`
	ComponentID string     `json:"component_id,omitempty"`
}
type Component struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Root     Node      `json:"root"`
	Variants []Variant `json:"variants"`
}
type Viewport struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}
type Screen struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Viewport Viewport `json:"viewport"`
	Root     Node     `json:"root"`
}
type Override struct {
	InstanceID string          `json:"instance_id"`
	Property   string          `json:"property"`
	Value      json.RawMessage `json:"value"`
}
type Model struct {
	Schema     string      `json:"schema"`
	Tokens     []Token     `json:"tokens"`
	Components []Component `json:"components"`
	Screens    []Screen    `json:"screens"`
	Base       *BaseRef    `json:"base"`
	Overrides  []Override  `json:"overrides"`
}
type Patch struct {
	Op          string          `json:"op"`
	ElementID   string          `json:"element_id,omitempty"`
	Property    string          `json:"property,omitempty"`
	Value       json.RawMessage `json:"value,omitempty"`
	Scope       string          `json:"scope,omitempty"`
	TokenID     string          `json:"token_id,omitempty"`
	ComponentID string          `json:"component_id,omitempty"`
	Component   *Component      `json:"component,omitempty"`
	Base        *BaseRef        `json:"base,omitempty"`
	ParentID    string          `json:"parent_id,omitempty"`
	Node        *Node           `json:"node,omitempty"`
	Index       *int            `json:"index,omitempty"`
}
type Diff struct {
	BaseRevision       int          `json:"base_revision"`
	Operations         []Patch      `json:"operations"`
	AffectedElementIDs []string     `json:"affected_element_ids"`
	Conflicts          []Diagnostic `json:"conflicts"`
}

func Strict(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return Invalid("unsupported_property", "$", err.Error())
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Invalid("unsupported_schema", "$", "ожидается один JSON объект")
	}
	return nil
}
func Parse(raw []byte) (Model, error) {
	var m Model
	if !utf8.Valid(raw) {
		return m, Invalid("unsupported_schema", "$", "JSON должен быть UTF-8")
	}
	if len(raw) > MaxDocumentBytes {
		return m, Invalid("size_limit", "$", "документ больше 2 МиБ")
	}
	if err := Strict(raw, &m); err != nil {
		return m, err
	}
	return m, Validate(m)
}
func validID(id string) bool {
	u, e := uuid.Parse(id)
	return e == nil && u.String() == id && u != uuid.Nil
}
func ValidID(id string) bool { return validID(id) }
func ValidSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil && strings.ToLower(s) == s
}
func name(s string) bool { return len(strings.TrimSpace(s)) > 0 && len([]rune(s)) <= 200 }

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}([0-9a-fA-F]{2})?$`)
var families = map[string]bool{"sans-serif": true, "serif": true, "monospace": true, "Arial": true, "Inter": true, "Roboto": true, "system-ui": true}

func number(raw json.RawMessage, min, max float64) bool {
	var n float64
	return string(bytes.TrimSpace(raw)) != "null" && json.Unmarshal(raw, &n) == nil && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= min && n <= max
}
func ref(raw json.RawMessage) (string, bool) {
	var r struct {
		TokenID string `json:"token_id"`
	}
	err := Strict(raw, &r)
	return r.TokenID, err == nil && validID(r.TokenID)
}
func validateProps(p Properties, tokens map[string]string, path string) error {
	if p == nil {
		return Invalid("unsupported_schema", path, "props обязателен")
	}
	for k, v := range p {
		if string(bytes.TrimSpace(v)) == "null" {
			return Invalid("unsupported_property", path+"."+k, "null не является значением свойства")
		}
		good := false
		switch k {
		case "text", "placeholder":
			var s string
			good = json.Unmarshal(v, &s) == nil && len([]rune(s)) <= 8000
		case "color", "background", "border_color":
			var s string
			if json.Unmarshal(v, &s) == nil {
				good = hexColor.MatchString(s)
			} else {
				id, ok := ref(v)
				good = ok && tokens[id] == "color"
			}
		case "typography":
			id, ok := ref(v)
			good = ok && tokens[id] == "typography"
		case "padding", "gap":
			if id, ok := ref(v); ok {
				good = tokens[id] == "spacing"
			} else {
				good = number(v, 0, 1024)
			}
		case "width", "height":
			good = number(v, 0, 4096)
		case "font_size":
			good = number(v, 6, 256)
		case "font_weight":
			good = number(v, 100, 900)
		case "line_height":
			good = number(v, 0.5, 4)
		case "font_family":
			var s string
			good = json.Unmarshal(v, &s) == nil && families[s]
		case "border", "radius":
			good = number(v, 0, 1024)
		case "alignment":
			var s string
			good = json.Unmarshal(v, &s) == nil && (s == "start" || s == "center" || s == "end" || s == "stretch")
		case "direction":
			var s string
			good = json.Unmarshal(v, &s) == nil && (s == "row" || s == "column")
		case "asset_id":
			var s string
			good = json.Unmarshal(v, &s) == nil && validID(s)
		case "disabled":
			var b bool
			good = json.Unmarshal(v, &b) == nil
		default:
			return Invalid("unsupported_property", path+"."+k, "неподдерживаемое свойство")
		}
		if !good {
			return Invalid("invalid_ref", path+"."+k, "неверное значение или ссылка")
		}
	}
	return nil
}
func Validate(m Model) error {
	if m.Schema != Schema {
		return Invalid("unsupported_schema", "schema", "поддерживается "+Schema)
	}
	if m.Tokens == nil || m.Components == nil || m.Screens == nil || m.Overrides == nil {
		return Invalid("unsupported_schema", "$", "tokens/components/screens/overrides обязательны")
	}
	if m.Base != nil && (!validID(m.Base.DocumentID) || m.Base.Revision < 1 || !ValidSHA(m.Base.ContentSHA)) {
		return Invalid("invalid_ref", "base", "неверная версия базы")
	}
	ids := map[string]bool{}
	tokens := map[string]string{}
	components := map[string]Component{}
	nodes := map[string]Node{}
	count := 0
	add := func(id, path string) error {
		if !validID(id) || ids[id] {
			return Invalid("invalid_ref", path, "UUID должен быть уникальным и ненулевым")
		}
		ids[id] = true
		return nil
	}
	for i, t := range m.Tokens {
		p := fmt.Sprintf("tokens[%d]", i)
		if err := add(t.ID, p+".id"); err != nil {
			return err
		}
		if !name(t.Name) {
			return Invalid("unsupported_schema", p+".name", "неверное имя")
		}
		tokens[t.ID] = t.Type
		switch t.Type {
		case "color":
			var v struct {
				Hex string `json:"hex"`
			}
			if Strict(t.Value, &v) != nil || !hexColor.MatchString(v.Hex) {
				return Invalid("unsupported_property", p+".value", "ожидается hex цвет")
			}
		case "spacing":
			var v struct {
				PX *float64 `json:"px"`
			}
			if Strict(t.Value, &v) != nil || v.PX == nil || !number(json.RawMessage(fmt.Sprint(*v.PX)), 0, 1024) {
				return Invalid("unsupported_property", p+".value", "неверный отступ")
			}
		case "typography":
			var v struct {
				FontFamily string  `json:"font_family"`
				FontSize   float64 `json:"font_size"`
				FontWeight int     `json:"font_weight"`
				LineHeight float64 `json:"line_height"`
			}
			if Strict(t.Value, &v) != nil || !families[v.FontFamily] || v.FontSize < 6 || v.FontSize > 256 || v.FontWeight < 100 || v.FontWeight > 900 || v.LineHeight < 0.5 || v.LineHeight > 4 {
				return Invalid("unsupported_property", p+".value", "неверная типографика")
			}
		default:
			return Invalid("unsupported_schema", p+".type", "неизвестный тип token")
		}
	}
	var walk func(Node, string, int) error
	walk = func(n Node, p string, depth int) error {
		count++
		if count > MaxNodes || depth > 64 {
			return Invalid("size_limit", p, "превышен предел узлов или глубины")
		}
		if err := add(n.ID, p+".id"); err != nil {
			return err
		}
		nodes[n.ID] = n
		switch n.Type {
		case "container", "text", "button", "input", "image", "instance":
		default:
			return Invalid("unsupported_schema", p+".type", "неизвестный тип элемента")
		}
		if n.Children == nil {
			return Invalid("unsupported_schema", p+".children", "children обязателен")
		}
		if n.Type != "container" && n.Type != "instance" && len(n.Children) > 0 {
			return Invalid("unsupported_schema", p, "только container/instance содержат дочерние элементы")
		}
		if n.Type == "instance" {
			if !validID(n.ComponentID) {
				return Invalid("invalid_ref", p+".component_id", "нужен component UUID")
			}
		} else if n.ComponentID != "" {
			return Invalid("unsupported_property", p+".component_id", "только для instance")
		}
		if err := validateProps(n.Props, tokens, p+".props"); err != nil {
			return err
		}
		for i, c := range n.Children {
			if err := walk(c, fmt.Sprintf("%s.children[%d]", p, i), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for i, c := range m.Components {
		p := fmt.Sprintf("components[%d]", i)
		if err := add(c.ID, p+".id"); err != nil {
			return err
		}
		if !name(c.Name) || c.Variants == nil {
			return Invalid("unsupported_schema", p, "нужны name и variants")
		}
		components[c.ID] = c
		states := map[string]bool{}
		for _, v := range c.Variants {
			if states[v.State] {
				return Invalid("unsupported_schema", p+".variants", "повтор состояния")
			}
			states[v.State] = true
			switch v.State {
			case "default", "hover", "focus", "disabled", "error":
			default:
				return Invalid("unsupported_schema", p+".variants", "неизвестное состояние")
			}
			if err := validateProps(v.Props, tokens, p+".variants.props"); err != nil {
				return err
			}
		}
		if err := walk(c.Root, p+".root", 0); err != nil {
			return err
		}
	}
	for i, s := range m.Screens {
		p := fmt.Sprintf("screens[%d]", i)
		if err := add(s.ID, p+".id"); err != nil {
			return err
		}
		if !name(s.Name) || s.Viewport.Width < 1 || s.Viewport.Height < 1 || s.Viewport.Width > 4096 || s.Viewport.Height > 4096 {
			return Invalid("unsupported_schema", p, "неверные name/viewport")
		}
		if err := walk(s.Root, p+".root", 0); err != nil {
			return err
		}
	}
	for _, n := range nodes {
		if n.Type == "instance" {
			if _, ok := components[n.ComponentID]; !ok {
				return Invalid("invalid_ref", n.ID, "компонент отсутствует")
			}
		}
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var comp func(string) error
	comp = func(id string) error {
		if visiting[id] {
			return Invalid("cycle", id, "цикл компонентов")
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		var scan func(Node) error
		scan = func(n Node) error {
			if n.Type == "instance" {
				if err := comp(n.ComponentID); err != nil {
					return err
				}
			}
			for _, c := range n.Children {
				if err := scan(c); err != nil {
					return err
				}
			}
			return nil
		}
		if err := scan(components[id].Root); err != nil {
			return err
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	for id := range components {
		if err := comp(id); err != nil {
			return err
		}
	}
	overrides := map[string]bool{}
	for _, o := range m.Overrides {
		n, ok := nodes[o.InstanceID]
		key := o.InstanceID + "/" + o.Property
		if !ok || n.Type != "instance" || overrides[key] {
			return Invalid("invalid_ref", "overrides", "неизвестный instance или повтор override")
		}
		overrides[key] = true
		if err := validateProps(Properties{o.Property: o.Value}, tokens, "overrides"); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(raw) > MaxDocumentBytes {
		return Invalid("size_limit", "$", "документ больше 2 МиБ")
	}
	return nil
}

// Canonical fixes object key order and normalizes numbers and insignificant whitespace.
// Arrays retain semantic order, including screen and child stacking order.
func Canonical(v any) ([]byte, string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, "", err
	}
	var normalized any
	if err = json.Unmarshal(raw, &normalized); err != nil {
		return nil, "", err
	}
	raw, err = json.Marshal(normalized)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(raw)
	return raw, hex.EncodeToString(sum[:]), nil
}
func NodeIDs(m Model) map[string]bool {
	out := map[string]bool{}
	var walk func(Node)
	walk = func(n Node) {
		out[n.ID] = true
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, c := range m.Components {
		walk(c.Root)
	}
	for _, s := range m.Screens {
		walk(s.Root)
	}
	return out
}
func AssetIDs(m Model) []string {
	ids := map[string]bool{}
	props := func(p Properties) {
		if r := p["asset_id"]; r != nil {
			var s string
			_ = json.Unmarshal(r, &s)
			ids[s] = true
		}
	}
	var walk func(Node)
	walk = func(n Node) {
		props(n.Props)
		for _, c := range n.Children {
			walk(c)
		}
	}
	for _, s := range m.Screens {
		walk(s.Root)
	}
	for _, c := range m.Components {
		walk(c.Root)
		for _, v := range c.Variants {
			props(v.Props)
		}
	}
	for _, o := range m.Overrides {
		props(Properties{o.Property: o.Value})
	}
	out := []string{}
	for id := range ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// IdentityIDs includes definitions as well as selectable nodes. Deleted token,
// screen and component UUIDs cannot be recycled by a later import either.
func IdentityIDs(m Model) map[string]bool {
	out := NodeIDs(m)
	for _, t := range m.Tokens {
		out[t.ID] = true
	}
	for _, c := range m.Components {
		out[c.ID] = true
	}
	for _, s := range m.Screens {
		out[s.ID] = true
	}
	return out
}
