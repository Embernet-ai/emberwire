package nodes

import (
	"reflect"
	"strings"
	"testing"

	"github.com/embernet-ai/emberwire/internal/engine"
)

func TestSplitArray(t *testing.T) {
	svc := newTestServices()
	n := build(t, "split", `{"arraySplt":1}`, svc)

	e, err := send(t, n, msg(t, `{"payload":["a","b","c"],"topic":"t"}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got := e.on(0)
	if len(got) != 3 {
		t.Fatalf("emitted %d messages, want 3", len(got))
	}

	for i, want := range []string{"a", "b", "c"} {
		if got[i].Payload() != want {
			t.Errorf("message %d payload = %#v, want %q", i, got[i].Payload(), want)
		}
		// Every other property must be carried through, or a split loses the
		// topic that says which machine the data came from.
		if got[i].Topic() != "t" {
			t.Errorf("message %d lost its topic", i)
		}
		p, ok := readParts(got[i])
		if !ok {
			t.Fatalf("message %d carries no msg.parts", i)
		}
		if p.Index != i || p.Count != 3 || p.Type != "array" {
			t.Errorf("message %d parts = %+v", i, p)
		}
	}

	// All messages in one split share a sequence id, which is what lets Join
	// tell two interleaved sequences apart.
	p0, _ := readParts(got[0])
	p2, _ := readParts(got[2])
	if p0.ID != p2.ID || p0.ID == "" {
		t.Error("messages from one split do not share a sequence id")
	}
}

func TestSplitArrayInChunks(t *testing.T) {
	svc := newTestServices()
	n := build(t, "split", `{"arraySplt":2}`, svc)

	e, err := send(t, n, msg(t, `{"payload":[1,2,3,4,5]}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got := e.on(0)
	if len(got) != 3 {
		t.Fatalf("emitted %d chunks, want 3", len(got))
	}
	if !reflect.DeepEqual(got[0].Payload(), []any{1.0, 2.0}) {
		t.Errorf("chunk 0 = %#v", got[0].Payload())
	}
	// The final chunk is short rather than padded.
	if !reflect.DeepEqual(got[2].Payload(), []any{5.0}) {
		t.Errorf("chunk 2 = %#v, want the remaining element only", got[2].Payload())
	}
}

func TestSplitObject(t *testing.T) {
	svc := newTestServices()
	n := build(t, "split", `{}`, svc)

	e, err := send(t, n, msg(t, `{"payload":{"b":2,"a":1,"c":3}}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got := e.on(0)
	if len(got) != 3 {
		t.Fatalf("emitted %d messages, want 3", len(got))
	}

	// Keys are emitted in sorted order. Go map iteration is randomised, so
	// without sorting the same flow would produce a different sequence on every
	// run — which is exactly the sort of thing that looks fine in testing and
	// ruins a report in production.
	wantKeys := []string{"a", "b", "c"}
	wantVals := []float64{1, 2, 3}
	for i := range got {
		p, _ := readParts(got[i])
		if p.Key != wantKeys[i] {
			t.Errorf("message %d key = %q, want %q", i, p.Key, wantKeys[i])
		}
		if got[i].Payload() != wantVals[i] {
			t.Errorf("message %d payload = %#v, want %v", i, got[i].Payload(), wantVals[i])
		}
		if p.Type != "object" {
			t.Errorf("message %d parts.type = %q, want object", i, p.Type)
		}
	}
}

func TestSplitStringByDelimiter(t *testing.T) {
	svc := newTestServices()
	n := build(t, "split", `{"splt":",","spltType":"str"}`, svc)

	e, err := send(t, n, msg(t, `{"payload":"one,two,three"}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got := e.on(0)
	if len(got) != 3 {
		t.Fatalf("emitted %d messages, want 3", len(got))
	}
	for i, want := range []string{"one", "two", "three"} {
		if got[i].Payload() != want {
			t.Errorf("message %d = %#v, want %q", i, got[i].Payload(), want)
		}
	}
}

func TestSplitStringByLengthIsRuneSafe(t *testing.T) {
	// Splitting UTF-8 by byte count breaks a character at every boundary. The
	// plant floor has degree signs and accented names in it.
	svc := newTestServices()
	n := build(t, "split", `{"splt":"2","spltType":"len"}`, svc)

	e, err := send(t, n, msg(t, `{"payload":"°C°C°C"}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got := e.on(0)
	if len(got) != 3 {
		t.Fatalf("emitted %d messages, want 3", len(got))
	}
	for i := range got {
		if got[i].Payload() != "°C" {
			t.Errorf("message %d = %#v, want \"°C\" — the string was split mid-character",
				i, got[i].Payload())
		}
	}
}

func TestSplitBuffer(t *testing.T) {
	svc := newTestServices()
	n := build(t, "split", `{"splt":"3","spltType":"len"}`, svc)

	m := engine.NewMsg()
	m.SetPayload([]byte{1, 2, 3, 4, 5, 6, 7})
	e, err := send(t, n, m)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got := e.on(0)
	if len(got) != 3 {
		t.Fatalf("emitted %d chunks, want 3", len(got))
	}
	if !reflect.DeepEqual(got[0].Payload(), []byte{1, 2, 3}) {
		t.Errorf("chunk 0 = %#v", got[0].Payload())
	}
	if !reflect.DeepEqual(got[2].Payload(), []byte{7}) {
		t.Errorf("chunk 2 = %#v, want the trailing byte", got[2].Payload())
	}
}

func TestSplitRejectsUnsplittablePayload(t *testing.T) {
	svc := newTestServices()
	n := build(t, "split", `{}`, svc)
	if _, err := send(t, n, msg(t, `{"payload":42}`)); err == nil {
		t.Error("splitting a number was silently accepted")
	}
}

func TestSplitRejectsStreamingMode(t *testing.T) {
	if err := buildErr(t, "split", `{"stream":true}`, newTestServices()); err == nil {
		t.Error("streaming mode was accepted despite not being implemented")
	}
}

func TestSplitJoinRoundTrip(t *testing.T) {
	// The property that matters: Join in automatic mode undoes exactly what
	// Split did, including for objects, where the keys have to come back.
	svc := newTestServices()

	t.Run("array", func(t *testing.T) {
		sp := build(t, "split", `{"arraySplt":1}`, svc)
		jn := build(t, "join", `{"mode":"auto"}`, svc)

		split, err := send(t, sp, msg(t, `{"payload":["a","b","c"],"topic":"line1"}`))
		if err != nil {
			t.Fatalf("split: %v", err)
		}

		var joined *engine.Msg
		for _, part := range split.on(0) {
			e, err := send(t, jn, part)
			if err != nil {
				t.Fatalf("join: %v", err)
			}
			if got := e.on(0); len(got) > 0 {
				joined = got[0]
			}
		}
		if joined == nil {
			t.Fatal("join never emitted a complete sequence")
		}
		if !reflect.DeepEqual(joined.Payload(), []any{"a", "b", "c"}) {
			t.Errorf("rejoined payload = %#v", joined.Payload())
		}
		if joined.Topic() != "line1" {
			t.Errorf("rejoined topic = %q, want line1", joined.Topic())
		}
		// The result is no longer part of a sequence.
		if _, still := joined.Data[engine.PropParts]; still {
			t.Error("rejoined message still carries msg.parts")
		}
	})

	t.Run("object", func(t *testing.T) {
		sp := build(t, "split", `{}`, svc)
		jn := build(t, "join", `{"mode":"auto"}`, svc)

		split, err := send(t, sp, msg(t, `{"payload":{"temp":21.5,"unit":"C"}}`))
		if err != nil {
			t.Fatalf("split: %v", err)
		}

		var joined *engine.Msg
		for _, part := range split.on(0) {
			e, _ := send(t, jn, part)
			if got := e.on(0); len(got) > 0 {
				joined = got[0]
			}
		}
		if joined == nil {
			t.Fatal("join never completed")
		}
		want := map[string]any{"temp": 21.5, "unit": "C"}
		if !reflect.DeepEqual(joined.Payload(), want) {
			t.Errorf("rejoined object = %#v, want %#v", joined.Payload(), want)
		}
	})
}

func TestJoinKeepsInterleavedSequencesApart(t *testing.T) {
	// Two Split nodes feeding one Join is normal. Without keying on the
	// sequence id, their messages would blend into each other.
	svc := newTestServices()
	spA := build(t, "split", `{"arraySplt":1}`, svc)
	spB := build(t, "split", `{"arraySplt":1}`, svc)
	jn := build(t, "join", `{"mode":"auto"}`, svc)

	a, _ := send(t, spA, msg(t, `{"payload":["a1","a2"]}`))
	b, _ := send(t, spB, msg(t, `{"payload":["b1","b2"]}`))

	// Interleave them deliberately.
	order := []*engine.Msg{a.on(0)[0], b.on(0)[0], a.on(0)[1], b.on(0)[1]}
	var results [][]any
	for _, m := range order {
		e, err := send(t, jn, m)
		if err != nil {
			t.Fatalf("join: %v", err)
		}
		for _, out := range e.on(0) {
			arr, ok := out.Payload().([]any)
			if !ok {
				t.Fatalf("joined payload is %T, want an array", out.Payload())
			}
			results = append(results, arr)
		}
	}

	if len(results) != 2 {
		t.Fatalf("join produced %d sequences, want 2", len(results))
	}
	for _, r := range results {
		if len(r) != 2 {
			t.Fatalf("a sequence has %d items, want 2: %#v", len(r), r)
		}
		first := r[0].(string)
		second := r[1].(string)
		if first[0] != second[0] {
			t.Errorf("sequence mixed messages from two splits: %#v", r)
		}
	}
}

func TestJoinManualByCount(t *testing.T) {
	svc := newTestServices()
	jn := build(t, "join", `{"mode":"custom","build":"array","count":3}`, svc)

	var joined *engine.Msg
	for i := 0; i < 3; i++ {
		e, err := send(t, jn, msg(t, `{"payload":`+ftoa(float64(i))+`}`))
		if err != nil {
			t.Fatalf("join %d: %v", i, err)
		}
		if got := e.on(0); len(got) > 0 {
			joined = got[0]
		}
	}
	if joined == nil {
		t.Fatal("manual join never emitted")
	}
	if !reflect.DeepEqual(joined.Payload(), []any{0.0, 1.0, 2.0}) {
		t.Errorf("joined = %#v", joined.Payload())
	}
}

func TestJoinToString(t *testing.T) {
	svc := newTestServices()
	jn := build(t, "join", `{"mode":"custom","build":"string","joiner":", ","count":3}`, svc)

	var joined *engine.Msg
	for _, v := range []string{"a", "b", "c"} {
		e, _ := send(t, jn, msg(t, `{"payload":"`+v+`"}`))
		if got := e.on(0); len(got) > 0 {
			joined = got[0]
		}
	}
	if joined == nil {
		t.Fatal("join never emitted")
	}
	if joined.Payload() != "a, b, c" {
		t.Errorf("joined string = %#v", joined.Payload())
	}
}

func TestJoinAutoRequiresParts(t *testing.T) {
	// Waiting silently forever for a sequence that will never complete is a
	// far worse failure than an error.
	svc := newTestServices()
	jn := build(t, "join", `{"mode":"auto"}`, svc)
	_, err := send(t, jn, msg(t, `{"payload":1}`))
	if err == nil {
		t.Fatal("automatic join accepted a message with no msg.parts")
	}
	if !strings.Contains(err.Error(), "msg.parts") {
		t.Errorf("error = %q, want it to name msg.parts", err)
	}
}

func TestJoinRejectsManualWithoutCount(t *testing.T) {
	if err := buildErr(t, "join", `{"mode":"custom","build":"array"}`, newTestServices()); err == nil {
		t.Error("manual join with no count was accepted; it would never emit")
	}
}

func TestSortArray(t *testing.T) {
	svc := newTestServices()

	asc := build(t, "sort", `{"target":"payload","order":"ascending"}`, svc)
	e, err := send(t, asc, msg(t, `{"payload":["pear","apple","cherry"]}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if got := e.on(0)[0].Payload(); !reflect.DeepEqual(got, []any{"apple", "cherry", "pear"}) {
		t.Errorf("ascending = %#v", got)
	}

	desc := build(t, "sort", `{"target":"payload","order":"descending"}`, svc)
	e2, _ := send(t, desc, msg(t, `{"payload":[1,3,2]}`))
	if got := e2.on(0)[0].Payload(); !reflect.DeepEqual(got, []any{3.0, 2.0, 1.0}) {
		t.Errorf("descending = %#v", got)
	}
}

func TestSortAsNumberVersusAsString(t *testing.T) {
	// The classic: as strings, "10" sorts before "9". A flow sorting readings
	// that came in as strings needs the numeric option to behave.
	svc := newTestServices()

	asStr := build(t, "sort", `{"target":"payload","order":"ascending"}`, svc)
	e, _ := send(t, asStr, msg(t, `{"payload":["10","9","100"]}`))
	if got := e.on(0)[0].Payload(); !reflect.DeepEqual(got, []any{"10", "100", "9"}) {
		t.Errorf("string sort = %#v, want lexical order", got)
	}

	asNum := build(t, "sort", `{"target":"payload","order":"ascending","as_num":true}`, svc)
	e2, _ := send(t, asNum, msg(t, `{"payload":["10","9","100"]}`))
	if got := e2.on(0)[0].Payload(); !reflect.DeepEqual(got, []any{"9", "10", "100"}) {
		t.Errorf("numeric sort = %#v, want numeric order", got)
	}
}

func TestSortRejectsNonArray(t *testing.T) {
	svc := newTestServices()
	n := build(t, "sort", `{"target":"payload"}`, svc)
	if _, err := send(t, n, msg(t, `{"payload":"not an array"}`)); err == nil {
		t.Error("sorting a string was silently accepted")
	}
}

func TestSortRejectsJSONataKey(t *testing.T) {
	if err := buildErr(t, "sort", `{"target":"payload","targetType":"jsonata"}`, newTestServices()); err == nil {
		t.Error("a JSONata sort key was accepted")
	}
}

func TestBatchByCount(t *testing.T) {
	svc := newTestServices()
	n := build(t, "batch", `{"mode":"count","count":3,"overlap":0}`, svc)

	// Nothing emitted until the group is full.
	for i := 0; i < 2; i++ {
		e, err := send(t, n, msg(t, `{"payload":`+ftoa(float64(i))+`}`))
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if e.total() != 0 {
			t.Fatalf("batch emitted after %d of 3 messages", i+1)
		}
	}
	e, err := send(t, n, msg(t, `{"payload":2}`))
	if err != nil {
		t.Fatalf("third message: %v", err)
	}
	got := e.on(0)
	if len(got) != 3 {
		t.Fatalf("batch emitted %d messages, want 3", len(got))
	}

	// The group is stamped as a sequence so a Join can pick it up.
	for i, m := range got {
		p, ok := readParts(m)
		if !ok {
			t.Fatalf("message %d carries no msg.parts", i)
		}
		if p.Index != i || p.Count != 3 {
			t.Errorf("message %d parts = %+v", i, p)
		}
	}
}

func TestBatchOverlap(t *testing.T) {
	svc := newTestServices()
	n := build(t, "batch", `{"mode":"count","count":3,"overlap":1}`, svc)

	var groups [][]*engine.Msg
	for i := 0; i < 5; i++ {
		e, err := send(t, n, msg(t, `{"payload":`+ftoa(float64(i))+`}`))
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got := e.on(0); len(got) > 0 {
			groups = append(groups, got)
		}
	}

	if len(groups) != 2 {
		t.Fatalf("produced %d groups, want 2", len(groups))
	}
	// With an overlap of one, the last message of a group opens the next.
	lastOfFirst := groups[0][2].Payload()
	firstOfSecond := groups[1][0].Payload()
	if lastOfFirst != firstOfSecond {
		t.Errorf("overlap not applied: group 1 ended with %v, group 2 started with %v",
			lastOfFirst, firstOfSecond)
	}

	// Overlapping groups must not share message objects, or the second group
	// would carry the first group's parts.
	p1, _ := readParts(groups[0][2])
	p2, _ := readParts(groups[1][0])
	if p1.ID == p2.ID {
		t.Error("overlapping groups share a sequence id; the message was not copied")
	}
}

func TestBatchRejectsBadConfig(t *testing.T) {
	svc := newTestServices()
	if err := buildErr(t, "batch", `{"mode":"interval","count":3}`, svc); err == nil {
		t.Error("interval mode was accepted despite not being implemented")
	}
	if err := buildErr(t, "batch", `{"mode":"count","count":0}`, svc); err == nil {
		t.Error("a batch size of zero was accepted")
	}
	if err := buildErr(t, "batch", `{"mode":"count","count":3,"overlap":3}`, svc); err == nil {
		t.Error("an overlap equal to the batch size was accepted; it would never advance")
	}
}
