package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/flowhttp"
	"github.com/embernet-ai/emberwire/internal/node"
)

func init() {
	registerHTTPIn()
	registerHTTPResponse()
	registerHTTPRequest()
}

// Routes is the process-wide flow route table. main installs it before any flow
// starts; the zero value has no routes and refuses to register any, so a node
// can never bind against a router nobody is serving.
var Routes = flowhttp.NewRouter("/", nil)

// Bounds on the HTTP nodes. Node-RED has none of these.
const (
	// defaultRequestTimeout is how long an HTTP In request waits for an HTTP
	// Response node before the connection is closed. Node-RED waits forever: a
	// flow that forgets the Response node leaks a connection per request until
	// the process runs out of sockets, and the symptom is a server that stops
	// answering with nothing in the log.
	defaultRequestTimeout = 60 * time.Second

	// defaultMaxBodyBytes bounds a request body and a response body.
	defaultMaxBodyBytes = 16 << 20

	// defaultOutboundTimeout bounds an HTTP Request node's own call.
	defaultOutboundTimeout = 30 * time.Second

	// maxRedirects bounds a redirect chain.
	maxRedirects = 10
)

// ---------------------------------------------------------------------------
// http in
// ---------------------------------------------------------------------------

// pendingResponse is the handle an HTTP In node puts on msg.res.
//
// It is a plain struct rather than anything serialisable, which is exactly what
// makes it survive Msg.Clone: the clone's default branch shares values it does
// not recognise rather than copying them. Two branches of a flow therefore hold
// the same response, and the once guard is what makes the second one to reply a
// reported error instead of a panic on a closed connection.
type pendingResponse struct {
	w    http.ResponseWriter
	req  *http.Request
	done chan struct{}
	once sync.Once
	// nodeID identifies the HTTP In node, for the error when nobody replies.
	nodeID string
}

// reply writes the response exactly once, reporting whether this caller was the
// one that did it.
func (p *pendingResponse) reply(status int, headers map[string]string, body []byte) bool {
	replied := false
	p.once.Do(func() {
		replied = true
		for k, v := range headers {
			p.w.Header().Set(k, v)
		}
		if p.w.Header().Get("Content-Type") == "" {
			p.w.Header().Set("Content-Type", sniffContentType(body))
		}
		p.w.WriteHeader(status)
		if p.req.Method != http.MethodHead {
			_, _ = p.w.Write(body)
		}
		close(p.done)
	})
	return replied
}

// timeout closes the request out when no Response node ran.
func (p *pendingResponse) timeout(after time.Duration) {
	p.once.Do(func() {
		p.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		p.w.WriteHeader(http.StatusGatewayTimeout)
		fmt.Fprintf(p.w, "no http response node replied to this request within %s\n", after)
		close(p.done)
	})
}

func sniffContentType(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[') {
		return "application/json"
	}
	return "text/plain; charset=utf-8"
}

type httpInNode struct {
	url      string
	method   string
	maxBody  int64
	timeout  time.Duration
	nodeID   string
	rawBody  bool
	unbind   func()
	bindOnce sync.Once

	mu      sync.Mutex
	pending int
}

func registerHTTPIn() {
	node.MustRegister(node.Descriptor{
		Type:         "http in",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "globe",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "http in",
		LabelProp:    "url",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Serves a path, with Express-style :params and a trailing *, and " +
				"builds the same msg.req / msg.res / msg.payload shape Node-RED does, " +
				"including JSON, form-encoded and raw bodies. Three deliberate " +
				"differences. A request that no HTTP Response node answers is closed " +
				"with 504 after a timeout instead of being held open forever, because " +
				"Node-RED's version leaks a connection per request until the process " +
				"runs out of sockets and stops answering with nothing in the log. Two " +
				"nodes claiming the same method and path is refused at deploy time " +
				"rather than one of them silently never firing. And a path that would " +
				"shadow the editor or the admin API is refused for the same reason. " +
				"File uploads are not parsed into msg.files; a multipart body arrives " +
				"as raw bytes.",
			UnsupportedProps: []string{"upload", "swaggerDoc"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "method", Kind: node.PropSelect, Label: "Method", Default: "get",
				Options: []node.Option{
					{Value: "get", Label: "GET"},
					{Value: "post", Label: "POST"},
					{Value: "put", Label: "PUT"},
					{Value: "delete", Label: "DELETE"},
					{Value: "patch", Label: "PATCH"},
					{Value: "all", Label: "Any method"},
				}},
			{Name: "url", Kind: node.PropString, Label: "URL", Required: true,
				Placeholder: "/readings/:line"},
			{Name: "ew_rawBody", Kind: node.PropBool, Label: "Always deliver the body as raw bytes",
				Help: "Off decodes JSON and form bodies the way Node-RED does."},
			{Name: "ew_maxBody", Kind: node.PropNumber, Label: "Request body limit (bytes)"},
			{Name: "ew_timeout", Kind: node.PropNumber, Label: "Reply timeout (seconds)", Default: 60},
		},
		Help: "Serves an HTTP endpoint. Every message must reach an HTTP Response " +
			"node or the request is closed with 504 once the reply timeout passes.",
	}, newHTTPIn)
}

func newHTTPIn(def *node.Definition) (node.Node, error) {
	n := &httpInNode{
		url:     strings.TrimSpace(def.Node.PropString("url", "")),
		method:  strings.ToUpper(def.Node.PropString("method", "get")),
		maxBody: int64(def.Node.PropInt("ew_maxBody", defaultMaxBodyBytes)),
		timeout: defaultRequestTimeout,
		nodeID:  def.Node.ID,
		rawBody: def.Node.PropBool("ew_rawBody", false),
	}
	if n.method == "ALL" {
		n.method = flowhttp.MethodAny
	}
	if n.maxBody <= 0 {
		n.maxBody = defaultMaxBodyBytes
	}
	if secs := def.Node.PropFloat("ew_timeout", 0); secs > 0 {
		n.timeout = time.Duration(secs * float64(time.Second))
	}
	if n.url == "" {
		return nil, fmt.Errorf("no URL configured")
	}
	return n, nil
}

// Receive is unreachable in a normal flow — an HTTP In node has no input — but
// the interface requires it and a subflow could wire one.
func (n *httpInNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *httpInNode) Start(ctx context.Context, out node.Emitter) error {
	unbind, err := Routes.Register(n.nodeID, n.method, n.url,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.serve(ctx, w, r, out)
		}))
	if err != nil {
		return err
	}
	n.unbind = unbind
	return nil
}

func (n *httpInNode) serve(ctx context.Context, w http.ResponseWriter, r *http.Request, out node.Emitter) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, n.maxBody))
	if err != nil {
		http.Error(w, fmt.Sprintf("request body is larger than the %d byte limit", n.maxBody),
			http.StatusRequestEntityTooLarge)
		return
	}

	pending := &pendingResponse{w: w, req: r, done: make(chan struct{}), nodeID: n.nodeID}

	m := engine.NewMsg()
	m.Data["req"] = buildRequestObject(r, body)
	m.Data["res"] = pending
	m.Data["method"] = r.Method
	m.Data["url"] = r.URL.String()
	m.SetPayload(n.decodeBody(r, body))

	n.mu.Lock()
	n.pending++
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		n.pending--
		n.mu.Unlock()
	}()

	out.Send(0, m)

	// The handler goroutine has to stay here: returning would let net/http
	// recycle the ResponseWriter while the flow still holds it.
	timer := time.NewTimer(n.timeout)
	defer timer.Stop()
	select {
	case <-pending.done:
	case <-timer.C:
		pending.timeout(n.timeout)
		out.Error(fmt.Errorf("no http response node replied to %s %s within %s",
			r.Method, r.URL.Path, n.timeout), m)
	case <-ctx.Done():
		// The flow is stopping. Answer rather than dropping the connection, so
		// a client sees a status instead of a reset.
		pending.reply(http.StatusServiceUnavailable, nil, []byte("the flow is restarting\n"))
	case <-r.Context().Done():
		// The client gave up. Nothing to write, but the Response node still has
		// to be told it lost the race rather than panicking on a dead writer.
		pending.once.Do(func() { close(pending.done) })
	}
}

// buildRequestObject assembles msg.req, matching the field names Node-RED
// exposes so an imported flow reading msg.req.headers still works.
func buildRequestObject(r *http.Request, body []byte) map[string]any {
	headers := make(map[string]any, len(r.Header))
	for k, v := range r.Header {
		// Lower-cased, because Node's http module lower-cases them and a flow
		// reading msg.req.headers["content-type"] would otherwise miss.
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}

	query := map[string]any{}
	for k, v := range r.URL.Query() {
		if len(v) == 1 {
			query[k] = v[0]
			continue
		}
		query[k] = toAnyStrings(v)
	}

	params := map[string]any{}
	for k, v := range flowhttp.RouteParams(r) {
		params[k] = v
	}

	cookies := map[string]any{}
	for _, c := range r.Cookies() {
		cookies[c.Name] = c.Value
	}

	return map[string]any{
		"method":      r.Method,
		"url":         r.URL.Path,
		"originalUrl": r.URL.String(),
		"headers":     headers,
		"query":       query,
		"params":      params,
		"cookies":     cookies,
		"ip":          clientIP(r),
		"host":        r.Host,
		// The raw body is kept alongside the decoded payload, because a
		// signature check needs the exact bytes and re-encoding the decoded form
		// does not reproduce them.
		"rawBody": engine.ImmutableBytes(body),
	}
}

func clientIP(r *http.Request) string {
	// X-Forwarded-For first, because in the App Store every request arrives
	// through the dashboard's proxy and RemoteAddr is always the proxy.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// decodeBody turns a request body into msg.payload, following Node-RED: a JSON
// body parses, a form-encoded body becomes an object, a GET carries its query,
// and anything else arrives as bytes.
func (n *httpInNode) decodeBody(r *http.Request, body []byte) any {
	if n.rawBody {
		return engine.ImmutableBytes(body)
	}

	if len(body) == 0 {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			out := map[string]any{}
			for k, v := range r.URL.Query() {
				if len(v) == 1 {
					out[k] = v[0]
					continue
				}
				out[k] = toAnyStrings(v)
			}
			return out
		}
		return ""
	}

	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch ct {
	case "application/json", "text/json":
		var parsed any
		if err := json.Unmarshal(body, &parsed); err == nil {
			return parsed
		}
		// A body that claims JSON and is not stays as text rather than being
		// dropped: the flow can see what actually arrived.
		return string(body)

	case "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return string(body)
		}
		out := make(map[string]any, len(values))
		for k, v := range values {
			if len(v) == 1 {
				out[k] = v[0]
				continue
			}
			out[k] = toAnyStrings(v)
		}
		return out

	case "text/plain", "text/html", "text/csv", "text/xml", "application/xml":
		return string(body)

	default:
		return engine.ImmutableBytes(body)
	}
}

func toAnyStrings(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

// Pending reports requests still waiting for a reply, so a redeploy does not
// tear the graph down around a request that is halfway through a flow.
func (n *httpInNode) Pending() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.pending
}

func (n *httpInNode) Close(context.Context, bool) error {
	n.bindOnce.Do(func() {
		if n.unbind != nil {
			n.unbind()
		}
	})
	return nil
}

// ---------------------------------------------------------------------------
// http response
// ---------------------------------------------------------------------------

type httpResponseNode struct {
	statusCode int
	headers    map[string]string
}

func registerHTTPResponse() {
	node.MustRegister(node.Descriptor{
		Type:         "http response",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "globe",
		Inputs:       1,
		Outputs:      0,
		Align:        "right",
		PaletteLabel: "http response",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Status code and headers from the node or from msg.statusCode and " +
				"msg.headers, with the payload as the body. Cookies set through " +
				"msg.cookies are not implemented; set a Set-Cookie header instead.",
			UnsupportedProps: []string{"cookies"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "statusCode", Kind: node.PropNumber, Label: "Status code",
				Help: "Leave empty to use msg.statusCode, or 200."},
			{Name: "headers", Kind: node.PropList, Label: "Headers", Fields: []node.Prop{
				{Name: "key", Kind: node.PropString, Label: "Header"},
				{Name: "value", Kind: node.PropString, Label: "Value"},
			}},
		},
		Help: "Replies to the request an HTTP In node started. The message must " +
			"still carry msg.res, which means it has to come from the same flow " +
			"rather than being rebuilt along the way.",
	}, newHTTPResponse)
}

func newHTTPResponse(def *node.Definition) (node.Node, error) {
	n := &httpResponseNode{
		statusCode: def.Node.PropInt("statusCode", 0),
		headers:    map[string]string{},
	}

	// Node-RED stores the header list either as an array of rows or as a plain
	// object, depending on how old the flow is.
	switch raw, _ := def.Node.Prop("headers"); t := raw.(type) {
	case []any:
		for _, e := range t {
			row, ok := e.(map[string]any)
			if !ok {
				continue
			}
			k, _ := row["key"].(string)
			if k == "" {
				continue
			}
			n.headers[k] = mustacheString(row["value"])
		}
	case map[string]any:
		for k, v := range t {
			n.headers[k] = mustacheString(v)
		}
	}
	return n, nil
}

func (n *httpResponseNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	pending, ok := m.Data["res"].(*pendingResponse)
	if !ok {
		return fmt.Errorf("the message carries no msg.res, so there is no request to reply to; " +
			"it must come from an http in node on this flow")
	}

	status := n.statusCode
	if status == 0 {
		if f, ok := asFloat(m.Data["statusCode"]); ok {
			status = int(f)
		}
	}
	if status == 0 {
		status = http.StatusOK
	}

	headers := make(map[string]string, len(n.headers))
	for k, v := range n.headers {
		headers[k] = v
	}
	if raw, ok := m.Data["headers"].(map[string]any); ok {
		for k, v := range raw {
			headers[k] = mustacheString(v)
		}
	}

	body, err := responseBody(m.Payload())
	if err != nil {
		return err
	}

	if !pending.reply(status, headers, body) {
		// Two branches both replied, or the client hung up. Either way the flow
		// author should know: Node-RED throws away the second reply silently and
		// half the response the flow believes it sent never existed.
		return fmt.Errorf("the request was already answered; only one http response " +
			"node may reply to a given message")
	}
	return nil
}

func responseBody(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(t), nil
	case []byte:
		return t, nil
	case engine.ImmutableBytes:
		return t, nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encoding the payload as the response body: %w", err)
		}
		return b, nil
	}
}

// ---------------------------------------------------------------------------
// http request
// ---------------------------------------------------------------------------

type httpRequestNode struct {
	url      string
	method   string
	ret      string // txt, bin, obj
	timeout  time.Duration
	maxBody  int64
	follow   bool
	authUser string
	authPass string
	svc      node.Services
	client   *http.Client
}

func registerHTTPRequest() {
	node.MustRegister(node.Descriptor{
		Type:         "http request",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "globe",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "http request",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Method, URL, headers, basic authentication, redirects and the three " +
				"return types, with msg.url, msg.method and msg.headers overriding the " +
				"node. The response body is size-capped and the call is bounded by a " +
				"timeout, neither of which Node-RED does. Cookie jars, proxy settings, " +
				"per-node TLS configuration and connection persistence are not " +
				"implemented in this build. There is no egress allowlist: this node " +
				"can reach anything the pod can, exactly as Node-RED's can, and the " +
				"place to bound that is a NetworkPolicy rather than an edit dialog " +
				"nobody outside the cluster can trust.",
			UnsupportedProps: []string{"proxy", "tls", "persist", "cookies"},
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "method", Kind: node.PropSelect, Label: "Method", Default: "GET",
				Options: []node.Option{
					{Value: "GET", Label: "GET"},
					{Value: "POST", Label: "POST"},
					{Value: "PUT", Label: "PUT"},
					{Value: "DELETE", Label: "DELETE"},
					{Value: "PATCH", Label: "PATCH"},
					{Value: "HEAD", Label: "HEAD"},
					{Value: "use", Label: "Take it from msg.method"},
				}},
			{Name: "url", Kind: node.PropString, Label: "URL",
				Help: "Leave empty to take it from msg.url. {{mustache}} placeholders are " +
					"filled from the message."},
			{Name: "ret", Kind: node.PropSelect, Label: "Return", Default: "txt",
				Options: []node.Option{
					{Value: "txt", Label: "A UTF-8 string"},
					{Value: "bin", Label: "Raw bytes"},
					{Value: "obj", Label: "A parsed JSON object"},
				}},
			{Name: "followRedirects", Kind: node.PropBool, Label: "Follow redirects", Default: true},
			{Name: "authType", Kind: node.PropSelect, Label: "Authentication", Default: "",
				Options: []node.Option{
					{Value: "", Label: "None"},
					{Value: "basic", Label: "Basic"},
				}},
			{Name: "user", Kind: node.PropString, Label: "Username"},
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
			{Name: "ew_timeout", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 30},
			{Name: "ew_maxBody", Kind: node.PropNumber, Label: "Response limit (bytes)"},
		},
		Help: "Makes an HTTP request and returns the response. msg.payload becomes " +
			"the request body for POST and PUT; msg.statusCode, msg.headers and " +
			"msg.responseUrl come back on the result.",
	}, newHTTPRequest)
}

func newHTTPRequest(def *node.Definition) (node.Node, error) {
	n := &httpRequestNode{
		url:     strings.TrimSpace(def.Node.PropString("url", "")),
		method:  strings.ToUpper(def.Node.PropString("method", "GET")),
		ret:     orDefault(def.Node.PropString("ret", ""), "txt"),
		timeout: defaultOutboundTimeout,
		maxBody: int64(def.Node.PropInt("ew_maxBody", defaultMaxBodyBytes)),
		follow:  def.Node.PropBool("followRedirects", true),
		svc:     def.Services,
	}
	switch n.ret {
	case "txt", "bin", "obj":
	default:
		return nil, fmt.Errorf("unknown return type %q", n.ret)
	}
	if n.maxBody <= 0 {
		n.maxBody = defaultMaxBodyBytes
	}
	if secs := def.Node.PropFloat("ew_timeout", 0); secs > 0 {
		n.timeout = time.Duration(secs * float64(time.Second))
	}

	if def.Node.PropString("authType", "") == "basic" {
		n.authUser = def.Node.PropString("user", "")
		if pass, ok := def.Services.Credential("password"); ok {
			n.authPass = pass
		}
	}

	n.client = &http.Client{
		Timeout: n.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !n.follow {
				return http.ErrUseLastResponse
			}
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
	return n, nil
}

func (n *httpRequestNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	target, err := n.resolveURL(m)
	if err != nil {
		return err
	}

	method := n.method
	if method == "USE" {
		method = strings.ToUpper(mustacheString(m.Data["method"]))
		if method == "" {
			return fmt.Errorf("the method comes from msg.method, which is not set")
		}
	}

	var body io.Reader
	contentType := ""
	if method != http.MethodGet && method != http.MethodHead && method != http.MethodDelete {
		raw, ct, err := requestBody(m.Payload())
		if err != nil {
			return err
		}
		if raw != nil {
			body = bytes.NewReader(raw)
			contentType = ct
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if headers, ok := m.Data["headers"].(map[string]any); ok {
		for k, v := range headers {
			req.Header.Set(k, mustacheString(v))
		}
	}
	if n.authUser != "" || n.authPass != "" {
		req.SetBasicAuth(n.authUser, n.authPass)
	}

	out.Status(node.Status{Fill: "blue", Shape: "dot", Text: "requesting"})
	resp, err := n.client.Do(req)
	if err != nil {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "failed"})
		return fmt.Errorf("%s %s: %w", method, target, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, n.maxBody+1))
	if err != nil {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "failed"})
		return fmt.Errorf("reading the response from %s: %w", target, err)
	}
	if int64(len(raw)) > n.maxBody {
		out.Status(node.Status{Fill: "red", Shape: "ring", Text: "too large"})
		return fmt.Errorf("the response from %s is larger than the %d byte limit; "+
			"raise ew_maxBody or fetch less", target, n.maxBody)
	}

	payload, err := n.decodeResponse(raw)
	if err != nil {
		return err
	}
	m.SetPayload(payload)
	m.Data["statusCode"] = float64(resp.StatusCode)
	m.Data["responseUrl"] = resp.Request.URL.String()

	headers := make(map[string]any, len(resp.Header))
	for k, v := range resp.Header {
		headers[strings.ToLower(k)] = strings.Join(v, ", ")
	}
	m.Data["headers"] = headers

	// A non-2xx is not an error here, matching Node-RED: the status is data, and
	// a flow polling an endpoint that returns 404 while a device boots wants to
	// handle it with a Switch node rather than a Catch.
	if resp.StatusCode >= 400 {
		out.Status(node.Status{Fill: "yellow", Shape: "dot", Text: strconv.Itoa(resp.StatusCode)})
	} else {
		out.Status(node.Status{})
	}

	out.Send(0, m)
	return nil
}

// resolveURL builds the target, filling {{mustache}} placeholders from the
// message the way Node-RED does.
func (n *httpRequestNode) resolveURL(m *engine.Msg) (string, error) {
	target := n.url
	if target == "" {
		target = strings.TrimSpace(mustacheString(m.Data["url"]))
		if target == "" {
			return "", fmt.Errorf("no URL configured and msg.url is not set")
		}
	} else if strings.Contains(target, "{{") {
		tmpl, err := parseMustache(target)
		if err != nil {
			return "", fmt.Errorf("the URL template: %w", err)
		}
		rendered, err := renderMustache(tmpl, &mustacheScope{stack: []any{m.Data}})
		if err != nil {
			return "", err
		}
		target = strings.TrimSpace(rendered)
	}

	if !strings.Contains(target, "://") {
		// Node-RED defaults a scheme-less URL to http, which is worth keeping:
		// a flow talking to a PLC's web interface almost never types one.
		target = "http://" + target
	}
	u, err := url.Parse(target)
	if err != nil {
		return "", fmt.Errorf("%q is not a valid URL: %w", target, err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return "", fmt.Errorf("%q is not an http or https URL", target)
	}
	return u.String(), nil
}

// requestBody turns a payload into request bytes and a content type.
func requestBody(v any) ([]byte, string, error) {
	switch t := v.(type) {
	case nil:
		return nil, "", nil
	case string:
		return []byte(t), "text/plain; charset=utf-8", nil
	case []byte:
		return t, "application/octet-stream", nil
	case engine.ImmutableBytes:
		return t, "application/octet-stream", nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, "", fmt.Errorf("encoding the payload as the request body: %w", err)
		}
		return b, "application/json", nil
	}
}

func (n *httpRequestNode) decodeResponse(raw []byte) (any, error) {
	switch n.ret {
	case "bin":
		return engine.ImmutableBytes(raw), nil
	case "obj":
		var parsed any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, fmt.Errorf("the response is not valid JSON: %w", err)
		}
		return parsed, nil
	default:
		return string(raw), nil
	}
}
