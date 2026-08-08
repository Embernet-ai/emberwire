package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

func init() {
	registerWebSocketListener()
	registerWebSocketClient()
	registerWebSocketIn()
	registerWebSocketOut()
}

// Bounds on the websocket nodes.
const (
	// wsSendQueue is how many messages may be pending for one connection. A
	// client that cannot keep up is disconnected rather than allowed to grow the
	// heap — the same rule the editor's own event channel follows, and for the
	// same reason: a browser tab left open on a laptop lid must not be able to
	// affect a production line.
	wsSendQueue = 256

	// wsWriteTimeout bounds a single frame write, so one wedged TCP connection
	// cannot hold a broadcast forever.
	wsWriteTimeout = 10 * time.Second

	// wsMaxFrameBytes bounds an inbound message.
	wsMaxFrameBytes = 8 << 20

	// wsReconnectDelay is how long a client waits before dialling again.
	wsReconnectDelay = 3 * time.Second
)

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

// wsSession is one open connection, on either side.
type wsSession struct {
	id   string
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
	once sync.Once
}

func newSession(conn *websocket.Conn) *wsSession {
	return &wsSession{
		id:   engine.GenerateID(),
		conn: conn,
		send: make(chan []byte, wsSendQueue),
		done: make(chan struct{}),
	}
}

// queue offers a frame to the connection, reporting whether there was room.
//
// It never blocks. Blocking here would apply back-pressure from a slow browser
// all the way into the flow's scheduler, which is a much worse failure than
// dropping a connection that cannot keep up.
func (s *wsSession) queue(data []byte) bool {
	select {
	case <-s.done:
		return false
	case s.send <- data:
		return true
	default:
		return false
	}
}

func (s *wsSession) close(code websocket.StatusCode, reason string) {
	s.once.Do(func() {
		close(s.done)
		_ = s.conn.Close(code, reason)
	})
}

// writeLoop drains the queue onto the wire.
func (s *wsSession) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case frame := <-s.send:
			wctx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
			err := s.conn.Write(wctx, websocket.MessageText, frame)
			cancel()
			if err != nil {
				s.close(websocket.StatusInternalError, "write failed")
				return
			}
		}
	}
}

// wsEndpoint is the behaviour the In and Out nodes share, implemented by both
// config node types.
type wsEndpoint interface {
	// subscribe registers a receiver for inbound frames, returning the function
	// that removes it.
	subscribe(fn func(session *wsSession, data []byte)) func()
	// sessions returns every open connection, for a broadcast.
	sessions() []*wsSession
	// lookup finds one connection by id.
	lookup(id string) (*wsSession, bool)
	// wholeMsg reports whether frames carry a whole message as JSON rather than
	// just a payload.
	wholeMsg() bool
	// describe names the endpoint for a status badge.
	describe() string
}

// wsHub is the session bookkeeping both config nodes need.
type wsHub struct {
	mu       sync.RWMutex
	open     map[string]*wsSession
	handlers map[int]func(*wsSession, []byte)
	nextID   int
	whole    bool
}

func newWSHub(whole bool) *wsHub {
	return &wsHub{
		open:     map[string]*wsSession{},
		handlers: map[int]func(*wsSession, []byte){},
		whole:    whole,
	}
}

func (h *wsHub) subscribe(fn func(*wsSession, []byte)) func() {
	h.mu.Lock()
	id := h.nextID
	h.nextID++
	h.handlers[id] = fn
	h.mu.Unlock()

	return func() {
		h.mu.Lock()
		delete(h.handlers, id)
		h.mu.Unlock()
	}
}

func (h *wsHub) deliver(s *wsSession, data []byte) {
	h.mu.RLock()
	fns := make([]func(*wsSession, []byte), 0, len(h.handlers))
	for _, fn := range h.handlers {
		fns = append(fns, fn)
	}
	h.mu.RUnlock()
	for _, fn := range fns {
		fn(s, data)
	}
}

func (h *wsHub) add(s *wsSession) {
	h.mu.Lock()
	h.open[s.id] = s
	h.mu.Unlock()
}

func (h *wsHub) remove(s *wsSession) {
	h.mu.Lock()
	delete(h.open, s.id)
	h.mu.Unlock()
}

func (h *wsHub) sessions() []*wsSession {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*wsSession, 0, len(h.open))
	for _, s := range h.open {
		out = append(out, s)
	}
	return out
}

func (h *wsHub) lookup(id string) (*wsSession, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.open[id]
	return s, ok
}

func (h *wsHub) wholeMsg() bool { return h.whole }

func (h *wsHub) closeAll(reason string) {
	for _, s := range h.sessions() {
		s.close(websocket.StatusGoingAway, reason)
	}
}

// readLoop pumps one connection until it closes.
func (h *wsHub) readLoop(ctx context.Context, s *wsSession) {
	defer h.remove(s)
	defer s.close(websocket.StatusNormalClosure, "")

	s.conn.SetReadLimit(wsMaxFrameBytes)
	go s.writeLoop(ctx)

	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			return
		}
		h.deliver(s, data)
	}
}

// ---------------------------------------------------------------------------
// websocket-listener
// ---------------------------------------------------------------------------

// wsListenerNode accepts connections on a path served by the flow router.
type wsListenerNode struct {
	*wsHub
	path   string
	nodeID string
	unbind func()
}

func registerWebSocketListener() {
	node.MustRegister(node.Descriptor{
		Type:         "websocket-listener",
		Category:     node.CategoryConfig,
		Color:        colorNetwork,
		Icon:         "globe",
		IsConfig:     true,
		PaletteLabel: "websocket-listener",
		LabelProp:    "path",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Serves a websocket path, in payload mode or whole-message mode. " +
				"The path shares the flow route table with the HTTP In nodes, so it " +
				"cannot shadow the editor or the admin API and cannot collide with " +
				"another node's path. A client that stops reading is disconnected " +
				"rather than queued without limit, which Node-RED does not do.",
		},
		Props: []node.Prop{
			{Name: "path", Kind: node.PropString, Label: "Path", Required: true,
				Placeholder: "/ws/readings"},
			{Name: "wholemsg", Kind: node.PropSelect, Label: "Send and receive", Default: "false",
				Options: []node.Option{
					{Value: "false", Label: "The payload only"},
					{Value: "true", Label: "The entire message, as JSON"},
				}},
		},
		Help: "Accepts websocket connections on a path. Referenced by WebSocket In " +
			"and WebSocket Out nodes.",
	}, newWebSocketListener)
}

func newWebSocketListener(def *node.Definition) (node.Node, error) {
	n := &wsListenerNode{
		wsHub:  newWSHub(def.Node.PropString("wholemsg", "false") == "true"),
		path:   strings.TrimSpace(def.Node.PropString("path", "")),
		nodeID: def.Node.ID,
	}
	if n.path == "" {
		return nil, fmt.Errorf("no path configured")
	}
	return n, nil
}

func (n *wsListenerNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *wsListenerNode) Start(ctx context.Context, _ node.Emitter) error {
	unbind, err := Routes.Register(n.nodeID, http.MethodGet, n.path,
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
				// The App Store proxies this through the dashboard on a
				// different host, so the browser's Origin never matches ours.
				// Origin is not the boundary here; the dashboard's own auth is.
				InsecureSkipVerify: true,
			})
			if err != nil {
				return
			}
			s := newSession(conn)
			n.add(s)
			n.readLoop(r.Context(), s)
		}))
	if err != nil {
		return err
	}
	n.unbind = unbind

	go func() {
		<-ctx.Done()
		n.closeAll("the flow is stopping")
	}()
	return nil
}

func (n *wsListenerNode) Close(context.Context, bool) error {
	if n.unbind != nil {
		n.unbind()
	}
	n.closeAll("the flow is stopping")
	return nil
}

func (n *wsListenerNode) describe() string { return n.path }

// ---------------------------------------------------------------------------
// websocket-client
// ---------------------------------------------------------------------------

// wsClientNode keeps one outbound connection open, redialling when it drops.
type wsClientNode struct {
	*wsHub
	url         string
	subprotocol string
}

func registerWebSocketClient() {
	node.MustRegister(node.Descriptor{
		Type:         "websocket-client",
		Category:     node.CategoryConfig,
		Color:        colorNetwork,
		Icon:         "globe",
		IsConfig:     true,
		PaletteLabel: "websocket-client",
		LabelProp:    "path",
		Compatibility: node.Compatibility{
			Level: node.CompatPartial,
			Notes: "Connects out and reconnects on its own when the connection drops, " +
				"in payload mode or whole-message mode. Per-node TLS configuration is " +
				"not implemented; the system trust store is used.",
			UnsupportedProps: []string{"tls"},
		},
		Props: []node.Prop{
			{Name: "path", Kind: node.PropString, Label: "URL", Required: true,
				Placeholder: "ws://gateway.local:8080/feed"},
			{Name: "subprotocol", Kind: node.PropString, Label: "Sub-protocol"},
			{Name: "wholemsg", Kind: node.PropSelect, Label: "Send and receive", Default: "false",
				Options: []node.Option{
					{Value: "false", Label: "The payload only"},
					{Value: "true", Label: "The entire message, as JSON"},
				}},
		},
		Help: "Connects to a websocket server. Referenced by WebSocket In and " +
			"WebSocket Out nodes.",
	}, newWebSocketClient)
}

func newWebSocketClient(def *node.Definition) (node.Node, error) {
	n := &wsClientNode{
		wsHub:       newWSHub(def.Node.PropString("wholemsg", "false") == "true"),
		url:         strings.TrimSpace(def.Node.PropString("path", "")),
		subprotocol: strings.TrimSpace(def.Node.PropString("subprotocol", "")),
	}
	if n.url == "" {
		return nil, fmt.Errorf("no URL configured")
	}
	if !strings.HasPrefix(n.url, "ws://") && !strings.HasPrefix(n.url, "wss://") {
		return nil, fmt.Errorf("%q must start with ws:// or wss://", n.url)
	}
	return n, nil
}

func (n *wsClientNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *wsClientNode) Start(ctx context.Context, _ node.Emitter) error {
	go n.dialLoop(ctx)
	return nil
}

func (n *wsClientNode) dialLoop(ctx context.Context) {
	opts := &websocket.DialOptions{}
	if n.subprotocol != "" {
		opts.Subprotocols = []string{n.subprotocol}
	}

	for {
		if ctx.Err() != nil {
			return
		}

		conn, _, err := websocket.Dial(ctx, n.url, opts)
		if err == nil {
			s := newSession(conn)
			n.add(s)
			n.readLoop(ctx, s)
		}

		// Redial on a fixed delay rather than backing off. A gateway on a plant
		// floor that reboots should be reconnected to promptly, and the
		// connection count is one, so there is nothing to be gentle about.
		select {
		case <-ctx.Done():
			return
		case <-time.After(wsReconnectDelay):
		}
	}
}

func (n *wsClientNode) Close(context.Context, bool) error {
	n.closeAll("the flow is stopping")
	return nil
}

func (n *wsClientNode) describe() string { return n.url }

// ---------------------------------------------------------------------------
// websocket in
// ---------------------------------------------------------------------------

type wsInNode struct {
	configID    string
	svc         node.Services
	unsubscribe func()
}

func registerWebSocketIn() {
	node.MustRegister(node.Descriptor{
		Type:         "websocket in",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "globe",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "websocket in",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatFull,
			Notes: "Emits a message per frame, carrying msg._session so a WebSocket Out " +
				"node can reply to the connection it came from.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Listen on",
				ConfigType: "websocket-listener"},
			{Name: "client", Kind: node.PropConfigRef, Label: "Connect to",
				ConfigType: "websocket-client"},
		},
		Help: "Receives websocket frames. Point it at either a listener or a client " +
			"configuration, not both.",
	}, newWebSocketIn)
}

func newWebSocketIn(def *node.Definition) (node.Node, error) {
	server := def.Node.PropString("server", "")
	client := def.Node.PropString("client", "")
	switch {
	case server != "" && client != "":
		return nil, fmt.Errorf("both a listener and a client are configured; pick one")
	case server == "" && client == "":
		return nil, fmt.Errorf("no listener or client configured")
	}
	id := server
	if id == "" {
		id = client
	}
	return &wsInNode{configID: id, svc: def.Services}, nil
}

func (n *wsInNode) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func (n *wsInNode) Start(_ context.Context, out node.Emitter) error {
	ep, err := lookupEndpoint(n.svc, n.configID)
	if err != nil {
		return err
	}

	n.unsubscribe = ep.subscribe(func(s *wsSession, data []byte) {
		m, err := frameToMsg(data, ep.wholeMsg())
		if err != nil {
			out.Error(err, nil)
			return
		}
		// The session id is what lets a WebSocket Out node reply to the
		// connection a message came from rather than broadcasting.
		m.Data["_session"] = map[string]any{"type": "websocket", "id": s.id}
		out.Send(0, m)
	})
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: ep.describe()})
	return nil
}

func (n *wsInNode) Close(context.Context, bool) error {
	if n.unsubscribe != nil {
		n.unsubscribe()
	}
	return nil
}

// frameToMsg turns an inbound frame into a message.
func frameToMsg(data []byte, whole bool) (*engine.Msg, error) {
	if !whole {
		return engine.NewMsgWithPayload(string(data)), nil
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		// Refused rather than silently downgraded to a payload: the node is
		// configured for whole messages, and quietly delivering something else
		// would make every property the flow reads come back undefined.
		return nil, fmt.Errorf("the connection is in whole-message mode but the frame "+
			"is not a JSON object: %w", err)
	}
	return engine.WrapMsg(raw), nil
}

// ---------------------------------------------------------------------------
// websocket out
// ---------------------------------------------------------------------------

type wsOutNode struct {
	configID string
	svc      node.Services
}

func registerWebSocketOut() {
	node.MustRegister(node.Descriptor{
		Type:         "websocket out",
		Category:     node.CategoryNetwork,
		Color:        colorNetwork,
		Icon:         "globe",
		Inputs:       1,
		Outputs:      0,
		Align:        "right",
		PaletteLabel: "websocket out",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Replies to the connection named by msg._session, or broadcasts to " +
				"every open connection when there is none, as Node-RED does. The " +
				"divergence is what happens when a connection cannot keep up: the " +
				"frame is refused to a Catch node and the connection is closed, rather " +
				"than queued without limit. Blocking instead would push back-pressure " +
				"from a slow browser into the flow's scheduler, which is a worse " +
				"failure than losing a connection that had already stopped reading.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Listen on",
				ConfigType: "websocket-listener"},
			{Name: "client", Kind: node.PropConfigRef, Label: "Connect to",
				ConfigType: "websocket-client"},
		},
		Help: "Sends the payload over a websocket. With msg._session it replies to " +
			"one connection; without, it broadcasts to all of them.",
	}, newWebSocketOut)
}

func newWebSocketOut(def *node.Definition) (node.Node, error) {
	server := def.Node.PropString("server", "")
	client := def.Node.PropString("client", "")
	switch {
	case server != "" && client != "":
		return nil, fmt.Errorf("both a listener and a client are configured; pick one")
	case server == "" && client == "":
		return nil, fmt.Errorf("no listener or client configured")
	}
	id := server
	if id == "" {
		id = client
	}
	return &wsOutNode{configID: id, svc: def.Services}, nil
}

func (n *wsOutNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	ep, err := lookupEndpoint(n.svc, n.configID)
	if err != nil {
		return err
	}

	frame, err := msgToFrame(m, ep.wholeMsg())
	if err != nil {
		return err
	}

	targets := ep.sessions()
	if sess, ok := m.Data["_session"].(map[string]any); ok {
		id, _ := sess["id"].(string)
		s, found := ep.lookup(id)
		if !found {
			// The connection went away between the request and the reply. That
			// is ordinary, and saying so beats writing into nothing.
			return fmt.Errorf("the websocket session %s is no longer connected", id)
		}
		targets = []*wsSession{s}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no websocket connections are open on %s", ep.describe())
	}

	var slow int
	for _, s := range targets {
		if !s.queue(frame) {
			slow++
			s.close(websocket.StatusPolicyViolation, "the client fell too far behind")
		}
	}
	if slow > 0 {
		return fmt.Errorf("%d websocket connection(s) could not keep up and were closed", slow)
	}
	return nil
}

func msgToFrame(m *engine.Msg, whole bool) ([]byte, error) {
	if whole {
		// _session is bookkeeping, not part of the message the far end asked
		// for, and it names a connection id that means nothing over there.
		cp := m.Clone()
		delete(cp.Data, "_session")
		b, err := json.Marshal(cp.Data)
		if err != nil {
			return nil, fmt.Errorf("encoding the message for the websocket: %w", err)
		}
		return b, nil
	}

	switch t := m.Payload().(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(t), nil
	case []byte:
		return t, nil
	case engine.ImmutableBytes:
		return t, nil
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("encoding the payload for the websocket: %w", err)
		}
		return b, nil
	}
}

// lookupEndpoint resolves the config node a WebSocket In or Out points at.
func lookupEndpoint(svc node.Services, id string) (wsEndpoint, error) {
	if svc == nil {
		return nil, errors.New("no runtime services available")
	}
	cfg, ok := svc.ConfigNode(id)
	if !ok {
		return nil, fmt.Errorf("the websocket configuration node %s is not running", id)
	}
	ep, ok := cfg.(wsEndpoint)
	if !ok {
		return nil, fmt.Errorf("node %s is not a websocket listener or client", id)
	}
	return ep, nil
}
