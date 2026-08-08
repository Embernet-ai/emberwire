package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
	emberruntime "github.com/embernet-ai/emberwire/internal/runtime"
	"github.com/embernet-ai/emberwire/internal/shell"
)

// The benchmark harness.
//
// It exists because the throughput and memory figures on the Node-RED forums
// are anecdotes — different hardware, different flows, different Node versions,
// no controlled measurement behind any of them — and quoting one in our own
// README would be repeating a rumour with our name on it. Nothing comparative
// goes in a document until this command produced it, on one box, in one run,
// against both runtimes.
//
// Two measurements, because they answer different questions:
//
//   - engine: messages per second through a chain of nodes with no I/O in the
//     path. This is the scheduler and the message model and nothing else, and it
//     is the number that says whether the runtime is the bottleneck.
//   - http:   requests per second and latency through a real HTTP endpoint that
//     a flow answers. This includes the HTTP stack, which is the point — it is
//     what somebody using the app actually experiences, and it is the only
//     surface both runtimes present identically.
//
// The comparison half drives an already-running instance over HTTP. It does not
// care which runtime is on the other end, which is exactly what makes it fair:
// the same load generator, the same payloads, the same box.

const benchUsage = `usage: emberwire bench [flags]

  -mode engine|http|both   what to measure (default both)
  -target URL              an instance to drive over HTTP, ours or Node-RED's
  -launch "cmd args..."    start this first and measure its cold start and memory
  -ready URL               the URL to poll for readiness (default -target)
  -path /bench             the endpoint the flow answers on (default /bench)
  -duration 30s            how long to hold the load
  -warmup 3s               load applied before measuring, to settle the JIT
  -connections 8           concurrent clients
  -payload 256             request body size in bytes
  -chain 5                 nodes in the engine chain
  -messages 200000         messages to push through the engine chain
  -json                    machine-readable output

Engine mode needs nothing. HTTP mode needs -target, and a flow on the far end
serving -path. There is an equivalent Node-RED flow in docs/bench/.
`

type benchOptions struct {
	mode        string
	target      string
	launch      string
	ready       string
	path        string
	duration    time.Duration
	warmup      time.Duration
	connections int
	payload     int
	chain       int
	messages    int
	asJSON      bool
}

// benchReport is everything one run measured. Fields left zero were not
// measured, and the printer says so rather than showing a plausible zero.
type benchReport struct {
	Host   benchHost    `json:"host"`
	Engine *engineStats `json:"engine,omitempty"`
	HTTP   *httpStats   `json:"http,omitempty"`
	Ran    string       `json:"ran"`
}

type benchHost struct {
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	CPUs    int    `json:"cpus"`
	Version string `json:"emberwireVersion"`
}

type engineStats struct {
	ChainNodes     int     `json:"chainNodes"`
	Messages       int     `json:"messages"`
	Seconds        float64 `json:"seconds"`
	MessagesPerSec float64 `json:"messagesPerSecond"`
	NanosPerMsg    float64 `json:"nanosecondsPerMessage"`
	// AllocatedBytes is what the process allocated over the run, which is the
	// figure that decides how hard the collector has to work.
	AllocatedBytes uint64  `json:"allocatedBytes"`
	AllocPerMsg    float64 `json:"allocatedBytesPerMessage"`
	PeakHeapBytes  uint64  `json:"peakHeapBytes"`
}

type httpStats struct {
	Target         string  `json:"target"`
	Path           string  `json:"path"`
	Connections    int     `json:"connections"`
	PayloadBytes   int     `json:"payloadBytes"`
	Seconds        float64 `json:"seconds"`
	Requests       int64   `json:"requests"`
	Errors         int64   `json:"errors"`
	NonOK          int64   `json:"nonOKResponses"`
	RequestsPerSec float64 `json:"requestsPerSecond"`

	LatencyP50 float64 `json:"latencyMillisP50"`
	LatencyP95 float64 `json:"latencyMillisP95"`
	LatencyP99 float64 `json:"latencyMillisP99"`
	LatencyMax float64 `json:"latencyMillisMax"`

	// The launched-process measurements. Absent when -launch was not used.
	ColdStartSeconds float64 `json:"coldStartSeconds,omitempty"`
	IdleRSSBytes     uint64  `json:"idleRSSBytes,omitempty"`
	LoadedRSSBytes   uint64  `json:"loadedRSSBytes,omitempty"`
	RSSAvailable     bool    `json:"rssAvailable"`
}

func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, benchUsage) }

	var o benchOptions
	fs.StringVar(&o.mode, "mode", "both", "engine, http or both")
	fs.StringVar(&o.target, "target", "", "base URL of an instance to drive")
	fs.StringVar(&o.launch, "launch", "", "command to start before measuring")
	fs.StringVar(&o.ready, "ready", "", "URL to poll for readiness")
	fs.StringVar(&o.path, "path", "/bench", "endpoint the flow answers on")
	fs.DurationVar(&o.duration, "duration", 30*time.Second, "how long to hold the load")
	fs.DurationVar(&o.warmup, "warmup", 3*time.Second, "load applied before measuring")
	fs.IntVar(&o.connections, "connections", 8, "concurrent clients")
	fs.IntVar(&o.payload, "payload", 256, "request body size in bytes")
	fs.IntVar(&o.chain, "chain", 5, "nodes in the engine chain")
	fs.IntVar(&o.messages, "messages", 200000, "messages through the engine chain")
	fs.BoolVar(&o.asJSON, "json", false, "machine-readable output")

	if err := fs.Parse(args); err != nil {
		return err
	}
	switch o.mode {
	case "engine", "http", "both":
	default:
		return fmt.Errorf("unknown mode %q; use engine, http or both", o.mode)
	}
	if o.mode != "engine" && o.target == "" {
		// Refused rather than silently downgraded to engine-only. A harness
		// that quietly measures half of what was asked for produces a report
		// somebody will read as the whole thing.
		return errors.New("-target is required for http mode; give it the base URL of the " +
			"instance to drive, or use -mode engine")
	}
	if o.connections < 1 {
		return errors.New("-connections must be at least 1")
	}

	report := benchReport{
		Host: benchHost{
			OS: runtime.GOOS, Arch: runtime.GOARCH,
			CPUs: runtime.NumCPU(), Version: version,
		},
		Ran: time.Now().Format(time.RFC3339),
	}

	if o.mode == "engine" || o.mode == "both" {
		stats, err := benchEngine(o)
		if err != nil {
			return fmt.Errorf("engine benchmark: %w", err)
		}
		report.Engine = stats
	}

	if o.mode == "http" || o.mode == "both" {
		stats, err := benchHTTP(o)
		if err != nil {
			return fmt.Errorf("http benchmark: %w", err)
		}
		report.HTTP = stats
	}

	if o.asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	printBenchReport(report)
	return nil
}

// ---------------------------------------------------------------------------
// engine
// ---------------------------------------------------------------------------

// benchEngine pushes messages through a chain of nodes with no I/O in the path.
//
// The chain is built out of real Change nodes rather than a purpose-built
// benchmark node, so what is measured is the same code a flow runs: the same
// property expressions, the same clone on every hop, the same inbox and the same
// goroutine handoff. A synthetic node that did nothing would measure the
// scheduler and flatter it.
func benchEngine(o benchOptions) (*engineStats, error) {
	if o.chain < 1 {
		return nil, errors.New("-chain must be at least 1")
	}
	if o.messages < 1 {
		return nil, errors.New("-messages must be at least 1")
	}

	flows, err := engine.ParseFlows(benchChainFlow(o.chain))
	if err != nil {
		return nil, err
	}

	rt := emberruntime.New(node.Default, flows, emberruntime.Options{
		// A deep inbox so the measurement is of the chain rather than of the
		// producer blocking on back-pressure. The bounded inbox is the right
		// production default and the wrong thing to measure through.
		InboxCapacity: 1 << 16,
	})

	// The event channel has to be drained or it fills and events are dropped,
	// which would show up as a stall that has nothing to do with the chain.
	go func() {
		for range rt.Events() {
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if failures := rt.Start(ctx); len(failures) > 0 {
		return nil, fmt.Errorf("starting the chain: %v", failures)
	}
	defer rt.Stop(context.Background())

	tail := fmt.Sprintf("n%d", o.chain)

	// Warm up, so the first measured message is not paying for a cold cache and
	// the goroutines are already scheduled.
	warm := min(o.messages/10, 10000)
	for range warm {
		_ = rt.Inject("n1", engine.NewMsgWithPayload(map[string]any{"v": float64(1)}))
	}
	if err := waitForCount(rt, tail, int64(warm), 60*time.Second); err != nil {
		return nil, fmt.Errorf("warm-up: %w", err)
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	peak := &peakHeap{}
	stopPeak := peak.watch()

	start := time.Now()
	for range o.messages {
		_ = rt.Inject("n1", engine.NewMsgWithPayload(map[string]any{"v": float64(1)}))
	}
	if err := waitForCount(rt, tail, int64(warm+o.messages), 10*time.Minute); err != nil {
		stopPeak()
		return nil, err
	}
	elapsed := time.Since(start)
	stopPeak()

	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc

	return &engineStats{
		ChainNodes:     o.chain,
		Messages:       o.messages,
		Seconds:        elapsed.Seconds(),
		MessagesPerSec: float64(o.messages) / elapsed.Seconds(),
		NanosPerMsg:    float64(elapsed.Nanoseconds()) / float64(o.messages),
		AllocatedBytes: allocated,
		AllocPerMsg:    float64(allocated) / float64(o.messages),
		PeakHeapBytes:  peak.value(),
	}, nil
}

// benchChainFlow builds a chain of n Change nodes.
func benchChainFlow(n int) []byte {
	entries := []map[string]any{
		{"id": "bench", "type": "tab", "label": "bench"},
	}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("n%d", i)
		var wires []any
		if i < n {
			wires = []any{[]any{fmt.Sprintf("n%d", i+1)}}
		} else {
			wires = []any{[]any{}}
		}
		entries = append(entries, map[string]any{
			"id": id, "type": "change", "z": "bench",
			"x": float64(100 * i), "y": 100.0, "wires": wires,
			"rules": []any{map[string]any{
				"t": "set", "p": "hop", "pt": "msg", "to": id, "tot": "str",
			}},
		})
	}
	b, err := json.Marshal(entries)
	if err != nil {
		panic("emberwire: building the benchmark flow: " + err.Error())
	}
	return b
}

// waitForCount blocks until a node has received n messages.
func waitForCount(rt *emberruntime.Runtime, nodeID string, n int64, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, s := range rt.Snapshots() {
			if s.NodeID == nodeID && s.Received >= n {
				return nil
			}
		}
		time.Sleep(200 * time.Microsecond)
	}
	return fmt.Errorf("timed out waiting for %s to receive %d messages", nodeID, n)
}

// peakHeap samples the heap while a measurement runs.
//
// Sampled rather than read once at the end, because the end of a run is exactly
// when the collector has just been and the peak is invisible.
type peakHeap struct {
	max  atomic.Uint64
	stop chan struct{}
	done chan struct{}
}

func (p *peakHeap) watch() func() {
	p.stop = make(chan struct{})
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		t := time.NewTicker(10 * time.Millisecond)
		defer t.Stop()
		var m runtime.MemStats
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				runtime.ReadMemStats(&m)
				for {
					cur := p.max.Load()
					if m.HeapAlloc <= cur || p.max.CompareAndSwap(cur, m.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	return func() {
		close(p.stop)
		<-p.done
	}
}

func (p *peakHeap) value() uint64 { return p.max.Load() }

// ---------------------------------------------------------------------------
// http
// ---------------------------------------------------------------------------

func benchHTTP(o benchOptions) (*httpStats, error) {
	stats := &httpStats{
		Target: o.target, Path: o.path,
		Connections: o.connections, PayloadBytes: o.payload,
	}

	readyURL := o.ready
	if readyURL == "" {
		readyURL = strings.TrimSuffix(o.target, "/") + o.path
	}

	var proc *launchedProcess
	if o.launch != "" {
		p, err := launch(o.launch, readyURL)
		if err != nil {
			return nil, err
		}
		proc = p
		defer proc.stop()

		stats.ColdStartSeconds = proc.coldStart.Seconds()
		// Sampled after a settle, so what is reported is a runtime at rest
		// rather than one still finishing its start-up allocations.
		time.Sleep(2 * time.Second)
		if rss, ok := readRSS(proc.pid); ok {
			stats.IdleRSSBytes = rss
			stats.RSSAvailable = true
		}
	} else if err := waitForReady(readyURL, 30*time.Second); err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(o.target, "/") + o.path
	body := strings.Repeat("x", o.payload)

	// Warm-up load, discarded. Node-RED's JIT needs it and it would be unfair
	// not to give it; ours does not and it costs nothing to be even-handed.
	if o.warmup > 0 {
		if _, err := driveHTTP(url, body, o.connections, o.warmup, nil); err != nil {
			return nil, err
		}
	}

	var rssPeak atomic.Uint64
	var rssStop chan struct{}
	if proc != nil && stats.RSSAvailable {
		rssStop = make(chan struct{})
		go sampleRSS(proc.pid, &rssPeak, rssStop)
	}

	result, err := driveHTTP(url, body, o.connections, o.duration, nil)
	if rssStop != nil {
		close(rssStop)
		stats.LoadedRSSBytes = rssPeak.Load()
	}
	if err != nil {
		return nil, err
	}

	stats.Seconds = result.elapsed.Seconds()
	stats.Requests = result.ok
	stats.Errors = result.failed
	stats.NonOK = result.nonOK
	stats.RequestsPerSec = float64(result.ok) / result.elapsed.Seconds()

	sort.Slice(result.latencies, func(i, j int) bool { return result.latencies[i] < result.latencies[j] })
	stats.LatencyP50 = percentileMillis(result.latencies, 0.50)
	stats.LatencyP95 = percentileMillis(result.latencies, 0.95)
	stats.LatencyP99 = percentileMillis(result.latencies, 0.99)
	if len(result.latencies) > 0 {
		stats.LatencyMax = float64(result.latencies[len(result.latencies)-1]) / 1e6
	}

	return stats, nil
}

type driveResult struct {
	ok        int64
	failed    int64
	nonOK     int64
	elapsed   time.Duration
	latencies []time.Duration
}

// driveHTTP holds a fixed number of clients against an endpoint for a duration.
//
// Closed-loop rather than a fixed rate: each client sends the next request when
// the last one came back. That measures what the far end can absorb, which is
// the question, and it cannot produce the misleading coordinated-omission
// latency an open-loop generator does when the target falls behind.
func driveHTTP(url, body string, connections int, duration time.Duration, _ any) (*driveResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	res := &driveResult{}
	// One sample slice per client, merged at the end: a shared slice under a
	// mutex would have the load generator contending with itself.
	samples := make([][]time.Duration, connections)

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: connections,
			MaxConnsPerHost:     connections,
			DisableCompression:  true,
		},
	}

	var wg sync.WaitGroup
	start := time.Now()
	for i := range connections {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			local := make([]time.Duration, 0, 4096)
			// Deferred rather than assigned after the loop. Every client is
			// mid-request when the deadline lands, so every one of them leaves
			// through the cancelled-request branch — assigning at the bottom
			// meant no client ever reached it and the whole latency sample was
			// silently empty. The percentiles printed as 0.00, which reads as a
			// very fast runtime rather than as no data at all.
			defer func() { samples[slot] = local }()

			for ctx.Err() == nil {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
					strings.NewReader(body))
				if err != nil {
					atomic.AddInt64(&res.failed, 1)
					continue
				}
				req.Header.Set("Content-Type", "text/plain")

				sent := time.Now()
				resp, err := client.Do(req)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					atomic.AddInt64(&res.failed, 1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				took := time.Since(sent)

				if resp.StatusCode >= 400 {
					atomic.AddInt64(&res.nonOK, 1)
					continue
				}
				atomic.AddInt64(&res.ok, 1)
				local = append(local, took)
			}
			samples[slot] = local
		}(i)
	}
	wg.Wait()
	res.elapsed = time.Since(start)

	for _, s := range samples {
		res.latencies = append(res.latencies, s...)
	}
	if res.ok == 0 {
		return res, fmt.Errorf("no request succeeded: %d errors, %d non-OK responses "+
			"— check that a flow is serving %s", res.failed, res.nonOK, url)
	}
	return res, nil
}

func percentileMillis(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return float64(sorted[i]) / 1e6
}

// ---------------------------------------------------------------------------
// launching and measuring another process
// ---------------------------------------------------------------------------

type launchedProcess struct {
	cmd       *exec.Cmd
	pid       int
	coldStart time.Duration
}

func launch(command, readyURL string) (*launchedProcess, error) {
	args, err := shell.SplitArgs(command)
	if err != nil {
		return nil, fmt.Errorf("-launch: %w", err)
	}

	// Deliberately not run through the exec node's allowlist: this is an
	// operator running a benchmark from a terminal, not a flow, and the
	// allowlist exists to bound what a flow author can reach.
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %q: %w", command, err)
	}

	p := &launchedProcess{cmd: cmd, pid: cmd.Process.Pid}
	if err := waitForReady(readyURL, 120*time.Second); err != nil {
		p.stop()
		return nil, err
	}
	p.coldStart = time.Since(start)
	return p, nil
}

func (p *launchedProcess) stop() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
}

// waitForReady polls until the endpoint answers anything at all.
//
// Anything, not 200: a Node-RED instance with no flow deployed answers 404 on
// the benchmark path, and that still means the process is up. Whether the flow
// is right is the load generator's problem, and it says so loudly.
func waitForReady(url string, within time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(within)
	var last error

	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s did not answer within %s: %w", url, within, last)
}

func sampleRSS(pid int, peak *atomic.Uint64, stop <-chan struct{}) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			rss, ok := readRSS(pid)
			if !ok {
				return
			}
			for {
				cur := peak.Load()
				if rss <= cur || peak.CompareAndSwap(cur, rss) {
					break
				}
			}
		}
	}
}

// readRSS reads a process's resident set size.
//
// Linux only, by reading /proc. That is not a limitation worth working around:
// a comparison is only meaningful on the box the software will run on, and that
// box is Linux. On anything else the report says the figure is unavailable
// rather than printing a zero somebody would quote.
func readRSS(pid int) (uint64, bool) {
	if runtime.GOOS != "linux" {
		return 0, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		var kb uint64
		if _, err := fmt.Sscanf(fields[1], "%d", &kb); err != nil {
			return 0, false
		}
		return kb * 1024, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// reporting
// ---------------------------------------------------------------------------

func printBenchReport(r benchReport) {
	fmt.Printf("emberwire bench — %s\n", r.Ran)
	fmt.Printf("  %s/%s, %d CPUs, emberwire %s\n\n", r.Host.OS, r.Host.Arch, r.Host.CPUs, r.Host.Version)

	if r.Engine != nil {
		e := r.Engine
		fmt.Printf("Engine — %d-node chain, no I/O in the path\n", e.ChainNodes)
		fmt.Printf("  %-28s %s\n", "messages", formatCount(int64(e.Messages)))
		fmt.Printf("  %-28s %.2fs\n", "elapsed", e.Seconds)
		fmt.Printf("  %-28s %s\n", "throughput", formatRate(e.MessagesPerSec)+" msg/s")
		fmt.Printf("  %-28s %.2f µs\n", "per message", e.NanosPerMsg/1000)
		fmt.Printf("  %-28s %.0f bytes\n", "allocated per message", e.AllocPerMsg)
		fmt.Printf("  %-28s %s\n", "peak heap", formatBytes(e.PeakHeapBytes))
		fmt.Println()
	}

	if r.HTTP != nil {
		h := r.HTTP
		fmt.Printf("HTTP — %s%s, %d connections, %d byte body\n",
			h.Target, h.Path, h.Connections, h.PayloadBytes)
		fmt.Printf("  %-28s %.2fs\n", "elapsed", h.Seconds)
		fmt.Printf("  %-28s %s\n", "requests", formatCount(h.Requests))
		fmt.Printf("  %-28s %s\n", "throughput", formatRate(h.RequestsPerSec)+" req/s")
		fmt.Printf("  %-28s %.2f / %.2f / %.2f ms\n", "latency p50 / p95 / p99",
			h.LatencyP50, h.LatencyP95, h.LatencyP99)
		fmt.Printf("  %-28s %.2f ms\n", "latency max", h.LatencyMax)
		if h.Errors > 0 || h.NonOK > 0 {
			// Printed whenever it is not zero. A throughput figure measured
			// against an endpoint that was erroring is not a throughput figure.
			fmt.Printf("  %-28s %d transport, %d non-OK\n", "failures", h.Errors, h.NonOK)
		}
		if h.ColdStartSeconds > 0 {
			fmt.Printf("  %-28s %.2fs\n", "cold start", h.ColdStartSeconds)
		}
		if h.RSSAvailable {
			fmt.Printf("  %-28s %s\n", "RSS idle", formatBytes(h.IdleRSSBytes))
			fmt.Printf("  %-28s %s\n", "RSS under load", formatBytes(h.LoadedRSSBytes))
		} else if h.ColdStartSeconds > 0 {
			fmt.Printf("  %-28s not available on %s; run the harness on the box under test\n",
				"RSS", runtime.GOOS)
		}
		fmt.Println()
	}

	fmt.Println("Run this against both runtimes on the same box, in the same session,")
	fmt.Println("before quoting a comparison anywhere.")
}

func formatCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func formatRate(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.1fk", v/1000)
	}
	return fmt.Sprintf("%.0f", v)
}

func formatBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGT"[exp])
}
