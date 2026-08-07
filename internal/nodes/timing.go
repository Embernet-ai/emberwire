package nodes

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

func init() {
	registerDelay()
	registerTrigger()
}

// defaultDeferLimit bounds how many messages a Delay or Trigger node will hold.
//
// Node-RED holds an unbounded number, which is the same class of bug as
// node-red#855: a source faster than the delay drains grows the heap until the
// kubelet kills the pod, and the flow author's first symptom is a restart with
// nothing in the log to explain it. Everything past the limit is refused to a
// Catch node, loudly, at the point the assumption actually broke.
const defaultDeferLimit = 10000

// ---------------------------------------------------------------------------
// units
// ---------------------------------------------------------------------------

// parseTimeUnits converts one of Node-RED's unit names to a multiplier. Both
// spellings are here because the Delay node persists "milliseconds" where the
// Trigger node persists "ms" for the same thing.
func parseTimeUnits(unit string) (time.Duration, error) {
	switch unit {
	case "", "ms", "milliseconds":
		return time.Millisecond, nil
	case "s", "seconds":
		return time.Second, nil
	case "min", "minutes":
		return time.Minute, nil
	case "hr", "hours":
		return time.Hour, nil
	case "d", "days":
		return 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("unknown time unit %q", unit)
}

// parseRateUnits converts the Delay node's rate-window unit.
func parseRateUnits(unit string) (time.Duration, error) {
	switch unit {
	case "", "second":
		return time.Second, nil
	case "minute":
		return time.Minute, nil
	case "hour":
		return time.Hour, nil
	case "day":
		return 24 * time.Hour, nil
	}
	return 0, fmt.Errorf("unknown rate unit %q", unit)
}

func scaled(v float64, unit time.Duration) time.Duration {
	return time.Duration(v * float64(unit))
}

// ---------------------------------------------------------------------------
// delay
// ---------------------------------------------------------------------------

// Delay modes, spelled as the flow file stores them.
const (
	delayFixed    = "delay"  // a fixed pause
	delayVariable = "delayv" // the pause comes from msg.delay
	delayRandom   = "random" // a pause chosen uniformly from a range
	delayRate     = "rate"   // at most one message per interval
	delayQueue    = "queue"  // one message per topic per interval, newest wins
	delayTimed    = "timed"  // everything queued, released every interval
)

// delayNode holds messages back: either each message individually on its own
// timer, or the whole stream behind a rate limiter.
//
// The three rate-limiting modes share one background goroutine and a queue; the
// three pausing modes use a goroutine per message. Both halves report their
// outstanding work through Pending, so a redeploy waits for it, and both release
// everything they hold as soon as the context is cancelled, so the wait is short.
type delayNode struct {
	mode      string
	fixed     time.Duration
	randMin   time.Duration
	randMax   time.Duration
	interval  time.Duration
	drop      bool
	allowRate bool
	limit     int
	dropPort  int // -1 when the node has only one output
	now       func() time.Time

	mu       sync.Mutex
	out      node.Emitter
	queue    []*engine.Msg
	byTopic  map[string]*engine.Msg
	order    []string
	lastSent time.Time
	timers   int

	// release is closed when the context is cancelled, telling every waiting
	// timer to fire now rather than lose its message.
	release chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

func registerDelay() {
	node.MustRegister(node.Descriptor{
		Type:         "delay",
		Category:     node.CategoryFunction,
		Color:        colorFunction,
		Icon:         "timer",
		Inputs:       1,
		Outputs:      1,
		OutputsProp:  "outputs",
		PaletteLabel: "delay",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "All six modes are implemented — fixed, variable, random, rate " +
				"limit, per-topic queue and timed release — along with msg.reset, " +
				"msg.flush and the second output for dropped messages. Two deliberate " +
				"differences: the queue is bounded, and past the limit a message is " +
				"refused to a Catch node rather than held, because Node-RED's " +
				"unbounded queue turns a source faster than the drain into an " +
				"OOM-kill with no explanation; and messages still held when the flow " +
				"stops are released rather than discarded.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "pauseType", Kind: node.PropSelect, Label: "Action", Default: delayFixed,
				Options: []node.Option{
					{Value: delayFixed, Label: "Delay each message by a fixed time"},
					{Value: delayVariable, Label: "Delay each message by msg.delay"},
					{Value: delayRandom, Label: "Delay each message by a random time"},
					{Value: delayRate, Label: "Rate limit"},
					{Value: delayQueue, Label: "Rate limit, one message per topic"},
					{Value: delayTimed, Label: "Rate limit, release everything queued"},
				}},
			{Name: "timeout", Kind: node.PropNumber, Label: "Delay", Default: 5},
			{Name: "timeoutUnits", Kind: node.PropSelect, Label: "Delay units", Default: "seconds",
				Options: timeUnitOptions()},
			{Name: "randomFirst", Kind: node.PropNumber, Label: "Random from", Default: 1},
			{Name: "randomLast", Kind: node.PropNumber, Label: "Random to", Default: 5},
			{Name: "randomUnits", Kind: node.PropSelect, Label: "Random units", Default: "seconds",
				Options: timeUnitOptions()},
			{Name: "rate", Kind: node.PropNumber, Label: "Rate", Default: 1},
			{Name: "nbRateUnits", Kind: node.PropNumber, Label: "Per", Default: 1},
			{Name: "rateUnits", Kind: node.PropSelect, Label: "Rate window", Default: "second",
				Options: []node.Option{
					{Value: "second", Label: "second"},
					{Value: "minute", Label: "minute"},
					{Value: "hour", Label: "hour"},
					{Value: "day", Label: "day"},
				}},
			{Name: "drop", Kind: node.PropBool, Label: "Drop intermediate messages"},
			{Name: "allowrate", Kind: node.PropBool, Label: "Allow msg.rate to override the rate"},
			{Name: "outputs", Kind: node.PropNumber, Label: "Outputs", Default: 1,
				Help: "Set to 2 to send dropped messages out of a second output instead of discarding them."},
			{Name: "ew_maxQueue", Kind: node.PropNumber, Label: "Queue limit",
				Help: "How many messages this node will hold before refusing. Emberwire's own; " +
					"Node-RED's queue has no limit."},
		},
		Help: "Holds messages back, either individually or as a rate limiter. " +
			"msg.reset empties the queue, msg.flush releases it — set msg.flush to a " +
			"number to release that many. The queue is bounded; past the limit a " +
			"message is raised to a Catch node rather than quietly retained.",
	}, newDelay)
}

func timeUnitOptions() []node.Option {
	return []node.Option{
		{Value: "milliseconds", Label: "milliseconds"},
		{Value: "seconds", Label: "seconds"},
		{Value: "minutes", Label: "minutes"},
		{Value: "hours", Label: "hours"},
		{Value: "days", Label: "days"},
	}
}

func newDelay(def *node.Definition) (node.Node, error) {
	n := &delayNode{
		mode:      def.Node.PropString("pauseType", delayFixed),
		drop:      def.Node.PropBool("drop", false),
		allowRate: def.Node.PropBool("allowrate", false),
		limit:     def.Node.PropInt("ew_maxQueue", defaultDeferLimit),
		dropPort:  -1,
		now:       time.Now,
		byTopic:   map[string]*engine.Msg{},
		release:   make(chan struct{}),
	}
	if n.limit <= 0 {
		n.limit = defaultDeferLimit
	}
	if def.Node.PropInt("outputs", 1) >= 2 {
		n.dropPort = 1
	}

	switch n.mode {
	case delayFixed, delayVariable:
		unit, err := parseTimeUnits(def.Node.PropString("timeoutUnits", "seconds"))
		if err != nil {
			return nil, err
		}
		n.fixed = scaled(def.Node.PropFloat("timeout", 5), unit)
		if n.fixed < 0 {
			return nil, fmt.Errorf("the delay must not be negative")
		}

	case delayRandom:
		unit, err := parseTimeUnits(def.Node.PropString("randomUnits", "seconds"))
		if err != nil {
			return nil, err
		}
		n.randMin = scaled(def.Node.PropFloat("randomFirst", 1), unit)
		n.randMax = scaled(def.Node.PropFloat("randomLast", 5), unit)
		if n.randMin < 0 || n.randMax < 0 {
			return nil, fmt.Errorf("a random delay range must not be negative")
		}
		if n.randMax < n.randMin {
			n.randMin, n.randMax = n.randMax, n.randMin
		}

	case delayRate, delayQueue, delayTimed:
		interval, err := rateInterval(def.Node.PropFloat("rate", 1),
			def.Node.PropFloat("nbRateUnits", 1), def.Node.PropString("rateUnits", "second"))
		if err != nil {
			return nil, err
		}
		n.interval = interval

	default:
		return nil, fmt.Errorf("unknown action %q", n.mode)
	}

	return n, nil
}

// rateInterval turns "rate messages per nbRateUnits rateUnits" into the gap
// between releases.
func rateInterval(rate, count float64, unit string) (time.Duration, error) {
	u, err := parseRateUnits(unit)
	if err != nil {
		return 0, err
	}
	if rate <= 0 {
		return 0, fmt.Errorf("the rate must be greater than zero, got %v", rate)
	}
	if count <= 0 {
		count = 1
	}
	d := time.Duration(float64(u) * count / rate)
	if d <= 0 {
		return 0, fmt.Errorf("a rate of %v per %v %s is too fast to schedule", rate, count, unit)
	}
	return d, nil
}

// rateLimited reports whether this node queues rather than pauses.
func (n *delayNode) rateLimited() bool {
	return n.mode == delayRate || n.mode == delayQueue || n.mode == delayTimed
}

// Start captures the emitter — the rate limiter's ticker needs one and has no
// message to hang it off — and arms the early release for shutdown.
func (n *delayNode) Start(ctx context.Context, out node.Emitter) error {
	n.mu.Lock()
	n.out = out
	n.mu.Unlock()

	go func() {
		<-ctx.Done()
		// Cancellation happens before the scheduler waits for the graph to go
		// quiet, so everything let go here still reaches its destination. The
		// alternative is Node-RED's: the queue evaporates on redeploy.
		n.once.Do(func() { close(n.release) })
		if held := n.flush(out, 0); held > 0 {
			out.Log(node.LogWarn, "released %d queued message(s) early because the flow is stopping", held)
		}
	}()

	if n.rateLimited() {
		go n.tick(ctx, out)
	}
	return nil
}

func (n *delayNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	n.mu.Lock()
	n.out = out
	n.mu.Unlock()

	// reset and flush are control messages in every mode, checked before
	// anything else so that a reset cannot itself be queued behind the backlog
	// it is meant to clear.
	if _, isReset := m.Data["reset"]; isReset {
		n.reset(out)
		return nil
	}
	if raw, isFlush := m.Data["flush"]; isFlush {
		count := 0
		if f, ok := asFloat(raw); ok && f > 0 {
			count = int(f)
		}
		n.flush(out, count)
		return nil
	}

	switch n.mode {
	case delayFixed:
		return n.pause(n.fixed, m, out)

	case delayVariable:
		d := n.fixed
		if raw, ok := m.Data["delay"]; ok {
			f, ok := asFloat(raw)
			if !ok {
				return fmt.Errorf("msg.delay is %T, want a number of milliseconds", raw)
			}
			if f < 0 {
				return fmt.Errorf("msg.delay must not be negative, got %v", f)
			}
			d = time.Duration(f * float64(time.Millisecond))
		}
		return n.pause(d, m, out)

	case delayRandom:
		d := n.randMin
		if n.randMax > n.randMin {
			d += rand.N(n.randMax - n.randMin)
		}
		return n.pause(d, m, out)

	default:
		return n.limitRate(m, out)
	}
}

// pause holds one message on its own timer.
func (n *delayNode) pause(d time.Duration, m *engine.Msg, out node.Emitter) error {
	if d <= 0 {
		out.Send(0, m)
		return nil
	}

	n.mu.Lock()
	if n.timers >= n.limit {
		n.mu.Unlock()
		return n.overflow(m, out)
	}
	n.timers++
	n.wg.Add(1)
	n.mu.Unlock()

	go func() {
		defer n.wg.Done()
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-n.release:
		}
		n.mu.Lock()
		n.timers--
		n.mu.Unlock()
		out.Send(0, m)
	}()
	return nil
}

// limitRate is the queueing half: rate, queue and timed modes.
//
// The first message of a burst goes straight through, which is what makes a
// rate limiter usable in front of a rarely-firing alarm: the alarm is not held
// for a whole window just because nothing had been sent for an hour.
func (n *delayNode) limitRate(m *engine.Msg, out node.Emitter) error {
	if n.allowRate {
		if raw, ok := m.Data["rate"]; ok {
			f, ok := asFloat(raw)
			if !ok || f <= 0 {
				return fmt.Errorf("msg.rate must be a positive number of milliseconds, got %v", raw)
			}
			n.mu.Lock()
			n.interval = time.Duration(f * float64(time.Millisecond))
			n.mu.Unlock()
		}
	}

	n.mu.Lock()

	if n.mode == delayRate && len(n.queue) == 0 && n.now().Sub(n.lastSent) >= n.interval {
		n.lastSent = n.now()
		n.mu.Unlock()
		out.Send(0, m)
		return nil
	}

	if n.mode == delayQueue {
		// One slot per topic, newest wins. That is the point of the mode: a
		// display showing the latest reading per tag does not want the backlog,
		// and the displaced message is reported rather than vanishing.
		topic := m.Topic()
		prev, exists := n.byTopic[topic]
		if !exists {
			if len(n.byTopic) >= n.limit {
				n.mu.Unlock()
				return n.overflow(m, out)
			}
			n.order = append(n.order, topic)
		}
		n.byTopic[topic] = m
		n.mu.Unlock()
		if prev != nil {
			n.discard(prev, out)
		}
		return nil
	}

	if n.drop {
		// Dropping means the window stays shut: the message is refused now
		// rather than queued and delivered late.
		n.mu.Unlock()
		n.discard(m, out)
		return nil
	}

	if len(n.queue) >= n.limit {
		n.mu.Unlock()
		return n.overflow(m, out)
	}
	n.queue = append(n.queue, m)
	n.mu.Unlock()
	return nil
}

// discard reports a message the rate limiter chose not to send. It leaves by the
// second output when the node has one; otherwise it is logged, because a message
// disappearing without a trace is the failure this codebase exists to avoid.
func (n *delayNode) discard(m *engine.Msg, out node.Emitter) {
	if n.dropPort >= 0 {
		out.Send(n.dropPort, m)
		return
	}
	out.Log(node.LogDebug, "dropped an intermediate message; give the node a second output to keep them")
}

// overflow refuses a message the node has no room for.
func (n *delayNode) overflow(m *engine.Msg, out node.Emitter) error {
	if n.dropPort >= 0 {
		out.Send(n.dropPort, m)
		return nil
	}
	return fmt.Errorf("the delay queue is full at %d messages: raise ew_maxQueue, "+
		"add a second output to catch the overflow, or slow the source down", n.limit)
}

// tick is the rate limiter's clock.
func (n *delayNode) tick(ctx context.Context, out node.Emitter) {
	n.mu.Lock()
	interval := n.interval
	n.mu.Unlock()

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.mu.Lock()
			// msg.rate may have moved it since the last tick.
			if n.interval != interval && n.interval > 0 {
				interval = n.interval
				t.Reset(interval)
			}
			n.mu.Unlock()
			n.releaseDue(out)
		}
	}
}

// releaseDue emits whatever this interval is due to send.
func (n *delayNode) releaseDue(out node.Emitter) {
	n.mu.Lock()
	var due []*engine.Msg

	switch n.mode {
	case delayQueue:
		// Round-robin across topics, skipping any that emptied since they were
		// queued.
		for len(n.order) > 0 {
			topic := n.order[0]
			n.order = n.order[1:]
			m, ok := n.byTopic[topic]
			if !ok {
				continue
			}
			delete(n.byTopic, topic)
			due = append(due, m)
			break
		}

	case delayTimed:
		due, n.queue = n.queue, nil

	default: // delayRate
		if len(n.queue) > 0 {
			due, n.queue = n.queue[:1], n.queue[1:]
		}
	}
	if len(due) > 0 {
		n.lastSent = n.now()
	}
	n.mu.Unlock()

	for _, m := range due {
		out.Send(0, m)
	}
}

// flush releases held messages immediately. count of zero means all of them. It
// returns how many were released.
func (n *delayNode) flush(out node.Emitter, count int) int {
	if out == nil {
		n.mu.Lock()
		out = n.out
		n.mu.Unlock()
	}
	if out == nil {
		return 0
	}

	n.mu.Lock()
	var due []*engine.Msg
	if count <= 0 {
		due, n.queue = n.queue, nil
		for _, topic := range n.order {
			if m, ok := n.byTopic[topic]; ok {
				due = append(due, m)
			}
		}
		n.byTopic = map[string]*engine.Msg{}
		n.order = nil
	} else {
		if count > len(n.queue) {
			count = len(n.queue)
		}
		due, n.queue = n.queue[:count], n.queue[count:]
	}
	n.mu.Unlock()

	for _, m := range due {
		out.Send(0, m)
	}
	return len(due)
}

// reset empties the queue without sending. This is the one place a message is
// dropped on purpose, and it is because the flow asked for it by name.
func (n *delayNode) reset(out node.Emitter) {
	n.mu.Lock()
	dropped := len(n.queue) + len(n.byTopic)
	n.queue = nil
	n.byTopic = map[string]*engine.Msg{}
	n.order = nil
	n.mu.Unlock()
	if dropped > 0 {
		out.Log(node.LogInfo, "msg.reset discarded %d queued message(s)", dropped)
	}
}

// Pending reports the outstanding work, which is what keeps a redeploy from
// completing while this node still holds messages.
func (n *delayNode) Pending() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.timers + len(n.queue) + len(n.byTopic)
}

// Close waits for the timer goroutines started by pause. They have already been
// told to fire early by the context cancellation, so this returns promptly.
func (n *delayNode) Close(ctx context.Context, _ bool) error {
	n.once.Do(func() { close(n.release) })

	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for %d delayed message(s)", n.Pending())
	}
}

// ---------------------------------------------------------------------------
// trigger
// ---------------------------------------------------------------------------

// triggerNode sends one message on arrival and, optionally, a second after a
// delay — the standard shape for a watchdog: emit "on" now, emit "off" unless
// something re-triggers first.
type triggerNode struct {
	op1, op2 TypedValue
	// nul means send nothing; payload means repeat the payload of the message
	// that armed the timer. Neither is a typedInput value type, so both are
	// lifted out of the config here rather than failing at the first message.
	op1Nul, op2Nul         bool
	op1Payload, op2Payload bool

	duration      time.Duration
	extend        bool
	overrideDelay bool
	resetValue    string
	byTopic       bool
	topicProp     string
	secondPort    int
	limit         int

	svc node.Services

	mu      sync.Mutex
	armed   map[string]*armedTrigger
	release chan struct{}
	once    sync.Once
	wg      sync.WaitGroup
}

// armedTrigger is one outstanding timer, keyed by topic when the node groups by
// topic and by "" when it does not.
type armedTrigger struct {
	cancel chan struct{}
	// msg is the message that armed the timer, kept because the second message
	// may need its payload and because its topic has to survive onto it.
	msg *engine.Msg
}

func registerTrigger() {
	node.MustRegister(node.Descriptor{
		Type:         "trigger",
		Category:     node.CategoryFunction,
		Color:        colorFunction,
		Icon:         "timer",
		Inputs:       1,
		Outputs:      1,
		OutputsProp:  "outputs",
		PaletteLabel: "trigger",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Both messages, extend-on-retrigger, wait-to-be-reset, msg.reset, " +
				"the msg.delay override, per-topic grouping and the second output are " +
				"implemented. The divergence is the same as the Delay node's: the " +
				"number of simultaneously armed timers is bounded, and a message past " +
				"the limit is refused to a Catch node rather than silently arming " +
				"another. Timers with a deadline that are still armed when the flow " +
				"stops fire immediately rather than being discarded; a timer waiting " +
				"to be reset is dropped, because firing it would invent an event that " +
				"never happened.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "op1", Kind: node.PropTypedInput, Label: "Send", TypeProp: "op1type",
				TypedInputTypes: triggerValueTypes()},
			{Name: "op2", Kind: node.PropTypedInput, Label: "Then send", TypeProp: "op2type",
				TypedInputTypes: triggerValueTypes()},
			{Name: "duration", Kind: node.PropNumber, Label: "Wait", Default: 250,
				Help: "Zero means wait to be reset: the second message goes out when a reset arrives."},
			{Name: "units", Kind: node.PropSelect, Label: "Wait units", Default: "ms",
				Options: []node.Option{
					{Value: "ms", Label: "milliseconds"},
					{Value: "s", Label: "seconds"},
					{Value: "min", Label: "minutes"},
					{Value: "hr", Label: "hours"},
				}},
			{Name: "extend", Kind: node.PropBool, Label: "Extend the wait if a new message arrives"},
			{Name: "overrideDelay", Kind: node.PropBool, Label: "Allow msg.delay to override the wait"},
			{Name: "reset", Kind: node.PropString, Label: "Reset when the payload equals",
				Help: "msg.reset always resets, whatever this says."},
			{Name: "bytopic", Kind: node.PropSelect, Label: "Handle", Default: "all",
				Options: []node.Option{
					{Value: "all", Label: "All messages as one stream"},
					{Value: "topic", Label: "Each topic independently"},
				}},
			{Name: "topic", Kind: node.PropString, Label: "Group by", Default: "topic"},
			{Name: "outputs", Kind: node.PropNumber, Label: "Outputs", Default: 1,
				Help: "Set to 2 to send the second message out of its own output."},
			{Name: "ew_maxTimers", Kind: node.PropNumber, Label: "Timer limit",
				Help: "How many timers may be armed at once. Emberwire's own; Node-RED has no limit."},
		},
		Help: "Sends a message when one arrives, then optionally a second after a " +
			"delay. Grouping by topic gives every tag its own timer, which is how " +
			"one node watches a hundred sensors for going quiet.",
	}, newTrigger)
}

func triggerValueTypes() []string {
	return []string{node.TypeStr, node.TypeNum, node.TypeBool, node.TypeJSON,
		node.TypeBin, node.TypeDate, node.TypeEnv, node.TypeFlow, node.TypeGlobal}
}

func newTrigger(def *node.Definition) (node.Node, error) {
	n := &triggerNode{
		extend:        def.Node.PropBool("extend", false),
		overrideDelay: def.Node.PropBool("overrideDelay", false),
		resetValue:    def.Node.PropString("reset", ""),
		byTopic:       def.Node.PropString("bytopic", "all") == "topic",
		topicProp:     orDefault(def.Node.PropString("topic", ""), engine.PropTopic),
		limit:         def.Node.PropInt("ew_maxTimers", defaultDeferLimit),
		svc:           def.Services,
		armed:         map[string]*armedTrigger{},
		release:       make(chan struct{}),
	}
	if n.limit <= 0 {
		n.limit = defaultDeferLimit
	}
	if def.Node.PropInt("outputs", 1) >= 2 {
		n.secondPort = 1
	}

	unit, err := parseTimeUnits(def.Node.PropString("units", "ms"))
	if err != nil {
		return nil, err
	}
	n.duration = scaled(def.Node.PropFloat("duration", 250), unit)
	if n.duration < 0 {
		return nil, fmt.Errorf("the wait must not be negative")
	}

	switch def.Node.PropString("op1type", node.TypeStr) {
	case "nul":
		n.op1Nul = true
	case "pay", "payl":
		n.op1Payload = true
	default:
		n.op1 = ReadTypedValue(def.Node.Raw, "op1", "op1type", node.TypeStr)
	}

	switch def.Node.PropString("op2type", node.TypeStr) {
	case "nul":
		n.op2Nul = true
	case "pay", "payl":
		n.op2Payload = true
	default:
		n.op2 = ReadTypedValue(def.Node.Raw, "op2", "op2type", node.TypeStr)
	}

	return n, nil
}

func (n *triggerNode) Start(ctx context.Context, _ node.Emitter) error {
	go func() {
		<-ctx.Done()
		// Same reasoning as the Delay node: fire now rather than lose the second
		// message. A watchdog whose "the sensor went quiet" alarm is swallowed by
		// a redeploy is worse than one that fires a little early.
		n.once.Do(func() { close(n.release) })
	}()
	return nil
}

func (n *triggerNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	key := ""
	if n.byTopic {
		if v, ok, err := m.Get(n.topicProp); err == nil && ok {
			key = mustacheString(v)
		}
	}

	if n.isReset(m) {
		n.disarm(key, out)
		return nil
	}

	duration := n.duration
	if n.overrideDelay {
		if raw, ok := m.Data["delay"]; ok {
			f, ok := asFloat(raw)
			if !ok || f < 0 {
				return fmt.Errorf("msg.delay must be a non-negative number of milliseconds, got %v", raw)
			}
			duration = time.Duration(f * float64(time.Millisecond))
		}
	}

	n.mu.Lock()
	existing, isArmed := n.armed[key]
	atLimit := !isArmed && len(n.armed) >= n.limit
	n.mu.Unlock()

	switch {
	case isArmed && !n.extend:
		// Already running. Node-RED ignores the message entirely, which is what
		// makes a Trigger a debounce rather than a repeater.
		return nil
	case isArmed:
		// Extending: cancel the outstanding timer and arm a fresh one from the
		// new message. The first message is not re-sent.
		n.cancelArmed(key, existing)
		n.arm(key, m, duration, out)
		return nil
	case atLimit:
		return fmt.Errorf("the trigger already has %d timers armed: raise ew_maxTimers "+
			"or group by a coarser topic", n.limit)
	}

	if err := n.sendOp(n.op1, n.op1Nul, n.op1Payload, m, 0, out); err != nil {
		return err
	}
	n.arm(key, m, duration, out)
	return nil
}

// isReset reports whether this message clears an armed timer.
func (n *triggerNode) isReset(m *engine.Msg) bool {
	if _, ok := m.Data["reset"]; ok {
		return true
	}
	if n.resetValue == "" {
		return false
	}
	return mustacheString(m.Payload()) == n.resetValue
}

// arm starts a timer. A zero duration arms it with no deadline, which is the
// wait-to-be-reset mode.
func (n *triggerNode) arm(key string, m *engine.Msg, d time.Duration, out node.Emitter) {
	entry := &armedTrigger{cancel: make(chan struct{}), msg: m.Clone()}

	n.mu.Lock()
	n.armed[key] = entry
	n.wg.Add(1)
	n.mu.Unlock()

	go func() {
		defer n.wg.Done()

		var fire <-chan time.Time
		if d > 0 {
			t := time.NewTimer(d)
			defer t.Stop()
			fire = t.C
		}

		select {
		case <-fire:
		case <-entry.cancel:
			return
		case <-n.release:
			if d == 0 {
				// Nothing to bring forward: this timer was waiting for an event
				// that has not happened. Firing it would fabricate one.
				n.forget(key, entry)
				return
			}
		}

		if !n.forget(key, entry) {
			return
		}
		if err := n.sendOp(n.op2, n.op2Nul, n.op2Payload, entry.msg, n.secondPort, out); err != nil {
			out.Error(err, entry.msg)
		}
	}()
}

// forget removes an armed entry if it is still the current one, reporting
// whether it was. A superseded goroutine loses that race and stays quiet.
func (n *triggerNode) forget(key string, entry *armedTrigger) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.armed[key] != entry {
		return false
	}
	delete(n.armed, key)
	return true
}

func (n *triggerNode) cancelArmed(key string, entry *armedTrigger) {
	n.mu.Lock()
	if n.armed[key] == entry {
		delete(n.armed, key)
	}
	n.mu.Unlock()
	close(entry.cancel)
}

// disarm handles a reset. In wait-to-be-reset mode the reset is what releases
// the second message; otherwise it cancels it.
func (n *triggerNode) disarm(key string, out node.Emitter) {
	n.mu.Lock()
	entry, ok := n.armed[key]
	if ok {
		delete(n.armed, key)
	}
	n.mu.Unlock()
	if !ok {
		return
	}
	close(entry.cancel)

	if n.duration == 0 {
		if err := n.sendOp(n.op2, n.op2Nul, n.op2Payload, entry.msg, n.secondPort, out); err != nil {
			out.Error(err, entry.msg)
		}
	}
}

// sendOp emits one of the two configured messages, built from the message that
// armed the trigger.
func (n *triggerNode) sendOp(tv TypedValue, nul, keepPayload bool, src *engine.Msg, port int, out node.Emitter) error {
	if nul {
		return nil
	}

	m := src.Clone()
	if !keepPayload {
		v, ok, err := tv.Eval(EvalContext{Msg: src, Services: n.svc})
		if err != nil {
			return err
		}
		if !ok {
			// The reference resolved to nothing. Passing the original payload
			// through would misrepresent what the node was told to send.
			return nil
		}
		m.SetPayload(v)
	}
	// A fresh id: this is a new message, and reusing the trigger's would make
	// the debug sidebar report one message arriving twice.
	m.Data[engine.PropMsgID] = engine.GenerateID()
	out.Send(port, m)
	return nil
}

// Pending reports armed timers, so a redeploy waits for them.
func (n *triggerNode) Pending() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.armed)
}

func (n *triggerNode) Close(ctx context.Context, _ bool) error {
	n.once.Do(func() { close(n.release) })

	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("timed out waiting for %d armed timer(s)", n.Pending())
	}
}
