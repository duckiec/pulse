package ast_test

import (
	"fmt"
	"strings"
	"testing"

	"pulse/internal/ast"
)

// genCorpus builds a deterministic ~10k-line .http document exercising every
// lexer construct: separators, comments, variables, interpolation anchors in
// targets and header values, JSON bodies with nested braces and braces inside
// strings, and CRLF-free LF line endings.
func genCorpus(lines int) []byte {
	var b strings.Builder
	written := 0
	for i := 0; written < lines; i++ {
		fmt.Fprintf(&b, "### generated request %d\n", i)                     // 1
		b.WriteString("# @name gen-" + itoa(i) + "\n")                       // 2
		b.WriteString("@base = https://api.example.com\n")                   // 3
		fmt.Fprintf(&b, "GET {{base}}/resource/%d?page=%d HTTP/1.1\n", i, i) // 4
		b.WriteString("Accept: application/json\n")                          // 5
		b.WriteString("Authorization: Bearer {{token}}\n")                   // 6
		b.WriteString("X-Request-Id: {{req_id}}-suffix\n")                   // 7
		b.WriteString("\n")                                                  // 8
		b.WriteString("{\n")                                                 // 9
		fmt.Fprintf(&b, "  \"id\": %d,\n", i)                                // 10
		b.WriteString("  \"tags\": [\"a\", \"b\"],\n")                       // 11
		b.WriteString("  \"note\": \"brace } inside string\",\n")            // 12
		b.WriteString("  \"nested\": {\"deep\": {\"deeper\": true}}\n")      // 13
		b.WriteString("}\n")                                                 // 14
		b.WriteString("\n")                                                  // 15
		written += 15
	}
	return []byte(b.String())
}

func itoa(i int) string { return fmt.Sprintf("%d", i) }

// BenchmarkLexer is the zero-allocation gate (ROADMAP.md §1.1.5, G-alloc):
// tokenizing the 10k-line corpus through the iter.Seq interface MUST report
// 0 B/op and 0 allocs/op. CI fails the build otherwise
// (scripts/ci/test-gates.sh).
func BenchmarkLexer(b *testing.B) {
	src := genCorpus(10_000)
	lx := ast.New(nil)
	b.ReportAllocs()
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lx.Reset(src)
		n := 0
		for tok := range lx.Tokens() {
			n += int(tok.Kind)
		}
		if n < 0 {
			b.Fatal("unreachable")
		}
	}
}

// BenchmarkLexerReset documents that rebinding a lexer to a new document
// allocates nothing beyond the single New above it.
func BenchmarkLexerReset(b *testing.B) {
	src := genCorpus(10_000)
	lx := ast.New(src)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lx.Reset(src)
	}
}
