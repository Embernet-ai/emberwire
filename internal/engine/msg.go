package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"time"
)

// Msg is a message travelling along a wire.
//
// Node-RED messages are arbitrary JavaScript objects with three conventional
// properties — payload, topic and _msgid — and any number of user-defined ones.
// Modelling that as a map rather than a struct is not laziness: flows address
// properties by runtime string expression (see property.go), and a struct cannot
// answer msg["whatever the user typed"].
//
// A Msg is owned by exactly one node at a time. The scheduler clones on fan-out,
// so a node may mutate the message it was handed without coordinating with
// anybody. It must not retain a reference after returning.
type Msg struct {
	// Data holds every property, including payload, topic and _msgid.
	Data map[string]any
}

// Conventional property names, spelled once so a typo cannot diverge.
const (
	PropPayload  = "payload"
	PropTopic    = "topic"
	PropMsgID    = "_msgid"
	PropError    = "error"
	PropParts    = "parts"
	PropComplete = "complete"
)

// NewMsg returns an empty message carrying a freshly generated id.
func NewMsg() *Msg {
	return &Msg{Data: map[string]any{PropMsgID: GenerateID()}}
}

// NewMsgWithPayload is the common case: a message carrying a single payload.
func NewMsgWithPayload(payload any) *Msg {
	return &Msg{Data: map[string]any{
		PropMsgID:   GenerateID(),
		PropPayload: payload,
	}}
}

// WrapMsg adopts an existing map as a message, generating an id if it has none.
// The map is taken by reference, not copied — used when decoding a message that
// arrived over the wire.
func WrapMsg(data map[string]any) *Msg {
	if data == nil {
		data = map[string]any{}
	}
	m := &Msg{Data: data}
	m.EnsureID()
	return m
}

// GenerateID returns a 16-character hex identifier.
//
// Node-RED's RED.util.generateId concatenates eight random bytes as hex, and
// node ids, message ids and group ids all share that shape. Matching it means an
// Emberwire-generated id is indistinguishable from a Node-RED one, so exported
// flows stay interchangeable. crypto/rand replaces Math.random; the extra cost is
// irrelevant next to the work of actually moving a message.
func GenerateID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand cannot fail on any platform we ship to. If it somehow
		// does, a time-derived id keeps the runtime moving rather than taking
		// the process down over an identifier.
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (8 * i))
		}
	}
	return hex.EncodeToString(b[:])
}

// ID returns the message id, or "" if it is absent or not a string.
func (m *Msg) ID() string {
	if m == nil {
		return ""
	}
	if s, ok := m.Data[PropMsgID].(string); ok {
		return s
	}
	return ""
}

// EnsureID assigns a message id if one is missing, returning the id in use.
// Node-RED does this on receive so that every message is traceable in the debug
// sidebar even when a node fabricated it without one.
func (m *Msg) EnsureID() string {
	if id := m.ID(); id != "" {
		return id
	}
	id := GenerateID()
	m.Data[PropMsgID] = id
	return id
}

// Payload returns msg.payload.
func (m *Msg) Payload() any { return m.Data[PropPayload] }

// SetPayload sets msg.payload.
func (m *Msg) SetPayload(v any) { m.Data[PropPayload] = v }

// Topic returns msg.topic as a string, or "" if absent or not a string.
func (m *Msg) Topic() string {
	if s, ok := m.Data[PropTopic].(string); ok {
		return s
	}
	return ""
}

// SetTopic sets msg.topic.
func (m *Msg) SetTopic(t string) { m.Data[PropTopic] = t }

// Get reads a property expression such as "payload.value" or "payload[0].id".
// The boolean distinguishes a property that is absent from one that is present
// and nil — switch nodes rely on telling those apart.
func (m *Msg) Get(expr string) (any, bool, error) {
	return GetProperty(m.Data, expr)
}

// Set writes a property expression, creating intermediate containers as needed.
func (m *Msg) Set(expr string, v any) error {
	if err := SetProperty(m.Data, expr, v); err != nil {
		return err
	}
	// Mutation uses pointer slices internally so that growth propagates up the
	// walk; normalise them away before anything else observes the message.
	Denormalise(m.Data)
	return nil
}

// Delete removes a property expression. Removing an absent property is a no-op.
func (m *Msg) Delete(expr string) error { return DeleteProperty(m.Data, expr) }

// ImmutableBytes is a byte slice whose contents are guaranteed never to change
// after construction. Clone shares it instead of copying, which is what keeps
// large binary payloads — a file read, an HTTP body, a captured frame — cheap to
// fan out across many wires.
//
// The guarantee is the producer's to keep. Any node that might mutate its buffer
// must emit a plain []byte, which Clone copies defensively.
type ImmutableBytes []byte

// Bytes returns the underlying slice. Callers must not write to it.
func (b ImmutableBytes) Bytes() []byte { return b }

// MarshalJSON encodes ImmutableBytes exactly as []byte does, so the wire format
// does not depend on which of the two a node chose to emit.
func (b ImmutableBytes) MarshalJSON() ([]byte, error) { return json.Marshal([]byte(b)) }

// containerID returns a stable identity for a map or slice header, used to spot
// a container that is already on the current clone path.
//
// Maps and slices are not comparable in Go, so they cannot be map keys directly.
// reflect's Pointer gives the underlying buffer address, which is exactly the
// identity a cycle check needs. A zero-length slice may have no allocation and
// reports 0; that is fine, because an empty container cannot contain a cycle.
func containerID(v any) uintptr {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Map, reflect.Slice:
		if rv.IsNil() {
			return 0
		}
		return rv.Pointer()
	default:
		return 0
	}
}

// maxCloneDepth bounds recursion in Clone. Messages are data, but they arrive
// from the admin API, from MQTT, from HTTP bodies and from JS functions, so the
// depth is attacker-influenced. Node-RED inherits V8's stack limit and crashes
// the process; we refuse the branch and keep running.
const maxCloneDepth = 512

// Clone returns a deep copy of the message.
//
// This deliberately diverges from Node-RED. Node-RED hands the *first* recipient
// on a wire the original object and clones only for recipients after it, which
// makes the last-wired branch share mutable state with the sender. That is a
// documented memory optimisation and an undocumented source of aliasing bugs —
// two branches editing what looks like their own message and stepping on each
// other. Emberwire clones for every recipient. The cost is bounded by the
// ImmutableBytes fast path above, which covers the payloads big enough for the
// copy to matter.
func (m *Msg) Clone() *Msg {
	if m == nil {
		return nil
	}
	return &Msg{Data: cloneMap(m.Data, 0, make(map[any]bool))}
}

func cloneMap(src map[string]any, depth int, seen map[any]bool) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = cloneValue(v, depth+1, seen)
	}
	return dst
}

// cloneValue deep-copies one value.
//
// seen tracks the containers on the current path so a self-referential message —
// which a Function node can trivially construct with msg.self = msg — terminates
// instead of recursing forever. A cycle is replaced with nil rather than a
// back-reference, because the message is about to be JSON-encoded for the debug
// sidebar and a cycle cannot survive that anyway.
func cloneValue(v any, depth int, seen map[any]bool) any {
	if depth > maxCloneDepth {
		return nil
	}

	switch t := v.(type) {
	case nil:
		return nil

	// Scalars are copied by assignment. Listed explicitly rather than falling
	// through to default so that adding a new case is a deliberate act.
	case bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64,
		time.Time, json.Number:
		return v

	case ImmutableBytes:
		// The whole point: shared, not copied.
		return t

	case []byte:
		cp := make([]byte, len(t))
		copy(cp, t)
		return cp

	case map[string]any:
		if ptr := containerID(t); ptr != 0 {
			if seen[ptr] {
				return nil
			}
			seen[ptr] = true
			defer delete(seen, ptr)
		}
		return cloneMap(t, depth, seen)

	case []any:
		if ptr := containerID(t); ptr != 0 {
			if seen[ptr] {
				return nil
			}
			seen[ptr] = true
			defer delete(seen, ptr)
		}
		cp := make([]any, len(t))
		for i := range t {
			cp[i] = cloneValue(t[i], depth+1, seen)
		}
		return cp

	case *[]any:
		if t == nil {
			return nil
		}
		cp := make([]any, len(*t))
		for i := range *t {
			cp[i] = cloneValue((*t)[i], depth+1, seen)
		}
		return cp

	case []string:
		cp := make([]string, len(t))
		copy(cp, t)
		return cp

	case []float64:
		cp := make([]float64, len(t))
		copy(cp, t)
		return cp

	case map[string]string:
		cp := make(map[string]string, len(t))
		for k, s := range t {
			cp[k] = s
		}
		return cp

	case error:
		// Errors reach messages via msg.error. They are immutable in practice
		// and not always copyable, so share the value.
		return t

	default:
		// Anything else — a node's private struct, a net.Conn handed along by a
		// config node — is shared. Copying it would need reflection and would
		// break types that must not be duplicated.
		return v
	}
}
