package engine

import (
	"errors"
	"reflect"
	"testing"
)

func TestParsePathValid(t *testing.T) {
	cases := []struct {
		expr string
		want string // re-rendered form
	}{
		{"payload", "payload"},
		{"payload.value", "payload.value"},
		{"a.b.c", "a.b.c"},
		{"a[0]", "a[0]"},
		{"a[0][1]", "a[0][1]"},
		{`a["b"]`, "a.b"},
		{`a['b']`, "a.b"},
		{`a["b c"]`, `a["b c"]`},
		{`a["b.c"]`, `a["b.c"]`},
		{"a[0].b", "a[0].b"},
		{"a.b[2].c", "a.b[2].c"},
		{"_msgid", "_msgid"},
		{"$x", "$x"},
		{"a1.b2", "a1.b2"},
		{"payload[msg.index]", "payload[msg.index]"},
		{"a[msg.b].c", "a[msg.b].c"},
	}
	for _, c := range cases {
		p, err := ParsePath(c.expr)
		if err != nil {
			t.Errorf("ParsePath(%q) unexpected error: %v", c.expr, err)
			continue
		}
		if got := p.String(); got != c.want {
			t.Errorf("ParsePath(%q).String() = %q, want %q", c.expr, got, c.want)
		}
	}
}

func TestParsePathInvalid(t *testing.T) {
	// Every one of these is rejected by Node-RED's normalisePropertyExpression.
	// Accepting any of them would mean an imported flow behaves differently here.
	cases := []string{
		"",
		".a",
		"a.",
		"a..b",
		"a[",
		"a]",
		"a[]",
		"[0]",
		"a[0",
		"a['b]",
		`a["b`,
		"a b",
		"a.b c",
		`a[""]`,
		"a[[0]]",
		"0.foo",
		"a[0]x", // index run-on: unquoted key directly after ]
	}
	for _, expr := range cases {
		if p, err := ParsePath(expr); err == nil {
			t.Errorf("ParsePath(%q) = %v, want error", expr, p)
		} else if !errors.Is(err, ErrInvalidPath) {
			t.Errorf("ParsePath(%q) error %v does not wrap ErrInvalidPath", expr, err)
		}
	}
}

func TestPathStatic(t *testing.T) {
	p, _ := ParsePath("a.b[0]")
	if !p.Static() {
		t.Error("a.b[0] should be static")
	}
	p, _ = ParsePath("a[msg.i]")
	if p.Static() {
		t.Error("a[msg.i] should not be static")
	}
}

func TestGetProperty(t *testing.T) {
	root := map[string]any{
		"payload": map[string]any{
			"value": 42.0,
			"list":  []any{"zero", "one", "two"},
			"a b":   "spaced",
			"nil":   nil,
		},
		"topic": "sensor/1",
		"index": 1.0,
		"buf":   []byte{0x10, 0x20},
	}

	cases := []struct {
		expr   string
		want   any
		exists bool
	}{
		{"topic", "sensor/1", true},
		{"payload.value", 42.0, true},
		{"payload.list[0]", "zero", true},
		{"payload.list[2]", "two", true},
		{"payload.list[9]", nil, false},
		{`payload["a b"]`, "spaced", true},
		{"payload.list.length", 3.0, true},
		{"payload.nil", nil, true}, // present but nil — distinct from absent
		{"missing", nil, false},
		{"payload.missing.deep", nil, false},
		{"payload.list[msg.index]", "one", true},
		{"buf[1]", 32.0, true},
		{"buf.length", 2.0, true},
		{"topic.length", 8.0, true},
	}
	for _, c := range cases {
		got, ok, err := GetProperty(root, c.expr)
		if err != nil {
			t.Errorf("GetProperty(%q) error: %v", c.expr, err)
			continue
		}
		if ok != c.exists {
			t.Errorf("GetProperty(%q) exists = %v, want %v", c.expr, ok, c.exists)
			continue
		}
		if ok && !reflect.DeepEqual(got, c.want) {
			t.Errorf("GetProperty(%q) = %#v, want %#v", c.expr, got, c.want)
		}
	}
}

func TestSetPropertyCreatesContainers(t *testing.T) {
	root := map[string]any{}

	if err := SetProperty(root, "payload.a.b", "deep"); err != nil {
		t.Fatalf("SetProperty: %v", err)
	}
	// An absent container whose next token is a key becomes a map.
	if err := SetProperty(root, "payload.list[2]", "third"); err != nil {
		t.Fatalf("SetProperty: %v", err)
	}
	// An absent container whose next token is an index becomes a slice, grown
	// and nil-padded up to the index.
	Denormalise(root)

	got, ok, _ := GetProperty(root, "payload.a.b")
	if !ok || got != "deep" {
		t.Errorf("payload.a.b = %#v (ok=%v), want \"deep\"", got, ok)
	}

	list, ok, _ := GetProperty(root, "payload.list")
	if !ok {
		t.Fatal("payload.list missing")
	}
	want := []any{nil, nil, "third"}
	if !reflect.DeepEqual(list, want) {
		t.Errorf("payload.list = %#v, want %#v", list, want)
	}
}

func TestSetPropertyOverwritesNonContainer(t *testing.T) {
	// Assigning through a scalar replaces it, as JavaScript would when the
	// scalar is boxed. Node-RED's setObjectProperty does the same.
	root := map[string]any{"payload": "a string"}
	if err := SetProperty(root, "payload.a", 1.0); err != nil {
		t.Fatalf("SetProperty: %v", err)
	}
	got, ok, _ := GetProperty(root, "payload.a")
	if !ok || got != 1.0 {
		t.Errorf("payload.a = %#v (ok=%v), want 1", got, ok)
	}
}

func TestSetPropertyNestedExpression(t *testing.T) {
	root := map[string]any{
		"index":   2.0,
		"payload": map[string]any{"list": []any{"a", "b", "c"}},
	}
	if err := SetProperty(root, "payload.list[msg.index]", "CHANGED"); err != nil {
		t.Fatalf("SetProperty: %v", err)
	}
	Denormalise(root)
	got, _, _ := GetProperty(root, "payload.list[2]")
	if got != "CHANGED" {
		t.Errorf("payload.list[2] = %#v, want \"CHANGED\"", got)
	}
}

func TestDeleteProperty(t *testing.T) {
	root := map[string]any{
		"payload": map[string]any{"a": 1.0, "b": 2.0},
		"topic":   "t",
	}
	if err := DeleteProperty(root, "payload.a"); err != nil {
		t.Fatalf("DeleteProperty: %v", err)
	}
	if _, ok, _ := GetProperty(root, "payload.a"); ok {
		t.Error("payload.a still present after delete")
	}
	if _, ok, _ := GetProperty(root, "payload.b"); !ok {
		t.Error("payload.b was removed by mistake")
	}
	// Deleting something absent is a no-op, not an error.
	if err := DeleteProperty(root, "payload.nope"); err != nil {
		t.Errorf("deleting absent property returned %v, want nil", err)
	}
	if err := DeleteProperty(root, "missing.deep"); err != nil {
		t.Errorf("deleting through absent parent returned %v, want nil", err)
	}
}

func TestDenormalise(t *testing.T) {
	root := map[string]any{}
	if err := SetProperty(root, "a[0].b[1]", "x"); err != nil {
		t.Fatalf("SetProperty: %v", err)
	}
	out := Denormalise(root).(map[string]any)

	// After denormalising there must be no *[]any anywhere, or JSON encoding
	// emits pointers and the JS bridge sees the wrong type.
	var walk func(any) error
	walk = func(v any) error {
		switch t2 := v.(type) {
		case *[]any:
			return errors.New("found *[]any after Denormalise")
		case []any:
			for _, e := range t2 {
				if err := walk(e); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, e := range t2 {
				if err := walk(e); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(out); err != nil {
		t.Error(err)
	}

	got, ok, _ := GetProperty(out, "a[0].b[1]")
	if !ok || got != "x" {
		t.Errorf("a[0].b[1] = %#v (ok=%v), want \"x\"", got, ok)
	}
}

func FuzzParsePath(f *testing.F) {
	for _, s := range []string{
		"payload", "a.b[0]", `a["b c"]`, "a[msg.i]", "", ".", "[", "]]",
		"a[0]x", `a['b]`, "$_.0", "a..b",
	} {
		f.Add(s)
	}
	// The parser runs on attacker-influenced input via the admin API, so the
	// only contract under fuzzing is: never panic, and never return both a nil
	// error and an unusable path.
	f.Fuzz(func(t *testing.T, expr string) {
		p, err := ParsePath(expr)
		if err != nil {
			return
		}
		if len(p) == 0 {
			t.Fatalf("ParsePath(%q) returned empty path with nil error", expr)
		}
		_ = p.String()
		_ = p.Static()
		root := map[string]any{"a": map[string]any{"b": []any{1.0}}, "msg": 0.0}
		_, _, _ = GetProperty(root, expr)
	})
}
