package nodes

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

// InfluxDB.
//
// Written against the line protocol over HTTP rather than a client library.
// The protocol is small, stable, and identical across 1.8 and 2.x apart from
// the endpoint and the auth header — and pulling in a client would add weight
// to the binary to save about eighty lines.

func init() {
	registerInfluxConfig()
	registerInfluxOut()
}

// influxConfig holds the endpoint and credentials.
type influxConfig struct {
	writeURL string
	header   http.Header
	client   *http.Client
}

// InfluxTarget is how an action node reaches its config node.
type InfluxTarget interface {
	Write(ctx context.Context, body []byte) error
}

func registerInfluxConfig() {
	node.MustRegister(node.Descriptor{
		Type:     "emberwire-influxdb",
		Category: node.CategoryConfig,
		Color:    colorStorage,
		Icon:     "db",
		IsConfig: true,
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "Emberwire's own InfluxDB connection, targeting the App Store's influxdb-app.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "url", Kind: node.PropString, Label: "URL", Required: true,
				Placeholder: "http://influxdb-app.tenant-fireball.svc.cluster.local:8086"},
			{Name: "apiVersion", Kind: node.PropSelect, Label: "API version", Default: "2",
				Options: []node.Option{
					{Value: "2", Label: "2.x — org, bucket and token"},
					{Value: "1", Label: "1.8 — database, username and password"},
				}},
			{Name: "org", Kind: node.PropString, Label: "Organisation", Help: "2.x only."},
			{Name: "bucket", Kind: node.PropString, Label: "Bucket", Help: "2.x only."},
			{Name: "database", Kind: node.PropString, Label: "Database", Help: "1.8 only."},
			{Name: "username", Kind: node.PropString, Label: "Username", Help: "1.8 only."},
			{Name: "token", Kind: node.PropCredential, Label: "Token or password"},
			{Name: "precision", Kind: node.PropSelect, Label: "Timestamp precision", Default: "ns",
				Options: []node.Option{
					{Value: "ns", Label: "Nanoseconds"},
					{Value: "us", Label: "Microseconds"},
					{Value: "ms", Label: "Milliseconds"},
					{Value: "s", Label: "Seconds"},
				}},
			{Name: "timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 15},
		},
		Help: "Connection to an InfluxDB instance, version 1.8 or 2.x.",
	}, newInfluxConfig)
}

func newInfluxConfig(def *node.Definition) (node.Node, error) {
	base := strings.TrimRight(def.Node.PropString("url", ""), "/")
	if base == "" {
		return nil, fmt.Errorf("url is required")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}

	token, _ := def.Services.Credential("token")
	precision := def.Node.PropString("precision", "ns")
	timeout := time.Duration(def.Node.PropInt("timeout", 15)) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	c := &influxConfig{
		header: http.Header{},
		client: &http.Client{Timeout: timeout},
	}
	c.header.Set("Content-Type", "text/plain; charset=utf-8")

	switch def.Node.PropString("apiVersion", "2") {
	case "1":
		db := def.Node.PropString("database", "")
		if db == "" {
			return nil, fmt.Errorf("database is required for the 1.8 API")
		}
		q := url.Values{}
		q.Set("db", db)
		q.Set("precision", precision)
		if user := def.Node.PropString("username", ""); user != "" {
			q.Set("u", user)
			q.Set("p", token)
		}
		c.writeURL = base + "/write?" + q.Encode()

	default:
		org := def.Node.PropString("org", "")
		bucket := def.Node.PropString("bucket", "")
		if org == "" || bucket == "" {
			return nil, fmt.Errorf("org and bucket are required for the 2.x API")
		}
		if token == "" {
			return nil, fmt.Errorf("a token is required for the 2.x API")
		}
		q := url.Values{}
		q.Set("org", org)
		q.Set("bucket", bucket)
		q.Set("precision", precision)
		c.writeURL = base + "/api/v2/write?" + q.Encode()
		c.header.Set("Authorization", "Token "+token)
	}

	return c, nil
}

func (c *influxConfig) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

// Write posts a line-protocol body.
func (c *influxConfig) Write(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.writeURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header = c.header.Clone()

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("writing to InfluxDB: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// Drain so the connection can be reused rather than being torn down and
		// re-dialled for every batch.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil
	}

	// InfluxDB puts the actual reason in the body — which line, which field,
	// what type conflict. Losing it and reporting only a status code turns a
	// five-second fix into an afternoon.
	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("InfluxDB returned %s: %s", resp.Status, strings.TrimSpace(string(detail)))
}

// ---------------------------------------------------------------------------
// influxdb out
// ---------------------------------------------------------------------------

type influxOutNode struct {
	cfgID       string
	measurement TypedValue
	tags        []columnMapping
	fields      []columnMapping
	timestamp   TypedValue
	precision   string
	svc         node.Services
	target      InfluxTarget
}

func registerInfluxOut() {
	node.MustRegister(node.Descriptor{
		Type:         "influxdb out",
		Category:     node.CategoryStorage,
		Color:        colorStorage,
		Icon:         "db",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "influxdb",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatOnly,
			Notes: "Emberwire's own node. The type name matches the community " +
				"node-red-contrib-influxdb so an imported flow finds it, but the " +
				"configuration is not identical — check the fields after importing.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Server",
				ConfigType: "emberwire-influxdb", Required: true},
			{Name: "measurement", Kind: node.PropTypedInput, Label: "Measurement",
				TypeProp: "measurementType", Default: "readings", Required: true},
			{Name: "tags", Kind: node.PropList, Label: "Tags", Fields: []node.Prop{
				{Name: "column", Kind: node.PropString, Label: "Tag"},
				{Name: "value", Kind: node.PropTypedInput, Label: "Value", TypeProp: "valueType"},
			}},
			{Name: "fields", Kind: node.PropList, Label: "Fields", Fields: []node.Prop{
				{Name: "column", Kind: node.PropString, Label: "Field"},
				{Name: "value", Kind: node.PropTypedInput, Label: "Value", TypeProp: "valueType"},
			}},
			{Name: "timestamp", Kind: node.PropTypedInput, Label: "Timestamp",
				TypeProp: "timestampType",
				Help:     "Leave empty to let InfluxDB stamp it on arrival."},
		},
		Help: "Writes a measurement to InfluxDB. Tags are indexed and must be " +
			"strings; fields hold the values.",
	}, newInfluxOut)
}

func newInfluxOut(def *node.Definition) (node.Node, error) {
	n := &influxOutNode{
		cfgID:       def.Node.PropString("server", ""),
		measurement: ReadTypedValue(def.Node.Raw, "measurement", "measurementType", node.TypeStr),
		timestamp:   ReadTypedValue(def.Node.Raw, "timestamp", "timestampType", node.TypeStr),
		svc:         def.Services,
	}
	if n.cfgID == "" {
		return nil, fmt.Errorf("no server selected")
	}
	if n.measurement.Value == "" {
		return nil, fmt.Errorf("measurement is required")
	}

	n.tags = readMappings(def.Node.Raw, "tags")
	n.fields = readMappings(def.Node.Raw, "fields")
	if len(n.fields) == 0 {
		// A point with no fields is rejected by InfluxDB. Catching it here beats
		// discovering it as a 400 on every message.
		return nil, fmt.Errorf("at least one field is required; a point with only tags is not valid")
	}
	return n, nil
}

func readMappings(raw map[string]any, key string) []columnMapping {
	arr, _ := raw[key].([]any)
	out := make([]columnMapping, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		name := stringOr(m["column"], "")
		if name == "" {
			continue
		}
		out = append(out, columnMapping{
			Column: name,
			Value:  ReadTypedValue(m, "value", "valueType", node.TypeMsg),
		})
	}
	return out
}

func (n *influxOutNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	if n.target == nil {
		cfg, ok := n.svc.ConfigNode(n.cfgID)
		if !ok {
			return fmt.Errorf("server config node %s is not running", n.cfgID)
		}
		t, ok := cfg.(InfluxTarget)
		if !ok {
			return fmt.Errorf("config node %s is not an InfluxDB server", n.cfgID)
		}
		n.target = t
	}

	line, err := n.buildLine(m)
	if err != nil {
		return err
	}

	if err := n.target.Write(ctx, []byte(line)); err != nil {
		out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
		return err
	}

	out.Status(node.Status{Fill: "green", Shape: "dot", Text: "written"})
	out.Send(0, m)
	return nil
}

func (n *influxOutNode) buildLine(m *engine.Msg) (string, error) {
	ec := EvalContext{Msg: m, Services: n.svc}

	measurement, ok, err := n.measurement.Eval(ec)
	if err != nil {
		return "", fmt.Errorf("measurement: %w", err)
	}
	if !ok {
		return "", fmt.Errorf("measurement did not resolve")
	}

	var b strings.Builder
	b.WriteString(escapeMeasurement(fmt.Sprint(measurement)))

	for _, t := range n.tags {
		v, ok, err := t.Value.Eval(ec)
		if err != nil {
			return "", fmt.Errorf("tag %q: %w", t.Column, err)
		}
		// An absent tag is skipped. Writing an empty tag value is not the same
		// as omitting it — InfluxDB indexes on the tag set, and an empty string
		// creates a distinct series that fragments the data.
		if !ok || v == nil {
			continue
		}
		s := fmt.Sprint(v)
		if s == "" {
			continue
		}
		b.WriteByte(',')
		b.WriteString(escapeTag(t.Column))
		b.WriteByte('=')
		b.WriteString(escapeTag(s))
	}

	b.WriteByte(' ')
	wrote := 0
	for _, f := range n.fields {
		v, ok, err := f.Value.Eval(ec)
		if err != nil {
			return "", fmt.Errorf("field %q: %w", f.Column, err)
		}
		if !ok || v == nil {
			continue
		}
		if wrote > 0 {
			b.WriteByte(',')
		}
		b.WriteString(escapeTag(f.Column))
		b.WriteByte('=')
		b.WriteString(formatFieldValue(v))
		wrote++
	}
	if wrote == 0 {
		return "", fmt.Errorf("no field resolved to a value; InfluxDB rejects a point with no fields")
	}

	if n.timestamp.Value != "" {
		ts, ok, err := n.timestamp.Eval(ec)
		if err != nil {
			return "", fmt.Errorf("timestamp: %w", err)
		}
		if ok && ts != nil {
			f, isNum := asFloat(ts)
			if !isNum {
				return "", fmt.Errorf("timestamp is %T, which is not a number", ts)
			}
			b.WriteByte(' ')
			b.WriteString(strconv.FormatInt(int64(f), 10))
		}
	}

	return b.String(), nil
}

// Line-protocol escaping.
//
// Getting this wrong does not fail loudly — it writes a point under a mangled
// series name, or splits one series into two because a tag value contained a
// space. The rules differ per position, which is why there are three functions
// rather than one.

// escapeMeasurement escapes commas and spaces.
func escapeMeasurement(s string) string {
	return strings.NewReplacer(",", `\,`, " ", `\ `).Replace(s)
}

// escapeTag escapes commas, equals signs and spaces. Used for tag keys, tag
// values and field keys, which share the same rules.
func escapeTag(s string) string {
	return strings.NewReplacer(",", `\,`, "=", `\=`, " ", `\ `).Replace(s)
}

// escapeStringField escapes double quotes and backslashes inside a quoted
// string field value.
func escapeStringField(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// formatFieldValue renders a value in the field's type syntax.
//
// The distinction between an integer and a float matters: InfluxDB stores them
// as different types and refuses a later write that disagrees with the first
// one for the same field. Everything numeric is written as a float, because a
// sensor that reports 21 and then 21.5 must not fail on the second reading.
func formatFieldValue(v any) string {
	switch t := v.(type) {
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		return `"` + escapeStringField(t) + `"`
	case []byte:
		return `"` + escapeStringField(string(t)) + `"`
	case engine.ImmutableBytes:
		return `"` + escapeStringField(string(t)) + `"`
	default:
		if f, ok := asFloat(v); ok {
			return strconv.FormatFloat(f, 'f', -1, 64)
		}
		return `"` + escapeStringField(fmt.Sprint(v)) + `"`
	}
}
