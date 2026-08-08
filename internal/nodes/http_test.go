package nodes

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/flowhttp"
)

// withRoutes gives the test its own route table and restores the previous one.
func withRoutes(t *testing.T) *flowhttp.Router {
	t.Helper()
	r := flowhttp.NewRouter("/", []string{"/flows"})
	prev := Routes
	Routes = r
	t.Cleanup(func() { Routes = prev })
	return r
}

// serveThrough dispatches a request into whichever HTTP In node claimed the
// path, the way the API server does.
func serveThrough(r *flowhttp.Router, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h, params, ok := r.Match(req.Method, req.URL.Path)
	if !ok {
		rec.WriteHeader(http.StatusNotFound)
		return rec
	}
	h.ServeHTTP(rec, flowhttp.WithRouteParams(req, params))
	return rec
}

// ---------------------------------------------------------------------------
// http in and http response
// ---------------------------------------------------------------------------

func TestHTTPInAndResponse(t *testing.T) {
	router := withRoutes(t)

	in := build(t, "http in", `{"url":"/readings/:line","method":"get"}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, in, e)
	defer cancel()

	resp := build(t, "http response", `{"statusCode":201}`, newTestServices())

	// The flow: whatever the In node emits goes to the Response node. Done on a
	// goroutine because the In node's handler blocks until the reply lands,
	// which is exactly what keeps net/http from recycling the writer.
	go func() {
		waitForCount(e, 1, 5*time.Second)
		for _, m := range e.on(0) {
			// Replying with a clone, so the assertions below see the message the
			// node actually emitted rather than whatever the flow did to it.
			_ = resp.Receive(t.Context(), m.Clone(), newTestEmitter())
		}
	}()

	rec := serveThrough(router, httptest.NewRequest("GET", "/readings/3?unit=bar", nil))

	if rec.Code != 201 {
		t.Fatalf("status = %d, want the node's 201: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON for an object payload", ct)
	}

	m := e.on(0)[0]
	req, ok := m.Data["req"].(map[string]any)
	if !ok {
		t.Fatalf("msg.req is %T", m.Data["req"])
	}
	params, _ := req["params"].(map[string]any)
	if params["line"] != "3" {
		t.Errorf("msg.req.params = %#v, want the captured :line", params)
	}
	query, _ := req["query"].(map[string]any)
	if query["unit"] != "bar" {
		t.Errorf("msg.req.query = %#v", query)
	}
	// A GET with no body carries its query as the payload, as Node-RED's does.
	queryPayload, ok := m.Payload().(map[string]any)
	if !ok || queryPayload["unit"] != "bar" {
		t.Errorf("payload = %#v, want the query object", m.Payload())
	}
}

func TestHTTPInDecodesBodies(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		check       func(t *testing.T, payload any)
	}{
		{
			name:        "json",
			contentType: "application/json",
			body:        `{"reading":4.2}`,
			check: func(t *testing.T, payload any) {
				obj, ok := payload.(map[string]any)
				if !ok || obj["reading"] != 4.2 {
					t.Fatalf("payload = %#v", payload)
				}
			},
		},
		{
			name:        "form encoded",
			contentType: "application/x-www-form-urlencoded",
			body:        "tag=press-01&value=4.2",
			check: func(t *testing.T, payload any) {
				obj, ok := payload.(map[string]any)
				if !ok || obj["tag"] != "press-01" {
					t.Fatalf("payload = %#v", payload)
				}
			},
		},
		{
			name:        "anything else arrives as bytes",
			contentType: "application/octet-stream",
			body:        "\x01\x02",
			check: func(t *testing.T, payload any) {
				if _, ok := payload.(engine.ImmutableBytes); !ok {
					t.Fatalf("payload is %T, want ImmutableBytes", payload)
				}
			},
		},
		{
			name:        "a body that claims JSON and is not stays as text",
			contentType: "application/json",
			body:        "not json",
			check: func(t *testing.T, payload any) {
				if payload != "not json" {
					t.Fatalf("payload = %#v; the flow has to be able to see what arrived", payload)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := withRoutes(t)
			in := build(t, "http in", `{"url":"/post","method":"post"}`, newTestServices())
			e := newTestEmitter()
			_, cancel := startNode(t, in, e)
			defer cancel()

			resp := build(t, "http response", `{}`, newTestServices())
			go func() {
				waitForCount(e, 1, 5*time.Second)
				for _, m := range e.on(0) {
					_ = resp.Receive(t.Context(), m, newTestEmitter())
				}
			}()

			req := httptest.NewRequest("POST", "/post", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			if rec := serveThrough(router, req); rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
			tc.check(t, e.on(0)[0].Payload())
		})
	}
}

// Node-RED holds a request open forever when no Response node runs, so a flow
// with the wiring missing leaks a connection per request until the process runs
// out of sockets and stops answering with nothing in the log.
func TestHTTPInClosesOutARequestNobodyAnswers(t *testing.T) {
	router := withRoutes(t)
	in := build(t, "http in", `{"url":"/orphan","method":"get","ew_timeout":0.2}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, in, e)
	defer cancel()

	start := time.Now()
	rec := serveThrough(router, httptest.NewRequest("GET", "/orphan", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the request was held for %s", elapsed)
	}
	// And the flow is told, so the wiring mistake is findable.
	e.mu.Lock()
	errs := len(e.errs)
	e.mu.Unlock()
	if errs == 0 {
		t.Error("the timeout was not raised to the flow")
	}
}

func TestHTTPResponseRefusesAMessageWithNoRequest(t *testing.T) {
	withRoutes(t)
	resp := build(t, "http response", `{}`, newTestServices())
	if _, err := send(t, resp, msg(t, `{"payload":"x"}`)); err == nil {
		t.Fatal("a message with no msg.res was accepted")
	}
}

// Node-RED throws the second reply away silently, so half the response the flow
// believes it sent never existed.
func TestHTTPResponseRefusesASecondReply(t *testing.T) {
	router := withRoutes(t)
	in := build(t, "http in", `{"url":"/twice","method":"get"}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, in, e)
	defer cancel()

	resp := build(t, "http response", `{}`, newTestServices())
	second := make(chan error, 1)
	go func() {
		waitForCount(e, 1, 5*time.Second)
		m := e.on(0)[0]
		_ = resp.Receive(t.Context(), m.Clone(), newTestEmitter())
		second <- resp.Receive(t.Context(), m.Clone(), newTestEmitter())
	}()

	serveThrough(router, httptest.NewRequest("GET", "/twice", nil))

	select {
	case err := <-second:
		if err == nil {
			t.Fatal("a second reply to the same request was accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second reply never returned")
	}
}

func TestHTTPResponseHeadersFromTheNodeAndTheMessage(t *testing.T) {
	router := withRoutes(t)
	in := build(t, "http in", `{"url":"/headers","method":"get"}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, in, e)
	defer cancel()

	cfg, err := jsonConfig(map[string]any{
		"headers": []any{map[string]any{"key": "X-Line", "value": "3"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := build(t, "http response", cfg, newTestServices())

	go func() {
		waitForCount(e, 1, 5*time.Second)
		m := e.on(0)[0]
		m.Data["headers"] = map[string]any{"X-Site": "ut3"}
		m.Data["statusCode"] = float64(418)
		m.SetPayload("teapot")
		_ = resp.Receive(t.Context(), m, newTestEmitter())
	}()

	rec := serveThrough(router, httptest.NewRequest("GET", "/headers", nil))
	if rec.Header().Get("X-Line") != "3" {
		t.Errorf("the node's header is missing: %v", rec.Header())
	}
	if rec.Header().Get("X-Site") != "ut3" {
		t.Errorf("the message's header is missing: %v", rec.Header())
	}
	// The node's own status code is empty here, so msg.statusCode wins.
	if rec.Code != 418 {
		t.Errorf("status = %d, want msg.statusCode", rec.Code)
	}
	if rec.Body.String() != "teapot" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// ---------------------------------------------------------------------------
// http request
// ---------------------------------------------------------------------------

func TestHTTPRequestRoundTrip(t *testing.T) {
	var gotBody, gotAuth, gotHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotAuth = r.Header.Get("Authorization")
		gotHeader = r.Header.Get("X-Line")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	svc := newTestServices()
	svc.creds["password"] = "hunter2"
	cfg, err := jsonConfig(map[string]any{
		"url": upstream.URL + "/ingest", "method": "POST", "ret": "obj",
		"authType": "basic", "user": "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "http request", cfg, svc)

	m := msg(t, `{"payload":{"reading":4.2}}`)
	m.Data["headers"] = map[string]any{"X-Line": "3"}
	e, err := send(t, n, m)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	if gotBody != `{"reading":4.2}` {
		t.Errorf("upstream saw body %q", gotBody)
	}
	if gotAuth == "" {
		t.Error("basic auth was not sent")
	}
	if gotHeader != "3" {
		t.Errorf("msg.headers was not applied: %q", gotHeader)
	}

	out := e.on(0)[0]
	obj, ok := out.Payload().(map[string]any)
	if !ok || obj["ok"] != true {
		t.Fatalf("payload = %#v, want the parsed JSON", out.Payload())
	}
	if out.Data["statusCode"] != float64(202) {
		t.Errorf("msg.statusCode = %#v", out.Data["statusCode"])
	}
	headers, _ := out.Data["headers"].(map[string]any)
	if !strings.Contains(mustacheString(headers["content-type"]), "json") {
		t.Errorf("msg.headers = %#v; header names must be lower-cased", headers)
	}
}

// A non-2xx is data, not an error. A flow polling an endpoint that 404s while a
// device boots wants a Switch node, not a Catch.
func TestHTTPRequestReportsStatusRatherThanErroring(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer upstream.Close()

	cfg, err := jsonConfig(map[string]any{"url": upstream.URL, "method": "GET"})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "http request", cfg, newTestServices())

	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("a 404 was raised as an error: %v", err)
	}
	if e.on(0)[0].Data["statusCode"] != float64(404) {
		t.Fatalf("statusCode = %#v", e.on(0)[0].Data["statusCode"])
	}
}

func TestHTTPRequestFillsTheURLFromTheMessage(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	t.Run("mustache in the configured URL", func(t *testing.T) {
		cfg, err := jsonConfig(map[string]any{"url": upstream.URL + "/device/{{payload.id}}"})
		if err != nil {
			t.Fatal(err)
		}
		n := build(t, "http request", cfg, newTestServices())
		if _, err := send(t, n, msg(t, `{"payload":{"id":"press-01"}}`)); err != nil {
			t.Fatalf("request: %v", err)
		}
		if gotPath != "/device/press-01" {
			t.Fatalf("upstream saw %q", gotPath)
		}
	})

	t.Run("msg.url when the node has none", func(t *testing.T) {
		n := build(t, "http request", `{"method":"GET"}`, newTestServices())
		m, err := jsonConfig(map[string]any{"url": upstream.URL + "/from-msg"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := send(t, n, msg(t, m)); err != nil {
			t.Fatalf("request: %v", err)
		}
		if gotPath != "/from-msg" {
			t.Fatalf("upstream saw %q", gotPath)
		}
	})
}

// Node-RED reads a response without a limit, so one oversized reply takes the
// pod with it.
func TestHTTPRequestCapsTheResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer upstream.Close()

	cfg, err := jsonConfig(map[string]any{"url": upstream.URL, "ew_maxBody": 1024})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "http request", cfg, newTestServices())

	if _, err := send(t, n, msg(t, `{}`)); err == nil {
		t.Fatal("an oversized response was accepted")
	}
}

func TestHTTPRequestRefusesBadConfiguration(t *testing.T) {
	cases := map[string]string{
		"unknown return type": `{"url":"http://example.invalid","ret":"yaml"}`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := buildErr(t, "http request", cfg, newTestServices()); err == nil {
				t.Fatal("expected the node to refuse to build")
			}
		})
	}

	t.Run("a non-http scheme", func(t *testing.T) {
		n := build(t, "http request", `{"url":"ftp://example.invalid/x"}`, newTestServices())
		if _, err := send(t, n, msg(t, `{}`)); err == nil {
			t.Fatal("an ftp URL was accepted")
		}
	})

	t.Run("no URL anywhere", func(t *testing.T) {
		n := build(t, "http request", `{}`, newTestServices())
		if _, err := send(t, n, msg(t, `{}`)); err == nil {
			t.Fatal("a request with no URL was accepted")
		}
	})
}

// waitForCount blocks until the emitter has seen n messages.
func waitForCount(e *testEmitter, n int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if e.total() >= n {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
