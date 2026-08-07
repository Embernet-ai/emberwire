package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

func init() {
	registerTemplate()
}

// ---------------------------------------------------------------------------
// template
// ---------------------------------------------------------------------------

// templateNode fills a Mustache template from the message and writes the result
// to a message property or a context key.
type templateNode struct {
	tmpl      []mustacheNode
	plain     string // the raw template, used when syntax is "plain"
	mustache  bool
	field     string
	fieldType string
	output    string // str, json, yaml
	svc       node.Services
}

func registerTemplate() {
	node.MustRegister(node.Descriptor{
		Type:         "template",
		Category:     node.CategoryFunction,
		Color:        colorFunction,
		Icon:         "template",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "template",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Mustache templating is implemented against mustache.js's dialect, " +
				"including its HTML escape set and standalone-line handling, so a " +
				"template moved from Node-RED renders the same bytes. Partials " +
				"({{>name}}) and custom delimiters are refused rather than ignored, " +
				"because there is nothing in a flow file that can supply either.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "field", Kind: node.PropString, Label: "Set property", Default: "payload"},
			{Name: "fieldType", Kind: node.PropSelect, Label: "Set in", Default: "msg", Options: []node.Option{
				{Value: "msg", Label: "msg."},
				{Value: "flow", Label: "flow."},
				{Value: "global", Label: "global."},
			}},
			{Name: "syntax", Kind: node.PropSelect, Label: "Syntax", Default: "mustache", Options: []node.Option{
				{Value: "mustache", Label: "Mustache"},
				{Value: "plain", Label: "Plain text"},
			}},
			{Name: "template", Kind: node.PropText, Label: "Template", Language: "handlebars"},
			{Name: "output", Kind: node.PropSelect, Label: "Output as", Default: "str", Options: []node.Option{
				{Value: "str", Label: "Plain text"},
				{Value: "json", Label: "Parsed JSON"},
				{Value: "yaml", Label: "Parsed YAML"},
			}},
			// Node-RED persists this to choose the editor's syntax highlighting.
			// It has no runtime effect, and is declared so the round-trip keeps it.
			{Name: "format", Kind: node.PropString, Label: "Editor syntax", Default: "handlebars"},
		},
		Help: "Fills a Mustache template from the incoming message. {{payload.x}} " +
			"reads the message, {{flow.x}} and {{global.x}} read context, and " +
			"{{env.NAME}} reads an environment variable. Use {{{x}}} to insert a " +
			"value without HTML escaping — {{x}} escapes slashes as well as angle " +
			"brackets, which surprises anyone templating a URL or a file path.",
	}, newTemplate)
}

func newTemplate(def *node.Definition) (node.Node, error) {
	n := &templateNode{
		plain:     def.Node.PropString("template", ""),
		field:     orDefault(def.Node.PropString("field", ""), engine.PropPayload),
		fieldType: orDefault(def.Node.PropString("fieldType", ""), node.TypeMsg),
		output:    orDefault(def.Node.PropString("output", ""), "str"),
		mustache:  def.Node.PropString("syntax", "mustache") != "plain",
		svc:       def.Services,
	}

	switch n.output {
	case "str", "json", "yaml":
	default:
		return nil, fmt.Errorf("unknown output format %q", n.output)
	}
	switch n.fieldType {
	case node.TypeMsg, node.TypeFlow, node.TypeGlobal:
	default:
		return nil, fmt.Errorf("cannot write a template result to a %q target", n.fieldType)
	}

	if n.mustache {
		// Compiled here rather than per message: a malformed template fails the
		// deploy, where somebody is watching, instead of the first message an
		// hour later.
		tmpl, err := parseMustache(n.plain)
		if err != nil {
			return nil, fmt.Errorf("template: %w", err)
		}
		n.tmpl = tmpl
	}
	return n, nil
}

func (n *templateNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	rendered := n.plain
	if n.mustache {
		scope := &mustacheScope{
			stack: []any{m.Data},
			root:  n.rootLookup(m),
		}
		var err error
		rendered, err = renderMustache(n.tmpl, scope)
		if err != nil {
			return err
		}
	}

	var value any = rendered
	switch n.output {
	case "json":
		var parsed any
		if err := json.Unmarshal([]byte(rendered), &parsed); err != nil {
			return fmt.Errorf("the rendered template is not valid JSON: %w", err)
		}
		value = parsed
	case "yaml":
		var parsed any
		if err := yaml.Unmarshal([]byte(rendered), &parsed); err != nil {
			return fmt.Errorf("the rendered template is not valid YAML: %w", err)
		}
		value = normaliseYAML(parsed)
	}

	ec := EvalContext{Msg: m, Services: n.svc}
	if err := SetTypedTarget(ec, n.fieldType, n.field, value); err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}

// rootLookup resolves the names that are not properties of the message: the
// flow and global context stores, and the environment. Node-RED injects these
// as objects on the render context; doing it lazily here means a template
// referencing one key does not have to materialise a whole context store.
func (n *templateNode) rootLookup(m *engine.Msg) func(string) (any, bool, error) {
	return func(path string) (any, bool, error) {
		head, rest, _ := strings.Cut(path, ".")
		var typ string
		switch head {
		case "flow":
			typ = node.TypeFlow
		case "global":
			typ = node.TypeGlobal
		case "env":
			typ = node.TypeEnv
		default:
			return nil, false, nil
		}
		if rest == "" {
			// {{flow}} on its own has nothing to resolve to. Node-RED renders
			// "[object Object]"; rendering nothing is less misleading.
			return nil, false, nil
		}
		return TypedValue{Type: typ, Value: rest}.Eval(EvalContext{Msg: m, Services: n.svc})
	}
}

// normaliseYAML converts what yaml.v3 decodes into what the rest of the runtime
// expects a message to hold.
//
// yaml.v3 decodes mappings as map[string]any when every key is a string and
// map[any]any otherwise, and integers as int. A message carrying either of those
// would fail to JSON-encode for the debug sidebar, and a Switch node comparing
// it numerically would not match, because every number elsewhere in the runtime
// is a float64 that arrived through encoding/json.
func normaliseYAML(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = normaliseYAML(e)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[fmt.Sprint(k)] = normaliseYAML(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normaliseYAML(e)
		}
		return out
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	default:
		return v
	}
}
