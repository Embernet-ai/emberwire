package engine

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

func parse(t *testing.T, doc string) *Flows {
	t.Helper()
	f, err := ParseFlows([]byte(doc))
	if err != nil {
		t.Fatalf("ParseFlows: %v", err)
	}
	return f
}

// A flow with a subflow template, one instance, and an internal chain of two
// nodes. The instance's output is wired to a sink on the tab.
const oneInstance = `[
    {"id":"tab1","type":"tab","label":"Main"},
    {"id":"sf1","type":"subflow","name":"Scale",
     "in":[{"x":40,"y":40,"wires":[{"id":"inner1"}]}],
     "out":[{"x":400,"y":40,"wires":[{"id":"inner2","port":0}]}],
     "env":[{"name":"FACTOR","type":"num","value":2}]},
    {"id":"inner1","type":"change","z":"sf1","x":100,"y":40,"wires":[["inner2"]]},
    {"id":"inner2","type":"function","z":"sf1","x":250,"y":40,"wires":[[]]},
    {"id":"src","type":"inject","z":"tab1","x":100,"y":100,"wires":[["i1"]]},
    {"id":"i1","type":"subflow:sf1","z":"tab1","x":250,"y":100,"wires":[["sink"]],
     "env":[{"name":"FACTOR","type":"num","value":10}]},
    {"id":"sink","type":"debug","z":"tab1","x":400,"y":100,"wires":[]}
]`

func TestExpandLeavesAFlowWithNoSubflowsAlone(t *testing.T) {
	f := parse(t, `[
        {"id":"tab1","type":"tab","label":"Main"},
        {"id":"a","type":"inject","z":"tab1","x":1,"y":1,"wires":[["b"]]},
        {"id":"b","type":"debug","z":"tab1","x":2,"y":1,"wires":[]}
    ]`)

	ex := ExpandSubflows(f)
	if ex.Flows != f {
		t.Fatal("a flow with no subflow instances was copied; the ordinary case should cost nothing")
	}
	if ex.Flows.Expanded {
		t.Error("the untouched graph was marked as expanded and can no longer be saved")
	}
}

func TestExpandCopiesTheTemplateAndRewiresIt(t *testing.T) {
	f := parse(t, oneInstance)
	ex := ExpandSubflows(f)
	out := ex.Flows

	if len(ex.Warnings) != 0 {
		t.Fatalf("warnings: %v", ex.Warnings)
	}

	inner1 := "i1" + SubflowScopeSeparator + "inner1"
	inner2 := "i1" + SubflowScopeSeparator + "inner2"

	for _, id := range []string{inner1, inner2} {
		n, ok := out.Nodes[id]
		if !ok {
			t.Fatalf("%s was not created", id)
		}
		// Every copy belongs to the instance's own scope, which is what gives
		// two instances of a counting subflow separate flow context.
		if n.Z != "i1" {
			t.Errorf("%s has scope %q, want the instance id", id, n.Z)
		}
	}

	// The template's own nodes must not be in the runnable graph: only copies
	// of them run.
	for _, id := range []string{"inner1", "inner2"} {
		if _, ok := out.Nodes[id]; ok {
			t.Errorf("the template's node %s is in the runnable graph", id)
		}
	}

	// The instance node stays as the entry point and forwards into the copy.
	entry, ok := out.Nodes["i1"]
	if !ok {
		t.Fatal("the instance node is gone; upstream wires would target nothing")
	}
	if !reflect.DeepEqual(entry.Wires, [][]string{{inner1}}) {
		t.Fatalf("the entry node wires to %v, want the copy of the input-connected node", entry.Wires)
	}

	// An internal wire points at the copy, not at the template.
	if !reflect.DeepEqual(out.Nodes[inner1].Wires, [][]string{{inner2}}) {
		t.Fatalf("inner1 wires to %v", out.Nodes[inner1].Wires)
	}

	// The template's output port carries the instance's external wire, so a
	// message leaves the subflow in one hop rather than being relayed.
	if !reflect.DeepEqual(out.Nodes[inner2].Wires, [][]string{{"sink"}}) {
		t.Fatalf("inner2 wires to %v, want the instance's external target", out.Nodes[inner2].Wires)
	}

	// The instance is an execution scope of its own.
	if _, ok := out.Tabs["i1"]; !ok {
		t.Error("the instance was not registered as a scope")
	}
	if ex.ParentScope["i1"] != "tab1" {
		t.Errorf("ParentScope[i1] = %q, want the calling tab", ex.ParentScope["i1"])
	}
	if !reflect.DeepEqual(ex.Instances, []string{"i1"}) {
		t.Errorf("Instances = %v", ex.Instances)
	}
}

// An expansion that rewrote the parsed flows would replace a customer's tidy
// subflow with its inlined guts on the first deploy.
func TestExpandDoesNotTouchTheInputAndCannotBeSaved(t *testing.T) {
	f := parse(t, oneInstance)
	before, err := f.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	ex := ExpandSubflows(f)

	after, err := f.Marshal()
	if err != nil {
		t.Fatalf("marshalling the original after expansion: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("expansion changed the parsed flow file")
	}

	if _, err := ex.Flows.Marshal(); err == nil {
		t.Fatal("the expanded graph was marshalled; saving it would inline every subflow")
	}
	if _, err := ex.Flows.MarshalJSON(); err == nil {
		t.Fatal("the expanded graph was marshalled through MarshalJSON")
	}
}

// Subflow properties are the whole point of a subflow: one template, different
// behaviour per instance. Without the chain they are decoration.
func TestExpandBuildsTheEnvironmentChain(t *testing.T) {
	f := parse(t, oneInstance)
	ex := ExpandSubflows(f)

	inner1 := "i1" + SubflowScopeSeparator + "inner1"
	chain := ex.EnvChains[inner1]
	if len(chain) == 0 {
		t.Fatal("the copied node has no environment chain")
	}
	if chain[0].ID != "i1" {
		t.Fatalf("the innermost scope is %q, want the instance", chain[0].ID)
	}

	var factor any
	for _, ev := range chain[0].Vars {
		if ev.Name == "FACTOR" {
			factor = ev.Value
		}
	}
	// The instance's value wins over the template's default.
	if factor != float64(10) {
		t.Fatalf("FACTOR = %#v, want the instance's 10 rather than the template's 2", factor)
	}
}

// A template's default has to survive, or an instance that leaves a property
// alone resolves to nothing.
func TestExpandKeepsTemplateDefaults(t *testing.T) {
	f := parse(t, `[
        {"id":"tab1","type":"tab","label":"Main"},
        {"id":"sf1","type":"subflow","name":"S",
         "in":[{"wires":[{"id":"inner"}]}],"out":[],
         "env":[{"name":"UNIT","type":"str","value":"bar"},
                {"name":"SCALE","type":"num","value":1}]},
        {"id":"inner","type":"change","z":"sf1","x":1,"y":1,"wires":[[]]},
        {"id":"i1","type":"subflow:sf1","z":"tab1","x":1,"y":1,"wires":[],
         "env":[{"name":"SCALE","type":"num","value":5}]}
    ]`)

	ex := ExpandSubflows(f)
	vars := ex.EnvChains["i1"+SubflowScopeSeparator+"inner"][0].Vars

	got := map[string]any{}
	for _, ev := range vars {
		got[ev.Name] = ev.Value
	}
	if got["UNIT"] != "bar" {
		t.Errorf("UNIT = %#v, want the template's default", got["UNIT"])
	}
	if got["SCALE"] != float64(5) {
		t.Errorf("SCALE = %#v, want the instance's override", got["SCALE"])
	}
}

func TestExpandTwoInstancesAreIndependent(t *testing.T) {
	f := parse(t, `[
        {"id":"tab1","type":"tab","label":"Main"},
        {"id":"sf1","type":"subflow","name":"S",
         "in":[{"wires":[{"id":"inner"}]}],
         "out":[{"wires":[{"id":"inner","port":0}]}]},
        {"id":"inner","type":"function","z":"sf1","x":1,"y":1,"wires":[[]]},
        {"id":"a","type":"subflow:sf1","z":"tab1","x":1,"y":1,"wires":[["sinkA"]]},
        {"id":"b","type":"subflow:sf1","z":"tab1","x":1,"y":2,"wires":[["sinkB"]]},
        {"id":"sinkA","type":"debug","z":"tab1","x":2,"y":1,"wires":[]},
        {"id":"sinkB","type":"debug","z":"tab1","x":2,"y":2,"wires":[]}
    ]`)

	out := ExpandSubflows(f).Flows
	innerA := "a" + SubflowScopeSeparator + "inner"
	innerB := "b" + SubflowScopeSeparator + "inner"

	if out.Nodes[innerA] == nil || out.Nodes[innerB] == nil {
		t.Fatal("both instances should have their own copy")
	}
	if out.Nodes[innerA] == out.Nodes[innerB] {
		t.Fatal("the two instances share a node; state would be shared too")
	}
	// Each copy leaves by its own instance's external wire.
	if !reflect.DeepEqual(out.Nodes[innerA].Wires, [][]string{{"sinkA"}}) {
		t.Errorf("instance a leaves to %v", out.Nodes[innerA].Wires)
	}
	if !reflect.DeepEqual(out.Nodes[innerB].Wires, [][]string{{"sinkB"}}) {
		t.Errorf("instance b leaves to %v", out.Nodes[innerB].Wires)
	}
}

func TestExpandNestedSubflows(t *testing.T) {
	f := parse(t, `[
        {"id":"tab1","type":"tab","label":"Main"},
        {"id":"outer","type":"subflow","name":"Outer",
         "in":[{"wires":[{"id":"midInstance"}]}],
         "out":[{"wires":[{"id":"midInstance","port":0}]}],
         "env":[{"name":"LEVEL","type":"str","value":"outer"}]},
        {"id":"midInstance","type":"subflow:inner","z":"outer","x":1,"y":1,"wires":[[]],
         "env":[{"name":"LEVEL","type":"str","value":"middle"}]},
        {"id":"inner","type":"subflow","name":"Inner",
         "in":[{"wires":[{"id":"leaf"}]}],
         "out":[{"wires":[{"id":"leaf","port":0}]}]},
        {"id":"leaf","type":"function","z":"inner","x":1,"y":1,"wires":[[]]},
        {"id":"i1","type":"subflow:outer","z":"tab1","x":1,"y":1,"wires":[["sink"]]},
        {"id":"sink","type":"debug","z":"tab1","x":2,"y":1,"wires":[]}
    ]`)

	ex := ExpandSubflows(f)
	out := ex.Flows

	midID := "i1" + SubflowScopeSeparator + "midInstance"
	leafID := midID + SubflowScopeSeparator + "leaf"

	if _, ok := out.Nodes[leafID]; !ok {
		var ids []string
		for id := range out.Nodes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		t.Fatalf("the nested leaf %s was not expanded; nodes are %v", leafID, ids)
	}

	// The message has to make it all the way back out to the tab's sink.
	if !reflect.DeepEqual(out.Nodes[leafID].Wires, [][]string{{"sink"}}) {
		t.Errorf("the leaf leaves to %v, want the outermost external target", out.Nodes[leafID].Wires)
	}

	// The chain is innermost first, so the middle instance's LEVEL shadows the
	// outer template's.
	chain := ex.EnvChains[leafID]
	if len(chain) < 2 {
		t.Fatalf("the leaf's environment chain has %d frames, want the nested scopes", len(chain))
	}
	var level any
	for _, ev := range chain[0].Vars {
		if ev.Name == "LEVEL" {
			level = ev.Value
		}
	}
	if level != "middle" {
		t.Errorf("the innermost LEVEL is %#v, want the middle instance's", level)
	}
	if ex.ParentScope[midID] != "i1" {
		t.Errorf("ParentScope[%s] = %q", midID, ex.ParentScope[midID])
	}
}

// A subflow containing an instance of itself would expand forever.
func TestExpandRefusesASubflowThatContainsItself(t *testing.T) {
	f := parse(t, `[
        {"id":"tab1","type":"tab","label":"Main"},
        {"id":"sf1","type":"subflow","name":"Recursive",
         "in":[{"wires":[{"id":"self"}]}],"out":[]},
        {"id":"self","type":"subflow:sf1","z":"sf1","x":1,"y":1,"wires":[[]]},
        {"id":"i1","type":"subflow:sf1","z":"tab1","x":1,"y":1,"wires":[]}
    ]`)

	ex := ExpandSubflows(f)
	if len(ex.Warnings) == 0 {
		t.Fatal("a recursive subflow expanded without complaint")
	}
	joined := strings.Join(ex.Warnings, " ")
	if !strings.Contains(joined, "cycle") {
		t.Errorf("warnings %q do not say what went wrong", joined)
	}
	if len(ex.Flows.Nodes) > 1000 {
		t.Fatalf("expansion produced %d nodes; the depth bound did not hold", len(ex.Flows.Nodes))
	}
}

// One connection, not one per instance. A broker inside a subflow is an author
// saying "share this", and copying it opens a connection per instance against
// something that is probably counting them.
func TestExpandSharesConfigNodesDeclaredInATemplate(t *testing.T) {
	f := parse(t, `[
        {"id":"tab1","type":"tab","label":"Main"},
        {"id":"sf1","type":"subflow","name":"S",
         "in":[{"wires":[{"id":"inner"}]}],"out":[]},
        {"id":"broker","type":"mqtt-broker","z":"sf1","broker":"mqtt.local"},
        {"id":"inner","type":"mqtt out","z":"sf1","x":1,"y":1,"broker":"broker","wires":[[]]},
        {"id":"a","type":"subflow:sf1","z":"tab1","x":1,"y":1,"wires":[]},
        {"id":"b","type":"subflow:sf1","z":"tab1","x":1,"y":2,"wires":[]}
    ]`)

	out := ExpandSubflows(f).Flows

	if _, ok := out.Nodes["broker"]; !ok {
		t.Fatal("the config node declared inside the template is not in the runnable graph")
	}
	for _, id := range []string{
		"a" + SubflowScopeSeparator + "broker",
		"b" + SubflowScopeSeparator + "broker",
	} {
		if _, ok := out.Nodes[id]; ok {
			t.Errorf("the config node was copied as %s; that is a connection per instance", id)
		}
	}
	// And the copies still reference it by its original id, which is what the
	// shared instance is registered under.
	inner := out.Nodes["a"+SubflowScopeSeparator+"inner"]
	if inner == nil || inner.Raw["broker"] != "broker" {
		t.Fatalf("the copy references %#v", inner)
	}
}

// A v1 wire names a target node, not a target port, so a second input is
// unreachable. Saying so beats delivering to the first and looking correct.
func TestExpandWarnsAboutUnreachableInputs(t *testing.T) {
	f := parse(t, `[
        {"id":"tab1","type":"tab","label":"Main"},
        {"id":"sf1","type":"subflow","name":"S",
         "in":[{"wires":[{"id":"one"}]},{"wires":[{"id":"two"}]}],"out":[]},
        {"id":"one","type":"function","z":"sf1","x":1,"y":1,"wires":[[]]},
        {"id":"two","type":"function","z":"sf1","x":1,"y":2,"wires":[[]]},
        {"id":"i1","type":"subflow:sf1","z":"tab1","x":1,"y":1,"wires":[]}
    ]`)

	ex := ExpandSubflows(f)
	if len(ex.Warnings) == 0 {
		t.Fatal("two subflow inputs expanded with no warning")
	}
	entry := ex.Flows.Nodes["i1"]
	if !reflect.DeepEqual(entry.Wires, [][]string{{"i1" + SubflowScopeSeparator + "one"}}) {
		t.Fatalf("the entry forwards to %v, want only the first input's targets", entry.Wires)
	}
}

func TestExpandCopiesGroupsPerInstance(t *testing.T) {
	f := parse(t, `[
        {"id":"tab1","type":"tab","label":"Main"},
        {"id":"sf1","type":"subflow","name":"S","in":[{"wires":[{"id":"inner"}]}],"out":[]},
        {"id":"grp","type":"group","z":"sf1","nodes":["inner"]},
        {"id":"inner","type":"function","z":"sf1","g":"grp","x":1,"y":1,"wires":[[]]},
        {"id":"i1","type":"subflow:sf1","z":"tab1","x":1,"y":1,"wires":[]}
    ]`)

	out := ExpandSubflows(f).Flows
	innerID := "i1" + SubflowScopeSeparator + "inner"
	groupID := "i1" + SubflowScopeSeparator + "grp"

	if out.Nodes[innerID].G != groupID {
		t.Fatalf("the copy belongs to group %q, want the per-instance copy", out.Nodes[innerID].G)
	}
	g, ok := out.Groups[groupID]
	if !ok {
		t.Fatal("the group was not copied; the Catch distance rule would not work inside the instance")
	}
	if g.Z != "i1" || !reflect.DeepEqual(g.Nodes, []string{innerID}) {
		t.Fatalf("the copied group is %#v", g)
	}
}

func TestSplitDerivedID(t *testing.T) {
	id := derivedID("inst1", "node9")
	scope, tmpl, ok := SplitDerivedID(id)
	if !ok || scope != "inst1" || tmpl != "node9" {
		t.Fatalf("SplitDerivedID(%q) = %q, %q, %v", id, scope, tmpl, ok)
	}
	if _, _, ok := SplitDerivedID("plainid"); ok {
		t.Error("a plain id was read as derived")
	}
}
