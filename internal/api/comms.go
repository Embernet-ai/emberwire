package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/embernet-ai/emberwire/internal/runtime"
)

// The editor's event channel.
//
// Node-RED's equivalent has two properties worth not copying. Its unsubscribe
// handler is a stub — the source comments that "all clients get automatically
// subscribed to everything and cannot unsubscribe" — so every open editor
// receives every debug and status message from every flow on the instance. And
// its outbound queue is unbounded, so a slow client accumulates until the
// process suffers.
//
// Here a client subscribes to the topics it wants, and a client that cannot keep
// up is disconnected rather than allowed to consume memory. A browser tab left
// open on a laptop lid must never be able to affect a production line.

const (
	// clientQueue is how many events may be pending for one client.
	clientQueue = 256
	// writeTimeout bounds a single frame write, so one wedged TCP connection
	// cannot hold a broadcast goroutine forever.
	writeTimeout = 10 * time.Second
	// pingInterval keeps intermediaries from dropping an idle connection. The
	// App Store proxies this through the dashboard, which has its own idle
	// timeouts.
	pingInterval = 25 * time.Second
	// retainedLimit bounds how many retained status entries are kept, so a flow
	// that churns node ids cannot grow the map without bound.
	retainedLimit = 8192
)

// hub fans runtime events out to connected editors.
type hub struct {
	mu      sync.RWMutex
	clients map[*client]struct{}
	log     *slog.Logger

	// retained holds the latest status per node so a newly connected editor
	// sees current badges immediately rather than waiting for the next change.
	retained map[string]runtime.Event
}

func newHub(log *slog.Logger) *hub {
	return &hub{
		clients:  map[*client]struct{}{},
		retained: map[string]runtime.Event{},
		log:      log,
	}
}

type client struct {
	conn *websocket.Conn
	send chan runtime.Event

	mu     sync.RWMutex
	topics map[string]bool
}

// wants reports whether a client subscribed to a topic. An empty subscription
// set means everything, which is what the editor asks for on connect.
func (c *client) wants(topic string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.topics) == 0 {
		return true
	}
	if c.topics[topic] {
		return true
	}
	// MQTT-style single-level wildcard: "status/#" matches "status/nodeId".
	for t := range c.topics {
		if strings.HasSuffix(t, "/#") && strings.HasPrefix(topic, strings.TrimSuffix(t, "#")) {
			return true
		}
	}
	return false
}

// Broadcast delivers an event to every interested client.
//
// A client whose queue is full is closed. Dropping the event instead would let
// an editor silently miss a debug message, which is worse than a visible
// reconnect: the operator would be reading an incomplete picture of the plant
// and would not know it.
func (h *hub) Broadcast(e runtime.Event) {
	h.retain(e)

	h.mu.RLock()
	targets := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		if c.wants(e.Topic) {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()

	for _, c := range targets {
		select {
		case c.send <- e:
		default:
			h.log.Warn("editor client fell behind; disconnecting")
			_ = c.conn.Close(websocket.StatusPolicyViolation, "client too slow")
		}
	}
}

// retain keeps the latest status per node for replay to new clients.
func (h *hub) retain(e runtime.Event) {
	if e.Topic != runtime.TopicStatus {
		return
	}
	id, _ := e.Data["nodeId"].(string)
	if id == "" {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// A cleared status removes the retained entry, which is how a badge is
	// blanked — the same rule Node-RED uses.
	if cleared, _ := e.Data["cleared"].(bool); cleared {
		delete(h.retained, id)
		return
	}
	if len(h.retained) >= retainedLimit {
		if _, known := h.retained[id]; !known {
			return
		}
	}
	h.retained[id] = e
}

func (h *hub) add(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	close(c.send)
}

// snapshot returns the retained statuses for replay on connect.
func (h *hub) snapshot() []runtime.Event {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]runtime.Event, 0, len(h.retained))
	for _, e := range h.retained {
		out = append(out, e)
	}
	return out
}

// Clients reports how many editors are connected, for /runtime/stats.
func (h *hub) Clients() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// subscribeMessage is what a client sends to narrow its subscription.
type subscribeMessage struct {
	Subscribe   []string `json:"subscribe,omitempty"`
	Unsubscribe []string `json:"unsubscribe,omitempty"`
}

func (s *Server) handleComms(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin only. The editor is served from this origin, and the App
		// Store reaches it through the dashboard's reverse proxy, which
		// preserves the browser's view of the origin.
		InsecureSkipVerify: false,
		CompressionMode:    websocket.CompressionContextTakeover,
	})
	if err != nil {
		s.log.Debug("websocket upgrade failed", "error", err)
		return
	}

	c := &client{
		conn:   conn,
		send:   make(chan runtime.Event, clientQueue),
		topics: map[string]bool{},
	}
	s.hub.add(c)
	defer func() {
		s.hub.remove(c)
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Replay retained statuses so the canvas paints current state immediately.
	for _, e := range s.hub.snapshot() {
		select {
		case c.send <- e:
		default:
		}
	}

	go s.readSubscriptions(ctx, cancel, c)
	s.writeLoop(ctx, c)
}

// readSubscriptions handles inbound frames. It also detects a dead peer: without
// a reader, a half-open connection is only noticed when a write eventually
// times out.
func (s *Server) readSubscriptions(ctx context.Context, cancel context.CancelFunc, c *client) {
	defer cancel()
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		var msg subscribeMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		c.mu.Lock()
		for _, t := range msg.Subscribe {
			c.topics[t] = true
		}
		for _, t := range msg.Unsubscribe {
			// Actually honoured, unlike Node-RED's stub, so an editor showing
			// one tab does not have to receive every flow's debug traffic.
			delete(c.topics, t)
		}
		c.mu.Unlock()
	}
}

func (s *Server) writeLoop(ctx context.Context, c *client) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case e, ok := <-c.send:
			if !ok {
				return
			}
			// Drain whatever else is queued and send it as one batch. Under
			// load the editor receives far fewer, larger frames, which is the
			// difference between a debug sidebar that keeps up and one that
			// locks the browser.
			batch := []runtime.Event{e}
			for len(batch) < 64 {
				select {
				case next, ok := <-c.send:
					if !ok {
						break
					}
					batch = append(batch, next)
					continue
				default:
				}
				break
			}

			data, err := json.Marshal(batch)
			if err != nil {
				continue
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err = c.conn.Write(wctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				return
			}

		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
