// Package js runs the Function node's JavaScript.
//
// Node-RED uses Node's own `vm` module, which its documentation states plainly
// is "not a security mechanism" and must not be used to run untrusted code. Its
// trust model is that anyone who can deploy a flow already owns the box — the
// exec node is right there — which is a defensible position for a tool you run
// on your own laptop and a poor one for something on a customer's plant floor.
//
// goja is a JavaScript interpreter written in Go. It has no host bindings at
// all unless you add them: no process, no require, no filesystem, no network.
// The escape surface is not "small", it is the set of things deliberately put
// into the runtime by the code below. It is also pure Go, which is what keeps
// CGO_ENABLED=0 and the distroless image true.
//
// What that costs: no npm. A Function node that requires a package does not
// work here and cannot be made to without embedding Node. That is stated in the
// compatibility matrix and never papered over.
package js

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/dop251/goja"
	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

// Limits bound what one invocation may do.
type Limits struct {
	// Timeout bounds wall-clock time per message. Node-RED's equivalent is
	// optional and off by default; here there is always a ceiling, because an
	// infinite loop in a Function node must not be able to hold a runtime
	// goroutine forever.
	Timeout time.Duration

	// MaxOutputBytes bounds the JSON size of the messages one call may emit.
	// A function that builds an ever-growing array is a slow OOM otherwise.
	MaxOutputBytes int
}

// Defaults.
const (
	DefaultTimeout        = 5 * time.Second
	DefaultMaxOutputBytes = 16 << 20 // 16 MiB
)

func (l Limits) withDefaults() Limits {
	if l.Timeout <= 0 {
		l.Timeout = DefaultTimeout
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = DefaultMaxOutputBytes
	}
	return l
}

// ErrTimeout is returned when a function exceeds its time limit.
var ErrTimeout = errors.New("function timed out")

// Program is a compiled function body, shared by every invocation of one node.
//
// Compiling once at deploy rather than per message matters: goja's parser is
// the expensive part, and a flow at a few thousand messages a second would
// spend most of its time re-parsing the same source.
type Program struct {
	main    *goja.Program
	onStart *goja.Program
	onStop  *goja.Program
	limits  Limits
	name    string

	// pool reuses runtimes. A goja.Runtime is not safe for concurrent use, and
	// building one plus its globals costs far more than running a short
	// function, so they are recycled rather than rebuilt.
	pool sync.Pool
}

// Compile prepares a function body.
//
// The user's code is wrapped in a function expression so that `return` works at
// the top level, which is how every Node-RED function is written.
func Compile(name, body, onStart, onStop string, limits Limits) (*Program, error) {
	p := &Program{limits: limits.withDefaults(), name: name}

	main, err := goja.Compile(name, "(function(msg, node, context, flow, global, env, util){"+body+"\n})", true)
	if err != nil {
		return nil, fmt.Errorf("compiling function: %w", cleanCompileError(err))
	}
	p.main = main

	if strings.TrimSpace(onStart) != "" {
		prog, err := goja.Compile(name+":onStart",
			"(function(node, context, flow, global, env, util){"+onStart+"\n})", true)
		if err != nil {
			return nil, fmt.Errorf("compiling setup code: %w", cleanCompileError(err))
		}
		p.onStart = prog
	}
	if strings.TrimSpace(onStop) != "" {
		prog, err := goja.Compile(name+":onStop",
			"(function(node, context, flow, global, env, util){"+onStop+"\n})", true)
		if err != nil {
			return nil, fmt.Errorf("compiling cleanup code: %w", cleanCompileError(err))
		}
		p.onStop = prog
	}
	return p, nil
}

// cleanCompileError strips the wrapper from a syntax error's reported position
// so the message points at the user's code rather than at generated text they
// never wrote.
func cleanCompileError(err error) error {
	msg := err.Error()
	msg = strings.ReplaceAll(msg, "(function(msg, node, context, flow, global, env, util){", "")
	msg = strings.ReplaceAll(msg, "(function(node, context, flow, global, env, util){", "")
	return errors.New(msg)
}

// Result is what one invocation produced.
type Result struct {
	// Messages, indexed by output port. A nil entry sends nothing on that port.
	ByPort [][]*engine.Msg
	// Status set by the function, if any.
	Status *node.Status
	// Logs emitted through node.log and friends.
	Logs []LogEntry
	// Errors raised through node.error.
	Errors []string
	// Done reports whether the function called node.done().
	Done bool
}

// LogEntry is one line from the function.
type LogEntry struct {
	Level   node.LogLevel
	Message string
}

// Sandbox is the host state one invocation may reach.
type Sandbox struct {
	Msg      *engine.Msg
	Services node.Services
	Outputs  int
}

// Run executes the function against a message.
func (p *Program) Run(ctx context.Context, sb Sandbox) (*Result, error) {
	vm := p.acquire()
	defer p.release(vm)

	res := &Result{ByPort: make([][]*engine.Msg, sb.Outputs)}

	// The interrupt is what makes the timeout real. goja checks for it between
	// operations, so `while(true){}` is stopped rather than spinning a core
	// until the pod is killed — which is exactly what happens in Node-RED when
	// its optional timeout is not configured.
	timer := time.AfterFunc(p.limits.Timeout, func() {
		vm.Interrupt(ErrTimeout)
	})
	defer timer.Stop()

	stop := context.AfterFunc(ctx, func() {
		vm.Interrupt(ctx.Err())
	})
	defer stop()

	fn, err := p.callable(vm, p.main)
	if err != nil {
		return nil, err
	}

	msgObj := vm.ToValue(sb.Msg.Data)
	nodeObj := p.nodeAPI(vm, sb, res)
	ctxObj := p.contextAPI(vm, sb.Services, node.ScopeNode)
	flowObj := p.contextAPI(vm, sb.Services, node.ScopeFlow)
	globalObj := p.contextAPI(vm, sb.Services, node.ScopeGlobal)
	envObj := p.envAPI(vm, sb.Services)
	utilObj := p.utilAPI(vm)

	out, err := fn(goja.Undefined(), msgObj, nodeObj, ctxObj, flowObj, globalObj, envObj, utilObj)
	vm.ClearInterrupt()
	if err != nil {
		return res, translateError(err)
	}

	// A returned value routes to outputs the same way Node-RED's does: a bare
	// object goes to port 0, an array indexes ports, and a nested array sends
	// several messages on one port.
	if err := p.collectReturn(vm, out, sb, res); err != nil {
		return res, err
	}
	if err := p.checkSize(res); err != nil {
		return res, err
	}
	return res, nil
}

// RunLifecycle executes the setup or cleanup code.
func (p *Program) RunLifecycle(ctx context.Context, sb Sandbox, which string) (*Result, error) {
	var prog *goja.Program
	switch which {
	case "start":
		prog = p.onStart
	case "stop":
		prog = p.onStop
	}
	if prog == nil {
		return &Result{}, nil
	}

	vm := p.acquire()
	defer p.release(vm)

	res := &Result{ByPort: make([][]*engine.Msg, sb.Outputs)}

	timer := time.AfterFunc(p.limits.Timeout, func() { vm.Interrupt(ErrTimeout) })
	defer timer.Stop()
	stop := context.AfterFunc(ctx, func() { vm.Interrupt(ctx.Err()) })
	defer stop()

	fn, err := p.callable(vm, prog)
	if err != nil {
		return nil, err
	}
	_, err = fn(goja.Undefined(),
		p.nodeAPI(vm, sb, res),
		p.contextAPI(vm, sb.Services, node.ScopeNode),
		p.contextAPI(vm, sb.Services, node.ScopeFlow),
		p.contextAPI(vm, sb.Services, node.ScopeGlobal),
		p.envAPI(vm, sb.Services),
		p.utilAPI(vm))
	vm.ClearInterrupt()
	if err != nil {
		return res, translateError(err)
	}
	return res, nil
}

func (p *Program) callable(vm *goja.Runtime, prog *goja.Program) (goja.Callable, error) {
	v, err := vm.RunProgram(prog)
	if err != nil {
		return nil, translateError(err)
	}
	fn, ok := goja.AssertFunction(v)
	if !ok {
		return nil, fmt.Errorf("internal: compiled body is not callable")
	}
	return fn, nil
}

// acquire returns a runtime from the pool, or a fresh one.
func (p *Program) acquire() *goja.Runtime {
	if v := p.pool.Get(); v != nil {
		return v.(*goja.Runtime)
	}
	return newRuntime()
}

func (p *Program) release(vm *goja.Runtime) {
	vm.ClearInterrupt()
	p.pool.Put(vm)
}

// newRuntime builds a bare runtime.
//
// Nothing is registered here. goja starts with the ECMAScript built-ins and
// nothing else — no require, no process, no fs, no net — and everything the
// function can reach is passed in as an argument on each call. That is the
// whole security story, and it is why it fits in one short function.
func newRuntime() *goja.Runtime {
	vm := goja.New()
	// Field names are used verbatim rather than lowercased, so msg.payload in
	// Go is msg.payload in JS. Without this, goja's default mapper would make
	// Go struct fields visible under different names.
	vm.SetFieldNameMapper(nil)
	return vm
}

// nodeAPI builds the `node` object.
func (p *Program) nodeAPI(vm *goja.Runtime, sb Sandbox, res *Result) goja.Value {
	obj := vm.NewObject()

	must := func(name string, fn func(goja.FunctionCall) goja.Value) {
		if err := obj.Set(name, fn); err != nil {
			panic("emberwire: building the node API: " + err.Error())
		}
	}

	must("send", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) == 0 {
			return goja.Undefined()
		}
		p.routeSend(vm, call.Argument(0), sb, res)
		return goja.Undefined()
	})

	must("done", func(goja.FunctionCall) goja.Value {
		res.Done = true
		return goja.Undefined()
	})

	must("error", func(call goja.FunctionCall) goja.Value {
		res.Errors = append(res.Errors, call.Argument(0).String())
		return goja.Undefined()
	})

	must("status", func(call goja.FunctionCall) goja.Value {
		s := node.Status{}
		if o, ok := call.Argument(0).Export().(map[string]any); ok {
			s.Fill, _ = o["fill"].(string)
			s.Shape, _ = o["shape"].(string)
			if t, ok := o["text"]; ok {
				s.Text = fmt.Sprint(t)
			}
		} else if !goja.IsUndefined(call.Argument(0)) {
			s.Text = call.Argument(0).String()
		}
		res.Status = &s
		return goja.Undefined()
	})

	for name, level := range map[string]node.LogLevel{
		"log": node.LogInfo, "warn": node.LogWarn, "error_": node.LogError,
		"debug": node.LogDebug, "trace": node.LogTrace,
	} {
		lvl := level
		if name == "error_" {
			continue
		}
		must(name, func(call goja.FunctionCall) goja.Value {
			res.Logs = append(res.Logs, LogEntry{Level: lvl, Message: argsToString(call)})
			return goja.Undefined()
		})
	}

	return obj
}

func argsToString(call goja.FunctionCall) string {
	parts := make([]string, 0, len(call.Arguments))
	for _, a := range call.Arguments {
		parts = append(parts, a.String())
	}
	return strings.Join(parts, " ")
}

// routeSend converts a value passed to node.send into messages per port.
func (p *Program) routeSend(vm *goja.Runtime, v goja.Value, sb Sandbox, res *Result) {
	exported := v.Export()

	// An array indexes output ports.
	if arr, ok := exported.([]any); ok {
		for port, entry := range arr {
			if port >= len(res.ByPort) {
				break
			}
			switch e := entry.(type) {
			case nil:
				// Explicitly nothing on this port.
			case []any:
				for _, m := range e {
					if msg := toMsg(m, sb.Msg); msg != nil {
						res.ByPort[port] = append(res.ByPort[port], msg)
					}
				}
			default:
				if msg := toMsg(entry, sb.Msg); msg != nil {
					res.ByPort[port] = append(res.ByPort[port], msg)
				}
			}
		}
		return
	}

	if msg := toMsg(exported, sb.Msg); msg != nil && len(res.ByPort) > 0 {
		res.ByPort[0] = append(res.ByPort[0], msg)
	}
}

func (p *Program) collectReturn(vm *goja.Runtime, v goja.Value, sb Sandbox, res *Result) error {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return nil
	}
	exported := v.Export()

	// Node-RED errors when a function returns a bare number or string, because
	// the author almost certainly meant to return a message and the silent
	// alternative is a message with no payload appearing downstream.
	switch exported.(type) {
	case string, float64, int64, bool:
		return fmt.Errorf("a function must return a message object or an array of them, not %T", exported)
	}

	p.routeSend(vm, v, sb, res)
	return nil
}

// toMsg converts a returned value into a message.
//
// A returned object is taken as the whole message. It carries the original
// message's id forward when it has none, so tracing survives a function that
// builds a fresh object — which is most of them.
func toMsg(v any, original *engine.Msg) *engine.Msg {
	if v == nil {
		return nil
	}
	data, ok := v.(map[string]any)
	if !ok {
		return nil
	}

	// Carry the id BEFORE wrapping. WrapMsg assigns a fresh one when the map
	// has none, so checking afterwards always finds an id and the original is
	// silently lost — which breaks correlation in the debug sidebar for every
	// function that builds a new object.
	if _, has := data[engine.PropMsgID]; !has && original != nil {
		if id := original.ID(); id != "" {
			data[engine.PropMsgID] = id
		}
	}

	return engine.WrapMsg(Normalise(data).(map[string]any))
}

// Normalise converts values coming out of the JavaScript runtime into the shapes
// the rest of the engine uses.
//
// goja exports an integral number as int64 and a fractional one as float64.
// Everything else in the runtime speaks JSON, where every number is a float64 —
// a message decoded from MQTT, a row read from PostgreSQL, a value loaded from
// the flow file. Letting a function node be the one place that emits int64 means
// a payload of 42 compares, serialises and stores differently depending on
// whether it passed through JavaScript, which is exactly the sort of difference
// nobody finds until it matters.
func Normalise(v any) any {
	switch t := v.(type) {
	case int64:
		return float64(t)
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case uint64:
		return float64(t)
	case uint32:
		return float64(t)
	case float32:
		return float64(t)
	case map[string]any:
		for k, val := range t {
			t[k] = Normalise(val)
		}
		return t
	case []any:
		for i, val := range t {
			t[i] = Normalise(val)
		}
		return t
	default:
		return v
	}
}

// contextAPI builds a context accessor: get, set, keys.
func (p *Program) contextAPI(vm *goja.Runtime, svc node.Services, scope node.ContextScope) goja.Value {
	obj := vm.NewObject()
	if svc == nil {
		return obj
	}
	store := svc.Context(scope)

	_ = obj.Set("get", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		v, ok, err := store.Get(key)
		if err != nil || !ok {
			return goja.Undefined()
		}
		return vm.ToValue(v)
	})

	_ = obj.Set("set", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		if len(call.Arguments) < 2 || goja.IsUndefined(call.Argument(1)) {
			_ = store.Delete(key)
			return goja.Undefined()
		}
		_ = store.Set(key, Normalise(call.Argument(1).Export()))
		return goja.Undefined()
	})

	_ = obj.Set("keys", func(goja.FunctionCall) goja.Value {
		keys, err := store.Keys()
		if err != nil {
			return vm.ToValue([]string{})
		}
		return vm.ToValue(keys)
	})

	// Not in Node-RED. Exposed because the underlying store is transactional and
	// a flow author doing get-modify-set on a shared counter otherwise loses
	// updates with no primitive available to fix it.
	_ = obj.Set("incr", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		delta := 1.0
		if len(call.Arguments) > 1 {
			delta = call.Argument(1).ToFloat()
		}
		next, err := store.Increment(key, delta)
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(next)
	})

	_ = obj.Set("cas", func(call goja.FunctionCall) goja.Value {
		key := call.Argument(0).String()
		swapped, err := store.CompareAndSwap(key, Normalise(call.Argument(1).Export()), Normalise(call.Argument(2).Export()))
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(swapped)
	})

	return obj
}

func (p *Program) envAPI(vm *goja.Runtime, svc node.Services) goja.Value {
	obj := vm.NewObject()
	_ = obj.Set("get", func(call goja.FunctionCall) goja.Value {
		if svc == nil {
			return goja.Undefined()
		}
		v, ok := svc.Env(call.Argument(0).String())
		if !ok {
			return goja.Undefined()
		}
		return vm.ToValue(v)
	})
	return obj
}

// utilAPI is the small standard library a Function node actually needs.
//
// Deliberately narrow. Every entry here is a decision to widen the surface, so
// the list is things that are otherwise impossible in pure JS rather than
// things that are merely convenient.
func (p *Program) utilAPI(vm *goja.Runtime) goja.Value {
	obj := vm.NewObject()

	_ = obj.Set("cloneMessage", func(call goja.FunctionCall) goja.Value {
		data, ok := call.Argument(0).Export().(map[string]any)
		if !ok {
			return call.Argument(0)
		}
		return vm.ToValue(engine.WrapMsg(data).Clone().Data)
	})

	_ = obj.Set("generateId", func(goja.FunctionCall) goja.Value {
		return vm.ToValue(engine.GenerateID())
	})

	_ = obj.Set("base64Encode", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(base64.StdEncoding.EncodeToString([]byte(call.Argument(0).String())))
	})

	_ = obj.Set("base64Decode", func(call goja.FunctionCall) goja.Value {
		b, err := base64.StdEncoding.DecodeString(call.Argument(0).String())
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return vm.ToValue(string(b))
	})

	_ = obj.Set("hexEncode", func(call goja.FunctionCall) goja.Value {
		return vm.ToValue(hex.EncodeToString([]byte(call.Argument(0).String())))
	})

	_ = obj.Set("getProperty", func(call goja.FunctionCall) goja.Value {
		root := call.Argument(0).Export()
		v, ok, err := engine.GetProperty(Normalise(root), call.Argument(1).String())
		if err != nil || !ok {
			return goja.Undefined()
		}
		return vm.ToValue(v)
	})

	_ = obj.Set("setProperty", func(call goja.FunctionCall) goja.Value {
		root, ok := call.Argument(0).Export().(map[string]any)
		if !ok {
			return goja.Undefined()
		}
		if err := engine.SetProperty(root, call.Argument(1).String(), Normalise(call.Argument(2).Export())); err != nil {
			panic(vm.NewGoError(err))
		}
		engine.Denormalise(root)
		return vm.ToValue(root)
	})

	return obj
}

// checkSize enforces the output ceiling.
func (p *Program) checkSize(res *Result) error {
	total := 0
	for _, port := range res.ByPort {
		for _, m := range port {
			b, err := json.Marshal(m.Data)
			if err != nil {
				// Not serialisable is not fatal here — a message may legitimately
				// carry a value only Go understands — so it is not counted.
				continue
			}
			total += len(b)
			if total > p.limits.MaxOutputBytes {
				return fmt.Errorf("function produced more than %d bytes of messages",
					p.limits.MaxOutputBytes)
			}
		}
	}
	return nil
}

// translateError turns a goja failure into something a flow author can act on.
func translateError(err error) error {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		if inner, ok := interrupted.Value().(error); ok {
			if errors.Is(inner, ErrTimeout) {
				return fmt.Errorf("%w after the configured limit", ErrTimeout)
			}
			return inner
		}
		return fmt.Errorf("function was interrupted: %v", interrupted.Value())
	}

	var ex *goja.Exception
	if errors.As(err, &ex) {
		// The stack points into the wrapper, which the author did not write, so
		// only the message is surfaced.
		return errors.New(strings.TrimSpace(ex.Value().String()))
	}
	return err
}
