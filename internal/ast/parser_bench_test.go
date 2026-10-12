package ast_test

import (
	"fmt"
	"strings"
	"testing"

	"pulse/internal/ast"
)

// genRequests builds a .http document containing exactly n requests, each with
// a separator, a "# @name" comment, a file-scoped variable, an interpolated
// target, three headers (one with two anchors), and a nested JSON body. This is
// the fixture shape both parser gates require: a 10,000-request AST
// (ROADMAP.md §1.1.5).
func genRequests(n int) []byte {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "### generated request %d\n", i)
		fmt.Fprintf(&b, "# @name gen-%d\n", i)
		b.WriteString("@base = https://api.example.com\n")
		fmt.Fprintf(&b, "GET {{base}}/resource/%d?page=%d HTTP/1.1\n", i, i)
		b.WriteString("Accept: application/json\n")
		b.WriteString("Authorization: Bearer {{token}}\n")
		b.WriteString("X-Request-Id: {{req_id}}-suffix\n")
		b.WriteString("\n")
		b.WriteString("{\n")
		fmt.Fprintf(&b, "  \"id\": %d,\n", i)
		b.WriteString("  \"tags\": [\"a\", \"b\"],\n")
		b.WriteString("  \"note\": \"brace } inside string\",\n")
		b.WriteString("  \"nested\": {\"deep\": {\"deeper\": true}}\n")
		b.WriteString("}\n")
		b.WriteString("\n")
	}
	return []byte(b.String())
}

// BenchmarkParseCached is the zero-allocation gate for the parser (ROADMAP.md
// §1.1.5, G-alloc): constructing a 10,000-request AST must report exactly
// 0 B/op and 0 allocs/op. The arena is warmed first (two full parses) so the
// measured loop exercises the steady state every long-lived process hits:
// arena blocks, the lexer, and the parser are all recycled through their pools.
func BenchmarkParseCached(b *testing.B) {
	src := genRequests(10_000)

	// Warm-up: size the pooled arena (and its blocks) to this fixture.
	for range 2 {
		arena := ast.AcquireArena()
		arena.Reset(src)
		doc, err := ast.Parse(arena)
		if err != nil {
			b.Fatalf("warm-up parse: %v", err)
		}
		doc.Release()
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		arena := ast.AcquireArena()
		arena.Reset(src)
		doc, err := ast.Parse(arena)
		if err != nil {
			b.Fatalf("parse: %v", err)
		}
		if doc.Len() == 0 {
			b.Fatal("parsed an empty document")
		}
		doc.Release()
	}
}

// BenchmarkParseSmall measures the per-request cost on a realistic single-file
// collection, which is the common interactive case.
func BenchmarkParseSmall(b *testing.B) {
	src := genRequests(8)
	for range 2 {
		arena := ast.AcquireArena()
		arena.Reset(src)
		doc, err := ast.Parse(arena)
		if err != nil {
			b.Fatal(err)
		}
		doc.Release()
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		arena := ast.AcquireArena()
		arena.Reset(src)
		doc, err := ast.Parse(arena)
		if err != nil {
			b.Fatal(err)
		}
		doc.Release()
	}
}
