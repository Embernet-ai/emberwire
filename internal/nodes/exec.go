package nodes

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
	"github.com/embernet-ai/emberwire/internal/shell"
)

func init() {
	registerExec()
}

// Commands is the process-wide policy the exec node is checked against. main
// installs it from the configuration before any flow starts, exactly as it does
// the discovery Scope, so a node can never run against an unset policy — the
// zero value refuses everything.
var Commands = &shell.Policy{}

// Defaults for the exec node, applied when the flow entry says nothing.
const (
	defaultExecTimeout     = 30 * time.Second
	defaultExecConcurrency = 4
	defaultExecMaxOutput   = 1 << 20 // 1 MiB per stream
)

// execNode runs an external command and reports its output and exit status.
type execNode struct {
	command    string
	extraArgs  []string
	appendProp string
	spawn      bool
	timeout    time.Duration
	maxOutput  int
	oldRC      bool

	sem chan struct{}

	mu      sync.Mutex
	running int
	wg      sync.WaitGroup
	ctx     context.Context
}

func registerExec() {
	node.MustRegister(node.Descriptor{
		Type:         "exec",
		Category:     node.CategoryFunction,
		Color:        "#F3B567",
		Icon:         "terminal",
		Inputs:       1,
		Outputs:      3,
		OutputLabels: []string{"stdout", "stderr", "return code"},
		PaletteLabel: "exec",
		LabelProp:    "name",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "The three outputs, both buffered and streaming modes, the timeout " +
				"and the appended message property all behave as Node-RED's do. Two " +
				"things do not, and neither is negotiable. There is no shell: the " +
				"command line is split on quoting rules only, and an unquoted shell " +
				"metacharacter is refused rather than run, so one allowed command " +
				"cannot become an arbitrary one. And the node is disabled until an " +
				"operator names the commands a flow may run — Node-RED's exec node " +
				"against a default configuration is CVE-2025-41656, unauthenticated " +
				"remote code execution. Output is capped per stream; a command that " +
				"exceeds it is killed and reported rather than being allowed to fill " +
				"the heap. A command that forks children of its own may leave them " +
				"behind when it is killed.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "command", Kind: node.PropString, Label: "Command", Required: true,
				Help: "Must be one of the commands the operator allowed. No shell, no pipes."},
			{Name: "append", Kind: node.PropString, Label: "Extra arguments",
				Help: "Appended to the command line, split on the same quoting rules."},
			{Name: "addpay", Kind: node.PropString, Label: "Append a message property", Default: "payload",
				Help: "Appended as one argument, whatever it contains. Leave empty to append nothing."},
			{Name: "useSpawn", Kind: node.PropSelect, Label: "Output", Default: "false",
				Options: []node.Option{
					{Value: "false", Label: "When the command completes"},
					{Value: "true", Label: "A message per line, while it runs"},
				}},
			{Name: "timer", Kind: node.PropNumber, Label: "Timeout (seconds)", Default: 30,
				Help: "The command is killed past this. Zero uses the configured default."},
			{Name: "oldrc", Kind: node.PropBool, Label: "Use the old return-code format"},
			{Name: "winHide", Kind: node.PropBool, Label: "Hide the console window (Windows)"},
			{Name: "ew_maxOutput", Kind: node.PropNumber, Label: "Output limit (bytes)",
				Help: "Per stream. Emberwire's own; Node-RED buffers without a limit."},
		},
		Help: "Runs an external command and returns its stdout, stderr and exit " +
			"status on three outputs. Disabled until the operator lists the commands " +
			"a flow may run, and there is no shell, so quoting is the only thing " +
			"interpreted in the command line.",
	}, newExec)
}

func newExec(def *node.Definition) (node.Node, error) {
	n := &execNode{
		command:   strings.TrimSpace(def.Node.PropString("command", "")),
		spawn:     def.Node.PropString("useSpawn", "false") == "true",
		oldRC:     def.Node.PropBool("oldrc", false),
		maxOutput: def.Node.PropInt("ew_maxOutput", defaultExecMaxOutput),
		sem:       make(chan struct{}, defaultExecConcurrency),
	}
	if n.command == "" {
		return nil, fmt.Errorf("no command configured")
	}
	if n.maxOutput <= 0 {
		n.maxOutput = defaultExecMaxOutput
	}

	// Node-RED spells this property two ways across versions: a boolean meaning
	// "append msg.payload", and a string naming the property to append.
	switch raw, _ := def.Node.Prop("addpay"); v := raw.(type) {
	case bool:
		if v {
			n.appendProp = engine.PropPayload
		}
	case string:
		n.appendProp = v
	case nil:
		n.appendProp = engine.PropPayload
	}

	if secs := def.Node.PropFloat("timer", 0); secs > 0 {
		n.timeout = time.Duration(secs * float64(time.Second))
	} else {
		n.timeout = defaultExecTimeout
	}

	if extra := strings.TrimSpace(def.Node.PropString("append", "")); extra != "" {
		args, err := shell.SplitArgs(extra)
		if err != nil {
			return nil, fmt.Errorf("extra arguments: %w", err)
		}
		n.extraArgs = args
	}

	// Checked at build time so a flow naming a forbidden command fails the
	// deploy, in front of whoever deployed it, rather than at the first message
	// in the middle of the night. The check is repeated per message because the
	// policy can promote a command that appeared after boot.
	if _, err := Commands.Resolve(n.command); err != nil {
		return nil, err
	}
	return n, nil
}

// Start keeps the flow's context so a running command is killed when the flow
// stops rather than outliving the runtime that spawned it.
func (n *execNode) Start(ctx context.Context, _ node.Emitter) error {
	n.mu.Lock()
	n.ctx = ctx
	n.mu.Unlock()
	return nil
}

func (n *execNode) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	path, err := Commands.Resolve(n.command)
	if err != nil {
		return err
	}

	args := append([]string(nil), n.extraArgs...)
	if n.appendProp != "" {
		v, ok, err := m.Get(n.appendProp)
		if err != nil {
			return fmt.Errorf("reading %s: %w", n.appendProp, err)
		}
		if ok {
			// One argument, never split. Splitting it would let a payload
			// arriving over MQTT decide how many arguments the command gets,
			// which is argument injection with extra steps.
			args = append(args, argString(v))
		}
	}

	// Bounded concurrency. A node whose command takes a minute must not be able
	// to accumulate a process per message; refusing is visible, and forking
	// until the pod dies is not.
	select {
	case n.sem <- struct{}{}:
	default:
		return fmt.Errorf("%d instances of %q are already running; "+
			"slow the source down or put a rate limit in front of this node",
			cap(n.sem), n.command)
	}

	n.mu.Lock()
	n.running++
	base := n.ctx
	n.mu.Unlock()
	if base == nil {
		base = ctx
	}
	n.wg.Add(1)

	go func() {
		defer func() {
			<-n.sem
			n.mu.Lock()
			n.running--
			n.mu.Unlock()
			n.wg.Done()
		}()
		n.run(base, path, args, m, out)
	}()
	return nil
}

// run executes the command and emits the results.
func (n *execNode) run(base context.Context, path string, args []string, m *engine.Msg, out node.Emitter) {
	ctx, cancel := context.WithTimeout(base, n.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, args...)
	// WaitDelay bounds how long Wait blocks on pipes still held open by a child
	// the command itself forked. Without it, killing a command whose grandchild
	// inherited stdout leaves this goroutine waiting for a process nobody is
	// tracking.
	cmd.WaitDelay = 2 * time.Second

	out.Status(node.Status{Fill: "blue", Shape: "dot", Text: "running"})

	var (
		stdout, stderr *cappedBuffer
		streamWG       sync.WaitGroup
		err            error
	)

	if n.spawn {
		outPipe, e1 := cmd.StdoutPipe()
		errPipe, e2 := cmd.StderrPipe()
		if e1 != nil || e2 != nil {
			out.Error(fmt.Errorf("attaching to %s: %w", path, errors.Join(e1, e2)), m)
			return
		}
		if err = cmd.Start(); err == nil {
			streamWG.Add(2)
			go func() { defer streamWG.Done(); n.streamLines(outPipe, 0, m, out) }()
			go func() { defer streamWG.Done(); n.streamLines(errPipe, 1, m, out) }()
			streamWG.Wait()
			err = cmd.Wait()
		}
	} else {
		stdout = &cappedBuffer{limit: n.maxOutput}
		stderr = &cappedBuffer{limit: n.maxOutput}
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		err = cmd.Run()
	}

	code, signalled := exitStatus(err)
	rc := n.returnCode(code, err, ctx)

	if !n.spawn {
		first := m.Clone()
		first.SetPayload(stdout.String())
		first.Data["rc"] = rc
		out.Send(0, first)

		if stderr.Len() > 0 {
			second := m.Clone()
			second.SetPayload(stderr.String())
			second.Data["rc"] = rc
			out.Send(1, second)
		}
		if stdout.Truncated() || stderr.Truncated() {
			out.Error(fmt.Errorf("%s produced more than the %d byte output limit and was truncated; "+
				"raise ew_maxOutput or have the command write to a file", n.command, n.maxOutput), m)
		}
	}

	third := m.Clone()
	third.SetPayload(rc)
	out.Send(2, third)

	switch {
	case ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		out.Status(node.Status{Fill: "red", Shape: "dot", Text: "timed out"})
		out.Error(fmt.Errorf("%s did not finish within %s and was killed", n.command, n.timeout), m)
	case signalled:
		out.Status(node.Status{Fill: "red", Shape: "dot", Text: "killed"})
	case code != 0:
		out.Status(node.Status{Fill: "red", Shape: "dot", Text: fmt.Sprintf("exit %d", code)})
	default:
		out.Status(node.Status{})
	}
}

// streamLines emits one message per line as the command produces it.
func (n *execNode) streamLines(r io.Reader, port int, src *engine.Msg, out node.Emitter) {
	sc := bufio.NewScanner(io.LimitReader(r, int64(n.maxOutput)))
	sc.Buffer(make([]byte, 0, 64<<10), n.maxOutput)
	for sc.Scan() {
		m := src.Clone()
		m.SetPayload(sc.Text() + "\n")
		out.Send(port, m)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		out.Log(node.LogWarn, "reading output from %s: %v", n.command, err)
	}
	// Drain the rest so the command is not blocked writing into a pipe nobody
	// is reading, which would turn the output cap into a hang.
	_, _ = io.Copy(io.Discard, r)
}

// returnCode builds the object the third output carries.
func (n *execNode) returnCode(code int, err error, ctx context.Context) any {
	if n.oldRC {
		// Node-RED's pre-1.0 shape: the bare number.
		return float64(code)
	}
	rc := map[string]any{"code": float64(code)}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		rc["message"] = "timed out"
	} else if err != nil && code == 0 {
		// The command never ran — not found, not executable, killed before it
		// started. A zero here without an explanation would read as success.
		rc["message"] = err.Error()
	}
	return rc
}

// exitStatus pulls the exit code out of whatever Run or Wait returned, and
// reports whether the process died on a signal.
func exitStatus(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if code := ee.ExitCode(); code >= 0 {
			return code, false
		}
		// A negative code means the process was signalled rather than exiting.
		// Report it the way a shell does, as 128 plus the signal number, without
		// reaching for the platform-specific syscall types.
		return 137, true
	}
	// Failed to start at all.
	return -1, false
}

// argString renders a message property as a single command-line argument.
func argString(v any) string {
	switch t := v.(type) {
	case []byte:
		return string(t)
	case engine.ImmutableBytes:
		return string(t)
	default:
		return mustacheString(v)
	}
}

// Pending reports running commands, so a redeploy waits for them to be killed
// and their exit status reported rather than tearing down around them.
func (n *execNode) Pending() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.running
}

func (n *execNode) Close(ctx context.Context, _ bool) error {
	done := make(chan struct{})
	go func() {
		n.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%d command(s) were still running at shutdown", n.Pending())
	}
}

// ---------------------------------------------------------------------------
// cappedBuffer
// ---------------------------------------------------------------------------

// cappedBuffer collects output up to a limit and then stops, remembering that it
// did. Node-RED buffers a command's output without a limit, so `cat` on the
// wrong file takes the pod with it.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room <= 0 {
		c.truncated = true
		// Report the full length: returning short makes os/exec treat it as an
		// I/O error and mask the command's own exit status.
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) String() string  { return c.buf.String() }
func (c *cappedBuffer) Len() int        { return c.buf.Len() }
func (c *cappedBuffer) Truncated() bool { return c.truncated }
