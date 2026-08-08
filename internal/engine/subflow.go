package engine

import (
	"fmt"
	"strings"
)

// Subflow instantiation.
//
// A subflow in the flow file is a template plus a set of instance nodes typed
// "subflow:<templateID>". Nothing in the file says what the instance's internals
// are, because they are the template's — so before anything can run, every
// instance has to be turned into a real copy of the template's nodes, wired to
// the instance's own neighbours.
//
// Node-RED does this inside its flow manager and the result is invisible. Doing
// it as an explicit, separate graph has two properties worth the extra code. The
// flow file is untouched, so the byte-identical round-trip still holds — an
// expansion that mutated Flows would rewrite a customer's file on the first
// deploy. And the expanded graph is an ordinary graph, so the scheduler, the
// Catch routing, the metrics and the editor's status events all work on subflow
// internals with no special cases.

// SubflowScopeSeparator joins an instance id to a template node id to make the
// id of the copy.
//
// It has to be a character Node-RED never puts in an id — ids there are hex from
// RED.util.generateId — so that a derived id can never collide with a real one.
// It also has to be stable across deploys, because context stores are keyed by
// node id and a fresh id every deploy would lose a subflow's node context.
const SubflowScopeSeparator = ":"

// maxSubflowDepth bounds nesting. A subflow containing an instance of itself is
// a cycle, and expanding it would not terminate.
const maxSubflowDepth = 32

// EnvScope is one frame of environment-variable resolution: the variables
// declared by a subflow instance, a template, or a tab.
type EnvScope struct {
	// ID names the scope for diagnostics — an instance id or a tab id.
	ID   string
	Vars []EnvVar
}

// Expansion is a flow set with every subflow instance replaced by a running
// copy of its template.
type Expansion struct {
	// Flows is the graph to execute. It is not the graph to save: see
	// Flows.Expanded.
	Flows *Flows

	// EnvChains maps a node id to the environment scopes enclosing it,
	// innermost first. This is Node-RED's _path resolution: a node inside two
	// nested instances sees the inner instance's variables, then the outer
	// one's, then the tab's, then the process environment.
	EnvChains map[string][]EnvScope

	// ParentScope maps a subflow instance's scope id to the scope containing it,
	// so an error nobody caught inside a subflow can be offered to the flow that
	// called it.
	ParentScope map[string]string

	// Instances lists the scope id of every expanded instance, in the order they
	// were expanded.
	Instances []string

	Warnings []string
}

// ExpandSubflows returns the graph to run.
//
// The input is left completely untouched. When there are no subflow instances
// the returned Flows is the input itself, so the ordinary case costs nothing.
func ExpandSubflows(f *Flows) *Expansion {
	ex := &Expansion{
		Flows:       f,
		EnvChains:   map[string][]EnvScope{},
		ParentScope: map[string]string{},
	}

	if !hasSubflowInstances(f) {
		return ex
	}

	out := &Flows{
		Tabs:     make(map[string]*Tab, len(f.Tabs)),
		Subflows: f.Subflows,
		Groups:   make(map[string]*Group, len(f.Groups)),
		Nodes:    make(map[string]*Node, len(f.Nodes)),
		Rev:      f.Rev,
		Warnings: f.Warnings,
		Expanded: true,
	}
	for id, t := range f.Tabs {
		out.Tabs[id] = t
	}
	for id, g := range f.Groups {
		out.Groups[id] = g
	}

	// Every node that is not itself a template's internals is copied across
	// unchanged, in file order, so the expanded graph starts as the real one.
	for _, id := range f.Order {
		n, ok := f.Nodes[id]
		if !ok {
			continue
		}
		if _, insideTemplate := f.Subflows[n.Z]; insideTemplate && !n.IsConfig {
			// A template's own flow nodes never run; only copies of them do.
			//
			// A configuration node scoped to a template is deliberately not
			// copied per instance: it runs once, at the top level, and every
			// instance shares it. That is what an author means by putting an
			// MQTT broker inside a subflow — one connection, not one per
			// instance — and copying it would open a connection per instance
			// against a broker that is probably counting them.
			continue
		}
		out.Nodes[id] = n
		out.Order = append(out.Order, id)
	}

	// Expand instances. The list grows as nested instances are discovered, so
	// this is a queue rather than a range over a fixed slice.
	type pending struct {
		inst  *Node
		depth int
		chain []EnvScope
	}
	var queue []pending

	for _, id := range out.Order {
		n := out.Nodes[id]
		if _, isInstance := n.SubflowTemplateID(); isInstance {
			queue = append(queue, pending{inst: n, depth: 0, chain: ex.chainFor(f, n.Z)})
		}
	}

	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]

		newInstances := ex.expandOne(f, out, p.inst, p.depth, p.chain)
		for _, child := range newInstances {
			if p.depth+1 >= maxSubflowDepth {
				ex.Warnings = append(ex.Warnings, fmt.Sprintf(
					"subflow instance %s nests more than %d levels deep and was not expanded; "+
						"a subflow containing itself is a cycle", child.ID, maxSubflowDepth))
				continue
			}
			queue = append(queue, pending{
				inst:  child,
				depth: p.depth + 1,
				chain: ex.EnvChains[child.ID],
			})
		}
	}

	ex.Flows = out
	return ex
}

func hasSubflowInstances(f *Flows) bool {
	for _, n := range f.Nodes {
		if _, ok := n.SubflowTemplateID(); ok {
			return true
		}
	}
	return false
}

// chainFor returns the environment chain for a node sitting directly in scope z,
// which for a top-level node is just its tab.
func (ex *Expansion) chainFor(f *Flows, z string) []EnvScope {
	if chain, ok := ex.EnvChains[z]; ok {
		return chain
	}
	if tab, ok := f.Tabs[z]; ok && len(tab.Env) > 0 {
		return []EnvScope{{ID: z, Vars: tab.Env}}
	}
	return nil
}

// expandOne instantiates a single subflow instance into out, returning any
// nested instances the copy contains.
func (ex *Expansion) expandOne(f, out *Flows, inst *Node, depth int, parentChain []EnvScope) []*Node {
	tmplID, _ := inst.SubflowTemplateID()
	tmpl, ok := f.Subflows[tmplID]
	if !ok {
		// Already reported as a parse warning; the instance is left in place as
		// a node with an unknown type, which fails only itself at start.
		return nil
	}

	// The instance's own scope. Using the instance node's id means each instance
	// gets its own flow context — two instances of a counting subflow must not
	// share a counter — and means the id is stable across deploys.
	scopeID := inst.ID
	ex.Instances = append(ex.Instances, scopeID)
	ex.ParentScope[scopeID] = inst.Z

	// The innermost environment frame is the template's declared variables with
	// the instance's own values laid over them. The template's entry is what
	// gives an unset property its default; the instance's entry is what makes
	// two instances of one template behave differently, which is the entire
	// point of subflow properties.
	chain := append([]EnvScope{{ID: scopeID, Vars: mergeEnv(tmpl.Env, parseEnv(inst.Raw["env"]))}}, parentChain...)
	ex.EnvChains[scopeID] = chain

	// The instance node stays in the graph as the entry point: upstream wires
	// still target its id, and it forwards into the copy. Its outgoing wires
	// move onto whichever internal nodes feed the template's output ports.
	entry := inst.shallowCopy()
	entry.Wires = [][]string{nil}

	if len(tmpl.In) > 1 {
		ex.Warnings = append(ex.Warnings, fmt.Sprintf(
			"subflow %s declares %d inputs but a v1 wire cannot address a port, so only "+
				"the first is reachable", tmplID, len(tmpl.In)))
	}
	if len(tmpl.In) > 0 {
		for _, w := range tmpl.In[0].Wires {
			entry.Wires[0] = append(entry.Wires[0], derivedID(scopeID, w.ID))
		}
	}
	out.Nodes[inst.ID] = entry

	// The instance is an execution scope of its own, so it is registered as a
	// tab. That is not a hack: a tab is exactly what the rest of the runtime
	// means by "a flow", and an instance is one. It also carries the instance's
	// environment, which is what a node inside it resolves against first.
	out.Tabs[scopeID] = &Tab{
		ID:       scopeID,
		Label:    subflowInstanceLabel(inst, tmpl),
		Disabled: inst.Disabled,
		Env:      chain[0].Vars,
	}

	// Copy the template's nodes.
	var nested []*Node
	internal := f.NodesInScope(tmplID)
	for _, tid := range internal {
		tn := f.Nodes[tid]
		copyNode := tn.shallowCopy()
		copyNode.ID = derivedID(scopeID, tid)
		copyNode.Z = scopeID
		if tn.G != "" {
			// Groups inside a template are per-instance too, or a Catch node in
			// one instance's group would be considered closest to a failure in
			// another's.
			copyNode.G = derivedID(scopeID, tn.G)
		}
		copyNode.Disabled = tn.Disabled || inst.Disabled

		copyNode.Wires = make([][]string, len(tn.Wires))
		for port, targets := range tn.Wires {
			for _, t := range targets {
				copyNode.Wires[port] = append(copyNode.Wires[port], derivedID(scopeID, t))
			}
		}

		out.Nodes[copyNode.ID] = copyNode
		out.Order = append(out.Order, copyNode.ID)
		ex.EnvChains[copyNode.ID] = chain

		if _, isInstance := copyNode.SubflowTemplateID(); isInstance {
			nested = append(nested, copyNode)
		}
	}

	// Copy the groups the template's nodes belong to, so the Catch and Status
	// distance rule still works inside an instance.
	for gid, g := range f.Groups {
		if g.Z != tmplID {
			continue
		}
		copyGroup := &Group{
			ID:   derivedID(scopeID, gid),
			Z:    scopeID,
			Name: g.Name,
			Raw:  g.Raw,
		}
		if g.G != "" {
			copyGroup.G = derivedID(scopeID, g.G)
		}
		for _, member := range g.Nodes {
			copyGroup.Nodes = append(copyGroup.Nodes, derivedID(scopeID, member))
		}
		out.Groups[copyGroup.ID] = copyGroup
	}

	// Wire the template's output ports to the instance's external neighbours.
	// An output port names an internal node and one of its output ports; the
	// external targets are appended there, so a message leaving the subflow
	// takes one hop rather than being relayed through a second node.
	for j, port := range tmpl.Out {
		if j >= len(inst.Wires) {
			break
		}
		external := inst.Wires[j]
		if len(external) == 0 {
			continue
		}
		for _, w := range port.Wires {
			target, ok := out.Nodes[derivedID(scopeID, w.ID)]
			if !ok {
				ex.Warnings = append(ex.Warnings, fmt.Sprintf(
					"subflow %s output %d is wired from node %s, which is not in the template",
					tmplID, j+1, w.ID))
				continue
			}
			for len(target.Wires) <= w.Port {
				target.Wires = append(target.Wires, nil)
			}
			target.Wires[w.Port] = append(target.Wires[w.Port], external...)
		}
	}

	return nested
}

// subflowInstanceLabel names an instance for the editor and the logs.
func subflowInstanceLabel(inst *Node, tmpl *Subflow) string {
	if inst.Name != "" {
		return inst.Name
	}
	if tmpl.Name != "" {
		return tmpl.Name
	}
	return "subflow " + tmpl.ID
}

// derivedID builds the id of a copied node.
func derivedID(scopeID, templateNodeID string) string {
	return scopeID + SubflowScopeSeparator + templateNodeID
}

// SplitDerivedID reverses derivedID, reporting whether the id was one.
func SplitDerivedID(id string) (scopeID, templateNodeID string, ok bool) {
	i := strings.LastIndex(id, SubflowScopeSeparator)
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// mergeEnv lays instance values over template declarations.
//
// The template's entries supply the defaults, so a property the instance leaves
// alone still resolves; the instance's entries win where both have a name. A
// subflow whose defaults were dropped would look like it worked until somebody
// deployed an instance that relied on one.
func mergeEnv(template, instance []EnvVar) []EnvVar {
	if len(instance) == 0 {
		return template
	}
	byName := make(map[string]int, len(template))
	out := make([]EnvVar, 0, len(template)+len(instance))
	for _, ev := range template {
		byName[ev.Name] = len(out)
		out = append(out, ev)
	}
	for _, ev := range instance {
		if i, ok := byName[ev.Name]; ok {
			out[i] = ev
			continue
		}
		byName[ev.Name] = len(out)
		out = append(out, ev)
	}
	return out
}

// shallowCopy duplicates a node's own fields, sharing Raw.
//
// Raw is shared deliberately. It is read-only after parsing — a node's factory
// reads its configuration out of it and never writes back — and copying it per
// instance would duplicate every property of every node in a template for no
// benefit.
func (n *Node) shallowCopy() *Node {
	cp := *n
	cp.Wires = make([][]string, len(n.Wires))
	for i, w := range n.Wires {
		cp.Wires[i] = append([]string(nil), w...)
	}
	return &cp
}
