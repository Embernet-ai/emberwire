// Package store holds Emberwire's persistence: the context stores, the flow
// file, and credentials.
package store

import (
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/embernet-ai/emberwire/internal/engine"
	"github.com/embernet-ai/emberwire/internal/node"
)

// MemoryContext is a volatile context store.
//
// It is the default for node-scoped context and the fallback when no persistent
// store is configured, matching Node-RED's "memory" store. Values do not survive
// a restart; for anything that must, see the bbolt-backed store.
//
// Every operation is atomic with respect to every other, which is what makes
// CompareAndSwap and Increment meaningful. Node-RED's context API offers only
// get and set, and FlowFuse identifies that gap as the specific reason a
// Node-RED flow cannot safely be run in more than one instance: two copies doing
// get-modify-set on a shared counter race with no primitive available to fix it.
type MemoryContext struct {
	mu   sync.RWMutex
	data map[string]any
}

// NewMemoryContext returns an empty in-memory store.
func NewMemoryContext() *MemoryContext {
	return &MemoryContext{data: map[string]any{}}
}

var _ node.Context = (*MemoryContext)(nil)

// Get returns the value at key. Values are cloned on the way out so that a
// caller mutating what it reads cannot corrupt the store — the failure mode
// where two nodes share a context object and one edits it in place.
func (c *MemoryContext) Get(key string) (any, bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.data[key]
	if !ok {
		return nil, false, nil
	}
	return cloneContextValue(v), true, nil
}

// Set stores a value, cloning it on the way in for the same reason Get clones on
// the way out.
func (c *MemoryContext) Set(key string, value any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if value == nil {
		// Node-RED treats setting undefined as a delete, and flows rely on it
		// to clear a key.
		delete(c.data, key)
		return nil
	}
	c.data[key] = cloneContextValue(value)
	return nil
}

// Keys returns every key, sorted so that iteration order is stable.
func (c *MemoryContext) Keys() ([]string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.data))
	for k := range c.data {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Delete removes a key.
func (c *MemoryContext) Delete(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
	return nil
}

// CompareAndSwap sets key to next only if its current value deep-equals prev,
// reporting whether the swap happened. An absent key compares equal to a nil
// prev, so a caller can use CAS to claim a key exactly once.
func (c *MemoryContext) CompareAndSwap(key string, prev, next any) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cur, exists := c.data[key]
	if !exists {
		cur = nil
	}
	if !contextEqual(cur, prev) {
		return false, nil
	}
	if next == nil {
		delete(c.data, key)
	} else {
		c.data[key] = cloneContextValue(next)
	}
	return true, nil
}

// Increment adds delta to a numeric key, creating it at zero if absent, and
// returns the new value. A non-numeric existing value is an error rather than a
// silent reset, because silently zeroing a counter hides the bug that set it to
// a string.
func (c *MemoryContext) Increment(key string, delta float64) (float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var cur float64
	if v, ok := c.data[key]; ok {
		f, ok := toFloat(v)
		if !ok {
			return 0, fmt.Errorf("context key %q holds %T, which is not numeric", key, v)
		}
		cur = f
	}
	next := cur + delta
	c.data[key] = next
	return next, nil
}

// Update applies fn to the current value and stores the result atomically. It is
// the general form of CompareAndSwap: no other operation can interleave between
// the read and the write.
//
// fn runs while the store lock is held, so it must not call back into the same
// context store.
func (c *MemoryContext) Update(key string, fn func(cur any) (any, error)) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cur, ok := c.data[key]
	if !ok {
		cur = nil
	}
	next, err := fn(cloneContextValue(cur))
	if err != nil {
		return nil, err
	}
	if next == nil {
		delete(c.data, key)
		return nil, nil
	}
	stored := cloneContextValue(next)
	c.data[key] = stored
	return cloneContextValue(stored), nil
}

// toFloat coerces the numeric types that can reach a context store. Values
// arriving from JSON are float64; values set by a JS function may be int.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}

// contextEqual compares two context values. Numeric types are compared by value
// so that an int 1 written by a JS function matches a float64 1 read back from a
// persisted store — otherwise a CAS would never succeed across a restart.
func contextEqual(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if af, ok := toFloat(a); ok {
		if bf, ok := toFloat(b); ok {
			return af == bf
		}
		return false
	}
	return reflect.DeepEqual(a, b)
}

// cloneContextValue deep-copies a value crossing the store boundary. It reuses
// the message cloner, which already handles cycles, depth limits and the
// ImmutableBytes fast path.
func cloneContextValue(v any) any {
	if v == nil {
		return nil
	}
	m := &engine.Msg{Data: map[string]any{"v": v}}
	return m.Clone().Data["v"]
}

// ScopedContexts holds the three context scopes a node can address, creating
// flow scopes on demand.
type ScopedContexts struct {
	mu     sync.RWMutex
	global node.Context
	flows  map[string]node.Context
	nodes  map[string]node.Context

	// newStore builds a store for a scope that does not exist yet. Swapping it
	// is how the bbolt-backed store is substituted for the memory one.
	newStore func(scope string) node.Context
}

// NewScopedContexts returns a set of context scopes backed by memory stores.
func NewScopedContexts() *ScopedContexts {
	return NewScopedContextsWith(func(string) node.Context { return NewMemoryContext() })
}

// NewScopedContextsWith returns a set of context scopes whose stores are built
// by newStore, keyed by a scope string such as "global", "flow:<tabID>" or
// "node:<nodeID>".
func NewScopedContextsWith(newStore func(scope string) node.Context) *ScopedContexts {
	return &ScopedContexts{
		global:   newStore("global"),
		flows:    map[string]node.Context{},
		nodes:    map[string]node.Context{},
		newStore: newStore,
	}
}

// Global returns the runtime-wide context.
func (s *ScopedContexts) Global() node.Context { return s.global }

// Flow returns the context for a tab or subflow instance, creating it on first
// use.
func (s *ScopedContexts) Flow(flowID string) node.Context {
	return s.lookup(s.flows, flowID, "flow:"+flowID)
}

// Node returns the context private to one node instance.
func (s *ScopedContexts) Node(nodeID string) node.Context {
	return s.lookup(s.nodes, nodeID, "node:"+nodeID)
}

func (s *ScopedContexts) lookup(m map[string]node.Context, key, scope string) node.Context {
	s.mu.RLock()
	c, ok := m[key]
	s.mu.RUnlock()
	if ok {
		return c
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Re-check: another goroutine may have created it between the read unlock
	// and the write lock.
	if c, ok := m[key]; ok {
		return c
	}
	c = s.newStore(scope)
	m[key] = c
	return c
}

// DropNode removes a node's private context. Called when a node is deleted from
// the flow, so that redeploying does not accumulate context for nodes that no
// longer exist — the leak Node-RED's context "clean" pass exists to sweep up.
func (s *ScopedContexts) DropNode(nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.nodes, nodeID)
}

// Clean removes the context of every node that is not in the supplied set of
// live node ids, and every flow scope not in the live flow set.
func (s *ScopedContexts) Clean(liveNodes, liveFlows map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.nodes {
		if !liveNodes[id] {
			delete(s.nodes, id)
		}
	}
	for id := range s.flows {
		if !liveFlows[id] {
			delete(s.flows, id)
		}
	}
}
