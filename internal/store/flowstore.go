package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/embernet-ai/emberwire/internal/engine"
)

// DefaultBackupGenerations is how many previous flow files are retained.
const DefaultBackupGenerations = 3

// ErrRevisionConflict is returned when a save is attempted against a stale
// revision, meaning somebody else deployed in between.
//
// Node-RED calls this a deploy conflict and carries the same "rev" token on
// /flows. Without it, two editor tabs open on the same runtime silently
// overwrite each other's work.
var ErrRevisionConflict = errors.New("flow revision conflict: the flows were modified by someone else")

// FlowStore persists the flow file.
type FlowStore struct {
	path       string
	backups    int
	perm       os.FileMode
	mu         sync.Mutex
	rev        string
	recovered  bool
	recoveryOf string
}

// NewFlowStore returns a store for the flow file at path.
func NewFlowStore(path string) *FlowStore {
	return &FlowStore{
		path:    path,
		backups: DefaultBackupGenerations,
		// 0600: the flow file is not secret in the way credentials are, but it
		// describes the plant's topology and it lives on a shared PVC.
		perm: 0o600,
	}
}

// SetBackupGenerations changes how many previous versions are kept.
func (s *FlowStore) SetBackupGenerations(n int) { s.backups = n }

// Path returns the flow file path.
func (s *FlowStore) Path() string { return s.path }

// Recovered reports whether the last Load fell back to a backup, and which file
// it came from. The API surfaces this so an operator finds out that their flow
// file was corrupt, rather than discovering it when a flow they wrote last week
// turns out to be missing.
func (s *FlowStore) Recovered() (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recovered, s.recoveryOf
}

// Load reads and parses the flow file.
//
// If the primary file is missing, an empty flow set is returned — a fresh pod
// with an empty PVC is a normal state, not an error. If it is present but
// unparseable, each backup generation is tried in turn, newest first. Starting
// empty because the file was corrupt would look like a successful start and
// silently discard every flow the customer had.
func (s *FlowStore) Load() (*engine.Flows, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.recovered, s.recoveryOf = false, ""

	data, err := os.ReadFile(s.path)
	switch {
	case os.IsNotExist(err):
		f := mustEmpty()
		s.rev = revisionOf(nil)
		f.Rev = s.rev
		return f, nil
	case err != nil:
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}

	flows, parseErr := engine.ParseFlows(data)
	if parseErr == nil {
		s.rev = revisionOf(data)
		flows.Rev = s.rev
		return flows, nil
	}

	// The primary file is corrupt. Try the backups before giving up.
	var attempts []string
	attempts = append(attempts, fmt.Sprintf("%s: %v", s.path, parseErr))

	for _, bak := range BackupNames(s.path, s.backups) {
		bdata, berr := os.ReadFile(bak)
		if berr != nil {
			if !os.IsNotExist(berr) {
				attempts = append(attempts, fmt.Sprintf("%s: %v", bak, berr))
			}
			continue
		}
		bflows, bparseErr := engine.ParseFlows(bdata)
		if bparseErr != nil {
			attempts = append(attempts, fmt.Sprintf("%s: %v", bak, bparseErr))
			continue
		}

		// Preserve the corrupt file for forensics rather than letting the next
		// save overwrite the evidence.
		_ = os.Rename(s.path, s.path+".corrupt")

		s.rev = revisionOf(bdata)
		bflows.Rev = s.rev
		bflows.Warnings = append(bflows.Warnings, fmt.Sprintf(
			"recovered from backup %s; the primary flow file was unparseable and has been kept as %s.corrupt",
			bak, s.path))
		s.recovered, s.recoveryOf = true, bak
		return bflows, nil
	}

	return nil, fmt.Errorf("no readable flow file: %v", attempts)
}

// Save writes the flow set, rotating backups first.
//
// expectedRev implements optimistic concurrency: pass the revision the caller
// last read, and the save fails with ErrRevisionConflict if the file has moved
// on. Pass "" to force.
func (s *FlowStore) Save(flows *engine.Flows, expectedRev string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if expectedRev != "" && s.rev != "" && expectedRev != s.rev {
		return "", fmt.Errorf("%w (have %s, you sent %s)", ErrRevisionConflict, s.rev, expectedRev)
	}

	data, err := flows.Marshal()
	if err != nil {
		return "", fmt.Errorf("serialising flows: %w", err)
	}
	// Trailing newline: the file is read by humans and diffed by git.
	data = append(data, '\n')

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(s.path), err)
	}
	if err := RotateBackups(s.path, s.backups); err != nil {
		return "", fmt.Errorf("rotating backups: %w", err)
	}
	if err := WriteFileAtomic(s.path, data, s.perm); err != nil {
		return "", err
	}

	s.rev = revisionOf(data)
	flows.Rev = s.rev
	return s.rev, nil
}

// Rev returns the current revision token.
func (s *FlowStore) Rev() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rev
}

// revisionOf derives a revision token from the file bytes.
//
// Content-derived rather than a counter, so two runtimes that independently
// arrive at the same flow file agree on the revision, and so a restart does not
// reset it and let a stale editor tab overwrite newer work.
func revisionOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:12])
}

func mustEmpty() *engine.Flows {
	f, err := engine.ParseFlows([]byte("[]"))
	if err != nil {
		// "[]" is a compile-time constant and always parses. If it ever does
		// not, the parser is broken in a way no runtime handling can help.
		panic("emberwire: empty flow document failed to parse: " + err.Error())
	}
	return f
}
