// Package wasmhost runs WebAssembly guests as flow nodes.
//
// This is the real sandbox. goja is a good boundary — it has no host bindings
// unless you add them — but it cannot bound memory: a JavaScript function that
// allocates in a loop grows the Go heap until the pod is OOM-killed, and the
// only defence is a wall-clock timeout. WebAssembly has a linear memory with a
// declared maximum, so a guest that allocates past its ceiling gets a trap and
// the host is unharmed.
//
// It also means a node can be written in Rust, TinyGo, Zig or AssemblyScript
// rather than JavaScript, which matters for the signal processing an OT flow
// actually wants to do.
//
// wazero is a pure-Go runtime with no cgo, which is what keeps CGO_ENABLED=0
// and the distroless image true.
package wasmhost

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// The ABI a guest implements.
//
// Deliberately tiny: three exports, two of them allocator hooks. A wide ABI is
// a wide attack surface, and every entry is something a guest author has to get
// right. Everything travels as JSON in linear memory because a guest in any
// language can produce it without a shared schema compiler.
const (
	// ExportProcess is the entry point:
	//   emberwire_process(ptr i32, len i32) -> i64
	// The result packs an offset in the high 32 bits and a length in the low 32,
	// pointing at a JSON response in the guest's own memory.
	ExportProcess = "emberwire_process"

	// ExportAlloc lets the host place the input inside the guest's allocator
	// rather than writing over memory the guest believes it owns:
	//   emberwire_alloc(size i32) -> i32
	ExportAlloc = "emberwire_alloc"

	// ExportFree returns a buffer. Optional: a guest with an arena or a bump
	// allocator has nothing to do here.
	//   emberwire_free(ptr i32, size i32)
	ExportFree = "emberwire_free"
)

// Limits bound one guest instance.
type Limits struct {
	// MaxMemoryBytes is the hard ceiling on the guest's linear memory. This is
	// the guarantee goja cannot give: past it, the guest's allocation traps and
	// the host keeps running.
	MaxMemoryBytes uint64

	// Timeout bounds one call.
	Timeout time.Duration

	// MaxOutputBytes bounds the response a guest may return, so a guest cannot
	// make the host allocate without limit by reporting a huge length.
	MaxOutputBytes uint32
}

// Defaults. Small on purpose: this runs on edge hardware alongside the flows
// that matter, and a node that needs more should say so explicitly.
const (
	DefaultMaxMemoryBytes = 64 << 20 // 64 MiB
	DefaultTimeout        = 5 * time.Second
	DefaultMaxOutputBytes = 8 << 20 // 8 MiB

	// wasmPageSize is fixed by the specification.
	wasmPageSize = 65536
)

func (l Limits) withDefaults() Limits {
	if l.MaxMemoryBytes == 0 {
		l.MaxMemoryBytes = DefaultMaxMemoryBytes
	}
	if l.Timeout <= 0 {
		l.Timeout = DefaultTimeout
	}
	if l.MaxOutputBytes == 0 {
		l.MaxOutputBytes = DefaultMaxOutputBytes
	}
	return l
}

// Errors a caller can distinguish.
var (
	ErrTimeout      = errors.New("wasm guest timed out")
	ErrOutOfMemory  = errors.New("wasm guest exceeded its memory limit")
	ErrGuestTrapped = errors.New("wasm guest trapped")
)

// Module is a compiled guest, shared across invocations of one node.
type Module struct {
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	limits   Limits
	name     string

	// Instances are pooled. Compiling is expensive and instantiating is not
	// free either, but an instance carries guest state, so pooling also means a
	// guest can legitimately keep state across messages — which is what makes a
	// filter or an integrator implementable.
	mu   sync.Mutex
	pool []api.Module
}

// Compile prepares a guest.
//
// The runtime is configured with no filesystem, no clock beyond what WASI
// needs, no network and no environment. A guest can compute and return; it
// cannot reach out.
func Compile(ctx context.Context, name string, wasm []byte, limits Limits) (*Module, error) {
	l := limits.withDefaults()

	pages := uint32(l.MaxMemoryBytes / wasmPageSize)
	if pages == 0 {
		pages = 1
	}

	cfg := wazero.NewRuntimeConfig().
		// The ceiling is enforced by the runtime itself: a memory.grow past it
		// returns -1 to the guest, and a guest that ignores that traps on the
		// next access. Either way the host is unaffected.
		WithMemoryLimitPages(pages).
		WithCloseOnContextDone(true)

	rt := wazero.NewRuntimeWithConfig(ctx, cfg)

	// WASI, so a guest compiled by TinyGo or Rust's wasm32-wasi target starts at
	// all — both emit calls to it in their entry code. The instance below is
	// given no preopened directories, no args and no environment, so the WASI
	// surface is present but reaches nothing.
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("preparing the WASI environment: %w", err)
	}

	compiled, err := rt.CompileModule(ctx, wasm)
	if err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("compiling guest: %w", err)
	}

	// Fail here rather than on the first message. A guest missing its entry
	// point is a packaging mistake, and finding out at deploy is the difference
	// between a red node and a flow that quietly drops everything.
	if _, ok := compiled.ExportedFunctions()[ExportProcess]; !ok {
		_ = compiled.Close(ctx)
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("guest does not export %s", ExportProcess)
	}
	if _, ok := compiled.ExportedFunctions()[ExportAlloc]; !ok {
		_ = compiled.Close(ctx)
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("guest does not export %s, so the host cannot pass it a message", ExportAlloc)
	}

	return &Module{runtime: rt, compiled: compiled, limits: l, name: name}, nil
}

// Close releases the runtime and every pooled instance.
func (m *Module) Close(ctx context.Context) error {
	m.mu.Lock()
	pool := m.pool
	m.pool = nil
	m.mu.Unlock()

	for _, inst := range pool {
		_ = inst.Close(ctx)
	}
	return m.runtime.Close(ctx)
}

func (m *Module) acquire(ctx context.Context) (api.Module, error) {
	m.mu.Lock()
	if n := len(m.pool); n > 0 {
		inst := m.pool[n-1]
		m.pool = m.pool[:n-1]
		m.mu.Unlock()
		return inst, nil
	}
	m.mu.Unlock()

	cfg := wazero.NewModuleConfig().
		WithName(""). // anonymous, so several instances can coexist
		WithStartFunctions("_initialize", "_start")
	return m.runtime.InstantiateModule(ctx, m.compiled, cfg)
}

func (m *Module) release(ctx context.Context, inst api.Module, healthy bool) {
	// A trapped instance is not reused. A trap can leave the guest's allocator
	// or its own state inconsistent, and handing that to the next message
	// produces failures that look random.
	if !healthy {
		_ = inst.Close(ctx)
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pool) >= 8 {
		go func() { _ = inst.Close(context.Background()) }()
		return
	}
	m.pool = append(m.pool, inst)
}

// Request is what a guest receives.
type Request struct {
	Msg    map[string]any `json:"msg"`
	Config map[string]any `json:"config,omitempty"`
}

// Response is what a guest returns.
type Response struct {
	// Messages by output port. A nil entry sends nothing on that port.
	Send [][]map[string]any `json:"send,omitempty"`
	// Status badge, if the guest set one.
	Status *Status `json:"status,omitempty"`
	// Error, if the guest wants to fail the message rather than trap.
	Error string `json:"error,omitempty"`
	// Log lines.
	Logs []string `json:"logs,omitempty"`
}

// Status is a node badge.
type Status struct {
	Fill  string `json:"fill,omitempty"`
	Shape string `json:"shape,omitempty"`
	Text  string `json:"text,omitempty"`
}

// Call runs the guest against one message.
func (m *Module) Call(ctx context.Context, req Request) (*Response, error) {
	callCtx, cancel := context.WithTimeout(ctx, m.limits.Timeout)
	defer cancel()

	inst, err := m.acquire(callCtx)
	if err != nil {
		return nil, fmt.Errorf("instantiating guest: %w", err)
	}

	healthy := true
	defer func() { m.release(context.Background(), inst, healthy) }()

	input, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encoding request: %w", err)
	}

	mem := inst.Memory()
	if mem == nil {
		healthy = false
		return nil, fmt.Errorf("guest exports no memory")
	}

	alloc := inst.ExportedFunction(ExportAlloc)
	process := inst.ExportedFunction(ExportProcess)
	free := inst.ExportedFunction(ExportFree)

	// Ask the guest's own allocator for space. Writing anywhere else would
	// corrupt whatever the guest thinks it owns there.
	allocRes, err := alloc.Call(callCtx, uint64(len(input)))
	if err != nil {
		healthy = false
		return nil, classify(err, "allocating in the guest")
	}
	if len(allocRes) == 0 || allocRes[0] == 0 {
		healthy = false
		return nil, fmt.Errorf("%w: allocator returned null for %d bytes", ErrOutOfMemory, len(input))
	}
	ptr := uint32(allocRes[0])

	if !mem.Write(ptr, input) {
		healthy = false
		return nil, fmt.Errorf("writing the request into guest memory failed at offset %d", ptr)
	}

	out, err := process.Call(callCtx, uint64(ptr), uint64(len(input)))
	if err != nil {
		healthy = false
		return nil, classify(err, "calling "+ExportProcess)
	}
	if free != nil {
		// Best effort. A guest with a bump allocator may not implement it, and
		// failing the message over a failed free would be absurd.
		_, _ = free.Call(callCtx, uint64(ptr), uint64(len(input)))
	}

	if len(out) == 0 {
		return &Response{}, nil
	}

	// The packed result: offset in the high half, length in the low half.
	packed := out[0]
	outPtr := uint32(packed >> 32)
	outLen := uint32(packed)

	if outLen == 0 {
		return &Response{}, nil
	}
	if outLen > m.limits.MaxOutputBytes {
		healthy = false
		return nil, fmt.Errorf("guest returned %d bytes, more than the %d limit",
			outLen, m.limits.MaxOutputBytes)
	}

	// Read is bounds-checked against the guest's actual memory size, so a guest
	// reporting a length past the end of its memory gets an error rather than
	// making the host read something it should not.
	data, ok := mem.Read(outPtr, outLen)
	if !ok {
		healthy = false
		return nil, fmt.Errorf("guest returned a response at %d+%d, outside its memory", outPtr, outLen)
	}

	// Copied out before the instance is reused: the returned slice aliases the
	// guest's linear memory, and the next call would overwrite it underneath us.
	buf := make([]byte, len(data))
	copy(buf, data)

	if free != nil {
		_, _ = free.Call(callCtx, uint64(outPtr), uint64(outLen))
	}

	var resp Response
	if err := json.Unmarshal(buf, &resp); err != nil {
		return nil, fmt.Errorf("decoding the guest's response: %w", err)
	}
	return &resp, nil
}

// classify turns a wazero failure into something the flow author can act on.
func classify(err error, during string) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w while %s", ErrTimeout, during)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("cancelled while %s", during)
	}

	// wazero reports a trap as a sys.ExitError or a plain error whose text names
	// the trap. Memory exhaustion surfaces as an out-of-bounds access, because
	// that is what a guest ignoring a failed memory.grow actually does.
	msg := err.Error()
	switch {
	case contains(msg, "out of bounds memory access"), contains(msg, "memory size exceeded"):
		return fmt.Errorf("%w (%s): %v", ErrOutOfMemory, during, err)
	case contains(msg, "unreachable"), contains(msg, "integer divide by zero"),
		contains(msg, "invalid conversion"), contains(msg, "indirect call type mismatch"):
		return fmt.Errorf("%w (%s): %v", ErrGuestTrapped, during, err)
	}
	return fmt.Errorf("%s: %w", during, err)
}

func contains(haystack, needle string) bool {
	if len(needle) > len(haystack) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// PackResult builds the i64 a guest must return: offset in the high 32 bits,
// length in the low 32. Exported so a Go guest and the host cannot disagree
// about the layout.
func PackResult(ptr, length uint32) uint64 {
	return uint64(ptr)<<32 | uint64(length)
}

// UnpackResult is the inverse, for tests and for guest authors.
func UnpackResult(packed uint64) (ptr, length uint32) {
	return uint32(packed >> 32), uint32(packed)
}

// ensure binary is used; the encoding is little-endian on the wasm side and the
// packing above is the only place the host and guest agree on a layout.
var _ = binary.LittleEndian
