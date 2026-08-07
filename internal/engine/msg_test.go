package engine

import (
	"encoding/json"
	"reflect"
	"regexp"
	"testing"
	"time"
)

var idShape = regexp.MustCompile(`^[0-9a-f]{16}$`)

// timeoutAfterSeconds gives the termination tests a deadline. A hang here is a
// real failure mode — an unbounded clone would wedge a runtime goroutine — so it
// has to fail the test rather than stall the suite.
func timeoutAfterSeconds(n int) <-chan time.Time {
	return time.After(time.Duration(n) * time.Second)
}

func TestGenerateIDShape(t *testing.T) {
	// Node-RED's RED.util.generateId emits eight hex-encoded bytes. Ids leak
	// into exported flows, so the shape has to match or a round-trip through
	// Node-RED looks foreign.
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		id := GenerateID()
		if !idShape.MatchString(id) {
			t.Fatalf("GenerateID() = %q, want 16 lowercase hex chars", id)
		}
		if seen[id] {
			t.Fatalf("GenerateID() produced a duplicate: %q", id)
		}
		seen[id] = true
	}
}

func TestNewMsgHasID(t *testing.T) {
	m := NewMsg()
	if m.ID() == "" {
		t.Error("NewMsg produced a message with no id")
	}
	m2 := NewMsgWithPayload("hello")
	if m2.Payload() != "hello" {
		t.Errorf("Payload() = %#v, want \"hello\"", m2.Payload())
	}
	if m2.ID() == "" {
		t.Error("NewMsgWithPayload produced a message with no id")
	}
}

func TestWrapMsgEnsuresID(t *testing.T) {
	m := WrapMsg(map[string]any{"payload": 1.0})
	if m.ID() == "" {
		t.Error("WrapMsg did not assign an id")
	}
	// An existing id must be preserved, otherwise message tracing breaks across
	// a link call or a subflow boundary.
	m2 := WrapMsg(map[string]any{PropMsgID: "abcdef0123456789"})
	if m2.ID() != "abcdef0123456789" {
		t.Errorf("WrapMsg overwrote an existing id: %q", m2.ID())
	}
	if m3 := WrapMsg(nil); m3.ID() == "" {
		t.Error("WrapMsg(nil) did not produce a usable message")
	}
}

func TestCloneIsDeep(t *testing.T) {
	orig := WrapMsg(map[string]any{
		"payload": map[string]any{
			"nested": map[string]any{"n": 1.0},
			"list":   []any{1.0, 2.0, map[string]any{"deep": "x"}},
		},
		"topic": "t",
		"buf":   []byte{1, 2, 3},
	})

	cp := orig.Clone()

	// Mutating the clone must not be visible in the original. This is the
	// property Node-RED does not give you on the first wire of a fan-out.
	if err := cp.Set("payload.nested.n", 999.0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := cp.Set("payload.list[2].deep", "CHANGED"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	cp.Data["buf"].([]byte)[0] = 0xFF
	cp.SetTopic("other")

	if got, _, _ := orig.Get("payload.nested.n"); got != 1.0 {
		t.Errorf("original payload.nested.n = %#v, want 1 — clone aliased it", got)
	}
	if got, _, _ := orig.Get("payload.list[2].deep"); got != "x" {
		t.Errorf("original payload.list[2].deep = %#v, want \"x\" — clone aliased it", got)
	}
	if orig.Data["buf"].([]byte)[0] != 1 {
		t.Error("original buf was mutated — []byte was shared, not copied")
	}
	if orig.Topic() != "t" {
		t.Errorf("original topic = %q, want \"t\"", orig.Topic())
	}
}

func TestCloneSharesImmutableBytes(t *testing.T) {
	// The fast path that keeps large binary payloads cheap to fan out. The
	// producer promises never to mutate; in exchange the clone is free.
	payload := ImmutableBytes{1, 2, 3, 4}
	orig := WrapMsg(map[string]any{"payload": payload})
	cp := orig.Clone()

	got, ok := cp.Payload().(ImmutableBytes)
	if !ok {
		t.Fatalf("clone payload is %T, want ImmutableBytes", cp.Payload())
	}
	if containerID(got) != containerID(payload) {
		t.Error("ImmutableBytes was copied; the fast path is not engaged")
	}

	// It must still encode identically to a plain []byte, so the two are
	// interchangeable everywhere downstream.
	a, err := json.Marshal(ImmutableBytes{1, 2, 3})
	if err != nil {
		t.Fatalf("marshal ImmutableBytes: %v", err)
	}
	b, err := json.Marshal([]byte{1, 2, 3})
	if err != nil {
		t.Fatalf("marshal []byte: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("ImmutableBytes encodes as %s, []byte as %s — must match", a, b)
	}
}

func TestCloneTerminatesOnCycle(t *testing.T) {
	// A Function node can write msg.self = msg in one line. Node-RED's clone
	// throws on that; the requirement here is simply that we do not hang or
	// blow the stack.
	data := map[string]any{"payload": "x"}
	data["self"] = data
	m := WrapMsg(data)

	done := make(chan *Msg, 1)
	go func() { done <- m.Clone() }()

	select {
	case cp := <-done:
		if cp.Payload() != "x" {
			t.Errorf("payload survived as %#v, want \"x\"", cp.Payload())
		}
	case <-timeoutAfterSeconds(5):
		t.Fatal("Clone did not terminate on a self-referential message")
	}
}

func TestCloneTerminatesOnSliceCycle(t *testing.T) {
	list := []any{1.0}
	list = append(list, list)
	m := WrapMsg(map[string]any{"payload": list})

	done := make(chan *Msg, 1)
	go func() { done <- m.Clone() }()

	select {
	case <-done:
	case <-timeoutAfterSeconds(5):
		t.Fatal("Clone did not terminate on a self-referential slice")
	}
}

func TestCloneDepthLimit(t *testing.T) {
	// Deeply nested input arrives from the admin API and from decoded MQTT
	// payloads. Refuse the branch rather than overflowing the stack.
	root := map[string]any{}
	cur := root
	for i := 0; i < maxCloneDepth+50; i++ {
		next := map[string]any{}
		cur["n"] = next
		cur = next
	}
	m := WrapMsg(root)

	done := make(chan *Msg, 1)
	go func() { done <- m.Clone() }()
	select {
	case cp := <-done:
		if cp == nil {
			t.Fatal("Clone returned nil")
		}
	case <-timeoutAfterSeconds(5):
		t.Fatal("Clone did not terminate on a deeply nested message")
	}
}

func TestCloneNilMsg(t *testing.T) {
	var m *Msg
	if got := m.Clone(); got != nil {
		t.Errorf("(*Msg)(nil).Clone() = %#v, want nil", got)
	}
	if got := m.ID(); got != "" {
		t.Errorf("(*Msg)(nil).ID() = %q, want \"\"", got)
	}
}

func TestClonePreservesScalarTypes(t *testing.T) {
	orig := WrapMsg(map[string]any{
		"s": "str", "i": 42, "i64": int64(7), "f": 1.5,
		"b": true, "n": nil, "u8": uint8(3),
		"strs": []string{"a", "b"}, "fs": []float64{1, 2},
		"kv": map[string]string{"k": "v"},
	})
	cp := orig.Clone()
	for k, want := range orig.Data {
		got := cp.Data[k]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("key %q: clone has %#v (%T), want %#v (%T)", k, got, got, want, want)
		}
	}
	// The composite ones must be copies, not shares.
	cp.Data["strs"].([]string)[0] = "MUTATED"
	if orig.Data["strs"].([]string)[0] != "a" {
		t.Error("[]string was shared, not copied")
	}
	cp.Data["kv"].(map[string]string)["k"] = "MUTATED"
	if orig.Data["kv"].(map[string]string)["k"] != "v" {
		t.Error("map[string]string was shared, not copied")
	}
}

func BenchmarkCloneSmall(b *testing.B) {
	m := WrapMsg(map[string]any{
		"payload": map[string]any{"temp": 21.5, "unit": "C"},
		"topic":   "sensor/1/temp",
	})
	b.ReportAllocs()
	for b.Loop() {
		_ = m.Clone()
	}
}

func BenchmarkCloneLargeBytes(b *testing.B) {
	m := WrapMsg(map[string]any{"payload": make([]byte, 1<<20)})
	b.ReportAllocs()
	for b.Loop() {
		_ = m.Clone()
	}
}

func BenchmarkCloneLargeImmutableBytes(b *testing.B) {
	m := WrapMsg(map[string]any{"payload": make(ImmutableBytes, 1<<20)})
	b.ReportAllocs()
	for b.Loop() {
		_ = m.Clone()
	}
}
