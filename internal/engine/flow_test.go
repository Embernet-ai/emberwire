package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func loadKitchenSink(t *testing.T) (*Flows, []byte) {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "flows_kitchen_sink.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	f, err := ParseFlows(data)
	if err != nil {
		t.Fatalf("ParseFlows: %v", err)
	}
	return f, data
}

func TestParseClassifiesEntries(t *testing.T) {
	f, _ := loadKitchenSink(t)

	if got, want := len(f.Tabs), 2; got != want {
		t.Errorf("tabs = %d, want %d", got, want)
	}
	if got, want := len(f.Subflows), 1; got != want {
		t.Errorf("subflows = %d, want %d", got, want)
	}
	if got, want := len(f.Groups), 2; got != want {
		t.Errorf("groups = %d, want %d", got, want)
	}
	// Everything that is not a tab, subflow or group is a node — including the
	// config node and the node type this build does not implement.
	if got, want := len(f.Nodes), 9; got != want {
		t.Errorf("nodes = %d, want %d", got, want)
	}
	if got, want := len(f.Order), 14; got != want {
		t.Errorf("order entries = %d, want %d", got, want)
	}
}

func TestParseConfigNodeDetection(t *testing.T) {
	f, _ := loadKitchenSink(t)

	// The structural rule: no x, no y, no wires means configuration node.
	broker := f.Nodes["cfg0broker000001"]
	if broker == nil {
		t.Fatal("mqtt-broker config node missing")
	}
	if !broker.IsConfig {
		t.Error("mqtt-broker should be detected as a config node")
	}
	if broker.Z != "" {
		t.Errorf("config node Z = %q, want \"\" (global scope)", broker.Z)
	}

	if got := f.ConfigNodes(); !reflect.DeepEqual(got, []string{"cfg0broker000001"}) {
		t.Errorf("ConfigNodes() = %v, want [cfg0broker000001]", got)
	}

	// A node with coordinates is never a config node.
	if f.Nodes["c3c3c3c3c3c3c3c3"].IsConfig {
		t.Error("debug node wrongly classified as config")
	}
}

func TestParseWires(t *testing.T) {
	f, _ := loadKitchenSink(t)

	mqtt := f.Nodes["a1a1a1a1a1a1a1a1"]
	if got, want := mqtt.Outputs(), 1; got != want {
		t.Fatalf("mqtt in outputs = %d, want %d", got, want)
	}
	want := []string{"b2b2b2b2b2b2b2b2", "d4d4d4d4d4d4d4d4"}
	if !reflect.DeepEqual(mqtt.Wires[0], want) {
		t.Errorf("mqtt in wires[0] = %v, want %v", mqtt.Wires[0], want)
	}

	// A node with an empty wires array has zero outputs, not one.
	dbg := f.Nodes["c3c3c3c3c3c3c3c3"]
	if got := dbg.Outputs(); got != 0 {
		t.Errorf("debug outputs = %d, want 0", got)
	}
}

func TestParseSubflow(t *testing.T) {
	f, _ := loadKitchenSink(t)

	sf := f.Subflows["5f6e7d8c9b0a1234"]
	if sf == nil {
		t.Fatal("subflow template missing")
	}
	if sf.Name != "Scale Reading" {
		t.Errorf("subflow name = %q", sf.Name)
	}
	if len(sf.In) != 1 || len(sf.Out) != 1 {
		t.Fatalf("subflow ports: in=%d out=%d, want 1 and 1", len(sf.In), len(sf.Out))
	}
	if got, want := sf.In[0].Wires[0].ID, "1111222233334444"; got != want {
		t.Errorf("subflow input wires to %q, want %q", got, want)
	}
	if got, want := len(sf.Env), 2; got != want {
		t.Errorf("subflow env vars = %d, want %d", got, want)
	}

	// The instance resolves back to its template.
	inst := f.Nodes["b2b2b2b2b2b2b2b2"]
	tmpl, isInstance := inst.SubflowTemplateID()
	if !isInstance {
		t.Fatal("subflow instance not detected")
	}
	if tmpl != "5f6e7d8c9b0a1234" {
		t.Errorf("instance template = %q", tmpl)
	}

	// A node whose type merely contains "subflow" is not an instance.
	other := &Node{Type: "subflow-ish"}
	if _, isInstance := other.SubflowTemplateID(); isInstance {
		t.Error("type \"subflow-ish\" wrongly detected as a subflow instance")
	}
}

func TestParseGroupHierarchy(t *testing.T) {
	f, _ := loadKitchenSink(t)

	if got := f.GroupDepth("9999888877776666"); got != 1 {
		t.Errorf("outer group depth = %d, want 1", got)
	}
	if got := f.GroupDepth("7777666655554444"); got != 2 {
		t.Errorf("inner group depth = %d, want 2", got)
	}

	// The chain is innermost first — that ordering is what lets Catch pick the
	// closest handler.
	chain := f.GroupChain("b2b2b2b2b2b2b2b2")
	want := []string{"7777666655554444", "9999888877776666"}
	if !reflect.DeepEqual(chain, want) {
		t.Errorf("GroupChain = %v, want %v", chain, want)
	}

	if got := f.GroupChain("c3c3c3c3c3c3c3c3"); got != nil {
		t.Errorf("ungrouped node chain = %v, want nil", got)
	}
}

func TestParseDisabledNode(t *testing.T) {
	f, _ := loadKitchenSink(t)
	if !f.Nodes["d4d4d4d4d4d4d4d4"].Disabled {
		t.Error(`node with "d": true not marked disabled`)
	}
	if f.Nodes["c3c3c3c3c3c3c3c3"].Disabled {
		t.Error("node without \"d\" wrongly marked disabled")
	}
	if !f.Tabs["aaaabbbbccccdddd"].Disabled {
		t.Error("tab with \"disabled\": true not marked disabled")
	}
}

func TestParseWarnsNotFails(t *testing.T) {
	f, _ := loadKitchenSink(t)

	// A dangling wire must be reported, not fatal. Node-RED loads these, and
	// refusing to would make a partially-pasted flow unopenable.
	var foundDangling bool
	for _, w := range f.Warnings {
		if strings.Contains(w, "this-node-does-not-exist") {
			foundDangling = true
		}
	}
	if !foundDangling {
		t.Errorf("dangling wire not reported; warnings = %v", f.Warnings)
	}
	// And it must still have parsed.
	if f.Nodes["dead0beef0000001"] == nil {
		t.Error("node with a dangling wire was dropped")
	}
}

func TestNodesInScope(t *testing.T) {
	f, _ := loadKitchenSink(t)

	got := f.NodesInScope("f1a2b3c4d5e6f708")
	want := []string{
		"a1a1a1a1a1a1a1a1",
		"b2b2b2b2b2b2b2b2",
		"c3c3c3c3c3c3c3c3",
		"d4d4d4d4d4d4d4d4",
		"e5e5e5e5e5e5e5e5",
		"dead0beef0000001",
		"0000111122223333",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("NodesInScope(tab) = %v\nwant %v", got, want)
	}

	// Subflow members are scoped to the subflow id, not to a tab.
	if got := f.NodesInScope("5f6e7d8c9b0a1234"); !reflect.DeepEqual(got, []string{"1111222233334444"}) {
		t.Errorf("NodesInScope(subflow) = %v", got)
	}
}

func TestRoundTripPreservesUnknownProperties(t *testing.T) {
	// The property that makes it safe to run a Node-RED-authored flow here and
	// hand it back: a node type this build has never heard of survives a
	// load-and-save with every property intact.
	f, orig := loadKitchenSink(t)

	out, err := f.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var before, after []map[string]any
	if err := json.Unmarshal(orig, &before); err != nil {
		t.Fatalf("unmarshal original: %v", err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatalf("unmarshal round-tripped: %v", err)
	}

	if len(before) != len(after) {
		t.Fatalf("entry count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if !reflect.DeepEqual(before[i], after[i]) {
			t.Errorf("entry %d changed across round-trip:\n before: %#v\n after:  %#v", i, before[i], after[i])
		}
	}
}

func TestRoundTripIsStableAcrossSaves(t *testing.T) {
	// Key order is not preserved (Go maps are unordered and encoding/json sorts
	// keys), so the first save after a Node-RED import reorders keys once. What
	// must hold is that every save after that is byte-identical, or the flow
	// file churns in git and on the PVC on every deploy.
	f, _ := loadKitchenSink(t)

	first, err := f.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	reparsed, err := ParseFlows(first)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	second, err := reparsed.Marshal()
	if err != nil {
		t.Fatalf("second Marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Error("save output is not stable across a save/load/save cycle")
	}
}

func TestStripCredentials(t *testing.T) {
	f, _ := loadKitchenSink(t)

	creds := f.StripCredentials()
	got, ok := creds["e5e5e5e5e5e5e5e5"]
	if !ok {
		t.Fatal("http request credentials were not extracted")
	}
	if got["password"] != "hunter2" {
		t.Errorf("extracted password = %#v", got["password"])
	}

	// And they must be gone from the flow, or a deploy writes a plaintext
	// password into flows.json on the PVC.
	if _, still := f.Nodes["e5e5e5e5e5e5e5e5"].Raw["credentials"]; still {
		t.Error("credentials still inline on the node after StripCredentials")
	}
	out, err := f.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(out), "hunter2") {
		t.Error("serialised flow still contains the credential value")
	}
}

func TestParseWrappedForm(t *testing.T) {
	// The admin API returns {"rev":...,"flows":[...]}; the file on disk is the
	// bare array. Both must load.
	wrapped := []byte(`{"rev":"abc123","flows":[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"n1","type":"debug","z":"t1","x":10,"y":10,"wires":[]}
    ]}`)
	f, err := ParseFlows(wrapped)
	if err != nil {
		t.Fatalf("ParseFlows(wrapped): %v", err)
	}
	if f.Rev != "abc123" {
		t.Errorf("rev = %q, want abc123", f.Rev)
	}
	if len(f.Nodes) != 1 || len(f.Tabs) != 1 {
		t.Errorf("wrapped form parsed to %d nodes, %d tabs", len(f.Nodes), len(f.Tabs))
	}
}

func TestParseEmptyAndWhitespace(t *testing.T) {
	for _, in := range []string{"", "  \n\t ", "[]"} {
		f, err := ParseFlows([]byte(in))
		if err != nil {
			t.Errorf("ParseFlows(%q) error: %v", in, err)
			continue
		}
		if len(f.Order) != 0 {
			t.Errorf("ParseFlows(%q) produced %d entries, want 0", in, len(f.Order))
		}
	}
}

func TestParseFatalErrors(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"not json", `this is not json`},
		{"not an array", `{"nope": true}`},
		{"missing id", `[{"type":"debug"}]`},
		{"missing type", `[{"id":"a"}]`},
		{"duplicate id", `[{"id":"a","type":"debug"},{"id":"a","type":"inject"}]`},
		{"wires not array", `[{"id":"a","type":"debug","x":1,"y":1,"wires":"nope"}]`},
		{"wires port not array", `[{"id":"a","type":"debug","x":1,"y":1,"wires":["nope"]}]`},
		{"wire target not string", `[{"id":"a","type":"debug","x":1,"y":1,"wires":[[123]]}]`},
	}
	for _, c := range cases {
		if _, err := ParseFlows([]byte(c.in)); err == nil {
			t.Errorf("%s: ParseFlows succeeded, want error", c.name)
		}
	}
}

func TestNodePropAccessors(t *testing.T) {
	f, _ := loadKitchenSink(t)
	n := f.Nodes["cfg0broker000001"]

	if got := n.PropString("broker", ""); got != "10.20.30.40" {
		t.Errorf("PropString(broker) = %q", got)
	}
	if got := n.PropString("missing", "fallback"); got != "fallback" {
		t.Errorf("PropString default = %q", got)
	}
	// Edit dialogs persist numbers as strings often enough that both forms must
	// work — "port": "1883" here is a string in the real Node-RED export.
	if got := n.PropInt("port", 0); got != 1883 {
		t.Errorf("PropInt(port) = %d, want 1883", got)
	}
	if got := n.PropBool("cleansession", false); !got {
		t.Error("PropBool(cleansession) = false, want true")
	}
	if got := n.PropBool("missing", true); !got {
		t.Error("PropBool default not returned")
	}

	fn := f.Nodes["d4d4d4d4d4d4d4d4"]
	if got := fn.PropInt("outputs", 0); got != 1 {
		t.Errorf("PropInt(outputs) = %d, want 1", got)
	}
	if _, ok := fn.Prop("func"); !ok {
		t.Error("Prop(func) not found")
	}
}

func TestGroupDepthTerminatesOnCycle(t *testing.T) {
	// A hand-edited flow can contain a group cycle. It must not hang the parser.
	in := []byte(`[
        {"id":"g1","type":"group","z":"t","g":"g2","nodes":[]},
        {"id":"g2","type":"group","z":"t","g":"g1","nodes":[]},
        {"id":"n1","type":"debug","z":"t","g":"g1","x":1,"y":1,"wires":[]}
    ]`)
	f, err := ParseFlows(in)
	if err != nil {
		t.Fatalf("ParseFlows: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = f.GroupDepth("g1")
		_ = f.GroupChain("n1")
		close(done)
	}()
	select {
	case <-done:
	case <-timeoutAfterSeconds(5):
		t.Fatal("group traversal did not terminate on a cycle")
	}
}

func BenchmarkParseFlows(b *testing.B) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "flows_kitchen_sink.json"))
	if err != nil {
		b.Fatalf("reading fixture: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseFlows(data); err != nil {
			b.Fatalf("ParseFlows: %v", err)
		}
	}
}
