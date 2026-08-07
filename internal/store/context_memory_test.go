package store

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestMemoryContextBasics(t *testing.T) {
	c := NewMemoryContext()

	if _, ok, err := c.Get("nope"); err != nil || ok {
		t.Errorf("Get on empty store = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if err := c.Set("k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, ok, err := c.Get("k")
	if err != nil || !ok || v != "v" {
		t.Errorf("Get(k) = (%#v, %v, %v)", v, ok, err)
	}

	if err := c.Set("k2", 1.0); err != nil {
		t.Fatalf("Set: %v", err)
	}
	keys, err := c.Keys()
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	if !reflect.DeepEqual(keys, []string{"k", "k2"}) {
		t.Errorf("Keys() = %v, want [k k2] — must be sorted for stable iteration", keys)
	}

	if err := c.Delete("k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := c.Get("k"); ok {
		t.Error("key still present after Delete")
	}
}

func TestMemoryContextSetNilDeletes(t *testing.T) {
	// Node-RED treats setting undefined as a delete and flows use it to clear a
	// key. Storing a literal nil instead would leave the key in Keys().
	c := NewMemoryContext()
	_ = c.Set("k", "v")
	if err := c.Set("k", nil); err != nil {
		t.Fatalf("Set nil: %v", err)
	}
	if _, ok, _ := c.Get("k"); ok {
		t.Error("Set(key, nil) did not remove the key")
	}
	keys, _ := c.Keys()
	if len(keys) != 0 {
		t.Errorf("Keys() = %v after clearing, want empty", keys)
	}
}

func TestMemoryContextClonesAcrossTheBoundary(t *testing.T) {
	// Two nodes sharing a context value and one editing it in place is a real
	// failure mode. Values are copied both in and out.
	c := NewMemoryContext()
	original := map[string]any{"n": 1.0, "list": []any{1.0, 2.0}}

	if err := c.Set("cfg", original); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// Mutating what we handed in must not affect the store.
	original["n"] = 999.0

	got, _, _ := c.Get("cfg")
	m := got.(map[string]any)
	if m["n"] != 1.0 {
		t.Errorf("stored value = %#v; the store aliased the caller's map", m["n"])
	}

	// Mutating what we read out must not affect the store either.
	m["n"] = 777.0
	again, _, _ := c.Get("cfg")
	if again.(map[string]any)["n"] != 1.0 {
		t.Error("mutating a read value corrupted the store")
	}
}

func TestMemoryContextCompareAndSwap(t *testing.T) {
	c := NewMemoryContext()

	// An absent key compares equal to nil, so CAS can claim a key exactly once.
	swapped, err := c.CompareAndSwap("leader", nil, "node-a")
	if err != nil || !swapped {
		t.Fatalf("first claim: swapped=%v err=%v, want true nil", swapped, err)
	}
	swapped, err = c.CompareAndSwap("leader", nil, "node-b")
	if err != nil || swapped {
		t.Errorf("second claim: swapped=%v, want false — the key was already taken", swapped)
	}

	swapped, _ = c.CompareAndSwap("leader", "node-a", "node-c")
	if !swapped {
		t.Error("CAS with the correct previous value failed")
	}
	v, _, _ := c.Get("leader")
	if v != "node-c" {
		t.Errorf("value after CAS = %#v, want node-c", v)
	}

	// Swapping to nil deletes.
	if swapped, _ := c.CompareAndSwap("leader", "node-c", nil); !swapped {
		t.Fatal("CAS to nil failed")
	}
	if _, ok, _ := c.Get("leader"); ok {
		t.Error("CAS to nil did not delete the key")
	}
}

func TestMemoryContextCASNumericTypesCompareByValue(t *testing.T) {
	// A JS function writes an int; a persisted store reads back a float64. If
	// CAS compared types, it could never succeed across a restart.
	c := NewMemoryContext()
	_ = c.Set("n", 5)
	swapped, err := c.CompareAndSwap("n", 5.0, 6.0)
	if err != nil {
		t.Fatalf("CAS: %v", err)
	}
	if !swapped {
		t.Error("CAS failed comparing int 5 against float64 5")
	}
}

func TestMemoryContextIncrement(t *testing.T) {
	c := NewMemoryContext()

	got, err := c.Increment("count", 1)
	if err != nil || got != 1 {
		t.Fatalf("Increment on absent key = (%v, %v), want (1, nil)", got, err)
	}
	got, _ = c.Increment("count", 4)
	if got != 5 {
		t.Errorf("Increment = %v, want 5", got)
	}
	got, _ = c.Increment("count", -2)
	if got != 3 {
		t.Errorf("Increment with a negative delta = %v, want 3", got)
	}

	// A non-numeric value is an error, not a silent reset — zeroing a counter
	// quietly hides whatever set it to a string.
	_ = c.Set("word", "not a number")
	if _, err := c.Increment("word", 1); err == nil {
		t.Error("Increment on a string value succeeded, want error")
	}
}

func TestMemoryContextUpdate(t *testing.T) {
	c := NewMemoryContext()

	got, err := c.Update("list", func(cur any) (any, error) {
		if cur == nil {
			return []any{"first"}, nil
		}
		return append(cur.([]any), "more"), nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !reflect.DeepEqual(got, []any{"first"}) {
		t.Errorf("Update returned %#v", got)
	}

	got, _ = c.Update("list", func(cur any) (any, error) {
		return append(cur.([]any), "second"), nil
	})
	if !reflect.DeepEqual(got, []any{"first", "second"}) {
		t.Errorf("second Update returned %#v", got)
	}

	// An error from fn leaves the stored value untouched.
	sentinel := errors.New("nope")
	if _, err := c.Update("list", func(any) (any, error) { return nil, sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("Update error = %v, want the sentinel", err)
	}
	v, _, _ := c.Get("list")
	if !reflect.DeepEqual(v, []any{"first", "second"}) {
		t.Errorf("value changed despite the update failing: %#v", v)
	}

	// Returning nil deletes.
	if _, err := c.Update("list", func(any) (any, error) { return nil, nil }); err != nil {
		t.Fatalf("Update to nil: %v", err)
	}
	if _, ok, _ := c.Get("list"); ok {
		t.Error("Update returning nil did not delete the key")
	}
}

func TestMemoryContextIncrementIsAtomicUnderConcurrency(t *testing.T) {
	// This is the primitive Node-RED's context API is missing. Without it, the
	// get-modify-set that flows write by hand loses updates under concurrency,
	// which is the documented reason a Node-RED flow cannot be run in more than
	// one instance safely.
	c := NewMemoryContext()

	const goroutines, perGoroutine = 50, 200
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if _, err := c.Increment("hits", 1); err != nil {
					t.Errorf("Increment: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got, _, _ := c.Get("hits")
	want := float64(goroutines * perGoroutine)
	if got != want {
		lost := want
		if f, ok := got.(float64); ok {
			lost = want - f
		}
		t.Errorf("counter = %v, want %v — %v updates were lost", got, want, lost)
	}
}

func TestMemoryContextUpdateIsAtomicUnderConcurrency(t *testing.T) {
	c := NewMemoryContext()

	const goroutines, perGoroutine = 20, 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				_, err := c.Update("n", func(cur any) (any, error) {
					if cur == nil {
						return 1.0, nil
					}
					return cur.(float64) + 1, nil
				})
				if err != nil {
					t.Errorf("Update: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got, _, _ := c.Get("n")
	if want := float64(goroutines * perGoroutine); got != want {
		t.Errorf("counter = %v, want %v", got, want)
	}
}

func TestScopedContextsIsolation(t *testing.T) {
	s := NewScopedContexts()

	_ = s.Global().Set("k", "global")
	_ = s.Flow("tab1").Set("k", "flow1")
	_ = s.Flow("tab2").Set("k", "flow2")
	_ = s.Node("n1").Set("k", "node1")

	if v, _, _ := s.Global().Get("k"); v != "global" {
		t.Errorf("global = %#v", v)
	}
	if v, _, _ := s.Flow("tab1").Get("k"); v != "flow1" {
		t.Errorf("flow tab1 = %#v", v)
	}
	if v, _, _ := s.Flow("tab2").Get("k"); v != "flow2" {
		t.Errorf("flow tab2 = %#v", v)
	}
	if v, _, _ := s.Node("n1").Get("k"); v != "node1" {
		t.Errorf("node n1 = %#v", v)
	}
	if _, ok, _ := s.Node("n2").Get("k"); ok {
		t.Error("a different node's context is not isolated")
	}

	// Repeated lookups return the same store, not a fresh one.
	if v, _, _ := s.Flow("tab1").Get("k"); v != "flow1" {
		t.Error("Flow() returned a new store on the second call")
	}
}

func TestScopedContextsConcurrentCreation(t *testing.T) {
	// Two nodes on the same tab starting at once must not each get their own
	// flow context.
	s := NewScopedContexts()
	var wg sync.WaitGroup
	wg.Add(64)
	for i := 0; i < 64; i++ {
		go func() {
			defer wg.Done()
			if _, err := s.Flow("shared").Increment("hits", 1); err != nil {
				t.Errorf("Increment: %v", err)
			}
		}()
	}
	wg.Wait()

	if v, _, _ := s.Flow("shared").Get("hits"); v != 64.0 {
		t.Errorf("shared flow counter = %v, want 64 — the scope was created more than once", v)
	}
}

func TestScopedContextsClean(t *testing.T) {
	// Redeploying repeatedly must not accumulate context for nodes that no
	// longer exist.
	s := NewScopedContexts()
	_ = s.Node("keep").Set("k", 1.0)
	_ = s.Node("drop").Set("k", 1.0)
	_ = s.Flow("keepFlow").Set("k", 1.0)
	_ = s.Flow("dropFlow").Set("k", 1.0)

	s.Clean(map[string]bool{"keep": true}, map[string]bool{"keepFlow": true})

	if _, ok, _ := s.Node("keep").Get("k"); !ok {
		t.Error("Clean removed a live node's context")
	}
	if _, ok, _ := s.Node("drop").Get("k"); ok {
		t.Error("Clean did not remove a dead node's context")
	}
	if _, ok, _ := s.Flow("keepFlow").Get("k"); !ok {
		t.Error("Clean removed a live flow's context")
	}
	if _, ok, _ := s.Flow("dropFlow").Get("k"); ok {
		t.Error("Clean did not remove a dead flow's context")
	}

	s.DropNode("keep")
	if _, ok, _ := s.Node("keep").Get("k"); ok {
		t.Error("DropNode did not remove the node's context")
	}
}

func BenchmarkMemoryContextIncrement(b *testing.B) {
	c := NewMemoryContext()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Increment("n", 1); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemoryContextGetSmall(b *testing.B) {
	c := NewMemoryContext()
	_ = c.Set("k", map[string]any{"a": 1.0, "b": "two"})
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := c.Get("k"); err != nil {
			b.Fatal(err)
		}
	}
}
