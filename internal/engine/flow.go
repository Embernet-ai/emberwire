package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
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
// document, and orig preserves each entry's original bytes so that key order
// inside an entry survives too. Together those give a byte-identical round-trip
// for anything the caller did not actually change — see Marshal.
type Flows struct {
	Tabs     map[string]*Tab
	Subflows map[string]*Subflow
	Groups   map[string]*Group
	Nodes    map[string]*Node

	// Order is the ids of every entry, in original file order.
	Order []string

	// orig holds each entry exactly as it was read, keyed by id. Entries
	// created after parsing have no entry here and are marshalled fresh.
	orig map[string]json.RawMessage

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

	// Entries are decoded twice: once as raw bytes, once as maps. The raw form
	// is what makes a byte-identical round-trip possible — see entryBytes.
	var rawEntries []json.RawMessage
	var rev string

	if strings.HasPrefix(trimmed, "{") {
		var wrapper struct {
			Rev string `json:"rev"`
			// A pointer so that an absent "flows" key is distinguishable from
			// an empty one. Treating a document without it as an empty flow set
			// would silently discard whatever the caller actually sent.
			Flows *[]json.RawMessage `json:"flows"`
		}
		if err := json.Unmarshal(data, &wrapper); err != nil {
			return nil, &ParseError{Index: -1, Msg: fmt.Sprintf("decoding wrapped flow document: %v", err)}
		}
		if wrapper.Flows == nil {
			return nil, &ParseError{Index: -1, Msg: `flow document is an object with no "flows" array`}
		}
		rawEntries = *wrapper.Flows
		rev = wrapper.Rev
	} else if err := json.Unmarshal(data, &rawEntries); err != nil {
		return nil, &ParseError{Index: -1, Msg: fmt.Sprintf("decoding flow array: %v", err)}
	}

	objs := make([]map[string]any, len(rawEntries))
	for i, re := range rawEntries {
		if err := json.Unmarshal(re, &objs[i]); err != nil {
			return nil, &ParseError{Index: i, Msg: fmt.Sprintf("decoding flow entry: %v", err)}
		}
		if objs[i] == nil {
			return nil, &ParseError{Index: i, Msg: "flow entry is null"}
		}
	}

	f, err := buildFlows(objs)
	if err != nil {
		return nil, err
	}
	f.Rev = rev

	// Index the original bytes by id so an unmodified entry can be written back
	// exactly as it arrived.
	for i, id := range f.Order {
		f.orig[id] = rawEntries[i]
	}
	return f, nil
}

func newEmptyFlows() *Flows {
	return &Flows{
		Tabs:     map[string]*Tab{},
		Subflows: map[string]*Subflow{},
		Groups:   map[string]*Group{},
		Nodes:    map[string]*Node{},
		orig:     map[string]json.RawMessage{},
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

// FlowFileIndent is the indentation Node-RED writes with, from
// JSON.stringify(flows, null, 4). Matching it is what lets a file written here
// be byte-identical to one written there.
const FlowFileIndent = "    "

// entryBytes returns the JSON for one entry.
//
// When the parsed form still deep-equals what was read, the original bytes are
// returned verbatim. That is the whole mechanism behind the byte-identical
// round-trip: JavaScript objects preserve key insertion order, so Node-RED gets
// this for free, whereas a Go map has no order at all and encoding/json emits
// keys sorted. Rather than impose an order-preserving map on every read path in
// the codebase, the bytes that already carry the right order are kept and
// reused.
//
// The comparison is done by decoding the original rather than by tracking a
// dirty flag, because a dirty flag relies on every mutation site remembering to
// set it, and the one that forgets is the one that silently corrupts a
// customer's flow file.
func (f *Flows) entryBytes(id string) ([]byte, error) {
	cur, ok := f.Entry(id)
	if !ok {
		return nil, fmt.Errorf("no flow entry with id %q", id)
	}
	if orig, ok := f.orig[id]; ok {
		var check map[string]any
		if err := json.Unmarshal(orig, &check); err == nil && reflect.DeepEqual(check, cur) {
			return orig, nil
		}
		// Changed, but the original still describes where its keys belong.
		// Re-encoding against it keeps an edit to one property a one-line diff
		// instead of reshuffling the whole node — which is the difference
		// between an operator being able to review a deploy and not.
		return marshalOrdered(orig, cur)
	}
	return marshalUnescaped(cur)
}

// marshalOrdered encodes v, emitting object keys in the order they appear in
// tmpl. Keys absent from tmpl are appended in sorted order so the output stays
// deterministic; keys absent from v are dropped.
//
// Recurses through nested objects and arrays, because a node's interesting
// structure — a Change node's rule list, a Switch node's rules — is nested, and
// preserving order only at the top level would still rewrite all of it.
func marshalOrdered(tmpl json.RawMessage, v any) ([]byte, error) {
	switch cur := v.(type) {
	case map[string]any:
		order, fields, ok := objectFields(tmpl)
		if !ok {
			return marshalUnescaped(cur)
		}
		var buf bytes.Buffer
		buf.WriteByte('{')
		written := make(map[string]bool, len(cur))
		first := true

		emit := func(k string, sub json.RawMessage) error {
			val, present := cur[k]
			if !present {
				return nil
			}
			if !first {
				buf.WriteByte(',')
			}
			first = false
			key, err := marshalUnescaped(k)
			if err != nil {
				return err
			}
			buf.Write(key)
			buf.WriteByte(':')

			var enc []byte
			if sub != nil {
				enc, err = marshalOrdered(sub, val)
			} else {
				enc, err = marshalUnescaped(val)
			}
			if err != nil {
				return err
			}
			buf.Write(enc)
			written[k] = true
			return nil
		}

		for _, k := range order {
			if err := emit(k, fields[k]); err != nil {
				return nil, err
			}
		}
		// Anything the caller added since the file was read.
		added := make([]string, 0, len(cur))
		for k := range cur {
			if !written[k] {
				added = append(added, k)
			}
		}
		sort.Strings(added)
		for _, k := range added {
			if err := emit(k, nil); err != nil {
				return nil, err
			}
		}
		buf.WriteByte('}')
		return buf.Bytes(), nil

	case []any:
		var elems []json.RawMessage
		if err := json.Unmarshal(tmpl, &elems); err != nil {
			return marshalUnescaped(cur)
		}
		var buf bytes.Buffer
		buf.WriteByte('[')
		for i, e := range cur {
			if i > 0 {
				buf.WriteByte(',')
			}
			var enc []byte
			var err error
			if i < len(elems) {
				enc, err = marshalOrdered(elems[i], e)
			} else {
				enc, err = marshalUnescaped(e)
			}
			if err != nil {
				return nil, err
			}
			buf.Write(enc)
		}
		buf.WriteByte(']')
		return buf.Bytes(), nil

	default:
		return marshalUnescaped(v)
	}
}

// objectFields reads a JSON object's keys in source order along with each
// value's raw bytes. It reports false when raw is not an object.
func objectFields(raw json.RawMessage) ([]string, map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, false
	}

	var order []string
	fields := map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, nil, false
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, nil, false
		}
		// A duplicate key in the source would otherwise be emitted twice.
		if _, seen := fields[key]; !seen {
			order = append(order, key)
		}
		fields[key] = val
	}
	return order, fields, true
}

// marshalUnescaped encodes without Go's default HTML escaping.
//
// encoding/json turns <, > and & into < and friends. JSON.stringify does
// not, so leaving it on would make every flow containing an HTML template or a
// URL query differ from what Node-RED writes — and would rewrite those escapes
// into the customer's file on the first save.
func marshalUnescaped(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// MarshalJSON writes the flow file back as a compact v1 array, in original
// entry order and with each entry's original key order.
func (f *Flows) MarshalJSON() ([]byte, error) {
	return f.render("")
}

// Marshal renders the flow file the way Node-RED's flowFilePretty does: a v1
// array indented with four spaces.
//
// A file that was read and not modified comes back out byte for byte. Entries
// that were changed are re-encoded; everything else keeps the bytes it arrived
// with, so a deploy diffs only the nodes actually touched rather than reshuffling
// the whole file.
func (f *Flows) Marshal() ([]byte, error) {
	return f.render(FlowFileIndent)
}

// render assembles the array. indent == "" produces compact output.
//
// json.Compact and json.Indent are byte-level transforms — they normalise
// whitespace without reordering keys — which is what lets an entry's original
// bytes be re-indented into the output without losing the order they carry.
func (f *Flows) render(indent string) ([]byte, error) {
	if len(f.Order) == 0 {
		return []byte("[]"), nil
	}

	var out bytes.Buffer
	if indent == "" {
		out.WriteByte('[')
	} else {
		out.WriteString("[\n")
	}

	for i, id := range f.Order {
		eb, err := f.entryBytes(id)
		if err != nil {
			return nil, err
		}

		var compact bytes.Buffer
		if err := json.Compact(&compact, eb); err != nil {
			return nil, fmt.Errorf("compacting entry %s: %w", id, err)
		}

		if indent == "" {
			out.Write(compact.Bytes())
		} else {
			var indented bytes.Buffer
			if err := json.Indent(&indented, compact.Bytes(), indent, indent); err != nil {
				return nil, fmt.Errorf("indenting entry %s: %w", id, err)
			}
			out.WriteString(indent)
			out.Write(indented.Bytes())
		}

		if i < len(f.Order)-1 {
			out.WriteByte(',')
		}
		if indent != "" {
			out.WriteByte('\n')
		}
	}

	out.WriteByte(']')
	return out.Bytes(), nil
}

// Touch marks an entry as modified, forcing it to be re-encoded on the next
// save even if it currently deep-equals what was read.
//
// Needed only when a caller has mutated a nested value in place and wants the
// canonical form written back. Ordinary edits are detected automatically.
func (f *Flows) Touch(id string) { delete(f.orig, id) }

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
