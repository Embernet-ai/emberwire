package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

// ---------------------------------------------------------------------------
// Test nodes
// ---------------------------------------------------------------------------

// passNode forwards whatever it receives to output 0.
type passNode struct{}

func (passNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	out.Send(0, m)
	return nil
}

// sinkNode records every message it receives.
type sinkNode struct {
	mu   sync.Mutex
	msgs []*engine.Msg
	// gate, when non-nil, blocks the node until closed — used to hold an inbox
	// full on purpose.
	gate <-chan struct{}
}

func (s *sinkNode) Receive(_ context.Context, m *engine.Msg, _ node.Emitter) error {
	if s.gate != nil {
		<-s.gate
	}
	s.mu.Lock()
	s.msgs = append(s.msgs, m)
	s.mu.Unlock()
	return nil
}

func (s *sinkNode) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.msgs)
}

func (s *sinkNode) all() []*engine.Msg {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*engine.Msg(nil), s.msgs...)
}

// mutateNode edits the message in place, to prove recipients are isolated.
type mutateNode struct{ tag string }

func (n mutateNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	_ = m.Set("payload.seen", n.tag)
	out.Send(0, m)
	return nil
}

// failNode always errors.
type failNode struct{ err error }

func (n failNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return n.err }

// panicNode panics, to prove one bad node cannot take the process down.
type panicNode struct{}

func (panicNode) Receive(context.Context, *engine.Msg, node.Emitter) error {
	panic("deliberate panic from a test node")
}

// statusNode sets a badge when it receives anything.
type statusNode struct{ s node.Status }

func (n statusNode) Receive(_ context.Context, _ *engine.Msg, out node.Emitter) error {
	out.Status(n.s)
	return nil
}

// doneNode signals completion, driving Complete nodes.
type doneNode struct{}

func (doneNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	out.Done(m, nil)
	return nil
}

// sourceNode emits n messages as fast as it can once started.
type sourceNode struct {
	n    int
	sent atomic.Int64
	done chan struct{}
}

func (s *sourceNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (s *sourceNode) Start(ctx context.Context, out node.Emitter) error {
	go func() {
		defer close(s.done)
		for i := 0; i < s.n; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			out.Send(0, engine.NewMsgWithPayload(float64(i)))
			s.sent.Add(1)
		}
	}()
	return nil
}

// closeNode records that it was closed.
type closeNode struct{ closed atomic.Bool }

func (c *closeNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }
func (c *closeNode) Close(context.Context, bool) error {
	c.closed.Store(true)
	return nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// testRegistry builds a registry whose factories return caller-supplied
// instances, so a test can hold a pointer to the node it wired in.
type testRegistry struct {
	*node.Registry
	instances map[string]node.Node
}

func newTestRegistry() *testRegistry {
	return &testRegistry{Registry: node.NewRegistry(), instances: map[string]node.Node{}}
}

// add registers a node type whose factory hands back inst for the node with the
// given id, or a fresh zero instance otherwise.
func (tr *testRegistry) add(typ string, inputs, outputs int, build func(id string) node.Node) {
	d := node.Descriptor{
		Type:          typ,
		Category:      node.CategoryCommon,
		Color:         "#E31837",
		Icon:          "cog",
		Inputs:        inputs,
		Outputs:       outputs,
		Compatibility: node.Compatibility{Level: node.CompatOnly},
	}
	err := tr.Register(d, func(def *node.Definition) (node.Node, error) {
		n := build(def.Node.ID)
		tr.instances[def.Node.ID] = n
		return n, nil
	})
	if err != nil {
		panic(err)
	}
}

func mustFlows(t *testing.T, js string) *engine.Flows {
	t.Helper()
	f, err := engine.ParseFlows([]byte(js))
	if err != nil {
		t.Fatalf("ParseFlows: %v", err)
	}
	return f
}

// waitFor polls until cond holds or the deadline passes. Used instead of a
// fixed sleep so the tests are neither flaky nor slow.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// drain collects events until the runtime stops, so tests can assert on them.
func drain(rt *Runtime) *eventLog {
	el := &eventLog{}
	go func() {
		for e := range rt.Events() {
			el.mu.Lock()
			el.events = append(el.events, e)
			el.mu.Unlock()
		}
	}()
	return el
}

type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (el *eventLog) byTopic(topic string) []Event {
	el.mu.Lock()
	defer el.mu.Unlock()
	var out []Event
	for _, e := range el.events {
		if e.Topic == topic {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Delivery
// ---------------------------------------------------------------------------

func TestDeliverAlongWires(t *testing.T) {
	tr := newTestRegistry()
	sink := &sinkNode{}
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"a","type":"pass","z":"t1","x":1,"y":1,"wires":[["b"]]},
        {"id":"b","type":"sink","z":"t1","x":2,"y":1,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("Start failures: %v", fails)
	}
	defer rt.Stop(context.Background())

	if err := rt.Inject("a", engine.NewMsgWithPayload("hello")); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	waitFor(t, "message to arrive at sink", func() bool { return sink.count() == 1 })

	if got := sink.all()[0].Payload(); got != "hello" {
		t.Errorf("payload = %#v, want \"hello\"", got)
	}
}

func TestFanOutIsolatesRecipients(t *testing.T) {
	// The divergence from Node-RED that matters: every recipient on a wire gets
	// its own copy, so two branches editing "their" message cannot collide.
	tr := newTestRegistry()
	sinkA, sinkB := &sinkNode{}, &sinkNode{}
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("mutA", 1, 1, func(string) node.Node { return mutateNode{tag: "A"} })
	tr.add("mutB", 1, 1, func(string) node.Node { return mutateNode{tag: "B"} })
	tr.add("sinkA", 1, 0, func(string) node.Node { return sinkA })
	tr.add("sinkB", 1, 0, func(string) node.Node { return sinkB })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"src","type":"pass","z":"t1","x":1,"y":1,"wires":[["ma","mb"]]},
        {"id":"ma","type":"mutA","z":"t1","x":2,"y":1,"wires":[["sa"]]},
        {"id":"mb","type":"mutB","z":"t1","x":2,"y":2,"wires":[["sb"]]},
        {"id":"sa","type":"sinkA","z":"t1","x":3,"y":1,"wires":[]},
        {"id":"sb","type":"sinkB","z":"t1","x":3,"y":2,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	m := engine.NewMsg()
	m.SetPayload(map[string]any{"v": 1.0})
	rt.Inject("src", m)

	waitFor(t, "both branches", func() bool { return sinkA.count() == 1 && sinkB.count() == 1 })

	gotA, _, _ := sinkA.all()[0].Get("payload.seen")
	gotB, _, _ := sinkB.all()[0].Get("payload.seen")
	if gotA != "A" {
		t.Errorf("branch A saw %#v, want \"A\"", gotA)
	}
	if gotB != "B" {
		t.Errorf("branch B saw %#v, want \"B\" — the branches shared a message", gotB)
	}
}

func TestMultiplePortsAndSendAll(t *testing.T) {
	tr := newTestRegistry()
	s0, s1 := &sinkNode{}, &sinkNode{}
	tr.add("split", 1, 2, func(string) node.Node {
		return nodeFunc(func(_ context.Context, m *engine.Msg, out node.Emitter) error {
			// Port 1 gets two messages; port 0 gets one. The nil entry proves a
			// gap in the slice sends nothing rather than panicking.
			out.SendAll([][]*engine.Msg{
				{m},
				{engine.NewMsgWithPayload("x"), engine.NewMsgWithPayload("y")},
			})
			return nil
		})
	})
	tr.add("s0", 1, 0, func(string) node.Node { return s0 })
	tr.add("s1", 1, 0, func(string) node.Node { return s1 })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"sp","type":"split","z":"t1","x":1,"y":1,"wires":[["a"],["b"]]},
        {"id":"a","type":"s0","z":"t1","x":2,"y":1,"wires":[]},
        {"id":"b","type":"s1","z":"t1","x":2,"y":2,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	rt.Inject("sp", engine.NewMsgWithPayload("in"))
	waitFor(t, "both ports", func() bool { return s0.count() == 1 && s1.count() == 2 })
}

// nodeFunc adapts a function to the Node interface.
type nodeFunc func(context.Context, *engine.Msg, node.Emitter) error

func (f nodeFunc) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	return f(ctx, m, out)
}

// ---------------------------------------------------------------------------
// Back-pressure — the headline feature
// ---------------------------------------------------------------------------

// backPressureFlow wires a source into a sink that is held blocked, so the
// sink's inbox saturates and the policy under test has to act.
func backPressureFlow(t *testing.T, policy OverflowPolicy, capacity, produce int) (*Runtime, *sourceNode, *sinkNode, chan struct{}, *eventLog) {
	t.Helper()
	gate := make(chan struct{})
	src := &sourceNode{n: produce, done: make(chan struct{})}
	sink := &sinkNode{gate: gate}

	tr := newTestRegistry()
	tr.add("src", 0, 1, func(string) node.Node { return src })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"s","type":"src","z":"t1","x":1,"y":1,"wires":[["k"]]},
        {"id":"k","type":"sink","z":"t1","x":2,"y":1,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{
		InboxCapacity: capacity,
		Overflow:      policy,
		BlockTimeout:  500 * time.Millisecond,
	})
	el := drain(rt)
	rt.Start(context.Background())
	return rt, src, sink, gate, el
}

func TestBackPressureBlockStallsProducer(t *testing.T) {
	// The property Node-RED cannot provide. Its send queue is an unbounded
	// setImmediate chain, so a fast source outruns a slow sink until the
	// process dies. Here the producer is stalled instead, and memory stays flat.
	const capacity = 8
	rt, src, _, gate, _ := backPressureFlow(t, OverflowBlock, capacity, 10000)
	defer func() { close(gate); rt.Stop(context.Background()) }()

	// The producer can get at most capacity into the queue plus the one the
	// sink is holding, then it must stall.
	time.Sleep(200 * time.Millisecond)
	sent := src.sent.Load()
	if sent > int64(capacity)+4 {
		t.Errorf("producer sent %d messages into a queue of %d — it was not back-pressured", sent, capacity)
	}
	if sent == 0 {
		t.Error("producer sent nothing at all")
	}
}

func TestBackPressureBlockTimesOutRatherThanDeadlocking(t *testing.T) {
	// A flow graph may legally contain a cycle. Under a blocking policy a cycle
	// of saturated inboxes would deadlock permanently, so the wait is bounded
	// and the overrun is reported.
	rt, _, _, gate, el := backPressureFlow(t, OverflowBlock, 2, 200)
	defer func() { close(gate); rt.Stop(context.Background()) }()

	waitFor(t, "a block timeout to be reported", func() bool {
		return len(el.byTopic(TopicError)) > 0
	})
	found := false
	for _, e := range el.byTopic(TopicError) {
		if msg, _ := e.Data["error"].(string); msg != "" &&
			errors.Is(ErrBlockTimeout, ErrBlockTimeout) &&
			contains(msg, "timed out waiting for space") {
			found = true
		}
	}
	if !found {
		t.Error("block timeout did not surface as an error event")
	}
}

func TestBackPressureDropNewest(t *testing.T) {
	const capacity = 4
	rt, _, _, gate, el := backPressureFlow(t, OverflowDropNewest, capacity, 500)
	defer func() { close(gate); rt.Stop(context.Background()) }()

	waitFor(t, "drops to be recorded", func() bool { return len(el.byTopic(TopicDropped)) > 0 })

	// Every drop is announced. Silently discarding a message is how a flow
	// appears healthy while losing data.
	for _, e := range el.byTopic(TopicDropped) {
		if e.Data["policy"] != "drop-newest" {
			t.Errorf("drop event policy = %#v, want drop-newest", e.Data["policy"])
		}
		if e.Data["nodeId"] != "k" {
			t.Errorf("drop attributed to %#v, want k", e.Data["nodeId"])
		}
	}

	snaps := rt.Snapshots()
	var sinkSnap *Snapshot
	for i := range snaps {
		if snaps[i].NodeID == "k" {
			sinkSnap = &snaps[i]
		}
	}
	if sinkSnap == nil {
		t.Fatal("no snapshot for the sink")
	}
	if sinkSnap.Dropped == 0 {
		t.Error("Dropped counter did not move")
	}
	if sinkSnap.QueueCap != capacity {
		t.Errorf("QueueCap = %d, want %d", sinkSnap.QueueCap, capacity)
	}
}

func TestBackPressureDropOldest(t *testing.T) {
	rt, _, _, gate, el := backPressureFlow(t, OverflowDropOldest, 4, 500)
	defer func() { close(gate); rt.Stop(context.Background()) }()

	waitFor(t, "drops to be recorded", func() bool { return len(el.byTopic(TopicDropped)) > 0 })
	for _, e := range el.byTopic(TopicDropped) {
		if e.Data["policy"] != "drop-oldest" {
			t.Errorf("drop event policy = %#v, want drop-oldest", e.Data["policy"])
		}
	}
}

func TestBackPressureErrorPolicyRoutesToCatch(t *testing.T) {
	// Under the error policy the flow decides what to do about saturation,
	// which means the overflow has to reach a Catch node like any other error.
	gate := make(chan struct{})
	defer close(gate)

	src := &sourceNode{n: 500, done: make(chan struct{})}
	sink := &sinkNode{gate: gate}
	caught := &sinkNode{}

	tr := newTestRegistry()
	tr.add("src", 0, 1, func(string) node.Node { return src })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })
	tr.add("catch", 0, 1, func(string) node.Node { return passNode{} })
	tr.add("caught", 1, 0, func(string) node.Node { return caught })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"s","type":"src","z":"t1","x":1,"y":1,"wires":[["k"]]},
        {"id":"k","type":"sink","z":"t1","x":2,"y":1,"wires":[]},
        {"id":"c","type":"catch","z":"t1","scope":null,"x":1,"y":3,"wires":[["out"]]},
        {"id":"out","type":"caught","z":"t1","x":2,"y":3,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{InboxCapacity: 4, Overflow: OverflowError})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	waitFor(t, "overflow to reach the catch node", func() bool { return caught.count() > 0 })

	errObj, ok, _ := caught.all()[0].Get("error.message")
	if !ok {
		t.Fatal("caught message carries no error.message")
	}
	if !contains(fmt.Sprint(errObj), "queue is at capacity") {
		t.Errorf("error.message = %q, want it to mention the full queue", errObj)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 ||
		indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// Error routing
// ---------------------------------------------------------------------------

func TestCatchReceivesError(t *testing.T) {
	tr := newTestRegistry()
	caught := &sinkNode{}
	tr.add("boom", 1, 0, func(string) node.Node { return failNode{err: errors.New("it broke")} })
	tr.add("catch", 0, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return caught })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"b","type":"boom","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"c","type":"catch","z":"t1","x":1,"y":3,"wires":[["s"]]},
        {"id":"s","type":"sink","z":"t1","x":2,"y":3,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	rt.Inject("b", engine.NewMsgWithPayload("x"))
	waitFor(t, "catch node to fire", func() bool { return caught.count() == 1 })

	m := caught.all()[0]
	if got, _, _ := m.Get("error.message"); got != "it broke" {
		t.Errorf("error.message = %#v", got)
	}
	if got, _, _ := m.Get("error.source.id"); got != "b" {
		t.Errorf("error.source.id = %#v, want b", got)
	}
	// The original payload must survive, or a Catch node cannot tell you what
	// the failing message contained.
	if got := m.Payload(); got != "x" {
		t.Errorf("payload = %#v, want \"x\"", got)
	}
}

func TestCatchPrefersClosestGroup(t *testing.T) {
	// Node-RED sorts eligible Catch nodes by distance in the group hierarchy so
	// a Catch inside a group beats a Catch on the tab. Getting this wrong means
	// a local error handler is silently bypassed.
	tr := newTestRegistry()
	inner, outer := &sinkNode{}, &sinkNode{}
	tr.add("boom", 1, 0, func(string) node.Node { return failNode{err: errors.New("x")} })
	tr.add("catch", 0, 1, func(string) node.Node { return passNode{} })
	tr.add("innerSink", 1, 0, func(string) node.Node { return inner })
	tr.add("outerSink", 1, 0, func(string) node.Node { return outer })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"g1","type":"group","z":"t1","nodes":["b","cin"]},
        {"id":"b","type":"boom","z":"t1","g":"g1","x":1,"y":1,"wires":[]},
        {"id":"cin","type":"catch","z":"t1","g":"g1","x":1,"y":2,"wires":[["si"]]},
        {"id":"si","type":"innerSink","z":"t1","x":2,"y":2,"wires":[]},
        {"id":"cout","type":"catch","z":"t1","x":1,"y":4,"wires":[["so"]]},
        {"id":"so","type":"outerSink","z":"t1","x":2,"y":4,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	rt.Inject("b", engine.NewMsgWithPayload("x"))
	waitFor(t, "the in-group catch to fire", func() bool { return inner.count() == 1 })

	time.Sleep(100 * time.Millisecond)
	if outer.count() != 0 {
		t.Error("the tab-level catch also fired; the closest handler should win alone")
	}
}

func TestCatchUncaughtOnlyFiresWhenNothingElseDid(t *testing.T) {
	tr := newTestRegistry()
	normal, uncaught := &sinkNode{}, &sinkNode{}
	tr.add("boom", 1, 0, func(string) node.Node { return failNode{err: errors.New("x")} })
	tr.add("catch", 0, 1, func(string) node.Node { return passNode{} })
	tr.add("n", 1, 0, func(string) node.Node { return normal })
	tr.add("u", 1, 0, func(string) node.Node { return uncaught })

	withNormal := `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"b","type":"boom","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"c1","type":"catch","z":"t1","x":1,"y":2,"wires":[["sn"]]},
        {"id":"sn","type":"n","z":"t1","x":2,"y":2,"wires":[]},
        {"id":"c2","type":"catch","z":"t1","uncaught":true,"x":1,"y":4,"wires":[["su"]]},
        {"id":"su","type":"u","z":"t1","x":2,"y":4,"wires":[]}
    ]`

	rt := New(tr.Registry, mustFlows(t, withNormal), Options{})
	drain(rt)
	rt.Start(context.Background())
	rt.Inject("b", engine.NewMsgWithPayload("x"))
	waitFor(t, "the normal catch to fire", func() bool { return normal.count() == 1 })
	time.Sleep(100 * time.Millisecond)
	if uncaught.count() != 0 {
		t.Error("uncaught catch fired even though a normal catch handled the error")
	}
	rt.Stop(context.Background())

	// Now with only the uncaught handler present.
	normal2, uncaught2 := &sinkNode{}, &sinkNode{}
	tr2 := newTestRegistry()
	tr2.add("boom", 1, 0, func(string) node.Node { return failNode{err: errors.New("x")} })
	tr2.add("catch", 0, 1, func(string) node.Node { return passNode{} })
	tr2.add("n", 1, 0, func(string) node.Node { return normal2 })
	tr2.add("u", 1, 0, func(string) node.Node { return uncaught2 })

	onlyUncaught := `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"b","type":"boom","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"c2","type":"catch","z":"t1","uncaught":true,"x":1,"y":4,"wires":[["su"]]},
        {"id":"su","type":"u","z":"t1","x":2,"y":4,"wires":[]}
    ]`
	rt2 := New(tr2.Registry, mustFlows(t, onlyUncaught), Options{})
	drain(rt2)
	rt2.Start(context.Background())
	defer rt2.Stop(context.Background())
	rt2.Inject("b", engine.NewMsgWithPayload("x"))
	waitFor(t, "the uncaught catch to fire", func() bool { return uncaught2.count() == 1 })
}

func TestCatchScopeLimitsWhichNodesAreWatched(t *testing.T) {
	tr := newTestRegistry()
	caught := &sinkNode{}
	tr.add("boom", 1, 0, func(string) node.Node { return failNode{err: errors.New("x")} })
	tr.add("catch", 0, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return caught })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"b1","type":"boom","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"b2","type":"boom","z":"t1","x":1,"y":2,"wires":[]},
        {"id":"c","type":"catch","z":"t1","scope":["b1"],"x":1,"y":4,"wires":[["s"]]},
        {"id":"s","type":"sink","z":"t1","x":2,"y":4,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	rt.Inject("b2", engine.NewMsgWithPayload("out-of-scope"))
	time.Sleep(100 * time.Millisecond)
	if caught.count() != 0 {
		t.Error("catch fired for a node outside its scope")
	}

	rt.Inject("b1", engine.NewMsgWithPayload("in-scope"))
	waitFor(t, "in-scope error to be caught", func() bool { return caught.count() == 1 })
}

func TestCatchDoesNotCrossFlows(t *testing.T) {
	tr := newTestRegistry()
	caught := &sinkNode{}
	tr.add("boom", 1, 0, func(string) node.Node { return failNode{err: errors.New("x")} })
	tr.add("catch", 0, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return caught })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"One"},
        {"id":"t2","type":"tab","label":"Two"},
        {"id":"b","type":"boom","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"c","type":"catch","z":"t2","x":1,"y":1,"wires":[["s"]]},
        {"id":"s","type":"sink","z":"t2","x":2,"y":1,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	rt.Inject("b", engine.NewMsgWithPayload("x"))
	time.Sleep(150 * time.Millisecond)
	if caught.count() != 0 {
		t.Error("a Catch node on another tab caught the error")
	}
}

func TestErrorLoopIsBroken(t *testing.T) {
	// A Catch wired back into the flow it catches for is an easy infinite loop.
	// It must terminate, and say why.
	tr := newTestRegistry()
	tr.add("boom", 1, 0, func(string) node.Node { return failNode{err: errors.New("recursive")} })
	tr.add("catch", 0, 1, func(string) node.Node { return passNode{} })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"b","type":"boom","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"c","type":"catch","z":"t1","x":1,"y":2,"wires":[["b"]]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	el := drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	rt.Inject("b", engine.NewMsgWithPayload("x"))

	waitFor(t, "the error loop to be broken", func() bool {
		for _, e := range el.byTopic(TopicError) {
			if _, ok := e.Data["dropped"]; ok {
				return true
			}
		}
		return false
	})

	// And it must actually stop, not merely report.
	time.Sleep(200 * time.Millisecond)
	before := len(el.byTopic(TopicError))
	time.Sleep(200 * time.Millisecond)
	if after := len(el.byTopic(TopicError)); after != before {
		t.Errorf("errors still being produced after the loop guard fired: %d -> %d", before, after)
	}
}

func TestPanicInNodeIsContained(t *testing.T) {
	// Node-RED lets an exception in a node's input handler take the process
	// down. One bad node must not stop the other flows on this pod.
	tr := newTestRegistry()
	survivor := &sinkNode{}
	tr.add("boom", 1, 0, func(string) node.Node { return panicNode{} })
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return survivor })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"b","type":"boom","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"p","type":"pass","z":"t1","x":1,"y":2,"wires":[["s"]]},
        {"id":"s","type":"sink","z":"t1","x":2,"y":2,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	el := drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	rt.Inject("b", engine.NewMsgWithPayload("kaboom"))
	waitFor(t, "the panic to be reported", func() bool {
		for _, e := range el.byTopic(TopicError) {
			if s, _ := e.Data["error"].(string); contains(s, "panicked") {
				return true
			}
		}
		return false
	})

	// The rest of the runtime still works.
	rt.Inject("p", engine.NewMsgWithPayload("still alive"))
	waitFor(t, "the unaffected flow to keep running", func() bool { return survivor.count() == 1 })
}

// ---------------------------------------------------------------------------
// Status and Complete
// ---------------------------------------------------------------------------

func TestStatusRoutedToStatusNode(t *testing.T) {
	tr := newTestRegistry()
	seen := &sinkNode{}
	tr.add("reporter", 1, 0, func(string) node.Node {
		return statusNode{s: node.Status{Fill: "green", Shape: "dot", Text: "connected"}}
	})
	tr.add("status", 0, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return seen })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"r","type":"reporter","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"st","type":"status","z":"t1","x":1,"y":2,"wires":[["s"]]},
        {"id":"s","type":"sink","z":"t1","x":2,"y":2,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	el := drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	rt.Inject("r", engine.NewMsg())
	waitFor(t, "status node to fire", func() bool { return seen.count() == 1 })

	m := seen.all()[0]
	if got, _, _ := m.Get("status.text"); got != "connected" {
		t.Errorf("status.text = %#v", got)
	}
	if got, _, _ := m.Get("status.source.id"); got != "r" {
		t.Errorf("status.source.id = %#v, want r", got)
	}

	// The badge is retained so a newly connected editor sees current state.
	waitFor(t, "the badge to be retained", func() bool {
		s, ok := rt.NodeStatus("r")
		return ok && s.Text == "connected"
	})
	if len(el.byTopic(TopicStatus)) == 0 {
		t.Error("no status event was published for the editor")
	}
}

func TestCompleteRequiresExplicitScope(t *testing.T) {
	// Unlike Catch, a Complete node with no scope watches nothing. Defaulting
	// to "everything" would flood the flow with a message per node per message.
	tr := newTestRegistry()
	scoped, unscoped := &sinkNode{}, &sinkNode{}
	tr.add("worker", 1, 0, func(string) node.Node { return doneNode{} })
	tr.add("complete", 0, 1, func(string) node.Node { return passNode{} })
	tr.add("a", 1, 0, func(string) node.Node { return scoped })
	tr.add("b", 1, 0, func(string) node.Node { return unscoped })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"w","type":"worker","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"c1","type":"complete","z":"t1","scope":["w"],"x":1,"y":2,"wires":[["sa"]]},
        {"id":"sa","type":"a","z":"t1","x":2,"y":2,"wires":[]},
        {"id":"c2","type":"complete","z":"t1","x":1,"y":4,"wires":[["sb"]]},
        {"id":"sb","type":"b","z":"t1","x":2,"y":4,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	rt.Inject("w", engine.NewMsgWithPayload("job"))
	waitFor(t, "scoped complete node to fire", func() bool { return scoped.count() == 1 })
	time.Sleep(100 * time.Millisecond)
	if unscoped.count() != 0 {
		t.Error("an unscoped Complete node fired; it should watch nothing")
	}
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func TestDisabledNodesAndTabsAreNotStarted(t *testing.T) {
	tr := newTestRegistry()
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"On"},
        {"id":"t2","type":"tab","label":"Off","disabled":true},
        {"id":"live","type":"pass","z":"t1","x":1,"y":1,"wires":[]},
        {"id":"off","type":"pass","z":"t1","d":true,"x":1,"y":2,"wires":[]},
        {"id":"onOffTab","type":"pass","z":"t2","x":1,"y":1,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	if !rt.Running("live") {
		t.Error("enabled node is not running")
	}
	if rt.Running("off") {
		t.Error(`node with "d": true was started`)
	}
	if rt.Running("onOffTab") {
		t.Error("node on a disabled tab was started")
	}
	if err := rt.Inject("off", engine.NewMsg()); err == nil {
		t.Error("Inject into a disabled node succeeded")
	}
}

func TestUnknownNodeTypeFailsOnlyThatNode(t *testing.T) {
	// One unknown type must not take the deploy down. A flow exported from a
	// Node-RED instance with a community node installed will contain types we
	// do not implement, and the rest of it should still run.
	tr := newTestRegistry()
	sink := &sinkNode{}
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"weird","type":"node-red-contrib-something","z":"t1","x":1,"y":1,"wires":[["s"]]},
        {"id":"p","type":"pass","z":"t1","x":1,"y":2,"wires":[["s"]]},
        {"id":"s","type":"sink","z":"t1","x":2,"y":1,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	fails := rt.Start(context.Background())
	defer rt.Stop(context.Background())

	if len(fails) != 1 {
		t.Fatalf("Start returned %d failures, want 1: %v", len(fails), fails)
	}
	if fails[0].NodeID != "weird" {
		t.Errorf("failure attributed to %s, want weird", fails[0].NodeID)
	}
	if !contains(fails[0].Error(), "unknown node type") {
		t.Errorf("failure message = %q", fails[0].Error())
	}

	rt.Inject("p", engine.NewMsgWithPayload("ok"))
	waitFor(t, "the rest of the flow to work", func() bool { return sink.count() == 1 })
}

func TestStopDrainsQueuedMessagesAndClosesNodes(t *testing.T) {
	// Stopping must not silently discard work already in flight, and a node
	// holding a resource must get the chance to release it.
	tr := newTestRegistry()
	sink := &sinkNode{}
	closer := &closeNode{}
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("sink", 1, 0, func(string) node.Node { return sink })
	tr.add("closer", 1, 0, func(string) node.Node { return closer })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"p","type":"pass","z":"t1","x":1,"y":1,"wires":[["s"]]},
        {"id":"s","type":"sink","z":"t1","x":2,"y":1,"wires":[]},
        {"id":"c","type":"closer","z":"t1","x":1,"y":2,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	rt.Start(context.Background())

	const n = 50
	for i := 0; i < n; i++ {
		rt.Inject("p", engine.NewMsgWithPayload(float64(i)))
	}

	if errs := rt.Stop(context.Background()); len(errs) > 0 {
		t.Errorf("Stop errors: %v", errs)
	}
	if got := sink.count(); got != n {
		t.Errorf("sink received %d of %d messages; queued work was discarded on stop", got, n)
	}
	if !closer.closed.Load() {
		t.Error("Close was not called on a node implementing Closer")
	}

	// Stop must be idempotent — a second call from a shutdown handler is normal.
	if errs := rt.Stop(context.Background()); errs != nil {
		t.Errorf("second Stop returned %v, want nil", errs)
	}
}

func TestPerNodeOverflowOverride(t *testing.T) {
	// A known-bursty branch can be given its own policy without changing the
	// whole runtime.
	tr := newTestRegistry()
	gate := make(chan struct{})
	defer close(gate)
	sink := &sinkNode{gate: gate}
	tr.add("sink", 1, 0, func(string) node.Node { return sink })

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"k","type":"sink","z":"t1","ew_overflow":"drop-newest","ew_inboxCapacity":2,"x":1,"y":1,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{InboxCapacity: 1000, Overflow: OverflowBlock})
	el := drain(rt)
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	for i := 0; i < 50; i++ {
		rt.Inject("k", engine.NewMsgWithPayload(float64(i)))
	}
	waitFor(t, "the per-node policy to drop", func() bool { return len(el.byTopic(TopicDropped)) > 0 })

	for _, s := range rt.Snapshots() {
		if s.NodeID == "k" && s.QueueCap != 2 {
			t.Errorf("per-node capacity override ignored: QueueCap = %d, want 2", s.QueueCap)
		}
	}
}

func TestConfigNodeIsResolvable(t *testing.T) {
	tr := newTestRegistry()
	var gotConfig atomic.Bool

	tr.add("broker", 0, 0, func(string) node.Node { return passNode{} })
	if err := tr.Register(node.Descriptor{
		Type: "consumer", Category: node.CategoryCommon, Color: "#E31837", Icon: "cog",
		Inputs: 1, Outputs: 0,
		Compatibility: node.Compatibility{Level: node.CompatOnly},
	}, func(def *node.Definition) (node.Node, error) {
		ref := def.Node.PropString("broker", "")
		if _, ok := def.Services.ConfigNode(ref); ok {
			gotConfig.Store(true)
		}
		return passNode{}, nil
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	flows := mustFlows(t, `[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"cfg","type":"broker","name":"Plant MQTT"},
        {"id":"c","type":"consumer","z":"t1","broker":"cfg","x":1,"y":1,"wires":[]}
    ]`)

	rt := New(tr.Registry, flows, Options{})
	drain(rt)
	if fails := rt.Start(context.Background()); len(fails) > 0 {
		t.Fatalf("Start failures: %v", fails)
	}
	defer rt.Stop(context.Background())

	if !gotConfig.Load() {
		t.Error("consumer could not resolve its config node")
	}
	if rt.Running("cfg") {
		t.Error("a config node was started as a flow runner")
	}
}

// ---------------------------------------------------------------------------
// Throughput
// ---------------------------------------------------------------------------

func BenchmarkChainThroughput(b *testing.B) {
	// A five-node chain, which is the shape the Node-RED comparison benchmark
	// uses. Reported in ns per message through the whole chain.
	tr := newTestRegistry()
	var received atomic.Int64
	tr.add("pass", 1, 1, func(string) node.Node { return passNode{} })
	tr.add("count", 1, 0, func(string) node.Node {
		return nodeFunc(func(context.Context, *engine.Msg, node.Emitter) error {
			received.Add(1)
			return nil
		})
	})

	flows, err := engine.ParseFlows([]byte(`[
        {"id":"t1","type":"tab","label":"T"},
        {"id":"n1","type":"pass","z":"t1","x":1,"y":1,"wires":[["n2"]]},
        {"id":"n2","type":"pass","z":"t1","x":2,"y":1,"wires":[["n3"]]},
        {"id":"n3","type":"pass","z":"t1","x":3,"y":1,"wires":[["n4"]]},
        {"id":"n4","type":"pass","z":"t1","x":4,"y":1,"wires":[["n5"]]},
        {"id":"n5","type":"count","z":"t1","x":5,"y":1,"wires":[]}
    ]`))
	if err != nil {
		b.Fatalf("ParseFlows: %v", err)
	}

	rt := New(tr.Registry, flows, Options{InboxCapacity: 4096})
	go func() {
		for range rt.Events() {
		}
	}()
	rt.Start(context.Background())
	defer rt.Stop(context.Background())

	b.ReportAllocs()
	b.ResetTimer()
	n := 0
	for b.Loop() {
		rt.Inject("n1", engine.NewMsgWithPayload(float64(n)))
		n++
	}
	// Wait for the chain to drain so the reported time includes the real work,
	// not just the enqueue.
	deadline := time.Now().Add(30 * time.Second)
	for received.Load() < int64(n) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	b.StopTimer()
	if received.Load() != int64(n) {
		b.Fatalf("chain drained %d of %d messages", received.Load(), n)
	}
}
