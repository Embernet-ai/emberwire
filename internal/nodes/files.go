package nodes

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/filescope"
	"github.com/embernet-ai/emberwire/internal/node"
)

func init() {
	registerFileOut()
	registerFileIn()
	registerWatch()
}

// Files is the process-wide path scope. main installs it from the configuration
// before any flow starts; the zero value refuses everything, so a node can never
// run against an unset scope.
var Files = &filescope.Scope{}

// defaultMaxReadBytes bounds a single file read.
//
// Node-RED reads a whole file into memory with no limit, which turns "point the
// File In node at the wrong path" into an OOM-kill. A megabyte and a half covers
// every configuration file and CSV export anyone puts on a PVC; past that a flow
// should be streaming, and the node says so.
const defaultMaxReadBytes = 16 << 20

// defaultChunkSize is how much a streaming read emits per message.
const defaultChunkSize = 64 << 10

// ---------------------------------------------------------------------------
// filename resolution
// ---------------------------------------------------------------------------

// fileTarget resolves the filename a file node should act on. Node-RED lets the
// name be a literal, a message property, a context key or an environment
// variable, which is how one node writes a file per device.
type fileTarget struct {
	tv  TypedValue
	svc node.Services
}

func newFileTarget(def *node.Definition, valueKey, typeKey string) fileTarget {
	// An older flow file carries the filename as a bare string with no type
	// property, which reads as a literal.
	return fileTarget{
		tv:  ReadTypedValue(def.Node.Raw, valueKey, typeKey, node.TypeStr),
		svc: def.Services,
	}
}

// resolve produces the checked absolute path for one message.
func (t fileTarget) resolve(m *engine.Msg) (string, error) {
	v, ok, err := t.tv.Eval(EvalContext{Msg: m, Services: t.svc})
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("the filename resolved to nothing")
	}
	name := strings.TrimSpace(mustacheString(v))
	if name == "" {
		return "", fmt.Errorf("the filename is empty")
	}
	return Files.Check(name)
}

// ---------------------------------------------------------------------------
// encodings
// ---------------------------------------------------------------------------

// decodePayload turns a message payload into the bytes to write.
func decodePayload(v any, encoding string) ([]byte, error) {
	if raw, ok := textBytes(v); ok {
		switch encoding {
		case "", "utf8", "none", "binary":
			return raw, nil
		case "base64":
			out, err := base64.StdEncoding.DecodeString(string(raw))
			if err != nil {
				return nil, fmt.Errorf("the payload is not valid base64: %w", err)
			}
			return out, nil
		case "hex":
			out, err := hex.DecodeString(strings.TrimSpace(string(raw)))
			if err != nil {
				return nil, fmt.Errorf("the payload is not valid hex: %w", err)
			}
			return out, nil
		}
		return nil, fmt.Errorf("unsupported encoding %q", encoding)
	}
	// Anything structured is rendered the way the rest of the runtime renders
	// values, so a number writes as 5 rather than 5.000000.
	return []byte(mustacheString(v)), nil
}

// encodeContents turns file bytes into a payload.
func encodeContents(raw []byte, encoding string) (any, error) {
	switch encoding {
	case "", "none", "binary":
		// ImmutableBytes rather than []byte: the buffer is never written to
		// after this, so fan-out shares it instead of copying a megabyte per
		// wire. That is the entire reason the type exists.
		return engine.ImmutableBytes(raw), nil
	case "utf8":
		return string(raw), nil
	case "base64":
		return base64.StdEncoding.EncodeToString(raw), nil
	case "hex":
		return hex.EncodeToString(raw), nil
	}
	return nil, fmt.Errorf("unsupported encoding %q", encoding)
}

// ---------------------------------------------------------------------------
// file out
// ---------------------------------------------------------------------------

type fileOutNode struct {
	target    fileTarget
	action    string // append, overwrite, delete
	newline   bool
	createDir bool
	encoding  string
	// syncWrites fsyncs after every write. On by default: this node is what a
	// flow uses to log to the PVC, and a log that loses its last hour to a power
	// cut is not a log.
	syncWrites bool
}

func registerFileOut() {
	node.MustRegister(node.Descriptor{
		Type:         "file",
		Category:     node.CategoryStorage,
		Color:        colorStorage,
		Icon:         "file",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "file",
		LabelProp:    "filename",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Append, overwrite and delete, with the filename from a literal, a " +
				"message property, context or the environment, and utf8, base64, hex " +
				"or raw encodings. The divergence is the path scope: the file nodes " +
				"may only reach the data directory and whatever else the operator " +
				"listed, resolved through symlinks so a link planted on the PVC " +
				"cannot point out of it. Node-RED's file nodes take any path, which " +
				"makes editing a flow equivalent to reading any file the process can. " +
				"Writes are fsynced by default, which Node-RED's are not.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "filename", Kind: node.PropTypedInput, Label: "Filename", Required: true,
				TypeProp: "filenameType",
				TypedInputTypes: []string{node.TypeStr, node.TypeMsg, node.TypeFlow,
					node.TypeGlobal, node.TypeEnv}},
			{Name: "overwriteFile", Kind: node.PropSelect, Label: "Action", Default: "false",
				Options: []node.Option{
					{Value: "false", Label: "Append to the file"},
					{Value: "true", Label: "Overwrite the file"},
					{Value: "delete", Label: "Delete the file"},
				}},
			{Name: "appendNewline", Kind: node.PropBool, Label: "Add a newline after each payload",
				Default: true},
			{Name: "createDir", Kind: node.PropBool, Label: "Create the directory if it is missing"},
			{Name: "encoding", Kind: node.PropSelect, Label: "Encoding", Default: "utf8",
				Options: encodingOptions()},
			{Name: "ew_sync", Kind: node.PropBool, Label: "Flush to disk after every write",
				Default: true,
				Help: "Emberwire's own. Off is faster and loses the tail of the file " +
					"on a power cut, which for a data log is the part that mattered."},
		},
		Help: "Writes the payload to a file, appending by default. The filename may " +
			"come from the message, so one node can write a file per device. " +
			"Restricted to the data directory unless the operator widened it.",
	}, newFileOut)
}

func encodingOptions() []node.Option {
	return []node.Option{
		{Value: "utf8", Label: "UTF-8 text"},
		{Value: "binary", Label: "Raw bytes"},
		{Value: "base64", Label: "base64"},
		{Value: "hex", Label: "hex"},
	}
}

func newFileOut(def *node.Definition) (node.Node, error) {
	n := &fileOutNode{
		target:     newFileTarget(def, "filename", "filenameType"),
		action:     def.Node.PropString("overwriteFile", "false"),
		newline:    def.Node.PropBool("appendNewline", true),
		createDir:  def.Node.PropBool("createDir", false),
		encoding:   orDefault(def.Node.PropString("encoding", ""), "utf8"),
		syncWrites: def.Node.PropBool("ew_sync", true),
	}
	switch n.action {
	case "false", "true", "delete":
	default:
		return nil, fmt.Errorf("unknown action %q", n.action)
	}
	if _, err := encodeContents(nil, n.encoding); err != nil {
		return nil, err
	}
	if n.target.tv.Type == node.TypeStr && strings.TrimSpace(n.target.tv.Value) == "" {
		return nil, fmt.Errorf("no filename configured")
	}
	return n, nil
}

func (n *fileOutNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	path, err := n.target.resolve(m)
	if err != nil {
		return err
	}

	if n.action == "delete" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("deleting %s: %w", path, err)
		}
		out.Send(0, m)
		return nil
	}

	data, err := decodePayload(m.Payload(), n.encoding)
	if err != nil {
		return err
	}
	if n.newline && (len(data) == 0 || data[len(data)-1] != '\n') {
		data = append(data, '\n')
	}

	if n.createDir {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
		}
	}

	flags := os.O_CREATE | os.O_WRONLY
	if n.action == "true" {
		flags |= os.O_TRUNC
	} else {
		flags |= os.O_APPEND
	}

	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	// Closed explicitly rather than deferred: a close error on a written file is
	// a real failure, and swallowing it would report a write that did not land.
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if n.syncWrites {
		if err := f.Sync(); err != nil {
			f.Close()
			return fmt.Errorf("flushing %s: %w", path, err)
		}
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}

	out.Send(0, m)
	return nil
}

// ---------------------------------------------------------------------------
// file in
// ---------------------------------------------------------------------------

type fileInNode struct {
	target    fileTarget
	format    string // "", utf8, lines, stream
	encoding  string
	allProps  bool
	sendError bool
	maxBytes  int64
	chunkSize int
	outProp   string
}

func registerFileIn() {
	node.MustRegister(node.Descriptor{
		Type:         "file in",
		Category:     node.CategoryStorage,
		Color:        colorStorage,
		Icon:         "file",
		Inputs:       1,
		Outputs:      1,
		PaletteLabel: "file in",
		LabelProp:    "filename",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Whole-file, per-line and chunked reads, with utf8, base64, hex or " +
				"raw output. Same path scope as the File node, and for the same " +
				"reason. A read is also size-capped: Node-RED reads a whole file into " +
				"memory with no limit, so pointing the node at the wrong path is an " +
				"OOM-kill rather than an error. Past the cap the node says which " +
				"limit was hit and that per-line or chunked mode would work.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "filename", Kind: node.PropTypedInput, Label: "Filename", Required: true,
				TypeProp: "filenameType",
				TypedInputTypes: []string{node.TypeStr, node.TypeMsg, node.TypeFlow,
					node.TypeGlobal, node.TypeEnv}},
			{Name: "format", Kind: node.PropSelect, Label: "Output", Default: "utf8",
				Options: []node.Option{
					{Value: "utf8", Label: "One message holding the whole file"},
					{Value: "lines", Label: "One message per line"},
					{Value: "stream", Label: "One message per chunk"},
				}},
			{Name: "encoding", Kind: node.PropSelect, Label: "Encoding", Default: "utf8",
				Options: encodingOptions()},
			{Name: "allProps", Kind: node.PropBool, Label: "Keep the incoming message's properties",
				Default: true},
			{Name: "sendError", Kind: node.PropBool, Label: "Send a message when the file is missing",
				Help: "Off means a missing file raises an error to a Catch node instead."},
			{Name: "ew_maxBytes", Kind: node.PropNumber, Label: "Read limit (bytes)",
				Help: "Emberwire's own; Node-RED reads without a limit."},
			{Name: "ew_chunkSize", Kind: node.PropNumber, Label: "Chunk size (bytes)"},
		},
		Help: "Reads a file and sends its contents, whole, a line at a time, or in " +
			"chunks. Restricted to the data directory unless the operator widened it.",
	}, newFileIn)
}

func newFileIn(def *node.Definition) (node.Node, error) {
	n := &fileInNode{
		target:    newFileTarget(def, "filename", "filenameType"),
		format:    def.Node.PropString("format", "utf8"),
		encoding:  orDefault(def.Node.PropString("encoding", ""), "utf8"),
		allProps:  def.Node.PropBool("allProps", true),
		sendError: def.Node.PropBool("sendError", false),
		maxBytes:  int64(def.Node.PropInt("ew_maxBytes", defaultMaxReadBytes)),
		chunkSize: def.Node.PropInt("ew_chunkSize", defaultChunkSize),
		outProp:   engine.PropPayload,
	}
	switch n.format {
	case "", "utf8", "lines", "stream":
	default:
		return nil, fmt.Errorf("unknown output format %q", n.format)
	}
	if n.maxBytes <= 0 {
		n.maxBytes = defaultMaxReadBytes
	}
	if n.chunkSize <= 0 {
		n.chunkSize = defaultChunkSize
	}
	if _, err := encodeContents(nil, n.encoding); err != nil {
		return nil, err
	}
	if n.target.tv.Type == node.TypeStr && strings.TrimSpace(n.target.tv.Value) == "" {
		return nil, fmt.Errorf("no filename configured")
	}
	return n, nil
}

func (n *fileInNode) Receive(_ context.Context, m *engine.Msg, out node.Emitter) error {
	path, err := n.target.resolve(m)
	if err != nil {
		return err
	}

	base := m
	if !n.allProps {
		// Node-RED's "single property" mode: a fresh message carrying only the
		// filename and the contents.
		base = engine.NewMsg()
	}
	base.Data["filename"] = path

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && n.sendError {
			// The flow asked to be told rather than to fail: a missing file is
			// a normal condition for a node polling for a drop.
			miss := base.Clone()
			miss.SetPayload(nil)
			miss.Data["error"] = map[string]any{"message": err.Error(), "source": path}
			out.Send(0, miss)
			return nil
		}
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}

	switch n.format {
	case "lines":
		return n.readLines(f, base, out)
	case "stream":
		return n.readChunks(f, base, out)
	default:
		if info.Size() > n.maxBytes {
			return fmt.Errorf("%s is %d bytes, past the %d byte read limit; "+
				"read it a line or a chunk at a time, or raise ew_maxBytes",
				path, info.Size(), n.maxBytes)
		}
		raw, err := io.ReadAll(io.LimitReader(f, n.maxBytes))
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		payload, err := encodeContents(raw, n.encoding)
		if err != nil {
			return err
		}
		cp := base.Clone()
		if err := cp.Set(n.outProp, payload); err != nil {
			return err
		}
		out.Send(0, cp)
		return nil
	}
}

func (n *fileInNode) readLines(f *os.File, base *engine.Msg, out node.Emitter) error {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), int(n.maxBytes))

	// The line count is not known until the file has been read, so the sequence
	// is collected first and stamped afterwards. A msg.parts with no count is
	// one a Join node cannot close.
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading %s: %w", base.Data["filename"], err)
	}

	seqID := engine.GenerateID()
	for i, line := range lines {
		payload, err := encodeContents([]byte(line), n.encoding)
		if err != nil {
			return err
		}
		cp := base.Clone()
		if err := cp.Set(n.outProp, payload); err != nil {
			return err
		}
		cp.Data[engine.PropParts] = partsInfo{
			ID: seqID, Index: i, Count: len(lines), Type: "string",
		}.toMap()
		out.Send(0, cp)
	}
	return nil
}

func (n *fileInNode) readChunks(f *os.File, base *engine.Msg, out node.Emitter) error {
	seqID := engine.GenerateID()
	buf := make([]byte, n.chunkSize)
	var (
		index int
		total int64
	)

	for {
		nRead, err := f.Read(buf)
		if nRead > 0 {
			total += int64(nRead)
			if total > n.maxBytes {
				return fmt.Errorf("%s is past the %d byte read limit; raise ew_maxBytes",
					base.Data["filename"], n.maxBytes)
			}
			// Copied, because buf is reused on the next iteration and the
			// message outlives this loop.
			chunk := make([]byte, nRead)
			copy(chunk, buf[:nRead])

			payload, encErr := encodeContents(chunk, n.encoding)
			if encErr != nil {
				return encErr
			}
			cp := base.Clone()
			if setErr := cp.Set(n.outProp, payload); setErr != nil {
				return setErr
			}
			// No count: the total is not known while streaming, and inventing
			// one would make a Join node close the sequence early.
			cp.Data[engine.PropParts] = partsInfo{
				ID: seqID, Index: index, Type: "buffer", Len: nRead,
			}.toMap()
			out.Send(0, cp)
			index++
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", base.Data["filename"], err)
		}
	}
}

// ---------------------------------------------------------------------------
// watch
// ---------------------------------------------------------------------------

// defaultWatchInterval is how often the watcher looks.
const defaultWatchInterval = 2 * time.Second

type watchNode struct {
	paths     []string
	recursive bool
	interval  time.Duration
	maxFiles  int

	mu    sync.Mutex
	seen  map[string]watchEntry
	first bool
}

type watchEntry struct {
	size    int64
	modTime time.Time
	isDir   bool
}

func registerWatch() {
	node.MustRegister(node.Descriptor{
		Type:         "watch",
		Category:     node.CategoryStorage,
		Color:        colorStorage,
		Icon:         "watch",
		Inputs:       0,
		Outputs:      1,
		PaletteLabel: "watch",
		LabelProp:    "files",
		Compatibility: node.Compatibility{
			Level: node.CompatDivergent,
			Notes: "Reports files and directories appearing, changing and being removed, " +
				"with the same message shape Node-RED produces. It polls rather than " +
				"using the kernel's notification interface, so a change is seen within " +
				"the poll interval rather than immediately, and two changes inside one " +
				"interval are reported once. That is a deliberate trade: fsnotify " +
				"means per-platform code and a filename suffix that is a build " +
				"constraint, which has already cost this codebase a day. Same path " +
				"scope as the other file nodes, and the number of watched entries is " +
				"capped so a recursive watch on a large tree cannot stall the runtime.",
		},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "files", Kind: node.PropString, Label: "Files or directories", Required: true,
				Help: "Comma-separated. Restricted to the paths the operator allowed."},
			{Name: "recursive", Kind: node.PropBool, Label: "Watch subdirectories"},
			{Name: "ew_interval", Kind: node.PropNumber, Label: "Poll interval (seconds)", Default: 2},
			{Name: "ew_maxFiles", Kind: node.PropNumber, Label: "Entry limit", Default: 10000},
		},
		Help: "Watches files and directories and emits a message when one appears, " +
			"changes or is removed. msg.payload is the path, msg.file its name, " +
			"msg.type \"file\" or \"directory\", and msg.size its size in bytes.",
	}, newWatch)
}

func newWatch(def *node.Definition) (node.Node, error) {
	n := &watchNode{
		recursive: def.Node.PropBool("recursive", false),
		interval:  defaultWatchInterval,
		maxFiles:  def.Node.PropInt("ew_maxFiles", 10000),
		seen:      map[string]watchEntry{},
		first:     true,
	}
	if secs := def.Node.PropFloat("ew_interval", 0); secs > 0 {
		n.interval = time.Duration(secs * float64(time.Second))
	}
	if n.maxFiles <= 0 {
		n.maxFiles = 10000
	}

	for _, p := range strings.Split(def.Node.PropString("files", ""), ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		checked, err := Files.Check(p)
		if err != nil {
			return nil, err
		}
		n.paths = append(n.paths, checked)
	}
	if len(n.paths) == 0 {
		return nil, fmt.Errorf("no files or directories to watch")
	}
	return n, nil
}

// Receive lets a watch node be polled on demand, which is how a flow forces a
// check without waiting for the interval.
func (n *watchNode) Receive(_ context.Context, _ *engine.Msg, out node.Emitter) error {
	n.poll(out)
	return nil
}

func (n *watchNode) Start(ctx context.Context, out node.Emitter) error {
	go func() {
		// The first pass records what is already there without emitting, so
		// starting a flow does not announce every existing file as new.
		n.poll(out)

		t := time.NewTicker(n.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n.poll(out)
			}
		}
	}()
	return nil
}

func (n *watchNode) poll(out node.Emitter) {
	current := make(map[string]watchEntry, len(n.seen))
	truncated := false

	for _, root := range n.paths {
		if !n.collect(root, current, &truncated) {
			break
		}
	}

	n.mu.Lock()
	first := n.first
	n.first = false
	previous := n.seen
	n.seen = current
	n.mu.Unlock()

	if truncated {
		out.Log(node.LogWarn, "the watch covers more than %d entries; the rest are not being watched",
			n.maxFiles)
	}
	if first {
		return
	}

	// Sorted, so a batch of changes arrives in the same order every time and a
	// downstream sequence is reproducible.
	for _, path := range sortedWatchKeys(current) {
		entry := current[path]
		prev, existed := previous[path]
		switch {
		case !existed:
			n.emit(out, path, entry, "added")
		case entry.size != prev.size || !entry.modTime.Equal(prev.modTime):
			n.emit(out, path, entry, "changed")
		}
	}
	for _, path := range sortedWatchKeys(previous) {
		if _, still := current[path]; !still {
			n.emit(out, path, previous[path], "removed")
		}
	}
}

// collect stats a path and, for a directory, its entries. It returns false when
// the entry cap was reached, so the caller stops walking.
func (n *watchNode) collect(path string, into map[string]watchEntry, truncated *bool) bool {
	if len(into) >= n.maxFiles {
		*truncated = true
		return false
	}

	info, err := os.Lstat(path)
	if err != nil {
		// A watched path that does not exist is not an error: watching for a
		// file to appear is the ordinary use of this node.
		return true
	}
	into[path] = watchEntry{size: info.Size(), modTime: info.ModTime(), isDir: info.IsDir()}
	if !info.IsDir() {
		return true
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return true
	}
	for _, e := range entries {
		child := filepath.Join(path, e.Name())
		if e.IsDir() && !n.recursive {
			ei, err := e.Info()
			if err != nil {
				continue
			}
			if len(into) >= n.maxFiles {
				*truncated = true
				return false
			}
			into[child] = watchEntry{size: ei.Size(), modTime: ei.ModTime(), isDir: true}
			continue
		}
		if !n.collect(child, into, truncated) {
			return false
		}
	}
	return true
}

func (n *watchNode) emit(out node.Emitter, path string, e watchEntry, change string) {
	m := engine.NewMsg()
	m.SetPayload(path)
	m.SetTopic(path)
	m.Data["file"] = filepath.Base(path)
	m.Data["filename"] = path
	m.Data["size"] = float64(e.size)
	m.Data["change"] = change
	if e.isDir {
		m.Data["type"] = "directory"
	} else {
		m.Data["type"] = "file"
	}
	out.Send(0, m)
}

func sortedWatchKeys(m map[string]watchEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
