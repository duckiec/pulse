package wasm

import (
	"context"
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero/api"
)

// # The ABI
//
// A signing guest exports exactly three things:
//
//	(memory)                                  linear memory, host-visible
//	(func (export "allocate") (param i32) (result i32))
//	(func (export "sign_request") (param i32 i32) (result i32))
//
// and the host drives them in a fixed order:
//
//	1. ptr := allocate(len(payload) + HEADROOM)   0 means allocation failed
//	2. memory[ptr : ptr+len] = payload            the pre-flight envelope
//	3. n    := sign_request(ptr, len)             signs IN PLACE; len is the
//	                                              ENVELOPE length, not the
//	                                              capacity, so a guest cannot
//	                                              mistake the tail for content
//	4. result = memory[ptr : ptr+n]               the signed envelope
//
// A guest that APPENDS (the usual case: a signature header after the request)
// writes at ptr+len and returns len+n. A guest that OVERWRITES returns at most
// len. Both are legal; both are bounds-checked against the reservation.
//
// # Why in-place, and why that is safer
//
// ROADMAP.md §3.4.2 sketched the guest returning `(sig_ptr, sig_len)` — a
// pointer and a length the HOST must then trust. This implementation does not,
// and the difference is the whole safety argument:
//
// A guest-returned pointer is a pointer the host must bounds-check before
// believing, and a bounds check that is wrong is a memory-safety bug in the
// host. Pinning the output to the buffer the host already allocated removes the
// untrusted pointer entirely: the only guest-supplied value the host converts to
// an address is LENGTH, and a length is checked against a buffer the host owns.
// There is no address for a malicious guest to choose.
//
// The cost is that the output cannot exceed the input buffer. That is the right
// trade for this feature: a signature is headers plus a digest, never more, and
// a guest that needs a larger output buffer can call allocate twice.
//
// # Every host-supplied address is checked, in this order
//
//	ptr + size <= memory.Size()             before any Read or Write
//	n <= size                               before the read-back
//	ptr + n   <= memory.Size()               before the read-back
//
// The first is checked against the LIVE memory size rather than a value captured
// at instantiation, because a guest may call memory.grow between the two
// points and the host's earlier view would then be stale.

// Export names a guest must provide.
const (
	// ABIAllocate reserves size bytes and returns the offset.
	ABIAllocate = "allocate"
	// ABISignRequest signs the envelope at (ptr, size) in place and returns
	// the number of bytes of OUTPUT written. A negative value is an ABI error.
	ABISignRequest = "sign_request"
	// ABIHandleRequest rewrites the pending request at (ptr, size) in place and
	// returns the number of bytes of OUTPUT written. The payload and the result
	// are both JSON (the document shape is the caller's contract, documented in
	// docs/FEATURES.md). Optional: a signing-only guest need not export it, and
	// a hook-only guest need not export sign_request.
	ABIHandleRequest = "handle_request"
	// ABIMemory is the export name of the linear memory.
	ABIMemory = "memory"
)

// ErrABIViolation reports a guest that broke the memory contract.
var ErrABIViolation = errors.New("wasm: guest violated the memory ABI")

// ErrBounds reports a host-side range that would leave linear memory.
//
// It is distinct from [ErrABIViolation] because the two have different
// authors: this one means the HOST computed a bad range, which is a bug in
// Pulse, and the other means the GUEST reported something impossible, which is
// a bug or an attack in the script.
var ErrBounds = errors.New("wasm: memory access would leave linear memory")

// OutputHeadroom is how many bytes BEYOND the envelope the host reserves for a
// guest's output.
//
// It exists because the ABI signs in place. A guest that appends a signature
// header needs somewhere to put it, and without a reservation it would either
// have to overwrite the request — destroying the thing it was asked to sign —
// or overrun the buffer it was given. Reserving the headroom makes "append" the
// natural operation and "overwrite the request" an error the host can see.
//
// 4 KiB is generous for a signature (an HMAC-SHA256 header is under 100 bytes)
// and bounded, so the reservation is a fixed cost rather than a guest-chosen
// one.
const OutputHeadroom = 4096

// MaxEnvelopeBytes bounds the pre-flight envelope a guest may be handed.
//
// It is a host-side ceiling on purpose. A signing script has no legitimate
// reason to sign a megabyte of request, and the limit is what stops a caller
// from using the sandbox as an allocation primitive: without it,
// `allocate(len)` is a request for `len` bytes of guest memory, and the guest's
// memory limit becomes the only thing standing between a bad caller and a full
// sandbox.
const MaxEnvelopeBytes = 1 << 20 // 1 MiB

// HookHeadroom is the output reservation for a `handle_request` call.
//
// A hook REWRITES the request document, so its output can be larger than its
// input (an appended Authorization header, a re-encoded body) in a way a
// signature never is. 64 KiB is still a fixed host-side cost, and the guest's
// 4 MiB default linear-memory ceiling remains the hard bound.
const HookHeadroom = 64 << 10

// invokeSign performs the signing memory round trip.
func (m *Module) invokeSign(ctx context.Context, payload []byte) ([]byte, error) {
	if m.sign == nil {
		return nil, fmt.Errorf("%w: the guest does not export %s", ErrABIViolation, ABISignRequest)
	}
	return m.invoke(ctx, m.sign, payload, OutputHeadroom, ABISignRequest)
}

// invokeHook performs the hook memory round trip.
func (m *Module) invokeHook(ctx context.Context, payload []byte) ([]byte, error) {
	if m.hook == nil {
		return nil, fmt.Errorf("%w: the guest does not export %s", ErrABIViolation, ABIHandleRequest)
	}
	return m.invoke(ctx, m.hook, payload, HookHeadroom, ABIHandleRequest)
}

// invoke is the shared in-place ABI: allocate, write the payload, call the
// guest's in-place function with the PAYLOAD length, then read back the length
// the guest reported. Both ABIs are identical bar the headroom and the export
// name, so they share one bounds-checked implementation rather than drifting
// copies.
func (m *Module) invoke(ctx context.Context, fn api.Function, payload []byte, headroom uint32, what string) ([]byte, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: the payload is empty", ErrABIViolation)
	}
	if len(payload) > MaxEnvelopeBytes {
		return nil, fmt.Errorf("wasm: payload is %d bytes, over the %d-byte ceiling",
			len(payload), MaxEnvelopeBytes)
	}

	// The reservation is len+headroom, and the two are added as int64 so the
	// sum cannot wrap uint32 before it is bounds-checked.
	capacity := uint64(len(payload)) + uint64(headroom)
	if capacity > uint64(^uint32(0)) {
		return nil, fmt.Errorf("%w: the payload plus headroom exceeds linear memory",
			ErrABIViolation)
	}

	ptr, err := m.allocate(ctx, uint32(capacity))
	if err != nil {
		return nil, err
	}

	if err := m.write(ptr, payload); err != nil {
		return nil, err
	}

	// The guest is told the PAYLOAD length, not the capacity: that is the
	// boundary between "what you were asked to process" and "where you may
	// write". A guest that confuses them can write over the request, and the
	// host cannot distinguish that from a correct answer.
	res, err := fn.Call(ctx, uint64(ptr), uint64(len(payload)))
	if err != nil {
		return nil, err
	}
	if len(res) != 1 {
		return nil, fmt.Errorf("%w: %s returned %d values, want 1",
			ErrABIViolation, what, len(res))
	}
	n := int64(int32(uint32(res[0])))
	if n < 0 {
		return nil, fmt.Errorf("%w: %s returned the error code %d",
			ErrABIViolation, what, n)
	}
	// The output length is the ONLY guest-supplied value that becomes a range,
	// and it is checked against the RESERVED CAPACITY before it becomes an
	// address arithmetic operand.
	if uint64(n) > capacity {
		return nil, fmt.Errorf("%w: %s claims %d output bytes for a %d-byte buffer",
			ErrABIViolation, what, n, capacity)
	}

	return m.read(ptr, uint32(n))
}

// allocate calls the guest's allocator and bounds-checks its answer.
//
// A guest that returns 0 (the conventional out-of-memory signal) is reported as
// an ABI error rather than silently passed to a write at offset zero, which
// would scribble over the guest's own stack frame.
func (m *Module) allocate(ctx context.Context, size uint32) (uint32, error) {
	res, err := m.alloc.Call(ctx, uint64(size))
	if err != nil {
		return 0, err
	}
	if len(res) != 1 {
		return 0, fmt.Errorf("%w: allocate returned %d values, want 1", ErrABIViolation, len(res))
	}
	ptr := uint32(res[0])
	if ptr == 0 {
		return 0, fmt.Errorf("%w: allocate(%d) returned 0", ErrABIViolation, size)
	}
	if err := m.checkRange(ptr, size); err != nil {
		return 0, err
	}
	return ptr, nil
}

// write copies the envelope into guest memory.
func (m *Module) write(ptr uint32, b []byte) error {
	if err := m.checkRange(ptr, uint32(len(b))); err != nil {
		return err
	}
	if !m.mem.Write(ptr, b) {
		// Unreachable given checkRange, and checked anyway: a false here means
		// the guest shrank its memory between the check and the write, and
		// proceeding would either drop bytes or panic.
		return fmt.Errorf("%w: write of %d bytes at %d was refused", ErrBounds, len(b), ptr)
	}
	return nil
}

// read copies bytes back out of guest memory into a HOST-OWNED slice.
//
// The copy is not optional. api.Memory.Read returns a VIEW into the guest's
// linear memory, and handing that view to a caller would give it a live window
// that a concurrent guest write could mutate under it — a signature that changes
// after it has been verified.
func (m *Module) read(ptr, size uint32) ([]byte, error) {
	if size == 0 {
		return []byte{}, nil
	}
	if err := m.checkRange(ptr, size); err != nil {
		return nil, err
	}
	view, ok := m.mem.Read(ptr, size)
	if !ok {
		return nil, fmt.Errorf("%w: read of %d bytes at %d was refused", ErrBounds, size, ptr)
	}
	out := make([]byte, len(view))
	copy(out, view)
	return out, nil
}

// checkRange is the single bounds gate every host access passes through.
//
// The size is read LIVE, not captured at instantiation, because the guest may
// have called memory.grow since. The overflow-safe comparison matters: `ptr+size`
// on uint32 wraps, and `ptr+size <= mem.Size()` is then trivially true for a
// pointer near 2^32 — which is exactly the value a hostile guest would return.
// Subtracting is written so that it cannot wrap.
func (m *Module) checkRange(ptr, size uint32) error {
	avail := m.mem.Size()
	if ptr > avail {
		return fmt.Errorf("%w: offset %d is past the %d-byte memory", ErrBounds, ptr, avail)
	}
	if size > avail-ptr {
		return fmt.Errorf("%w: %d bytes at %d exceeds the %d-byte memory",
			ErrBounds, size, ptr, avail)
	}
	return nil
}

// Memory exposes the guest's linear memory for callers that need to inspect it.
//
// It is the raw handle, and it carries none of this package's bounds checks —
// which is why it is not part of the signing path and exists only so a test or
// a diagnostic can read a byte back. A caller that uses it to WRITE bypasses
// every guarantee above.
func (m *Module) Memory() api.Memory { return m.mem }
