package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/embernet-ai/emberwire/internal/engine"
)

func sampleFlows(t *testing.T, label string) *engine.Flows {
	t.Helper()
	f, err := engine.ParseFlows([]byte(`[
        {"id":"t1","type":"tab","label":"` + label + `"},
        {"id":"n1","type":"debug","z":"t1","x":10,"y":10,"wires":[]}
    ]`))
	if err != nil {
		t.Fatalf("ParseFlows: %v", err)
	}
	return f
}

func TestWriteFileAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flows.json")

	if err := WriteFileAtomic(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "flows.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory contains %v, want only flows.json — a temp file was left behind", names)
	}

	got, _ := os.ReadFile(path)
	if string(got) != `{"a":1}` {
		t.Errorf("contents = %q", got)
	}
}

func TestWriteFileAtomicSetsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Windows has no POSIX mode bits; Chmod there only toggles read-only.
		// The container this ships in is Linux, which is where the guarantee
		// has to hold and where CI checks it.
		t.Skip("POSIX permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	if err := WriteFileAtomic(path, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// A credential file at 0644 on a shared PVC is the bug this guards.
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("mode = %o, want no group or other access", perm)
	}
}

func TestWriteFileAtomicNeverExposesAPartialFile(t *testing.T) {
	// The core survivability property. A reader — or a pod that dies and comes
	// back — must see either the previous contents or the new ones, never a
	// half-written file. Node-RED writes flows.json in place, so an eviction
	// mid-write leaves a truncated file and the plant does not come back up.
	//
	// Linux only, and not as a convenience: on Windows, MoveFileEx refuses to
	// replace a file that any handle has open, so the scenario this test sets
	// up cannot occur there at all. Retrying the rename does not help against a
	// continuous reader, and weakening either the test or the write path to
	// make a Windows limitation disappear would be testing the wrong thing. The
	// binary ships on distroless Linux; CI runs this on ubuntu.
	if runtime.GOOS == "windows" {
		t.Skip("rename-over-open-file is not permitted on Windows; this guarantee is verified on Linux in CI")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "flows.json")

	small := []byte(`[{"id":"a","type":"tab","label":"small"}]`)
	large := []byte(`[{"id":"b","type":"tab","label":"` + strings.Repeat("x", 200000) + `"}]`)

	if err := WriteFileAtomic(path, small, 0o600); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	var stop atomic.Bool
	var reads, bad atomic.Int64
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			data, err := os.ReadFile(path)
			if err != nil {
				// On Windows a rename can briefly make the path unavailable to
				// a concurrent opener. That is not a torn read.
				continue
			}
			reads.Add(1)
			if !json.Valid(data) {
				bad.Add(1)
			}
		}
	}()

	for i := 0; i < 200; i++ {
		payload := small
		if i%2 == 0 {
			payload = large
		}
		if err := WriteFileAtomic(path, payload, 0o600); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	stop.Store(true)
	wg.Wait()

	if reads.Load() == 0 {
		t.Fatal("the reader never observed the file; the test proved nothing")
	}
	if bad.Load() != 0 {
		t.Errorf("%d of %d reads saw a partially written file", bad.Load(), reads.Load())
	}
}

func TestRotateBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flows.json")

	// Rotating before anything exists is a no-op, not an error.
	if err := RotateBackups(path, 3); err != nil {
		t.Fatalf("RotateBackups on a missing file: %v", err)
	}

	for _, gen := range []string{"one", "two", "three", "four"} {
		if err := WriteFileAtomic(path, []byte(gen), 0o600); err != nil {
			t.Fatalf("write %s: %v", gen, err)
		}
		if err := RotateBackups(path, 3); err != nil {
			t.Fatalf("rotate after %s: %v", gen, err)
		}
	}

	// After writing four generations and rotating each time, bak.1 holds the
	// newest and only three are kept.
	want := map[string]string{
		path + ".bak.1": "four",
		path + ".bak.2": "three",
		path + ".bak.3": "two",
	}
	for p, expect := range want {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("reading %s: %v", filepath.Base(p), err)
			continue
		}
		if string(got) != expect {
			t.Errorf("%s = %q, want %q", filepath.Base(p), got, expect)
		}
	}
	if _, err := os.Stat(path + ".bak.4"); !os.IsNotExist(err) {
		t.Error("bak.4 exists; more generations were kept than requested")
	}
}

func TestFlowStoreLoadMissingFileIsEmpty(t *testing.T) {
	// A fresh pod with an empty PVC is a normal state, not a failure.
	s := NewFlowStore(filepath.Join(t.TempDir(), "flows.json"))
	f, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(f.Order) != 0 {
		t.Errorf("expected an empty flow set, got %d entries", len(f.Order))
	}
	if f.Rev == "" {
		t.Error("empty flow set has no revision token")
	}
}

func TestFlowStoreSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flows.json")
	s := NewFlowStore(path)

	rev, err := s.Save(sampleFlows(t, "First"), "")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if rev == "" {
		t.Fatal("Save returned an empty revision")
	}

	s2 := NewFlowStore(path)
	f, err := s2.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if f.Tabs["t1"].Label != "First" {
		t.Errorf("label = %q", f.Tabs["t1"].Label)
	}
	// The revision is content-derived, so an independent store that reads the
	// same bytes agrees on it. A counter would reset on restart and let a stale
	// editor tab overwrite newer work.
	if f.Rev != rev {
		t.Errorf("revision after reload = %q, want %q", f.Rev, rev)
	}
}

func TestFlowStoreRevisionConflict(t *testing.T) {
	// Two editor tabs open on one runtime must not silently overwrite each
	// other.
	path := filepath.Join(t.TempDir(), "flows.json")
	s := NewFlowStore(path)

	rev1, err := s.Save(sampleFlows(t, "First"), "")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	rev2, err := s.Save(sampleFlows(t, "Second"), rev1)
	if err != nil {
		t.Fatalf("Save with the current revision: %v", err)
	}
	if rev2 == rev1 {
		t.Error("revision did not change after a save with different content")
	}

	_, err = s.Save(sampleFlows(t, "Third"), rev1)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Errorf("save with a stale revision returned %v, want ErrRevisionConflict", err)
	}

	// An empty expected revision forces the write, which is what the CLI and
	// the "overwrite anyway" path in the editor need.
	if _, err := s.Save(sampleFlows(t, "Forced"), ""); err != nil {
		t.Errorf("forced save: %v", err)
	}
}

func TestFlowStoreRecoversFromCorruptPrimary(t *testing.T) {
	// Starting empty because the file was corrupt would look like a successful
	// start and silently discard every flow the customer had. That is the
	// failure this exists to prevent.
	path := filepath.Join(t.TempDir(), "flows.json")
	s := NewFlowStore(path)

	if _, err := s.Save(sampleFlows(t, "Good"), ""); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Save again so a backup generation exists holding the good content.
	if _, err := s.Save(sampleFlows(t, "AlsoGood"), s.Rev()); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	// Simulate a torn write from a pod evicted mid-save.
	if err := os.WriteFile(path, []byte(`[{"id":"t1","type":"tab","lab`), 0o600); err != nil {
		t.Fatalf("corrupting the file: %v", err)
	}

	s2 := NewFlowStore(path)
	f, err := s2.Load()
	if err != nil {
		t.Fatalf("Load did not recover: %v", err)
	}
	if f.Tabs["t1"] == nil {
		t.Fatal("recovered flow set has no tab")
	}

	recovered, from := s2.Recovered()
	if !recovered {
		t.Error("Recovered() = false; the operator would never learn their flow file was corrupt")
	}
	if from == "" {
		t.Error("Recovered() did not report which backup was used")
	}

	var warned bool
	for _, w := range f.Warnings {
		if strings.Contains(w, "recovered from backup") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no recovery warning on the flow set; warnings = %v", f.Warnings)
	}

	// The corrupt file is preserved for forensics rather than being silently
	// overwritten by the next save.
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Errorf("corrupt file was not preserved: %v", err)
	}
}

func TestFlowStoreFailsWhenNothingIsReadable(t *testing.T) {
	// With no usable backup there is nothing honest to do but refuse to start.
	path := filepath.Join(t.TempDir(), "flows.json")
	if err := os.WriteFile(path, []byte(`not json at all`), 0o600); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	s := NewFlowStore(path)
	if _, err := s.Load(); err == nil {
		t.Fatal("Load succeeded on an unreadable file with no backups; it must refuse")
	}
}

func TestFlowStoreSurvivesRepeatedSaveLoadCycles(t *testing.T) {
	// A deploy loop must converge, not accumulate drift.
	path := filepath.Join(t.TempDir(), "flows.json")
	s := NewFlowStore(path)

	flows := sampleFlows(t, "Stable")
	rev, err := s.Save(flows, "")
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	for i := 0; i < 25; i++ {
		loaded, err := s.Load()
		if err != nil {
			t.Fatalf("Load %d: %v", i, err)
		}
		newRev, err := s.Save(loaded, loaded.Rev)
		if err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
		if newRev != rev {
			t.Fatalf("cycle %d changed the revision: %s -> %s; the file is churning", i, rev, newRev)
		}
	}
}
