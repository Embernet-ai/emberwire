package nodes

import (
	"context"
	"testing"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

// startNode runs a node's Start method against a cancellable context, returning
// the emitter it was given and the cancel func. Tests that exercise timers need
// both: cancellation is how the runtime tells a Delay node to let go early.
func startNode(t *testing.T, n node.Node, e *testEmitter) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if s, ok := n.(node.Starter); ok {
		if err := s.Start(ctx, e); err != nil {
			cancel()
			t.Fatalf("start: %v", err)
		}
	}
	return ctx, cancel
}

// pushTo runs a message through a node against a specific emitter, so a test can
// watch several messages arrive on the same one.
func pushTo(t *testing.T, n node.Node, e *testEmitter, m *engine.Msg) error {
	t.Helper()
	return n.Receive(context.Background(), m, e)
}

// waitFor polls until cond holds or the deadline passes. Timer tests need a
// bound rather than a sleep: a fixed sleep is either flaky or slow, and on a
// loaded CI box it is both.
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// ---------------------------------------------------------------------------
// delay
// ---------------------------------------------------------------------------

func TestDelayHoldsThenReleases(t *testing.T) {
	n := build(t, "delay",
		`{"pauseType":"delay","timeout":40,"timeoutUnits":"milliseconds"}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{"payload":1}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if e.total() != 0 {
		t.Fatal("the message was not held at all")
	}
	if p := n.(node.Deferrer).Pending(); p != 1 {
		t.Fatalf("Pending = %d, want 1", p)
	}

	waitFor(t, 2*time.Second, "the delayed message", func() bool { return e.total() == 1 })
	if got := e.on(0)[0].Payload(); got != float64(1) {
		t.Fatalf("payload = %v", got)
	}
	waitFor(t, time.Second, "Pending to fall back to zero",
		func() bool { return n.(node.Deferrer).Pending() == 0 })
}

func TestDelayVariableReadsMsgDelay(t *testing.T) {
	n := build(t, "delay",
		`{"pauseType":"delayv","timeout":60,"timeoutUnits":"minutes"}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	// The configured hour must lose to msg.delay, or a flow computing its own
	// pause would silently wait until nobody is looking.
	if err := pushTo(t, n, e, msg(t, `{"payload":1,"delay":20}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	waitFor(t, 2*time.Second, "the message delayed by msg.delay", func() bool { return e.total() == 1 })
}

func TestDelayVariableRefusesANonNumericDelay(t *testing.T) {
	n := build(t, "delay", `{"pauseType":"delayv"}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{"payload":1,"delay":"soon"}`)); err == nil {
		t.Fatal("expected an error rather than a silently ignored msg.delay")
	}
}

// The first message of a burst goes straight through. Holding it for a whole
// window makes a rate limiter unusable in front of a rarely-firing alarm.
func TestDelayRateLetsTheFirstMessageStraightThrough(t *testing.T) {
	n := build(t, "delay",
		`{"pauseType":"rate","rate":1,"nbRateUnits":1,"rateUnits":"minute"}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{"payload":"first"}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if e.total() != 1 {
		t.Fatalf("the first message was held; sent %d", e.total())
	}

	if err := pushTo(t, n, e, msg(t, `{"payload":"second"}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if e.total() != 1 {
		t.Fatalf("the second message was not rate limited; sent %d", e.total())
	}
	if p := n.(node.Deferrer).Pending(); p != 1 {
		t.Fatalf("Pending = %d, want the queued message to be counted", p)
	}
}

func TestDelayRateWithDropDiscardsToTheSecondOutput(t *testing.T) {
	n := build(t, "delay",
		`{"pauseType":"rate","rate":1,"nbRateUnits":1,"rateUnits":"minute","drop":true,"outputs":2}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	for _, p := range []string{"first", "second", "third"} {
		cfg, err := jsonConfig(map[string]any{"payload": p})
		if err != nil {
			t.Fatal(err)
		}
		if err := pushTo(t, n, e, msg(t, cfg)); err != nil {
			t.Fatalf("receive: %v", err)
		}
	}

	if got := len(e.on(0)); got != 1 {
		t.Fatalf("sent %d messages on the main output, want 1", got)
	}
	// The two the limiter refused are reported rather than vanishing, which is
	// the whole reason for the second output.
	if got := len(e.on(1)); got != 2 {
		t.Fatalf("sent %d messages on the drop output, want 2", got)
	}
	if p := n.(node.Deferrer).Pending(); p != 0 {
		t.Fatalf("Pending = %d; dropping must not queue", p)
	}
}

func TestDelayQueueKeepsTheNewestPerTopic(t *testing.T) {
	n := build(t, "delay",
		`{"pauseType":"queue","rate":1,"nbRateUnits":1,"rateUnits":"minute","outputs":2}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	for _, cfg := range []string{
		`{"topic":"a","payload":1}`,
		`{"topic":"b","payload":2}`,
		`{"topic":"a","payload":3}`,
	} {
		if err := pushTo(t, n, e, msg(t, cfg)); err != nil {
			t.Fatalf("receive: %v", err)
		}
	}

	if p := n.(node.Deferrer).Pending(); p != 2 {
		t.Fatalf("Pending = %d, want one slot per topic", p)
	}
	// The superseded reading for topic a is announced, not silently forgotten.
	if got := len(e.on(1)); got != 1 {
		t.Fatalf("displaced messages reported: %d, want 1", got)
	}

	// Flushing releases what is held, and topic a must carry the later value.
	if err := pushTo(t, n, e, msg(t, `{"flush":true}`)); err != nil {
		t.Fatalf("flush: %v", err)
	}
	sent := e.on(0)
	if len(sent) != 2 {
		t.Fatalf("flush released %d messages, want 2", len(sent))
	}
	byTopic := map[string]any{}
	for _, m := range sent {
		byTopic[m.Topic()] = m.Payload()
	}
	if byTopic["a"] != float64(3) {
		t.Errorf("topic a released %v, want the newest value 3", byTopic["a"])
	}
	if byTopic["b"] != float64(2) {
		t.Errorf("topic b released %v", byTopic["b"])
	}
}

func TestDelayTimedReleasesEverythingOnTheInterval(t *testing.T) {
	n := build(t, "delay",
		`{"pauseType":"timed","rate":25,"nbRateUnits":1,"rateUnits":"second"}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	for i := range 3 {
		cfg, err := jsonConfig(map[string]any{"payload": i})
		if err != nil {
			t.Fatal(err)
		}
		if err := pushTo(t, n, e, msg(t, cfg)); err != nil {
			t.Fatalf("receive: %v", err)
		}
	}
	if e.total() != 0 {
		t.Fatal("timed mode released something before its interval")
	}
	waitFor(t, 5*time.Second, "the whole batch", func() bool { return e.total() == 3 })
}

func TestDelayResetDiscardsAndFlushReleases(t *testing.T) {
	newNode := func(t *testing.T) (node.Node, *testEmitter, context.CancelFunc) {
		t.Helper()
		n := build(t, "delay",
			`{"pauseType":"rate","rate":1,"nbRateUnits":1,"rateUnits":"minute"}`, newTestServices())
		e := newTestEmitter()
		_, cancel := startNode(t, n, e)
		// The first message goes straight through, so queue three and expect two
		// to be held.
		for i := range 3 {
			cfg, err := jsonConfig(map[string]any{"payload": i})
			if err != nil {
				t.Fatal(err)
			}
			if err := pushTo(t, n, e, msg(t, cfg)); err != nil {
				t.Fatalf("receive: %v", err)
			}
		}
		if p := n.(node.Deferrer).Pending(); p != 2 {
			t.Fatalf("Pending = %d, want 2", p)
		}
		return n, e, cancel
	}

	t.Run("reset", func(t *testing.T) {
		n, e, cancel := newNode(t)
		defer cancel()
		if err := pushTo(t, n, e, msg(t, `{"reset":true}`)); err != nil {
			t.Fatalf("reset: %v", err)
		}
		if p := n.(node.Deferrer).Pending(); p != 0 {
			t.Fatalf("Pending = %d after reset", p)
		}
		if e.total() != 1 {
			t.Fatalf("reset released %d messages; it must discard them", e.total()-1)
		}
	})

	t.Run("flush releases a count", func(t *testing.T) {
		n, e, cancel := newNode(t)
		defer cancel()
		if err := pushTo(t, n, e, msg(t, `{"flush":1}`)); err != nil {
			t.Fatalf("flush: %v", err)
		}
		if p := n.(node.Deferrer).Pending(); p != 1 {
			t.Fatalf("Pending = %d, want 1 left after flushing one", p)
		}
		if e.total() != 2 {
			t.Fatalf("sent %d, want the first plus the one flushed", e.total())
		}
	})
}

// Node-RED's queue is unbounded: a source faster than the drain grows the heap
// until the pod is OOM-killed, with nothing in the log to explain it.
func TestDelayRefusesPastItsQueueLimit(t *testing.T) {
	n := build(t, "delay",
		`{"pauseType":"rate","rate":1,"nbRateUnits":1,"rateUnits":"minute","ew_maxQueue":2}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	// One through, two queued, then the refusal.
	var lastErr error
	for i := range 4 {
		cfg, err := jsonConfig(map[string]any{"payload": i})
		if err != nil {
			t.Fatal(err)
		}
		lastErr = pushTo(t, n, e, msg(t, cfg))
	}
	if lastErr == nil {
		t.Fatal("the fourth message was accepted past the limit")
	}
	if p := n.(node.Deferrer).Pending(); p != 2 {
		t.Fatalf("Pending = %d, want the limit of 2", p)
	}
}

// A redeploy must not evaporate a Delay node's contents the way Node-RED's does.
// Cancellation happens before the scheduler waits for the graph to go quiet, so
// what is released here still reaches its destination.
func TestDelayReleasesHeldMessagesWhenTheFlowStops(t *testing.T) {
	t.Run("individual timers", func(t *testing.T) {
		n := build(t, "delay",
			`{"pauseType":"delay","timeout":1,"timeoutUnits":"hours"}`, newTestServices())
		e := newTestEmitter()
		_, cancel := startNode(t, n, e)

		if err := pushTo(t, n, e, msg(t, `{"payload":"held"}`)); err != nil {
			t.Fatalf("receive: %v", err)
		}
		cancel()
		waitFor(t, 2*time.Second, "the held message to be released",
			func() bool { return e.total() == 1 })
	})

	t.Run("the rate limiter queue", func(t *testing.T) {
		n := build(t, "delay",
			`{"pauseType":"rate","rate":1,"nbRateUnits":1,"rateUnits":"hour"}`, newTestServices())
		e := newTestEmitter()
		_, cancel := startNode(t, n, e)

		for i := range 3 {
			cfg, err := jsonConfig(map[string]any{"payload": i})
			if err != nil {
				t.Fatal(err)
			}
			if err := pushTo(t, n, e, msg(t, cfg)); err != nil {
				t.Fatalf("receive: %v", err)
			}
		}
		cancel()
		waitFor(t, 2*time.Second, "the queue to drain", func() bool { return e.total() == 3 })
	})
}

func TestDelayRefusesBadConfiguration(t *testing.T) {
	cases := map[string]string{
		"unknown action":  `{"pauseType":"eventually"}`,
		"unknown units":   `{"pauseType":"delay","timeoutUnits":"fortnights"}`,
		"negative delay":  `{"pauseType":"delay","timeout":-1}`,
		"zero rate":       `{"pauseType":"rate","rate":0}`,
		"unknown window":  `{"pauseType":"rate","rate":1,"rateUnits":"week"}`,
		"negative random": `{"pauseType":"random","randomFirst":-2,"randomLast":-1}`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			if err := buildErr(t, "delay", cfg, newTestServices()); err == nil {
				t.Fatal("expected the node to refuse to build")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// trigger
// ---------------------------------------------------------------------------

func TestTriggerSendsBothMessages(t *testing.T) {
	n := build(t, "trigger",
		`{"op1":"on","op1type":"str","op2":"off","op2type":"str","duration":30,"units":"ms"}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{"topic":"pump"}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	if got := len(e.on(0)); got != 1 || e.on(0)[0].Payload() != "on" {
		t.Fatalf("first message: %d sent, payload %v", got, e.on(0)[0].Payload())
	}

	waitFor(t, 2*time.Second, "the second message", func() bool { return len(e.on(0)) == 2 })
	second := e.on(0)[1]
	if second.Payload() != "off" {
		t.Errorf("second payload = %v", second.Payload())
	}
	// The topic has to survive, or a per-topic downstream cannot tell which
	// sensor went quiet.
	if second.Topic() != "pump" {
		t.Errorf("second topic = %q, want it carried over", second.Topic())
	}
	if second.ID() == e.on(0)[0].ID() {
		t.Error("the two messages share an id; the debug sidebar would report one message twice")
	}
}

func TestTriggerSecondOutput(t *testing.T) {
	n := build(t, "trigger",
		`{"op1":"on","op1type":"str","op2":"off","op2type":"str","duration":20,"units":"ms","outputs":2}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	waitFor(t, 2*time.Second, "the second message on its own output",
		func() bool { return len(e.on(1)) == 1 })
	if len(e.on(0)) != 1 {
		t.Fatalf("main output got %d messages, want only the first", len(e.on(0)))
	}
}

// A trigger already running ignores further messages. That is what makes it a
// debounce rather than a repeater.
func TestTriggerIgnoresRetriggerUnlessExtending(t *testing.T) {
	n := build(t, "trigger",
		`{"op1":"on","op1type":"str","op2type":"nul","duration":1,"units":"hr"}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	for range 3 {
		if err := pushTo(t, n, e, msg(t, `{}`)); err != nil {
			t.Fatalf("receive: %v", err)
		}
	}
	if got := len(e.on(0)); got != 1 {
		t.Fatalf("sent %d first messages, want 1", got)
	}
	if p := n.(node.Deferrer).Pending(); p != 1 {
		t.Fatalf("Pending = %d, want one armed timer", p)
	}
}

func TestTriggerExtendRestartsTheTimer(t *testing.T) {
	n := build(t, "trigger",
		`{"op1type":"nul","op2":"quiet","op2type":"str","duration":80,"units":"ms","extend":true}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	// Retrigger faster than the timeout for a while; nothing must fire.
	deadline := time.Now().Add(240 * time.Millisecond)
	for time.Now().Before(deadline) {
		if err := pushTo(t, n, e, msg(t, `{}`)); err != nil {
			t.Fatalf("receive: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e.total() != 0 {
		t.Fatalf("the timer fired %d time(s) while being extended", e.total())
	}

	// Stop feeding it and the watchdog goes off.
	waitFor(t, 3*time.Second, "the watchdog to fire once fed nothing",
		func() bool { return e.total() == 1 })
}

func TestTriggerReset(t *testing.T) {
	t.Run("msg.reset cancels the second message", func(t *testing.T) {
		n := build(t, "trigger",
			`{"op1type":"nul","op2":"off","op2type":"str","duration":60,"units":"ms"}`,
			newTestServices())
		e := newTestEmitter()
		_, cancel := startNode(t, n, e)
		defer cancel()

		if err := pushTo(t, n, e, msg(t, `{}`)); err != nil {
			t.Fatalf("receive: %v", err)
		}
		if err := pushTo(t, n, e, msg(t, `{"reset":true}`)); err != nil {
			t.Fatalf("reset: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if e.total() != 0 {
			t.Fatalf("the reset did not cancel the timer; sent %d", e.total())
		}
		if p := n.(node.Deferrer).Pending(); p != 0 {
			t.Fatalf("Pending = %d after a reset", p)
		}
	})

	t.Run("a matching payload resets", func(t *testing.T) {
		n := build(t, "trigger",
			`{"op1type":"nul","op2":"off","op2type":"str","duration":60,"units":"ms","reset":"stop"}`,
			newTestServices())
		e := newTestEmitter()
		_, cancel := startNode(t, n, e)
		defer cancel()

		if err := pushTo(t, n, e, msg(t, `{"payload":"go"}`)); err != nil {
			t.Fatalf("receive: %v", err)
		}
		if err := pushTo(t, n, e, msg(t, `{"payload":"stop"}`)); err != nil {
			t.Fatalf("reset: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
		if e.total() != 0 {
			t.Fatalf("the payload reset did not take; sent %d", e.total())
		}
	})

	t.Run("a zero wait sends the second message on reset", func(t *testing.T) {
		n := build(t, "trigger",
			`{"op1":"on","op1type":"str","op2":"off","op2type":"str","duration":0}`,
			newTestServices())
		e := newTestEmitter()
		_, cancel := startNode(t, n, e)
		defer cancel()

		if err := pushTo(t, n, e, msg(t, `{}`)); err != nil {
			t.Fatalf("receive: %v", err)
		}
		if len(e.on(0)) != 1 {
			t.Fatalf("first message: %d sent", len(e.on(0)))
		}
		if err := pushTo(t, n, e, msg(t, `{"reset":true}`)); err != nil {
			t.Fatalf("reset: %v", err)
		}
		if len(e.on(0)) != 2 || e.on(0)[1].Payload() != "off" {
			t.Fatalf("the reset did not release the second message: %d sent", len(e.on(0)))
		}
	})
}

func TestTriggerGroupsByTopic(t *testing.T) {
	n := build(t, "trigger",
		`{"op1":"on","op1type":"str","op2type":"nul","duration":1,"units":"hr","bytopic":"topic"}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	for _, cfg := range []string{
		`{"topic":"a"}`, `{"topic":"b"}`, `{"topic":"a"}`, `{"topic":"c"}`,
	} {
		if err := pushTo(t, n, e, msg(t, cfg)); err != nil {
			t.Fatalf("receive: %v", err)
		}
	}
	// Three distinct topics, so three first messages and three armed timers.
	// The repeat of topic a is swallowed by its own timer, not by b's.
	if got := len(e.on(0)); got != 3 {
		t.Fatalf("sent %d first messages, want one per topic", got)
	}
	if p := n.(node.Deferrer).Pending(); p != 3 {
		t.Fatalf("Pending = %d, want one timer per topic", p)
	}
}

func TestTriggerRepeatsThePayloadWhenAsked(t *testing.T) {
	n := build(t, "trigger",
		`{"op1type":"nul","op2type":"payl","duration":20,"units":"ms"}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{"payload":{"reading":9}}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	waitFor(t, 2*time.Second, "the second message", func() bool { return e.total() == 1 })

	obj, ok := e.on(0)[0].Payload().(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want the original object", e.on(0)[0].Payload())
	}
	if obj["reading"] != float64(9) {
		t.Fatalf("payload = %#v", obj)
	}
}

func TestTriggerMsgDelayOverride(t *testing.T) {
	n := build(t, "trigger",
		`{"op1type":"nul","op2":"off","op2type":"str","duration":1,"units":"hr","overrideDelay":true}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{"delay":25}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	waitFor(t, 2*time.Second, "the overridden wait to elapse", func() bool { return e.total() == 1 })
}

func TestTriggerRefusesPastItsTimerLimit(t *testing.T) {
	n := build(t, "trigger",
		`{"op1type":"nul","op2type":"nul","duration":1,"units":"hr","bytopic":"topic","ew_maxTimers":2}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	for _, cfg := range []string{`{"topic":"a"}`, `{"topic":"b"}`} {
		if err := pushTo(t, n, e, msg(t, cfg)); err != nil {
			t.Fatalf("receive: %v", err)
		}
	}
	if err := pushTo(t, n, e, msg(t, `{"topic":"c"}`)); err == nil {
		t.Fatal("a third topic armed a timer past the limit")
	}
}

// A watchdog whose alarm is swallowed by a redeploy is worse than one that fires
// a little early.
func TestTriggerFiresArmedTimersWhenTheFlowStops(t *testing.T) {
	n := build(t, "trigger",
		`{"op1type":"nul","op2":"off","op2type":"str","duration":1,"units":"hr"}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)

	if err := pushTo(t, n, e, msg(t, `{}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	cancel()
	waitFor(t, 2*time.Second, "the armed timer to fire early",
		func() bool { return e.total() == 1 })
}

// A timer waiting to be reset has no deadline to bring forward. Firing it on
// shutdown would invent an event that never happened.
func TestTriggerDropsAWaitingTimerWhenTheFlowStops(t *testing.T) {
	n := build(t, "trigger",
		`{"op1type":"nul","op2":"off","op2type":"str","duration":0}`, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)

	if err := pushTo(t, n, e, msg(t, `{}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	cancel()
	waitFor(t, 2*time.Second, "the waiting timer to be dropped",
		func() bool { return n.(node.Deferrer).Pending() == 0 })
	if e.total() != 0 {
		t.Fatalf("a fabricated event was emitted: %d message(s)", e.total())
	}
}

func TestTriggerCloseWaitsForItsGoroutines(t *testing.T) {
	n := build(t, "trigger",
		`{"op1type":"nul","op2":"off","op2type":"str","duration":1,"units":"hr"}`,
		newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if err := n.(node.Closer).Close(ctx, false); err != nil {
		t.Fatalf("close: %v", err)
	}
	if p := n.(node.Deferrer).Pending(); p != 0 {
		t.Fatalf("Pending = %d after Close", p)
	}
}
