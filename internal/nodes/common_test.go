package nodes

import (
	"strings"
	"testing"
)

// TestDebugPublishesToSidebar exists because it did not, and the sidebar was
// silently empty.
//
// The debug node originally reached the editor through a package-level function
// variable that nothing ever assigned, so every publish was skipped by a nil
// check. It built, it passed every test, and it produced nothing. Publishing now
// goes through the Emitter, which means it is wired by construction and this
// test fails if it is ever unwired again.
func TestDebugPublishesToSidebar(t *testing.T) {
	svc := newTestServices()
	n := build(t, "debug", `{"active":true,"tosidebar":true,"complete":"payload","name":"out"}`, svc)

	e, err := send(t, n, msg(t, `{"payload":{"temp":21.5},"topic":"press/01"}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}

	events := e.publishedOn("debug")
	if len(events) != 1 {
		t.Fatalf("published %d debug events, want 1 — the sidebar would be empty", len(events))
	}
	d := events[0].Data
	if d["topic"] != "press/01" {
		t.Errorf("topic = %#v", d["topic"])
	}
	if d["name"] != "out" {
		t.Errorf("name = %#v", d["name"])
	}
	if s, _ := d["msg"].(string); !strings.Contains(s, "21.5") {
		t.Errorf("msg = %#v, want it to contain the payload", d["msg"])
	}
	if d["format"] != "object" {
		t.Errorf("format = %#v, want object", d["format"])
	}
	if id, _ := d["msgId"].(string); id == "" {
		t.Error("no msgId; the sidebar could not correlate this with a message")
	}
}

func TestDebugInactivePublishesNothing(t *testing.T) {
	svc := newTestServices()
	n := build(t, "debug", `{"active":false,"tosidebar":true,"complete":"payload"}`, svc)
	e, _ := send(t, n, msg(t, `{"payload":1}`))
	if len(e.publishedOn("debug")) != 0 {
		t.Error("a disabled debug node still published")
	}
}

func TestDebugCompleteMessage(t *testing.T) {
	// complete:"true" shows the whole message rather than one property.
	svc := newTestServices()
	n := build(t, "debug", `{"active":true,"tosidebar":true,"complete":"true"}`, svc)
	e, _ := send(t, n, msg(t, `{"payload":1,"topic":"t","extra":"kept"}`))

	events := e.publishedOn("debug")
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	s, _ := events[0].Data["msg"].(string)
	for _, want := range []string{"payload", "topic", "extra"} {
		if !strings.Contains(s, want) {
			t.Errorf("complete message is missing %q: %s", want, s)
		}
	}
}

func TestDebugMissingPropertyIsVisible(t *testing.T) {
	// A typo in the property path must be visible rather than looking like
	// silence, which is why Node-RED prints "(undefined)" instead of nothing.
	svc := newTestServices()
	n := build(t, "debug", `{"active":true,"tosidebar":true,"complete":"payload.nope"}`, svc)
	e, _ := send(t, n, msg(t, `{"payload":{"temp":1}}`))

	events := e.publishedOn("debug")
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	if s, _ := events[0].Data["msg"].(string); s != "(undefined)" {
		t.Errorf("msg = %#v, want \"(undefined)\"", s)
	}
}

func TestDebugToStatusBadge(t *testing.T) {
	svc := newTestServices()
	n := build(t, "debug", `{"active":true,"tosidebar":false,"tostatus":true,"complete":"payload"}`, svc)
	e, _ := send(t, n, msg(t, `{"payload":"running"}`))

	if len(e.statuses) != 1 {
		t.Fatalf("set %d statuses, want 1", len(e.statuses))
	}
	if e.statuses[0].Text != "running" {
		t.Errorf("status text = %q", e.statuses[0].Text)
	}
	if len(e.publishedOn("debug")) != 0 {
		t.Error("tosidebar was false but the node still published to the sidebar")
	}
}

func TestDebugTruncatesLongPayloads(t *testing.T) {
	// An unbounded debug string crossing the websocket to every connected
	// editor is a good way to lock a browser.
	svc := newTestServices()
	n := build(t, "debug", `{"active":true,"tosidebar":true,"complete":"payload","maxLength":32}`, svc)
	e, _ := send(t, n, msg(t, `{"payload":"`+strings.Repeat("x", 500)+`"}`))

	s, _ := e.publishedOn("debug")[0].Data["msg"].(string)
	if len(s) > 100 {
		t.Errorf("debug string is %d chars; it was not truncated", len(s))
	}
	if !strings.Contains(s, "more bytes") {
		t.Errorf("truncated string does not say how much was dropped: %q", s)
	}
}

func TestInjectEmitsConfiguredProperties(t *testing.T) {
	svc := newTestServices()
	n := build(t, "inject", `{"props":[
        {"p":"payload","v":"42","vt":"num"},
        {"p":"topic","v":"press/01","vt":"str"}
    ]}`, svc)

	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	out := e.on(0)
	if len(out) != 1 {
		t.Fatalf("emitted %d messages, want 1", len(out))
	}
	if got := out[0].Payload(); got != 42.0 {
		t.Errorf("payload = %#v (%T), want float64 42", got, got)
	}
	if got := out[0].Topic(); got != "press/01" {
		t.Errorf("topic = %q", got)
	}
	if out[0].ID() == "" {
		t.Error("injected message has no id")
	}
}

func TestInjectRejectsBadRepeat(t *testing.T) {
	svc := newTestServices()
	if err := buildErr(t, "inject", `{"repeat":"0","props":[]}`, svc); err == nil {
		t.Error("a repeat interval of zero was accepted; it would spin")
	}
	if err := buildErr(t, "inject", `{"repeat":"not a number","props":[]}`, svc); err == nil {
		t.Error("a non-numeric repeat interval was accepted")
	}
}

func TestJunctionAndCommentBehaviour(t *testing.T) {
	svc := newTestServices()

	j := build(t, "junction", `{}`, svc)
	e, err := send(t, j, msg(t, `{"payload":"through"}`))
	if err != nil {
		t.Fatalf("junction: %v", err)
	}
	if len(e.on(0)) != 1 || e.on(0)[0].Payload() != "through" {
		t.Error("junction did not pass the message straight through")
	}

	c := build(t, "comment", `{"name":"a note"}`, svc)
	e2, err := send(t, c, msg(t, `{"payload":1}`))
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	if e2.total() != 0 {
		t.Error("a comment node emitted a message")
	}
}

func TestLinkOutReportsMissingTargets(t *testing.T) {
	// Node-RED drops these silently, which makes a mistyped or deleted link
	// target very hard to find.
	Links.Reset()
	svc := newTestServices()
	n := build(t, "link out", `{"mode":"link","links":["no-such-node"]}`, svc)

	_, err := send(t, n, msg(t, `{"payload":1}`))
	if err == nil {
		t.Fatal("a link to a node that is not running was accepted silently")
	}
	if !strings.Contains(err.Error(), "no-such-node") {
		t.Errorf("error = %q, want it to name the missing target", err)
	}
}

func TestLinkOutReturnModeIsRefused(t *testing.T) {
	Links.Reset()
	svc := newTestServices()
	n := build(t, "link out", `{"mode":"return"}`, svc)
	if _, err := send(t, n, msg(t, `{"payload":1}`)); err == nil {
		t.Error("return mode was accepted despite Link Call not being implemented")
	}
}
