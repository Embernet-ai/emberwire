package nodes

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

const colorParser = "#DEBD5C"

func init() {
	registerJSONNode()
	registerCSVNode()
}

// ---------------------------------------------------------------------------
// json
// ---------------------------------------------------------------------------

type jsonNode struct {
	action string // toggle, str, obj
	prop   string
	indent int
}

func registerJSONNode() {
	node.MustRegister(node.Descriptor{
		Type:         "json",
		Category:     node.CategoryParser,
		Color:        colorParser,
		Icon:         "parser",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "json",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Conversion in both directions is supported. Schema validation " +
				"against msg.schema is not implemented in this build.",
			UnsupportedProps: []string{"schema"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "property", Kind: node.PropString, Label: "Property", Default: "payload"},
			{Name: "action", Kind: node.PropSelect, Label: "Action", Default: "", Options: []node.Option{
				{Value: "", Label: "Convert between JSON text and an object"},
				{Value: "obj", Label: "Always parse to an object"},
				{Value: "str", Label: "Always render to JSON text"},
			}},
			{Name: "pretty", Kind: node.PropBool, Label: "Format the JSON text"},
		},
		Help: "Converts a property between JSON text and a parsed object. With no " +
			"action set it toggles: text becomes an object and an object becomes text.",
	}, newJSONNode)
}

func newJSONNode(def *node.Definition) (node.Node, error) {
	n := &jsonNode{
		action: def.Node.PropString("action", ""),
		prop:   orDefault(def.Node.PropString("property", ""), engine.PropPayload),
	}
	if def.Node.PropBool("pretty", false) {
		n.indent = 4
	}
	switch n.action {
	case "", "obj", "str":
	default:
		return nil, fmt.Errorf("unknown action %q", n.action)
	}
	return n, nil
}

func (n *jsonNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	value, ok, err := m.Get(n.prop)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("message has no property %q", n.prop)
	}

	action := n.action
	if action == "" {
		// Toggle: whichever direction the value is not already in.
		switch value.(type) {
		case string, []byte, engine.ImmutableBytes:
			action = "obj"
		default:
			action = "str"
		}
	}

	switch action {
	case "obj":
		var raw []byte
		switch t := value.(type) {
		case string:
			raw = []byte(t)
		case []byte:
			raw = t
		case engine.ImmutableBytes:
			raw = t
		default:
			// Already an object. Node-RED passes it through rather than
			// erroring, so a flow with a JSON node in front of a database node
			// works whether the source produced text or an object.
			out.Send(0, m)
			return nil
		}
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return fmt.Errorf("parsing %s as JSON: %w", n.prop, err)
		}
		if err := m.Set(n.prop, parsed); err != nil {
			return err
		}

	case "str":
		if s, isStr := value.(string); isStr {
			// Already text; do not double-encode it into a quoted string.
			_ = s
			out.Send(0, m)
			return nil
		}
		var (
			b   []byte
			err error
		)
		if n.indent > 0 {
			b, err = json.MarshalIndent(value, "", strings.Repeat(" ", n.indent))
		} else {
			b, err = json.Marshal(value)
		}
		if err != nil {
			return fmt.Errorf("rendering %s as JSON: %w", n.prop, err)
		}
		if err := m.Set(n.prop, string(b)); err != nil {
			return err
		}
	}

	out.Send(0, m)
	return nil
}

// ---------------------------------------------------------------------------
// csv
// ---------------------------------------------------------------------------

type csvNode struct {
	columns   []string
	separator rune
	hdrin     bool
	hdrout    string // none, all, once
	parseNum  bool
	sendAsOne bool
	prop      string

	wroteHeader bool
}

func registerCSVNode() {
	node.MustRegister(node.Descriptor{
		Type:         "csv",
		Category:     node.CategoryParser,
		Color:        colorParser,
		Icon:         "parser",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "csv",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Parsing to objects and rendering from objects are supported, with " +
				"configurable separator and header handling. Multi-line quoted fields " +
				"spanning separate messages are not reassembled.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "property", Kind: node.PropString, Label: "Property", Default: "payload"},
			{Name: "temp", Kind: node.PropString, Label: "Columns",
				Help: "Comma-separated column names. Leave empty to take them from the first row."},
			{Name: "sep", Kind: node.PropString, Label: "Separator", Default: ","},
			{Name: "hdrin", Kind: node.PropBool, Label: "The first row contains column names"},
			{Name: "hdrout", Kind: node.PropSelect, Label: "Write a header row", Default: "none",
				Options: []node.Option{
					{Value: "none", Label: "Never"},
					{Value: "all", Label: "On every message"},
					{Value: "once", Label: "Once, on the first message"},
				}},
			{Name: "strings", Kind: node.PropBool, Label: "Parse numeric values", Default: true},
			{Name: "multi", Kind: node.PropSelect, Label: "Output", Default: "one",
				Options: []node.Option{
					{Value: "one", Label: "One message per row"},
					{Value: "mult", Label: "One message holding an array of rows"},
				}},
		},
		Help: "Converts between CSV text and objects. Parsing a CSV string produces " +
			"one message per row; sending an object or array produces CSV text.",
	}, newCSVNode)
}

func newCSVNode(def *node.Definition) (node.Node, error) {
	n := &csvNode{
		prop:      orDefault(def.Node.PropString("property", ""), engine.PropPayload),
		hdrin:     def.Node.PropBool("hdrin", false),
		hdrout:    def.Node.PropString("hdrout", "none"),
		parseNum:  def.Node.PropBool("strings", true),
		sendAsOne: def.Node.PropString("multi", "one") == "mult",
		separator: ',',
	}

	if cols := strings.TrimSpace(def.Node.PropString("temp", "")); cols != "" {
		for _, c := range strings.Split(cols, ",") {
			if c = strings.TrimSpace(c); c != "" {
				n.columns = append(n.columns, c)
			}
		}
	}

	sep := def.Node.PropString("sep", ",")
	// Node-RED stores tab as the literal two-character escape.
	sep = strings.NewReplacer(`\t`, "\t", `\n`, "\n").Replace(sep)
	if sep != "" {
		r := []rune(sep)
		if len(r) != 1 {
			return nil, fmt.Errorf("separator must be a single character, got %q", sep)
		}
		n.separator = r[0]
	}

	if len(n.columns) == 0 && !n.hdrin {
		return nil, fmt.Errorf("either list the columns or set that the first row contains column names")
	}
	return n, nil
}

func (n *csvNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	value, ok, err := m.Get(n.prop)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("message has no property %q", n.prop)
	}

	switch t := value.(type) {
	case string:
		return n.parse(t, m, out)
	case []byte:
		return n.parse(string(t), m, out)
	case engine.ImmutableBytes:
		return n.parse(string(t), m, out)
	default:
		return n.render(value, m, out)
	}
}

func (n *csvNode) parse(text string, m *engine.Msg, out node.Emitter) error {
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = n.separator
	// Rows with a different field count are common in real exports and are far
	// better handled per-row than by refusing the whole message.
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	columns := append([]string(nil), n.columns...)
	var rows []any

	for i := 0; ; i++ {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading CSV row %d: %w", i+1, err)
		}

		if i == 0 && n.hdrin {
			columns = make([]string, 0, len(rec))
			for _, c := range rec {
				columns = append(columns, strings.TrimSpace(c))
			}
			continue
		}

		row := make(map[string]any, len(rec))
		for j, field := range rec {
			name := ""
			if j < len(columns) {
				name = columns[j]
			}
			if name == "" {
				// A column with no name still has to go somewhere or the data
				// is silently dropped.
				name = "col" + strconv.Itoa(j+1)
			}
			row[name] = n.coerce(field)
		}
		rows = append(rows, row)
	}

	if n.sendAsOne {
		if err := m.Set(n.prop, rows); err != nil {
			return err
		}
		out.Send(0, m)
		return nil
	}

	// One message per row, stamped as a sequence so a Join or a batching
	// database insert downstream can reassemble them.
	seqID := engine.GenerateID()
	for i, row := range rows {
		cp := m.Clone()
		if err := cp.Set(n.prop, row); err != nil {
			return err
		}
		cp.Data[engine.PropParts] = partsInfo{
			ID: seqID, Index: i, Count: len(rows), Type: "object",
		}.toMap()
		out.Send(0, cp)
	}
	return nil
}

// coerce turns a CSV field into a number when it looks like one and the node is
// configured to.
//
// A leading zero is kept as a string: "007" is a device id, not the number 7,
// and turning it into one loses the identity of the thing being measured.
func (n *csvNode) coerce(field string) any {
	if !n.parseNum {
		return field
	}
	s := strings.TrimSpace(field)
	if s == "" {
		return ""
	}
	if len(s) > 1 && s[0] == '0' && s[1] != '.' {
		return field
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return field
}

func (n *csvNode) render(value any, m *engine.Msg, out node.Emitter) error {
	var rows []map[string]any
	switch t := value.(type) {
	case map[string]any:
		rows = []map[string]any{t}
	case []any:
		for _, e := range t {
			row, ok := e.(map[string]any)
			if !ok {
				return fmt.Errorf("array element is %T, want an object", e)
			}
			rows = append(rows, row)
		}
	default:
		return fmt.Errorf("cannot render %T as CSV", value)
	}

	columns := n.columns
	if len(columns) == 0 && len(rows) > 0 {
		// Sorted, so repeated messages produce the same column order. Go map
		// iteration is randomised, and a CSV whose columns move between rows is
		// unusable.
		for k := range rows[0] {
			columns = append(columns, k)
		}
		sortStrings(columns)
	}

	var b strings.Builder
	w := csv.NewWriter(&b)
	w.Comma = n.separator

	writeHeader := n.hdrout == "all" || (n.hdrout == "once" && !n.wroteHeader)
	if writeHeader {
		if err := w.Write(columns); err != nil {
			return err
		}
		n.wroteHeader = true
	}

	for _, row := range rows {
		rec := make([]string, len(columns))
		for i, c := range columns {
			rec[i] = csvField(row[c])
		}
		if err := w.Write(rec); err != nil {
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		return fmt.Errorf("writing CSV: %w", err)
	}

	if err := m.Set(n.prop, b.String()); err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}

func csvField(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	default:
		if f, ok := asFloat(v); ok {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
