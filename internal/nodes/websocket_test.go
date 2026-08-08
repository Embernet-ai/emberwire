package nodes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/embernet-ai/emberwire/internal/flowhttp"
	"github.com/embernet-ai/emberwire/internal/node"
)

// wsTestServices resolves one config node by id, which is all the websocket
// nodes ask of the runtime.
type wsTestServices struct {
	*testServices
	configs map[string]node.Node
}

func (s *wsTestServices) ConfigNode(id string) (node.Node, bool) {
	n, ok := s.configs[id]
	return n, ok
}

// wsListener stands up a listener config node behind a real HTTP server, and
// returns the services the In and Out nodes need plus the server's URL.
func wsListener(t *testing.T, whole bool) (*wsTestServices, string, context.CancelFunc) {
	t.Helper()
	router := withRoutes(t)

	cfg, err := jsonConfig(map[string]any{
		"path": "/ws/test", "wholemsg": boolString(whole),
	})
	if err != nil {
		t.Fatal(err)
	}
	listener := build(t, "websocket-listener", cfg, newTestServices())
	ctx, cancel := context.WithCancel(context.Background())
	if err := listener.(node.Starter).Start(ctx, newTestEmitter()); err != nil {
		cancel()
		t.Fatalf("starting the listener: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, params, ok := router.Match(r.Method, r.URL.Path)
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, flowhttp.WithRouteParams(r, params))
	}))
	t.Cleanup(srv.Close)

	svc := &wsTestServices{
		testServices: newTestServices(),
		configs:      map[string]node.Node{"listener-1": listener},
	}
	return svc, "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/test", cancel
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dialling %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return conn
}

func TestWebSocketInAndOut(t *testing.T) {
	svc, url, cancel := wsListener(t, false)
	defer cancel()

	in := build(t, "websocket in", `{"server":"listener-1"}`, svc)
	e := newTestEmitter()
	_, inCancel := startNode(t, in, e)
	defer inCancel()

	out := build(t, "websocket out", `{"server":"listener-1"}`, svc)

	conn := dialWS(t, url)
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()

	if err := conn.Write(ctx, websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, 10*time.Second, "the frame to reach the flow", func() bool { return e.total() == 1 })

	m := e.on(0)[0]
	if m.Payload() != "hello" {
		t.Fatalf("payload = %v", m.Payload())
	}
	// The session id is what lets the Out node reply to the connection the
	// message came from rather than broadcasting.
	sess, ok := m.Data["_session"].(map[string]any)
	if !ok || sess["type"] != "websocket" {
		t.Fatalf("msg._session = %#v", m.Data["_session"])
	}

	m.SetPayload("pong")
	if _, err := send(t, out, m); err != nil {
		t.Fatalf("reply: %v", err)
	}

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("reading the reply: %v", err)
	}
	if string(data) != "pong" {
		t.Fatalf("reply = %q", data)
	}
}

func TestWebSocketWholeMessageMode(t *testing.T) {
	svc, url, cancel := wsListener(t, true)
	defer cancel()

	in := build(t, "websocket in", `{"server":"listener-1"}`, svc)
	e := newTestEmitter()
	_, inCancel := startNode(t, in, e)
	defer inCancel()

	conn := dialWS(t, url)
	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()

	if err := conn.Write(ctx, websocket.MessageText,
		[]byte(`{"topic":"press-01","payload":4.2}`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, 10*time.Second, "the frame", func() bool { return e.total() == 1 })

	m := e.on(0)[0]
	if m.Topic() != "press-01" || m.Payload() != 4.2 {
		t.Fatalf("message = %#v", m.Data)
	}

	// A frame that is not a JSON object is refused rather than quietly
	// downgraded to a payload: every property the flow reads would otherwise
	// come back undefined.
	if err := conn.Write(ctx, websocket.MessageText, []byte("not json")); err != nil {
		t.Fatalf("write: %v", err)
	}
	waitFor(t, 10*time.Second, "the refusal", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.errs) > 0
	})
}

func TestWebSocketOutBroadcastsWithoutASession(t *testing.T) {
	svc, url, cancel := wsListener(t, false)
	defer cancel()

	in := build(t, "websocket in", `{"server":"listener-1"}`, svc)
	e := newTestEmitter()
	_, inCancel := startNode(t, in, e)
	defer inCancel()

	first := dialWS(t, url)
	second := dialWS(t, url)

	// Both connections have to be accepted before the broadcast, which the
	// listener does on its own goroutine.
	waitFor(t, 10*time.Second, "both connections", func() bool {
		ep, err := lookupEndpoint(svc, "listener-1")
		return err == nil && len(ep.sessions()) == 2
	})

	out := build(t, "websocket out", `{"server":"listener-1"}`, svc)
	if _, err := send(t, out, msg(t, `{"payload":"broadcast"}`)); err != nil {
		t.Fatalf("broadcast: %v", err)
	}

	ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	for i, conn := range []*websocket.Conn{first, second} {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		if string(data) != "broadcast" {
			t.Errorf("connection %d received %q", i, data)
		}
	}
}

func TestWebSocketOutReportsWhenNobodyIsConnected(t *testing.T) {
	svc, _, cancel := wsListener(t, false)
	defer cancel()

	out := build(t, "websocket out", `{"server":"listener-1"}`, svc)
	if _, err := send(t, out, msg(t, `{"payload":"nobody there"}`)); err == nil {
		t.Fatal("a send with no connections open was reported as success")
	}
}

func TestWebSocketNodesRefuseAmbiguousConfiguration(t *testing.T) {
	svc := &wsTestServices{testServices: newTestServices(), configs: map[string]node.Node{}}

	for _, typ := range []string{"websocket in", "websocket out"} {
		if err := buildErr(t, typ, `{}`, svc); err == nil {
			t.Errorf("%s built with no listener or client", typ)
		}
		if err := buildErr(t, typ, `{"server":"a","client":"b"}`, svc); err == nil {
			t.Errorf("%s built with both a listener and a client", typ)
		}
	}
}

func TestWebSocketClientRefusesABadURL(t *testing.T) {
	for _, cfg := range []string{`{}`, `{"path":"http://example.invalid/ws"}`} {
		if err := buildErr(t, "websocket-client", cfg, newTestServices()); err == nil {
			t.Errorf("config %s was accepted", cfg)
		}
	}
}

// The listener shares the flow route table, so it cannot shadow the editor or
// collide with an HTTP In node.
func TestWebSocketListenerSharesTheFlowRouteTable(t *testing.T) {
	router := withRoutes(t)

	httpIn := build(t, "http in", `{"url":"/shared","method":"get"}`, newTestServices())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := httpIn.(node.Starter).Start(ctx, newTestEmitter()); err != nil {
		t.Fatalf("starting the http in node: %v", err)
	}

	ws := build(t, "websocket-listener", `{"path":"/shared"}`, newTestServices())
	if err := ws.(node.Starter).Start(ctx, newTestEmitter()); err == nil {
		t.Fatal("a websocket listener claimed a path an http in node already served")
	}
	if router.Len() != 1 {
		t.Fatalf("the route table holds %d routes, want only the http in node's", router.Len())
	}
}
