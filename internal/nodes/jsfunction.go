package nodes

import (
	"context"
	"fmt"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/js"
	"github.com/embernet-ai/emberwire/internal/node"
)

func init() {
	registerFunction()
}

// functionNode runs JavaScript against each message.
type functionNode struct {
	prog    *js.Program
	outputs int
	svc     node.Services
}

func registerFunction() {
	node.MustRegister(node.Descriptor{
		Type:         "function",
		Category:     node.CategoryFunction,
		Color:        colorFunction,
		Icon:         "function",
		Inputs:       1,
		Outputs:      1,
		OutputsProp:  "outputs",
		PaletteLabel: "function",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Runs on goja, a JavaScript interpreter written in Go, rather than " +
				"Node's vm module. The language is ES2023; the Node standard library is " +
				"not present. require() and npm modules do not work and cannot be made " +
				"to without embedding Node. There is always a CPU time limit, which " +
				"Node-RED leaves optional and off. setTimeout and setInterval are not " +
				"available — use a Delay or Trigger node, which the runtime can account for.",
			UnsupportedProps: []string{"libs", "setTimeout", "setInterval", "require"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "func", Kind: node.PropJS, Label: "On message",
				Default: "return msg;"},
			{Name: "initialize", Kind: node.PropJS, Label: "On start",
				Help: "Runs once when the flow starts."},
			{Name: "finalize", Kind: node.PropJS, Label: "On stop",
				Help: "Runs once when the flow stops or is redeployed."},
			{Name: "outputs", Kind: node.PropNumber, Label: "Outputs", Default: 1},
			{Name: "timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 5,
				Help: "A hard ceiling. An infinite loop is stopped rather than holding " +
					"a runtime goroutine forever."},
		},
		Help: "Runs JavaScript against each message. Return a message to send it on, " +
			"an array to address several outputs, or nothing to stop it here.\n\n" +
			"Available: msg, node (send, done, error, status, log, warn, debug), " +
			"context, flow, global, env and util. Not available: require, npm modules, " +
			"the Node standard library, setTimeout and setInterval.",
	}, newFunction)
}

func newFunction(def *node.Definition) (node.Node, error) {
	body := def.Node.PropString("func", "")
	if body == "" {
		body = "return msg;"
	}

	outputs := def.Node.PropInt("outputs", 1)
	if outputs < 0 {
		return nil, fmt.Errorf("outputs must not be negative, got %d", outputs)
	}
	// A function with zero declared outputs is a terminal node; keep one slot
	// so node.send() from it is a no-op rather than an index panic.
	slots := outputs
	if slots < 1 {
		slots = 1
	}

	timeout := time.Duration(def.Node.PropInt("timeout", 5)) * time.Second

	// Compiled once at deploy. goja's parser is the expensive part, and a flow
	// running a few thousand messages a second would otherwise spend most of
	// its time re-parsing the same source.
	name := def.Node.Name
	if name == "" {
		name = "function:" + def.Node.ID
	}
	prog, err := js.Compile(
		name,
		body,
		def.Node.PropString("initialize", ""),
		def.Node.PropString("finalize", ""),
		js.Limits{Timeout: timeout},
	)
	if err != nil {
		return nil, err
	}

	return &functionNode{prog: prog, outputs: slots, svc: def.Services}, nil
}

func (n *functionNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	res, err := n.prog.Run(ctx, js.Sandbox{
		Msg:      m,
		Services: n.svc,
		Outputs:  n.outputs,
	})

	// Emit whatever the function produced before reporting the failure. A
	// function that sends two messages and then throws should not have those
	// two messages discarded — they already happened.
	n.emit(res, out)

	if err != nil {
		return err
	}
	if res.Done {
		out.Done(m, nil)
	}
	return nil
}

// emit applies a result's side effects.
func (n *functionNode) emit(res *js.Result, out node.Emitter) {
	if res == nil {
		return
	}
	for _, l := range res.Logs {
		out.Log(l.Level, "%s", l.Message)
	}
	if res.Status != nil {
		out.Status(*res.Status)
	}
	for _, e := range res.Errors {
		out.Error(fmt.Errorf("%s", e), nil)
	}
	if len(res.ByPort) > 0 {
		out.SendAll(res.ByPort)
	}
}

// Start runs the setup code.
func (n *functionNode) Start(ctx context.Context, out node.Emitter) error {
	res, err := n.prog.RunLifecycle(ctx, js.Sandbox{
		Services: n.svc,
		Outputs:  n.outputs,
	}, "start")
	n.emit(res, out)
	if err != nil {
		return fmt.Errorf("setup code: %w", err)
	}
	return nil
}

// Close runs the cleanup code.
//
// Bounded by the context the runtime provides, so a cleanup function that hangs
// cannot wedge a deploy — which is the failure Node-RED only bounded in 0.17
// and still allows fifteen seconds of.
func (n *functionNode) Close(ctx context.Context, _ bool) error {
	res, err := n.prog.RunLifecycle(ctx, js.Sandbox{
		Services: n.svc,
		Outputs:  n.outputs,
	}, "stop")
	_ = res
	if err != nil {
		return fmt.Errorf("cleanup code: %w", err)
	}
	return nil
}
