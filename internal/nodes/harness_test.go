package nodes

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
	"github.com/embernet-ai/emberwire/internal/store"
)

// testServices is a node.Services standing on real context stores, so context
// behaviour under test is the behaviour that ships.
type testServices struct {
	contexts *store.ScopedContexts
	creds    map[string]string
	env      map[string]string
	nodeID   string
	flowID   string
}

func newTestServices() *testServices {
	return &testServices{
		contexts: store.NewScopedContexts(),
		creds:    map[string]string{},
		env:      map[string]string{},
		nodeID:   "test-node",
		flowID:   "test-flow",
	}
}

func (s *testServices) Context(scope node.ContextScope) node.Context {
	switch scope {
	case node.ScopeGlobal:
		return s.contexts.Global()
	case node.ScopeFlow:
		return s.contexts.Flow(s.flowID)
	default:
		return s.contexts.Node(s.nodeID)
	}
}

func (s *testServices) Credential(key string) (string, bool) { v, ok := s.creds[key]; return v, ok }
func (s *testServices) ConfigNode(string) (node.Node, bool)  { return nil, false }
func (s *testServices) Env(name string) (string, bool)       { v, ok := s.env[name]; return v, ok }
func (s *testServices) Log(node.LogLevel, string, ...any)    {}

// testEmitter records everything a node emits.
type testEmitter struct {
	mu        sync.Mutex
	sent      map[int][]*engine.Msg
	statuses  []node.Status
	errs      []error
	dones     int
	published []publishedEvent
}

func newTestEmitter() *testEmitter {
	return &testEmitter{sent: map[int][]*engine.Msg{}}
}

func (e *testEmitter) Send(port int, m *engine.Msg) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent[port] = append(e.sent[port], m)
}

func (e *testEmitter) SendAll(byPort [][]*engine.Msg) {
	for port, msgs := range byPort {
		for _, m := range msgs {
			if m != nil {
				e.Send(port, m)
			}
		}
	}
}

func (e *testEmitter) Status(s node.Status) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.statuses = append(e.statuses, s)
}

func (e *testEmitter) Error(err error, _ *engine.Msg) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.errs = append(e.errs, err)
}

func (e *testEmitter) Done(*engine.Msg, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dones++
}

func (e *testEmitter) Log(node.LogLevel, string, ...any) {}

func (e *testEmitter) Publish(topic string, data map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.published = append(e.published, publishedEvent{Topic: topic, Data: data})
}

type publishedEvent struct {
	Topic string
	Data  map[string]any
}

// publishedOn returns the events published on a topic.
func (e *testEmitter) publishedOn(topic string) []publishedEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []publishedEvent
	for _, p := range e.published {
		if p.Topic == topic {
			out = append(out, p)
		}
	}
	return out
}

// on returns the messages emitted on a port.
func (e *testEmitter) on(port int) []*engine.Msg {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*engine.Msg(nil), e.sent[port]...)
}

// ports returns which ports received anything.
func (e *testEmitter) ports() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []int
	for p, msgs := range e.sent {
		if len(msgs) > 0 {
			out = append(out, p)
		}
	}
	return out
}

func (e *testEmitter) total() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, msgs := range e.sent {
		n += len(msgs)
	}
	return n
}

// parseNodeConfig turns a JSON config fragment into a parsed flow entry.
//
// It goes through engine.ParseFlows rather than constructing engine.Node by
// hand, so a test exercises the same path a deploy does — including the typed
// fields the parser derives from the raw map. Hand-building the struct hid a
// bug once already: the harness left Node.Name empty, so a node reading it
// looked broken in a test that was itself wrong.
func parseNodeConfig(t *testing.T, typ, configJSON string) *engine.Node {
	t.Helper()

	var raw map[string]any
	if err := json.Unmarshal([]byte(configJSON), &raw); err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	raw["id"] = "test-node"
	raw["type"] = typ
	if _, ok := raw["z"]; !ok {
		raw["z"] = "test-flow"
	}
	// Coordinates and wires make this a flow node rather than a config node,
	// which is the structural rule the parser applies.
	if _, ok := raw["x"]; !ok {
		raw["x"] = 100.0
	}
	if _, ok := raw["y"]; !ok {
		raw["y"] = 100.0
	}
	if _, ok := raw["wires"]; !ok {
		raw["wires"] = []any{[]any{}}
	}

	doc, err := json.Marshal([]any{
		map[string]any{"id": "test-flow", "type": "tab", "label": "test"},
		raw,
	})
	if err != nil {
		t.Fatalf("building flow document: %v", err)
	}
	flows, err := engine.ParseFlows(doc)
	if err != nil {
		t.Fatalf("parsing flow document: %v", err)
	}
	en, ok := flows.Nodes["test-node"]
	if !ok {
		t.Fatal("the node did not survive parsing")
	}
	return en
}

// build constructs a node of the given type from a JSON config fragment.
//
// Taking the config as JSON rather than a Go literal is deliberate: it is the
// same shape the flow file carries, so a test exercises the real parsing path
// including the string-vs-number quirks Node-RED edit dialogs produce.
func build(t *testing.T, typ, configJSON string, svc node.Services) node.Node {
	t.Helper()

	reg, ok := node.Default.Lookup(typ)
	if !ok {
		t.Fatalf("node type %q is not registered", typ)
	}
	n, err := reg.New(&node.Definition{Node: parseNodeConfig(t, typ, configJSON), Services: svc})
	if err != nil {
		t.Fatalf("building %s: %v", typ, err)
	}
	return n
}

// jsonConfig renders a config map as the JSON fragment build takes. Worth having
// whenever a property value is multi-line or full of braces, where escaping it
// into a Go string literal by hand is how a test ends up asserting on a typo.
func jsonConfig(m map[string]any) (string, error) {
	b, err := json.Marshal(m)
	return string(b), err
}

// buildErr expects construction to fail, returning the error.
func buildErr(t *testing.T, typ, configJSON string, svc node.Services) error {
	t.Helper()

	reg, ok := node.Default.Lookup(typ)
	if !ok {
		t.Fatalf("node type %q is not registered", typ)
	}
	_, err := reg.New(&node.Definition{Node: parseNodeConfig(t, typ, configJSON), Services: svc})
	return err
}

// send pushes a message through a node and returns the emitter.
func send(t *testing.T, n node.Node, m *engine.Msg) (*testEmitter, error) {
	t.Helper()
	e := newTestEmitter()
	err := n.Receive(context.Background(), m, e)
	return e, err
}

// msg builds a message from a JSON object, which is how a payload actually
// arrives over MQTT or HTTP.
func msg(t *testing.T, jsonObj string) *engine.Msg {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal([]byte(jsonObj), &data); err != nil {
		t.Fatalf("parsing message: %v", err)
	}
	return engine.WrapMsg(data)
}

// TestEveryRegisteredNodeHasAValidDescriptor guards the whole palette.
//
// Descriptors are validated at registration, so this mostly asserts that the
// package init ran — but it also pins the invariants that only matter in
// aggregate, such as every node declaring how it relates to its Node-RED
// counterpart.
func TestEveryRegisteredNodeHasAValidDescriptor(t *testing.T) {
	descs := node.Default.Descriptors()
	if len(descs) == 0 {
		t.Fatal("no node types are registered")
	}
	for _, d := range descs {
		if err := d.Validate(); err != nil {
			t.Errorf("%s: %v", d.Type, err)
		}
		if d.Help == "" {
			t.Errorf("%s: no help text; it would show an empty info sidebar", d.Type)
		}
		if d.Color == "" {
			t.Errorf("%s: no colour", d.Type)
		}
		if d.Icon == "" {
			t.Errorf("%s: no icon", d.Type)
		}
	}
}

// TestPartialNodesDeclareTheirGaps is the honesty check.
//
// A node that is partially compatible and silent about it is worse than one
// that is obviously missing: the flow appears to work and quietly does the
// wrong thing. Validate already requires notes; this additionally requires that
// the notes actually say what is missing.
func TestPartialNodesDeclareTheirGaps(t *testing.T) {
	for _, d := range node.Default.Descriptors() {
		switch d.Compatibility.Level {
		case node.CompatPartial, node.CompatDivergent:
			if len(d.Compatibility.Notes) < 20 {
				t.Errorf("%s: compatibility notes are too terse to be useful: %q",
					d.Type, d.Compatibility.Notes)
			}
		}
	}
}
