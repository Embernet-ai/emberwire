package wasmhost

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// guestPath is a reference guest built from testdata/wasmguest with:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared \
//	  -o testdata/wasmguest/guest.wasm ./testdata/wasmguest
const guestPath = "../../testdata/wasmguest/guest.wasm"

func loadGuest(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.FromSlash(guestPath))
	if err != nil {
		t.Skipf("no compiled guest at %s; build it with "+
			"GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o %s ./testdata/wasmguest",
			guestPath, guestPath)
	}
	return data
}

func compile(t *testing.T, limits Limits) *Module {
	t.Helper()
	m, err := Compile(context.Background(), "test", loadGuest(t), limits)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	return m
}

func TestWasmRoundTrip(t *testing.T) {
	m := compile(t, Limits{})

	resp, err := m.Call(context.Background(), Request{
		Msg: map[string]any{"payload": 21.0, "topic": "press/01"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(resp.Send) != 1 || len(resp.Send[0]) != 1 {
		t.Fatalf("send = %#v, want one message on one port", resp.Send)
	}
	out := resp.Send[0][0]
	if out["payload"] != 42.0 {
		t.Errorf("payload = %#v, want 42", out["payload"])
	}
	// Everything else on the message has to survive the crossing, or a guest
	// silently strips the topic that says which machine the data came from.
	if out["topic"] != "press/01" {
		t.Errorf("topic = %#v, want it preserved", out["topic"])
	}
	if out["viaWasm"] != true {
		t.Error("the guest's own additions did not come back")
	}
	if resp.Status == nil || resp.Status.Text != "doubled" {
		t.Errorf("status = %#v", resp.Status)
	}
	if len(resp.Logs) != 1 {
		t.Errorf("logs = %#v", resp.Logs)
	}
}

// TestWasmMemoryCeilingIsEnforced is the reason this package exists.
//
// goja has no way to bound memory: a JavaScript function allocating in a loop
// grows the Go heap until the pod is OOM-killed, and a wall-clock timeout is the
// only defence. A WebAssembly guest has a linear memory with a declared maximum,
// so allocating past it fails inside the guest and the host is untouched.
func TestWasmMemoryCeilingIsEnforced(t *testing.T) {
	m := compile(t, Limits{MaxMemoryBytes: 32 << 20, Timeout: 30 * time.Second})

	_, err := m.Call(context.Background(), Request{Msg: map[string]any{"payload": "eat"}})
	if err == nil {
		t.Fatal("a guest allocating without limit returned successfully")
	}
	if !errors.Is(err, ErrOutOfMemory) && !errors.Is(err, ErrGuestTrapped) {
		t.Errorf("error = %v, want it classified as out of memory or a trap", err)
	}

	// And the host is still fine: the next message works.
	resp, err := m.Call(context.Background(), Request{Msg: map[string]any{"payload": 5.0}})
	if err != nil {
		t.Fatalf("the host did not survive a guest exhausting its memory: %v", err)
	}
	if resp.Send[0][0]["payload"] != 10.0 {
		t.Errorf("payload after recovery = %#v", resp.Send[0][0]["payload"])
	}
}

func TestWasmTrapIsContained(t *testing.T) {
	m := compile(t, Limits{})

	_, err := m.Call(context.Background(), Request{Msg: map[string]any{"payload": "explode"}})
	if err == nil {
		t.Fatal("a trapping guest returned successfully")
	}

	// The host keeps running, and the trapped instance is not reused — a trap
	// can leave the guest's allocator inconsistent, and handing that to the next
	// message produces failures that look random.
	for i := 0; i < 3; i++ {
		resp, err := m.Call(context.Background(), Request{Msg: map[string]any{"payload": 3.0}})
		if err != nil {
			t.Fatalf("call %d after a trap failed: %v", i, err)
		}
		if resp.Send[0][0]["payload"] != 6.0 {
			t.Errorf("call %d payload = %#v, want 6", i, resp.Send[0][0]["payload"])
		}
	}
}

func TestWasmTimeout(t *testing.T) {
	// A guest that will not finish must not hold a runtime goroutine.
	m := compile(t, Limits{MaxMemoryBytes: 512 << 20, Timeout: 300 * time.Millisecond})

	done := make(chan error, 1)
	go func() {
		_, err := m.Call(context.Background(), Request{Msg: map[string]any{"payload": "eat"}})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a long-running guest returned successfully")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the guest was not interrupted; it would hold a goroutine forever")
	}
}

func TestWasmRejectsGuestWithoutEntryPoints(t *testing.T) {
	// A packaging mistake must fail at deploy, not on the first message. The
	// difference is a red node versus a flow that quietly drops everything.
	//
	// The smallest valid module: the magic bytes, a version, and nothing else.
	empty := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	_, err := Compile(context.Background(), "empty", empty, Limits{})
	if err == nil {
		t.Fatal("a guest with no exports was accepted")
	}
	if !contains(err.Error(), ExportProcess) {
		t.Errorf("error = %q, want it to name the missing export", err)
	}
}

func TestWasmRejectsGarbage(t *testing.T) {
	if _, err := Compile(context.Background(), "junk", []byte("this is not wasm"), Limits{}); err == nil {
		t.Fatal("invalid bytes were accepted as a module")
	}
}

func TestWasmConcurrentCalls(t *testing.T) {
	// A wazero instance is not safe for concurrent use, so the pool has to hand
	// each caller its own. The scheduler runs one message per node at a time,
	// but a config node shared by several nodes does not get that guarantee.
	m := compile(t, Limits{})

	const goroutines, each = 8, 25
	var wg sync.WaitGroup
	errs := make(chan error, goroutines*each)

	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(base float64) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				resp, err := m.Call(context.Background(), Request{
					Msg: map[string]any{"payload": base},
				})
				if err != nil {
					errs <- err
					return
				}
				if got := resp.Send[0][0]["payload"]; got != base*2 {
					errs <- errors.New("wrong result under concurrency")
					return
				}
			}
		}(float64(g + 1))
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent call failed: %v", err)
	}
}

func TestWasmOutputSizeIsBounded(t *testing.T) {
	// A guest reporting a huge length must not make the host allocate for it.
	m := compile(t, Limits{MaxOutputBytes: 8})

	_, err := m.Call(context.Background(), Request{Msg: map[string]any{"payload": 1.0}})
	if err == nil {
		t.Fatal("an oversized response was accepted")
	}
	if !contains(err.Error(), "limit") {
		t.Errorf("error = %q, want it to mention the limit", err)
	}
}

func TestPackResultRoundTrip(t *testing.T) {
	// The one layout the host and every guest must agree on.
	cases := []struct{ ptr, length uint32 }{
		{0, 0}, {1, 1}, {65536, 1024}, {0xFFFFFFFF, 0xFFFFFFFF}, {0x12345678, 0x9ABCDEF0},
	}
	for _, c := range cases {
		ptr, length := UnpackResult(PackResult(c.ptr, c.length))
		if ptr != c.ptr || length != c.length {
			t.Errorf("PackResult(%d,%d) round-tripped to (%d,%d)", c.ptr, c.length, ptr, length)
		}
	}
}

func TestLimitsDefaults(t *testing.T) {
	l := Limits{}.withDefaults()
	if l.MaxMemoryBytes != DefaultMaxMemoryBytes {
		t.Errorf("MaxMemoryBytes = %d", l.MaxMemoryBytes)
	}
	if l.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v", l.Timeout)
	}
	if l.MaxOutputBytes != DefaultMaxOutputBytes {
		t.Errorf("MaxOutputBytes = %d", l.MaxOutputBytes)
	}
}

func BenchmarkWasmCall(b *testing.B) {
	data, err := os.ReadFile(filepath.FromSlash(guestPath))
	if err != nil {
		b.Skip("no compiled guest")
	}
	m, err := Compile(context.Background(), "bench", data, Limits{})
	if err != nil {
		b.Fatal(err)
	}
	defer m.Close(context.Background())

	ctx := context.Background()
	req := Request{Msg: map[string]any{"payload": 21.0}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := m.Call(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}
