//go:build wasip1

// A reference Emberwire WASM guest.
//
// Built with:
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o guest.wasm ./testdata/wasmguest
//
// It doubles msg.payload, sets a status, and deliberately provides the two
// misbehaviours the host has to survive: a payload of "explode" traps, and a
// payload of "eat" allocates until it hits the memory ceiling. Both are how the
// sandbox guarantees get tested rather than asserted.
package main

import (
	"encoding/json"
	"unsafe"
)

// buffers keeps allocations alive.
//
// Go's garbage collector has no idea the host is holding a pointer into linear
// memory, so anything handed across the boundary has to be rooted here until
// the host frees it. Without this the collector is free to reuse the bytes the
// host is about to read, which produces corruption that looks like a host bug.
var buffers = map[uintptr][]byte{}

//go:wasmexport emberwire_alloc
func alloc(size uint32) uint32 {
	buf := make([]byte, size)
	ptr := uintptr(unsafe.Pointer(&buf[0]))
	buffers[ptr] = buf
	return uint32(ptr)
}

//go:wasmexport emberwire_free
func free(ptr uint32, _ uint32) {
	delete(buffers, uintptr(ptr))
}

type request struct {
	Msg    map[string]any `json:"msg"`
	Config map[string]any `json:"config,omitempty"`
}

type status struct {
	Fill  string `json:"fill,omitempty"`
	Shape string `json:"shape,omitempty"`
	Text  string `json:"text,omitempty"`
}

type response struct {
	Send   [][]map[string]any `json:"send,omitempty"`
	Status *status            `json:"status,omitempty"`
	Error  string             `json:"error,omitempty"`
	Logs   []string           `json:"logs,omitempty"`
}

//go:wasmexport emberwire_process
func process(ptr uint32, length uint32) uint64 {
	input := unsafe.Slice((*byte)(unsafe.Pointer(uintptr(ptr))), length)

	var req request
	if err := json.Unmarshal(input, &req); err != nil {
		return respond(response{Error: "could not decode the request: " + err.Error()})
	}

	switch req.Msg["payload"] {
	case "explode":
		// An out-of-bounds write. The host must report a trap and must not
		// reuse this instance afterwards.
		var empty []byte
		_ = empty[1]

	case "eat":
		// Allocate until the memory ceiling stops us. The host must survive
		// this; goja cannot make that promise.
		var hog [][]byte
		for i := 0; i < 1<<20; i++ {
			hog = append(hog, make([]byte, 1<<20))
		}
		_ = hog
	}

	out := map[string]any{}
	for k, v := range req.Msg {
		out[k] = v
	}
	if f, ok := req.Msg["payload"].(float64); ok {
		out["payload"] = f * 2
	}
	out["viaWasm"] = true

	return respond(response{
		Send:   [][]map[string]any{{out}},
		Status: &status{Fill: "green", Shape: "dot", Text: "doubled"},
		Logs:   []string{"processed one message"},
	})
}

// respond encodes a response into linear memory and packs its location into the
// i64 the host expects: offset in the high 32 bits, length in the low 32.
func respond(r response) uint64 {
	data, err := json.Marshal(r)
	if err != nil {
		return 0
	}
	buf := make([]byte, len(data))
	copy(buf, data)
	ptr := uintptr(unsafe.Pointer(&buf[0]))
	buffers[ptr] = buf
	return uint64(ptr)<<32 | uint64(len(buf))
}

func main() {}
