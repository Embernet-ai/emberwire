package nodes

import (
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/embernet-ai/emberwire/internal/node"
)

// ---------------------------------------------------------------------------
// change
// ---------------------------------------------------------------------------

func TestChangeSet(t *testing.T) {
	svc := newTestServices()
	n := build(t, "change", `{"rules":[
        {"t":"set","p":"payload","pt":"msg","to":"hello","tot":"str"},
        {"t":"set","p":"topic","pt":"msg","to":"sensor/1","tot":"str"},
        {"t":"set","p":"count","pt":"msg","to":"42","tot":"num"},
        {"t":"set","p":"ok","pt":"msg","to":"true","tot":"bool"},
        {"t":"set","p":"cfg","pt":"msg","to":"{\"a\":1}","tot":"json"}
    ]}`, svc)

	e, err := send(t, n, msg(t, `{"payload":"original"}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	out := e.on(0)[0]

	if got := out.Payload(); got != "hello" {
		t.Errorf("payload = %#v", got)
	}
	if got := out.Topic(); got != "sensor/1" {
		t.Errorf("topic = %#v", got)
	}
	// A num-typed value must land as a number, not the string "42", or every
	// downstream comparison behaves differently.
	if got, _, _ := out.Get("count"); got != 42.0 {
		t.Errorf("count = %#v (%T), want float64 42", got, got)
	}
	if got, _, _ := out.Get("ok"); got != true {
		t.Errorf("ok = %#v (%T), want bool true", got, got)
	}
	if got, _, _ := out.Get("cfg.a"); got != 1.0 {
		t.Errorf("cfg.a = %#v", got)
	}
}

func TestChangeSetFromMessageProperty(t *testing.T) {
	svc := newTestServices()
	n := build(t, "change", `{"rules":[
        {"t":"set","p":"payload","pt":"msg","to":"payload.reading.value","tot":"msg"}
    ]}`, svc)

	e, err := send(t, n, msg(t, `{"payload":{"reading":{"value":21.5,"unit":"C"}}}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != 21.5 {
		t.Errorf("payload = %#v, want 21.5", got)
	}
}

func TestChangeSetDeletesWhenSourceMissing(t *testing.T) {
	// Node-RED removes the target rather than writing undefined when the source
	// does not resolve.
	svc := newTestServices()
	n := build(t, "change", `{"rules":[
        {"t":"set","p":"payload","pt":"msg","to":"nope.not.here","tot":"msg"}
    ]}`, svc)

	e, err := send(t, n, msg(t, `{"payload":"original"}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if _, ok, _ := e.on(0)[0].Get("payload"); ok {
		t.Error("payload still present; an unresolvable source should delete the target")
	}
}

func TestChangeContextRoundTrip(t *testing.T) {
	svc := newTestServices()
	// Write into flow context, then read it back out into the message.
	writer := build(t, "change", `{"rules":[
        {"t":"set","p":"lastReading","pt":"flow","to":"payload","tot":"msg"}
    ]}`, svc)
	if _, err := send(t, writer, msg(t, `{"payload":99.5}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	reader := build(t, "change", `{"rules":[
        {"t":"set","p":"payload","pt":"msg","to":"lastReading","tot":"flow"}
    ]}`, svc)
	e, err := send(t, reader, msg(t, `{"payload":"replace me"}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != 99.5 {
		t.Errorf("payload from flow context = %#v, want 99.5", got)
	}
}

func TestChangeContextNestedPath(t *testing.T) {
	// flow.get("a.b") reads property b of what is stored under "a" — the key
	// and the path into it are different things.
	svc := newTestServices()
	n := build(t, "change", `{"rules":[
        {"t":"set","p":"stats.count","pt":"flow","to":"7","tot":"num"}
    ]}`, svc)
	if _, err := send(t, n, msg(t, `{}`)); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	stored, ok, err := svc.Context(node.ScopeFlow).Get("stats")
	if err != nil || !ok {
		t.Fatalf("flow context key \"stats\" missing (err=%v)", err)
	}
	obj, ok := stored.(map[string]any)
	if !ok {
		t.Fatalf("stats is %T, want an object", stored)
	}
	if obj["count"] != 7.0 {
		t.Errorf("stats.count = %#v, want 7", obj["count"])
	}
}

func TestChangeDeleteAndMove(t *testing.T) {
	svc := newTestServices()
	n := build(t, "change", `{"rules":[
        {"t":"move","p":"payload","pt":"msg","to":"reading","tot":"msg"},
        {"t":"delete","p":"junk","pt":"msg"}
    ]}`, svc)

	e, err := send(t, n, msg(t, `{"payload":123,"junk":"remove me"}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	out := e.on(0)[0]

	if got, _, _ := out.Get("reading"); got != 123.0 {
		t.Errorf("reading = %#v, want 123", got)
	}
	if _, ok, _ := out.Get("payload"); ok {
		t.Error("payload survived a move; move must delete the source")
	}
	if _, ok, _ := out.Get("junk"); ok {
		t.Error("junk survived a delete")
	}
}

func TestChangeSearchAndReplace(t *testing.T) {
	svc := newTestServices()

	t.Run("substring", func(t *testing.T) {
		n := build(t, "change", `{"rules":[
            {"t":"change","p":"payload","pt":"msg","from":"WARN","fromt":"str","to":"ALERT","tot":"str"}
        ]}`, svc)
		e, err := send(t, n, msg(t, `{"payload":"WARN: pressure high, WARN: again"}`))
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		want := "ALERT: pressure high, ALERT: again"
		if got := e.on(0)[0].Payload(); got != want {
			t.Errorf("payload = %#v, want %#v", got, want)
		}
	})

	t.Run("regex", func(t *testing.T) {
		n := build(t, "change", `{"rules":[
            {"t":"change","p":"payload","pt":"msg","from":"[0-9]+","fromt":"re","to":"N","tot":"str"}
        ]}`, svc)
		e, err := send(t, n, msg(t, `{"payload":"line 42 of 99"}`))
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if got := e.on(0)[0].Payload(); got != "line N of N" {
			t.Errorf("payload = %#v", got)
		}
	})

	t.Run("whole value keeps replacement type", func(t *testing.T) {
		// Replacing an entire value must preserve the replacement's type, not
		// stringify it.
		n := build(t, "change", `{"rules":[
            {"t":"change","p":"payload","pt":"msg","from":"off","fromt":"str","to":"0","tot":"num"}
        ]}`, svc)
		e, err := send(t, n, msg(t, `{"payload":"off"}`))
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if got := e.on(0)[0].Payload(); got != 0.0 {
			t.Errorf("payload = %#v (%T), want float64 0", got, got)
		}
	})

	t.Run("non-string is untouched", func(t *testing.T) {
		n := build(t, "change", `{"rules":[
            {"t":"change","p":"payload","pt":"msg","from":"1","fromt":"str","to":"X","tot":"str"}
        ]}`, svc)
		e, err := send(t, n, msg(t, `{"payload":{"a":1}}`))
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		got := e.on(0)[0].Payload()
		if !reflect.DeepEqual(got, map[string]any{"a": 1.0}) {
			t.Errorf("payload = %#v, want the object left alone", got)
		}
	})
}

func TestChangeRejectsRuleWithoutProperty(t *testing.T) {
	if err := buildErr(t, "change", `{"rules":[{"t":"set","to":"x","tot":"str"}]}`, newTestServices()); err == nil {
		t.Error("a rule with no target property was accepted")
	}
}

func TestChangeRejectsBadRegex(t *testing.T) {
	err := buildErr(t, "change", `{"rules":[
        {"t":"change","p":"payload","from":"([unclosed","fromt":"re","to":"x","tot":"str"}
    ]}`, newTestServices())
	if err == nil {
		t.Fatal("an invalid regular expression was accepted at build time")
	}
	if !strings.Contains(err.Error(), "regular expression") {
		t.Errorf("error = %q, want it to name the regular expression", err)
	}
}

func TestChangeJSONataIsRefusedNotIgnored(t *testing.T) {
	// Silently returning the expression text would make the flow appear to work
	// while setting a property to a literal string.
	svc := newTestServices()
	n := build(t, "change", `{"rules":[
        {"t":"set","p":"payload","pt":"msg","to":"$sum(payload)","tot":"jsonata"}
    ]}`, svc)
	_, err := send(t, n, msg(t, `{"payload":[1,2,3]}`))
	if err == nil {
		t.Fatal("a JSONata expression was silently accepted")
	}
	if !strings.Contains(err.Error(), "JSONata") {
		t.Errorf("error = %q, want it to name JSONata", err)
	}
}

// ---------------------------------------------------------------------------
// switch
// ---------------------------------------------------------------------------

func TestSwitchRoutesByRule(t *testing.T) {
	svc := newTestServices()
	n := build(t, "switch", `{"property":"payload","propertyType":"msg","checkall":"true","rules":[
        {"t":"lt","v":"10","vt":"num"},
        {"t":"btwn","v":"10","vt":"num","v2":"20","v2t":"num"},
        {"t":"else"}
    ]}`, svc)

	cases := []struct {
		payload  string
		wantPort int
	}{
		{`{"payload":5}`, 0},
		{`{"payload":15}`, 1},
		{`{"payload":100}`, 2},
		{`{"payload":10}`, 1}, // btwn is inclusive
		{`{"payload":20}`, 1},
	}
	for _, c := range cases {
		e, err := send(t, n, msg(t, c.payload))
		if err != nil {
			t.Fatalf("%s: %v", c.payload, err)
		}
		ports := e.ports()
		if len(ports) != 1 || ports[0] != c.wantPort {
			t.Errorf("%s routed to ports %v, want [%d]", c.payload, ports, c.wantPort)
		}
	}
}

func TestSwitchCheckAllVersusStopAtFirst(t *testing.T) {
	svc := newTestServices()
	rules := `"rules":[{"t":"gt","v":"0","vt":"num"},{"t":"gt","v":"5","vt":"num"}]`

	all := build(t, "switch", `{"property":"payload","checkall":"true",`+rules+`}`, svc)
	e, err := send(t, all, msg(t, `{"payload":10}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if e.total() != 2 {
		t.Errorf("check-all matched %d rules, want 2", e.total())
	}

	first := build(t, "switch", `{"property":"payload","checkall":"false",`+rules+`}`, svc)
	e2, err := send(t, first, msg(t, `{"payload":10}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if e2.total() != 1 {
		t.Errorf("stop-at-first matched %d rules, want 1", e2.total())
	}
}

func TestSwitchElseOnlyWhenNothingMatched(t *testing.T) {
	svc := newTestServices()
	n := build(t, "switch", `{"property":"payload","checkall":"true","rules":[
        {"t":"eq","v":"on","vt":"str"},
        {"t":"else"}
    ]}`, svc)

	e, _ := send(t, n, msg(t, `{"payload":"on"}`))
	if ports := e.ports(); len(ports) != 1 || ports[0] != 0 {
		t.Errorf("matching message went to %v, want [0] only — else must not also fire", ports)
	}

	e2, _ := send(t, n, msg(t, `{"payload":"off"}`))
	if ports := e2.ports(); len(ports) != 1 || ports[0] != 1 {
		t.Errorf("non-matching message went to %v, want [1]", ports)
	}
}

func TestSwitchOperators(t *testing.T) {
	svc := newTestServices()

	cases := []struct {
		name    string
		rule    string
		payload string
		match   bool
	}{
		{"eq string", `{"t":"eq","v":"on","vt":"str"}`, `{"payload":"on"}`, true},
		{"eq number as string", `{"t":"eq","v":"5","vt":"num"}`, `{"payload":5}`, true},
		{"neq", `{"t":"neq","v":"on","vt":"str"}`, `{"payload":"off"}`, true},
		{"gte boundary", `{"t":"gte","v":"10","vt":"num"}`, `{"payload":10}`, true},
		{"lte boundary", `{"t":"lte","v":"10","vt":"num"}`, `{"payload":10}`, true},
		{"cont", `{"t":"cont","v":"err","vt":"str"}`, `{"payload":"an error here"}`, true},
		{"cont miss", `{"t":"cont","v":"err","vt":"str"}`, `{"payload":"all fine"}`, false},
		{"regex", `{"t":"regex","v":"^ALARM","vt":"str"}`, `{"payload":"ALARM: high"}`, true},
		{"regex case-insensitive by default", `{"t":"regex","v":"^alarm","vt":"str"}`, `{"payload":"ALARM"}`, true},
		{"true", `{"t":"true"}`, `{"payload":true}`, true},
		{"true against string", `{"t":"true"}`, `{"payload":"true"}`, false},
		{"false", `{"t":"false"}`, `{"payload":false}`, true},
		{"null", `{"t":"null"}`, `{"payload":null}`, true},
		{"nnull", `{"t":"nnull"}`, `{"payload":0}`, true},
		{"empty string", `{"t":"empty"}`, `{"payload":""}`, true},
		{"empty array", `{"t":"empty"}`, `{"payload":[]}`, true},
		{"empty object", `{"t":"empty"}`, `{"payload":{}}`, true},
		{"zero is not empty", `{"t":"empty"}`, `{"payload":0}`, false},
		{"nempty", `{"t":"nempty"}`, `{"payload":"x"}`, true},
		{"istype number", `{"t":"istype","v":"number","vt":"str"}`, `{"payload":1}`, true},
		{"istype string", `{"t":"istype","v":"string","vt":"str"}`, `{"payload":"1"}`, true},
		{"istype array", `{"t":"istype","v":"array","vt":"str"}`, `{"payload":[1]}`, true},
		{"istype object", `{"t":"istype","v":"object","vt":"str"}`, `{"payload":{}}`, true},
		{"hask", `{"t":"hask","v":"unit","vt":"str"}`, `{"payload":{"unit":"C"}}`, true},
		{"hask miss", `{"t":"hask","v":"unit","vt":"str"}`, `{"payload":{"value":1}}`, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := build(t, "switch", `{"property":"payload","checkall":"true","rules":[`+c.rule+`]}`, svc)
			e, err := send(t, n, msg(t, c.payload))
			if err != nil {
				t.Fatalf("Receive: %v", err)
			}
			matched := e.total() > 0
			if matched != c.match {
				t.Errorf("matched = %v, want %v", matched, c.match)
			}
		})
	}
}

func TestSwitchNumericStringsAreNotCoercedAgainstEachOther(t *testing.T) {
	// "01" and "1" are both numeric-looking strings but are not the same
	// string. Coercing them would make a switch on a zero-padded device id
	// route two different devices down the same branch.
	svc := newTestServices()
	n := build(t, "switch", `{"property":"payload","checkall":"true","rules":[
        {"t":"eq","v":"1","vt":"str"}
    ]}`, svc)

	e, _ := send(t, n, msg(t, `{"payload":"01"}`))
	if e.total() != 0 {
		t.Error(`"01" matched a rule testing for the string "1"`)
	}
	e2, _ := send(t, n, msg(t, `{"payload":"1"}`))
	if e2.total() != 1 {
		t.Error(`"1" did not match a rule testing for the string "1"`)
	}
}

func TestSwitchOrdersTwoStringsLexically(t *testing.T) {
	// In JavaScript "10" < "9" is true, because two strings compare
	// lexically no matter how numeric they look. A flow routing on a
	// zero-padded device id or a version-like string depends on that, and
	// coercing to numbers would silently send it down the wrong branch.
	svc := newTestServices()
	n := build(t, "switch", `{"property":"payload","checkall":"true","rules":[
        {"t":"lt","v":"9","vt":"str"}
    ]}`, svc)

	if e, _ := send(t, n, msg(t, `{"payload":"10"}`)); e.total() != 1 {
		t.Error(`"10" < "9" should be true as a string comparison`)
	}

	// But a genuine number on either side still coerces, because edit dialogs
	// persist numeric fields as strings.
	numeric := build(t, "switch", `{"property":"payload","checkall":"true","rules":[
        {"t":"lt","v":"9","vt":"num"}
    ]}`, svc)
	if e, _ := send(t, numeric, msg(t, `{"payload":"10"}`)); e.total() != 0 {
		t.Error(`"10" < 9 should be false once one side is a real number`)
	}
}

func TestSwitchBetweenAcceptsReversedBounds(t *testing.T) {
	svc := newTestServices()
	n := build(t, "switch", `{"property":"payload","checkall":"true","rules":[
        {"t":"btwn","v":"20","vt":"num","v2":"10","v2t":"num"}
    ]}`, svc)
	e, _ := send(t, n, msg(t, `{"payload":15}`))
	if e.total() != 1 {
		t.Error("a between rule written high-to-low did not match a value inside it")
	}
}

func TestSwitchOnNestedProperty(t *testing.T) {
	svc := newTestServices()
	n := build(t, "switch", `{"property":"payload.reading.value","propertyType":"msg","checkall":"true","rules":[
        {"t":"gt","v":"100","vt":"num"}
    ]}`, svc)
	e, err := send(t, n, msg(t, `{"payload":{"reading":{"value":150}}}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if e.total() != 1 {
		t.Error("a rule on a nested property did not match")
	}
}

func TestSwitchRejectsJSONataRule(t *testing.T) {
	err := buildErr(t, "switch", `{"property":"payload","rules":[{"t":"jsonata_exp","v":"$x","vt":"str"}]}`, newTestServices())
	if err == nil {
		t.Fatal("a JSONata rule was accepted")
	}
}

func TestSwitchRejectsNoRules(t *testing.T) {
	if err := buildErr(t, "switch", `{"property":"payload","rules":[]}`, newTestServices()); err == nil {
		t.Error("a switch node with no rules was accepted")
	}
}

// ---------------------------------------------------------------------------
// range
// ---------------------------------------------------------------------------

func TestRangeScale(t *testing.T) {
	svc := newTestServices()
	// A 4-20mA loop mapped to 0-100%, which is the everyday case.
	n := build(t, "range", `{"minin":4,"maxin":20,"minout":0,"maxout":100,"action":"scale"}`, svc)

	cases := []struct{ in, want float64 }{
		{4, 0}, {12, 50}, {20, 100}, {8, 25},
	}
	for _, c := range cases {
		e, err := send(t, n, msg(t, `{"payload":`+ftoa(c.in)+`}`))
		if err != nil {
			t.Fatalf("in=%v: %v", c.in, err)
		}
		got := e.on(0)[0].Payload()
		if got != c.want {
			t.Errorf("in=%v -> %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRangeClampAndRoll(t *testing.T) {
	svc := newTestServices()

	clamp := build(t, "range", `{"minin":0,"maxin":10,"minout":0,"maxout":100,"action":"clamp"}`, svc)
	e, _ := send(t, clamp, msg(t, `{"payload":20}`))
	if got := e.on(0)[0].Payload(); got != 100.0 {
		t.Errorf("clamp above range = %v, want 100", got)
	}
	e2, _ := send(t, clamp, msg(t, `{"payload":-5}`))
	if got := e2.on(0)[0].Payload(); got != 0.0 {
		t.Errorf("clamp below range = %v, want 0", got)
	}

	roll := build(t, "range", `{"minin":0,"maxin":10,"minout":0,"maxout":100,"action":"roll"}`, svc)
	e3, _ := send(t, roll, msg(t, `{"payload":11}`))
	if got := e3.on(0)[0].Payload(); got != 10.0 {
		t.Errorf("roll past the top = %v, want 10", got)
	}
}

func TestRangeRound(t *testing.T) {
	svc := newTestServices()
	n := build(t, "range", `{"minin":0,"maxin":3,"minout":0,"maxout":10,"action":"scale","round":true}`, svc)
	e, _ := send(t, n, msg(t, `{"payload":1}`))
	if got := e.on(0)[0].Payload(); got != 3.0 {
		t.Errorf("rounded output = %v, want 3", got)
	}
}

func TestRangeRejectsEmptyInputRange(t *testing.T) {
	// Dividing by a zero span yields infinity and quietly poisons everything
	// downstream, so it is refused at build time.
	err := buildErr(t, "range", `{"minin":5,"maxin":5,"minout":0,"maxout":100}`, newTestServices())
	if err == nil {
		t.Fatal("an empty input range was accepted")
	}
	if !strings.Contains(err.Error(), "input range") {
		t.Errorf("error = %q", err)
	}
}

func TestRangeRejectsNonNumeric(t *testing.T) {
	svc := newTestServices()
	n := build(t, "range", `{"minin":0,"maxin":10,"minout":0,"maxout":100}`, svc)
	_, err := send(t, n, msg(t, `{"payload":"not a number"}`))
	if err == nil {
		t.Error("a non-numeric payload was silently accepted")
	}
}

// ftoa renders a float as JSON number text for building test messages.
func ftoa(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// ---------------------------------------------------------------------------
// filter (rbe)
// ---------------------------------------------------------------------------

func TestFilterBlocksUnchanged(t *testing.T) {
	svc := newTestServices()
	n := build(t, "rbe", `{"func":"rbe","property":"payload","septopics":false}`, svc)

	seq := []struct {
		payload string
		pass    bool
	}{
		{`{"payload":1}`, true},  // first value always passes in rbe mode
		{`{"payload":1}`, false}, // unchanged
		{`{"payload":2}`, true},
		{`{"payload":2}`, false},
		{`{"payload":1}`, true},
	}
	for i, s := range seq {
		e, err := send(t, n, msg(t, s.payload))
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		passed := e.total() > 0
		if passed != s.pass {
			t.Errorf("step %d (%s): passed = %v, want %v", i, s.payload, passed, s.pass)
		}
	}
}

func TestFilterIgnoresFirstValueInRbeiMode(t *testing.T) {
	svc := newTestServices()
	n := build(t, "rbe", `{"func":"rbei","property":"payload","septopics":false}`, svc)

	e, _ := send(t, n, msg(t, `{"payload":1}`))
	if e.total() != 0 {
		t.Error("rbei mode passed the first value; it should record it silently")
	}
	e2, _ := send(t, n, msg(t, `{"payload":2}`))
	if e2.total() != 1 {
		t.Error("rbei mode blocked a changed value")
	}
}

func TestFilterTracksTopicsSeparately(t *testing.T) {
	// Two sensors publishing on different topics must not mask each other.
	svc := newTestServices()
	n := build(t, "rbe", `{"func":"rbe","property":"payload","septopics":true}`, svc)

	if e, _ := send(t, n, msg(t, `{"topic":"a","payload":1}`)); e.total() != 1 {
		t.Error("first value on topic a was blocked")
	}
	if e, _ := send(t, n, msg(t, `{"topic":"b","payload":1}`)); e.total() != 1 {
		t.Error("first value on topic b was blocked by topic a's state")
	}
	if e, _ := send(t, n, msg(t, `{"topic":"a","payload":1}`)); e.total() != 0 {
		t.Error("repeated value on topic a was passed")
	}
}

func TestFilterDeadband(t *testing.T) {
	svc := newTestServices()
	n := build(t, "rbe", `{"func":"deadband","gap":"5","property":"payload","septopics":false}`, svc)

	seq := []struct {
		payload string
		pass    bool
	}{
		{`{"payload":100}`, true},  // first establishes the baseline
		{`{"payload":103}`, false}, // within the band
		{`{"payload":106}`, true},  // moved by more than 5
		{`{"payload":108}`, false},
		{`{"payload":112}`, true},
	}
	for i, s := range seq {
		e, err := send(t, n, msg(t, s.payload))
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if passed := e.total() > 0; passed != s.pass {
			t.Errorf("step %d (%s): passed = %v, want %v", i, s.payload, passed, s.pass)
		}
	}
}

func TestFilterDeadbandPercent(t *testing.T) {
	svc := newTestServices()
	n := build(t, "rbe", `{"func":"deadband","gap":"10%","property":"payload","septopics":false}`, svc)

	if e, _ := send(t, n, msg(t, `{"payload":100}`)); e.total() != 1 {
		t.Fatal("baseline was blocked")
	}
	if e, _ := send(t, n, msg(t, `{"payload":105}`)); e.total() != 0 {
		t.Error("5% change passed a 10% deadband")
	}
	if e, _ := send(t, n, msg(t, `{"payload":115}`)); e.total() != 1 {
		t.Error("15% change was blocked by a 10% deadband")
	}
}

func TestFilterRejectsUnimplementedModes(t *testing.T) {
	// Narrowband is not implemented. Accepting it and behaving like something
	// else would be worse than refusing.
	err := buildErr(t, "rbe", `{"func":"narrowband","gap":"5"}`, newTestServices())
	if err == nil {
		t.Fatal("narrowband mode was accepted")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error = %q, want it to say the mode is not implemented", err)
	}
}
