package ast

import (
	"sync"
	"unsafe"
)

// Memory constants for the arena (ROADMAP.md §1.1.5).
const (
	// arenaAlign is the alignment guarantee for every allocation: 8 bytes,
	// which satisfies the alignment of every AST node type (all fields are
	// int32/uint8/bool or slice/string headers of 8- or 16-byte alignment).
	arenaAlign = 8
	// arenaBlockMin is the smallest retained block. Larger allocations get
	// an exact-fit block so a big []Request never drags a geometric
	// successor block along with it (memory-ceiling discipline).
	arenaBlockMin = 8 << 10
)

// arenaPool recycles arenas (and their retained blocks) across parse cycles.
// A warmed arena survives Release/Acquire, so steady-state parsing performs no
// heap traffic at all.
var arenaPool = sync.Pool{
	New: func() any { return new(Arena) },
}

// Arena is a bump allocator owning every byte an AST references.
//
// Two regions:
//   - src: a private copy of the source buffer. All strings handed out by the
//     parser are views into this copy, so a Document is self-contained and its
//     lifetime is exactly the arena's.
//   - blocks: retained node slabs, filled by a bump pointer, 8-byte aligned.
//     A single large node array (the []Request slab) gets its own exact-fit
//     block so no allocation is ever split across blocks.
//
// The arena performs zero heap allocations once warmed: Reset reuses the
// existing src slab and restarts the bump; Release recycles the whole arena
// (retaining its blocks) into the pool.
type Arena struct {
	src      []byte   // owned source copy (strings alias this)
	blocks   [][]byte // retained node slabs (kept across cycles)
	blockIdx int      // current block during this cycle
	cur      []byte   // current slab
	off      int      // bump offset within cur
}

// AcquireArena takes an arena from the pool. Call [Arena.Reset] before use and
// [Document.Release] (or [Arena.Release]) when the AST is done.
func AcquireArena() *Arena { return arenaPool.Get().(*Arena) }

// Reset binds the arena to src (copying it once) and restarts the bump pointer,
// retaining every block for reuse. This is the only place src is copied.
func (a *Arena) Reset(src []byte) {
	if cap(a.src) < len(src) {
		a.src = make([]byte, len(src))
	} else {
		a.src = a.src[:len(src)]
	}
	copy(a.src, src)
	a.blockIdx = 0
	if len(a.blocks) > 0 {
		a.cur, a.off = a.blocks[0], 0
	} else {
		a.cur, a.off = nil, 0
	}
	a.off = 0
}

// Release recycles the arena into the pool. After Release every Document,
// string, and byte previously obtained from the arena is invalid — the memory
// belongs to the next parse cycle. One-shot: the arena must be Reused via
// Reset or re-Acquired, never Released twice.
//
// The source buffer and the retained blocks deliberately SURVIVE the trip
// through the pool: that retained capacity is what makes a warm cycle allocate
// nothing at all (the G-alloc gate). Only the parse state — current block,
// offset, and block cursor — is cleared.
func (a *Arena) Release() {
	a.blockIdx = 0
	a.cur = nil
	a.off = 0
	arenaPool.Put(a)
}

// alloc returns an n-byte, 8-byte-aligned slice carved from the arena. It never
// returns nil: n <= 0 yields a minimal aligned span (callers treat it as empty).
func (a *Arena) alloc(n int) []byte {
	if n <= 0 {
		n = arenaAlign
	}
	n = (n + arenaAlign - 1) &^ (arenaAlign - 1)
	if a.cur == nil || a.off+n > len(a.cur) {
		a.grow(n)
	}
	b := a.cur[a.off : a.off+n : a.off+n]
	a.off += n
	return b
}

// grow moves to the next block, reusing a retained block when it is large
// enough, otherwise allocating an exact-fit block for the large request.
func (a *Arena) grow(n int) {
	a.blockIdx++
	if a.blockIdx < len(a.blocks) {
		if len(a.blocks[a.blockIdx]) >= n {
			a.cur, a.off = a.blocks[a.blockIdx], 0
			return
		}
		// Retained block too small for this (larger) cycle: drop it and
		// replace with an exact-fit block, truncating so indices stay dense.
		a.blocks = a.blocks[:a.blockIdx]
	}
	size := n
	if size < arenaBlockMin && len(a.blocks) == 0 {
		size = arenaBlockMin
	}
	a.blocks = append(a.blocks, make([]byte, size))
	a.cur, a.off = a.blocks[len(a.blocks)-1], 0
}

// str returns a zero-copy string view into the arena-owned source copy for the
// byte span [start, end). An empty or inverted span yields "" (never a pointer
// into an empty buffer).
func (a *Arena) str(start, end int32) string {
	if end <= start || start < 0 {
		return ""
	}
	if int(end) > len(a.src) {
		end = int32(len(a.src))
	}
	if start >= int32(len(a.src)) || start < 0 {
		return ""
	}
	// The source copy is immutable for the arena's lifetime, so aliasing it as
	// a string is safe: it never mutates after Reset.
	return unsafe.String(&a.src[start], int(end)-int(start))
}

// bytes returns the arena-owned source bytes for the span [start, end) as a
// sub-slice (no copy, no allocation).
func (a *Arena) bytes(start, end int32) []byte {
	if end <= start || start < 0 || int(end) > len(a.src) {
		return nil
	}
	return a.src[start:end]
}

// allocSlice carves a []T of length n from the arena. The backing array lives in
// arena memory; the slice header itself is a normal Go value on the stack/frame.
// n <= 0 yields a zero-length, zero-cap slice (nil-safe, no allocation).
func allocSlice[T any](a *Arena, n int) []T {
	if n <= 0 {
		return nil
	}
	b := a.alloc(n * int(unsafe.Sizeof(*new(T))))
	return unsafe.Slice((*T)(unsafe.Pointer(&b[0])), n)
}
