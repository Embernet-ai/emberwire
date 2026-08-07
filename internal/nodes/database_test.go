package nodes

import (
	"context"
	"strings"
	"testing"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

// ---------------------------------------------------------------------------
// InfluxDB line protocol
// ---------------------------------------------------------------------------

// fakeInflux captures what would have gone over the wire.
type fakeInflux struct {
	writes []string
	err    error
}

func (f *fakeInflux) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }
func (f *fakeInflux) Write(_ context.Context, body []byte) error {
	if f.err != nil {
		return f.err
	}
	f.writes = append(f.writes, string(body))
	return nil
}

// servicesWithConfig returns Services that hand back a specific config node.
type servicesWithConfig struct {
	*testServices
	configs map[string]node.Node
}

func (s *servicesWithConfig) ConfigNode(id string) (node.Node, bool) {
	n, ok := s.configs[id]
	return n, ok
}

func influxHarness(t *testing.T, cfgJSON string) (node.Node, *fakeInflux, *servicesWithConfig) {
	t.Helper()
	fake := &fakeInflux{}
	svc := &servicesWithConfig{
		testServices: newTestServices(),
		configs:      map[string]node.Node{"srv": fake},
	}
	n := build(t, "influxdb out", cfgJSON, svc)
	return n, fake, svc
}

func TestInfluxLineProtocolBasics(t *testing.T) {
	n, fake, _ := influxHarness(t, `{
        "server":"srv",
        "measurement":"readings","measurementType":"str",
        "tags":[{"column":"line","value":"topic","valueType":"msg"}],
        "fields":[{"column":"value","value":"payload","valueType":"msg"}]
    }`)

	if _, err := send(t, n, msg(t, `{"payload":21.5,"topic":"press01"}`)); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(fake.writes) != 1 {
		t.Fatalf("wrote %d bodies, want 1", len(fake.writes))
	}
	want := "readings,line=press01 value=21.5"
	if fake.writes[0] != want {
		t.Errorf("line = %q, want %q", fake.writes[0], want)
	}
}

func TestInfluxEscaping(t *testing.T) {
	// Getting escaping wrong does not fail loudly. It writes a point under a
	// mangled series name, or splits one series into two because a tag value
	// contained a space — and you find out weeks later when a dashboard is
	// missing half its data.
	n, fake, _ := influxHarness(t, `{
        "server":"srv",
        "measurement":"press readings,raw","measurementType":"str",
        "tags":[{"column":"machine name","value":"topic","valueType":"msg"}],
        "fields":[{"column":"note","value":"payload","valueType":"msg"}]
    }`)

	if _, err := send(t, n, msg(t, `{"payload":"has \"quotes\" and \\ backslash","topic":"line 1,west"}`)); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got := fake.writes[0]

	for _, want := range []string{
		`press\ readings\,raw`,                   // measurement: comma and space
		`machine\ name=line\ 1\,west`,            // tag key and value
		`note="has \"quotes\" and \\ backslash"`, // string field: quotes and backslash
	} {
		if !strings.Contains(got, want) {
			t.Errorf("line protocol is missing %s\ngot: %s", want, got)
		}
	}
}

func TestInfluxFieldTypes(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"float", `{"payload":21.5}`, "value=21.5"},
		// Integers are written as floats deliberately: InfluxDB stores int and
		// float as different types and rejects a later write that disagrees, so
		// a sensor reporting 21 then 21.5 would fail on the second reading.
		{"whole number stays a float", `{"payload":21}`, "value=21"},
		{"bool true", `{"payload":true}`, "value=true"},
		{"bool false", `{"payload":false}`, "value=false"},
		{"string", `{"payload":"running"}`, `value="running"`},
		{"negative", `{"payload":-40.5}`, "value=-40.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, fake, _ := influxHarness(t, `{
                "server":"srv","measurement":"m","measurementType":"str",
                "fields":[{"column":"value","value":"payload","valueType":"msg"}]
            }`)
			if _, err := send(t, n, msg(t, c.payload)); err != nil {
				t.Fatalf("Receive: %v", err)
			}
			if !strings.Contains(fake.writes[0], c.want) {
				t.Errorf("line = %q, want it to contain %q", fake.writes[0], c.want)
			}
		})
	}
}

func TestInfluxSkipsEmptyTags(t *testing.T) {
	// Writing an empty tag value is not the same as omitting it. InfluxDB
	// indexes on the tag set, so an empty string creates a distinct series and
	// fragments the data for that machine.
	n, fake, _ := influxHarness(t, `{
        "server":"srv","measurement":"m","measurementType":"str",
        "tags":[
            {"column":"line","value":"topic","valueType":"msg"},
            {"column":"missing","value":"nope","valueType":"msg"}
        ],
        "fields":[{"column":"value","value":"payload","valueType":"msg"}]
    }`)

	if _, err := send(t, n, msg(t, `{"payload":1,"topic":"a"}`)); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	got := fake.writes[0]
	if strings.Contains(got, "missing=") {
		t.Errorf("an unresolved tag was written as an empty value: %s", got)
	}
	if !strings.Contains(got, "line=a") {
		t.Errorf("the resolved tag is missing: %s", got)
	}
}

func TestInfluxRequiresAField(t *testing.T) {
	// A point with only tags is rejected by InfluxDB. Catching it here beats a
	// 400 on every single message.
	svc := &servicesWithConfig{testServices: newTestServices(), configs: map[string]node.Node{}}
	err := buildErr(t, "influxdb out", `{
        "server":"srv","measurement":"m","measurementType":"str",
        "tags":[{"column":"line","value":"topic","valueType":"msg"}]
    }`, svc)
	if err == nil {
		t.Fatal("a node with no fields was accepted")
	}
	if !strings.Contains(err.Error(), "field") {
		t.Errorf("error = %q", err)
	}
}

func TestInfluxErrorsWhenNoFieldResolves(t *testing.T) {
	n, _, _ := influxHarness(t, `{
        "server":"srv","measurement":"m","measurementType":"str",
        "fields":[{"column":"value","value":"nope","valueType":"msg"}]
    }`)
	if _, err := send(t, n, msg(t, `{"payload":1}`)); err == nil {
		t.Error("a point with no resolved fields was written; InfluxDB would reject it")
	}
}

func TestInfluxTimestamp(t *testing.T) {
	n, fake, _ := influxHarness(t, `{
        "server":"srv","measurement":"m","measurementType":"str",
        "fields":[{"column":"value","value":"payload","valueType":"msg"}],
        "timestamp":"ts","timestampType":"msg"
    }`)
	if _, err := send(t, n, msg(t, `{"payload":1,"ts":1700000000000000000}`)); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if !strings.HasSuffix(fake.writes[0], " 1700000000000000000") {
		t.Errorf("line = %q, want it to end with the timestamp", fake.writes[0])
	}
}

func TestInfluxSurfacesServerError(t *testing.T) {
	// InfluxDB puts the actual reason in the body — which line, which field,
	// what type conflict. Losing it turns a five-second fix into an afternoon.
	fake := &fakeInflux{err: errInflux("InfluxDB returned 400 Bad Request: field type conflict")}
	svc := &servicesWithConfig{
		testServices: newTestServices(),
		configs:      map[string]node.Node{"srv": fake},
	}
	n := build(t, "influxdb out", `{
        "server":"srv","measurement":"m","measurementType":"str",
        "fields":[{"column":"value","value":"payload","valueType":"msg"}]
    }`, svc)

	_, err := send(t, n, msg(t, `{"payload":1}`))
	if err == nil {
		t.Fatal("a write failure was swallowed")
	}
	if !strings.Contains(err.Error(), "field type conflict") {
		t.Errorf("error = %q, want the server's explanation preserved", err)
	}
}

type errInflux string

func (e errInflux) Error() string { return string(e) }

// ---------------------------------------------------------------------------
// PostgreSQL
// ---------------------------------------------------------------------------

func TestValidateSQLIdentifier(t *testing.T) {
	// Identifiers cannot be parameterised, so they are concatenated into the
	// statement and therefore have to be validated. They come from an edit
	// dialog reachable by anyone who can deploy a flow.
	valid := []string{"readings", "public.readings", "line_1", "t2", "_private"}
	for _, s := range valid {
		if err := validateSQLIdentifier(s); err != nil {
			t.Errorf("validateSQLIdentifier(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"",
		"readings; DROP TABLE users",
		"readings--",
		`readings"`,
		"readings'",
		"read ings",
		"public..readings",
		".readings",
		"readings.",
		"1readings",
		strings.Repeat("a", 129),
	}
	for _, s := range invalid {
		if err := validateSQLIdentifier(s); err == nil {
			t.Errorf("validateSQLIdentifier(%q) = nil, want an error", s)
		}
	}
}

func TestPostgresRejectsInjectionInIdentifiers(t *testing.T) {
	svc := &servicesWithConfig{testServices: newTestServices(), configs: map[string]node.Node{}}

	err := buildErr(t, "postgres", `{
        "server":"srv","mode":"insert","table":"readings; DROP TABLE users",
        "columns":[{"column":"v","value":"payload","valueType":"msg"}]
    }`, svc)
	if err == nil {
		t.Error("a table name containing SQL was accepted")
	}

	err = buildErr(t, "postgres", `{
        "server":"srv","mode":"insert","table":"readings",
        "columns":[{"column":"v) VALUES (1); --","value":"payload","valueType":"msg"}]
    }`, svc)
	if err == nil {
		t.Error("a column name containing SQL was accepted")
	}
}

func TestPostgresBuildInsert(t *testing.T) {
	// Values must always be parameters, never concatenated.
	n := &postgresNode{
		table: "public.readings",
		columns: []columnMapping{
			{Column: "machine"}, {Column: "value"}, {Column: "recorded_at"},
		},
	}

	sql, args := n.buildInsert([][]any{
		{"press01", 21.5, "2026-08-07T12:00:00Z"},
	})
	want := "INSERT INTO public.readings (machine, value, recorded_at) VALUES ($1, $2, $3)"
	if sql != want {
		t.Errorf("sql = %q\nwant %q", sql, want)
	}
	if len(args) != 3 || args[0] != "press01" || args[1] != 21.5 {
		t.Errorf("args = %#v", args)
	}

	// A multi-row insert numbers parameters continuously across rows.
	sql2, args2 := n.buildInsert([][]any{
		{"a", 1.0, "t1"},
		{"b", 2.0, "t2"},
	})
	if !strings.Contains(sql2, "($1, $2, $3), ($4, $5, $6)") {
		t.Errorf("multi-row sql = %q", sql2)
	}
	if len(args2) != 6 {
		t.Errorf("multi-row args = %d, want 6", len(args2))
	}
}

func TestPostgresOnConflictNothing(t *testing.T) {
	n := &postgresNode{
		table:   "readings",
		columns: []columnMapping{{Column: "v"}},
		onConfl: "nothing",
	}
	sql, _ := n.buildInsert([][]any{{1.0}})
	if !strings.HasSuffix(sql, "ON CONFLICT DO NOTHING") {
		t.Errorf("sql = %q, want the conflict clause", sql)
	}
}

func TestPostgresRequiresConfiguration(t *testing.T) {
	svc := &servicesWithConfig{testServices: newTestServices(), configs: map[string]node.Node{}}

	cases := []struct{ name, cfg string }{
		{"no server", `{"mode":"insert","table":"t","columns":[{"column":"c"}]}`},
		{"insert with no table", `{"server":"srv","mode":"insert","columns":[{"column":"c"}]}`},
		{"insert with no columns", `{"server":"srv","mode":"insert","table":"t"}`},
		{"query with no sql", `{"server":"srv","mode":"query"}`},
		{"unknown mode", `{"server":"srv","mode":"upsert","table":"t"}`},
	}
	for _, c := range cases {
		if err := buildErr(t, "postgres", c.cfg, svc); err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

func TestQuotePGKeyword(t *testing.T) {
	// The connection string is keyword/value rather than a URL precisely so a
	// password containing URL-significant characters does not need escaping —
	// but spaces, quotes and backslashes still do.
	cases := []struct{ in, want string }{
		{"simple", "simple"},
		{"", "''"},
		{"has space", "'has space'"},
		{`has'quote`, `'has\'quote'`},
		{`has\backslash`, `'has\\backslash'`},
	}
	for _, c := range cases {
		if got := quotePGKeyword(c.in); got != c.want {
			t.Errorf("quotePGKeyword(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalisePGRow(t *testing.T) {
	// Without this a timestamp arrives as a time.Time that a Switch node cannot
	// compare and a debug pane renders opaquely.
	row := map[string]any{
		"id":    int64(7),
		"name":  []byte("press01"),
		"value": float32(21.5),
	}
	got := normalisePGRow(row)

	if got["id"] != 7.0 {
		t.Errorf("id = %#v (%T), want float64 7", got["id"], got["id"])
	}
	if got["name"] != "press01" {
		t.Errorf("name = %#v, want a string", got["name"])
	}
	if f, ok := got["value"].(float64); !ok || f != 21.5 {
		t.Errorf("value = %#v (%T), want float64 21.5", got["value"], got["value"])
	}
}
