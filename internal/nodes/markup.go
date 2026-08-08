package nodes

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strings"

	"golang.org/x/net/html"
	"gopkg.in/yaml.v3"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

func init() {
	registerYAMLNode()
	registerXMLNode()
	registerHTMLNode()
}

// ---------------------------------------------------------------------------
// yaml
// ---------------------------------------------------------------------------

type yamlNode struct {
	action string // "", obj, str
	prop   string
}

func registerYAMLNode() {
	node.MustRegister(node.Descriptor{
		Type:         "yaml",
		Category:     node.CategoryParser,
		Color:        colorParser,
		Icon:         "parser",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "yaml",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatFull,
			Notes: "Conversion in both directions, toggling on the value's type when no " +
				"action is set, as Node-RED's does.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "property", Kind: node.PropString, Label: "Property", Default: "payload"},
			{Name: "action", Kind: node.PropSelect, Label: "Action", Default: "", Options: []node.Option{
				{Value: "", Label: "Convert between YAML text and an object"},
				{Value: "obj", Label: "Always parse to an object"},
				{Value: "str", Label: "Always render to YAML text"},
			}},
		},
		Help: "Converts a property between YAML text and a parsed object. With no " +
			"action set it toggles: text becomes an object and an object becomes text.",
	}, newYAMLNode)
}

func newYAMLNode(def *node.Definition) (node.Node, error) {
	n := &yamlNode{
		action: def.Node.PropString("action", ""),
		prop:   orDefault(def.Node.PropString("property", ""), engine.PropPayload),
	}
	switch n.action {
	case "", "obj", "str":
	default:
		return nil, fmt.Errorf("unknown action %q", n.action)
	}
	return n, nil
}

func (n *yamlNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	value, ok, err := m.Get(n.prop)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("message has no property %q", n.prop)
	}

	action := n.action
	if action == "" {
		switch value.(type) {
		case string, []byte, engine.ImmutableBytes:
			action = "obj"
		default:
			action = "str"
		}
	}

	switch action {
	case "obj":
		raw, ok := textBytes(value)
		if !ok {
			// Already structured. Node-RED passes it through rather than
			// erroring, so a flow works whether the source produced text or an
			// object.
			out.Send(0, m)
			return nil
		}
		var parsed any
		if err := yaml.Unmarshal(raw, &parsed); err != nil {
			return fmt.Errorf("parsing %s as YAML: %w", n.prop, err)
		}
		if err := m.Set(n.prop, normaliseYAML(parsed)); err != nil {
			return err
		}

	case "str":
		if _, isStr := value.(string); isStr {
			out.Send(0, m)
			return nil
		}
		b, err := yaml.Marshal(value)
		if err != nil {
			return fmt.Errorf("rendering %s as YAML: %w", n.prop, err)
		}
		if err := m.Set(n.prop, string(b)); err != nil {
			return err
		}
	}

	out.Send(0, m)
	return nil
}

// textBytes returns the bytes of a value that is already text, and whether it
// was one. The three cases are the three ways a payload arrives: decoded as a
// string, read from a socket, or shared as an immutable buffer.
func textBytes(v any) ([]byte, bool) {
	switch t := v.(type) {
	case string:
		return []byte(t), true
	case []byte:
		return t, true
	case engine.ImmutableBytes:
		return t, true
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// xml
// ---------------------------------------------------------------------------

type xmlNode struct {
	prop          string
	attrKey       string
	charKey       string
	explicitArray bool
}

func registerXMLNode() {
	node.MustRegister(node.Descriptor{
		Type:         "xml",
		Category:     node.CategoryParser,
		Color:        colorParser,
		Icon:         "parser",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "xml",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Both directions, using xml2js's object convention: attributes under " +
				"the key named by \"attr\" (default $), element text under \"chr\" " +
				"(default _), and every child element as an array, which is xml2js's " +
				"own explicitArray default. Set ew_explicitArray to false to collapse " +
				"single children instead. The per-message msg.options that Node-RED " +
				"passes through to xml2js is not honoured — the other xml2js options " +
				"change the shape of the output, and silently ignoring one would " +
				"produce an object the flow does not expect while looking like it " +
				"worked. Namespaces are kept as part of the element name rather than " +
				"being resolved.",
			UnsupportedProps: []string{"options"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "property", Kind: node.PropString, Label: "Property", Default: "payload"},
			{Name: "attr", Kind: node.PropString, Label: "Attribute key", Default: "$"},
			{Name: "chr", Kind: node.PropString, Label: "Text key", Default: "_"},
			{Name: "ew_explicitArray", Kind: node.PropBool, Label: "Every child is an array",
				Default: true,
				Help: "xml2js's own default, and what an imported flow will expect. " +
					"Turning it off collapses an element that appears once into a " +
					"single value, which is easier to read and breaks the moment a " +
					"second one appears."},
		},
		Help: "Converts a property between XML text and an object, following xml2js's " +
			"convention so an imported flow finds the shape it expects.",
	}, newXMLNode)
}

func newXMLNode(def *node.Definition) (node.Node, error) {
	n := &xmlNode{
		prop:          orDefault(def.Node.PropString("property", ""), engine.PropPayload),
		attrKey:       orDefault(def.Node.PropString("attr", ""), "$"),
		charKey:       orDefault(def.Node.PropString("chr", ""), "_"),
		explicitArray: def.Node.PropBool("ew_explicitArray", true),
	}
	if n.attrKey == n.charKey {
		return nil, fmt.Errorf("the attribute key and the text key are both %q; "+
			"attributes and text would overwrite each other", n.attrKey)
	}
	return n, nil
}

func (n *xmlNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	value, ok, err := m.Get(n.prop)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("message has no property %q", n.prop)
	}

	if raw, isText := textBytes(value); isText {
		parsed, err := xmlToValue(raw, n.attrKey, n.charKey, n.explicitArray)
		if err != nil {
			return fmt.Errorf("parsing %s as XML: %w", n.prop, err)
		}
		if err := m.Set(n.prop, parsed); err != nil {
			return err
		}
		out.Send(0, m)
		return nil
	}

	text, err := valueToXML(value, n.attrKey, n.charKey)
	if err != nil {
		return fmt.Errorf("rendering %s as XML: %w", n.prop, err)
	}
	if err := m.Set(n.prop, text); err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}

// xmlElement accumulates one element while decoding.
type xmlElement struct {
	attrs    map[string]any
	order    []string // child element names, in first-appearance order
	children map[string][]any
	text     strings.Builder
}

// xmlToValue decodes an XML document into xml2js's object shape.
func xmlToValue(data []byte, attrKey, charKey string, explicitArray bool) (map[string]any, error) {
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	// Non-UTF-8 documents are common from older PLC web servers. Without a
	// charset reader the decoder refuses them outright; this at least reads the
	// ASCII range of any single-byte encoding rather than failing the message.
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	dec.Strict = false

	var root *xml.StartElement
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("the document contains no elements")
		}
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			root = &se
			break
		}
	}

	value, err := decodeElement(dec, *root, attrKey, charKey, explicitArray)
	if err != nil {
		return nil, err
	}
	return map[string]any{elementName(*root): value}, nil
}

// decodeElement consumes one element, having already read its start tag.
func decodeElement(dec *xml.Decoder, start xml.StartElement, attrKey, charKey string, explicitArray bool) (any, error) {
	el := &xmlElement{children: map[string][]any{}}

	for _, a := range start.Attr {
		if el.attrs == nil {
			el.attrs = map[string]any{}
		}
		el.attrs[attrName(a.Name)] = a.Value
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil, fmt.Errorf("<%s> is not closed", elementName(start))
		}
		if err != nil {
			return nil, err
		}

		switch t := tok.(type) {
		case xml.StartElement:
			child, err := decodeElement(dec, t, attrKey, charKey, explicitArray)
			if err != nil {
				return nil, err
			}
			name := elementName(t)
			if _, seen := el.children[name]; !seen {
				el.order = append(el.order, name)
			}
			el.children[name] = append(el.children[name], child)

		case xml.CharData:
			el.text.Write(t)

		case xml.EndElement:
			return el.value(attrKey, charKey, explicitArray), nil
		}
	}
}

func (e *xmlElement) value(attrKey, charKey string, explicitArray bool) any {
	text := strings.TrimSpace(e.text.String())

	// A leaf with no attributes is its text, which is the shape that makes
	// {{payload.reading}} work instead of {{payload.reading._}}.
	if len(e.attrs) == 0 && len(e.children) == 0 {
		return text
	}

	out := make(map[string]any, len(e.children)+2)
	if len(e.attrs) > 0 {
		out[attrKey] = e.attrs
	}
	if text != "" {
		out[charKey] = text
	}
	for _, name := range e.order {
		vals := e.children[name]
		if !explicitArray && len(vals) == 1 {
			out[name] = vals[0]
			continue
		}
		out[name] = toAnySlice(vals)
	}
	return out
}

func toAnySlice(vals []any) []any {
	out := make([]any, len(vals))
	copy(out, vals)
	return out
}

// elementName renders a name with its namespace prefix intact.
//
// encoding/xml resolves a prefix to its URI, which would turn <soap:Body> into
// a key named after the whole schema URL. Keeping the local name is closer to
// what xml2js produces by default and to what a flow author typed.
func elementName(se xml.StartElement) string { return se.Name.Local }

func attrName(n xml.Name) string {
	// xmlns declarations arrive with the namespace already resolved; keep the
	// local name so the attribute map is readable.
	return n.Local
}

// valueToXML renders an object back to XML.
func valueToXML(v any, attrKey, charKey string) (string, error) {
	root, ok := v.(map[string]any)
	if !ok {
		return "", fmt.Errorf("cannot render %T as XML; the value must be an object with one root element", v)
	}
	if len(root) != 1 {
		return "", fmt.Errorf("an XML document has exactly one root element, but the object has %d keys",
			len(root))
	}

	var name string
	for k := range root {
		name = k
	}

	var b strings.Builder
	if err := writeElement(&b, name, root[name], attrKey, charKey, 0); err != nil {
		return "", err
	}
	return b.String(), nil
}

// maxXMLDepth bounds recursion. The value being rendered came from a message, so
// its depth is attacker-influenced in the same way a clone's is.
const maxXMLDepth = 256

func writeElement(b *strings.Builder, name string, v any, attrKey, charKey string, depth int) error {
	if depth > maxXMLDepth {
		return fmt.Errorf("the object nests more than %d levels deep", maxXMLDepth)
	}
	if name == "" {
		return fmt.Errorf("an element with an empty name cannot be rendered")
	}

	switch t := v.(type) {
	case []any:
		// A repeated element: emit the tag once per entry, which is the inverse
		// of how the decoder collects them.
		for _, item := range t {
			if err := writeElement(b, name, item, attrKey, charKey, depth+1); err != nil {
				return err
			}
		}
		return nil

	case map[string]any:
		b.WriteByte('<')
		b.WriteString(name)
		if attrs, ok := t[attrKey].(map[string]any); ok {
			// Sorted: Go map iteration is randomised, and an XML document whose
			// attribute order changes between messages defeats any downstream
			// diff or checksum.
			for _, k := range sortedMapKeys(attrs) {
				b.WriteString(" ")
				b.WriteString(k)
				b.WriteString(`="`)
				b.WriteString(escapeXML(mustacheString(attrs[k])))
				b.WriteString(`"`)
			}
		}
		b.WriteByte('>')

		if text, ok := t[charKey]; ok {
			b.WriteString(escapeXML(mustacheString(text)))
		}
		for _, k := range sortedMapKeys(t) {
			if k == attrKey || k == charKey {
				continue
			}
			if err := writeElement(b, k, t[k], attrKey, charKey, depth+1); err != nil {
				return err
			}
		}

		b.WriteString("</")
		b.WriteString(name)
		b.WriteByte('>')
		return nil

	default:
		b.WriteByte('<')
		b.WriteString(name)
		b.WriteByte('>')
		b.WriteString(escapeXML(mustacheString(v)))
		b.WriteString("</")
		b.WriteString(name)
		b.WriteByte('>')
		return nil
	}
}

func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// escapeXML escapes text for both element content and attribute values.
//
// xml.EscapeText is the standard-library equivalent but turns a newline into
// &#xA;, which is correct in an attribute and noise in element content. Doing it
// here keeps a rendered document readable in a diff.
var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)

func escapeXML(s string) string { return xmlEscaper.Replace(s) }

// ---------------------------------------------------------------------------
// html
// ---------------------------------------------------------------------------

type htmlNode struct {
	selectorText string
	selector     selectorGroup
	prop         string
	outProp      string
	ret          string // html, text, attr
	multi        bool
}

func registerHTMLNode() {
	node.MustRegister(node.Descriptor{
		Type:         "html",
		Category:     node.CategoryParser,
		Color:        colorParser,
		Icon:         "parser",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "html",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Extracts elements by CSS selector, returning inner HTML, text or " +
				"attributes, as one message per match or one message holding an array. " +
				"The selector engine covers type, id, class, attribute, descendant, " +
				"child and comma groups. Pseudo-classes, pseudo-elements, sibling " +
				"combinators and the ~= and |= attribute operators are refused at " +
				"deploy time rather than ignored, because a selector that quietly " +
				"drops its :nth-child matches the wrong elements and keeps working. " +
				"Returned HTML is re-rendered from the parse tree, so it is normalised " +
				"markup rather than the original bytes.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "property", Kind: node.PropString, Label: "Property", Default: "payload"},
			{Name: "outproperty", Kind: node.PropString, Label: "Output to", Default: "payload"},
			{Name: "tag", Kind: node.PropString, Label: "Selector", Required: true,
				Placeholder: "table.readings td.value"},
			{Name: "ret", Kind: node.PropSelect, Label: "Return", Default: "html", Options: []node.Option{
				{Value: "html", Label: "The HTML inside the element"},
				{Value: "text", Label: "The text inside the element"},
				{Value: "attr", Label: "The element's attributes"},
			}},
			{Name: "as", Kind: node.PropSelect, Label: "Output", Default: "single", Options: []node.Option{
				{Value: "single", Label: "One message holding an array"},
				{Value: "multi", Label: "One message per match"},
			}},
		},
		Help: "Pulls elements out of an HTML document with a CSS selector. Useful for " +
			"reading a device's status page when it has no API, which on a plant " +
			"floor is most of them.",
	}, newHTMLNode)
}

func newHTMLNode(def *node.Definition) (node.Node, error) {
	n := &htmlNode{
		selectorText: strings.TrimSpace(def.Node.PropString("tag", "")),
		prop:         orDefault(def.Node.PropString("property", ""), engine.PropPayload),
		outProp:      orDefault(def.Node.PropString("outproperty", ""), engine.PropPayload),
		ret:          orDefault(def.Node.PropString("ret", ""), "html"),
		multi:        def.Node.PropString("as", "single") == "multi",
	}
	switch n.ret {
	case "html", "text", "attr":
	default:
		return nil, fmt.Errorf("unknown return type %q", n.ret)
	}
	if n.selectorText == "" {
		return nil, fmt.Errorf("no selector configured")
	}

	// Compiled at build time, so an unsupported selector fails the deploy rather
	// than matching nothing forever.
	sel, err := parseSelector(n.selectorText)
	if err != nil {
		return nil, err
	}
	n.selector = sel
	return n, nil
}

func (n *htmlNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	value, ok, err := m.Get(n.prop)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("message has no property %q", n.prop)
	}
	raw, isText := textBytes(value)
	if !isText {
		return fmt.Errorf("%s is %T, want HTML text", n.prop, value)
	}

	doc, err := html.Parse(strings.NewReader(string(raw)))
	if err != nil {
		return fmt.Errorf("parsing %s as HTML: %w", n.prop, err)
	}

	matches := n.selector.Select(doc)
	results := make([]any, 0, len(matches))
	for _, el := range matches {
		v, err := n.extract(el)
		if err != nil {
			return err
		}
		results = append(results, v)
	}

	if !n.multi {
		if err := m.Set(n.outProp, results); err != nil {
			return err
		}
		out.Send(0, m)
		return nil
	}

	// One message per match, stamped as a sequence so a Join downstream can put
	// them back together.
	seqID := engine.GenerateID()
	for i, r := range results {
		cp := m.Clone()
		if err := cp.Set(n.outProp, r); err != nil {
			return err
		}
		cp.Data[engine.PropParts] = partsInfo{
			ID: seqID, Index: i, Count: len(results), Type: "string",
		}.toMap()
		out.Send(0, cp)
	}
	return nil
}

func (n *htmlNode) extract(el *html.Node) (any, error) {
	switch n.ret {
	case "text":
		return nodeText(el), nil
	case "attr":
		attrs := make(map[string]any, len(el.Attr))
		for _, a := range el.Attr {
			attrs[a.Key] = a.Val
		}
		return attrs, nil
	default:
		return nodeInnerHTML(el)
	}
}
