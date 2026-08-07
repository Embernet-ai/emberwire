package node

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/embernet-ai/emberwire/internal/engine"
)

// stubNode is a minimal Node used to exercise registration.
type stubNode struct{}

func (stubNode) Receive(context.Context, *engine.Msg, Emitter) error { return nil }

func stubFactory(*Definition) (Node, error) { return stubNode{}, nil }

// valid returns a descriptor that passes validation, for tests to mutate.
func valid() Descriptor {
	return Descriptor{
		Type:          "test-node",
		Category:      CategoryFunction,
		Color:         "#E31837",
		Icon:          "function",
		Inputs:        1,
		Outputs:       1,
		Compatibility: Compatibility{Level: CompatFull},
	}
}

func TestDescriptorValidateAcceptsMinimal(t *testing.T) {
	d := valid()
	if err := d.Validate(); err != nil {
		t.Fatalf("minimal valid descriptor rejected: %v", err)
	}
}

func TestDescriptorValidateRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Descriptor)
		wantSub string
	}{
		{"no type", func(d *Descriptor) { d.Type = "" }, "no type"},
		{"reserved tab", func(d *Descriptor) { d.Type = "tab" }, "reserved"},
		{"reserved subflow", func(d *Descriptor) { d.Type = "subflow" }, "reserved"},
		{"reserved group", func(d *Descriptor) { d.Type = "group" }, "reserved"},
		{"subflow prefix", func(d *Descriptor) { d.Type = "subflow:abc" }, "reserved"},
		{"no category", func(d *Descriptor) { d.Category = "" }, "no category"},
		{"two inputs", func(d *Descriptor) { d.Inputs = 2 }, "must be 0 or 1"},
		{"negative outputs", func(d *Descriptor) { d.Outputs = -1 }, "must not be negative"},
		{"config with ports", func(d *Descriptor) { d.IsConfig = true }, "cannot have inputs or outputs"},
		{"no compatibility", func(d *Descriptor) { d.Compatibility = Compatibility{} }, "no compatibility level"},
		{"bad compatibility", func(d *Descriptor) { d.Compatibility.Level = "mostly" }, "unknown compatibility level"},
		{
			// A node that is partially compatible and does not say how is worse
			// than one that is plainly absent: the flow appears to work.
			"partial without notes",
			func(d *Descriptor) { d.Compatibility = Compatibility{Level: CompatPartial} },
			"no notes explain how",
		},
		{
			"outputsProp not declared",
			func(d *Descriptor) { d.OutputsProp = "outputs" },
			"not a declared property",
		},
		{
			"labelProp not declared",
			func(d *Descriptor) { d.LabelProp = "name" },
			"not a declared property",
		},
		{
			"unnamed property",
			func(d *Descriptor) { d.Props = []Prop{{Kind: PropString}} },
			"has no name",
		},
		{
			"duplicate property",
			func(d *Descriptor) {
				d.Props = []Prop{{Name: "a", Kind: PropString}, {Name: "a", Kind: PropString}}
			},
			"duplicate property",
		},
		{
			// Declaring a property called "wires" would let a node overwrite its
			// own graph edges from an edit dialog.
			"reserved key collision",
			func(d *Descriptor) { d.Props = []Prop{{Name: "wires", Kind: PropString}} },
			"reserved flow-entry key",
		},
		{
			"credentials key",
			func(d *Descriptor) { d.Props = []Prop{{Name: "credentials", Kind: PropString}} },
			"use PropCredential",
		},
		{
			"select without options",
			func(d *Descriptor) { d.Props = []Prop{{Name: "mode", Kind: PropSelect}} },
			"no options",
		},
		{
			"configRef without type",
			func(d *Descriptor) { d.Props = []Prop{{Name: "broker", Kind: PropConfigRef}} },
			"names no config type",
		},
		{
			"list without fields",
			func(d *Descriptor) { d.Props = []Prop{{Name: "rules", Kind: PropList}} },
			"has no fields",
		},
		{
			"unknown kind",
			func(d *Descriptor) { d.Props = []Prop{{Name: "whatever", Kind: PropKind("mystery")}} },
			"unknown kind",
		},
		{
			"bad typedInput type",
			func(d *Descriptor) {
				d.Props = []Prop{{Name: "to", Kind: PropTypedInput, TypedInputTypes: []string{"telepathy"}}}
			},
			"unknown typedInput type",
		},
		{
			// The type discriminator is owned by its parent control; declaring
			// it separately would render two controls for one value.
			"typeProp also declared",
			func(d *Descriptor) {
				d.Props = []Prop{
					{Name: "to", Kind: PropTypedInput, TypeProp: "tot"},
					{Name: "tot", Kind: PropString},
				}
			},
			"which is also declared",
		},
		{
			"nested list field invalid",
			func(d *Descriptor) {
				d.Props = []Prop{{Name: "rules", Kind: PropList, Fields: []Prop{
					{Name: "op", Kind: PropSelect}, // no options
				}}}
			},
			"no options",
		},
	}

	for _, c := range cases {
		d := valid()
		c.mutate(&d)
		err := d.Validate()
		if err == nil {
			t.Errorf("%s: Validate() = nil, want error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.wantSub)
		}
	}
}

func TestDescriptorValidateAcceptsRichProps(t *testing.T) {
	d := valid()
	d.OutputsProp = "outputs"
	d.LabelProp = "name"
	d.Props = []Prop{
		{Name: "name", Kind: PropString},
		{Name: "outputs", Kind: PropNumber, Default: 1},
		{Name: "mode", Kind: PropSelect, Options: []Option{{Value: "a", Label: "A"}}},
		{Name: "to", Kind: PropTypedInput, TypeProp: "tot", TypedInputTypes: []string{TypeMsg, TypeStr}},
		{Name: "broker", Kind: PropConfigRef, ConfigType: "mqtt-broker"},
		{Name: "password", Kind: PropCredential},
		{Name: "rules", Kind: PropList, Fields: []Prop{
			{Name: "t", Kind: PropSelect, Options: []Option{{Value: "set", Label: "Set"}}},
			{Name: "p", Kind: PropString},
		}},
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("rich descriptor rejected: %v", err)
	}

	if p, ok := d.PropByName("broker"); !ok || p.ConfigType != "mqtt-broker" {
		t.Error("PropByName did not find the configRef property")
	}
	if _, ok := d.PropByName("nope"); ok {
		t.Error("PropByName found a property that does not exist")
	}
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()

	if err := r.Register(valid(), stubFactory); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if r.Len() != 1 {
		t.Errorf("Len() = %d, want 1", r.Len())
	}

	// A duplicate registration is a programming error, not a silent overwrite:
	// two node types answering to one name means a flow does something
	// different depending on package init order.
	if err := r.Register(valid(), stubFactory); err == nil {
		t.Error("duplicate Register succeeded, want error")
	}

	if err := r.Register(Descriptor{Type: "x"}, stubFactory); err == nil {
		t.Error("Register accepted an invalid descriptor")
	}

	d2 := valid()
	d2.Type = "other"
	if err := r.Register(d2, nil); err == nil {
		t.Error("Register accepted a nil factory")
	}

	reg, ok := r.Lookup("test-node")
	if !ok {
		t.Fatal("Lookup failed for a registered type")
	}
	n, err := reg.New(&Definition{})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if n == nil {
		t.Error("factory returned a nil node")
	}

	if _, ok := r.Lookup("nope"); ok {
		t.Error("Lookup succeeded for an unregistered type")
	}
}

func TestRegistryDescriptorsSorted(t *testing.T) {
	r := NewRegistry()
	add := func(typ string, cat Category) {
		d := valid()
		d.Type = typ
		d.Category = cat
		if err := r.Register(d, stubFactory); err != nil {
			t.Fatalf("Register(%s): %v", typ, err)
		}
	}
	// Registered out of order; the palette must still come back grouped by
	// category then sorted by type.
	add("switch", CategoryFunction)
	add("debug", CategoryCommon)
	add("change", CategoryFunction)
	add("inject", CategoryCommon)

	got := r.Descriptors()
	want := []string{"debug", "inject", "change", "switch"}
	if len(got) != len(want) {
		t.Fatalf("Descriptors() returned %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Type != want[i] {
			t.Errorf("Descriptors()[%d] = %s, want %s", i, got[i].Type, want[i])
		}
	}

	wantTypes := []string{"change", "debug", "inject", "switch"}
	gotTypes := r.Types()
	for i := range wantTypes {
		if gotTypes[i] != wantTypes[i] {
			t.Errorf("Types()[%d] = %s, want %s", i, gotTypes[i], wantTypes[i])
		}
	}
}

func TestDescriptorSerialisesForEditor(t *testing.T) {
	// The editor renders edit dialogs from exactly this JSON. If a field the
	// editor needs is dropped by omitempty, the control silently disappears.
	d := valid()
	d.Props = []Prop{
		{Name: "topic", Kind: PropString, Label: "Topic", Placeholder: "sensor/#"},
		{Name: "qos", Kind: PropSelect, Default: 0, Options: []Option{
			{Value: 0, Label: "0 — at most once"},
			{Value: 1, Label: "1 — at least once"},
		}},
	}

	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, needle := range []string{
		`"type":"test-node"`,
		`"category":"function"`,
		`"inputs":1`,
		`"outputs":1`,
		`"kind":"string"`,
		`"kind":"select"`,
		`"placeholder":"sensor/#"`,
		`"label":"0 — at most once"`,
		`"compatibility":{"level":"full"}`,
	} {
		if !strings.Contains(string(out), needle) {
			t.Errorf("serialised descriptor is missing %s\ngot: %s", needle, out)
		}
	}
}

func TestStatusCleared(t *testing.T) {
	// Node-RED treats an empty status as "remove the badge", and the editor
	// depends on that to blank one.
	if !(Status{}).Cleared() {
		t.Error("empty Status should report Cleared")
	}
	if (Status{Text: "connected"}).Cleared() {
		t.Error("Status with text should not report Cleared")
	}
	if (Status{Fill: "green"}).Cleared() {
		t.Error("Status with fill should not report Cleared")
	}
	if (Status{Shape: "dot"}).Cleared() {
		t.Error("Status with shape should not report Cleared")
	}
}

func TestMustRegisterPanicsOnBadDescriptor(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustRegister did not panic on an invalid descriptor")
		}
	}()
	MustRegister(Descriptor{Type: "broken"}, stubFactory)
}
