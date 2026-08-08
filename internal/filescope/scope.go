// Package filescope bounds where the file nodes may read and write.
//
// Node-RED's file nodes take any path at all, which means "can edit a flow"
// implies "can read anything the process can read and write anything it can
// write". On a Pi in a workshop that is fine. In an App Store app on a
// customer's plant floor, where the editor is reachable by anyone with a
// dashboard login, it is a way to read a mounted Secret.
//
// So the file nodes are scoped. The default scope is the data directory — the
// PVC, where a flow's own files live and where they almost always belong — and
// an operator adds more roots deliberately. This is enabled rather than
// disabled by default, unlike the exec node: reading and writing files under
// your own PVC is the ordinary use, whereas running a command never is.
package filescope

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Scope is the set of directory trees the file nodes may touch.
type Scope struct {
	roots []string
}

// NewScope builds a scope. dataDir is always included, because refusing a flow
// access to its own PVC would make the nodes useless out of the box and every
// operator would widen the scope to everything just to get started — which is
// worse than having no scope at all, because it would look deliberate.
func NewScope(dataDir string, extra []string) (*Scope, error) {
	s := &Scope{}

	all := append([]string{dataDir}, extra...)
	for _, r := range all {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("allowed path %q: %w", r, err)
		}
		// Resolved through symlinks so that a root given as /data and a path
		// arriving as /var/lib/kubelet/.../data compare equal. A root that does
		// not exist yet keeps its textual form; the data directory is created
		// before this runs, and an extra root that is missing will simply not
		// match anything until it appears.
		if real, err := filepath.EvalSymlinks(abs); err == nil {
			abs = real
		}
		s.roots = append(s.roots, normalise(abs))
	}

	if len(s.roots) == 0 {
		return nil, fmt.Errorf("no allowed paths; at minimum the data directory must be one")
	}
	return s, nil
}

// Roots lists the allowed trees, for the error message and for the boot log.
func (s *Scope) Roots() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.roots...)
}

// ErrOutOfScope is returned for a path outside every allowed root.
type ErrOutOfScope struct {
	Path  string
	Roots []string
}

func (e *ErrOutOfScope) Error() string {
	if len(e.Roots) == 0 {
		return fmt.Sprintf("%s is outside the paths the file nodes may use", e.Path)
	}
	return fmt.Sprintf("%s is outside the paths the file nodes may use (%s); "+
		"add it to files.allowedPaths if a flow is meant to reach it",
		e.Path, strings.Join(e.Roots, ", "))
}

// Check resolves a path and reports whether it is inside the scope, returning
// the cleaned absolute path to use.
//
// Symlinks are followed as far as the path exists, which is what closes the
// obvious hole: a flow writing a file under the PVC, symlinking it at
// /data/escape -> /, and then reading /data/escape/etc/shadow. Resolving only
// the textual path would let that through, because /data/escape/etc/shadow
// starts with /data.
func (s *Scope) Check(path string) (string, error) {
	if s == nil || len(s.roots) == 0 {
		return "", &ErrOutOfScope{Path: path}
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("no filename given")
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", path, err)
	}
	abs = filepath.Clean(abs)

	resolved, err := resolveExisting(abs)
	if err != nil {
		return "", err
	}

	target := normalise(resolved)
	for _, root := range s.roots {
		if target == root || strings.HasPrefix(target, root+string(filepath.Separator)) {
			// Return the unresolved-but-cleaned path: opening the symlink is
			// the caller's intent, and it is safe now that the destination has
			// been checked.
			return abs, nil
		}
	}
	return "", &ErrOutOfScope{Path: abs, Roots: s.roots}
}

// resolveExisting evaluates symlinks over the longest prefix of the path that
// exists, then re-appends the rest.
//
// A file about to be created does not exist, so EvalSymlinks on the whole path
// fails. Resolving the deepest existing ancestor and keeping the remainder is
// what makes the check work for a write as well as a read.
func resolveExisting(abs string) (string, error) {
	rest := ""
	cur := abs
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolving %q: %w", abs, err)
		}

		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the root without finding anything that exists. Fall back
			// to the textual path rather than refusing outright; the containment
			// check below still applies.
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// normalise puts a path into the form the containment comparison uses.
func normalise(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		// Case-insensitive, and comparing "C:\Data" against "c:\data" as
		// different strings would silently refuse a legitimate path. Only
		// relevant for development; the shipped image is Linux.
		p = strings.ToLower(p)
	}
	return p
}
