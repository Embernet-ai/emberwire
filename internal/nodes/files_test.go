package nodes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/filescope"
)

// withFileScope points the file nodes at a temporary directory and restores the
// previous scope afterwards.
func withFileScope(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := filescope.NewScope(dir, nil)
	if err != nil {
		t.Fatalf("building the test file scope: %v", err)
	}
	prev := Files
	Files = s
	t.Cleanup(func() { Files = prev })
	return dir
}

// fileConfig builds a node config with a filename, which needs escaping on
// Windows where a path is full of backslashes.
func fileConfig(t *testing.T, filename string, extra map[string]any) string {
	t.Helper()
	cfg := map[string]any{"filename": filename, "filenameType": "str"}
	for k, v := range extra {
		cfg[k] = v
	}
	s, err := jsonConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ---------------------------------------------------------------------------
// file out
// ---------------------------------------------------------------------------

func TestFileOutAppendsAndOverwrites(t *testing.T) {
	dir := withFileScope(t)
	path := filepath.Join(dir, "log.txt")

	appendNode := build(t, "file", fileConfig(t, path, nil), newTestServices())
	for _, line := range []string{"first", "second"} {
		cfg, err := jsonConfig(map[string]any{"payload": line})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := send(t, appendNode, msg(t, cfg)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := readFile(t, path); got != "first\nsecond\n" {
		t.Fatalf("appended file = %q", got)
	}

	overwrite := build(t, "file", fileConfig(t, path, map[string]any{"overwriteFile": "true"}),
		newTestServices())
	if _, err := send(t, overwrite, msg(t, `{"payload":"third"}`)); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got := readFile(t, path); got != "third\n" {
		t.Fatalf("overwritten file = %q", got)
	}
}

func TestFileOutDeletes(t *testing.T) {
	dir := withFileScope(t)
	path := filepath.Join(dir, "gone.txt")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	n := build(t, "file", fileConfig(t, path, map[string]any{"overwriteFile": "delete"}),
		newTestServices())
	if _, err := send(t, n, msg(t, `{"payload":""}`)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the file is still there: %v", err)
	}

	// Deleting again must not be an error: a flow cleaning up after itself
	// should not have to check first.
	if _, err := send(t, n, msg(t, `{"payload":""}`)); err != nil {
		t.Fatalf("deleting a file that is already gone: %v", err)
	}
}

// One node writing a file per device is the reason the filename is a
// typedInput at all.
func TestFileOutTakesTheFilenameFromTheMessage(t *testing.T) {
	dir := withFileScope(t)

	cfg, err := jsonConfig(map[string]any{
		"filename": "target", "filenameType": "msg", "appendNewline": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "file", cfg, newTestServices())

	for _, device := range []string{"press-01", "press-02"} {
		m, err := jsonConfig(map[string]any{
			"target":  filepath.Join(dir, device+".txt"),
			"payload": device,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := send(t, n, msg(t, m)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := readFile(t, filepath.Join(dir, "press-02.txt")); got != "press-02" {
		t.Fatalf("second file = %q", got)
	}
}

func TestFileOutCreatesTheDirectory(t *testing.T) {
	dir := withFileScope(t)
	path := filepath.Join(dir, "a", "b", "c.txt")

	without := build(t, "file", fileConfig(t, path, nil), newTestServices())
	if _, err := send(t, without, msg(t, `{"payload":"x"}`)); err == nil {
		t.Fatal("a write into a missing directory succeeded without createDir")
	}

	with := build(t, "file", fileConfig(t, path, map[string]any{"createDir": true}), newTestServices())
	if _, err := send(t, with, msg(t, `{"payload":"x"}`)); err != nil {
		t.Fatalf("write with createDir: %v", err)
	}
	if got := readFile(t, path); got != "x\n" {
		t.Fatalf("file = %q", got)
	}
}

func TestFileOutEncodings(t *testing.T) {
	dir := withFileScope(t)

	t.Run("base64", func(t *testing.T) {
		path := filepath.Join(dir, "b64.bin")
		n := build(t, "file", fileConfig(t, path, map[string]any{
			"encoding": "base64", "appendNewline": false,
		}), newTestServices())
		if _, err := send(t, n, msg(t, `{"payload":"aGVsbG8="}`)); err != nil {
			t.Fatalf("write: %v", err)
		}
		if got := readFile(t, path); got != "hello" {
			t.Fatalf("decoded file = %q", got)
		}
	})

	t.Run("bad base64 is refused", func(t *testing.T) {
		path := filepath.Join(dir, "bad.bin")
		n := build(t, "file", fileConfig(t, path, map[string]any{"encoding": "base64"}),
			newTestServices())
		if _, err := send(t, n, msg(t, `{"payload":"not base64!!!"}`)); err == nil {
			t.Fatal("a payload that is not base64 was written anyway")
		}
	})
}

// Node-RED's file nodes take any path at all, which makes editing a flow
// equivalent to reading any file the process can.
func TestFileNodesRefusePathsOutsideTheScope(t *testing.T) {
	withFileScope(t)
	outside := filepath.Join(t.TempDir(), "secret")

	if err := buildErr(t, "watch", fileConfig(t, "", map[string]any{"files": outside}),
		newTestServices()); err == nil {
		t.Error("a watch node outside the scope built successfully")
	}

	for _, typ := range []string{"file", "file in"} {
		n := build(t, typ, fileConfig(t, outside, nil), newTestServices())
		if _, err := send(t, n, msg(t, `{"payload":"x"}`)); err == nil {
			t.Errorf("%s reached a path outside the scope", typ)
		}
	}
}

// ---------------------------------------------------------------------------
// file in
// ---------------------------------------------------------------------------

func TestFileInWholeFile(t *testing.T) {
	dir := withFileScope(t)
	path := filepath.Join(dir, "in.txt")
	if err := os.WriteFile(path, []byte("hello\nworld\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	n := build(t, "file in", fileConfig(t, path, nil), newTestServices())
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := e.on(0)[0].Payload(); got != "hello\nworld\n" {
		t.Fatalf("payload = %q", got)
	}
	if got := e.on(0)[0].Data["filename"]; got != path {
		t.Errorf("msg.filename = %v", got)
	}
}

// A raw read produces ImmutableBytes rather than []byte, so fanning a large file
// out across several wires shares the buffer instead of copying it per wire.
// That is the entire reason the type exists.
func TestFileInRawReadSharesItsBuffer(t *testing.T) {
	dir := withFileScope(t)
	path := filepath.Join(dir, "raw.bin")
	if err := os.WriteFile(path, []byte{0x01, 0x02, 0x03}, 0o600); err != nil {
		t.Fatal(err)
	}

	n := build(t, "file in", fileConfig(t, path, map[string]any{"encoding": "binary"}),
		newTestServices())
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	buf, ok := e.on(0)[0].Payload().(engine.ImmutableBytes)
	if !ok {
		t.Fatalf("payload is %T, want engine.ImmutableBytes", e.on(0)[0].Payload())
	}
	if len(buf) != 3 || buf[0] != 1 {
		t.Fatalf("payload = %#v", buf)
	}
}

func TestFileInLines(t *testing.T) {
	dir := withFileScope(t)
	path := filepath.Join(dir, "lines.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	n := build(t, "file in", fileConfig(t, path, map[string]any{"format": "lines"}),
		newTestServices())
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	sent := e.on(0)
	if len(sent) != 3 {
		t.Fatalf("sent %d messages, want one per line", len(sent))
	}
	if sent[0].Payload() != "a" || sent[2].Payload() != "c" {
		t.Fatalf("payloads %v .. %v", sent[0].Payload(), sent[2].Payload())
	}
	// The count has to be there, or a Join node downstream never closes the
	// sequence.
	parts, ok := readParts(sent[2])
	if !ok || parts.Index != 2 || parts.Count != 3 {
		t.Fatalf("msg.parts = %#v", sent[2].Data["parts"])
	}
}

func TestFileInChunks(t *testing.T) {
	dir := withFileScope(t)
	path := filepath.Join(dir, "chunks.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 250)), 0o600); err != nil {
		t.Fatal(err)
	}

	n := build(t, "file in", fileConfig(t, path, map[string]any{
		"format": "stream", "ew_chunkSize": 100, "encoding": "utf8",
	}), newTestServices())
	e, err := send(t, n, msg(t, `{}`))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	sent := e.on(0)
	if len(sent) != 3 {
		t.Fatalf("sent %d chunks, want 3", len(sent))
	}
	total := 0
	for _, m := range sent {
		s, _ := m.Payload().(string)
		total += len(s)
	}
	if total != 250 {
		t.Fatalf("chunks total %d bytes, want 250", total)
	}
	// No count while streaming: the total is not known, and inventing one would
	// make a Join node close the sequence early.
	parts, _ := readParts(sent[0])
	if parts.Count != 0 {
		t.Errorf("a streaming chunk claims a count of %d", parts.Count)
	}
}

// Node-RED reads a whole file into memory with no limit, so pointing the node at
// the wrong path is an OOM-kill rather than an error.
func TestFileInRefusesAFileOverItsLimit(t *testing.T) {
	dir := withFileScope(t)
	path := filepath.Join(dir, "big.bin")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 5000)), 0o600); err != nil {
		t.Fatal(err)
	}

	n := build(t, "file in", fileConfig(t, path, map[string]any{"ew_maxBytes": 1024}),
		newTestServices())
	_, err := send(t, n, msg(t, `{}`))
	if err == nil {
		t.Fatal("a file past the read limit was read anyway")
	}
	// The message has to say what to do about it, not just that it failed.
	if !strings.Contains(err.Error(), "chunk") {
		t.Errorf("error %q does not suggest a way to read the file", err)
	}
}

func TestFileInMissingFile(t *testing.T) {
	dir := withFileScope(t)
	path := filepath.Join(dir, "absent.txt")

	t.Run("raises an error by default", func(t *testing.T) {
		n := build(t, "file in", fileConfig(t, path, nil), newTestServices())
		if _, err := send(t, n, msg(t, `{}`)); err == nil {
			t.Fatal("reading a missing file succeeded")
		}
	})

	t.Run("reports it when asked", func(t *testing.T) {
		n := build(t, "file in", fileConfig(t, path, map[string]any{"sendError": true}),
			newTestServices())
		e, err := send(t, n, msg(t, `{}`))
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if len(e.on(0)) != 1 {
			t.Fatalf("sent %d messages, want one reporting the miss", len(e.on(0)))
		}
		if e.on(0)[0].Data["error"] == nil {
			t.Error("the message carries no error detail")
		}
	})
}

// ---------------------------------------------------------------------------
// watch
// ---------------------------------------------------------------------------

func TestWatchReportsChanges(t *testing.T) {
	dir := withFileScope(t)
	watched := filepath.Join(dir, "spool")
	if err := os.MkdirAll(watched, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg, err := jsonConfig(map[string]any{"files": watched, "ew_interval": 0.05})
	if err != nil {
		t.Fatal(err)
	}
	n := build(t, "watch", cfg, newTestServices())
	e := newTestEmitter()
	_, cancel := startNode(t, n, e)
	defer cancel()

	// The first pass records what is already there without emitting, so starting
	// a flow does not announce every existing file as new. Wait several poll
	// intervals to be sure nothing arrives late.
	time.Sleep(300 * time.Millisecond)
	if e.total() != 0 {
		t.Fatalf("the watcher announced %d pre-existing entries", e.total())
	}

	target := filepath.Join(watched, "new.csv")
	if err := os.WriteFile(target, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the new file to be noticed", func() bool { return e.total() >= 1 })

	found := false
	for _, m := range e.on(0) {
		if m.Payload() == target {
			found = true
			if m.Data["change"] != "added" {
				t.Errorf("change = %v, want added", m.Data["change"])
			}
			if m.Data["type"] != "file" {
				t.Errorf("type = %v, want file", m.Data["type"])
			}
			if m.Data["file"] != "new.csv" {
				t.Errorf("file = %v", m.Data["file"])
			}
		}
	}
	if !found {
		t.Fatalf("no message named the new file; got %d messages", e.total())
	}

	// And removal.
	before := e.total()
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, "the removal to be noticed",
		func() bool { return e.total() > before })

	last := e.on(0)[len(e.on(0))-1]
	if last.Data["change"] != "removed" {
		t.Fatalf("change = %v, want removed", last.Data["change"])
	}
}

func TestWatchRefusesAnEmptyList(t *testing.T) {
	withFileScope(t)
	if err := buildErr(t, "watch", `{"files":"  "}`, newTestServices()); err == nil {
		t.Fatal("a watch node with nothing to watch built successfully")
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}
