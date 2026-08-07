package nodes

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

func init() {
	registerSplit()
	registerJoin()
	registerSort()
	registerBatch()
}

// msg.parts is how Node-RED tracks a message sequence: Split stamps it, Join
// and Sort and Batch read it. Reproducing the exact field names matters,
// because a flow may pass a sequence through a Function node that inspects
// them, and because a sequence produced here has to be joinable by a
// Node-RED-authored Join node and the reverse.
type partsInfo struct {
	ID    string // groups messages belonging to one sequence
	Index int    // position within the sequence
	Count int    // total, when known
	Type  string // "array", "object", "string", "buffer"
	Key   string // object key, for object splits
	Len   int    // chunk length, for string and buffer splits
}

func (p partsInfo) toMap() map[string]any {
	m := map[string]any{
		"id":    p.ID,
		"index": float64(p.Index),
		"type":  p.Type,
	}
	if p.Count > 0 {
		m["count"] = float64(p.Count)
	}
	if p.Key != "" {
		m["key"] = p.Key
	}
	if p.Len > 0 {
		m["len"] = float64(p.Len)
	}
	return m
}

func readParts(m *engine.Msg) (partsInfo, bool) {
	raw, ok := m.Data[engine.PropParts].(map[string]any)
	if !ok {
		return partsInfo{}, false
	}
	p := partsInfo{}
	p.ID, _ = raw["id"].(string)
	p.Type, _ = raw["type"].(string)
	p.Key, _ = raw["key"].(string)
	if f, ok := raw["index"].(float64); ok {
		p.Index = int(f)
	}
	if f, ok := raw["count"].(float64); ok {
		p.Count = int(f)
	}
	if f, ok := raw["len"].(float64); ok {
		p.Len = int(f)
	}
	return p, p.ID != ""
}

// ---------------------------------------------------------------------------
// split
// ---------------------------------------------------------------------------

type splitNode struct {
	splt      string // string/buffer delimiter
	spltType  string // "str", "len", "bin"
	arraySplt int    // array chunk size
	stream    bool
}

func registerSplit() {
	node.MustRegister(node.Descriptor{
		Type:         "split",
		Category:     node.CategorySequence,
		Color:        colorSequence,
		Icon:         "split",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "split",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Splits arrays, objects, strings and buffers. Streaming mode, which " +
				"carries a partial remainder between messages, is not implemented.",
			UnsupportedProps: []string{"stream"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "splt", Kind: node.PropString, Label: "Split using", Default: `\n`,
				Help: "For strings: the delimiter, or a length if the type is set to length."},
			{Name: "spltType", Kind: node.PropSelect, Label: "Split type", Default: "str",
				Options: []node.Option{
					{Value: "str", Label: "Delimiter"},
					{Value: "len", Label: "Fixed length"},
				}},
			{Name: "arraySplt", Kind: node.PropNumber, Label: "Array chunk size", Default: 1},
		},
		Help: "Splits a message into a sequence of messages: one per array element, " +
			"one per object key, or one per delimited chunk of a string or buffer.",
	}, newSplit)
}

func newSplit(def *node.Definition) (node.Node, error) {
	n := &splitNode{
		splt:      def.Node.PropString("splt", "\\n"),
		spltType:  def.Node.PropString("spltType", "str"),
		arraySplt: def.Node.PropInt("arraySplt", 1),
		stream:    def.Node.PropBool("stream", false),
	}
	if n.stream {
		return nil, fmt.Errorf("split streaming mode is not implemented in this build")
	}
	if n.arraySplt < 1 {
		n.arraySplt = 1
	}
	// Node-RED stores the delimiter with the escape sequence literal, because
	// the edit dialog is a text field.
	n.splt = strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t").Replace(n.splt)
	return n, nil
}

func (n *splitNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	seqID := engine.GenerateID()

	switch payload := m.Payload().(type) {
	case []any:
		chunks := chunkSlice(payload, n.arraySplt)
		for i, c := range chunks {
			cp := m.Clone()
			// A chunk size of one unwraps to the element itself, which is what
			// makes the common case behave like "one message per item".
			if n.arraySplt == 1 && len(c) == 1 {
				cp.SetPayload(c[0])
			} else {
				cp.SetPayload(c)
			}
			cp.Data[engine.PropParts] = partsInfo{
				ID: seqID, Index: i, Count: len(chunks), Type: "array", Len: n.arraySplt,
			}.toMap()
			out.Send(0, cp)
		}
		return nil

	case map[string]any:
		// Sorted so the sequence is deterministic; Go map order is not.
		keys := make([]string, 0, len(payload))
		for k := range payload {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for i, k := range keys {
			cp := m.Clone()
			cp.SetPayload(payload[k])
			cp.Data[engine.PropParts] = partsInfo{
				ID: seqID, Index: i, Count: len(keys), Type: "object", Key: k,
			}.toMap()
			out.Send(0, cp)
		}
		return nil

	case string:
		pieces, err := splitString(payload, n.splt, n.spltType)
		if err != nil {
			return err
		}
		for i, p := range pieces {
			cp := m.Clone()
			cp.SetPayload(p)
			cp.Data[engine.PropParts] = partsInfo{
				ID: seqID, Index: i, Count: len(pieces), Type: "string",
			}.toMap()
			out.Send(0, cp)
		}
		return nil

	case []byte:
		return n.splitBytes(m, payload, seqID, out)

	case engine.ImmutableBytes:
		return n.splitBytes(m, payload, seqID, out)

	default:
		return fmt.Errorf("cannot split a payload of type %T", m.Payload())
	}
}

func (n *splitNode) splitBytes(m *engine.Msg, buf []byte, seqID string, out node.Emitter) error {
	size := n.arraySplt
	if n.spltType == "len" {
		var err error
		size, err = parseIntStrict(n.splt)
		if err != nil {
			return fmt.Errorf("split length: %w", err)
		}
	}
	if size < 1 {
		size = 1
	}
	total := (len(buf) + size - 1) / size
	for i := 0; i < total; i++ {
		lo := i * size
		hi := lo + size
		if hi > len(buf) {
			hi = len(buf)
		}
		chunk := make([]byte, hi-lo)
		copy(chunk, buf[lo:hi])

		cp := m.Clone()
		cp.SetPayload(chunk)
		cp.Data[engine.PropParts] = partsInfo{
			ID: seqID, Index: i, Count: total, Type: "buffer", Len: size,
		}.toMap()
		out.Send(0, cp)
	}
	return nil
}

func splitString(s, delim, spltType string) ([]string, error) {
	if spltType == "len" {
		size, err := parseIntStrict(delim)
		if err != nil {
			return nil, fmt.Errorf("split length: %w", err)
		}
		if size < 1 {
			return nil, fmt.Errorf("split length must be at least 1, got %d", size)
		}
		// Runes, not bytes: splitting a UTF-8 string by byte count produces
		// broken characters at every boundary.
		runes := []rune(s)
		var out []string
		for i := 0; i < len(runes); i += size {
			j := i + size
			if j > len(runes) {
				j = len(runes)
			}
			out = append(out, string(runes[i:j]))
		}
		return out, nil
	}
	if delim == "" {
		return nil, fmt.Errorf("split delimiter is empty")
	}
	return strings.Split(s, delim), nil
}

func chunkSlice(s []any, size int) [][]any {
	if size < 1 {
		size = 1
	}
	var out [][]any
	for i := 0; i < len(s); i += size {
		j := i + size
		if j > len(s) {
			j = len(s)
		}
		out = append(out, s[i:j])
	}
	if len(out) == 0 {
		// An empty array still produces one empty chunk, so a downstream Join
		// sees a complete sequence rather than waiting forever.
		out = append(out, nil)
	}
	return out
}

func parseIntStrict(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, fmt.Errorf("%q is not a whole number", s)
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// join
// ---------------------------------------------------------------------------

type joinNode struct {
	mode     string // auto, custom
	build    string // array, object, string, buffer, merged
	joiner   string
	count    int
	timeout  int
	propName string

	mu    sync.Mutex
	group map[string]*joinGroup
}

type joinGroup struct {
	items    []any
	keys     []string
	expected int
	typ      string
	template *engine.Msg
}

func registerJoin() {
	node.MustRegister(node.Descriptor{
		Type:         "join",
		Category:     node.CategorySequence,
		Color:        colorSequence,
		Icon:         "join",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "join",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Automatic mode rejoins sequences produced by Split, and manual mode " +
				"joins by count. Timeout-based and reduce-sequence modes are not implemented.",
			UnsupportedProps: []string{"timeout", "reduceRight", "reduceExp"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "mode", Kind: node.PropSelect, Label: "Mode", Default: "auto", Options: []node.Option{
				{Value: "auto", Label: "Automatic — rejoin a split sequence"},
				{Value: "custom", Label: "Manual"},
			}},
			{Name: "build", Kind: node.PropSelect, Label: "Combine into", Default: "array",
				Options: []node.Option{
					{Value: "array", Label: "Array"},
					{Value: "object", Label: "Object"},
					{Value: "string", Label: "String"},
					{Value: "buffer", Label: "Buffer"},
				}},
			{Name: "joiner", Kind: node.PropString, Label: "Join using", Default: `\n`},
			{Name: "count", Kind: node.PropNumber, Label: "After this many messages", Default: 0},
			{Name: "property", Kind: node.PropString, Label: "Property", Default: "payload"},
		},
		Help: "Combines a sequence of messages back into one. In automatic mode it " +
			"uses msg.parts, so it undoes exactly what Split did.",
	}, newJoin)
}

func newJoin(def *node.Definition) (node.Node, error) {
	n := &joinNode{
		mode:     def.Node.PropString("mode", "auto"),
		build:    def.Node.PropString("build", "array"),
		joiner:   def.Node.PropString("joiner", "\\n"),
		count:    def.Node.PropInt("count", 0),
		timeout:  def.Node.PropInt("timeout", 0),
		propName: orDefault(def.Node.PropString("property", ""), engine.PropPayload),
		group:    map[string]*joinGroup{},
	}
	n.joiner = strings.NewReplacer(`\n`, "\n", `\r`, "\r", `\t`, "\t").Replace(n.joiner)

	if n.timeout > 0 {
		return nil, fmt.Errorf("join timeout mode is not implemented in this build")
	}
	if n.mode == "custom" && n.count < 1 {
		return nil, fmt.Errorf("manual join mode needs a message count")
	}
	return n, nil
}

func (n *joinNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	value, ok, err := m.Get(n.propName)
	if err != nil {
		return err
	}
	if !ok {
		value = nil
	}

	key := "__manual__"
	var parts partsInfo
	hasParts := false

	if n.mode == "auto" {
		parts, hasParts = readParts(m)
		if !hasParts {
			return fmt.Errorf("automatic join needs msg.parts, which this message does not carry; " +
				"use manual mode or put a Split node upstream")
		}
		key = parts.ID
	}

	n.mu.Lock()
	g, exists := n.group[key]
	if !exists {
		g = &joinGroup{template: m.Clone()}
		if hasParts {
			g.expected = parts.Count
			g.typ = parts.Type
		} else {
			g.expected = n.count
			g.typ = n.build
		}
		n.group[key] = g
	}
	g.items = append(g.items, value)
	if hasParts && parts.Key != "" {
		g.keys = append(g.keys, parts.Key)
	}
	complete := g.expected > 0 && len(g.items) >= g.expected
	if complete {
		delete(n.group, key)
	}
	n.mu.Unlock()

	if !complete {
		return nil
	}

	joined, err := n.combine(g)
	if err != nil {
		return err
	}

	res := g.template
	// The rejoined message is no longer part of a sequence.
	delete(res.Data, engine.PropParts)
	if err := res.Set(n.propName, joined); err != nil {
		return err
	}
	out.Send(0, res)
	return nil
}

func (n *joinNode) combine(g *joinGroup) (any, error) {
	shape := g.typ
	if n.mode == "custom" {
		shape = n.build
	}

	switch shape {
	case "object":
		obj := map[string]any{}
		for i, item := range g.items {
			k := fmt.Sprint(i)
			if i < len(g.keys) {
				k = g.keys[i]
			}
			obj[k] = item
		}
		return obj, nil

	case "string":
		var b strings.Builder
		for i, item := range g.items {
			if i > 0 {
				b.WriteString(n.joiner)
			}
			b.WriteString(fmt.Sprint(item))
		}
		return b.String(), nil

	case "buffer":
		var buf []byte
		for _, item := range g.items {
			switch t := item.(type) {
			case []byte:
				buf = append(buf, t...)
			case engine.ImmutableBytes:
				buf = append(buf, t...)
			case string:
				buf = append(buf, t...)
			default:
				return nil, fmt.Errorf("cannot append %T to a buffer", item)
			}
		}
		return buf, nil

	default: // array
		return append([]any(nil), g.items...), nil
	}
}

// ---------------------------------------------------------------------------
// sort
// ---------------------------------------------------------------------------

type sortNode struct {
	order    string // ascending, descending
	asNumber bool
	target   string
	seq      bool // sort a message sequence rather than an array payload

	mu    sync.Mutex
	group map[string][]*engine.Msg
}

func registerSort() {
	node.MustRegister(node.Descriptor{
		Type:         "sort",
		Category:     node.CategorySequence,
		Color:        colorSequence,
		Icon:         "sort",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "sort",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Sorts array payloads and message sequences by a property. JSONata " +
				"key expressions are not supported.",
			UnsupportedProps: []string{"keyType:jsonata"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "target", Kind: node.PropString, Label: "Sort", Default: "payload"},
			{Name: "seqKey", Kind: node.PropString, Label: "Key", Default: "payload",
				Help: "When sorting a sequence, the property of each message to sort on."},
			{Name: "order", Kind: node.PropSelect, Label: "Direction", Default: "ascending",
				Options: []node.Option{
					{Value: "ascending", Label: "Ascending"},
					{Value: "descending", Label: "Descending"},
				}},
			{Name: "as_num", Kind: node.PropBool, Label: "Compare as numbers"},
		},
		Help: "Sorts an array payload, or a sequence of messages produced by Split.",
	}, newSort)
}

func newSort(def *node.Definition) (node.Node, error) {
	if def.Node.PropString("keyType", "") == "jsonata" || def.Node.PropString("targetType", "") == "jsonata" {
		return nil, fmt.Errorf("sort with a JSONata key is not supported in this build")
	}
	return &sortNode{
		order:    def.Node.PropString("order", "ascending"),
		asNumber: def.Node.PropBool("as_num", false),
		target:   orDefault(def.Node.PropString("target", ""), engine.PropPayload),
		seq:      def.Node.PropString("targetType", "msg") == "seq",
		group:    map[string][]*engine.Msg{},
	}, nil
}

func (n *sortNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	if n.seq {
		return n.sortSequence(m, out)
	}

	value, ok, err := m.Get(n.target)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("message has no property %q", n.target)
	}
	arr, ok := value.([]any)
	if !ok {
		return fmt.Errorf("%s is %T, which is not an array", n.target, value)
	}

	cp := append([]any(nil), arr...)
	sort.SliceStable(cp, func(i, j int) bool { return n.less(cp[i], cp[j]) })

	if err := m.Set(n.target, cp); err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}

func (n *sortNode) sortSequence(m *engine.Msg, out node.Emitter) error {
	parts, ok := readParts(m)
	if !ok {
		return fmt.Errorf("sorting a sequence needs msg.parts, which this message does not carry")
	}

	n.mu.Lock()
	n.group[parts.ID] = append(n.group[parts.ID], m)
	batch := n.group[parts.ID]
	complete := parts.Count > 0 && len(batch) >= parts.Count
	if complete {
		delete(n.group, parts.ID)
	}
	n.mu.Unlock()

	if !complete {
		return nil
	}

	sort.SliceStable(batch, func(i, j int) bool {
		a, _, _ := batch[i].Get(engine.PropPayload)
		b, _, _ := batch[j].Get(engine.PropPayload)
		return n.less(a, b)
	})

	// Renumber so a downstream Join reassembles them in the new order.
	for i, bm := range batch {
		p, _ := readParts(bm)
		p.Index = i
		bm.Data[engine.PropParts] = p.toMap()
		out.Send(0, bm)
	}
	return nil
}

func (n *sortNode) less(a, b any) bool {
	asc := n.order != "descending"

	if n.asNumber {
		af, aOK := asFloat(a)
		bf, bOK := asFloat(b)
		if aOK && bOK {
			if asc {
				return af < bf
			}
			return af > bf
		}
	}

	if cmp, ok := compare(a, b); ok {
		if asc {
			return cmp < 0
		}
		return cmp > 0
	}

	// Not comparable: fall back to the string form so the sort is at least
	// deterministic rather than arbitrary.
	as, bs := fmt.Sprint(a), fmt.Sprint(b)
	if asc {
		return as < bs
	}
	return as > bs
}

// ---------------------------------------------------------------------------
// batch
// ---------------------------------------------------------------------------

type batchNode struct {
	mode  string // count, interval, concat
	count int
	over  int // overlap

	mu      sync.Mutex
	pending []*engine.Msg
}

func registerBatch() {
	node.MustRegister(node.Descriptor{
		Type:         "batch",
		Category:     node.CategorySequence,
		Color:        colorSequence,
		Icon:         "batch",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "batch",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Grouping by message count, with overlap, is supported. Time-interval " +
				"and concatenate-sequences modes are not implemented.",
			UnsupportedProps: []string{"interval", "concat"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "mode", Kind: node.PropSelect, Label: "Mode", Default: "count", Options: []node.Option{
				{Value: "count", Label: "Group by message count"},
			}},
			{Name: "count", Kind: node.PropNumber, Label: "Messages per group", Default: 10},
			{Name: "overlap", Kind: node.PropNumber, Label: "Overlap", Default: 0},
		},
		Help: "Groups a stream of messages into sequences of a fixed size, ready for a Join node.",
	}, newBatch)
}

func newBatch(def *node.Definition) (node.Node, error) {
	n := &batchNode{
		mode:  def.Node.PropString("mode", "count"),
		count: def.Node.PropInt("count", 10),
		over:  def.Node.PropInt("overlap", 0),
	}
	if n.mode != "count" {
		return nil, fmt.Errorf("batch mode %q is not implemented in this build", n.mode)
	}
	if n.count < 1 {
		return nil, fmt.Errorf("batch size must be at least 1, got %d", n.count)
	}
	if n.over < 0 || n.over >= n.count {
		return nil, fmt.Errorf("overlap must be between 0 and %d, got %d", n.count-1, n.over)
	}
	return n, nil
}

func (n *batchNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	n.mu.Lock()
	n.pending = append(n.pending, m)
	if len(n.pending) < n.count {
		n.mu.Unlock()
		return nil
	}
	batch := append([]*engine.Msg(nil), n.pending...)
	// Retain the overlap for the next group, dropping the rest.
	if n.over > 0 {
		n.pending = append([]*engine.Msg(nil), n.pending[len(n.pending)-n.over:]...)
	} else {
		n.pending = nil
	}
	n.mu.Unlock()

	seqID := engine.GenerateID()
	for i, bm := range batch {
		cp := bm
		if n.over > 0 {
			// Overlapping groups reuse messages, so each group needs its own
			// copy or the second group would carry the first group's parts.
			cp = bm.Clone()
		}
		cp.Data[engine.PropParts] = partsInfo{
			ID: seqID, Index: i, Count: len(batch), Type: "array",
		}.toMap()
		out.Send(0, cp)
	}
	return nil
}
