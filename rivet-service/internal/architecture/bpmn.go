package architecture

import (
	"encoding/xml"
	"fmt"
	"slices"
	"strings"
)

// Пространства имён BPMN 2.0 и ссылок на модель.
const (
	bpmnNS  = "http://www.omg.org/spec/BPMN/20100524/MODEL"
	RivetNS = "urn:rivet:architecture:1"
)

// Process — бизнес-процесс в BPMN 2.0. Разбирается подмножество: события,
// задачи, исключающие и параллельные шлюзы, потоки и дорожки. Остальное
// даёт предупреждение и участвует в обходе графа как непрозрачный узел.
type Process struct {
	ID    string // идентификатор элемента process модели
	File  string
	Raw   []byte
	Name  string
	Lanes []Lane
	Nodes []FlowNode
	Flows []SequenceFlow

	parsed  bool
	defects []Defect // дефекты разбора: от модели не зависят
}

// Lane — дорожка процесса. Ref ссылается на роль или актора модели.
type Lane struct {
	ID, Name, Ref string
	Nodes         []string
	Lanes         []Lane
}

// FlowNode — узел процесса.
type FlowNode struct {
	ID, Name, Kind string
	Ref            string // rivet:ref
	Direction      string // gatewayDirection у шлюзов
	AttachedTo     string // attachedToRef у граничных событий
	Supported      bool
}

// SequenceFlow — поток управления.
type SequenceFlow struct {
	ID, Name, Source, Target string
}

var (
	bpmnEvents   = []string{"startEvent", "endEvent", "intermediateCatchEvent", "intermediateThrowEvent"}
	bpmnTasks    = []string{"task", "userTask", "serviceTask", "manualTask"}
	bpmnGateways = []string{"exclusiveGateway", "parallelGateway"}
	// Узлы вне подмножества: предупреждение, но в обходе графа участвуют,
	// иначе потоки через них дали бы ложные тупики.
	bpmnOpaque = []string{"subProcess", "adHocSubProcess", "transaction", "callActivity", "sendTask",
		"receiveTask", "scriptTask", "businessRuleTask", "inclusiveGateway", "eventBasedGateway",
		"complexGateway", "boundaryEvent"}
	bpmnSilent = []string{"documentation", "extensionElements"}
)

func supportedNode(kind string) bool {
	return slices.Contains(bpmnEvents, kind) || slices.Contains(bpmnTasks, kind) || slices.Contains(bpmnGateways, kind)
}

// xmlElem — произвольный элемент XML: разбор идёт обходом, чтобы видеть
// элементы вне подмножества.
type xmlElem struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Children []xmlElem  `xml:",any"`
	Text     string     `xml:",chardata"`
}

func (e *xmlElem) attr(space, local string) string {
	for _, a := range e.Attrs {
		if a.Name.Local == local && a.Name.Space == space {
			return a.Value
		}
	}
	return ""
}

func (e *xmlElem) bpmn(local string) bool {
	return e.XMLName.Space == bpmnNS && e.XMLName.Local == local
}

func newProcess(id, file string, raw []byte) *Process {
	p := &Process{ID: id, File: file, Raw: raw}
	r := newReporter()
	p.parse(r)
	p.defects = r.defects
	return p
}

func (p *Process) parse(r *reporter) {
	var root xmlElem
	if err := xml.Unmarshal(p.Raw, &root); err != nil {
		r.errorf("bpmn.parse", p.File, "", "BPMN не разбирается: %v", err)
		return
	}
	if !root.bpmn("definitions") {
		r.errorf("bpmn.parse", p.File, "", "корневой элемент должен быть definitions из пространства имён %s", bpmnNS)
		return
	}
	var procs []*xmlElem
	for i := range root.Children {
		if root.Children[i].bpmn("process") {
			procs = append(procs, &root.Children[i])
		}
	}
	if len(procs) == 0 {
		r.errorf("bpmn.no-process", p.File, "", "в файле нет элемента process")
		return
	}
	if len(procs) > 1 {
		r.warnf("bpmn.unsupported", p.File, "", "в файле %d процессов (пулов), проверяется первый", len(procs))
	}
	proc := procs[0]
	p.Name = proc.attr("", "name")
	for i := range proc.Children {
		c := &proc.Children[i]
		if c.XMLName.Space != bpmnNS {
			continue
		}
		kind := c.XMLName.Local
		switch {
		case kind == "laneSet":
			p.Lanes = append(p.Lanes, parseLanes(c)...)
		case kind == "sequenceFlow":
			p.Flows = append(p.Flows, SequenceFlow{
				ID: c.attr("", "id"), Name: c.attr("", "name"),
				Source: c.attr("", "sourceRef"), Target: c.attr("", "targetRef"),
			})
		case supportedNode(kind) || slices.Contains(bpmnOpaque, kind):
			n := FlowNode{
				ID: c.attr("", "id"), Name: c.attr("", "name"), Kind: kind,
				Ref: c.attr(RivetNS, "ref"), Direction: c.attr("", "gatewayDirection"),
				AttachedTo: c.attr("", "attachedToRef"), Supported: supportedNode(kind),
			}
			if !n.Supported {
				r.warnf("bpmn.unsupported", p.File, n.ID, "элемент %s вне поддерживаемого подмножества BPMN, проверяется только его связность", kind)
			}
			p.Nodes = append(p.Nodes, n)
		case slices.Contains(bpmnSilent, kind):
		default:
			r.warnf("bpmn.unsupported", p.File, c.attr("", "id"), "элемент %s вне поддерживаемого подмножества BPMN и не проверяется", kind)
		}
	}
	p.parsed = true
}

func parseLanes(set *xmlElem) []Lane {
	var out []Lane
	for i := range set.Children {
		l := &set.Children[i]
		if !l.bpmn("lane") {
			continue
		}
		lane := Lane{ID: l.attr("", "id"), Name: l.attr("", "name"), Ref: l.attr(RivetNS, "ref")}
		for j := range l.Children {
			c := &l.Children[j]
			switch {
			case c.bpmn("flowNodeRef"):
				lane.Nodes = append(lane.Nodes, strings.TrimSpace(c.Text))
			case c.bpmn("childLaneSet"):
				lane.Lanes = append(lane.Lanes, parseLanes(c)...)
			}
		}
		out = append(out, lane)
	}
	return out
}

func (p *Process) node(id string) *FlowNode {
	for i := range p.Nodes {
		if p.Nodes[i].ID == id {
			return &p.Nodes[i]
		}
	}
	return nil
}

func nodeLabel(n *FlowNode) string {
	if n.Name != "" {
		return fmt.Sprintf("«%s» (%s)", n.Name, n.ID)
	}
	return n.ID
}

// check проверяет структуру процесса и ссылки на модель.
func (p *Process) check(m *Model, r *reporter, opt checkOptions) {
	r.merge(p.defects)
	if !p.parsed {
		return
	}
	ids := map[string]bool{}
	for _, n := range p.Nodes {
		if ids[n.ID] {
			r.errorf("bpmn.duplicate-id", p.File, n.ID, "идентификатор узла %s повторяется", n.ID)
		}
		ids[n.ID] = true
	}
	next, prev := map[string][]string{}, map[string][]string{}
	edge := func(a, b string) {
		next[a] = append(next[a], b)
		prev[b] = append(prev[b], a)
	}
	for _, f := range p.Flows {
		bad := false
		for _, ref := range []string{f.Source, f.Target} {
			if !ids[ref] {
				r.errorf("bpmn.flow-ref", p.File, f.ID, "поток ссылается на несуществующий узел %q", ref)
				bad = true
			}
		}
		if !bad {
			edge(f.Source, f.Target)
		}
	}
	var starts, ends []string
	for _, n := range p.Nodes {
		if n.AttachedTo != "" && ids[n.AttachedTo] {
			// Граничное событие достижимо, если достижима его активность.
			edge(n.AttachedTo, n.ID)
		}
		switch n.Kind {
		case "startEvent":
			starts = append(starts, n.ID)
		case "endEvent":
			ends = append(ends, n.ID)
		}
	}
	if len(starts) == 0 {
		r.errorf("bpmn.no-start", p.File, "", "в процессе %s нет начального события", p.ID)
	}
	if len(ends) == 0 {
		r.errorf("bpmn.no-end", p.File, "", "в процессе %s нет конечного события", p.ID)
	}
	if len(starts) > 0 {
		seen := reach(starts, next)
		for i := range p.Nodes {
			if n := &p.Nodes[i]; !seen[n.ID] {
				r.errorf("bpmn.unreachable", p.File, n.ID, "узел %s процесса %s недостижим из начального события", nodeLabel(n), p.ID)
			}
		}
	}
	if len(ends) > 0 {
		seen := reach(ends, prev)
		for i := range p.Nodes {
			if n := &p.Nodes[i]; !seen[n.ID] {
				r.errorf("bpmn.dead-end", p.File, n.ID, "из узла %s процесса %s не достижимо конечное событие", nodeLabel(n), p.ID)
			}
		}
	}
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if !slices.Contains(bpmnGateways, n.Kind) {
			continue
		}
		in, out := len(prev[n.ID]), len(next[n.ID])
		// Ветвящий шлюз: явно Diverging или не сливающий потоки. Шлюз
		// с одним выходом ничего не ветвит и чаще всего — недописанная ветка.
		if (n.Direction == "Diverging" || in < 2) && out < 2 {
			r.errorf("bpmn.gateway-split", p.File, n.ID, "у ветвящего шлюза %s исходящих потоков: %d, нужно не меньше двух", nodeLabel(n), out)
		}
		if n.Ref != "" {
			r.warnf("bpmn.ref-ignored", p.File, n.ID, "rivet:ref учитывается только у дорожек и сервисных задач")
		}
	}
	p.checkLanes(m, p.Lanes, ids, r, opt)
	for i := range p.Nodes {
		n := &p.Nodes[i]
		switch {
		case n.Ref == "" || slices.Contains(bpmnGateways, n.Kind):
		case n.Kind == "serviceTask":
			m.checkModelRef(r, p.File, n.ID, "сервисная задача "+nodeLabel(n), n.Ref, serviceTaskRefTypes, opt)
		default:
			r.warnf("bpmn.ref-ignored", p.File, n.ID, "rivet:ref учитывается только у дорожек и сервисных задач")
		}
	}
}

func (p *Process) checkLanes(m *Model, lanes []Lane, ids map[string]bool, r *reporter, opt checkOptions) {
	for _, l := range lanes {
		for _, ref := range l.Nodes {
			if !ids[ref] {
				r.errorf("bpmn.lane-ref", p.File, l.ID, "дорожка ссылается на несуществующий узел %q", ref)
			}
		}
		if l.Ref != "" {
			m.checkModelRef(r, p.File, l.ID, "дорожка «"+l.Name+"»", l.Ref, laneRefTypes, opt)
		}
		p.checkLanes(m, l.Lanes, ids, r, opt)
	}
}

func (m *Model) checkModelRef(r *reporter, file, loc, what, ref string, types []string, opt checkOptions) {
	if opt.removed[ref] {
		return
	}
	e := m.element(ref)
	if e == nil {
		r.errorf("bpmn.ref-unresolved", file, loc, "%s ссылается на несуществующий элемент %s", what, ref)
		return
	}
	if !slices.Contains(types, e.Type) {
		r.errorf("bpmn.ref-type", file, loc, "%s может ссылаться только на %s, а %s имеет тип %s",
			what, strings.Join(types, " или "), ref, e.Type)
	}
}

// refs возвращает ссылки процесса на модель, которые учитывает проверка:
// дорожки и сервисные задачи. rivet:ref у других узлов игнорируется и
// удалению элемента не мешает.
func (p *Process) refs() []string {
	var out []string
	var walk func([]Lane)
	walk = func(ls []Lane) {
		for _, l := range ls {
			if l.Ref != "" {
				out = append(out, l.Ref)
			}
			walk(l.Lanes)
		}
	}
	walk(p.Lanes)
	for _, n := range p.Nodes {
		if n.Ref != "" && n.Kind == "serviceTask" {
			out = append(out, n.Ref)
		}
	}
	return out
}

func reach(from []string, adj map[string][]string) map[string]bool {
	seen := map[string]bool{}
	queue := append([]string(nil), from...)
	for _, s := range from {
		seen[s] = true
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, n := range adj[cur] {
			if !seen[n] {
				seen[n] = true
				queue = append(queue, n)
			}
		}
	}
	return seen
}
