package ast

import (
	"sync"
	"testing"
	"unsafe"
)

// TestTokenAlignment locks the arena's alignment contract: every allocation is
// 8-byte aligned, which covers every AST node type (int32 fields and slice
// headers).
func TestTokenAlignment(t *testing.T) {
	a := AcquireArena()
	defer a.Release()
	a.Reset([]byte("x"))
	for _, n := range []int{1, 2, 3, 7, 8, 9, 17, 33, 64, 65, 129, 1024} {
		b := a.alloc(n)
		if uintptr(unsafe.Pointer(&b[0]))%8 != 0 {
			t.Fatalf("alloc(%d) not 8-byte aligned: %%#x", uintptr(unsafe.Pointer(&b[0])))
		}
		if len(b)%8 != 0 && len(b) != 0 {
			t.Fatalf("alloc(%d) returned a span of non-multiple-of-8 length %d", n, len(b))
		}
		if len(b) < n {
			t.Fatalf("alloc(%d) returned a span of length %d", n, len(b))
		}
	}
}

// TestArenaReusesBlocks proves a warmed arena performs no new block allocation
// across cycles — the mechanism behind the 0-alloc/op benchmark gate.
func TestArenaReusesBlocks(t *testing.T) {
	a := AcquireArena()
	defer a.Release()
	src := make([]byte, 64<<10)
	a.Reset(src)
	first := allocSlice[Request](a, 4096)
	blocksAfterFirst := len(a.blocks)

	a.Reset(src)
	second := allocSlice[Request](a, 4096)
	if len(a.blocks) != blocksAfterFirst {
		t.Fatalf("second cycle allocated new blocks: %d -> %d", blocksAfterFirst, len(a.blocks))
	}
	if uintptr(unsafe.Pointer(&first[0])) != uintptr(unsafe.Pointer(&second[0])) {
		t.Fatal("second cycle did not reuse the first block's memory")
	}
}

// TestArenaStrViews verifies arena string views alias the owned source copy and
// are never empty-but-invalid.
func TestArenaStrViews(t *testing.T) {
	a := AcquireArena()
	defer a.Release()
	src := []byte("hello {{world}} end")
	a.Reset(src)
	if got := a.str(0, 5); got != "hello" {
		t.Fatalf("str(0,5) = %q", got)
	}
	if got := a.str(6, 15); got != "{{world}}" {
		t.Fatalf("str(6,14) = %q", got)
	}
	if got := a.str(5, 5); got != "" {
		t.Fatalf("empty span should be empty string, got %q", got)
	}
	if got := a.str(9, 3); got != "" {
		t.Fatalf("inverted span should be empty string, got %q", got)
	}
	if got := a.str(-1, -1); got != "" {
		t.Fatalf("negative span should be empty string, got %q", got)
	}
	if string(a.bytes(0, 5)) != "hello" {
		t.Fatalf("bytes(0,5) = %q", a.bytes(0, 5))
	}
	if a.bytes(5, 5) != nil {
		t.Fatal("empty bytes span should be nil")
	}
}

// TestArenaSourceIsolation proves the arena owns its source copy: mutating the
// caller's buffer after Reset cannot corrupt AST strings.
func TestArenaSourceIsolation(t *testing.T) {
	src := []byte("GET /users\n")
	a := AcquireArena()
	defer a.Release()
	a.Reset(src)
	for i := range src {
		src[i] = 'Z'
	}
	if got := a.str(4, 10); got != "/users" {
		t.Fatalf("arena string aliased the caller's mutable buffer: %q", got)
	}
}

// TestArenaPoolRecycles documents the pool contract under concurrency: many
// arenas can be acquired and released concurrently without cross-talk.
func TestArenaPoolRecycles(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 32 {
				a := AcquireArena()
				a.Reset([]byte("GET /a\n\n{}\n"))
				_ = allocSlice[Request](a, 8)
				_ = a.str(0, 3)
				a.Release()
			}
		}()
	}
	wg.Wait()
}

// TestArenaLargeAllocationExactFit verifies a large node array gets its own
// single exact-fit block instead of forcing a geometric successor block (the
// memory-ceiling discipline).
//
// It deliberately builds a pristine Arena rather than acquiring one from the
// pool: a recycled arena may already hold a block large enough to serve the
// allocation, which would hide the growth path this test exists to cover.
func TestArenaLargeAllocationExactFit(t *testing.T) {
	a := &Arena{}
	a.Reset(nil)
	if len(a.blocks) != 0 {
		t.Fatalf("pristine arena should have no retained blocks, got %d", len(a.blocks))
	}

	big := allocSlice[Header](a, 100_000) // 100k * 20B = 2 MB
	if len(big) != 100_000 {
		t.Fatalf("len = %d", len(big))
	}
	if len(a.blocks) != 1 {
		t.Fatalf("expected exactly one block for the large array, got %d", len(a.blocks))
	}
	if cap(a.blocks[0]) < 100_000*int(unsafe.Sizeof(Header{})) {
		t.Fatalf("block too small: %d", cap(a.blocks[0]))
	}
	// The array must live entirely inside that one block.
	if got := uintptr(unsafe.Pointer(&big[0])); got < uintptr(unsafe.Pointer(&a.blocks[0][0])) ||
		got+uintptr(len(big))*uintptr(unsafe.Sizeof(Header{})) > uintptr(unsafe.Pointer(&a.blocks[0][0]))+uintptr(cap(a.blocks[0])) {
		t.Fatal("large array is not fully contained in its exact-fit block")
	}
}
