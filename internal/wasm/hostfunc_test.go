package wasm

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"testing"
	"time"
)

// Extra opcodes the host-function guests need (see wasmgen_test.go).
const (
	opI32Load   = 0x28 // i32.load <align> <offset>
	opI32Store  = 0x36 // i32.store <align> <offset>
	opI32Load8U = 0x2d // i32.load8_u <align> <offset>
	opI64Const  = 0x42 // i64.const <sleb>
	opI32Eq     = 0x46 // i32.eq
	opI32Ne     = 0x47 // i32.ne
	opI32LtU    = 0x49 // i32.lt_u
	opI32Sub    = 0x6b // i32.sub
	opI32And    = 0x71 // i32.and
	opI64And    = 0x83 // i64.and
	opI64ShrU   = 0x8a // i64.shr_u
	opI32Wrap64 = 0xa7 // i32.wrap_i64
	opBrIf      = 0x0d // br_if <label>
	opBlock     = 0x02 // block <empty>
) // nowNSGuest builds a guest importing only now_ns, whose run() stores the
// low 32 bits at address 0 and returns 0.
//
//	(module
//	  (import "pulse/env" "now_ns" (func (result i64)))
//	  (memory (export "memory") 1)
//	  (func (export "allocate") (param i32) (result i32) i32.const 512)
//	  (func (export "sign_request") (param i32 i32) (result i32) i32.const 0)
//	  (func (export "run") (result i32)
//	    i32.const 0; call 0; i64.const 0xFFFFFFFF; i64.and; i32.wrap_i64; i32.store
//	    i32.const 0))
func nowNSGuest() []byte {
	w := newWasm()
	w.types(
		funcType(nil, []byte{0x7e}),                         // 0: now_ns () -> i64
		funcType([]byte{typeI32}, []byte{typeI32}),          // 1: allocate
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}), // 2: sign_request
		funcType(nil, []byte{typeI32}),                      // 3: run
	)
	w.importSection(importEntry(HostModuleName, HostFuncNowNS, 0))
	w.funcs(1, 2, 3)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 1),
		export(ABISignRequest, 0x00, 2),
		export("run", 0x00, 3),
	)
	alloc := []byte{opI32Const, 0x00, opEnd} // never used; ABI requires the export
	sign := []byte{opI32Const, 0x00, opEnd}
	run := []byte{
		opI32Const, 0x00, // store address
		opCall, 0x00, // now_ns -> i64
		opI64Const, 0xFF, 0xFF, 0xFF, 0xFF, 0x0F, // mask low 32 bits
		opI64And,
		opI32Wrap64,
		opI32Store, 0x00, 0x00,
		opI32Const, 0x00,
		opEnd,
	}
	w.code(alloc, sign, run)
	return w.bytes()
}

// TestNowNSReturnsWallClock proves the guest-observable clock moves forward
// with the wall clock: the stored value falls inside the test's own window.
func TestNowNSReturnsWallClock(t *testing.T) {
	m := newModule(t, Config{Wasm: nowNSGuest(), Resolver: stubResolver{secret: "x"}, Name: "now-ns"})
	before := time.Now().UnixNano()
	res, err := m.mod.ExportedFunction("run").Call(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res) != 1 || res[0] != 0 {
		t.Fatalf("run returned %v, want [0]", res)
	}
	after := time.Now().UnixNano()
	view, ok := m.Memory().Read(0, 4)
	if !ok {
		t.Fatal("read stored timestamp")
	}
	lo := uint32(view[0]) | uint32(view[1])<<8 | uint32(view[2])<<16 | uint32(view[3])<<24
	// now_ns returns full nanoseconds; the guest stored the low 32 bits, so
	// compare in the same truncation (wrap makes before/after straddle safe:
	// the window is microseconds wide, the space 4 billion).
	if uint64(lo) < uint64(uint32(before)) || uint64(lo) > uint64(uint32(after))+2 {
		t.Fatalf("stored low bits %d outside [%d, %d]", lo, uint32(before), uint32(after))
	}
	t.Logf("now_ns low bits %d within the call window", lo)
}

// headerGuest builds a guest importing only header_get, with two runs:
// run_ok reads name at [0, nameLen) into [64, 128) and returns the count;
// run_oob calls header_get(0xFFFFFF00, 8, 64, 64) and returns its result
// (expecting -1).
func headerGuest(nameLen byte) []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32, typeI32, typeI32, typeI32}, []byte{typeI32}), // 0: header_get
		funcType([]byte{typeI32}, []byte{typeI32}),                            // 1: allocate
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}),                   // 2: sign_request
		funcType(nil, []byte{typeI32}),                                        // 3: run
	)
	w.importSection(importEntry(HostModuleName, HostFuncHeaderGet, 0))
	w.funcs(1, 2, 3, 3)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 1),
		export(ABISignRequest, 0x00, 2),
		export("run_ok", 0x00, 3),
		export("run_oob", 0x00, 4),
	)
	alloc := []byte{opI32Const, 0x00, opEnd}
	sign := []byte{opI32Const, 0x00, opEnd}
	// sleb-encode properly with the shared encoder.
	ok := []byte{opI32Const}
	ok = append(ok, sleb(0)...)
	ok = append(ok, opI32Const)
	ok = append(ok, sleb(int32(nameLen))...)
	ok = append(ok, opI32Const)
	ok = append(ok, sleb(64)...)
	ok = append(ok, opI32Const)
	ok = append(ok, sleb(64)...)
	ok = append(ok, opCall, 0x00, opEnd)
	oob := []byte{opI32Const}
	oob = append(oob, sleb(int32(-256))...) // 0xFFFFFF00 as i32
	oob = append(oob, opI32Const)
	oob = append(oob, sleb(8)...)
	oob = append(oob, opI32Const)
	oob = append(oob, sleb(64)...)
	oob = append(oob, opI32Const)
	oob = append(oob, sleb(64)...)
	oob = append(oob, opCall, 0x00, opEnd)
	w.code(alloc, sign, ok, oob)
	return w.bytes()
}

// TestHeaderGetServesPreFlightHeaders proves the read-only seam: the guest
// reads a header the host attached, learns nothing about absent names, and an
// out-of-bounds call returns -1 without trapping the host.
func TestHeaderGetServesPreFlightHeaders(t *testing.T) {
	const name = "user-agent"
	m := newModule(t, Config{Wasm: headerGuest(byte(len(name))), Resolver: stubResolver{secret: "x"}, Name: "header-get"})
	if !m.Memory().Write(0, []byte(name)) {
		t.Fatal("write header name")
	}
	req, _ := http.NewRequest("GET", "https://api.example.com/v1/charge", nil)
	req.Header.Set("User-Agent", "pulse-test/1.0")
	ctx := WithRequest(context.Background(), req)

	res, err := m.mod.ExportedFunction("run_ok").Call(ctx)
	if err != nil {
		t.Fatalf("run_ok: %v", err)
	}
	if len(res) != 1 || res[0] != uint64(len("pulse-test/1.0")) {
		t.Fatalf("run_ok returned %v, want [%d]", res, len("pulse-test/1.0"))
	}
	view, ok := m.Memory().Read(64, uint32(len("pulse-test/1.0")))
	if !ok {
		t.Fatal("read header value")
	}
	if string(view) != "pulse-test/1.0" {
		t.Fatalf("header value = %q, want pulse-test/1.0", view)
	}

	// Absent header: -1, indistinguishable from empty by design.
	if !m.Memory().Write(0, []byte("x-missing!")) {
		t.Fatal("write missing name")
	}
	res, err = m.mod.ExportedFunction("run_ok").Call(ctx)
	if err != nil {
		t.Fatalf("run_ok missing: %v", err)
	}
	if len(res) != 1 || int32(res[0]) != -1 {
		t.Fatalf("missing header returned %v, want -1", res)
	}

	// Out of bounds: -1, no trap (M86's shape).
	res, err = m.mod.ExportedFunction("run_oob").Call(ctx)
	if err != nil {
		t.Fatalf("run_oob trapped the host: %v", err)
	}
	if len(res) != 1 || int32(res[0]) != -1 {
		t.Fatalf("oob header_get returned %v, want -1", res)
	}

	// No request attached: fail closed, not an empty map.
	res, err = m.mod.ExportedFunction("run_ok").Call(context.Background())
	if err != nil {
		t.Fatalf("run_ok without request trapped: %v", err)
	}
	if len(res) != 1 || int32(res[0]) != -1 {
		t.Fatalf("header_get without a request returned %v, want -1", res)
	}
}

// TestHeaderGetRedactsSecrets proves a guest can attest to a sensitive
// header's presence without learning its value.
func TestHeaderGetRedactsSecrets(t *testing.T) {
	const name = "authorization"
	m := newModule(t, Config{Wasm: headerGuest(byte(len(name))), Resolver: stubResolver{secret: "x"}, Name: "header-redact"})
	if !m.Memory().Write(0, []byte(name)) {
		t.Fatal("write header name")
	}
	req, _ := http.NewRequest("GET", "https://api.example.com/", nil)
	req.Header.Set("Authorization", "Bearer live-secret-value")
	ctx := WithRequest(context.Background(), req)

	res, err := m.mod.ExportedFunction("run_ok").Call(ctx)
	if err != nil {
		t.Fatalf("run_ok: %v", err)
	}
	if len(res) != 1 || res[0] != uint64(len(redactedValue)) {
		t.Fatalf("run_ok returned %v, want [%d]", res, len(redactedValue))
	}
	view, ok := m.Memory().Read(64, uint32(len(redactedValue)))
	if !ok {
		t.Fatal("read redacted value")
	}
	if string(view) != redactedValue {
		t.Fatalf("sensitive header read as %q, want %q", view, redactedValue)
	}
	full := make([]byte, m.MemorySize())
	if v, ok := m.Memory().Read(0, uint32(len(full))); ok {
		copy(full, v)
	}
	if bytes.Contains(full, []byte("live-secret-value")) {
		t.Fatal("ZERO-TRUST VIOLATION: bearer secret reachable from guest memory")
	}
}

// logGuest builds a guest importing only log: run_ok logs [0,5) ("hello"),
// run_oob logs [0xFFFFFF00, 5).
func logGuest() []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32, typeI32}, nil),             // 0: log
		funcType([]byte{typeI32}, []byte{typeI32}),          // 1: allocate
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}), // 2: sign_request
		funcType(nil, []byte{typeI32}),                      // 3: run
	)
	w.importSection(importEntry(HostModuleName, HostFuncLog, 0))
	w.funcs(1, 2, 3, 3)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 1),
		export(ABISignRequest, 0x00, 2),
		export("run_ok", 0x00, 3),
		export("run_oob", 0x00, 4),
	)
	alloc := []byte{opI32Const, 0x00, opEnd}
	sign := []byte{opI32Const, 0x00, opEnd}
	mkcall := func(ptr int32, n int32) []byte {
		b := []byte{opI32Const}
		b = append(b, sleb(ptr)...)
		b = append(b, opI32Const)
		b = append(b, sleb(n)...)
		b = append(b, opCall, 0x00, opI32Const, 0x00, opEnd)
		return b
	}
	w.code(alloc, sign, mkcall(0, 5), mkcall(int32(-256), 5))
	return w.bytes()
}

// bufSink captures log output for the gates.
type bufSink struct{ buf bytes.Buffer }

func (b *bufSink) Write(p []byte) (int, error) { return b.buf.Write(p) }

// TestLogGoesToTheSinkNotStdout proves the diagnostic sink contract: bytes
// land in logSink, and a stdout swap proves nothing touches the JSON-RPC
// channel (AGENTS.md §3.3).
func TestLogGoesToTheSinkNotStdout(t *testing.T) {
	m := newModule(t, Config{Wasm: logGuest(), Resolver: stubResolver{secret: "x"}, Name: "log-sink"})
	if !m.Memory().Write(0, []byte("hello")) {
		t.Fatal("write log message")
	}
	// Swap stdout for a pipe: anything the guest writes there is caught.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	sink := &bufSink{}
	prev := logSink
	logSink = sink
	defer func() { logSink = prev }()

	if _, err := m.mod.ExportedFunction("run_ok").Call(context.Background()); err != nil {
		t.Fatalf("run_ok: %v", err)
	}
	_ = w.Close()
	os.Stdout = orig
	leaked := make([]byte, 64)
	n, _ := r.Read(leaked)
	_ = r.Close()
	if n > 0 {
		t.Fatalf("guest log wrote %d bytes to stdout: %q", n, leaked[:n])
	}
	if got := sink.buf.String(); got != "pulse: wasm: hello\n" {
		t.Fatalf("sink got %q, want %q", got, "pulse: wasm: hello\n")
	}

	// Out of bounds: dropped, no trap, nothing logged (M86's shape).
	sink.buf.Reset()
	if _, err := m.mod.ExportedFunction("run_oob").Call(context.Background()); err != nil {
		t.Fatalf("run_oob trapped the host: %v", err)
	}
	if sink.buf.Len() != 0 {
		t.Fatalf("oob log wrote %q to the sink", sink.buf.String())
	}
}

// TestLogTruncatesAtTheBound proves the sink is not a memory pipe.
func TestLogTruncatesAtTheBound(t *testing.T) {
	m := newModule(t, Config{Wasm: logGuest(), Resolver: stubResolver{secret: "x"}, Name: "log-trunc"})
	big := bytes.Repeat([]byte("A"), maxLogBytes+1000)
	if !m.Memory().Write(0, big) {
		t.Fatal("write long message")
	}
	sink := &bufSink{}
	prev := logSink
	logSink = sink
	defer func() { logSink = prev }()

	// Call the raw host func with the full length (the guest's run_ok only
	// logs 5 bytes).
	newLogFunc()(context.Background(), m.mod, []uint64{0, uint64(len(big))})
	if sink.buf.Len() != len("pulse: wasm: \n")+maxLogBytes {
		t.Fatalf("truncated log is %d bytes, want %d", sink.buf.Len(), len("pulse: wasm: \n")+maxLogBytes)
	}
}

// TestHostBoundsChecksAreSubtractionForm is M86's direct gate: the wrap case
// (ptr near 2^32) must fail even though ptr+size wraps to something small.
// An additive check (`ptr+size <= avail`) passes it and fails this test.
func TestHostBoundsChecksAreSubtractionForm(t *testing.T) {
	m := newModule(t, Config{Wasm: nowNSGuest(), Resolver: stubResolver{secret: "x"}, Name: "bounds"})
	mem := m.Memory()
	avail := mem.Size()
	if avail == 0 {
		t.Fatal("zero-byte memory")
	}
	cases := []struct {
		name string
		ptr  uint32
		size uint32
		want bool
	}{
		{"in bounds", 0, 16, true},
		{"empty at end", avail, 0, true},
		{"past the end", avail + 1, 0, false},
		{"overrun", avail - 4, 8, false},
		{"wrap case", 0xFFFFFF00, 0x200, false},
		{"max ptr", 0xFFFFFFFF, 1, false},
		{"max size at zero", 0, 0xFFFFFFFF, false},
	}
	for _, tc := range cases {
		if got := inBounds(mem, tc.ptr, tc.size); got != tc.want {
			t.Errorf("inBounds(%d, %d) = %v, want %v", tc.ptr, tc.size, got, tc.want)
		}
	}
}
