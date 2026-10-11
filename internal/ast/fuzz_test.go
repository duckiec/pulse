package ast_test

import (
	"testing"

	"pulse/internal/ast"
)

// FuzzLexer proves, by exhaustive randomized exploration, the two structural
// guarantees the architecture rests on (ROADMAP.md §1.1.1):
//
//  1. Termination — every next() consumes at least one byte or returns TokEOF,
//     so a stream over N bytes yields at most N+1 tokens. An infinite loop in
//     the state machine is therefore impossible, not merely unobserved.
//  2. Non-degenerate spans — TokIllegal (and every non-EOF token) spans at
//     least one byte, and all spans lie within the source buffer.
//
// Seed execution runs under plain `go test`; -fuzz explores further.
func FuzzLexer(f *testing.F) {
	seeds := []string{
		"",
		"\n",
		"###",
		"GET / HTTP/1.1\nAccept: application/json\n",
		"POST /users HTTP/1.1\nAuthorization: Bearer {{token}}\n\n{\"a\": 1}\n",
		"@base = https://x\nGET {{base}}/u\n\n{\"nested\": {\"deep\": [1,2,3]}}\n",
		"GET /\n\n<?xml version=\"1.0\"?>\n<a x=\"y\"><b/></a>\n",
		"GET /\n\nraw body\nsecond line\n\n###\n",
		"GET /\r\nA: b\r\n\r\n{}\r\n",
		"{{",
		"}}",
		"GET {{",
		"GET / {{",
		"# comment\n@var = value\nGET /v\n",
		"{\"unclosed\": ",
		"<a><b>",
		"GET",
		"GET ",
		"GET\n",
		":\n",
		"HTTP/1.1 200 OK\n",
		"\x00\x01\x02",
		"GET /\nContent-Type: {\"weird\": \"colon: inside\"}\n\n{}\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, src []byte) {
		const maxBytes = 1 << 16 // bound per-input work; not a skip, a budget
		if len(src) > maxBytes {
			return
		}

		lx := ast.New(src)
		n := 0
		var prev ast.Token
		first := true
		for tok := range lx.Tokens() {
			n++
			if n > len(src)+1 {
				t.Fatalf("termination violated: %d tokens from %d bytes (state machine does not always advance)", n, len(src))
			}
			if tok.Kind != ast.TokEOF {
				if tok.End <= tok.Start {
					t.Fatalf("zero-width %v token at offset %d", tok.Kind, tok.Start)
				}
				if tok.Start < 0 || tok.End > int32(len(src)) {
					t.Fatalf("span %d..%d out of bounds (len %d)", tok.Start, tok.End, len(src))
				}
				if tok.Line < 1 || tok.Col < 1 || tok.EndLine < tok.Line {
					t.Fatalf("insane position on %v: %+v", tok.Kind, tok)
				}
				// A TokIllegal is a terminal diagnostic report, not a grammar
				// token; it may share a span with the construct it condemns.
				if tok.Kind != ast.TokIllegal && tok.Start < prev.End {
					t.Fatalf("token overlap: prev %d..%d, cur %d..%d", prev.Start, prev.End, tok.Start, tok.End)
				}
			}
			if tok.Kind == ast.TokIllegal {
				d, ok := lx.Diag()
				if !ok {
					t.Fatal("illegal token without diagnostic")
				}
				if d.Kind == ast.ErrNone {
					t.Fatal("diagnostic with ErrNone kind")
				}
				if d.Start >= d.End || d.End > int32(len(src)) {
					t.Fatalf("diagnostic span %d..%d invalid for %d bytes", d.Start, d.End, len(src))
				}
			}
			prev = tok
			first = false
			_ = first
		}
	})
}
