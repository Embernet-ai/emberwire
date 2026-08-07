package nodes

import (
	"strings"
	"testing"
)

func TestTemplateInterpolatesTheMessage(t *testing.T) {
	svc := newTestServices()
	n := build(t, "template", `{"template":"tag {{topic}} reads {{payload.value}}"}`, svc)

	e, err := send(t, n, msg(t, `{"topic":"press-01","payload":{"value":42.5}}`))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	got := e.on(0)[0].Payload()
	if got != "tag press-01 reads 42.5" {
		t.Fatalf("rendered %q", got)
	}
}

// A whole number must not render as 5.000000, which is what Go's default float
// formatting produces. A rendered SQL statement or MQTT topic carrying that is
// wrong in a way nobody notices until it is in a database.
func TestTemplateRendersNumbersAsJavaScriptWould(t *testing.T) {
	svc := newTestServices()
	n := build(t, "template", `{"template":"{{payload.a}}|{{payload.b}}|{{payload.c}}|{{payload.d}}"}`, svc)

	e, err := send(t, n, msg(t, `{"payload":{"a":5,"b":5.25,"c":true,"d":"007"}}`))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != "5|5.25|true|007" {
		t.Fatalf("rendered %q", got)
	}
}

// mustache.js escapes "/" as well as the usual HTML entities, which the Mustache
// specification does not mention. A flow moved over from Node-RED may well be
// compensating for it, so matching matters more than being tidy.
func TestTemplateEscapesTheMustacheJSEntitySet(t *testing.T) {
	svc := newTestServices()
	n := build(t, "template", `{"template":"{{payload}} :: {{{payload}}}"}`, svc)

	e, err := send(t, n, msg(t, `{"payload":"a/b <c> &d 'e' \"f\" =g"}`))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	got, _ := e.on(0)[0].Payload().(string)
	escaped, raw, found := strings.Cut(got, " :: ")
	if !found {
		t.Fatalf("rendered %q", got)
	}
	want := "a&#x2F;b &lt;c&gt; &amp;d &#39;e&#39; &quot;f&quot; &#x3D;g"
	if escaped != want {
		t.Errorf("escaped form:\n got %q\nwant %q", escaped, want)
	}
	if raw != "a/b <c> &d 'e' \"f\" =g" {
		t.Errorf("triple mustache escaped something: %q", raw)
	}
}

func TestTemplateSections(t *testing.T) {
	cases := []struct {
		name     string
		template string
		msg      string
		want     string
	}{
		{
			name:     "iterates an array",
			template: "{{#payload}}[{{name}}={{v}}]{{/payload}}",
			msg:      `{"payload":[{"name":"a","v":1},{"name":"b","v":2}]}`,
			want:     "[a=1][b=2]",
		},
		{
			name:     "dot is the current item",
			template: "{{#payload}}<{{.}}>{{/payload}}",
			msg:      `{"payload":["x","y"]}`,
			want:     "<x><y>",
		},
		{
			name:     "an empty array renders nothing",
			template: "before{{#payload}}never{{/payload}}after",
			msg:      `{"payload":[]}`,
			want:     "beforeafter",
		},
		{
			name:     "an object pushes a scope",
			template: "{{#payload}}{{unit}}{{/payload}}",
			msg:      `{"payload":{"unit":"bar"}}`,
			want:     "bar",
		},
		{
			name:     "an inverted section fires on absence",
			template: "{{^payload}}nothing{{/payload}}",
			msg:      `{"topic":"t"}`,
			want:     "nothing",
		},
		{
			name:     "an inverted section fires on an empty list",
			template: "{{^payload}}nothing{{/payload}}",
			msg:      `{"payload":[]}`,
			want:     "nothing",
		},
		{
			name:     "zero is falsy, as in JavaScript",
			template: "{{^payload}}zero{{/payload}}",
			msg:      `{"payload":0}`,
			want:     "zero",
		},
		{
			name:     "an outer scope is still visible inside a section",
			template: "{{#payload}}{{topic}}:{{v}} {{/payload}}",
			msg:      `{"topic":"line","payload":[{"v":1},{"v":2}]}`,
			want:     "line:1 line:2 ",
		},
		{
			name:     "a missing name renders as nothing",
			template: "[{{payload.nope}}]",
			msg:      `{"payload":{}}`,
			want:     "[]",
		},
		{
			name:     "a numeric segment indexes an array",
			template: "{{payload.1.name}}",
			msg:      `{"payload":[{"name":"a"},{"name":"b"}]}`,
			want:     "b",
		},
		{
			name:     "comments render nothing",
			template: "a{{! this is a note }}b",
			msg:      `{}`,
			want:     "ab",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestServices()
			cfg, err := jsonConfig(map[string]any{"template": tc.template})
			if err != nil {
				t.Fatal(err)
			}
			n := build(t, "template", cfg, svc)
			e, err := send(t, n, msg(t, tc.msg))
			if err != nil {
				t.Fatalf("receive: %v", err)
			}
			if got := e.on(0)[0].Payload(); got != tc.want {
				t.Fatalf("rendered %q, want %q", got, tc.want)
			}
		})
	}
}

// Without standalone-line handling every section tag leaves a blank line behind,
// and a generated YAML or CSV document comes out malformed rather than merely
// untidy.
func TestTemplateStripsStandaloneSectionLines(t *testing.T) {
	svc := newTestServices()
	cfg, err := jsonConfig(map[string]any{
		"template": "items:\n{{#payload}}\n  - {{.}}\n{{/payload}}\ndone\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "template", cfg, svc)

	e, err := send(t, n, msg(t, `{"payload":["a","b"]}`))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	want := "items:\n  - a\n  - b\ndone\n"
	if got := e.on(0)[0].Payload(); got != want {
		t.Fatalf("rendered %q, want %q", got, want)
	}
}

func TestTemplateReadsContextAndEnvironment(t *testing.T) {
	svc := newTestServices()
	if err := svc.Context("flow").Set("setpoint", 72.5); err != nil {
		t.Fatal(err)
	}
	if err := svc.Context("global").Set("site", map[string]any{"name": "ut3"}); err != nil {
		t.Fatal(err)
	}
	svc.env["LINE"] = "3"

	n := build(t, "template",
		`{"template":"{{flow.setpoint}} {{global.site.name}} {{env.LINE}}"}`, svc)

	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != "72.5 ut3 3" {
		t.Fatalf("rendered %q", got)
	}
}

func TestTemplateOutputFormats(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		svc := newTestServices()
		n := build(t, "template",
			`{"template":"{\"v\":{{payload}}}","output":"json"}`, svc)
		e, err := send(t, n, msg(t, `{"payload":7}`))
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		obj, ok := e.on(0)[0].Payload().(map[string]any)
		if !ok {
			t.Fatalf("payload is %T, want an object", e.on(0)[0].Payload())
		}
		if obj["v"] != float64(7) {
			t.Fatalf("v = %v", obj["v"])
		}
	})

	t.Run("yaml decodes to the same shapes as JSON", func(t *testing.T) {
		svc := newTestServices()
		cfg, err := jsonConfig(map[string]any{
			"template": "host: {{payload}}\nport: 1883\ntags:\n  - a\n  - b\n",
			"output":   "yaml",
		})
		if err != nil {
			t.Fatal(err)
		}
		n := build(t, "template", cfg, svc)
		e, err := send(t, n, msg(t, `{"payload":"broker"}`))
		if err != nil {
			t.Fatalf("receive: %v", err)
		}
		obj, ok := e.on(0)[0].Payload().(map[string]any)
		if !ok {
			t.Fatalf("payload is %T, want an object", e.on(0)[0].Payload())
		}
		if obj["host"] != "broker" {
			t.Errorf("host = %v", obj["host"])
		}
		// float64, not int: every number elsewhere in the runtime arrived
		// through encoding/json, and a Switch node comparing an int would not
		// match.
		if got, ok := obj["port"].(float64); !ok || got != 1883 {
			t.Errorf("port = %#v, want float64(1883)", obj["port"])
		}
		if tags, ok := obj["tags"].([]any); !ok || len(tags) != 2 {
			t.Errorf("tags = %#v", obj["tags"])
		}
	})

	t.Run("bad json is refused, not passed on", func(t *testing.T) {
		svc := newTestServices()
		n := build(t, "template", `{"template":"not json","output":"json"}`, svc)
		e, err := send(t, n, msg(t, `{}`))
		if err == nil {
			t.Fatal("expected an error")
		}
		if e.total() != 0 {
			t.Fatal("a message escaped despite the parse failing")
		}
	})
}

func TestTemplatePlainSyntaxDoesNotInterpolate(t *testing.T) {
	svc := newTestServices()
	n := build(t, "template", `{"template":"{{payload}}","syntax":"plain"}`, svc)

	e, err := send(t, n, msg(t, `{"payload":"x"}`))
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != "{{payload}}" {
		t.Fatalf("rendered %q", got)
	}
}

func TestTemplateWritesToContext(t *testing.T) {
	svc := newTestServices()
	n := build(t, "template",
		`{"template":"{{payload}}!","field":"greeting","fieldType":"flow"}`, svc)

	if _, err := send(t, n, msg(t, `{"payload":"hello"}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	v, ok, err := svc.Context("flow").Get("greeting")
	if err != nil || !ok {
		t.Fatalf("flow.greeting: ok=%v err=%v", ok, err)
	}
	if v != "hello!" {
		t.Fatalf("flow.greeting = %v", v)
	}
}

// A malformed template must fail the deploy, where somebody is watching, rather
// than the first message an hour later.
func TestTemplateRefusesAtBuildTime(t *testing.T) {
	cases := map[string]string{
		"unclosed section":  `{"template":"{{#a}}oops"}`,
		"mismatched close":  `{"template":"{{#a}}x{{/b}}"}`,
		"stray close":       `{"template":"x{{/a}}"}`,
		"unclosed braces":   `{"template":"{{a"}`,
		"partials":          `{"template":"{{>header}}"}`,
		"custom delimiters": `{"template":"{{=<% %>=}}"}`,
		"unknown output":    `{"template":"x","output":"toml"}`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := buildErr(t, "template", cfg, newTestServices()); err == nil {
				t.Fatal("expected the node to refuse to build")
			}
		})
	}
}
