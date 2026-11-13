package design

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

func clone(m Model) Model {
	raw, _ := json.Marshal(m)
	var out Model
	_ = json.Unmarshal(raw, &out)
	return out
}
func eachNode(m *Model, fn func(*Node)) {
	var visit func(*Node)
	visit = func(n *Node) {
		fn(n)
		for i := range n.Children {
			visit(&n.Children[i])
		}
	}
	for i := range m.Components {
		visit(&m.Components[i].Root)
	}
	for i := range m.Screens {
		visit(&m.Screens[i].Root)
	}
}
func findNode(m *Model, id string) *Node {
	var out *Node
	eachNode(m, func(n *Node) {
		if n.ID == id {
			out = n
		}
	})
	return out
}
func same(a, b any) bool {
	ar, _, _ := Canonical(a)
	br, _, _ := Canonical(b)
	return string(ar) == string(br)
}
func rawRefs(v json.RawMessage, id string) bool {
	var r struct {
		TokenID string `json:"token_id"`
	}
	return json.Unmarshal(v, &r) == nil && r.TokenID == id
}
func markConsumers(m *Model, p Patch, out map[string]bool) {
	eachNode(m, func(n *Node) {
		switch p.Op {
		case "set_token":
			for k, v := range n.Props {
				if rawRefs(v, p.TokenID) {
					overridden := false
					for _, o := range m.Overrides {
						if o.InstanceID == n.ID && o.Property == k {
							overridden = true
						}
					}
					if !overridden {
						out[n.ID] = true
					}
				}
			}
		case "update_component":
			if n.ComponentID == p.ComponentID {
				out[n.ID] = true
			}
		case "update_base":
			out[n.ID] = true
		}
	})
	// A token in a component definition affects its instances, including nested
	// definitions. Bounded propagation covers a DAG without expanding its tree.
	for changed := true; changed; {
		changed = false
		affectedComponents := map[string]bool{}
		for _, c := range m.Components {
			ids := NodeIDs(Model{Components: []Component{c}})
			for id := range ids {
				if out[id] {
					affectedComponents[c.ID] = true
					break
				}
			}
			if p.Op == "set_token" {
				for _, variant := range c.Variants {
					for _, v := range variant.Props {
						if rawRefs(v, p.TokenID) {
							affectedComponents[c.ID] = true
						}
					}
				}
			}
		}
		eachNode(m, func(n *Node) {
			if n.Type == "instance" && affectedComponents[n.ComponentID] && !out[n.ID] {
				out[n.ID] = true
				changed = true
			}
		})
	}
}

func Apply(m Model, revision int, ops []Patch) (Model, Diff, error) {
	d := Diff{BaseRevision: revision, Operations: ops, AffectedElementIDs: []string{}, Conflicts: []Diagnostic{}}
	if len(ops) == 0 || len(ops) > 100 {
		return m, d, Invalid("size_limit", "operations", "нужно 1–100 операций")
	}
	b, _ := json.Marshal(ops)
	if len(b) > MaxPatchBytes {
		return m, d, Invalid("size_limit", "operations", "patch больше 1 МиБ")
	}
	result := clone(m)
	affected := map[string]bool{}
	for i, p := range ops {
		path := fmt.Sprintf("operations[%d]", i)
		markConsumers(&result, p, affected)
		switch p.Op {
		case "set_property":
			if p.Scope != "instance" || p.Property == "" || len(p.Value) == 0 {
				return m, d, Invalid("unsupported_property", path, "set_property требует scope instance, property и value")
			}
			for _, component := range result.Components {
				if NodeIDs(Model{Components: []Component{component}})[p.ElementID] {
					return m, d, Invalid("unsupported_property", path, "component definition требует update_component")
				}
			}
			n := findNode(&result, p.ElementID)
			if n == nil {
				return m, d, Invalid("invalid_ref", path, "элемент отсутствует")
			}
			n.Props[p.Property] = p.Value
			affected[n.ID] = true
			if n.Type == "instance" {
				updated := false
				for i := range result.Overrides {
					if result.Overrides[i].InstanceID == n.ID && result.Overrides[i].Property == p.Property {
						result.Overrides[i].Value = p.Value
						updated = true
					}
				}
				if !updated {
					result.Overrides = append(result.Overrides, Override{n.ID, p.Property, p.Value})
				}
			}
		case "set_token":
			found := false
			for j := range result.Tokens {
				if result.Tokens[j].ID == p.TokenID {
					result.Tokens[j].Value = p.Value
					found = true
				}
			}
			if !found {
				return m, d, Invalid("invalid_ref", path, "token отсутствует")
			}
		case "update_component":
			if p.Component == nil || p.Component.ID != p.ComponentID {
				return m, d, Invalid("invalid_ref", path, "component ID не совпадает")
			}
			found := false
			for j := range result.Components {
				if result.Components[j].ID == p.ComponentID {
					before := NodeIDs(Model{Components: []Component{result.Components[j]}})
					after := NodeIDs(Model{Components: []Component{*p.Component}})
					if result.Components[j].Root.ID != p.Component.Root.ID {
						return m, d, Invalid("invalid_ref", path, "root ID существующего компонента неизменен")
					}
					for id := range before {
						if !after[id] {
							affected[id] = true
						}
					}
					result.Components[j] = *p.Component
					found = true
				}
			}
			if !found {
				return m, d, Invalid("invalid_ref", path, "component отсутствует")
			}
		case "update_base":
			result.Base = p.Base
		case "add_node":
			if p.Node == nil || p.Index == nil {
				return m, d, Invalid("unsupported_property", path, "node/index обязательны")
			}
			n := findNode(&result, p.ParentID)
			if n == nil || n.Type != "container" || *p.Index < 0 || *p.Index > len(n.Children) {
				return m, d, Invalid("invalid_ref", path, "неверный parent/index")
			}
			n.Children = append(n.Children, Node{})
			copy(n.Children[*p.Index+1:], n.Children[*p.Index:])
			n.Children[*p.Index] = *p.Node
			affected[p.Node.ID] = true
		case "remove_node":
			removed := false
			eachNode(&result, func(n *Node) {
				for j := range n.Children {
					if n.Children[j].ID == p.ElementID {
						n.Children = append(n.Children[:j], n.Children[j+1:]...)
						removed = true
						break
					}
				}
			})
			if !removed {
				return m, d, Invalid("invalid_ref", path, "нельзя удалить отсутствующий элемент или root")
			}
			affected[p.ElementID] = true
			remaining := NodeIDs(result)
			o := []Override{}
			for _, v := range result.Overrides {
				if remaining[v.InstanceID] {
					o = append(o, v)
				}
			}
			result.Overrides = o
		default:
			return m, d, Invalid("unsupported_property", path+".op", "неподдерживаемая операция")
		}
	}
	if err := Validate(result); err != nil {
		return m, d, err
	}
	for id := range affected {
		d.AffectedElementIDs = append(d.AffectedElementIDs, id)
	}
	sort.Strings(d.AffectedElementIDs)
	return result, d, nil
}

// Inverse performs a conservative three-way undo. A later conflicting edit is
// never overwritten; unchanged fields are preserved rather than rewinding state.
func Inverse(current, before, after Model) (Model, error) {
	var c, b, a any
	for _, x := range []struct {
		m Model
		v *any
	}{{current, &c}, {before, &b}, {after, &a}} {
		raw, _ := json.Marshal(x.m)
		_ = json.Unmarshal(raw, x.v)
	}
	var undo func(any, any, any) (any, error)
	undo = func(c, b, a any) (any, error) {
		if reflect.DeepEqual(a, b) {
			return c, nil
		}
		if reflect.DeepEqual(c, a) {
			return b, nil
		}
		cm, cok := c.(map[string]any)
		bm, bok := b.(map[string]any)
		am, aok := a.(map[string]any)
		if !cok || !bok || !aok {
			return nil, Invalid("undo_conflict", "$", "более поздняя правка затронула отменяемые данные")
		}
		keys := map[string]bool{}
		for k := range bm {
			keys[k] = true
		}
		for k := range am {
			keys[k] = true
		}
		for k := range keys {
			v, e := undo(cm[k], bm[k], am[k])
			if e != nil {
				return nil, e
			}
			if v == nil {
				delete(cm, k)
			} else {
				cm[k] = v
			}
		}
		return cm, nil
	}
	value, e := undo(c, b, a)
	if e != nil {
		return Model{}, e
	}
	raw, _ := json.Marshal(value)
	return Parse(raw)
}

// Rebase updates inherited entries only where the local value still matches the
// pinned base. Explicit local edits and instance overrides remain unchanged.
func Rebase(current, oldBase, newBase Model) (Model, error) {
	out := clone(current)
	merge := func(local, old, new any, out any) error {
		raw, _ := json.Marshal(local)
		var ls []map[string]any
		_ = json.Unmarshal(raw, &ls)
		raw, _ = json.Marshal(old)
		var os []map[string]any
		_ = json.Unmarshal(raw, &os)
		raw, _ = json.Marshal(new)
		var ns []map[string]any
		_ = json.Unmarshal(raw, &ns)
		om, nm := map[string]map[string]any{}, map[string]map[string]any{}
		for _, v := range os {
			om[v["id"].(string)] = v
		}
		for _, v := range ns {
			nm[v["id"].(string)] = v
		}
		merged := []map[string]any{}
		seen := map[string]bool{}
		for _, v := range ls {
			id := v["id"].(string)
			seen[id] = true
			if o, ok := om[id]; ok && same(v, o) {
				if n, exists := nm[id]; exists {
					merged = append(merged, n)
				}
			} else {
				merged = append(merged, v)
			}
		}
		for _, v := range ns {
			id := v["id"].(string)
			if !seen[id] {
				if _, wasInherited := om[id]; !wasInherited {
					merged = append(merged, v)
				}
			}
		}
		raw, _ = json.Marshal(merged)
		return json.Unmarshal(raw, out)
	}
	if err := merge(current.Tokens, oldBase.Tokens, newBase.Tokens, &out.Tokens); err != nil {
		return Model{}, err
	}
	if err := merge(current.Components, oldBase.Components, newBase.Components, &out.Components); err != nil {
		return Model{}, err
	}
	if err := merge(current.Screens, oldBase.Screens, newBase.Screens, &out.Screens); err != nil {
		return Model{}, err
	}
	return out, Validate(out)
}

// Reject fields belonging to another operation instead of silently ignoring
// them. This also enforces required keys even when their value is zero/null.
func (p *Patch) UnmarshalJSON(raw []byte) error {
	type plain Patch
	var v plain
	if err := Strict(raw, &v); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	keys := map[string][]string{"set_property": {"element_id", "property", "value", "scope"}, "set_token": {"token_id", "value"}, "update_component": {"component_id", "component"}, "update_base": {"base"}, "add_node": {"parent_id", "node", "index"}, "remove_node": {"element_id"}}
	required, ok := keys[v.Op]
	if !ok {
		return Invalid("unsupported_property", "op", "неподдерживаемая операция")
	}
	allowed := map[string]bool{"op": true}
	for _, k := range required {
		allowed[k] = true
		if _, exists := fields[k]; !exists {
			return Invalid("unsupported_property", k, "обязательное поле операции")
		}
	}
	for k := range fields {
		if !allowed[k] {
			return Invalid("unsupported_property", k, "поле не относится к операции")
		}
	}
	*p = Patch(v)
	return nil
}

func (p Patch) MarshalJSON() ([]byte, error) {
	v := map[string]any{"op": p.Op}
	switch p.Op {
	case "set_property":
		v["element_id"] = p.ElementID
		v["property"] = p.Property
		v["value"] = p.Value
		v["scope"] = p.Scope
	case "set_token":
		v["token_id"] = p.TokenID
		v["value"] = p.Value
	case "update_component":
		v["component_id"] = p.ComponentID
		v["component"] = p.Component
	case "update_base":
		v["base"] = p.Base
	case "add_node":
		v["parent_id"] = p.ParentID
		v["node"] = p.Node
		v["index"] = p.Index
	case "remove_node":
		v["element_id"] = p.ElementID
	default:
		return nil, Invalid("unsupported_property", "op", "неподдерживаемая операция")
	}
	return json.Marshal(v)
}
