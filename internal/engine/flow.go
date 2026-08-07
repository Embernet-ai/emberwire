package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// The Node-RED v1 flow file is a single flat JSON array in which everything is a
// node object. A tab is a node. A subflow definition is a node. A group is a
// node. Containment is expressed by the "z" property pointing at the id of the
// containing tab or subflow, which the Node-RED wiki itself describes as "a bit
// of a hack" — but it is the format every existing flow, every export and every
// entry in the public flow library is written in, so it is the format we read.
//
// A v2 hierarchical format has been designed upstream and never shipped as the
// on-disk default. We do not implement it.

// Reserved type names that identify structural entries rather than runnable nodes.
const (
	TypeTab     = "tab"
	TypeSubflow = "subflow"
	TypeGroup   = "group"

	// SubflowInstancePrefix marks a node that is an instance of a subflow
	// template, e.g. "subflow:a1b2c3d4e5f60718".
	SubflowInstancePrefix = "subflow:"
)

// Node is one entry from the flow file.
//
// Raw holds the complete original object. The typed fields are a parsed view of
// it, not a replacement: a flow may carry properties belonging to node types we
// have never heard of, and dropping them would corrupt the user's flow the first
// time we saved it. Raw is the source of truth on write.
type Node struct {
	ID    string
	Type  string
	Z     string // containing tab or subflow id; "" for global config nodes
	G     string // containing group id; "" if ungrouped
	Name  string
	Wires [][]string // outer index is the output port number
	X, Y  float64

	// Disabled mirrors the "d" property. A disabled node stays in the flow file
	// and in the editor but is never started and never receives a message.
	Disabled bool

	// IsConfig reports whether this is a configuration node — one referenced by
	// id from other nodes' properties rather than wired into the graph.
	IsConfig bool

	Raw map[string]any
}

// SubflowTemplateID returns the template id for a subflow instance node, and
// whether this node is one.
func (n *Node) SubflowTemplateID() (string, bool) {
	if strings.HasPrefix(n.Type, SubflowInstancePrefix) {
		return strings.TrimPrefix(n.Type, SubflowInstancePrefix), true
	}
	return "", false
}

// Prop reads a raw property. Node implementations use this to pull their own
// configuration out of the flow entry.
func (n *Node) Prop(key string) (any, bool) {
	v, ok := n.Raw[key]
	return v, ok
}

// PropString reads a string property, returning def when absent or not a string.
func (n *Node) PropString(key, def string) string {
	if s, ok := n.Raw[key].(string); ok {
		return s
	}
	return def
}

// PropBool reads a boolean property. Node-RED edit dialogs sometimes persist
// booleans as the strings "true"/"false", so both spellings are accepted.
func (n *Node) PropBool(key string, def bool) bool {
	switch v := n.Raw[key].(type) {
	case bool:
		return v
	case string:
		switch v {
		case "true":
			return true
		case "false":
			return false
		}
	}
	return def
}

// PropFloat reads a numeric property. JSON numbers decode as float64, but edit
// dialogs persist numbers as strings often enough that both are handled.
func (n *Node) PropFloat(key string, def float64) float64 {
	switch v := n.Raw[key].(type) {
	case float64:
		return v
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
			return f
		}
	}
	return def
}

// PropInt reads an integer property.
func (n *Node) PropInt(key string, def int) int {
	f := n.PropFloat(key, float64(def))
	return int(f)
}

// Outputs returns the number of output ports the node is wired for.
func (n *Node) Outputs() int { return len(n.Wires) }

// Tab is a flow tab — one canvas in the editor, and one scope for flow context.
type Tab struct {
	ID       string
	Label    string
	Disabled bool
	Info     string
	Env      []EnvVar
	Raw      map[string]any
}

// Subflow is a subflow template. Instances of it appear in the graph as nodes
// with type "subflow:<ID>".
type Subflow struct {
	ID       string
	Name     string
	Info     string
	Category string
	In       []SubflowPort
	Out      []SubflowPort
	Env      []EnvVar
	Raw      map[string]any
}

// SubflowPort is one input or output of a subflow template. Wires holds the
// internal nodes the port connects to.
type SubflowPort struct {
	X, Y  float64
	Wires []SubflowWire
}

// SubflowWire is a connection between a subflow port and an internal node.
type SubflowWire struct {
	ID   string
	Port int
}

// EnvVar is a tab- or subflow-scoped environment variable. Subflow properties
// are how an instance is parameterised — the same template behaves differently
// per instance because env resolution walks the instance path.
type EnvVar struct {
	Name  string
	Type  string
	Value any
}

// Group is a visual grouping of nodes. Groups are not execution containers, but
// they are not purely cosmetic either: Catch and Status resolution prefers the
// handler closest in the group hierarchy, so the nesting is load-bearing.
type Group struct {
	ID    string
	Z     string // containing tab
	G     string // parent group id, for nested groups
	Name  string
	Nodes []string // member node ids
	Raw   map[string]any
}

// Flows is a parsed flow file.
//
// Order preserves the original array order so that a save does not reshuffle the
// document. Note that key order *within* an object is not preserved: Go maps are
// unordered and encoding/json emits keys sorted. The output is therefore stable
// and diff-friendly across saves, but the first save after importing a Node-RED
// file will reorder keys once. That is a deliberate trade — an order-preserving
// map would touch every read path to buy cosmetic fidelity on a single write.
type Flows struct {
	Tabs     map[string]*Tab
	Subflows map[string]*Subflow
	Groups   map[string]*Group
	Nodes    map[string]*Node

	// Order is the ids of every entry, in original file order.
	Order []string

	// Rev is the revision token used for optimistic concurrency on deploy, the
	// same mechanism as Node-RED's /flows rev field.
	Rev string

	// Warnings collects non-fatal problems found while parsing: dangling wires,
	// unknown group members, nodes on a missing tab. Node-RED tolerates all of
	// these and so do we, but silently repairing a flow is how you lose a
	// customer's work without telling them.
	Warnings []string
}

// ParseError is a fatal problem that prevents a flow file being loaded at all.
type ParseError struct {
	Index  int    // position in the flow array, or -1
	NodeID string // id of the offending entry, if known
	Msg    string
}

func (e *ParseError) Error() string {
	switch {
	case e.NodeID != "" && e.Index >= 0:
		return fmt.Sprintf("flow entry %d (id %s): %s", e.Index, e.NodeID, e.Msg)
	case e.Index >= 0:
		return fmt.Sprintf("flow entry %d: %s", e.Index, e.Msg)
	default:
		return e.Msg
	}
}

// ParseFlows parses a Node-RED v1 flow file.
//
// It accepts both the bare array form written to disk and the wrapped
// {"rev":...,"flows":[...]} form returned by the admin API since Node-RED 0.15.
func ParseFlows(data []byte) (*Flows, error) {
	trimmed := strings.TrimLeft(string(data), " \t\r\n")
	if trimmed == "" {
		return newEmptyFlows(), nil
	}

	var raw []map[string]any

	if strings.HasPrefix(trimmed, "{") {
		var wrapper struct {
			Rev string `json:"rev"`
			// A pointer so that an absent "flows" key is distinguishable from
			// an empty one. Treating a document without it as an empty flow set
			// would silently discard whatever the caller actually sent.
			Flows *[]map[string]any `json:"flows"`
		}
		if err := json.Unmarshal(data, &wrapper); err != nil {
			return nil, &ParseError{Index: -1, Msg: fmt.Sprintf("decoding wrapped flow document: %v", err)}
		}
		if wrapper.Flows == nil {
			return nil, &ParseError{Index: -1, Msg: `flow document is an object with no "flows" array`}
		}
		raw = *wrapper.Flows
		f, err := buildFlows(raw)
		if err != nil {
			return nil, err
		}
		f.Rev = wrapper.Rev
		return f, nil
	}

	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, &ParseError{Index: -1, Msg: fmt.Sprintf("decoding flow array: %v", err)}
	}
	return buildFlows(raw)
}

func newEmptyFlows() *Flows {
	return &Flows{
		Tabs:     map[string]*Tab{},
		Subflows: map[string]*Subflow{},
		Groups:   map[string]*Group{},
		Nodes:    map[string]*Node{},
	}
}

func buildFlows(raw []map[string]any) (*Flows, error) {
	f := newEmptyFlows()

	// Pass 1: classify every entry and index it by id. Cross-references are not
	// resolved yet because the file has no ordering guarantee — a node may be
	// wired to a node that appears later in the array.
	for i, obj := range raw {
		id, _ := obj["id"].(string)
		typ, _ := obj["type"].(string)

		if id == "" {
			return nil, &ParseError{Index: i, Msg: "entry has no id"}
		}
		if typ == "" {
			return nil, &ParseError{Index: i, NodeID: id, Msg: "entry has no type"}
		}
		if f.has(id) {
			return nil, &ParseError{Index: i, NodeID: id, Msg: "duplicate id"}
		}

		f.Order = append(f.Order, id)

		switch typ {
		case TypeTab:
			f.Tabs[id] = parseTab(id, obj)
		case TypeSubflow:
			f.Subflows[id] = parseSubflow(id, obj)
		case TypeGroup:
			f.Groups[id] = parseGroup(id, obj)
		default:
			n, err := parseNode(i, id, typ, obj)
			if err != nil {
				return nil, err
			}
			f.Nodes[id] = n
		}
	}

	f.resolve()
	return f, nil
}

func (f *Flows) has(id string) bool {
	if _, ok := f.Tabs[id]; ok {
		return true
	}
	if _, ok := f.Subflows[id]; ok {
		return true
	}
	if _, ok := f.Groups[id]; ok {
		return true
	}
	_, ok := f.Nodes[id]
	return ok
}

func parseTab(id string, obj map[string]any) *Tab {
	t := &Tab{ID: id, Raw: obj}
	t.Label, _ = obj["label"].(string)
	t.Info, _ = obj["info"].(string)
	t.Disabled, _ = obj["disabled"].(bool)
	t.Env = parseEnv(obj["env"])
	return t
}

func parseSubflow(id string, obj map[string]any) *Subflow {
	s := &Subflow{ID: id, Raw: obj}
	s.Name, _ = obj["name"].(string)
	s.Info, _ = obj["info"].(string)
	s.Category, _ = obj["category"].(string)
	s.In = parseSubflowPorts(obj["in"])
	s.Out = parseSubflowPorts(obj["out"])
	s.Env = parseEnv(obj["env"])
	return s
}

func parseSubflowPorts(v any) []SubflowPort {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	ports := make([]SubflowPort, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		p := SubflowPort{}
		p.X, _ = m["x"].(float64)
		p.Y, _ = m["y"].(float64)
		if wires, ok := m["wires"].([]any); ok {
			for _, w := range wires {
				wm, ok := w.(map[string]any)
				if !ok {
					continue
				}
				sw := SubflowWire{}
				sw.ID, _ = wm["id"].(string)
				if pf, ok := wm["port"].(float64); ok {
					sw.Port = int(pf)
				}
				p.Wires = append(p.Wires, sw)
			}
		}
		ports = append(ports, p)
	}
	return ports
}

func parseEnv(v any) []EnvVar {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]EnvVar, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		ev := EnvVar{Value: m["value"]}
		ev.Name, _ = m["name"].(string)
		ev.Type, _ = m["type"].(string)
		if ev.Name != "" {
			out = append(out, ev)
		}
	}
	return out
}

func parseGroup(id string, obj map[string]any) *Group {
	g := &Group{ID: id, Raw: obj}
	g.Z, _ = obj["z"].(string)
	g.G, _ = obj["g"].(string)
	g.Name, _ = obj["name"].(string)
	if arr, ok := obj["nodes"].([]any); ok {
		for _, e := range arr {
			if s, ok := e.(string); ok {
				g.Nodes = append(g.Nodes, s)
			}
		}
	}
	return g
}

func parseNode(index int, id, typ string, obj map[string]any) (*Node, error) {
	n := &Node{ID: id, Type: typ, Raw: obj}
	n.Z, _ = obj["z"].(string)
	n.G, _ = obj["g"].(string)
	n.Name, _ = obj["name"].(string)
	n.Disabled, _ = obj["d"].(bool)

	_, hasX := obj["x"]
	_, hasY := obj["y"]
	n.X, _ = obj["x"].(float64)
	n.Y, _ = obj["y"].(float64)

	// The structural rule from the Node-RED Admin API type documentation:
	// configuration nodes must not carry x, y or wires. Everything else is a
	// flow node. This is decidable without a node registry, which matters
	// because the parser runs before the registry is consulted.
	n.IsConfig = !hasX && !hasY

	if w, present := obj["wires"]; present {
		wires, err := parseWires(w)
		if err != nil {
			return nil, &ParseError{Index: index, NodeID: id, Msg: err.Error()}
		}
		n.Wires = wires
		// A node carrying wires is wired into the graph by definition, even if
		// the editor omitted its coordinates.
		n.IsConfig = false
	}

	return n, nil
}

func parseWires(v any) ([][]string, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("wires is %T, want array", v)
	}
	out := make([][]string, len(arr))
	for i, port := range arr {
		targets, ok := port.([]any)
		if !ok {
			return nil, fmt.Errorf("wires[%d] is %T, want array", i, port)
		}
		out[i] = make([]string, 0, len(targets))
		for j, t := range targets {
			s, ok := t.(string)
			if !ok {
				return nil, fmt.Errorf("wires[%d][%d] is %T, want string node id", i, j, t)
			}
			out[i] = append(out[i], s)
		}
	}
	return out, nil
}

// resolve performs the cross-reference pass and records anything questionable as
// a warning. Nothing here is fatal: Node-RED loads flows with dangling wires and
// so must we, or importing a partially-copied flow would fail outright.
func (f *Flows) resolve() {
	var warn []string

	for _, id := range f.Order {
		n, ok := f.Nodes[id]
		if !ok {
			continue
		}

		// Containing scope must exist.
		if n.Z != "" {
			_, isTab := f.Tabs[n.Z]
			_, isSubflow := f.Subflows[n.Z]
			if !isTab && !isSubflow {
				warn = append(warn, fmt.Sprintf("node %s (%s) references missing tab or subflow %q", n.ID, n.Type, n.Z))
			}
		}

		// Group membership must exist.
		if n.G != "" {
			if _, ok := f.Groups[n.G]; !ok {
				warn = append(warn, fmt.Sprintf("node %s (%s) references missing group %q", n.ID, n.Type, n.G))
			}
		}

		// Subflow instances must have a template.
		if tmpl, isInstance := n.SubflowTemplateID(); isInstance {
			if _, ok := f.Subflows[tmpl]; !ok {
				warn = append(warn, fmt.Sprintf("node %s is an instance of missing subflow %q", n.ID, tmpl))
			}
		}

		// Wire targets must exist. A dangling wire is dropped at start-up
		// rather than at parse, so the editor can still show and repair it.
		for port, targets := range n.Wires {
			for _, t := range targets {
				if _, ok := f.Nodes[t]; !ok {
					warn = append(warn, fmt.Sprintf("node %s output %d wires to missing node %q", n.ID, port, t))
				}
			}
		}
	}

	for _, gid := range sortedKeys(f.Groups) {
		g := f.Groups[gid]
		if g.G != "" {
			if _, ok := f.Groups[g.G]; !ok {
				warn = append(warn, fmt.Sprintf("group %s references missing parent group %q", g.ID, g.G))
			}
		}
		for _, member := range g.Nodes {
			if !f.has(member) {
				warn = append(warn, fmt.Sprintf("group %s lists missing member %q", g.ID, member))
			}
		}
	}

	f.Warnings = warn
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// GroupDepth returns how deeply a group is nested, counting from 0 at the
// outermost group. It is the basis of Catch and Status handler selection, where
// the handler closest to the erroring node wins.
//
// A cycle in the parent chain — which a hand-edited flow can contain — is
// treated as terminating rather than looping forever.
func (f *Flows) GroupDepth(groupID string) int {
	depth := 0
	seen := map[string]bool{}
	for groupID != "" && !seen[groupID] {
		seen[groupID] = true
		g, ok := f.Groups[groupID]
		if !ok {
			break
		}
		groupID = g.G
		depth++
	}
	return depth
}

// GroupChain returns the group ids containing a node, innermost first, ending at
// the outermost group. An ungrouped node yields nil.
func (f *Flows) GroupChain(nodeID string) []string {
	n, ok := f.Nodes[nodeID]
	if !ok || n.G == "" {
		return nil
	}
	var chain []string
	seen := map[string]bool{}
	for gid := n.G; gid != "" && !seen[gid]; {
		seen[gid] = true
		chain = append(chain, gid)
		g, ok := f.Groups[gid]
		if !ok {
			break
		}
		gid = g.G
	}
	return chain
}

// NodesInScope returns the ids of every non-config node belonging to a tab or
// subflow, in file order.
func (f *Flows) NodesInScope(scopeID string) []string {
	var out []string
	for _, id := range f.Order {
		if n, ok := f.Nodes[id]; ok && n.Z == scopeID && !n.IsConfig {
			out = append(out, id)
		}
	}
	return out
}

// ConfigNodes returns the ids of every configuration node, in file order.
func (f *Flows) ConfigNodes() []string {
	var out []string
	for _, id := range f.Order {
		if n, ok := f.Nodes[id]; ok && n.IsConfig {
			out = append(out, id)
		}
	}
	return out
}

// Entry returns the raw object for any id, whatever kind of entry it is.
func (f *Flows) Entry(id string) (map[string]any, bool) {
	if t, ok := f.Tabs[id]; ok {
		return t.Raw, true
	}
	if s, ok := f.Subflows[id]; ok {
		return s.Raw, true
	}
	if g, ok := f.Groups[id]; ok {
		return g.Raw, true
	}
	if n, ok := f.Nodes[id]; ok {
		return n.Raw, true
	}
	return nil, false
}

// MarshalJSON writes the flow file back as a v1 array, in original entry order.
//
// The raw objects are emitted directly, so properties belonging to node types
// this build does not implement survive a load-and-save cycle untouched. That is
// the property that makes it safe to run Emberwire against a flow authored in
// Node-RED and then hand it back.
func (f *Flows) MarshalJSON() ([]byte, error) {
	out := make([]map[string]any, 0, len(f.Order))
	for _, id := range f.Order {
		if raw, ok := f.Entry(id); ok {
			out = append(out, raw)
		}
	}
	return json.Marshal(out)
}

// Marshal renders the flow file as indented JSON, matching Node-RED's
// flowFilePretty output so the file stays readable and diffable in git.
func (f *Flows) Marshal() ([]byte, error) {
	out := make([]map[string]any, 0, len(f.Order))
	for _, id := range f.Order {
		if raw, ok := f.Entry(id); ok {
			out = append(out, raw)
		}
	}
	return json.MarshalIndent(out, "", "    ")
}

// StripCredentials removes inline credential objects from every node and returns
// them keyed by node id.
//
// Node-RED never writes credentials into the flow file: the editor posts them
// inline on deploy, the runtime splits them out, and they are stored separately
// and encrypted. Doing the same split here is what stops a broker password
// ending up in a git-committed flows.json.
func (f *Flows) StripCredentials() map[string]map[string]any {
	creds := map[string]map[string]any{}
	for _, id := range f.Order {
		n, ok := f.Nodes[id]
		if !ok {
			continue
		}
		c, ok := n.Raw["credentials"].(map[string]any)
		if !ok {
			continue
		}
		creds[id] = c
		delete(n.Raw, "credentials")
	}
	return creds
}
