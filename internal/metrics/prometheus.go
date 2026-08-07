// Package metrics exposes runtime counters in Prometheus text format.
//
// Written by hand rather than with client_golang. The data already exists as
// atomics on each node runner, the exposition format is small and stable, and
// the library would add a couple of megabytes to a binary whose whole argument
// is that it is small. What the library buys — label escaping, consistent
// HELP/TYPE emission, collision safety — is reproduced here deliberately rather
// than skipped.
//
// Node-RED exposes nothing comparable. Per-node message counts, error counts and
// queue depth are the difference between knowing a flow is backing up and
// finding out when the pod is OOM-killed.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sort"
	"strings"
	"time"
)

// NodeStat is one node's counters. Mirrors runtime.Snapshot, kept separate so
// this package does not depend on the runtime and can be tested on its own.
type NodeStat struct {
	NodeID    string
	Type      string
	Received  int64
	Sent      int64
	Errors    int64
	Dropped   int64
	Blocked   int64
	QueueLen  int
	QueueCap  int
	QueueHigh int64
}

// Source supplies the current counters.
type Source func() []NodeStat

// Handler serves the metrics endpoint.
type Handler struct {
	source    Source
	version   string
	startedAt time.Time
}

// NewHandler builds the endpoint.
func NewHandler(version string, source Source) *Handler {
	return &Handler{source: source, version: version, startedAt: time.Now()}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	h.Write(w)
}

// Write emits the exposition.
func (h *Handler) Write(w io.Writer) {
	var b strings.Builder

	stats := h.source()
	// Sorted so a diff between two scrapes is readable and so the output is
	// deterministic; Prometheus does not care, humans reading it do.
	sort.Slice(stats, func(i, j int) bool { return stats[i].NodeID < stats[j].NodeID })

	writeGauge(&b, "emberwire_build_info",
		"Build information. Always 1; the version is carried in the label.",
		[]labelled{{labels: [][2]string{{"version", h.version}}, value: 1}})

	writeGauge(&b, "emberwire_uptime_seconds",
		"Seconds since the runtime started.",
		[]labelled{{value: time.Since(h.startedAt).Seconds()}})

	writeGauge(&b, "emberwire_nodes_running",
		"Number of node instances currently running.",
		[]labelled{{value: float64(len(stats))}})

	// Per-node series.
	counters := []struct {
		name, help string
		get        func(NodeStat) float64
	}{
		{"emberwire_node_messages_received_total",
			"Messages delivered to a node since it started.",
			func(s NodeStat) float64 { return float64(s.Received) }},
		{"emberwire_node_messages_sent_total",
			"Messages a node has emitted since it started.",
			func(s NodeStat) float64 { return float64(s.Sent) }},
		{"emberwire_node_errors_total",
			"Errors raised by a node since it started.",
			func(s NodeStat) float64 { return float64(s.Errors) }},
		{"emberwire_node_messages_dropped_total",
			"Messages discarded because a node's inbox was full. Node-RED cannot " +
				"report this because its queue is unbounded.",
			func(s NodeStat) float64 { return float64(s.Dropped) }},
		{"emberwire_node_sends_blocked_total",
			"Times a sender waited for space in a node's inbox. Sustained " +
				"back-pressure, which is the signal that a flow cannot keep up.",
			func(s NodeStat) float64 { return float64(s.Blocked) }},
	}

	for _, c := range counters {
		series := make([]labelled, 0, len(stats))
		for _, s := range stats {
			series = append(series, labelled{labels: nodeLabels(s), value: c.get(s)})
		}
		writeCounter(&b, c.name, c.help, series)
	}

	gauges := []struct {
		name, help string
		get        func(NodeStat) float64
	}{
		{"emberwire_node_queue_length",
			"Messages currently waiting in a node's inbox.",
			func(s NodeStat) float64 { return float64(s.QueueLen) }},
		{"emberwire_node_queue_capacity",
			"Maximum messages a node's inbox holds before its overflow policy applies.",
			func(s NodeStat) float64 { return float64(s.QueueCap) }},
		{"emberwire_node_queue_high_water",
			"Deepest a node's inbox has been since it started. The early warning " +
				"that a flow is close to overflowing, before it does.",
			func(s NodeStat) float64 { return float64(s.QueueHigh) }},
	}

	for _, g := range gauges {
		series := make([]labelled, 0, len(stats))
		for _, s := range stats {
			series = append(series, labelled{labels: nodeLabels(s), value: g.get(s)})
		}
		writeGauge(&b, g.name, g.help, series)
	}

	// Process health. Enough to alert on without a separate exporter, which an
	// edge box has no room for.
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	writeGauge(&b, "emberwire_goroutines",
		"Goroutines in flight. Roughly one per node plus the I/O each one holds.",
		[]labelled{{value: float64(runtime.NumGoroutine())}})
	writeGauge(&b, "emberwire_memory_heap_bytes",
		"Heap bytes currently allocated.",
		[]labelled{{value: float64(mem.HeapAlloc)}})
	writeGauge(&b, "emberwire_memory_sys_bytes",
		"Bytes obtained from the operating system.",
		[]labelled{{value: float64(mem.Sys)}})
	writeCounter(&b, "emberwire_gc_cycles_total",
		"Completed garbage collection cycles.",
		[]labelled{{value: float64(mem.NumGC)}})

	_, _ = io.WriteString(w, b.String())
}

func nodeLabels(s NodeStat) [][2]string {
	return [][2]string{{"node", s.NodeID}, {"type", s.Type}}
}

type labelled struct {
	labels [][2]string
	value  float64
}

func writeCounter(b *strings.Builder, name, help string, series []labelled) {
	writeFamily(b, name, help, "counter", series)
}

func writeGauge(b *strings.Builder, name, help string, series []labelled) {
	writeFamily(b, name, help, "gauge", series)
}

func writeFamily(b *strings.Builder, name, help, typ string, series []labelled) {
	// A family with no series still gets its HELP and TYPE, so a dashboard
	// querying it sees an empty result rather than an unknown metric.
	fmt.Fprintf(b, "# HELP %s %s\n", name, escapeHelp(help))
	fmt.Fprintf(b, "# TYPE %s %s\n", name, typ)

	for _, s := range series {
		b.WriteString(name)
		if len(s.labels) > 0 {
			b.WriteByte('{')
			for i, kv := range s.labels {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(kv[0])
				b.WriteString(`="`)
				b.WriteString(escapeLabel(kv[1]))
				b.WriteByte('"')
			}
			b.WriteByte('}')
		}
		b.WriteByte(' ')
		b.WriteString(formatValue(s.value))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
}

// escapeLabel escapes a label value per the exposition format.
//
// A node id is hex and a type is an identifier, so in practice neither needs
// this — but a node's type comes from a flow file that anybody can write, and an
// unescaped quote or newline there would produce a scrape that silently fails
// to parse.
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// escapeHelp escapes a HELP string, where only backslash and newline are
// special.
func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// formatValue renders a sample.
//
// %g rather than a fixed precision: a counter in the billions must not lose its
// last digits, and a fractional gauge must not be padded with meaningless zeros.
func formatValue(v float64) string {
	return strings.TrimSuffix(fmt.Sprintf("%g", v), ".0")
}
