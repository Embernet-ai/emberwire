// Package runtime schedules message delivery across a flow graph.
//
// The scheduling model is the main departure from Node-RED. Node-RED runs
// everything on one event loop and puts a setImmediate between every wire hop,
// so a message crossing five nodes yields to the loop five times and a
// CPU-heavy node stalls the editor, the HTTP endpoints and every other flow.
// Its send queue is also unbounded: a fast source outruns a slow sink until the
// process dies (node-red#855).
//
// Emberwire gives each node instance its own goroutine and its own bounded
// inbox. Within a node, messages are handled strictly in order, so a node keeps
// state across messages without locking. Across nodes, work runs in parallel.
// When an inbox fills, the configured overflow policy decides what gives — and
// something always gives, visibly, rather than the process growing until the
// kubelet kills it.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

// OverflowPolicy decides what happens when a node's inbox is full.
type OverflowPolicy string

const (
	// OverflowBlock makes the sender wait, propagating back-pressure upstream
	// to the source. This is the default because it is the only policy that
	// does not lose data.
	OverflowBlock OverflowPolicy = "block"

	// OverflowDropNewest discards the arriving message. Correct for sampling a
	// high-rate sensor feed where the freshest reading is not sacred.
	OverflowDropNewest OverflowPolicy = "drop-newest"

	// OverflowDropOldest evicts the oldest queued message to make room. Correct
	// when only the latest value matters, such as a display update.
	OverflowDropOldest OverflowPolicy = "drop-oldest"

	// OverflowError raises an error to the nearest Catch node instead of
	// queueing, letting the flow decide.
	OverflowError OverflowPolicy = "error"
)

// Valid reports whether p is a recognised policy.
func (p OverflowPolicy) Valid() bool {
	switch p {
	case OverflowBlock, OverflowDropNewest, OverflowDropOldest, OverflowError:
		return true
	}
	return false
}

// ErrInboxFull is raised under OverflowError when a target inbox is full.
var ErrInboxFull = errors.New("node inbox is full")

// ErrBlockTimeout is raised when OverflowBlock waits longer than the configured
// timeout. A flow graph may legally contain a cycle, and a cycle of full inboxes
// under a blocking policy would deadlock permanently; bounding the wait turns
// that into a reported error instead of a wedged runtime.
var ErrBlockTimeout = errors.New("timed out waiting for space in a node inbox")

// Options tunes the scheduler.
type Options struct {
	// InboxCapacity is how many messages may queue for one node. Zero uses
	// DefaultInboxCapacity.
	InboxCapacity int

	// Overflow is the default policy for nodes that do not override it.
	Overflow OverflowPolicy

	// BlockTimeout bounds how long OverflowBlock waits before giving up.
	BlockTimeout time.Duration

	// CloseTimeout bounds how long a node may take to shut down. Node-RED
	// waited indefinitely until 0.17 and now allows 15 seconds; a node that
	// will not close must not be able to wedge a deploy.
	CloseTimeout time.Duration

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Defaults applied when Options leaves a field zero.
const (
	DefaultInboxCapacity = 1024
	DefaultBlockTimeout  = 30 * time.Second
	DefaultCloseTimeout  = 15 * time.Second
)

func (o Options) withDefaults() Options {
	if o.InboxCapacity <= 0 {
		o.InboxCapacity = DefaultInboxCapacity
	}
	if !o.Overflow.Valid() {
		o.Overflow = OverflowBlock
	}
	if o.BlockTimeout <= 0 {
		o.BlockTimeout = DefaultBlockTimeout
	}
	if o.CloseTimeout <= 0 {
		o.CloseTimeout = DefaultCloseTimeout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// Stats are the per-node counters the metrics endpoint and the editor's status
// badge read. Counters are read with atomics so a scrape never blocks delivery.
type Stats struct {
	Received  atomic.Int64
	Sent      atomic.Int64
	Errors    atomic.Int64
	Dropped   atomic.Int64
	Blocked   atomic.Int64
	QueueHigh atomic.Int64
}

// Snapshot is a point-in-time copy of a node's counters.
type Snapshot struct {
	NodeID    string `json:"nodeId"`
	Type      string `json:"type"`
	Received  int64  `json:"received"`
	Sent      int64  `json:"sent"`
	Errors    int64  `json:"errors"`
	Dropped   int64  `json:"dropped"`
	Blocked   int64  `json:"blocked"`
	QueueLen  int    `json:"queueLen"`
	QueueCap  int    `json:"queueCap"`
	QueueHigh int64  `json:"queueHigh"`
}

// delivery is one message queued for a node, carrying the port it arrived on so
// that nodes with several inputs — subflow instances — can tell them apart.
type delivery struct {
	msg *engine.Msg
	// enqueued is when the message entered the inbox, used to report queue
	// latency.
	enqueued time.Time
}

// runner owns one node instance: its goroutine, its inbox, and its wiring.
type runner struct {
	id   string
	typ  string
	name string

	// z is the containing tab or subflow id, used for context scoping and for
	// resolving which Catch nodes are eligible.
	z string
	// groups is the chain of containing group ids, innermost first. Catch and
	// Status handler selection prefers the closest one.
	groups []string

	node node.Node
	rt   *Runtime

	// deferred is set when the node holds messages on a timer. It is read
	// during quiesce so that a redeploy waits for a Delay node's queue rather
	// than dropping it. nil for the nodes that never defer, which is most.
	deferred node.Deferrer

	inbox    chan delivery
	capacity int
	overflow OverflowPolicy

	// wires[port] holds the runners a message sent on that port goes to.
	// Resolved once at start so delivery never touches a map.
	wires [][]*runner

	stats Stats

	// inFlight counts messages currently inside Receive. Shutdown needs it:
	// an empty inbox does not mean idle, because the node may still be running
	// a handler that is about to send downstream.
	inFlight atomic.Int64

	// status is the last badge the node set, retained so a newly connected
	// editor can be shown current state without waiting for the next change.
	statusMu sync.RWMutex
	status   node.Status

	// quit is closed to ask the goroutine to drain and exit.
	//
	// The inbox itself is deliberately never closed. A node that does its work
	// asynchronously — any Starter, any network node with a callback — can call
	// Send at any moment, including during shutdown, and sending on a closed
	// channel panics. Signalling through a separate channel keeps the send path
	// lock-free and makes a late Send harmless instead of fatal.
	quit chan struct{}

	// done closes when the runner's goroutine has exited.
	done chan struct{}

	// quitOnce guards quit so a double stop is safe.
	quitOnce sync.Once
}

// enqueue offers a message to this node's inbox, applying the overflow policy.
//
// from identifies the sending node, so that an overflow error is attributed to
// the sender's Catch scope rather than appearing from nowhere.
func (r *runner) enqueue(ctx context.Context, msg *engine.Msg, from *runner) {
	d := delivery{msg: msg, enqueued: r.rt.opts.Now()}

	// Fast path: room available right now.
	select {
	case r.inbox <- d:
		r.recordQueueDepth()
		return
	default:
	}

	switch r.overflow {
	case OverflowDropNewest:
		r.stats.Dropped.Add(1)
		r.rt.onDropped(r, msg, "drop-newest")
		return

	case OverflowDropOldest:
		// Evict one message to make room. The receive may fail if the node's
		// goroutine drained it first, in which case the send below succeeds
		// anyway.
		select {
		case old := <-r.inbox:
			r.stats.Dropped.Add(1)
			r.rt.onDropped(r, old.msg, "drop-oldest")
		default:
		}
		select {
		case r.inbox <- d:
			r.recordQueueDepth()
		case <-ctx.Done():
		}
		return

	case OverflowError:
		r.stats.Dropped.Add(1)
		err := fmt.Errorf("%w: %s (%s) queue is at capacity %d", ErrInboxFull, r.id, r.typ, r.capacity)
		r.rt.raiseError(from, err, msg)
		return

	default: // OverflowBlock
		r.stats.Blocked.Add(1)
		timer := time.NewTimer(r.rt.opts.BlockTimeout)
		defer timer.Stop()
		select {
		case r.inbox <- d:
			r.recordQueueDepth()
		case <-ctx.Done():
		case <-timer.C:
			// A cycle of saturated inboxes would otherwise deadlock here
			// permanently. Report it and drop, so the flow keeps running and
			// the operator finds out.
			r.stats.Dropped.Add(1)
			err := fmt.Errorf("%w: %s (%s) after %s", ErrBlockTimeout, r.id, r.typ, r.rt.opts.BlockTimeout)
			r.rt.raiseError(from, err, msg)
		}
	}
}

// recordQueueDepth tracks the high-water mark, which is what tells an operator a
// flow is close to overflowing before it actually does.
func (r *runner) recordQueueDepth() {
	depth := int64(len(r.inbox))
	for {
		high := r.stats.QueueHigh.Load()
		if depth <= high || r.stats.QueueHigh.CompareAndSwap(high, depth) {
			return
		}
	}
}

// loop is the node's goroutine. It handles exactly one message at a time, so a
// node needs no internal locking for state it keeps across messages.
//
// On quit it drains whatever is already queued before exiting, so a redeploy
// finishes work in flight rather than silently discarding it.
func (r *runner) loop(ctx context.Context) {
	defer close(r.done)

	for {
		select {
		case d := <-r.inbox:
			r.handle(ctx, d)
		case <-r.quit:
			for {
				select {
				case d := <-r.inbox:
					r.handle(ctx, d)
				default:
					return
				}
			}
		}
	}
}

// handle runs one message through the node, converting a panic into an error
// rather than taking the process down.
func (r *runner) handle(ctx context.Context, d delivery) {
	r.stats.Received.Add(1)
	r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	r.rt.observeQueueLatency(r, r.rt.opts.Now().Sub(d.enqueued))

	defer func() {
		if p := recover(); p != nil {
			// A node panicking is a bug in that node. Node-RED's equivalent —
			// an exception in a node's input handler — takes down the whole
			// process. Containing it to one message keeps the other flows on
			// this pod alive.
			r.stats.Errors.Add(1)
			err := fmt.Errorf("node %s (%s) panicked: %v", r.id, r.typ, p)
			r.rt.raiseError(r, err, d.msg)
		}
	}()

	start := r.rt.opts.Now()
	err := r.node.Receive(ctx, d.msg, r.emitter())
	r.rt.observeExecTime(r, r.rt.opts.Now().Sub(start))

	if err != nil {
		r.stats.Errors.Add(1)
		r.rt.raiseError(r, err, d.msg)
	}
}

// emitter returns this runner's Emitter. It is a value type wrapping the runner
// pointer, so handing it out costs nothing and it is safe to keep.
func (r *runner) emitter() node.Emitter { return emitter{r: r} }

// idle reports whether the node has nothing queued, nothing in a handler and
// nothing held back on a timer.
//
// The last of those is the reason a Delay node's contents survive a redeploy:
// an empty inbox is not the same as no work outstanding.
func (r *runner) idle() bool {
	if len(r.inbox) != 0 || r.inFlight.Load() != 0 {
		return false
	}
	return r.deferred == nil || r.deferred.Pending() == 0
}

// Snapshot copies the runner's counters.
func (r *runner) Snapshot() Snapshot {
	return Snapshot{
		NodeID:    r.id,
		Type:      r.typ,
		Received:  r.stats.Received.Load(),
		Sent:      r.stats.Sent.Load(),
		Errors:    r.stats.Errors.Load(),
		Dropped:   r.stats.Dropped.Load(),
		Blocked:   r.stats.Blocked.Load(),
		QueueLen:  len(r.inbox),
		QueueCap:  r.capacity,
		QueueHigh: r.stats.QueueHigh.Load(),
	}
}

// setStatus retains the node's badge and forwards it to Status nodes and the
// editor.
func (r *runner) setStatus(s node.Status) {
	r.statusMu.Lock()
	r.status = s
	r.statusMu.Unlock()
	r.rt.onStatus(r, s)
}

// currentStatus returns the retained badge.
func (r *runner) currentStatus() node.Status {
	r.statusMu.RLock()
	defer r.statusMu.RUnlock()
	return r.status
}

// stop asks the goroutine to drain its inbox and exit, then waits for it.
//
// Draining rather than cancelling means messages already queued are still
// processed, so a redeploy does not silently discard work in flight.
func (r *runner) stop(ctx context.Context) {
	r.quitOnce.Do(func() { close(r.quit) })
	select {
	case <-r.done:
	case <-ctx.Done():
	}
}

// emitter is the Emitter handed to a node. Every method is safe to call from any
// goroutine, so a node doing asynchronous work can emit from a callback.
type emitter struct{ r *runner }

var _ node.Emitter = emitter{}

func (e emitter) Send(port int, msg *engine.Msg) {
	if msg == nil {
		return
	}
	e.r.rt.deliver(e.r, port, msg)
}

func (e emitter) SendAll(byPort [][]*engine.Msg) {
	for port, msgs := range byPort {
		for _, m := range msgs {
			if m != nil {
				e.r.rt.deliver(e.r, port, m)
			}
		}
	}
}

func (e emitter) Status(s node.Status) { e.r.setStatus(s) }

func (e emitter) Error(err error, msg *engine.Msg) {
	if err == nil {
		return
	}
	e.r.stats.Errors.Add(1)
	e.r.rt.raiseError(e.r, err, msg)
}

func (e emitter) Done(msg *engine.Msg, err error) { e.r.rt.onComplete(e.r, msg, err) }

func (e emitter) Publish(topic string, data map[string]any) {
	e.r.rt.Publish(Event{Topic: topic, Data: data})
}

func (e emitter) Log(level node.LogLevel, format string, args ...any) {
	e.r.rt.log(e.r, level, format, args...)
}
