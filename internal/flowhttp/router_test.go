package flowhttp

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func handlerNamed(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(name))
	})
}

// served runs whichever handler matched and returns what it wrote.
func served(t *testing.T, r *Router, method, path string) (string, map[string]string, bool) {
	t.Helper()
	h, params, ok := r.Match(method, path)
	if !ok {
		return "", nil, false
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec.Body.String(), params, true
}

func mustRegister(t *testing.T, r *Router, nodeID, method, pattern string) func() {
	t.Helper()
	un, err := r.Register(nodeID, method, pattern, handlerNamed(nodeID))
	if err != nil {
		t.Fatalf("Register(%s %s): %v", method, pattern, err)
	}
	return un
}

func TestRouterMatchesLiteralsAndParameters(t *testing.T) {
	r := NewRouter("/", nil)
	mustRegister(t, r, "readings", "GET", "/readings")
	mustRegister(t, r, "byline", "GET", "/readings/:line")
	mustRegister(t, r, "deep", "GET", "/files/*")

	cases := []struct {
		path   string
		want   string
		params map[string]string
	}{
		{"/readings", "readings", nil},
		{"/readings/3", "byline", map[string]string{"line": "3"}},
		{"/files/a/b/c.csv", "deep", map[string]string{"*": "a/b/c.csv"}},
	}
	for _, tc := range cases {
		got, params, ok := served(t, r, "GET", tc.path)
		if !ok {
			t.Errorf("%s did not match anything", tc.path)
			continue
		}
		if got != tc.want {
			t.Errorf("%s reached %s, want %s", tc.path, got, tc.want)
		}
		if tc.params != nil && !reflect.DeepEqual(params, tc.params) {
			t.Errorf("%s captured %v, want %v", tc.path, params, tc.params)
		}
	}

	if _, _, ok := served(t, r, "GET", "/readings/3/extra"); ok {
		t.Error("a single parameter matched two path segments")
	}
	if _, _, ok := served(t, r, "POST", "/readings"); ok {
		t.Error("a GET route answered a POST")
	}
}

// Without specificity ordering, which of two routes a request reaches would
// depend on the order the nodes happened to start in.
func TestRouterPrefersTheMostSpecificRoute(t *testing.T) {
	r := NewRouter("/", nil)
	// Registered least-specific first, deliberately.
	mustRegister(t, r, "wild", "GET", "/api/*")
	mustRegister(t, r, "param", "GET", "/api/:id")
	mustRegister(t, r, "literal", "GET", "/api/health")

	for path, want := range map[string]string{
		"/api/health": "literal",
		"/api/7":      "param",
		"/api/a/b":    "wild",
		// A trailing slash is the same path. Treating it as a separate one is
		// how a flow ends up with an endpoint that works from a browser and 404s
		// from curl.
		"/api/health/": "literal",
	} {
		got, _, ok := served(t, r, "GET", path)
		if !ok {
			t.Errorf("%s did not match", path)
			continue
		}
		if got != want {
			t.Errorf("%s reached %s, want %s", path, got, want)
		}
	}
}

func TestRouterMethodAnyLosesToAnExplicitMethod(t *testing.T) {
	r := NewRouter("/", nil)
	mustRegister(t, r, "any", MethodAny, "/hook")
	mustRegister(t, r, "post", "POST", "/hook")

	if got, _, _ := served(t, r, "POST", "/hook"); got != "post" {
		t.Errorf("POST reached %s, want the explicit route", got)
	}
	if got, _, _ := served(t, r, "PUT", "/hook"); got != "any" {
		t.Errorf("PUT reached %s, want the any-method route", got)
	}
}

// Express runs whichever route matched first and the second node never fires,
// with nothing said about it.
func TestRouterRefusesADuplicateRoute(t *testing.T) {
	r := NewRouter("/", nil)
	mustRegister(t, r, "first", "GET", "/readings")

	_, err := r.Register("second", "GET", "/readings", handlerNamed("second"))
	if err == nil {
		t.Fatal("a second node claimed the same method and path")
	}
	if !strings.Contains(err.Error(), "first") {
		t.Errorf("error %q does not name the node already serving it", err)
	}

	// A different method on the same path is fine.
	if _, err := r.Register("third", "POST", "/readings", handlerNamed("third")); err != nil {
		t.Fatalf("a different method on the same path was refused: %v", err)
	}
}

// A flow that shadows the editor or the admin API makes them unreachable, and
// the symptom is an editor that stops loading with no explanation.
func TestRouterRefusesReservedPaths(t *testing.T) {
	r := NewRouter("/", []string{"/flows", "/auth", "/metrics"})

	for _, p := range []string{"/", "/flows", "/flows/anything", "/auth/token", "/metrics"} {
		if _, err := r.Register("greedy", "GET", p, handlerNamed("greedy")); err == nil {
			t.Errorf("a flow claimed the reserved path %s", p)
		}
	}
	// A path that merely starts with the same letters is not reserved.
	if _, err := r.Register("ok", "GET", "/flowsheet", handlerNamed("ok")); err != nil {
		t.Errorf("/flowsheet was refused: %v", err)
	}
}

func TestRouterAppliesItsRoot(t *testing.T) {
	r := NewRouter("/api", nil)
	mustRegister(t, r, "readings", "GET", "/readings")

	if _, _, ok := served(t, r, "GET", "/readings"); ok {
		t.Error("a route matched without the configured root prefix")
	}
	if got, _, ok := served(t, r, "GET", "/api/readings"); !ok || got != "readings" {
		t.Errorf("/api/readings reached %q, matched=%v", got, ok)
	}
}

func TestRouterUnregisterAndReset(t *testing.T) {
	r := NewRouter("/", nil)
	un := mustRegister(t, r, "a", "GET", "/a")
	mustRegister(t, r, "b", "GET", "/b")

	un()
	if _, _, ok := served(t, r, "GET", "/a"); ok {
		t.Error("an unregistered route still matched")
	}
	if _, _, ok := served(t, r, "GET", "/b"); !ok {
		t.Error("unregistering one route removed another")
	}
	// The path is free again, which is what makes a redeploy work.
	if _, err := r.Register("a2", "GET", "/a", handlerNamed("a2")); err != nil {
		t.Fatalf("re-registering a released path: %v", err)
	}

	r.Reset()
	if r.Len() != 0 {
		t.Fatalf("Len = %d after Reset", r.Len())
	}
}

func TestRouterRefusesMalformedPatterns(t *testing.T) {
	r := NewRouter("/", nil)
	for _, p := range []string{"", "   ", "/a/*/b", "/a/:"} {
		if _, err := r.Register("bad", "GET", p, handlerNamed("bad")); err == nil {
			t.Errorf("pattern %q was accepted", p)
		}
	}
}

// HEAD has to be answerable by a GET route, as every HTTP server does.
func TestRouterServesHeadFromGet(t *testing.T) {
	r := NewRouter("/", nil)
	mustRegister(t, r, "page", "GET", "/page")

	if _, _, ok := r.Match("HEAD", "/page"); !ok {
		t.Fatal("HEAD did not reach the GET route")
	}
}
