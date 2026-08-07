package nodes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/js"
	"github.com/embernet-ai/emberwire/internal/node"
)

// jsNode builds a function node from a JavaScript body.
//
// The body is JSON-quoted rather than interpolated, so a test can contain
// quotes, newlines and backslashes without the harness mangling them.
func jsNode(t *testing.T, body string, svc node.Services) node.Node {
	t.Helper()
	quoted, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("quoting body: %v", err)
	}
	return build(t, "function", `{"func":`+string(quoted)+`,"outputs":1,"timeout":5}`, svc)
}

func TestFunctionBasics(t *testing.T) {
	svc := newTestServices()
	n := jsNode(t, `msg.payload = msg.payload * 2; return msg;`, svc)

	e, err := send(t, n, msg(t, `{"payload":21}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != 42.0 {
		t.Errorf("payload = %#v, want 42", got)
	}
}

func TestFunctionReturningNothingStopsTheMessage(t *testing.T) {
	svc := newTestServices()
	n := jsNode(t, `if (msg.payload < 10) { return null; } return msg;`, svc)

	if e, _ := send(t, n, msg(t, `{"payload":5}`)); e.total() != 0 {
		t.Error("returning null still emitted a message")
	}
	if e, _ := send(t, n, msg(t, `{"payload":50}`)); e.total() != 1 {
		t.Error("returning the message did not emit it")
	}
}

func TestFunctionMultipleOutputs(t *testing.T) {
	svc := newTestServices()
	n := build(t, "function", `{
        "func":"return [{payload:'a'}, null, {payload:'c'}];",
        "outputs":3,"timeout":5
    }`, svc)

	e, err := send(t, n, msg(t, `{"payload":1}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(e.on(0)) != 1 || e.on(0)[0].Payload() != "a" {
		t.Errorf("port 0 = %#v", e.on(0))
	}
	if len(e.on(1)) != 0 {
		t.Error("a null array entry still sent a message")
	}
	if len(e.on(2)) != 1 || e.on(2)[0].Payload() != "c" {
		t.Errorf("port 2 = %#v", e.on(2))
	}
}

func TestFunctionSendsSeveralOnOnePort(t *testing.T) {
	svc := newTestServices()
	n := build(t, "function", `{
        "func":"return [[{payload:1},{payload:2},{payload:3}]];",
        "outputs":1,"timeout":5
    }`, svc)
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(e.on(0)) != 3 {
		t.Errorf("emitted %d messages, want 3", len(e.on(0)))
	}
}

func TestFunctionNodeSendAPI(t *testing.T) {
	svc := newTestServices()
	n := jsNode(t, `node.send({payload:'sent'}); node.done(); return null;`, svc)

	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(e.on(0)) != 1 || e.on(0)[0].Payload() != "sent" {
		t.Errorf("node.send did not emit: %#v", e.on(0))
	}
	if e.dones != 1 {
		t.Errorf("node.done() produced %d completions, want 1", e.dones)
	}
}

func TestFunctionStatusAndLog(t *testing.T) {
	svc := newTestServices()
	n := jsNode(t, `
        node.status({fill:'green', shape:'dot', text:'ok'});
        node.warn('careful');
        return msg;
    `, svc)

	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(e.statuses) != 1 || e.statuses[0].Text != "ok" || e.statuses[0].Fill != "green" {
		t.Errorf("statuses = %#v", e.statuses)
	}
}

func TestFunctionErrorsAreReported(t *testing.T) {
	svc := newTestServices()
	n := jsNode(t, `throw new Error('deliberate failure');`, svc)

	_, err := send(t, n, msg(t, `{}`))
	if err == nil {
		t.Fatal("a thrown error was swallowed")
	}
	if !strings.Contains(err.Error(), "deliberate failure") {
		t.Errorf("error = %q, want the thrown message", err)
	}
	// The wrapper the body is compiled into must not leak into the message.
	if strings.Contains(err.Error(), "function(msg, node") {
		t.Errorf("error exposes the compilation wrapper: %q", err)
	}
}

func TestFunctionEmitsBeforeThrowing(t *testing.T) {
	// A function that sends two messages and then fails should not have those
	// two discarded — they already happened.
	svc := newTestServices()
	n := jsNode(t, `
        node.send({payload:'first'});
        node.send({payload:'second'});
        throw new Error('after sending');
    `, svc)

	e, err := send(t, n, msg(t, `{}`))
	if err == nil {
		t.Fatal("the error was swallowed")
	}
	if len(e.on(0)) != 2 {
		t.Errorf("emitted %d messages before the throw, want 2", len(e.on(0)))
	}
}

func TestFunctionRejectsBareScalarReturn(t *testing.T) {
	// Node-RED errors here because the author almost certainly meant to return
	// a message, and the silent alternative is a payload-less message appearing
	// downstream.
	svc := newTestServices()
	for _, body := range []string{`return 42;`, `return "hello";`, `return true;`} {
		n := jsNode(t, body, svc)
		if _, err := send(t, n, msg(t, `{}`)); err == nil {
			t.Errorf("%s was accepted, want an error", body)
		}
	}
}

func TestFunctionSyntaxErrorFailsAtBuild(t *testing.T) {
	// Not at the first message. A flow with a typo in a function should refuse
	// to start that node rather than erroring once per message forever.
	svc := newTestServices()
	err := buildErr(t, "function", `{"func":"return msg","outputs":1,"timeout":5,"initialize":"this is ( not js"}`, svc)
	if err == nil {
		t.Fatal("a syntax error in the setup code was accepted at build time")
	}
}

// ---------------------------------------------------------------------------
// The security claims
// ---------------------------------------------------------------------------

// TestFunctionHasNoHostAccess is the reason for choosing goja.
//
// Node's own documentation says the vm module "is not a security mechanism" and
// must not be used to run untrusted code; escapes work by reaching a Function
// constructor and walking out to process and child_process. goja has no host
// bindings at all unless they are added, so there is nothing to reach. These
// assertions pin that: if somebody later registers a convenience global, this
// fails.
func TestFunctionHasNoHostAccess(t *testing.T) {
	svc := newTestServices()

	// Note: `global` is deliberately NOT in this list. In a flow, global is the
	// global context store, exactly as in Node-RED — it is a provided API, not
	// an escape. What must not be reachable is Node's host environment.
	forbidden := []string{
		"require", "process", "module", "exports", "__dirname", "__filename",
		"globalThis.process", "Buffer", "setImmediate",
		// Timers are deliberately absent: work scheduled inside a function is
		// invisible to the scheduler, cannot be back-pressured and outlives the
		// message. A Delay or Trigger node does the same thing accountably.
		"setTimeout", "setInterval",
	}

	for _, name := range forbidden {
		t.Run(name, func(t *testing.T) {
			n := jsNode(t, `msg.payload = (typeof `+name+`); return msg;`, svc)
			e, err := send(t, n, msg(t, `{}`))
			if err != nil {
				// A ReferenceError is also a pass: the identifier does not exist.
				if strings.Contains(err.Error(), "not defined") {
					return
				}
				t.Fatalf("Receive: %v", err)
			}
			if got := e.on(0)[0].Payload(); got != "undefined" {
				t.Errorf("%s is reachable from a function (typeof = %v)", name, got)
			}
		})
	}
}

func TestFunctionCannotEscapeViaConstructor(t *testing.T) {
	// The standard vm escape: reach Function through a constructor chain and
	// evaluate in the host realm. In goja there is no host realm to reach — the
	// worst case is more JavaScript.
	svc := newTestServices()
	n := jsNode(t, `
        var F = (function(){}).constructor;
        var probe = F('try { return typeof process; } catch (e) { return "blocked"; }')();
        msg.payload = probe;
        return msg;
    `, svc)

	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		return // an error is also an acceptable outcome
	}
	got := e.on(0)[0].Payload()
	if got != "undefined" && got != "blocked" {
		t.Errorf("constructor escape reached the host: typeof process = %v", got)
	}
}

func TestFunctionInfiniteLoopIsStopped(t *testing.T) {
	// The claim that matters operationally. Node-RED's function timeout is
	// optional and off by default, so `while(true){}` spins a core until the
	// pod is OOM-killed or somebody notices. Here there is always a ceiling.
	svc := newTestServices()
	n := build(t, "function", `{"func":"while(true){}","outputs":1,"timeout":1}`, svc)

	done := make(chan error, 1)
	go func() {
		_, err := send(t, n, msg(t, `{}`))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an infinite loop returned without error")
		}
		if !strings.Contains(err.Error(), "timed out") {
			t.Errorf("error = %q, want a timeout", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("an infinite loop was not interrupted; it would hold a runtime goroutine forever")
	}
}

func TestFunctionRespectsContextCancellation(t *testing.T) {
	// A shutdown must not wait for a long-running function.
	svc := newTestServices()
	n := build(t, "function", `{"func":"while(true){}","outputs":1,"timeout":60}`, svc)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	done := make(chan error, 1)
	go func() {
		done <- n.Receive(ctx, engine.NewMsg(), newTestEmitter())
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("cancellation did not stop the function")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancellation did not interrupt the function")
	}
}

// ---------------------------------------------------------------------------
// Context and environment
// ---------------------------------------------------------------------------

func TestFunctionContextAccess(t *testing.T) {
	svc := newTestServices()
	n := jsNode(t, `
        var count = flow.get('count') || 0;
        count = count + 1;
        flow.set('count', count);
        msg.payload = count;
        return msg;
    `, svc)

	for i := 1; i <= 3; i++ {
		e, err := send(t, n, msg(t, `{}`))
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got := e.on(0)[0].Payload(); got != float64(i) {
			t.Errorf("message %d payload = %#v, want %d", i, got, i)
		}
	}

	// And it is the real store, visible to other nodes.
	v, ok, _ := svc.Context(node.ScopeFlow).Get("count")
	if !ok || v != 3.0 {
		t.Errorf("flow context count = %#v (ok=%v), want 3", v, ok)
	}
}

func TestFunctionAtomicContextHelpers(t *testing.T) {
	// Not in Node-RED. Exposed because get-modify-set on a shared counter loses
	// updates and Node-RED offers no primitive to fix it.
	svc := newTestServices()
	n := jsNode(t, `msg.payload = global.incr('hits', 1); return msg;`, svc)

	for i := 1; i <= 5; i++ {
		e, err := send(t, n, msg(t, `{}`))
		if err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
		if got := e.on(0)[0].Payload(); got != float64(i) {
			t.Errorf("incr returned %#v on call %d", got, i)
		}
	}

	cas := jsNode(t, `msg.payload = global.cas('leader', null, 'me'); return msg;`, svc)
	e, _ := send(t, cas, msg(t, `{}`))
	if e.on(0)[0].Payload() != true {
		t.Error("first cas claim failed")
	}
	e2, _ := send(t, cas, msg(t, `{}`))
	if e2.on(0)[0].Payload() != false {
		t.Error("second cas claim succeeded; the key was already taken")
	}
}

func TestFunctionEnvAccess(t *testing.T) {
	svc := newTestServices()
	svc.env["CELL_ID"] = "press-01"
	n := jsNode(t, `msg.payload = env.get('CELL_ID'); return msg;`, svc)

	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != "press-01" {
		t.Errorf("env.get = %#v", got)
	}
}

func TestFunctionUtilHelpers(t *testing.T) {
	svc := newTestServices()
	n := jsNode(t, `
        msg.encoded = util.base64Encode('hello');
        msg.decoded = util.base64Decode(msg.encoded);
        msg.id = util.generateId();
        msg.nested = util.getProperty({a:{b:7}}, 'a.b');
        return msg;
    `, svc)

	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	out := e.on(0)[0]
	if v, _, _ := out.Get("encoded"); v != "aGVsbG8=" {
		t.Errorf("base64Encode = %#v", v)
	}
	if v, _, _ := out.Get("decoded"); v != "hello" {
		t.Errorf("base64Decode = %#v", v)
	}
	if v, _, _ := out.Get("nested"); v != 7.0 {
		t.Errorf("getProperty = %#v", v)
	}
	if v, _, _ := out.Get("id"); v == "" {
		t.Error("generateId returned nothing")
	}
}

func TestFunctionLifecycleCode(t *testing.T) {
	svc := newTestServices()
	n := build(t, "function", `{
        "func":"msg.payload = flow.get('startedAt'); return msg;",
        "initialize":"flow.set('startedAt', 'yes');",
        "finalize":"flow.set('stopped', 'yes');",
        "outputs":1,"timeout":5
    }`, svc)

	starter, ok := n.(node.Starter)
	if !ok {
		t.Fatal("the function node does not implement Starter")
	}
	if err := starter.Start(context.Background(), newTestEmitter()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != "yes" {
		t.Errorf("setup code did not run: payload = %#v", got)
	}

	closer, ok := n.(node.Closer)
	if !ok {
		t.Fatal("the function node does not implement Closer")
	}
	if err := closer.Close(context.Background(), false); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if v, ok, _ := svc.Context(node.ScopeFlow).Get("stopped"); !ok || v != "yes" {
		t.Error("cleanup code did not run")
	}
}

func TestFunctionPreservesMessageId(t *testing.T) {
	// A function that builds a fresh object is the common case, and tracing has
	// to survive it or the debug sidebar cannot correlate anything.
	svc := newTestServices()
	n := jsNode(t, `return {payload: 'rebuilt'};`, svc)

	in := msg(t, `{"payload":1}`)
	originalID := in.ID()
	e, err := send(t, n, in)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if got := e.on(0)[0].ID(); got != originalID {
		t.Errorf("message id = %q, want the original %q", got, originalID)
	}
}

func TestFunctionOutputSizeIsBounded(t *testing.T) {
	// A function building an ever-growing array is a slow OOM otherwise.
	prog, err := js.Compile("test",
		`var big = []; for (var i = 0; i < 200000; i++) { big.push('xxxxxxxxxxxxxxxxxxxx'); } return {payload: big};`,
		"", "", js.Limits{Timeout: 20 * time.Second, MaxOutputBytes: 64 * 1024})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	_, err = prog.Run(context.Background(), js.Sandbox{Msg: engine.NewMsg(), Outputs: 1})
	if err == nil {
		t.Fatal("an oversized result was accepted")
	}
	if !strings.Contains(err.Error(), "bytes") {
		t.Errorf("error = %q, want it to mention the size limit", err)
	}
}

func BenchmarkFunctionSimple(b *testing.B) {
	prog, err := js.Compile("bench", "msg.payload = msg.payload * 2; return msg;", "", "",
		js.Limits{Timeout: 5 * time.Second})
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		m := engine.NewMsgWithPayload(21.0)
		if _, err := prog.Run(ctx, js.Sandbox{Msg: m, Outputs: 1}); err != nil {
			b.Fatal(err)
		}
	}
}
