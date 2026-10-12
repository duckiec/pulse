//go:build linux

package ast_test

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"pulse/internal/ast"
)

// TestMemoryCeilingRSS is the RSS-flavoured memory-ceiling gate: the resident
// footprint of a live 10,000-request document must stay within 2x its source
// size. Linux-only (reads /proc/self/statm); other platforms run the portable
// TotalAlloc gate in parser_test.go.
func TestMemoryCeilingRSS(t *testing.T) {
	src := genRequests(10_000)

	// Warm the arena so the measurement is the steady state, not first-touch
	// growth, then measure the resident delta while the document is live.
	warm := ast.AcquireArena()
	warm.Reset(src)
	warmDoc, err := ast.Parse(warm)
	if err != nil {
		t.Fatalf("warm-up parse: %v", err)
	}
	warmDoc.Release()

	runtime.GC()
	before := rssBytes(t)

	arena := ast.AcquireArena()
	arena.Reset(src)
	doc, err := ast.Parse(arena)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	runtime.GC() // reclaim garbage so RSS reflects live arena memory
	after := rssBytes(t)

	if doc.Len() < 9000 {
		t.Fatalf("expected ~10k requests, got %d", doc.Len())
	}
	growth := after - before
	budget := int64(2 * len(src))
	if growth > budget {
		t.Errorf("RSS ceiling exceeded: +%d bytes for a %d-byte source (budget %d = 2x source)",
			growth, len(src), budget)
	}
	t.Logf("RSS growth: +%d bytes for a %d-byte source (%.2fx)", growth, len(src), float64(growth)/float64(len(src)))
	doc.Release()
}

// rssBytes reads the resident set size from /proc/self/statm (field 2, pages).
func rssBytes(t *testing.T) int64 {
	t.Helper()
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		t.Fatalf("read /proc/self/statm: %v", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		t.Fatalf("unexpected statm format: %q", data)
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("parse statm: %v", err)
	}
	return pages * int64(os.Getpagesize())
}
