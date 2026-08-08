// Package flowhttp routes HTTP requests to the HTTP In nodes in a running flow.
//
// The admin API and the editor are served by a plain http.ServeMux built once at
// start-up. Flow routes cannot be: they appear and disappear on every deploy,
// and ServeMux has no way to remove a pattern. So they live here, in a router
// the API server delegates to for anything the fixed routes did not claim.
//
// Two rules that Node-RED does not have, both because the alternative fails
// quietly. A flow may not register a path that would shadow the editor or the
// admin API — the deploy is refused instead, because the symptom otherwise is
// an editor that stops loading with no explanation. And two nodes may not claim
// the same method and path; Express runs whichever matched first and the second
// node simply never fires.
package flowhttp

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Router holds the routes the flow's HTTP In nodes have registered.
type Router struct {
	mu     sync.RWMutex
	routes []*route
	root   string

	// reserved are path prefixes a flow route may not take, so a flow cannot
	// make the editor or the admin API unreachable.
	reserved []string
}

// MethodAny matches any request method, which is Node-RED's "all".
const MethodAny = ""

type route struct {
	method   string
	pattern  string
	segments []segment
	handler  http.Handler
	nodeID   string
}

type segment struct {
	literal string
	param   string // non-empty for ":name"
	wild    bool   // "*", matching the rest of the path
}

// NewRouter builds a router serving flow routes under root.
func NewRouter(root string, reserved []string) *Router {
	r := &Router{root: cleanPrefix(root)}
	for _, p := range reserved {
		if p = cleanPrefix(p); p != "/" {
			r.reserved = append(r.reserved, p)
		}
	}
	return r
}

// Root is the prefix flow routes are served under.
func (r *Router) Root() string { return r.root }

// Register adds a route and returns the function that removes it again, which is
// what an HTTP In node calls on Close.
//
// The method is one of the usual verbs, or MethodAny.
func (r *Router) Register(nodeID, method, pattern string, h http.Handler) (func(), error) {
	if h == nil {
		return nil, fmt.Errorf("no handler")
	}
	full, segs, err := r.compile(pattern)
	if err != nil {
		return nil, err
	}
	method = strings.ToUpper(strings.TrimSpace(method))

	rt := &route{method: method, pattern: full, segments: segs, handler: h, nodeID: nodeID}

	r.mu.Lock()
	defer r.mu.Unlock()

	for _, existing := range r.routes {
		if existing.pattern == full && existing.method == method {
			// Express would run whichever matched first and the second node
			// would never fire, with nothing said about it.
			return nil, fmt.Errorf("%s %s is already served by node %s",
				methodLabel(method), full, existing.nodeID)
		}
	}

	r.routes = append(r.routes, rt)
	r.sortLocked()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, e := range r.routes {
			if e == rt {
				r.routes = append(r.routes[:i], r.routes[i+1:]...)
				return
			}
		}
	}, nil
}

// Reset drops every route. Called when a runtime is torn down, so a node that
// failed to close cannot leave a route pointing at a dead flow.
func (r *Router) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.routes = nil
}

// Len reports how many routes are registered.
func (r *Router) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.routes)
}

// compile turns a node's url property into a full path and its segments.
func (r *Router) compile(pattern string) (string, []segment, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return "", nil, fmt.Errorf("no URL configured")
	}
	if !strings.HasPrefix(pattern, "/") {
		pattern = "/" + pattern
	}

	full := pattern
	if r.root != "/" {
		full = strings.TrimSuffix(r.root, "/") + pattern
	}
	full = "/" + strings.Trim(full, "/")
	if full == "" {
		full = "/"
	}

	for _, res := range r.reserved {
		if full == res || strings.HasPrefix(full+"/", res+"/") {
			return "", nil, fmt.Errorf("%s is reserved for the editor and the admin API; "+
				"serving a flow there would make them unreachable", full)
		}
	}
	if full == "/" {
		return "", nil, fmt.Errorf("a flow may not serve the root path; " +
			"the editor is there and would become unreachable")
	}

	var segs []segment
	parts := strings.Split(strings.Trim(full, "/"), "/")
	for i, p := range parts {
		switch {
		case p == "*":
			if i != len(parts)-1 {
				return "", nil, fmt.Errorf("* may only appear at the end of a path, in %q", pattern)
			}
			segs = append(segs, segment{wild: true})
		case strings.HasPrefix(p, ":"):
			name := p[1:]
			if name == "" {
				return "", nil, fmt.Errorf("a parameter in %q has no name", pattern)
			}
			segs = append(segs, segment{param: name})
		default:
			segs = append(segs, segment{literal: p})
		}
	}
	return full, segs, nil
}

// sortLocked orders routes most specific first, so Match can take the first hit.
//
// Specificity is per segment — a literal beats a parameter beats a wildcard —
// and a route naming a method beats one accepting any. Without this, /:id
// registered before /health would swallow /health, and which of the two a
// request reached would depend on the order the nodes happened to start in.
func (r *Router) sortLocked() {
	sort.SliceStable(r.routes, func(i, j int) bool {
		a, b := r.routes[i], r.routes[j]
		for k := 0; k < len(a.segments) && k < len(b.segments); k++ {
			sa, sb := segmentRank(a.segments[k]), segmentRank(b.segments[k])
			if sa != sb {
				return sa > sb
			}
		}
		if len(a.segments) != len(b.segments) {
			return len(a.segments) > len(b.segments)
		}
		if (a.method != MethodAny) != (b.method != MethodAny) {
			return a.method != MethodAny
		}
		return a.pattern < b.pattern
	})
}

func segmentRank(s segment) int {
	switch {
	case s.wild:
		return 0
	case s.param != "":
		return 1
	default:
		return 2
	}
}

// Match finds the handler for a request, along with the path parameters it
// captured.
func (r *Router) Match(method, path string) (http.Handler, map[string]string, bool) {
	method = strings.ToUpper(method)
	// HEAD is served by a GET route, as every HTTP server does; the response
	// writer drops the body.
	alt := method
	if method == http.MethodHead {
		alt = http.MethodGet
	}

	parts := splitPath(path)

	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, rt := range r.routes {
		if rt.method != MethodAny && rt.method != method && rt.method != alt {
			continue
		}
		if params, ok := matchSegments(rt.segments, parts); ok {
			return rt.handler, params, true
		}
	}
	return nil, nil, false
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func matchSegments(segs []segment, parts []string) (map[string]string, bool) {
	var params map[string]string

	for i, s := range segs {
		if s.wild {
			if params == nil {
				params = map[string]string{}
			}
			params["*"] = strings.Join(parts[i:], "/")
			return params, true
		}
		if i >= len(parts) {
			return nil, false
		}
		if s.param != "" {
			if params == nil {
				params = map[string]string{}
			}
			params[s.param] = parts[i]
			continue
		}
		if s.literal != parts[i] {
			return nil, false
		}
	}
	if len(parts) != len(segs) {
		return nil, false
	}
	return params, true
}

// routeParamsKey carries the captured path parameters from the router to the
// handler. It lives here rather than in the node package so that the API server
// can attach them without importing the whole palette.
type routeParamsKey struct{}

// WithRouteParams attaches captured path parameters to a request.
func WithRouteParams(r *http.Request, params map[string]string) *http.Request {
	if len(params) == 0 {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), routeParamsKey{}, params))
}

// RouteParams reads back what WithRouteParams attached. An HTTP In node uses it
// to fill msg.req.params.
func RouteParams(r *http.Request) map[string]string {
	p, _ := r.Context().Value(routeParamsKey{}).(map[string]string)
	return p
}

func cleanPrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = "/" + strings.Trim(p, "/")
	return p
}

func methodLabel(m string) string {
	if m == MethodAny {
		return "any method on"
	}
	return m
}
