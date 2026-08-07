package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

// Palette colours. Kept here rather than scattered as literals so the canvas
// stays coherent; these are the EmberNET accent and its supporting neutrals
// rather than Node-RED's pastels.
const (
	colorCommon   = "#B3B3B3"
	colorInject   = "#A6BBCF"
	colorDebug    = "#87A980"
	colorCatch    = "#E31837"
	colorStatus   = "#E6A03C"
	colorLink     = "#DDDDDD"
	colorFunction = "#FDD0A2"
	colorSequence = "#C0C0C0"
)

func init() {
	registerInject()
	registerDebug()
	registerJunction()
	registerComment()
	registerCatch()
	registerStatus()
	registerComplete()
	registerLinkIn()
	registerLinkOut()
}

// ---------------------------------------------------------------------------
// inject
// ---------------------------------------------------------------------------

// injectNode emits a message on a schedule, at startup, or on demand.
type injectNode struct {
	props    []injectProp
	repeat   time.Duration
	onceThen time.Duration
	once     bool
	topic    string

	svc node.Services
	mu  sync.Mutex
}

type injectProp struct {
	Prop string // message property to set
	TV   TypedValue
}

func registerInject() {
	node.MustRegister(node.Descriptor{
		Type:         "inject",
		Category:     node.CategoryCommon,
		Color:        colorInject,
		Icon:         "inject",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "inject",
		LabelProp:    "name",
		HasButton:    true,
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Interval and startup injection are supported. Cron-style scheduling " +
				"(\"at a specific time\", \"on these days\") is not implemented in this build.",
			UnsupportedProps: []string{"crontab"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "topic", Kind: node.PropString, Label: "Topic"},
			{Name: "repeat", Kind: node.PropString, Label: "Repeat (seconds)",
				Help: "Emit every N seconds. Leave empty to inject only manually or at startup."},
			{Name: "once", Kind: node.PropBool, Label: "Inject once at start"},
			{Name: "onceDelay", Kind: node.PropString, Label: "Startup delay (seconds)", Default: "0.1"},
			{Name: "props", Kind: node.PropList, Label: "Properties", Fields: []node.Prop{
				{Name: "p", Kind: node.PropString, Label: "Property", Default: "payload"},
				{Name: "v", Kind: node.PropTypedInput, Label: "Value", TypeProp: "vt"},
			}},
		},
		Help: "Emits a message, either manually from the button, at a repeating " +
			"interval, or once shortly after the flow starts.",
	}, newInject)
}

func newInject(def *node.Definition) (node.Node, error) {
	n := &injectNode{
		svc:   def.Services,
		topic: def.Node.PropString("topic", ""),
		once:  def.Node.PropBool("once", false),
	}

	if s := strings.TrimSpace(def.Node.PropString("repeat", "")); s != "" {
		secs, err := parseSeconds(s)
		if err != nil {
			return nil, fmt.Errorf("repeat: %w", err)
		}
		if secs <= 0 {
			return nil, fmt.Errorf("repeat must be greater than zero, got %v", secs)
		}
		n.repeat = time.Duration(secs * float64(time.Second))
	}

	// Node-RED defaults the startup delay to 0.1s so that downstream nodes have
	// finished starting before the first message arrives.
	n.onceThen = 100 * time.Millisecond
	if s := strings.TrimSpace(def.Node.PropString("onceDelay", "")); s != "" {
		if secs, err := parseSeconds(s); err == nil && secs >= 0 {
			n.onceThen = time.Duration(secs * float64(time.Second))
		}
	}

	raw, _ := def.Node.Prop("props")
	if arr, ok := raw.([]any); ok {
		for _, e := range arr {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			p, _ := m["p"].(string)
			if p == "" {
				continue
			}
			// Node-RED special-cases the topic row: it carries no explicit
			// value type and takes the node's own topic property.
			if p == engine.PropTopic {
				if _, hasV := m["v"]; !hasV {
					n.props = append(n.props, injectProp{
						Prop: engine.PropTopic,
						TV:   TypedValue{Type: node.TypeStr, Value: n.topic},
					})
					continue
				}
			}
			n.props = append(n.props, injectProp{
				Prop: p,
				TV:   ReadTypedValue(m, "v", "vt", node.TypeStr),
			})
		}
	}

	if len(n.props) == 0 {
		// A flow written before the props array existed sets payload and topic
		// directly on the node.
		n.props = append(n.props,
			injectProp{Prop: engine.PropPayload, TV: ReadTypedValue(def.Node.Raw, "payload", "payloadType", node.TypeDate)},
			injectProp{Prop: engine.PropTopic, TV: TypedValue{Type: node.TypeStr, Value: n.topic}},
		)
	}

	return n, nil
}

// Receive lets an inject node be triggered by an inbound message, which is how
// the editor's manual button and a wired trigger both work.
func (n *injectNode) Receive(_ context.Context, _ *engine.Msg, out node.Emitter) error {
	return n.emit(out)
}

func (n *injectNode) emit(out node.Emitter) error {
	m := engine.NewMsg()
	ec := EvalContext{Msg: m, Services: n.svc}

	for _, p := range n.props {
		v, ok, err := p.TV.Eval(ec)
		if err != nil {
			return fmt.Errorf("evaluating %s: %w", p.Prop, err)
		}
		if !ok {
			continue
		}
		if err := m.Set(p.Prop, v); err != nil {
			return fmt.Errorf("setting %s: %w", p.Prop, err)
		}
	}
	out.Send(0, m)
	return nil
}

func (n *injectNode) Start(ctx context.Context, out node.Emitter) error {
	if n.once {
		go func() {
			select {
			case <-time.After(n.onceThen):
			case <-ctx.Done():
				return
			}
			if err := n.emit(out); err != nil {
				out.Error(err, nil)
			}
		}()
	}

	if n.repeat > 0 {
		go func() {
			t := time.NewTicker(n.repeat)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := n.emit(out); err != nil {
						out.Error(err, nil)
					}
				}
			}
		}()
	}
	return nil
}

// parseSeconds reads a duration expressed in seconds, which is how Node-RED
// persists every interval field — sometimes as a number, sometimes as a string.
func parseSeconds(s string) (float64, error) {
	var f float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%g", &f); err != nil {
		return 0, fmt.Errorf("%q is not a number of seconds", s)
	}
	return f, nil
}

// ---------------------------------------------------------------------------
// debug
// ---------------------------------------------------------------------------

// debugNode publishes messages to the editor's debug sidebar.
type debugNode struct {
	active     bool
	toSidebar  bool
	toStatus   bool
	complete   string // property to show, or "true" for the whole message
	targetType string
	maxLength  int
	svc        node.Services
	nodeID     string
	nodeName   string
}

func registerDebug() {
	node.MustRegister(node.Descriptor{
		Type:          "debug",
		Category:      node.CategoryCommon,
		Color:         colorDebug,
		Icon:          "debug",
		Inputs:        1,
		Outputs:       0,
		Align:         "right",
		PaletteLabel:  "debug",
		LabelProp:     "name",
		HasButton:     true,
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "active", Kind: node.PropBool, Label: "Enabled", Default: true},
			{Name: "complete", Kind: node.PropString, Label: "Property", Default: "payload",
				Help: `Property to display, or "true" for the complete message.`},
			{Name: "tosidebar", Kind: node.PropBool, Label: "To debug sidebar", Default: true},
			{Name: "tostatus", Kind: node.PropBool, Label: "To node status"},
			{Name: "statusVal", Kind: node.PropString, Label: "Status property"},
		},
		Help: "Shows a message property in the debug sidebar, and optionally as " +
			"the node's own status badge.",
	}, newDebug)
}

func newDebug(def *node.Definition) (node.Node, error) {
	n := &debugNode{
		active:     def.Node.PropBool("active", true),
		toSidebar:  def.Node.PropBool("tosidebar", true),
		toStatus:   def.Node.PropBool("tostatus", false),
		complete:   def.Node.PropString("complete", "payload"),
		targetType: def.Node.PropString("targetType", "msg"),
		maxLength:  def.Node.PropInt("maxLength", 1000),
		svc:        def.Services,
		nodeID:     def.Node.ID,
		nodeName:   def.Node.Name,
	}
	if n.complete == "" {
		n.complete = "payload"
	}
	return n, nil
}

func (n *debugNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	if !n.active {
		return nil
	}

	var shown any
	if n.complete == "true" || n.complete == "complete" {
		shown = m.Data
	} else {
		v, ok, err := m.Get(n.complete)
		if err != nil {
			return fmt.Errorf("debug property %q: %w", n.complete, err)
		}
		if !ok {
			// Node-RED shows "(undefined)" rather than nothing, so that a typo
			// in the property path is visible instead of looking like silence.
			shown = "(undefined)"
		} else {
			shown = v
		}
	}

	if n.toStatus {
		out.Status(node.Status{Fill: "grey", Shape: "dot", Text: truncate(stringify(shown), 32)})
	}

	if n.toSidebar {
		out.Publish("debug", map[string]any{
			"id":     n.nodeID,
			"name":   n.nodeName,
			"topic":  m.Topic(),
			"msgId":  m.ID(),
			"format": describeType(shown),
			"msg":    truncate(stringify(shown), n.maxLength),
		})
	}
	return nil
}

func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return "(undefined)"
	case []byte:
		return fmt.Sprintf("buffer[%d]", len(t))
	case engine.ImmutableBytes:
		return fmt.Sprintf("buffer[%d]", len(t))
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

func describeType(v any) string {
	switch t := v.(type) {
	case nil:
		return "undefined"
	case string:
		return fmt.Sprintf("string[%d]", len(t))
	case bool:
		return "boolean"
	case float64, int, int64:
		return "number"
	case []any:
		return fmt.Sprintf("array[%d]", len(t))
	case []byte:
		return fmt.Sprintf("buffer[%d]", len(t))
	case engine.ImmutableBytes:
		return fmt.Sprintf("buffer[%d]", len(t))
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("... (%d more bytes)", len(s)-max)
}

// ---------------------------------------------------------------------------
// junction, comment
// ---------------------------------------------------------------------------

type passThrough struct{}

func (passThrough) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	out.Send(0, m)
	return nil
}

func registerJunction() {
	node.MustRegister(node.Descriptor{
		Type:          "junction",
		Category:      node.CategoryCommon,
		Color:         colorCommon,
		Icon:          "junction",
		Inputs:        1,
		Outputs:       1,
		PaletteLabel:  "junction",
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Help:          "A wiring convenience. Passes messages straight through so wires can be routed tidily.",
	}, func(*node.Definition) (node.Node, error) { return passThrough{}, nil })
}

type noopNode struct{}

func (noopNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func registerComment() {
	node.MustRegister(node.Descriptor{
		Type:          "comment",
		Category:      node.CategoryCommon,
		Color:         colorCommon,
		Icon:          "comment",
		Inputs:        0,
		Outputs:       0,
		PaletteLabel:  "comment",
		LabelProp:     "name",
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Title"},
			{Name: "info", Kind: node.PropText, Label: "Note", Language: "markdown"},
		},
		Help: "A note on the canvas. Does nothing at runtime.",
	}, func(*node.Definition) (node.Node, error) { return noopNode{}, nil })
}

// ---------------------------------------------------------------------------
// catch, status, complete
// ---------------------------------------------------------------------------
//
// These three have no logic of their own. The runtime routes to them — see
// Runtime.raiseError, onStatus and onComplete, which handle scope filtering and
// the group-distance rule — and all they do is forward what arrives.

func registerCatch() {
	node.MustRegister(node.Descriptor{
		Type:          "catch",
		Category:      node.CategoryCommon,
		Color:         colorCatch,
		Icon:          "alert",
		Inputs:        0,
		Outputs:       1,
		PaletteLabel:  "catch",
		LabelProp:     "name",
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "uncaught", Kind: node.PropBool, Label: "Only catch unhandled errors"},
		},
		Help: "Catches errors raised by other nodes on the same flow. When several " +
			"Catch nodes are eligible, the one closest in the group hierarchy wins.",
	}, func(*node.Definition) (node.Node, error) { return passThrough{}, nil })
}

func registerStatus() {
	node.MustRegister(node.Descriptor{
		Type:          "status",
		Category:      node.CategoryCommon,
		Color:         colorStatus,
		Icon:          "status",
		Inputs:        0,
		Outputs:       1,
		PaletteLabel:  "status",
		LabelProp:     "name",
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
		},
		Help: "Reports status changes from other nodes on the same flow.",
	}, func(*node.Definition) (node.Node, error) { return passThrough{}, nil })
}

func registerComplete() {
	node.MustRegister(node.Descriptor{
		Type:         "complete",
		Category:     node.CategoryCommon,
		Color:        colorCommon,
		Icon:         "complete",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "complete",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatFull,
			Notes: "Watches only the nodes explicitly selected in its scope, as Node-RED does.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
		},
		Help: "Fires when a selected node finishes handling a message.",
	}, func(*node.Definition) (node.Node, error) { return passThrough{}, nil })
}

// ---------------------------------------------------------------------------
// link in / link out
// ---------------------------------------------------------------------------
//
// Link nodes are virtual wires: a Link Out names Link In nodes by id and
// delivers to them without a drawn connection, which is how a flow spanning
// several tabs stays readable.

// LinkRegistry resolves link targets across the whole runtime. Link wires cross
// tab boundaries, so they cannot be resolved from the flow graph alone.
type LinkRegistry struct {
	mu      sync.RWMutex
	inputs  map[string]*linkInNode
	pending map[string][]*linkOutNode
}

// Links is the process-wide link registry.
var Links = &LinkRegistry{
	inputs:  map[string]*linkInNode{},
	pending: map[string][]*linkOutNode{},
}

// Reset clears the registry. Called on redeploy, and by tests.
func (lr *LinkRegistry) Reset() {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.inputs = map[string]*linkInNode{}
	lr.pending = map[string][]*linkOutNode{}
}

func (lr *LinkRegistry) registerIn(id string, n *linkInNode) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.inputs[id] = n
}

func (lr *LinkRegistry) lookup(id string) (*linkInNode, bool) {
	lr.mu.RLock()
	defer lr.mu.RUnlock()
	n, ok := lr.inputs[id]
	return n, ok
}

// linkInNode receives from Link Out nodes and emits into its own flow.
type linkInNode struct {
	mu  sync.Mutex
	out node.Emitter
}

func registerLinkIn() {
	node.MustRegister(node.Descriptor{
		Type:          "link in",
		Category:      node.CategoryCommon,
		Color:         colorLink,
		Icon:          "link",
		Inputs:        1,
		Outputs:       1,
		PaletteLabel:  "link in",
		LabelProp:     "name",
		Compatibility: node.Compatibility{Level: node.CompatFull},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
		},
		Help: "Receives messages from Link Out nodes anywhere in the runtime.",
	}, newLinkIn)
}

func newLinkIn(def *node.Definition) (node.Node, error) {
	n := &linkInNode{}
	Links.registerIn(def.Node.ID, n)
	return n, nil
}

func (n *linkInNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	// Remember the emitter so a Link Out on another tab can deliver here.
	n.mu.Lock()
	n.out = out
	n.mu.Unlock()
	out.Send(0, m)
	return nil
}

// deliver is called by a Link Out node.
func (n *linkInNode) deliver(m *engine.Msg) bool {
	n.mu.Lock()
	out := n.out
	n.mu.Unlock()
	if out == nil {
		return false
	}
	out.Send(0, m)
	return true
}

// Start captures the emitter before any message arrives, so a Link Out can
// deliver to a Link In that has not itself received anything yet.
func (n *linkInNode) Start(_ context.Context, out node.Emitter) error {
	n.mu.Lock()
	n.out = out
	n.mu.Unlock()
	return nil
}

// linkOutNode sends to named Link In nodes.
type linkOutNode struct {
	targets []string
	// mode "link" delivers to the named targets; mode "return" sends the
	// message back to the Link Call node that initiated it.
	returnMode bool
}

func registerLinkOut() {
	node.MustRegister(node.Descriptor{
		Type:         "link out",
		Category:     node.CategoryCommon,
		Color:        colorLink,
		Icon:         "link",
		Inputs:       1,
		Outputs:      0,
		Align:        "right",
		PaletteLabel: "link out",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Link Out in \"send to\" mode is supported. \"Return to calling Link Call\" " +
				"requires the Link Call node, which is not implemented in this build.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "mode", Kind: node.PropSelect, Label: "Mode", Default: "link", Options: []node.Option{
				{Value: "link", Label: "Send to link in nodes"},
				{Value: "return", Label: "Return to calling link call node"},
			}},
		},
		Help: "Sends messages to Link In nodes without a drawn wire.",
	}, newLinkOut)
}

func newLinkOut(def *node.Definition) (node.Node, error) {
	n := &linkOutNode{returnMode: def.Node.PropString("mode", "link") == "return"}
	if raw, ok := def.Node.Prop("links"); ok {
		if arr, ok := raw.([]any); ok {
			for _, e := range arr {
				if s, ok := e.(string); ok {
					n.targets = append(n.targets, s)
				}
			}
		}
	}
	return n, nil
}

func (n *linkOutNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	if n.returnMode {
		return fmt.Errorf("link out in return mode requires a link call node, which is not implemented in this build")
	}
	var missing []string
	for _, id := range n.targets {
		target, ok := Links.lookup(id)
		if !ok || !target.deliver(m.Clone()) {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		// A link pointing at a node that is not running is a broken flow, not a
		// silent no-op. Node-RED drops these quietly, which makes a mistyped or
		// deleted link target very hard to find.
		return fmt.Errorf("link targets not running: %s", strings.Join(missing, ", "))
	}
	return nil
}
