package nodes

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

// Integration tests against real infrastructure.
//
// Skipped unless the corresponding environment variable is set, so the normal
// suite stays hermetic and fast. These are meant to be run against the actual
// App Store apps on a cluster — monster-mq, influxdb-app, postgresql-app — where
// the things unit tests cannot check are checked: that the broker accepts our
// client id, that InfluxDB accepts our line protocol, that the PostgreSQL DSN
// and TLS mode are right.
//
// Run at ut3 with, for example:
//
//	EMBERWIRE_TEST_MQTT=monster-mq.tenant-fireball.svc.cluster.local:1883 \
//	EMBERWIRE_TEST_INFLUX_URL=http://influxdb-app.tenant-fireball.svc.cluster.local:8086 \
//	EMBERWIRE_TEST_INFLUX_ORG=fireball \
//	EMBERWIRE_TEST_INFLUX_BUCKET=emberwire-test \
//	EMBERWIRE_TEST_INFLUX_TOKEN=... \
//	go test ./internal/nodes/ -run Integration -v

func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skipf("set %s to run this test against real infrastructure", key)
	}
	return v
}

// credServices lets an integration test supply a credential the way the
// credential store would.
type credServices struct {
	*testServices
	configs map[string]node.Node
}

func (s *credServices) ConfigNode(id string) (node.Node, bool) {
	n, ok := s.configs[id]
	return n, ok
}

func TestIntegrationMQTTRoundTrip(t *testing.T) {
	addr := requireEnv(t, "EMBERWIRE_TEST_MQTT")

	host, port := splitHostPort(t, addr)
	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}

	broker := build(t, "mqtt-broker", `{
        "broker":"`+host+`","port":`+port+`,"cleansession":true,"keepalive":30
    }`, svc)
	svc.configs["brk"] = broker
	defer func() {
		if c, ok := broker.(interface {
			Close(context.Context, bool) error
		}); ok {
			_ = c.Close(context.Background(), false)
		}
	}()

	topic := "emberwire/test/" + engine.GenerateID()

	in := build(t, "mqtt in", `{"broker":"brk","topic":"`+topic+`","qos":"1","datatype":"auto"}`, svc)
	inEmitter := newTestEmitter()
	starter, ok := in.(node.Starter)
	if !ok {
		t.Fatal("mqtt in does not implement Starter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := starter.Start(ctx, inEmitter); err != nil {
		t.Fatalf("subscribing: %v", err)
	}

	// Give the subscription time to reach the broker before publishing.
	time.Sleep(500 * time.Millisecond)

	out := build(t, "mqtt out", `{"broker":"brk","topic":"`+topic+`","qos":"1"}`, svc)
	if _, err := send(t, out, msg(t, `{"payload":{"temp":21.5,"unit":"C"}}`)); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && inEmitter.total() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if inEmitter.total() == 0 {
		t.Fatal("published message never came back through the subscription")
	}

	got := inEmitter.on(0)[0]
	if got.Topic() != topic {
		t.Errorf("topic = %q, want %q", got.Topic(), topic)
	}
	// auto mode should have parsed the JSON body into an object.
	obj, isObj := got.Payload().(map[string]any)
	if !isObj {
		t.Fatalf("payload is %T, want a parsed object", got.Payload())
	}
	if obj["temp"] != 21.5 {
		t.Errorf("payload.temp = %#v, want 21.5", obj["temp"])
	}
}

func TestIntegrationInfluxWrite(t *testing.T) {
	url := requireEnv(t, "EMBERWIRE_TEST_INFLUX_URL")
	org := requireEnv(t, "EMBERWIRE_TEST_INFLUX_ORG")
	bucket := requireEnv(t, "EMBERWIRE_TEST_INFLUX_BUCKET")
	token := requireEnv(t, "EMBERWIRE_TEST_INFLUX_TOKEN")

	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	svc.creds["token"] = token

	cfg := build(t, "emberwire-influxdb", `{
        "url":"`+url+`","apiVersion":"2","org":"`+org+`","bucket":"`+bucket+`","precision":"ns"
    }`, svc)
	svc.configs["srv"] = cfg

	n := build(t, "influxdb out", `{
        "server":"srv",
        "measurement":"emberwire_test","measurementType":"str",
        "tags":[{"column":"line","value":"topic","valueType":"msg"}],
        "fields":[{"column":"value","value":"payload","valueType":"msg"}]
    }`, svc)

	// A tag value containing a space and a comma, because that is exactly the
	// case where bad escaping silently fragments a series.
	if _, err := send(t, n, msg(t, `{"payload":21.5,"topic":"press 01,west"}`)); err != nil {
		t.Fatalf("writing to InfluxDB: %v", err)
	}
}

func TestIntegrationPostgresInsert(t *testing.T) {
	host := requireEnv(t, "EMBERWIRE_TEST_PG_HOST")
	database := requireEnv(t, "EMBERWIRE_TEST_PG_DATABASE")
	user := requireEnv(t, "EMBERWIRE_TEST_PG_USER")
	password := requireEnv(t, "EMBERWIRE_TEST_PG_PASSWORD")

	svc := &credServices{testServices: newTestServices(), configs: map[string]node.Node{}}
	svc.creds["password"] = password

	cfg := build(t, "emberwire-postgres", `{
        "host":"`+host+`","port":5432,"database":"`+database+`",
        "user":"`+user+`","sslmode":"prefer","maxConns":2
    }`, svc)
	svc.configs["srv"] = cfg
	defer func() {
		if c, ok := cfg.(interface {
			Close(context.Context, bool) error
		}); ok {
			_ = c.Close(context.Background(), false)
		}
	}()

	// Create the table through the query mode of the node itself, which also
	// exercises that path.
	ddl := build(t, "postgres", `{
        "server":"srv","mode":"query",
        "sql":"CREATE TABLE IF NOT EXISTS emberwire_test (machine text, value double precision, recorded_at timestamptz default now())"
    }`, svc)
	if _, err := send(t, ddl, msg(t, `{}`)); err != nil {
		t.Fatalf("creating the test table: %v", err)
	}

	ins := build(t, "postgres", `{
        "server":"srv","mode":"insert","table":"emberwire_test",
        "columns":[
            {"column":"machine","value":"topic","valueType":"msg"},
            {"column":"value","value":"payload","valueType":"msg"}
        ]
    }`, svc)
	e, err := send(t, ins, msg(t, `{"payload":21.5,"topic":"press01"}`))
	if err != nil {
		t.Fatalf("inserting: %v", err)
	}
	if rc, _, _ := e.on(0)[0].Get("rowCount"); rc != 1.0 {
		t.Errorf("rowCount = %#v, want 1", rc)
	}

	q := build(t, "postgres", `{
        "server":"srv","mode":"query",
        "sql":"SELECT machine, value FROM emberwire_test WHERE machine = $1 ORDER BY recorded_at DESC LIMIT 1",
        "params":[{"value":"topic","valueType":"msg"}]
    }`, svc)
	qe, err := send(t, q, msg(t, `{"topic":"press01"}`))
	if err != nil {
		t.Fatalf("querying: %v", err)
	}
	rows, ok := qe.on(0)[0].Payload().([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("query returned %#v, want one row", qe.on(0)[0].Payload())
	}
	row := rows[0].(map[string]any)
	if row["machine"] != "press01" || row["value"] != 21.5 {
		t.Errorf("row = %#v", row)
	}

	cleanup := build(t, "postgres", `{
        "server":"srv","mode":"query","sql":"DROP TABLE emberwire_test"
    }`, svc)
	if _, err := send(t, cleanup, msg(t, `{}`)); err != nil {
		t.Logf("could not drop the test table: %v", err)
	}
}

func splitHostPort(t *testing.T, addr string) (host, port string) {
	t.Helper()
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:]
		}
	}
	return addr, "1883"
}
