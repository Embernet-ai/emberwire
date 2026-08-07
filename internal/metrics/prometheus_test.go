package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func render(stats []NodeStat) string {
	var b strings.Builder
	NewHandler("1.2.3", func() []NodeStat { return stats }).Write(&b)
	return b.String()
}

func TestExpositionShape(t *testing.T) {
	out := render([]NodeStat{
		{NodeID: "a1", Type: "mqtt in", Received: 100, Sent: 100, QueueLen: 3, QueueCap: 1024, QueueHigh: 12},
	})

	// Every family needs HELP and TYPE, or a scraper treats it as untyped and
	// rate() over a counter silently produces nonsense.
	for _, want := range []string{
		"# HELP emberwire_node_messages_received_total",
		"# TYPE emberwire_node_messages_received_total counter",
		"# TYPE emberwire_node_queue_length gauge",
		`emberwire_node_messages_received_total{node="a1",type="mqtt in"} 100`,
		`emberwire_node_queue_length{node="a1",type="mqtt in"} 3`,
		`emberwire_node_queue_high_water{node="a1",type="mqtt in"} 12`,
		`emberwire_build_info{version="1.2.3"} 1`,
		"emberwire_goroutines",
		"emberwire_memory_heap_bytes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exposition is missing %q\n---\n%s", want, out)
		}
	}
}

func TestLabelsAreEscaped(t *testing.T) {
	// A node's type comes from a flow file anyone can write. An unescaped quote
	// or newline produces a scrape that silently fails to parse, and the first
	// symptom is a dashboard that stops updating.
	out := render([]NodeStat{
		{NodeID: `weird"id`, Type: "line\nbreak", Received: 1},
	})

	if !strings.Contains(out, `node="weird\"id"`) {
		t.Errorf("a quote in a label was not escaped:\n%s", out)
	}
	if !strings.Contains(out, `type="line\nbreak"`) {
		t.Errorf("a newline in a label was not escaped:\n%s", out)
	}
	// And no raw newline leaked into the middle of a sample line.
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "break") {
			t.Errorf("a label newline broke the exposition into an invalid line: %q", line)
		}
	}
}

func TestBackslashInLabel(t *testing.T) {
	out := render([]NodeStat{{NodeID: `a\b`, Type: "t", Received: 1}})
	if !strings.Contains(out, `node="a\\b"`) {
		t.Errorf("a backslash was not escaped:\n%s", out)
	}
}

func TestEmptyRuntimeStillExposesFamilies(t *testing.T) {
	// A runtime with no flows must still expose the families, so a dashboard
	// querying them gets an empty result rather than an unknown metric — the
	// difference between "no messages" and "monitoring is broken".
	out := render(nil)

	for _, want := range []string{
		"# TYPE emberwire_node_messages_received_total counter",
		"# TYPE emberwire_node_queue_length gauge",
		"emberwire_nodes_running 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("empty runtime is missing %q\n---\n%s", want, out)
		}
	}
}

func TestOutputIsSortedAndDeterministic(t *testing.T) {
	// Two scrapes of an unchanged runtime must be identical, so a diff between
	// them shows what changed rather than a reshuffle.
	stats := []NodeStat{
		{NodeID: "zz", Type: "debug", Received: 1},
		{NodeID: "aa", Type: "inject", Received: 2},
		{NodeID: "mm", Type: "change", Received: 3},
	}
	first := render(stats)
	second := render(stats)

	// Uptime and heap move between renders; compare only the node series.
	nodeLines := func(s string) []string {
		var out []string
		for _, l := range strings.Split(s, "\n") {
			if strings.HasPrefix(l, "emberwire_node_") {
				out = append(out, l)
			}
		}
		return out
	}
	a, b := nodeLines(first), nodeLines(second)
	if len(a) != len(b) {
		t.Fatalf("scrape line counts differ: %d and %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("line %d differs between scrapes:\n  %s\n  %s", i, a[i], b[i])
		}
	}

	iAA := strings.Index(first, `node="aa"`)
	iMM := strings.Index(first, `node="mm"`)
	iZZ := strings.Index(first, `node="zz"`)
	if !(iAA < iMM && iMM < iZZ) {
		t.Error("node series are not sorted by id")
	}
}

func TestLargeCountersKeepTheirDigits(t *testing.T) {
	// A busy edge box runs for months. A counter in the billions rendered in
	// scientific notation loses precision and rate() goes wrong.
	out := render([]NodeStat{{NodeID: "n", Type: "t", Received: 9007199254740992}})
	if !strings.Contains(out, "9.007199254740992e+15") && !strings.Contains(out, "9007199254740992") {
		t.Errorf("a large counter was not rendered usefully:\n%s", out)
	}
}

func TestHandlerServesCorrectContentType(t *testing.T) {
	// Prometheus checks this. The wrong type and the scrape is discarded.
	h := NewHandler("dev", func() []NodeStat { return nil })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	if !strings.Contains(rec.Body.String(), "emberwire_build_info") {
		t.Error("the handler produced no exposition")
	}
}
