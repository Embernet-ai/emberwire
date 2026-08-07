// Package node defines what a node type is: how it presents itself to the
// editor, and how it behaves at runtime.
//
// Node-RED ships every node type as two files — foo.js for the runtime and
// foo.html for the editor, the latter carrying a hand-written edit dialog, a
// help pane and a call to RED.nodes.registerType. The two must be kept in step
// by hand, and adding a property means editing markup in a second language.
//
// Emberwire has one definition. A node declares a Descriptor in Go, the admin
// API serves it as JSON, and the editor renders the edit dialog generically from
// the property list. There is no per-node HTML anywhere in this repository.
package node

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/embernet-ai/emberwire/internal/engine"
)

// Category groups node types in the editor palette. The set mirrors Node-RED's
// palette sections so that a flow author's muscle memory carries over, plus
// Discover, which is ours.
type Category string

const (
	CategoryCommon   Category = "common"
	CategoryFunction Category = "function"
	CategoryNetwork  Category = "network"
	CategoryParser   Category = "parser"
	CategorySequence Category = "sequence"
	CategoryStorage  Category = "storage"
	CategoryDiscover Category = "discover"
	CategoryConfig   Category = "config"
)

// PropKind is the editing affordance the editor renders for a property.
//
// The kind determines the control, the validation and the JSON shape of the
// stored value. Adding a kind means teaching the editor one new control, once,
// rather than writing markup per node.
type PropKind string

const (
	// PropString is a single-line text field.
	PropString PropKind = "string"
	// PropText is a multi-line text area.
	PropText PropKind = "text"
	// PropNumber is a numeric field. Values are stored as JSON numbers, but
	// strings are tolerated on read because Node-RED edit dialogs persist some
	// numeric fields as strings.
	PropNumber PropKind = "number"
	// PropBool is a checkbox.
	PropBool PropKind = "bool"
	// PropSelect is a fixed-choice dropdown; see Prop.Options.
	PropSelect PropKind = "select"
	// PropTypedInput is Node-RED's typedInput: a value paired with a type
	// selector, so a field can hold a literal, a message property, a context
	// lookup, an environment variable or an expression. This is the control
	// that makes flows composable, and most node properties want it.
	PropTypedInput PropKind = "typedInput"
	// PropJS is a JavaScript editor pane.
	PropJS PropKind = "js"
	// PropJSON is a JSON editor pane with validation.
	PropJSON PropKind = "json"
	// PropJSONata is a JSONata expression editor.
	PropJSONata PropKind = "jsonata"
	// PropConfigRef selects a configuration node of a given type; see
	// Prop.ConfigType.
	PropConfigRef PropKind = "configRef"
	// PropCredential is a secret. It is stored encrypted, never written to the
	// flow file, and never sent to the editor once set — the editor is told
	// only whether a value exists.
	PropCredential PropKind = "credential"
	// PropList is a repeatable group of sub-properties, used for things like a
	// Change node's rule list; see Prop.Fields.
	PropList PropKind = "list"
)

// TypedInput value types, matching Node-RED's set so that imported flows carry
// their "pt"/"tot"/"vt" discriminators across unchanged.
const (
	TypeMsg     = "msg"
	TypeFlow    = "flow"
	TypeGlobal  = "global"
	TypeStr     = "str"
	TypeNum     = "num"
	TypeBool    = "bool"
	TypeJSON    = "json"
	TypeBin     = "bin"
	TypeRe      = "re"
	TypeDate    = "date"
	TypeJSONata = "jsonata"
	TypeEnv     = "env"
)

// DefaultTypedInputTypes is the set offered when a Prop does not narrow it.
var DefaultTypedInputTypes = []string{
	TypeMsg, TypeFlow, TypeGlobal, TypeStr, TypeNum, TypeBool, TypeJSON, TypeJSONata, TypeEnv,
}

// Option is one choice in a PropSelect.
type Option struct {
	Value any    `json:"value"`
	Label string `json:"label"`
}

// Prop declares one configurable property of a node type.
type Prop struct {
	// Name is the key the value is stored under in the flow file. It must match
	// the Node-RED property name for any node type we are compatible with, or
	// imported flows lose their configuration.
	Name string `json:"name"`

	// Label is what the editor shows. Defaults to Name when empty.
	Label string `json:"label,omitempty"`

	Kind PropKind `json:"kind"`

	// Default is applied when the flow file omits the property.
	Default any `json:"default,omitempty"`

	Required    bool   `json:"required,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`

	// Help is a one-line explanation shown beside the field.
	Help string `json:"help,omitempty"`

	// Options enumerates the choices for PropSelect.
	Options []Option `json:"options,omitempty"`

	// TypedInputTypes narrows the type selector for PropTypedInput. Empty means
	// DefaultTypedInputTypes.
	TypedInputTypes []string `json:"typedInputTypes,omitempty"`

	// TypeProp names the companion property storing the selected type for a
	// PropTypedInput. Node-RED spells these inconsistently — "pt" for a Change
	// rule's property type, "tot" for its target type, "vt" for a Switch rule's
	// value type — so it is declared per property rather than derived.
	TypeProp string `json:"typeProp,omitempty"`

	// ConfigType is the node type a PropConfigRef may point at, e.g.
	// "mqtt-broker".
	ConfigType string `json:"configType,omitempty"`

	// Fields are the sub-properties of a PropList row.
	Fields []Prop `json:"fields,omitempty"`

	// Language hints syntax highlighting for PropText, e.g. "handlebars".
	Language string `json:"language,omitempty"`
}

// Descriptor is everything the editor and the registry need to know about a
// node type. It is serialised straight to JSON for GET /nodes.
type Descriptor struct {
	Type     string   `json:"type"`
	Category Category `json:"category"`

	// Color is the node body colour on the canvas. Palette values live in
	// docs/palette.md so the canvas stays coherent rather than accumulating
	// one-off hex codes.
	Color string `json:"color"`

	// Icon is the name of a built-in editor icon.
	Icon string `json:"icon"`

	Inputs  int `json:"inputs"`
	Outputs int `json:"outputs"`

	// OutputsProp names a property that determines the output count at edit
	// time, for nodes whose port count is configurable — a Switch node's rule
	// count, a Function node's "outputs".
	OutputsProp string `json:"outputsProp,omitempty"`

	// Align places the label and icon on the right, for nodes that terminate a
	// flow. Node-RED uses this for Debug and similar.
	Align string `json:"align,omitempty"`

	// LabelProp names the property used as the node's canvas label when set.
	// Falls back to PaletteLabel, then Type.
	LabelProp    string `json:"labelProp,omitempty"`
	PaletteLabel string `json:"paletteLabel,omitempty"`

	InputLabels  []string `json:"inputLabels,omitempty"`
	OutputLabels []string `json:"outputLabels,omitempty"`

	// IsConfig marks a configuration node: referenced by id from other nodes
	// rather than wired into the graph.
	IsConfig bool `json:"isConfig,omitempty"`

	// HasButton gives the node a clickable button on the canvas, as Inject has.
	HasButton bool `json:"hasButton,omitempty"`

	Props []Prop `json:"props,omitempty"`

	// Help is Markdown shown in the editor's info sidebar.
	Help string `json:"help,omitempty"`

	// Compatibility records how this node relates to its Node-RED counterpart.
	// It is surfaced in the editor and collected into the compatibility matrix,
	// because a node that is 90% compatible is more dangerous than one that is
	// obviously absent.
	Compatibility Compatibility `json:"compatibility"`
}

// Compatibility describes the relationship between an Emberwire node and the
// Node-RED node of the same type.
type Compatibility struct {
	// Level is one of "full", "partial", "divergent" or "emberwire-only".
	Level string `json:"level"`
	// Notes explains any divergence in one or two sentences.
	Notes string `json:"notes,omitempty"`
	// UnsupportedProps lists properties accepted in the flow file but ignored
	// at runtime. Silently ignoring configuration is how a flow appears to work
	// and quietly does the wrong thing.
	UnsupportedProps []string `json:"unsupportedProps,omitempty"`
}

// Compatibility levels.
const (
	CompatFull      = "full"
	CompatPartial   = "partial"
	CompatDivergent = "divergent"
	CompatOnly      = "emberwire-only"
)

// PropByName returns the named property declaration.
func (d *Descriptor) PropByName(name string) (*Prop, bool) {
	for i := range d.Props {
		if d.Props[i].Name == name {
			return &d.Props[i], true
		}
	}
	return nil, false
}

// Validate checks a descriptor for the mistakes that produce a node which
// registers cleanly and then misbehaves. It runs at registration time, so a
// malformed descriptor fails the build's tests rather than a customer's deploy.
func (d *Descriptor) Validate() error {
	if d.Type == "" {
		return fmt.Errorf("descriptor has no type")
	}
	if strings.HasPrefix(d.Type, engine.SubflowInstancePrefix) {
		return fmt.Errorf("%s: type may not use the reserved %q prefix", d.Type, engine.SubflowInstancePrefix)
	}
	switch d.Type {
	case engine.TypeTab, engine.TypeSubflow, engine.TypeGroup:
		return fmt.Errorf("%s: type is reserved for structural flow entries", d.Type)
	}
	if d.Category == "" {
		return fmt.Errorf("%s: no category", d.Type)
	}
	if d.Inputs < 0 || d.Inputs > 1 {
		return fmt.Errorf("%s: inputs = %d, must be 0 or 1", d.Type, d.Inputs)
	}
	if d.Outputs < 0 {
		return fmt.Errorf("%s: outputs = %d, must not be negative", d.Type, d.Outputs)
	}
	if d.IsConfig && (d.Inputs != 0 || d.Outputs != 0) {
		return fmt.Errorf("%s: a config node cannot have inputs or outputs", d.Type)
	}
	if d.Compatibility.Level == "" {
		return fmt.Errorf("%s: no compatibility level declared", d.Type)
	}
	switch d.Compatibility.Level {
	case CompatFull, CompatPartial, CompatDivergent, CompatOnly:
	default:
		return fmt.Errorf("%s: unknown compatibility level %q", d.Type, d.Compatibility.Level)
	}
	if d.Compatibility.Level == CompatPartial || d.Compatibility.Level == CompatDivergent {
		if d.Compatibility.Notes == "" {
			return fmt.Errorf("%s: compatibility is %q but no notes explain how", d.Type, d.Compatibility.Level)
		}
	}
	if d.OutputsProp != "" {
		if _, ok := d.PropByName(d.OutputsProp); !ok {
			return fmt.Errorf("%s: outputsProp %q is not a declared property", d.Type, d.OutputsProp)
		}
	}
	if d.LabelProp != "" {
		if _, ok := d.PropByName(d.LabelProp); !ok {
			return fmt.Errorf("%s: labelProp %q is not a declared property", d.Type, d.LabelProp)
		}
	}
	return validateProps(d.Type, d.Props, "")
}

func validateProps(typ string, props []Prop, prefix string) error {
	seen := map[string]bool{}
	for i := range props {
		p := &props[i]
		path := prefix + p.Name
		if p.Name == "" {
			return fmt.Errorf("%s: property %d has no name", typ, i)
		}
		if seen[p.Name] {
			return fmt.Errorf("%s: duplicate property %q", typ, path)
		}
		seen[p.Name] = true

		switch p.Name {
		case "id", "type", "z", "g", "x", "y", "wires", "d":
			return fmt.Errorf("%s: property %q collides with a reserved flow-entry key", typ, path)
		case "credentials":
			return fmt.Errorf("%s: property %q is reserved; use PropCredential instead", typ, path)
		}

		switch p.Kind {
		case PropSelect:
			if len(p.Options) == 0 {
				return fmt.Errorf("%s: select property %q has no options", typ, path)
			}
		case PropConfigRef:
			if p.ConfigType == "" {
				return fmt.Errorf("%s: configRef property %q names no config type", typ, path)
			}
		case PropList:
			if len(p.Fields) == 0 {
				return fmt.Errorf("%s: list property %q has no fields", typ, path)
			}
			if err := validateProps(typ, p.Fields, path+"."); err != nil {
				return err
			}
		case PropTypedInput:
			for _, t := range p.TypedInputTypes {
				if !validTypedInputType(t) {
					return fmt.Errorf("%s: property %q offers unknown typedInput type %q", typ, path, t)
				}
			}
		case PropString, PropText, PropNumber, PropBool, PropJS, PropJSON, PropJSONata, PropCredential:
		default:
			return fmt.Errorf("%s: property %q has unknown kind %q", typ, path, p.Kind)
		}

		// A typeProp must not itself be declared, or the editor would render a
		// control for the discriminator alongside the control that owns it.
		if p.TypeProp != "" && seenIn(props, p.TypeProp) {
			return fmt.Errorf("%s: property %q names typeProp %q, which is also declared as a property", typ, path, p.TypeProp)
		}
	}
	return nil
}

func seenIn(props []Prop, name string) bool {
	for i := range props {
		if props[i].Name == name {
			return true
		}
	}
	return false
}

func validTypedInputType(t string) bool {
	switch t {
	case TypeMsg, TypeFlow, TypeGlobal, TypeStr, TypeNum, TypeBool,
		TypeJSON, TypeBin, TypeRe, TypeDate, TypeJSONata, TypeEnv:
		return true
	}
	return false
}

// Factory builds a runtime instance of a node type from its flow-file entry.
//
// The factory runs at deploy time. Returning an error fails that one node —
// the rest of the flow still starts, and the failed node is reported to the
// editor — rather than taking the whole deploy down.
type Factory func(def *Definition) (Node, error)

// Definition is what a Factory is handed: the node's own flow entry, plus the
// runtime services it is allowed to reach.
type Definition struct {
	// Node is the parsed flow entry, including Raw for properties this build
	// does not model.
	Node *engine.Node

	// Services gives access to context stores, credentials, config-node lookup
	// and logging.
	Services Services
}

// Services is the runtime surface a node may use. It is an interface so that
// tests can construct a node without standing up a whole runtime.
type Services interface {
	// Context returns the node-, flow- or global-scoped context store.
	Context(scope ContextScope) Context

	// Credential reads a decrypted credential belonging to this node.
	Credential(key string) (string, bool)

	// ConfigNode returns a started configuration-node instance by id.
	ConfigNode(id string) (Node, bool)

	// Env resolves an environment variable through the subflow instance chain,
	// then the tab, then the process environment.
	Env(name string) (string, bool)

	// Log emits a runtime log line attributed to this node.
	Log(level LogLevel, msg string, args ...any)
}

// ContextScope selects which context store a node is addressing.
type ContextScope string

const (
	ScopeNode   ContextScope = "node"
	ScopeFlow   ContextScope = "flow"
	ScopeGlobal ContextScope = "global"
)

// Context is a key/value store scoped to a node, a flow or the whole runtime.
//
// CompareAndSwap and Increment are the additions over Node-RED, whose context
// API offers only get and set. FlowFuse names that omission as the specific
// reason a Node-RED flow cannot be run in more than one instance safely: two
// copies of a flow doing get-modify-set on a shared counter race, and there is
// no primitive to fix it with. Backed by a transactional store, these cost
// nothing to provide.
type Context interface {
	Get(key string) (any, bool, error)
	Set(key string, value any) error
	Keys() ([]string, error)
	Delete(key string) error

	// CompareAndSwap sets key to next only if its current value deep-equals
	// prev. It reports whether the swap happened.
	CompareAndSwap(key string, prev, next any) (bool, error)

	// Increment adds delta to a numeric key, creating it at zero if absent, and
	// returns the new value.
	Increment(key string, delta float64) (float64, error)

	// Update applies fn to the current value and stores the result atomically.
	Update(key string, fn func(cur any) (any, error)) (any, error)
}

// LogLevel mirrors Node-RED's logging levels.
type LogLevel string

const (
	LogError LogLevel = "error"
	LogWarn  LogLevel = "warn"
	LogInfo  LogLevel = "info"
	LogDebug LogLevel = "debug"
	LogTrace LogLevel = "trace"
)

// Status is the badge shown under a node on the canvas.
type Status struct {
	Fill  string `json:"fill,omitempty"`  // red, green, yellow, blue, grey
	Shape string `json:"shape,omitempty"` // ring, dot
	Text  string `json:"text,omitempty"`
}

// Cleared reports whether this status clears the badge. Node-RED treats a status
// with no fill, shape or text as a request to remove the retained status, and
// the editor relies on that to blank a badge.
func (s Status) Cleared() bool {
	return s.Fill == "" && s.Shape == "" && s.Text == ""
}

// Emitter is how a node talks back to the runtime: sending messages onward,
// reporting status, and raising errors.
//
// Every method is safe to call from any goroutine, so a node that does its work
// asynchronously can emit from a callback without coordinating with the
// scheduler.
type Emitter interface {
	// Send delivers a message to one output port.
	Send(port int, msg *engine.Msg)

	// SendAll delivers to several ports at once. The outer index is the port
	// number; a nil entry sends nothing on that port. This is the Go spelling
	// of Node-RED's send([msgA, null, msgC]).
	SendAll(byPort [][]*engine.Msg)

	// Status sets the node's badge.
	Status(s Status)

	// Error raises an error against the message, which routes it to the nearest
	// eligible Catch node. Passing a nil message reports an error not
	// attributable to any one message.
	Error(err error, msg *engine.Msg)

	// Done signals that processing of a message finished, which drives Complete
	// nodes and message-tracing.
	Done(msg *engine.Msg, err error)

	// Publish sends an event to connected editors on an arbitrary topic. The
	// Debug node uses it to reach the debug sidebar.
	//
	// This is on the Emitter rather than a package-level hook so that a node
	// publishes into the runtime it belongs to. With more than one runtime in a
	// process — which every test that starts two of them is — a global would
	// cross the streams.
	Publish(topic string, data map[string]any)

	// Log emits a log line attributed to this node.
	Log(level LogLevel, format string, args ...any)
}

// Node is a runtime node instance.
//
// Receive is called once per inbound message, and never concurrently for the
// same instance: the scheduler gives each node its own goroutine and serialises
// its inbox. A node therefore needs no internal locking for its own state, which
// removes an entire category of bug that Node-RED node authors hit whenever they
// keep state across messages.
//
// Returning an error is equivalent to calling Emitter.Error with the message.
type Node interface {
	Receive(ctx context.Context, msg *engine.Msg, out Emitter) error
}

// Starter is implemented by nodes that produce messages on their own — Inject,
// MQTT In, HTTP In — rather than only reacting to input.
//
// Start is called once when the flow starts. It must not block: long-running
// work belongs in a goroutine that exits when ctx is cancelled.
type Starter interface {
	Start(ctx context.Context, out Emitter) error
}

// Closer is implemented by nodes holding resources that must be released on
// redeploy or shutdown.
//
// removed reports whether the node was deleted from the flow, as opposed to
// being restarted in place. Node-RED allows an unbounded wait here and only
// added a 15-second timeout in 0.17; we always bound it via ctx, so a node that
// refuses to close cannot wedge a deploy.
type Closer interface {
	Close(ctx context.Context, removed bool) error
}

// Registry holds every known node type.
//
// It is populated at init time by the node packages and is read-only once the
// runtime starts, so lookups need no locking on the hot path.
type Registry struct {
	byType map[string]Registration
}

// Registration pairs a descriptor with the factory that builds instances of it.
type Registration struct {
	Descriptor Descriptor
	New        Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byType: map[string]Registration{}}
}

// Register adds a node type. It returns an error rather than panicking so that
// a test can assert on a bad descriptor; the package-level MustRegister on the
// default registry is the convenient form.
func (r *Registry) Register(d Descriptor, f Factory) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("%s: nil factory", d.Type)
	}
	if _, exists := r.byType[d.Type]; exists {
		return fmt.Errorf("%s: already registered", d.Type)
	}
	r.byType[d.Type] = Registration{Descriptor: d, New: f}
	return nil
}

// Lookup returns the registration for a node type.
func (r *Registry) Lookup(typ string) (Registration, bool) {
	reg, ok := r.byType[typ]
	return reg, ok
}

// Descriptors returns every descriptor, sorted by category then type, which is
// the order the editor renders the palette in.
func (r *Registry) Descriptors() []Descriptor {
	out := make([]Descriptor, 0, len(r.byType))
	for _, reg := range r.byType {
		out = append(out, reg.Descriptor)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// Types returns every registered type name, sorted.
func (r *Registry) Types() []string {
	out := make([]string, 0, len(r.byType))
	for t := range r.byType {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Len reports how many node types are registered.
func (r *Registry) Len() int { return len(r.byType) }

// Default is the registry the built-in node packages register into.
var Default = NewRegistry()

// MustRegister adds a node type to the default registry, panicking on error.
//
// Called from package init, so a malformed descriptor fails at process start and
// in every test run — not on a customer's cluster at deploy time.
func MustRegister(d Descriptor, f Factory) {
	if err := Default.Register(d, f); err != nil {
		panic("emberwire: registering node type: " + err.Error())
	}
}
