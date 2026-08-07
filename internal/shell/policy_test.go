package shell

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// testCommand writes an executable file into a temporary directory and returns
// its path. Which interpreter it uses does not matter — nothing here runs it,
// the policy only ever resolves paths.
func testCommand(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".bat"
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPolicyDisabledByDefault(t *testing.T) {
	var p Policy
	if p.Enabled() {
		t.Fatal("the zero policy is enabled; the whole point is that it refuses everything")
	}
	if _, err := p.Resolve("ls"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Resolve on a disabled policy returned %v, want ErrDisabled", err)
	}

	// A nil policy has to behave the same, because a node holding one that was
	// never installed must not fail open.
	var np *Policy
	if np.Enabled() {
		t.Fatal("a nil policy reports itself enabled")
	}
	if _, err := np.Resolve("ls"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("Resolve on a nil policy returned %v, want ErrDisabled", err)
	}
}

// An enabled policy with no commands must be a configuration error, not
// "anything". The permissive reading of an empty list is how a deliberately
// narrow capability becomes a general-purpose shell.
func TestPolicyEnabledWithNoCommandsIsRefused(t *testing.T) {
	if _, err := NewPolicy(true, nil); err == nil {
		t.Fatal("an enabled policy with no allowed commands was accepted")
	}
	if _, err := NewPolicy(true, []string{"", "   "}); err == nil {
		t.Fatal("an enabled policy whose commands are all blank was accepted")
	}
}

func TestPolicyAllowsWhatWasListed(t *testing.T) {
	allowed := testCommand(t, "allowed")
	forbidden := testCommand(t, "forbidden")

	p, err := NewPolicy(true, []string{allowed})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}

	got, err := p.Resolve(allowed)
	if err != nil {
		t.Fatalf("Resolve(%s): %v", allowed, err)
	}
	if !strings.EqualFold(filepath.Base(got), filepath.Base(allowed)) {
		t.Fatalf("resolved to %s, want %s", got, allowed)
	}

	var notAllowed *ErrNotAllowed
	if _, err := p.Resolve(forbidden); !errors.As(err, &notAllowed) {
		t.Fatalf("Resolve on a command outside the list returned %v, want ErrNotAllowed", err)
	}
}

// A command that does not exist must be refused the same way as one that is
// forbidden. Distinguishing them lets a flow author probe what is installed on
// the box, one guess at a time.
func TestPolicyDoesNotLeakWhatIsInstalled(t *testing.T) {
	allowed := testCommand(t, "allowed")
	p, err := NewPolicy(true, []string{allowed})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}

	var missing, forbidden *ErrNotAllowed
	_, errMissing := p.Resolve(filepath.Join(t.TempDir(), "definitely-not-here"))
	_, errForbidden := p.Resolve(testCommand(t, "other"))

	if !errors.As(errMissing, &missing) || !errors.As(errForbidden, &forbidden) {
		t.Fatalf("errors were %v and %v, want both to be ErrNotAllowed", errMissing, errForbidden)
	}
}

// An entry that is not on the PATH at startup must not stop the runtime from
// booting: a command mounted from a ConfigMap or installed by an init container
// is a deployment ordering problem, not a configuration error.
func TestPolicyKeepsCommandsThatAreNotThereYet(t *testing.T) {
	dir := t.TempDir()
	later := filepath.Join(dir, "arrives-later")

	p, err := NewPolicy(true, []string{later})
	if err != nil {
		t.Fatalf("NewPolicy refused a command that is not installed yet: %v", err)
	}
	if got := p.Unresolved(); !reflect.DeepEqual(got, []string{later}) {
		t.Fatalf("Unresolved = %v, want the missing entry reported", got)
	}
	if _, err := p.Resolve(later); err == nil {
		t.Fatal("a command that does not exist resolved")
	}

	// Now it shows up.
	if err := os.WriteFile(later, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Resolve(later); err != nil {
		t.Fatalf("Resolve after the command appeared: %v", err)
	}
	if got := p.Unresolved(); len(got) != 0 {
		t.Fatalf("Unresolved = %v after the command was promoted", got)
	}
}

func TestSplitArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`-l /var/log`, []string{"-l", "/var/log"}},
		{`  spaced   out  `, []string{"spaced", "out"}},
		{`"one arg" two`, []string{"one arg", "two"}},
		{`'one arg' two`, []string{"one arg", "two"}},
		{`a\ b c`, []string{"a b", "c"}},
		{`"say \"hi\""`, []string{`say "hi"`}},
		// Quoting is the only way a metacharacter gets through, and it arrives
		// as a literal rather than as an operator.
		{`'a|b'`, []string{"a|b"}},
		{`"x;y"`, []string{"x;y"}},
		{`--flag=value`, []string{"--flag=value"}},
	}
	for _, tc := range cases {
		got, err := SplitArgs(tc.in)
		if err != nil {
			t.Errorf("SplitArgs(%q): %v", tc.in, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("SplitArgs(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

// There is no shell, so a metacharacter cannot be silently taken literally. A
// flow author who typed a pipe expected a pipe, and running the command with a
// literal "|" argument would do something they did not ask for while looking
// like it worked.
func TestSplitArgsRefusesShellMetacharacters(t *testing.T) {
	for _, in := range []string{
		`a | b`, `a && b`, `a; b`, `a > out`, `a < in`, `$(whoami)`,
		"`whoami`", `a & b`, `(a)`, `${HOME}`,
	} {
		if got, err := SplitArgs(in); err == nil {
			t.Errorf("SplitArgs(%q) = %#v, want a refusal", in, got)
		}
	}
}

func TestSplitArgsRefusesMalformedQuoting(t *testing.T) {
	for _, in := range []string{`"unclosed`, `'unclosed`, `trailing\`, ``, `   `} {
		if _, err := SplitArgs(in); err == nil {
			t.Errorf("SplitArgs(%q) was accepted", in)
		}
	}
}
