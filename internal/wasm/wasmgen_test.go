package wasm

// A minimal WebAssembly binary ASSEMBLER, used only by the tests.
//
// # Why assemble by hand instead of shipping a .wasm fixture or a WAT toolchain
//
// The gates need three guest modules that no real compiler emits: an infinite
// loop, a guest that lies about its output length, and a guest that returns a
// pointer past the end of its memory. Compiling those needs TinyGo or a
// `wat2wasm` binary, which makes the test suite depend on a toolchain it does
// not otherwise have and cannot run offline.
//
// The WebAssembly binary format is small enough that emitting the roughly
// forty bytes each of these needs is cheaper than the dependency, and the
// result is a hermetic test: `go test` is the only tool required.
//
// Each builder below is annotated with the equivalent WAT, so the bytes can be
// checked against the specification by eye rather than by running a compiler.

// WebAssembly opcodes used by these modules.
const (
	opI32Const   = 0x41 // i32.const <s32>
	opGlobalGet  = 0x23 // global.get <idx>
	opGlobalSet  = 0x24 // global.set <idx>
	opLocalGet   = 0x20 // local.get <idx>
	opI32Add     = 0x6a // i32.add
	opI32Store8  = 0x3a // i32.store8 <align> <offset>
	opLoop       = 0x03 // loop <blocktype>
	opBr         = 0x0c // br <label>
	opBlockEmpty = 0x40
	opCall       = 0x10 // call <funcidx>
	opMemoryGrow = 0x40 // memory.grow <memidx>
	opDrop       = 0x1a // drop
	opEnd        = 0x0b
)

// Value types.
const typeI32 = 0x7f

// wasmBuilder accumulates a WebAssembly module.
type wasmBuilder struct{ b []byte }

// newWasm starts a module: the 8-byte magic and version preamble.
func newWasm() *wasmBuilder {
	return &wasmBuilder{b: []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}}
}

// uleb encodes an unsigned LEB128 integer, which is how WebAssembly encodes
// every count, index and size in the binary format.
func uleb(n uint32) []byte {
	var out []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		if n != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if n == 0 {
			return out
		}
	}
}

// sleb encodes a signed LEB128 integer, used by i32.const.
func sleb(n int32) []byte {
	var out []byte
	more := true
	for more {
		b := byte(n & 0x7f)
		n >>= 7 // arithmetic shift, so the sign carries
		if (n == 0 && b&0x40 == 0) || (n == -1 && b&0x40 != 0) {
			more = false
		} else {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

// section appends one section: id, LEB length, payload.
func (w *wasmBuilder) section(id byte, content []byte) {
	w.b = append(w.b, id)
	w.b = append(w.b, uleb(uint32(len(content)))...)
	w.b = append(w.b, content...)
}

// funcType builds a function type: 0x60, param types, result types.
func funcType(params, results []byte) []byte {
	out := []byte{0x60}
	out = append(out, uleb(uint32(len(params)))...)
	out = append(out, params...)
	out = append(out, uleb(uint32(len(results)))...)
	out = append(out, results...)
	return out
}

func (w *wasmBuilder) types(defs ...[]byte) {
	c := uleb(uint32(len(defs)))
	for _, d := range defs {
		c = append(c, d...)
	}
	w.section(1, c)
}

func (w *wasmBuilder) funcs(typeIdx ...uint32) {
	c := uleb(uint32(len(typeIdx)))
	for _, i := range typeIdx {
		c = append(c, uleb(i)...)
	}
	w.section(3, c)
}

// memory emits one memory with the given minimum page count.
func (w *wasmBuilder) memory(minPages uint32) {
	c := append(uleb(1), 0x00) // count=1, limits flag 0 = min only
	c = append(c, uleb(minPages)...)
	w.section(5, c)
}

// global emits one mutable i32 global with a constant initialiser.
func (w *wasmBuilder) global(init int32) {
	body := append([]byte{typeI32, 0x01}, opI32Const) // i32, mutable
	body = append(body, sleb(init)...)
	body = append(body, opEnd)
	w.section(6, append(uleb(1), body...))
}

// importEntry builds one import entry: module, field, kind 0x00, type index.
//
// The lengths are DERIVED from the strings rather than written by hand. The
// first draft hardcoded them, got "read_file" wrong by one byte, and produced
// import sections that wazero rejected with "invalid byte for importdesc" --
// which reads exactly like a sandbox refusing an escape attempt. It took
// mutation M54 ("WASI linked") coming back GREEN to find it: the guest was
// refused for a malformed section with or without WASI present, so the mutation
// proved nothing at all.
func importEntry(module, field string, typeIdx uint32) []byte {
	out := append(uleb(uint32(len(module))), module...)
	out = append(out, uleb(uint32(len(field)))...)
	out = append(out, field...)
	out = append(out, 0x00) // kind: function
	return append(out, uleb(typeIdx)...)
}

// importSection emits the import section (id 2). It must be called AFTER
// types(), because the WebAssembly binary format fixes the section order:
// type (1), import (2), function (3), table (4), memory (5), global (6),
// export (7), code (10). The first draft emitted imports first and wazero
// rejected them with "invalid section order" -- which looks precisely like a
// sandbox refusing an escape attempt, and cost two mutations their meaning.
func (w *wasmBuilder) importSection(entries ...[]byte) {
	c := uleb(uint32(len(entries)))
	for _, e := range entries {
		c = append(c, e...)
	}
	w.section(2, c)
}

// export builds one export entry: name, kind, index.
// kind 0x00 is a function, 0x02 is a memory.
func export(name string, kind byte, idx uint32) []byte {
	out := append(uleb(uint32(len(name))), name...)
	out = append(out, kind)
	return append(out, uleb(idx)...)
}

func (w *wasmBuilder) exports(entries ...[]byte) {
	c := uleb(uint32(len(entries)))
	for _, e := range entries {
		c = append(c, e...)
	}
	w.section(7, c)
}

// code emits one function body per entry. Each body declares zero locals, which
// is all these modules need: they operate on their parameters and on globals.
func (w *wasmBuilder) code(bodies ...[]byte) {
	c := uleb(uint32(len(bodies)))
	for _, b := range bodies {
		inner := append(uleb(0), b...) // 0 local declarations
		c = append(c, uleb(uint32(len(inner)))...)
		c = append(c, inner...)
	}
	w.section(10, c)
}

func (w *wasmBuilder) bytes() []byte { return w.b }

// ---- concrete guest modules ----------------------------------------------

// headerBytes is the `X-Wasm-Signed: true` line the mock signer appends.
//
// It is the observable effect the Execution Gate asserts, so it is spelled out
// here rather than hidden inside the encoder: a reader should be able to predict
// the gate's output from this constant alone.
var headerBytes = []byte("X-Wasm-Signed: true\r\n")

// benignSigner builds the module the Execution Gate drives.
//
//	(module
//	  (memory (export "memory") 1)
//	  (global $bump (mut i32) (i32.const 1024))
//	  (func (export "allocate") (param $size i32) (result i32)
//	    global.get $bump            ;; the address we return
//	    global.get $bump            ;; ...
//	    local.get  $size            ;; ... plus the size
//	    i32.add                     ;; ... is the new bump
//	    global.set $bump)
//	  (func (export "sign_request") (param $ptr i32) (param $size i32) (result i32)
//	    ;; unrolled: memory[$ptr + i] = headerBytes[i]
//	    local.get $ptr
//	    i32.const 'X'
//	    i32.store8
//	    ... one pair per byte ...
//	    i32.const 21))              ;; the output length
func benignSigner() []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32}, []byte{typeI32}),          // allocate
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}), // sign_request
	)
	w.funcs(0, 1)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 0),
		export(ABISignRequest, 0x00, 1),
	)

	// allocate: return the current bump, then advance it by size.
	alloc := []byte{
		opGlobalGet, 0x00, // push bump  (this is the return value)
		opGlobalGet, 0x00, // push bump
		opLocalGet, 0x00, // push size
		opI32Add,          // bump + size
		opGlobalSet, 0x00, // store it back
		opEnd,
	}

	// The header is APPENDED at ptr+size, so the envelope the host wrote is
	// preserved and the signed form is the request plus the signature. The
	// address must also ADVANCE per byte: the first draft pushed the pointer and
	// stored twenty-one times, so every store hit the same offset and the
	// "header" collapsed to its final byte.
	sign := make([]byte, 0, len(headerBytes)*11+16)
	for i, b := range headerBytes {
		sign = append(sign, opLocalGet, 0x00) // ptr
		sign = append(sign, opLocalGet, 0x01) // size
		sign = append(sign, opI32Add)         // ptr + size
		sign = append(sign, opI32Const)
		sign = append(sign, sleb(int32(i))...)
		sign = append(sign, opI32Add) // ptr + size + i
		sign = append(sign, opI32Const)
		sign = append(sign, sleb(int32(b))...)
		sign = append(sign, opI32Store8, 0x00, 0x00)
	}
	// return size + len(header)
	sign = append(sign, opLocalGet, 0x01)
	sign = append(sign, opI32Const)
	sign = append(sign, sleb(int32(len(headerBytes)))...)
	sign = append(sign, opI32Add)
	sign = append(sign, opEnd)

	w.code(alloc, sign)
	return w.bytes()
}

// infiniteLoop builds the module the Metering Gate drives.
//
//	(module
//	  (memory (export "memory") 1)
//	  (func (export "sign_request") (param i32 i32) (result i32)
//	    i32.const 0     ;; satisfy the result type
//	    loop            ;;   forever
//	      br 0
//	    end))
//
// There is no `allocate`, so the module would fail the ABI check if it were
// reached — the gate never gets that far, because the budget fires first. The
// export name is kept identical so the failure under test is the LOOP and not a
// missing symbol.
func infiniteLoop() []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32}, []byte{typeI32}),          // allocate
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}), // sign_request
	)
	w.funcs(0, 1)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 0),
		export(ABISignRequest, 0x00, 1),
	)
	alloc := []byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	}
	body := []byte{
		opI32Const, 0x00,
		opLoop, opBlockEmpty,
		opBr, 0x00,
		opEnd, // end loop
		opEnd, // end func
	}
	w.code(alloc, body)
	return w.bytes()
}

// infiniteRecursion builds a guest that recurses until the fuel budget stops it.
//
//	(func (export "sign_request") (param i32 i32) (result i32)
//	  call 0)   ;; tail-position call to itself
//
// This is the case the FUEL tier exists for and the timeout is a poor fit for: a
// deep recursion is terminated by the call budget in deterministic, wall-clock
// independent time.
func infiniteRecursion() []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32}, []byte{typeI32}),          // allocate
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}), // sign_request
		funcType(nil, []byte{typeI32}),                      // recurse: () -> i32
	)
	w.funcs(0, 1, 2)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 0),
		export(ABISignRequest, 0x00, 1),
	)
	alloc := []byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	}
	// sign_request enters the recursion exactly once, discards its i32 result,
	// and returns 0. Leaving the result on the stack would be a type error:
	// the function declares one i32 result and the call would leave two.
	sign := []byte{opCall, 0x02, opDrop, opI32Const, 0x00, opEnd}
	// recurse calls itself in tail position, forever.
	recurse := []byte{opCall, 0x02, opEnd}
	w.code(alloc, sign, recurse)
	return w.bytes()
}

// callStorm builds a guest that makes `n` function calls at BOUNDED DEPTH.
//
//	(func (export "sign_request") (param i32 i32) (result i32)
//	  call 1
//	  i32.const 0)
//	(func $driver (result i32)
//	  call 2   ;; leaf
//	  call 2   ;; ... n times, unrolled
//	  ...)
//	(func $leaf (result i32)
//	  i32.const 0)
//
// It exists because a RECURSION bomb turned out to be the wrong vehicle, and
// finding that out was worth recording. wazero's interpreter enforces its own
// call-stack bound, and a self-recursive guest hits it long before any
// multi-thousand-call budget: `TestDeepRecursionTrap` measures that.
//
// Fuel bounds TOTAL WORK, not DEPTH. This module reaches n calls while the stack
// stays two frames deep, which is the case the budget is actually for.
func callStorm(n int) []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32}, []byte{typeI32}),          // allocate
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}), // sign_request
		funcType(nil, []byte{typeI32}),                      // driver
		funcType(nil, []byte{typeI32}),                      // leaf
	)
	w.funcs(0, 1, 2, 3)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 0),
		export(ABISignRequest, 0x00, 1),
	)
	alloc := []byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	}
	// The driver's i32 result must be dropped before the return value is pushed:
	// sign_request declares ONE result, and leaving the call's result on the
	// stack makes it two.
	sign := []byte{opCall, 0x02, opDrop, opI32Const, 0x00, opEnd}

	driver := make([]byte, 0, n*2+8)
	for i := 0; i < n; i++ {
		driver = append(driver, opCall, 0x03) // leaf, which pushes an i32
		driver = append(driver, opDrop)       // discard it: the stack must not grow
	}
	driver = append(driver, opI32Const, 0x00, opEnd)

	leaf := []byte{opI32Const, 0x00, opEnd}
	w.code(alloc, sign, driver, leaf)
	return w.bytes()
}

// lyingSigner builds a guest whose sign_request reports an output LONGER than the
// buffer it was given.
//
//	(func (export "sign_request") (param $ptr i32) (param $size i32) (result i32)
//	  i32.const 1048576)   ;; 1 MiB, regardless of $size
//
// It is the ROADMAP.md §3.4.2 "malicious guest returns a huge length" case. The
// host must refuse it without reading a byte, because 1 MiB is not this
// instance's memory.
func lyingSigner() []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32}, []byte{typeI32}),
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}),
	)
	w.funcs(0, 1)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 0),
		export(ABISignRequest, 0x00, 1),
	)
	alloc := []byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	}
	sign := []byte{opI32Const}
	sign = append(sign, sleb(1<<20)...)
	sign = append(sign, opEnd)
	w.code(alloc, sign)
	return w.bytes()
}

// negativeSigner builds a guest whose sign_request returns an error code.
//
//	(func (export "sign_request") (param i32 i32) (result i32)
//	  i32.const -1)
func negativeSigner() []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32}, []byte{typeI32}),
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}),
	)
	w.funcs(0, 1)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 0),
		export(ABISignRequest, 0x00, 1),
	)
	alloc := []byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	}
	sign := []byte{opI32Const}
	sign = append(sign, sleb(-1)...)
	sign = append(sign, opEnd)
	w.code(alloc, sign)
	return w.bytes()
}

// outOfBoundsAllocator builds a guest whose allocate returns a pointer past the
// end of its memory.
//
//	(func (export "allocate") (param i32) (result i32)
//	  i32.const 4294967295)   ;; 2^32 - 1
//
// This is the integer-overflow case in checkRange: `ptr + size` wraps on uint32,
// so a naive bounds check is trivially satisfied at this value.
func outOfBoundsAllocator() []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32}, []byte{typeI32}),
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}),
	)
	w.funcs(0, 1)
	w.memory(1)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 0),
		export(ABISignRequest, 0x00, 1),
	)
	// i32.const must be encoded as a signed 32-bit LEB; 2^32-1 is -1 as int32.
	alloc := []byte{opI32Const}
	alloc = append(alloc, sleb(-1)...)
	alloc = append(alloc, opEnd)
	sign := []byte{opI32Const, 0x00, opEnd}
	w.code(alloc, sign)
	return w.bytes()
}

// importingSigner builds a guest that imports a host function it was never
// offered — the canonical escape attempt.
//
//	(import "env" "read_file" (func (param i32) (result i32)))
//
// The sandbox registers no host modules, so this must fail to INSTANTIATE. It is
// the structural denial of the filesystem stated as a gate.
func importingSigner() []byte {
	w := newWasm()
	w.types(
		funcType(nil, nil),                                  // the imported signature
		funcType([]byte{typeI32}, []byte{typeI32}),          // allocate
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}), // sign_request
	)
	w.importSection(importEntry("env", "read_file", 0))
	w.funcs(1, 2) // func 0 is the import, so the defined ones shift up
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 1),
		export(ABISignRequest, 0x00, 2),
	)
	alloc := []byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	}
	sign := []byte{opI32Const, 0x00, opEnd}
	w.code(alloc, sign)
	return w.bytes()
}

// wasiGuest builds a guest that imports WASI. It is refused for the same reason
// as any other import, but it is the one a user is most likely to ship by
// accident (every TinyGo and Rust wasm32 target imports it), so it earns its
// own gate.
func wasiGuest() []byte {
	w := newWasm()
	w.types(
		// fd_write's REAL signature is (i32,i32,i32,i32) -> i32. Importing it
		// under any other type index fails to link for a reason that has
		// nothing to do with WASI being absent, so mutation M54 would pass while
		// proving nothing: the guest would be refused for a bad signature with or
		// without WASI linked.
		funcType([]byte{typeI32, typeI32, typeI32, typeI32}, []byte{typeI32}), // fd_write
		funcType([]byte{typeI32}, []byte{typeI32}),                            // allocate
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}),                   // sign_request
	)
	w.importSection(importEntry("wasi_snapshot_preview1", "fd_write", 0))
	w.funcs(1, 2)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 1),
		export(ABISignRequest, 0x00, 2),
	)
	alloc := []byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	}
	sign := []byte{opI32Const, 0x00, opEnd}
	w.code(alloc, sign)
	return w.bytes()
}

// noMemoryGuest builds a module that satisfies the function ABI but exports no
// memory, which the sandbox must reject at construction.
func noMemoryGuest() []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32}, []byte{typeI32}),
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}),
	)
	w.funcs(0, 1)
	w.exports(
		export(ABIAllocate, 0x00, 0),
		export(ABISignRequest, 0x00, 1),
	)
	alloc := []byte{opLocalGet, 0x00, opEnd}
	sign := []byte{opI32Const, 0x00, opEnd}
	w.code(alloc, sign)
	return w.bytes()
}

// noAllocateGuest builds a module with memory and sign_request but no allocate.
func noAllocateGuest() []byte {
	w := newWasm()
	w.types(funcType([]byte{typeI32, typeI32}, []byte{typeI32}))
	w.funcs(0)
	w.memory(1)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABISignRequest, 0x00, 0),
	)
	w.code([]byte{opI32Const, 0x00, opEnd})
	return w.bytes()
}

// growingSigner builds a guest that grows its linear memory during a call.
//
//	(func (export "sign_request") (param $ptr i32) (param $size i32) (result i32)
//	  i32.const 4
//	  memory.grow 0
//	  drop
//	  i32.const 0)
//
// It exists to pin the claim in memory.go that the bounds check reads the LIVE
// memory size rather than a value captured at instantiation. The host writes the
// envelope BEFORE this runs, so a host that cached the size at New would still
// pass this round trip; what it would get WRONG is the reported size afterwards,
// which is what the gate asserts.
func growingSigner() []byte {
	w := newWasm()
	w.types(
		funcType([]byte{typeI32}, []byte{typeI32}),
		funcType([]byte{typeI32, typeI32}, []byte{typeI32}),
	)
	w.funcs(0, 1)
	w.memory(1)
	w.global(1024)
	w.exports(
		export(ABIMemory, 0x02, 0),
		export(ABIAllocate, 0x00, 0),
		export(ABISignRequest, 0x00, 1),
	)
	alloc := []byte{
		opGlobalGet, 0x00,
		opGlobalGet, 0x00,
		opLocalGet, 0x00,
		opI32Add,
		opGlobalSet, 0x00,
		opEnd,
	}
	sign := []byte{
		opI32Const, 0x04, // grow by 4 pages
		opMemoryGrow, 0x00,
		opDrop,
		opI32Const, 0x00,
		opEnd,
	}
	w.code(alloc, sign)
	return w.bytes()
}
