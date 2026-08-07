package nodes

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/embernet-ai/emberwire/internal/node"
	"github.com/embernet-ai/emberwire/internal/shell"
)

// execHelperSentinel marks a run of this test binary that is standing in for an
// external command.
//
// Running the test binary as its own subject is the standard library's own
// pattern for testing os/exec, and it is the only portable one: there is no
// command that exists with the same name and the same behaviour on both a
// developer's Windows box and the distroless image this ships in. Deliberately
// not starting with a dash — Go's flag package would try to parse it and the
// binary would exit before reaching any test.
const execHelperSentinel = "emberwire-exec-helper"

// TestExecHelperProcess is not a test. It is the program the exec node tests
// run, selected by passing the sentinel on the command line.
func TestExecHelperProcess(t *testing.T) {
	mode := ""
	for i, a := range os.Args {
		if a == execHelperSentinel && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	if mode == "" {
		t.Skip("not running as the exec helper")
	}

	rest := os.Args[len(os.Args)-1]
	switch mode {
	case "echo":
		fmt.Print(rest)
	case "stderr":
		fmt.Fprint(os.Stderr, "something went wrong")
	case "both":
		fmt.Print("out")
		fmt.Fprint(os.Stderr, "err")
	case "fail":
		fmt.Fprint(os.Stderr, "refusing")
		os.Exit(3)
	case "lines":
		for i := range 3 {
			fmt.Printf("line %d\n", i)
		}
	case "flood":
		// More than any sane output cap, to prove the cap holds.
		chunk := strings.Repeat("x", 4096)
		for range 512 {
			fmt.Print(chunk)
		}
	case "sleep":
		time.Sleep(30 * time.Second)
	}
	// Exit before the testing framework prints anything, so the command's
	// output is exactly what this wrote.
	os.Exit(0)
}

// withExecPolicy installs a policy allowing the test binary, and restores
// whatever was there before.
func withExecPolicy(t *testing.T) {
	t.Helper()
	p, err := shell.NewPolicy(true, []string{os.Args[0]})
	if err != nil {
		t.Fatalf("building the test exec policy: %v", err)
	}
	prev := Commands
	Commands = p
	t.Cleanup(func() { Commands = prev })
}

// execConfig builds an exec node configuration that runs this test binary in the
// given helper mode.
func execConfig(t *testing.T, mode string, extra map[string]any) string {
	t.Helper()
	cfg := map[string]any{
		"command": os.Args[0],
		"append":  fmt.Sprintf("-test.run=TestExecHelperProcess %s %s", execHelperSentinel, mode),
	}
	for k, v := range extra {
		cfg[k] = v
	}
	s, err := jsonConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The node ships refusing everything. Node-RED's exec node against a default
// configuration is CVE-2025-41656; this is the check that makes that
// impossible here.
func TestExecIsDisabledUntilAnOperatorSaysOtherwise(t *testing.T) {
	prev := Commands
	Commands = &shell.Policy{}
	t.Cleanup(func() { Commands = prev })

	err := buildErr(t, "exec", `{"command":"ls"}`, newTestServices())
	if err == nil {
		t.Fatal("the exec node built against a policy that allows nothing")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("error was %q; it should say the node is disabled and how to change that", err)
	}
}

// A flow naming a forbidden command must fail the deploy, in front of whoever
// deployed it, rather than at the first message in the middle of the night.
func TestExecRefusesACommandOutsideTheAllowlistAtBuildTime(t *testing.T) {
	withExecPolicy(t)
	if err := buildErr(t, "exec", `{"command":"definitely-not-allowed"}`, newTestServices()); err == nil {
		t.Fatal("a command outside the allowlist built successfully")
	}
}

func TestExecRunsAndReportsOutput(t *testing.T) {
	withExecPolicy(t)
	n := build(t, "exec", execConfig(t, "echo", nil), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{"payload":"hello there"}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	waitFor(t, 30*time.Second, "the command to finish", func() bool { return len(e.on(2)) == 1 })

	stdout := e.on(0)
	if len(stdout) != 1 {
		t.Fatalf("stdout output got %d messages, want 1", len(stdout))
	}
	// The payload is appended as one argument, whatever it contains: splitting
	// it would let a message arriving over MQTT decide how many arguments the
	// command gets.
	if got, _ := stdout[0].Payload().(string); got != "hello there" {
		t.Fatalf("stdout payload = %q, want the payload passed through as one argument", got)
	}

	rc, ok := e.on(2)[0].Payload().(map[string]any)
	if !ok || rc["code"] != float64(0) {
		t.Fatalf("return code payload = %#v, want {code: 0}", e.on(2)[0].Payload())
	}
	if len(e.on(1)) != 0 {
		t.Fatalf("stderr output got %d messages for a command that wrote nothing to it", len(e.on(1)))
	}
}

func TestExecSeparatesStderrAndReportsANonZeroExit(t *testing.T) {
	withExecPolicy(t)
	n := build(t, "exec", execConfig(t, "fail", nil), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{"payload":""}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	waitFor(t, 30*time.Second, "the command to finish", func() bool { return len(e.on(2)) == 1 })

	if len(e.on(1)) != 1 {
		t.Fatalf("stderr output got %d messages, want 1", len(e.on(1)))
	}
	if got, _ := e.on(1)[0].Payload().(string); !strings.Contains(got, "refusing") {
		t.Errorf("stderr payload = %q", got)
	}
	rc, _ := e.on(2)[0].Payload().(map[string]any)
	if rc["code"] != float64(3) {
		t.Fatalf("return code = %#v, want 3", rc)
	}
	// The exit status has to reach the first two outputs as well, or a flow
	// reading stdout cannot tell a successful run from a failed one.
	if got, _ := e.on(0)[0].Data["rc"].(map[string]any); got["code"] != float64(3) {
		t.Errorf("msg.rc on the stdout output = %#v", e.on(0)[0].Data["rc"])
	}
}

func TestExecStreamsAMessagePerLine(t *testing.T) {
	withExecPolicy(t)
	n := build(t, "exec", execConfig(t, "lines", map[string]any{"useSpawn": "true"}), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{"payload":""}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	waitFor(t, 30*time.Second, "the command to finish", func() bool { return len(e.on(2)) == 1 })

	lines := e.on(0)
	if len(lines) != 3 {
		t.Fatalf("streaming produced %d messages, want one per line", len(lines))
	}
	for i, m := range lines {
		want := fmt.Sprintf("line %d\n", i)
		if got, _ := m.Payload().(string); got != want {
			t.Errorf("line %d = %q, want %q", i, got, want)
		}
	}
}

// Node-RED buffers a command's output without a limit, so `cat` on the wrong
// file takes the pod with it.
func TestExecCapsOutputAndSaysSo(t *testing.T) {
	withExecPolicy(t)
	n := build(t, "exec", execConfig(t, "flood", map[string]any{"ew_maxOutput": 4096}), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	if err := pushTo(t, n, e, msg(t, `{"payload":""}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	waitFor(t, 30*time.Second, "the command to finish", func() bool { return len(e.on(2)) == 1 })

	got, _ := e.on(0)[0].Payload().(string)
	if len(got) > 4096 {
		t.Fatalf("stdout was %d bytes, past the 4096 limit", len(got))
	}
	// Truncation that nobody is told about is worse than no truncation: the
	// flow would act on half a result believing it had all of it.
	e.mu.Lock()
	errCount := len(e.errs)
	e.mu.Unlock()
	if errCount == 0 {
		t.Fatal("the output was truncated without raising an error")
	}
}

func TestExecKillsACommandThatOverrunsItsTimeout(t *testing.T) {
	withExecPolicy(t)
	n := build(t, "exec", execConfig(t, "sleep", map[string]any{"timer": 0.25}), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	start := time.Now()
	if err := pushTo(t, n, e, msg(t, `{"payload":""}`)); err != nil {
		t.Fatalf("receive: %v", err)
	}
	waitFor(t, 30*time.Second, "the command to be killed", func() bool { return len(e.on(2)) == 1 })

	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("the command ran for %s; the timeout did not take", elapsed)
	}
	rc, _ := e.on(2)[0].Payload().(map[string]any)
	if rc["message"] != "timed out" {
		t.Fatalf("return code = %#v, want it to say the command timed out", rc)
	}
}

// A node whose command takes a minute must not accumulate a process per message.
func TestExecBoundsConcurrentCommands(t *testing.T) {
	withExecPolicy(t)
	n := build(t, "exec", execConfig(t, "sleep", map[string]any{"timer": 20}), newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	var lastErr error
	for range defaultExecConcurrency + 2 {
		lastErr = pushTo(t, n, e, msg(t, `{"payload":""}`))
	}
	if lastErr == nil {
		t.Fatal("a command was started past the concurrency limit")
	}
	if p := n.(node.Deferrer).Pending(); p != defaultExecConcurrency {
		t.Fatalf("Pending = %d, want the limit of %d", p, defaultExecConcurrency)
	}

	// Cancelling the flow's context must kill them, so a redeploy does not leave
	// processes behind.
	cancel()
	waitFor(t, 30*time.Second, "the running commands to be killed",
		func() bool { return n.(node.Deferrer).Pending() == 0 })
}

func TestExecRefusesAShellMetacharacterInItsArguments(t *testing.T) {
	withExecPolicy(t)
	cfg, err := jsonConfig(map[string]any{
		"command": os.Args[0],
		"append":  "-test.run=X | rm -rf /",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := buildErr(t, "exec", cfg, newTestServices()); err == nil {
		t.Fatal("a pipe in the extra arguments was accepted; there is no shell to run it")
	}
}
