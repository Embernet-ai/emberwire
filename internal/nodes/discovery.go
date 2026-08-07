package nodes

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/embernet-ai/emberwire/internal/discover"
	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

// The discovery family.
//
// These are why Emberwire has a macvlan network mode. A flow engine that can
// only see what k3s routes to it cannot inventory an OT segment; with its own
// MAC and IP on the plant VLAN, it can.
//
// Every one of them is bounded by the operator-configured CIDR allowlist. The
// nodes are reachable by anyone who can edit a flow, so the boundary lives in
// configuration an operator sets, not in an edit dialog a flow author fills in.

const colorDiscover = "#5B8DEF"

// Scope is the process-wide discovery scope, installed at startup from the
// configuration. Nil until then, which reads as disabled.
var Scope *discover.Scope

func init() {
	registerNetInfo()
	registerScan()
}

// ---------------------------------------------------------------------------
// netinfo
// ---------------------------------------------------------------------------

type netInfoNode struct{}

func registerNetInfo() {
	node.MustRegister(node.Descriptor{
		Type:         "netinfo",
		Category:     node.CategoryDiscover,
		Color:        colorDiscover,
		Icon:         "network",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "netinfo",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "Emberwire's own node. Reports the interfaces the runtime can see, " +
				"which in macvlan mode is how a flow learns its address on the OT VLAN.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "property", Kind: node.PropString, Label: "Output to", Default: "payload"},
		},
		Help: "Reports the runtime's network interfaces, addresses and MAC addresses. " +
			"Not gated by the discovery scope: it reads local state and probes nothing.",
	}, func(*node.Definition) (node.Node, error) { return &netInfoNode{}, nil })
}

func (n *netInfoNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	ifaces, err := discover.Interfaces()
	if err != nil {
		return fmt.Errorf("reading interfaces: %w", err)
	}
	list := make([]any, len(ifaces))
	for i, f := range ifaces {
		list[i] = f
	}
	if err := m.Set(engine.PropPayload, list); err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}

// ---------------------------------------------------------------------------
// scan
// ---------------------------------------------------------------------------

type scanNode struct {
	target      TypedValue
	ports       []int
	timeout     time.Duration
	concurrency int
	identify    bool
	resolve     bool
	splitOutput bool
	svc         node.Services
}

func registerScan() {
	node.MustRegister(node.Descriptor{
		Type:         "scan",
		Category:     node.CategoryDiscover,
		Color:        colorDiscover,
		Icon:         "search",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "scan",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "Emberwire's own node. Sweeps a CIDR range for OT devices and " +
				"identifies Modbus and EtherNet/IP endpoints. Bounded by the " +
				"discovery allowlist in the runtime configuration, not by this dialog.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "target", Kind: node.PropTypedInput, Label: "Range", TypeProp: "targetType",
				Required: true, Placeholder: "10.20.30.0/24",
				Help: "A CIDR range. Must sit entirely inside the configured discovery scope."},
			{Name: "ports", Kind: node.PropString, Label: "Ports",
				Placeholder: "502,4840,44818",
				Help:        "Comma-separated. Leave empty to probe the common OT ports."},
			{Name: "identify", Kind: node.PropBool, Label: "Identify devices", Default: true,
				Help: "Send a read-only identification request to Modbus and EtherNet/IP endpoints."},
			{Name: "resolveNames", Kind: node.PropBool, Label: "Reverse-resolve hostnames"},
			{Name: "timeout", Kind: node.PropNumber, Label: "Per-probe timeout (ms)", Default: 700},
			{Name: "concurrency", Kind: node.PropNumber, Label: "Concurrent probes", Default: 32,
				Help: "Kept low on purpose. An OT link is often 100Mb and shared with " +
					"the process traffic that matters."},
			{Name: "output", Kind: node.PropSelect, Label: "Output", Default: "array",
				Options: []node.Option{
					{Value: "array", Label: "One message holding every device"},
					{Value: "each", Label: "One message per device"},
				}},
		},
		Help: "Sweeps a range for devices and reports what answered. TCP connect only " +
			"— nothing here writes to a device. Requires discovery to be enabled in " +
			"the runtime configuration and the range to be inside the allowlist.",
	}, newScan)
}

func newScan(def *node.Definition) (node.Node, error) {
	n := &scanNode{
		target:      ReadTypedValue(def.Node.Raw, "target", "targetType", node.TypeStr),
		identify:    def.Node.PropBool("identify", true),
		resolve:     def.Node.PropBool("resolveNames", false),
		concurrency: def.Node.PropInt("concurrency", 32),
		timeout:     time.Duration(def.Node.PropInt("timeout", 700)) * time.Millisecond,
		splitOutput: def.Node.PropString("output", "array") == "each",
		svc:         def.Services,
	}
	if n.target.Value == "" {
		return nil, fmt.Errorf("range is required")
	}
	if n.timeout <= 0 {
		n.timeout = 700 * time.Millisecond
	}
	if n.concurrency < 1 {
		n.concurrency = 1
	}

	if s := strings.TrimSpace(def.Node.PropString("ports", "")); s != "" {
		for _, p := range strings.Split(s, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			v, err := strconv.Atoi(p)
			if err != nil || v < 1 || v > 65535 {
				return nil, fmt.Errorf("port %q is not between 1 and 65535", p)
			}
			n.ports = append(n.ports, v)
		}
	}
	return n, nil
}

func (n *scanNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	if !Scope.Enabled() {
		// Explicit rather than an empty result. A scan node that silently
		// returns nothing looks like a network with nothing on it.
		return discover.ErrDisabled
	}

	raw, ok, err := n.target.Eval(EvalContext{Msg: m, Services: n.svc})
	if err != nil {
		return fmt.Errorf("range: %w", err)
	}
	if !ok {
		return fmt.Errorf("range did not resolve")
	}

	prefix, err := netip.ParsePrefix(strings.TrimSpace(fmt.Sprint(raw)))
	if err != nil {
		return fmt.Errorf("range %q is not a CIDR: %w", raw, err)
	}

	out.Status(node.Status{Fill: "blue", Shape: "dot", Text: "scanning " + prefix.String()})

	devices, err := discover.Sweep(ctx, Scope, prefix, discover.Options{
		Timeout:      n.timeout,
		Concurrency:  n.concurrency,
		Ports:        n.ports,
		Identify:     n.identify,
		ResolveNames: n.resolve,
	})
	if err != nil {
		out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
		return err
	}

	out.Status(node.Status{
		Fill: "green", Shape: "dot",
		Text: fmt.Sprintf("%d device(s)", len(devices)),
	})

	if !n.splitOutput {
		list := make([]any, len(devices))
		for i, d := range devices {
			list[i] = deviceToMap(d)
		}
		if err := m.Set(engine.PropPayload, list); err != nil {
			return err
		}
		m.Data["deviceCount"] = float64(len(devices))
		out.Send(0, m)
		return nil
	}

	// One message per device, stamped as a sequence so a Join or a batching
	// database insert downstream can reassemble the inventory.
	seqID := engine.GenerateID()
	for i, d := range devices {
		cp := m.Clone()
		if err := cp.Set(engine.PropPayload, deviceToMap(d)); err != nil {
			return err
		}
		cp.SetTopic(d.Address)
		cp.Data[engine.PropParts] = partsInfo{
			ID: seqID, Index: i, Count: len(devices), Type: "array",
		}.toMap()
		out.Send(0, cp)
	}
	return nil
}

// deviceToMap renders a finding as the JSON-shaped object a flow works with.
func deviceToMap(d discover.Device) map[string]any {
	out := map[string]any{
		"address": d.Address,
		"source":  d.Source,
	}
	if d.MAC != "" {
		out["mac"] = d.MAC
	}
	if d.Hostname != "" {
		out["hostname"] = d.Hostname
	}
	if d.Vendor != "" {
		out["vendor"] = d.Vendor
	}
	if d.Protocol != "" {
		out["protocol"] = d.Protocol
	}
	if len(d.Ports) > 0 {
		ports := make([]any, len(d.Ports))
		names := make([]any, len(d.Ports))
		for i, p := range d.Ports {
			ports[i] = float64(p)
			names[i] = discover.PortName(p)
		}
		out["ports"] = ports
		out["services"] = names
	}
	if len(d.Details) > 0 {
		out["details"] = d.Details
	}
	return out
}
