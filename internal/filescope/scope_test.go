package filescope

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newTestScope(t *testing.T, extra ...string) (*Scope, string) {
	t.Helper()
	data := t.TempDir()
	s, err := NewScope(data, extra)
	if err != nil {
		t.Fatalf("NewScope: %v", err)
	}
	return s, data
}

func TestScopeAllowsTheDataDirectory(t *testing.T) {
	s, data := newTestScope(t)

	for _, p := range []string{
		data,
		filepath.Join(data, "flows.json"),
		filepath.Join(data, "logs", "line-3", "2026-08.csv"),
	} {
		if _, err := s.Check(p); err != nil {
			t.Errorf("Check(%s): %v", p, err)
		}
	}
}

func TestScopeRefusesOutside(t *testing.T) {
	s, data := newTestScope(t)
	outside := t.TempDir()

	for _, p := range []string{
		outside,
		filepath.Join(outside, "secret"),
		filepath.Join(data, "..", "escape"),
		// A sibling whose name starts with the root's, which a naive prefix
		// check on the string would let through.
		data + "-other/file",
	} {
		var oos *ErrOutOfScope
		if _, err := s.Check(p); !errors.As(err, &oos) {
			t.Errorf("Check(%s) returned %v, want ErrOutOfScope", p, err)
		}
	}
}

// The hole this closes: a flow writes a file under the PVC, symlinks it to /,
// and then reads through the link. The textual path still starts with the data
// directory, so only resolving symlinks catches it.
func TestScopeFollowsSymlinksOutOfTheTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating a symlink on Windows needs privileges the test runner may not have")
	}
	s, data := newTestScope(t)
	outside := t.TempDir()

	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(data, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("creating the symlink: %v", err)
	}

	var oos *ErrOutOfScope
	if _, err := s.Check(filepath.Join(link, "secret")); !errors.As(err, &oos) {
		t.Fatalf("a path through a symlink out of the tree was allowed: %v", err)
	}
}

// A file that does not exist yet has to be checkable, or the scope would refuse
// every write to a new file.
func TestScopeChecksPathsThatDoNotExistYet(t *testing.T) {
	s, data := newTestScope(t)

	target := filepath.Join(data, "not", "there", "yet.csv")
	got, err := s.Check(target)
	if err != nil {
		t.Fatalf("Check on a path that does not exist: %v", err)
	}
	if got != filepath.Clean(target) {
		t.Fatalf("Check returned %s, want %s", got, target)
	}
}

func TestScopeExtraRoots(t *testing.T) {
	extra := t.TempDir()
	s, data := newTestScope(t, extra)

	for _, p := range []string{data, extra, filepath.Join(extra, "config.yaml")} {
		if _, err := s.Check(p); err != nil {
			t.Errorf("Check(%s): %v", p, err)
		}
	}
	if _, err := s.Check(filepath.Join(t.TempDir(), "elsewhere")); err == nil {
		t.Error("a third directory was allowed")
	}
}

// A node holding a scope that was never installed must not fail open.
func TestNilAndZeroScopeRefuseEverything(t *testing.T) {
	var zero Scope
	if _, err := zero.Check("/tmp/anything"); err == nil {
		t.Error("the zero scope allowed a path")
	}
	var nilScope *Scope
	if _, err := nilScope.Check("/tmp/anything"); err == nil {
		t.Error("a nil scope allowed a path")
	}
}

func TestScopeReportsWhatIsAllowed(t *testing.T) {
	s, data := newTestScope(t)
	_, err := s.Check(filepath.Join(t.TempDir(), "x"))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	// The message has to name the roots and the setting to change, because the
	// person reading it is looking at a flow that used to work under Node-RED.
	if !strings.Contains(err.Error(), "files.allowedPaths") {
		t.Errorf("error %q does not say how to widen the scope", err)
	}
	if len(s.Roots()) != 1 || !strings.EqualFold(s.Roots()[0], normalise(mustEval(t, data))) {
		t.Errorf("Roots() = %v, want the data directory", s.Roots())
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}
