package nodes

import (
	"reflect"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// yaml
// ---------------------------------------------------------------------------

func TestYAMLTogglesBothWays(t *testing.T) {
	svc := newTestServices()
	n := build(t, "yaml", `{}`, svc)

	cfg, err := jsonConfig(map[string]any{"payload": "host: broker\nport: 1883\n"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := send(t, n, msg(t, cfg))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	obj, ok := e.on(0)[0].Payload().(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want an object", e.on(0)[0].Payload())
	}
	// float64, not int: every other number in the runtime arrived through
	// encoding/json, and a Switch node comparing an int would not match.
	if got, ok := obj["port"].(float64); !ok || got != 1883 {
		t.Fatalf("port = %#v, want float64(1883)", obj["port"])
	}

	// Back the other way.
	e2, err := send(t, n, e.on(0)[0])
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	text, _ := e2.on(0)[0].Payload().(string)
	if !strings.Contains(text, "host: broker") {
		t.Fatalf("rendered YAML = %q", text)
	}
}

func TestYAMLRefusesTextThatIsNotYAML(t *testing.T) {
	svc := newTestServices()
	n := build(t, "yaml", `{"action":"obj"}`, svc)

	cfg, err := jsonConfig(map[string]any{"payload": "a:\n  - b\n c: broken\n"})
	if err != nil {
		t.Fatal(err)
	}
	e, err := send(t, n, msg(t, cfg))
	if err == nil {
		t.Fatal("malformed YAML was accepted")
	}
	if e.total() != 0 {
		t.Fatal("a message escaped despite the parse failing")
	}
}

// ---------------------------------------------------------------------------
// xml
// ---------------------------------------------------------------------------

func TestXMLDecodesToTheXML2JSShape(t *testing.T) {
	svc := newTestServices()
	n := build(t, "xml", `{}`, svc)

	doc := `<readings site="ut3"><reading unit="bar">4.2</reading>` +
		`<reading unit="bar">4.5</reading></readings>`
	cfg, err := jsonConfig(map[string]any{"payload": doc})
	if err != nil {
		t.Fatal(err)
	}
	e, err := send(t, n, msg(t, cfg))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}

	root, ok := e.on(0)[0].Payload().(map[string]any)
	if !ok {
		t.Fatalf("payload is %T", e.on(0)[0].Payload())
	}
	readings, ok := root["readings"].(map[string]any)
	if !ok {
		t.Fatalf("readings is %#v", root["readings"])
	}
	attrs, ok := readings["$"].(map[string]any)
	if !ok || attrs["site"] != "ut3" {
		t.Fatalf("attributes = %#v, want them under $", readings["$"])
	}
	// Every child is an array, which is xml2js's explicitArray default and what
	// an imported flow will be indexing into.
	list, ok := readings["reading"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("reading = %#v, want a two-element array", readings["reading"])
	}
	first, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("the first reading is %#v", list[0])
	}
	if first["_"] != "4.2" {
		t.Errorf("element text = %#v, want it under _", first["_"])
	}
}

// A leaf with no attributes is its text. Anything else would make the common
// case read as payload.reading[0]._ instead of payload.reading[0].
func TestXMLLeafElementsAreTheirText(t *testing.T) {
	svc := newTestServices()
	n := build(t, "xml", `{}`, svc)

	cfg, err := jsonConfig(map[string]any{"payload": `<a><b>hello</b></a>`})
	if err != nil {
		t.Fatal(err)
	}
	e, err := send(t, n, msg(t, cfg))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	root := e.on(0)[0].Payload().(map[string]any)
	a := root["a"].(map[string]any)
	list, ok := a["b"].([]any)
	if !ok || len(list) != 1 || list[0] != "hello" {
		t.Fatalf("b = %#v, want [\"hello\"]", a["b"])
	}
}

func TestXMLCollapsesSingleChildrenWhenAsked(t *testing.T) {
	svc := newTestServices()
	n := build(t, "xml", `{"ew_explicitArray":false}`, svc)

	cfg, err := jsonConfig(map[string]any{"payload": `<a><b>hello</b></a>`})
	if err != nil {
		t.Fatal(err)
	}
	e, err := send(t, n, msg(t, cfg))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	root := e.on(0)[0].Payload().(map[string]any)
	a := root["a"].(map[string]any)
	if a["b"] != "hello" {
		t.Fatalf("b = %#v, want the value collapsed out of its array", a["b"])
	}
}

func TestXMLRoundTrips(t *testing.T) {
	svc := newTestServices()
	n := build(t, "xml", `{}`, svc)

	doc := `<readings site="ut3"><reading unit="bar">4.2</reading><reading unit="bar">4.5</reading></readings>`
	cfg, err := jsonConfig(map[string]any{"payload": doc})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := send(t, n, msg(t, cfg))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	encoded, err := send(t, n, decoded.on(0)[0])
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, _ := encoded.on(0)[0].Payload().(string)
	if got != doc {
		t.Fatalf("round trip:\n got %s\nwant %s", got, doc)
	}
}

func TestXMLEscapesRenderedText(t *testing.T) {
	svc := newTestServices()
	n := build(t, "xml", `{}`, svc)

	m := msg(t, `{"payload":{"note":"a < b & c"}}`)
	e, err := send(t, n, m)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	got, _ := e.on(0)[0].Payload().(string)
	if got != "<note>a &lt; b &amp; c</note>" {
		t.Fatalf("rendered %q", got)
	}
}

func TestXMLRefusesAmbiguousDocuments(t *testing.T) {
	svc := newTestServices()
	n := build(t, "xml", `{}`, svc)

	t.Run("two roots", func(t *testing.T) {
		if _, err := send(t, n, msg(t, `{"payload":{"a":1,"b":2}}`)); err == nil {
			t.Fatal("an object with two top-level keys rendered as XML")
		}
	})
	t.Run("a scalar payload", func(t *testing.T) {
		if _, err := send(t, n, msg(t, `{"payload":42}`)); err == nil {
			t.Fatal("a bare number rendered as XML")
		}
	})
	t.Run("text that is not XML", func(t *testing.T) {
		if _, err := send(t, n, msg(t, `{"payload":"not xml at all"}`)); err == nil {
			t.Fatal("text with no elements was accepted")
		}
	})
}

// Attributes and text sharing a key would overwrite each other, which is the
// kind of silent data loss that only shows up under a document nobody tested.
func TestXMLRefusesCollidingKeys(t *testing.T) {
	if err := buildErr(t, "xml", `{"attr":"x","chr":"x"}`, newTestServices()); err == nil {
		t.Fatal("the node built with the attribute key equal to the text key")
	}
}

// ---------------------------------------------------------------------------
// html
// ---------------------------------------------------------------------------

const testPage = `<html><body>
<table class="readings" id="main">
  <tr><td class="tag">press-01</td><td class="value" data-unit="bar">4.2</td></tr>
  <tr><td class="tag">press-02</td><td class="value" data-unit="bar">4.5</td></tr>
</table>
<div class="value">not in the table</div>
</body></html>`

func TestHTMLSelectsText(t *testing.T) {
	svc := newTestServices()
	cfg, err := jsonConfig(map[string]any{"tag": "table.readings td.value", "ret": "text"})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "html", cfg, svc)

	page, err := jsonConfig(map[string]any{"payload": testPage})
	if err != nil {
		t.Fatal(err)
	}
	e, err := send(t, n, msg(t, page))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}

	got, ok := e.on(0)[0].Payload().([]any)
	if !ok {
		t.Fatalf("payload is %T, want an array", e.on(0)[0].Payload())
	}
	// The div outside the table must not match: the descendant combinator is
	// the entire point of the selector.
	if !reflect.DeepEqual(got, []any{"4.2", "4.5"}) {
		t.Fatalf("matched %#v", got)
	}
}

func TestHTMLReturnsAttributes(t *testing.T) {
	svc := newTestServices()
	cfg, err := jsonConfig(map[string]any{"tag": "td[data-unit]", "ret": "attr"})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "html", cfg, svc)

	page, err := jsonConfig(map[string]any{"payload": testPage})
	if err != nil {
		t.Fatal(err)
	}
	e, err := send(t, n, msg(t, page))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	list := e.on(0)[0].Payload().([]any)
	if len(list) != 2 {
		t.Fatalf("matched %d elements, want 2", len(list))
	}
	first := list[0].(map[string]any)
	if first["data-unit"] != "bar" || first["class"] != "value" {
		t.Fatalf("attributes = %#v", first)
	}
}

func TestHTMLOneMessagePerMatch(t *testing.T) {
	svc := newTestServices()
	cfg, err := jsonConfig(map[string]any{"tag": "td.tag", "ret": "text", "as": "multi"})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "html", cfg, svc)

	page, err := jsonConfig(map[string]any{"payload": testPage})
	if err != nil {
		t.Fatal(err)
	}
	e, err := send(t, n, msg(t, page))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}

	sent := e.on(0)
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want one per match", len(sent))
	}
	if sent[0].Payload() != "press-01" || sent[1].Payload() != "press-02" {
		t.Fatalf("payloads %v, %v", sent[0].Payload(), sent[1].Payload())
	}
	// Stamped as a sequence, so a Join downstream can put them back together.
	second, ok := readParts(sent[1])
	if !ok || second.Index != 1 || second.Count != 2 {
		t.Fatalf("msg.parts = %#v", sent[1].Data["parts"])
	}
	first, _ := readParts(sent[0])
	if second.ID != first.ID {
		t.Error("the two messages carry different sequence ids")
	}
}

func TestHTMLSelectorSubset(t *testing.T) {
	cases := []struct {
		selector string
		want     []any
	}{
		{"#main td.tag", []any{"press-01", "press-02"}},
		{"tr > td.value", []any{"4.2", "4.5"}},
		{"td.tag, div.value", []any{"press-01", "press-02", "not in the table"}},
		{`td[data-unit=bar]`, []any{"4.2", "4.5"}},
		{`td[data-unit^=b]`, []any{"4.2", "4.5"}},
		{`td[data-unit$=r]`, []any{"4.2", "4.5"}},
		{`td[data-unit*=a]`, []any{"4.2", "4.5"}},
		{`table#main > tr > td.tag`, nil}, // the parser inserts tbody
	}

	for _, tc := range cases {
		t.Run(tc.selector, func(t *testing.T) {
			cfg, err := jsonConfig(map[string]any{"tag": tc.selector, "ret": "text"})
			if err != nil {
				t.Fatal(err)
			}
			n := build(t, "html", cfg, newTestServices())
			page, err := jsonConfig(map[string]any{"payload": testPage})
			if err != nil {
				t.Fatal(err)
			}
			e, err := send(t, n, msg(t, page))
			if err != nil {
				t.Fatalf("receive: %v", err)
			}
			got := e.on(0)[0].Payload().([]any)
			if len(got) == 0 && tc.want == nil {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("matched %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A selector whose pseudo-class is quietly dropped matches the wrong elements
// and keeps working, which is how a flow ends up reading the wrong meter for a
// month. Refusing at deploy time is the whole point.
func TestHTMLRefusesUnsupportedSelectors(t *testing.T) {
	for _, sel := range []string{
		"td:nth-child(2)", "p::first-line", "h1 + p", "h1 ~ p",
		`[class~=value]`, `[lang|=en]`, "svg|circle", "> td", "td >", "",
	} {
		cfg, err := jsonConfig(map[string]any{"tag": sel})
		if err != nil {
			t.Fatal(err)
		}
		if err := buildErr(t, "html", cfg, newTestServices()); err == nil {
			t.Errorf("selector %q was accepted", sel)
		}
	}
}

func TestHTMLRefusesANonTextPayload(t *testing.T) {
	cfg, err := jsonConfig(map[string]any{"tag": "td"})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "html", cfg, newTestServices())
	if _, err := send(t, n, msg(t, `{"payload":{"not":"html"}}`)); err == nil {
		t.Fatal("an object payload was accepted as HTML")
	}
}
