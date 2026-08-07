package runtime

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
	"github.com/embernet-ai/emberwire/internal/store"
)

// Node types with runtime-wide routing behaviour rather than ordinary wiring.
const (
	TypeCatch    = "catch"
	TypeStatus   = "status"
	TypeComplete = "complete"
)

// maxErrorDepth bounds how many times one message may be re-raised as an error.
//
// A Catch node wired back into the flow it catches for is a normal pattern and
// a trivially easy way to build an infinite error loop. Node-RED carries a
// counter on the error for the same reason.
const maxErrorDepth = 10

// Event is something the runtime wants the editor to know about: a status
// change, a debug message, a dropped message, a log line.
type Event struct {
	Topic string         `json:"topic"`
	Data  map[string]any `json:"data"`
	At    time.Time      `json:"at"`
}

// Event topics.
const (
	TopicStatus  = "status"
	TopicError   = "error"
	TopicDropped = "dropped"
	TopicLog     = "log"
	TopicDebug   = "debug"
)

// Runtime owns a running flow graph.
type Runtime struct {
	reg   *node.Registry
	flows *engine.Flows
	opts  Options

	ctx    context.Context
	cancel context.CancelFunc

	// runners holds every started flow node, keyed by node id. Written once
	// during Start and read-only afterwards, so delivery needs no lock.
	runners map[string]*runner

	// configs holds started configuration-node instances.
	configs map[string]node.Node

	// Routing tables, resolved at start.
	catches   []*handler
	statuses  []*handler
	completes []*completeHandler

	contexts *store.ScopedContexts

	// credentials returns a node's decrypted credentials.
	credentials func(nodeID string) map[string]string

	events chan Event
	// eventsMu guards both the channel and eventsClosed. Both are touched only
	// under this mutex — the send path and the close path have to agree on one
	// lock, or a late emit panics on a closed channel.
	eventsMu     sync.Mutex
	eventsClosed bool

	started bool
	stopped bool
	mu      sync.Mutex

	// observers are optional hooks for metrics. Nil-safe.
	onExecTime     func(nodeID, typ string, d time.Duration)
	onQueueLatency func(nodeID, typ string, d time.Duration)
}

// handler is a Catch or Status node together with the scope it watches.
type handler struct {
	r *runner

	// scope lists the node ids this handler watches. Empty means the whole
	// containing flow.
	scope map[string]bool

	// uncaught marks a Catch node that only fires when no other handler took
	// the error.
	uncaught bool

	// group is the id of the group containing the handler, "" if ungrouped.
	group string
}

// completeHandler is a Complete node and the nodes whose completion it observes.
type completeHandler struct {
	r     *runner
	scope map[string]bool
}

// New builds a runtime for a parsed flow set. Nothing starts until Start.
func New(reg *node.Registry, flows *engine.Flows, opts Options) *Runtime {
	return &Runtime{
		reg:      reg,
		flows:    flows,
		opts:     opts.withDefaults(),
		runners:  map[string]*runner{},
		configs:  map[string]node.Node{},
		contexts: store.NewScopedContexts(),
		events:   make(chan Event, 1024),
	}
}

// SetContexts replaces the context stores, which is how a persistent store is
// substituted for the default in-memory one.
func (rt *Runtime) SetContexts(c *store.ScopedContexts) { rt.contexts = c }

// SetCredentials installs the lookup used to hand a node its decrypted secrets.
func (rt *Runtime) SetCredentials(fn func(nodeID string) map[string]string) {
	rt.credentials = fn
}

// SetMetricsHooks installs optional observers for per-node timings.
func (rt *Runtime) SetMetricsHooks(execTime, queueLatency func(nodeID, typ string, d time.Duration)) {
	rt.onExecTime = execTime
	rt.onQueueLatency = queueLatency
}

// Events returns the channel the editor subscribes to. The channel is buffered;
// when it fills, events are dropped rather than blocking message delivery,
// because a slow editor must never be able to stall the plant floor.
func (rt *Runtime) Events() <-chan Event { return rt.events }

// StartError records a node that failed to build or start. A failure is scoped
// to the one node: the rest of the flow still runs, and the editor is told which
// node is broken. Failing the whole deploy because one node's config is wrong is
// how a single typo takes a line down.
type StartError struct {
	NodeID string
	Type   string
	Err    error
}

func (e StartError) Error() string {
	return fmt.Sprintf("node %s (%s): %v", e.NodeID, e.Type, e.Err)
}

// Start builds and starts every enabled node.
//
// It returns the per-node failures it tolerated. A non-nil slice does not mean
// the runtime failed to start.
func (rt *Runtime) Start(ctx context.Context) []StartError {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.started {
		return []StartError{{Err: fmt.Errorf("runtime already started")}}
	}
	rt.started = true
	rt.ctx, rt.cancel = context.WithCancel(ctx)

	var failures []StartError

	// Configuration nodes first: a flow node's factory may look one up.
	for _, id := range rt.flows.ConfigNodes() {
		n := rt.flows.Nodes[id]
		if n.Disabled {
			continue
		}
		inst, err := rt.build(n)
		if err != nil {
			failures = append(failures, StartError{NodeID: id, Type: n.Type, Err: err})
			continue
		}
		rt.configs[id] = inst
	}

	// Flow nodes. Build every runner before wiring any, because a wire may
	// point forward in file order.
	for _, id := range rt.flows.Order {
		n, ok := rt.flows.Nodes[id]
		if !ok || n.IsConfig || n.Disabled || !rt.scopeEnabled(n.Z) {
			continue
		}
		inst, err := rt.build(n)
		if err != nil {
			failures = append(failures, StartError{NodeID: id, Type: n.Type, Err: err})
			continue
		}
		rt.runners[id] = rt.newRunner(n, inst)
	}

	rt.wire()
	rt.buildRoutingTables()

	// Launch the goroutines, then start the message sources. Sources start last
	// so that no message can be produced before every consumer is draining.
	for _, r := range rt.runners {
		go r.loop(rt.ctx)
	}
	for _, id := range rt.sortedRunnerIDs() {
		r := rt.runners[id]
		s, ok := r.node.(node.Starter)
		if !ok {
			continue
		}
		if err := s.Start(rt.ctx, r.emitter()); err != nil {
			failures = append(failures, StartError{NodeID: r.id, Type: r.typ, Err: err})
		}
	}

	return failures
}

// scopeEnabled reports whether a node's containing tab is enabled. A node on a
// disabled tab is built into no runner at all.
func (rt *Runtime) scopeEnabled(z string) bool {
	if z == "" {
		return true
	}
	if tab, ok := rt.flows.Tabs[z]; ok {
		return !tab.Disabled
	}
	if _, ok := rt.flows.Subflows[z]; ok {
		return true
	}
	// A node whose scope is missing entirely was already reported as a parse
	// warning. Do not start it.
	return false
}

// build instantiates one node from its flow entry.
func (rt *Runtime) build(n *engine.Node) (node.Node, error) {
	reg, ok := rt.reg.Lookup(n.Type)
	if !ok {
		return nil, fmt.Errorf("unknown node type %q", n.Type)
	}
	return reg.New(&node.Definition{
		Node:     n,
		Services: &services{rt: rt, nodeID: n.ID, z: n.Z},
	})
}

func (rt *Runtime) newRunner(n *engine.Node, inst node.Node) *runner {
	capacity := rt.opts.InboxCapacity
	overflow := rt.opts.Overflow

	// A node may override the defaults from its flow entry, so a known-bursty
	// branch can be given a deeper queue or a lossy policy without changing the
	// whole runtime.
	if c := n.PropInt("ew_inboxCapacity", 0); c > 0 {
		capacity = c
	}
	if p := OverflowPolicy(n.PropString("ew_overflow", "")); p.Valid() {
		overflow = p
	}

	r := &runner{
		id:       n.ID,
		typ:      n.Type,
		name:     n.Name,
		z:        n.Z,
		groups:   rt.flows.GroupChain(n.ID),
		node:     inst,
		rt:       rt,
		inbox:    make(chan delivery, capacity),
		capacity: capacity,
		overflow: overflow,
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	if d, ok := inst.(node.Deferrer); ok {
		r.deferred = d
	}
	return r
}

// wire resolves every node's wires from ids to runner pointers. Wires pointing
// at a node that was not started — unknown type, disabled, failed to build — are
// dropped here, which is why delivery itself never has to check.
func (rt *Runtime) wire() {
	for id, r := range rt.runners {
		n := rt.flows.Nodes[id]
		r.wires = make([][]*runner, len(n.Wires))
		for port, targets := range n.Wires {
			for _, tid := range targets {
				if t, ok := rt.runners[tid]; ok {
					r.wires[port] = append(r.wires[port], t)
				}
			}
		}
	}
}

// buildRoutingTables collects the Catch, Status and Complete nodes.
func (rt *Runtime) buildRoutingTables() {
	for _, id := range rt.sortedRunnerIDs() {
		r := rt.runners[id]
		n := rt.flows.Nodes[id]

		switch r.typ {
		case TypeCatch:
			rt.catches = append(rt.catches, &handler{
				r:        r,
				scope:    scopeSet(n),
				uncaught: n.PropBool("uncaught", false),
				group:    n.G,
			})
		case TypeStatus:
			rt.statuses = append(rt.statuses, &handler{
				r:     r,
				scope: scopeSet(n),
				group: n.G,
			})
		case TypeComplete:
			rt.completes = append(rt.completes, &completeHandler{r: r, scope: scopeSet(n)})
		}
	}
}

// scopeSet reads a handler node's "scope" property: a list of node ids it
// watches, or absent/null meaning the whole containing flow.
func scopeSet(n *engine.Node) map[string]bool {
	raw, ok := n.Raw["scope"]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}
	set := make(map[string]bool, len(arr))
	for _, v := range arr {
		if s, ok := v.(string); ok {
			set[s] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

func (rt *Runtime) sortedRunnerIDs() []string {
	out := make([]string, 0, len(rt.runners))
	for id := range rt.runners {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Stop shuts the runtime down: sources first so nothing new is produced, then
// inboxes drain, then resources are released.
func (rt *Runtime) Stop(ctx context.Context) []error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.started || rt.stopped {
		return nil
	}
	rt.stopped = true

	closeCtx, cancel := context.WithTimeout(ctx, rt.opts.CloseTimeout)
	defer cancel()

	// Cancel first so Starter goroutines wind down and stop producing.
	rt.cancel()

	// Then let the graph go quiet before signalling anyone to exit.
	//
	// Without this, runners stop concurrently and a downstream node can drain
	// its inbox to empty and exit while an upstream node still has messages to
	// forward — the work is accepted into a channel nobody is reading any more
	// and is silently lost. Waiting for every node to be simultaneously idle is
	// what makes "a redeploy finishes work in flight" true rather than
	// usually-true.
	rt.quiesce(closeCtx)

	var errs []error
	var wg sync.WaitGroup
	for _, r := range rt.runners {
		wg.Add(1)
		go func(r *runner) {
			defer wg.Done()
			r.stop(closeCtx)
		}(r)
	}
	wg.Wait()

	// Close nodes after their inboxes have drained, so a node still holds its
	// resources while it finishes the work already queued for it.
	for _, id := range rt.sortedRunnerIDs() {
		r := rt.runners[id]
		if c, ok := r.node.(node.Closer); ok {
			if err := c.Close(closeCtx, false); err != nil {
				errs = append(errs, StartError{NodeID: r.id, Type: r.typ, Err: err})
			}
		}
	}
	for _, id := range sortedKeys(rt.configs) {
		if c, ok := rt.configs[id].(node.Closer); ok {
			if err := c.Close(closeCtx, false); err != nil {
				errs = append(errs, StartError{NodeID: id, Err: err})
			}
		}
	}

	rt.eventsMu.Lock()
	rt.eventsClosed = true
	close(rt.events)
	rt.eventsMu.Unlock()

	return errs
}

// quiesce waits until every runner is simultaneously idle — nothing queued and
// nothing inside a handler — so that messages still moving between nodes reach
// their destination before anything shuts down.
//
// A flow containing a self-sustaining cycle never goes quiet. That is what the
// context deadline is for: shutdown proceeds anyway rather than hanging, and the
// close timeout bounds how long a deploy can be held up by one runaway loop.
func (rt *Runtime) quiesce(ctx context.Context) {
	const pollInterval = 250 * time.Microsecond
	for {
		allIdle := true
		for _, r := range rt.runners {
			if !r.idle() {
				allIdle = false
				break
			}
		}
		if allIdle {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// deliver sends a message from one node's output port to everything wired to it.
//
// Every recipient gets its own clone. Node-RED hands the first recipient the
// original and clones only for the rest, which makes the last branch wired share
// mutable state with the sender — a documented memory optimisation and an
// undocumented source of aliasing bugs. The cost of cloning unconditionally is
// bounded by engine.ImmutableBytes, which shares rather than copies the large
// binary payloads where it would actually hurt.
func (rt *Runtime) deliver(from *runner, port int, msg *engine.Msg) {
	if port < 0 || port >= len(from.wires) {
		return
	}
	targets := from.wires[port]
	if len(targets) == 0 {
		return
	}
	msg.EnsureID()
	from.stats.Sent.Add(1)
	for _, t := range targets {
		t.enqueue(rt.ctx, msg.Clone(), from)
	}
}

// Inject pushes a message into a node from outside the graph. Used by the
// editor's manual Inject button and by tests.
func (rt *Runtime) Inject(nodeID string, msg *engine.Msg) error {
	r, ok := rt.runners[nodeID]
	if !ok {
		return fmt.Errorf("node %s is not running", nodeID)
	}
	msg.EnsureID()
	r.enqueue(rt.ctx, msg, nil)
	return nil
}

// raiseError routes an error to the nearest eligible Catch node.
//
// Selection follows Node-RED: candidates are Catch nodes in the same flow whose
// scope covers the failing node; among those, the one closest in the group
// hierarchy wins, so a Catch inside a group beats a Catch on the tab. Catch
// nodes marked "uncaught" fire only when nothing else did.
func (rt *Runtime) raiseError(from *runner, err error, msg *engine.Msg) {
	if from == nil {
		rt.emit(Event{Topic: TopicError, At: rt.opts.Now(), Data: map[string]any{
			"error": err.Error(),
		}})
		return
	}

	depth := 0
	if msg != nil {
		if e, ok := msg.Data["_errorDepth"].(float64); ok {
			depth = int(e)
		}
	}
	if depth >= maxErrorDepth {
		// A Catch wired back into the flow it catches for is an easy infinite
		// loop. Break it and say so, rather than spinning a core.
		rt.emit(Event{Topic: TopicError, At: rt.opts.Now(), Data: map[string]any{
			"nodeId": from.id, "type": from.typ,
			"error":   err.Error(),
			"dropped": fmt.Sprintf("error loop exceeded %d hops", maxErrorDepth),
		}})
		return
	}

	rt.emit(Event{Topic: TopicError, At: rt.opts.Now(), Data: map[string]any{
		"nodeId": from.id, "type": from.typ, "name": from.name,
		"error": err.Error(),
	}})

	targets := rt.selectHandlers(rt.catches, from)
	for _, h := range targets {
		var out *engine.Msg
		if msg != nil {
			out = msg.Clone()
		} else {
			out = engine.NewMsg()
		}
		out.Data["error"] = map[string]any{
			"message": err.Error(),
			"source": map[string]any{
				"id":   from.id,
				"type": from.typ,
				"name": from.name,
				"count": func() int {
					return depth + 1
				}(),
			},
		}
		out.Data["_errorDepth"] = float64(depth + 1)
		h.r.enqueue(rt.ctx, out, from)
	}
}

// onStatus routes a status change to Status nodes watching the reporting node,
// and publishes it for the editor.
func (rt *Runtime) onStatus(from *runner, s node.Status) {
	rt.emit(Event{Topic: TopicStatus, At: rt.opts.Now(), Data: map[string]any{
		"nodeId": from.id,
		"fill":   s.Fill, "shape": s.Shape, "text": s.Text,
		"cleared": s.Cleared(),
	}})

	for _, h := range rt.selectHandlers(rt.statuses, from) {
		m := engine.NewMsg()
		m.Data["status"] = map[string]any{
			"fill": s.Fill, "shape": s.Shape, "text": s.Text,
			"source": map[string]any{"id": from.id, "type": from.typ, "name": from.name},
		}
		h.r.enqueue(rt.ctx, m, from)
	}
}

// onComplete routes a completion to Complete nodes watching the finished node.
func (rt *Runtime) onComplete(from *runner, msg *engine.Msg, err error) {
	if err != nil {
		rt.raiseError(from, err, msg)
		return
	}
	for _, h := range rt.completes {
		if h.scope == nil || !h.scope[from.id] {
			// Unlike Catch, a Complete node with no scope watches nothing.
			// Node-RED requires an explicit selection here, and defaulting to
			// "everything" would flood the flow.
			continue
		}
		var out *engine.Msg
		if msg != nil {
			out = msg.Clone()
		} else {
			out = engine.NewMsg()
		}
		h.r.enqueue(rt.ctx, out, from)
	}
}

// selectHandlers picks the eligible handlers closest to the reporting node.
//
// Distance is measured in the group hierarchy: 0 means the handler sits in the
// same innermost group as the failing node, higher numbers mean progressively
// more enclosing groups, and a handler at flow level is furthest. A handler in a
// group that does not contain the failing node is not eligible at all.
func (rt *Runtime) selectHandlers(all []*handler, from *runner) []*handler {
	if len(all) == 0 {
		return nil
	}

	chain := from.groups // innermost first
	depthOf := func(group string) (int, bool) {
		if group == "" {
			// Flow level: eligible for everything in the flow, but furthest.
			return len(chain), true
		}
		for i, g := range chain {
			if g == group {
				return i, true
			}
		}
		return 0, false
	}

	best := -1
	var chosen []*handler
	var uncaught []*handler

	for _, h := range all {
		if h.r.z != from.z {
			continue
		}
		if h.r.id == from.id {
			// A Catch node erroring must not catch itself.
			continue
		}
		if h.scope != nil && !h.scope[from.id] {
			continue
		}
		d, ok := depthOf(h.group)
		if !ok {
			continue
		}
		if h.uncaught {
			uncaught = append(uncaught, h)
			continue
		}
		switch {
		case best == -1 || d < best:
			best = d
			chosen = []*handler{h}
		case d == best:
			chosen = append(chosen, h)
		}
	}

	if len(chosen) > 0 {
		return chosen
	}
	return uncaught
}

// onDropped publishes a dropped-message event. Silently discarding a message is
// how a flow appears healthy while losing data; every drop is counted and
// announced.
func (rt *Runtime) onDropped(r *runner, msg *engine.Msg, policy string) {
	rt.emit(Event{Topic: TopicDropped, At: rt.opts.Now(), Data: map[string]any{
		"nodeId": r.id, "type": r.typ, "policy": policy,
		"msgId": msg.ID(), "queueCap": r.capacity,
	}})
}

func (rt *Runtime) log(r *runner, level node.LogLevel, format string, args ...any) {
	rt.emit(Event{Topic: TopicLog, At: rt.opts.Now(), Data: map[string]any{
		"nodeId": r.id, "type": r.typ, "name": r.name,
		"level": string(level), "message": fmt.Sprintf(format, args...),
	}})
}

// Publish sends an arbitrary event to subscribers. The Debug node uses it.
func (rt *Runtime) Publish(e Event) { rt.emit(e) }

// emit queues an event, dropping it if the channel is full.
//
// A slow or absent editor must never apply back-pressure to message delivery.
// Node-RED's comms layer has the opposite problem: every connected editor is
// subscribed to everything and cannot unsubscribe.
func (rt *Runtime) emit(e Event) {
	if e.At.IsZero() {
		e.At = rt.opts.Now()
	}
	rt.eventsMu.Lock()
	defer rt.eventsMu.Unlock()
	if rt.eventsClosed {
		return
	}
	select {
	case rt.events <- e:
	default:
	}
}

func (rt *Runtime) observeExecTime(r *runner, d time.Duration) {
	if rt.onExecTime != nil {
		rt.onExecTime(r.id, r.typ, d)
	}
}

func (rt *Runtime) observeQueueLatency(r *runner, d time.Duration) {
	if rt.onQueueLatency != nil {
		rt.onQueueLatency(r.id, r.typ, d)
	}
}

// Snapshots returns per-node counters for every running node, sorted by id.
func (rt *Runtime) Snapshots() []Snapshot {
	out := make([]Snapshot, 0, len(rt.runners))
	for _, id := range rt.sortedRunnerIDs() {
		out = append(out, rt.runners[id].Snapshot())
	}
	return out
}

// NodeStatus returns the retained badge for a node.
func (rt *Runtime) NodeStatus(nodeID string) (node.Status, bool) {
	r, ok := rt.runners[nodeID]
	if !ok {
		return node.Status{}, false
	}
	return r.currentStatus(), true
}

// Running reports whether a node id has a live runner.
func (rt *Runtime) Running(nodeID string) bool {
	_, ok := rt.runners[nodeID]
	return ok
}

// services implements node.Services for one node instance.
type services struct {
	rt     *Runtime
	nodeID string
	z      string
}

var _ node.Services = (*services)(nil)

func (s *services) Context(scope node.ContextScope) node.Context {
	switch scope {
	case node.ScopeGlobal:
		return s.rt.contexts.Global()
	case node.ScopeFlow:
		return s.rt.contexts.Flow(s.z)
	default:
		return s.rt.contexts.Node(s.nodeID)
	}
}

func (s *services) Credential(key string) (string, bool) {
	if s.rt.credentials == nil {
		return "", false
	}
	creds := s.rt.credentials(s.nodeID)
	v, ok := creds[key]
	return v, ok
}

func (s *services) ConfigNode(id string) (node.Node, bool) {
	n, ok := s.rt.configs[id]
	return n, ok
}

// Env resolves an environment variable, innermost scope first: the node's
// containing subflow instance or tab, then the process environment.
func (s *services) Env(name string) (string, bool) {
	if tab, ok := s.rt.flows.Tabs[s.z]; ok {
		for _, ev := range tab.Env {
			if ev.Name == name {
				return fmt.Sprint(ev.Value), true
			}
		}
	}
	if sf, ok := s.rt.flows.Subflows[s.z]; ok {
		for _, ev := range sf.Env {
			if ev.Name == name {
				return fmt.Sprint(ev.Value), true
			}
		}
	}
	return lookupProcessEnv(name)
}

func (s *services) Log(level node.LogLevel, msg string, args ...any) {
	if r, ok := s.rt.runners[s.nodeID]; ok {
		s.rt.log(r, level, msg, args...)
		return
	}
	s.rt.emit(Event{Topic: TopicLog, At: s.rt.opts.Now(), Data: map[string]any{
		"nodeId": s.nodeID, "level": string(level), "message": fmt.Sprintf(msg, args...),
	}})
}
