package ast_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"pulse/internal/ast"
)

var update = flag.Bool("update", false, "regenerate golden token files")

// TestTokenSize locks the zero-allocation contract: ast.Token is exactly 32
// bytes (eight 4-byte fields, no padding) so tokens flow through iter.Seq
// without heap traffic (ROADMAP.md §1.1.5).
func TestTokenSize(t *testing.T) {
	if got := unsafe.Sizeof(ast.Token{}); got != 32 {
		t.Fatalf("ast.Token is %d bytes; the contract requires exactly 32", got)
	}
}

// lexAll drains the token stream of src and returns it with the diagnostic.
func lexAll(src []byte) ([]ast.Token, ast.Diag, bool) {
	lx := ast.New(src)
	var toks []ast.Token
	for tok := range lx.Tokens() {
		toks = append(toks, tok)
	}
	d, ok := lx.Diag()
	return toks, d, ok
}

func formatStream(toks []ast.Token, d ast.Diag, hasDiag bool) string {
	var b strings.Builder
	for _, t := range toks {
		fmt.Fprintf(&b, "%v %d-%d %d:%d-%d:%d %d\n",
			t.Kind, t.Start, t.End, t.Line, t.Col, t.EndLine, t.EndCol, t.Depth)
	}
	if hasDiag {
		fmt.Fprintf(&b, "DIAG %v %d-%d %d:%d\n", d.Kind, d.Start, d.End, d.Line, d.Col)
	}
	return b.String()
}

// TestGoldenCorpus verifies the lexer against the testdata corpus. Golden
// files are regenerated with: go test ./internal/ast -run TestGoldenCorpus -update
func TestGoldenCorpus(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "http")
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".http") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk corpus: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no corpus files found")
	}
	goldenDir := filepath.Join(root, "golden")
	for _, path := range files {
		name := strings.TrimPrefix(path, root+string(filepath.Separator))
		name = strings.ReplaceAll(name, string(filepath.Separator), "-") + ".tokens"
		goldenPath := filepath.Join(goldenDir, name)

		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		toks, d, hasDiag := lexAll(src)
		got := formatStream(toks, d, hasDiag)

		if *update {
			if err := os.MkdirAll(goldenDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(goldenPath)
		if err != nil {
			t.Fatalf("read golden %s (run with -update): %v", goldenPath, err)
		}
		if got != string(want) {
			t.Errorf("%s: token stream mismatch (run with -update if the grammar moved)\n--- got ---\n%s--- want ---\n%s",
				path, got, want)
		}
	}
}

// TestLexBehavior pins exact token-kind sequences per construct. Every case is
// a characterization of documented grammar behavior (AGENTS.md §4.2.4).
func TestLexBehavior(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []ast.TokenKind
	}{
		{"full request", "POST https://x.com/a HTTP/1.1\nContent-Type: application/json\n\n{\"a\": 1}\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokHTTPVersion, ast.TokNewline,
				ast.TokHeaderName, ast.TokHeaderValue, ast.TokNewline, ast.TokBlankLine,
				ast.TokBodyOpen, ast.TokBodyClose, ast.TokNewline, ast.TokEOF}},
		{"no version", "GET /x\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokEOF}},
		{"interp url", "GET {{base}}/u\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokInterp, ast.TokURL, ast.TokNewline, ast.TokEOF}},
		{"interp header value", "GET /\nA: pre {{t}} post\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokHeaderName,
				ast.TokHeaderValue, ast.TokInterp, ast.TokHeaderValue, ast.TokNewline, ast.TokEOF}},
		{"separator no body", "GET /\n\n###\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokBlankLine,
				ast.TokSeparator, ast.TokNewline, ast.TokEOF}},
		{"json body brace in string", "POST /\nC: v\n\n{\"a\":\"}\"}\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokHeaderName,
				ast.TokHeaderValue, ast.TokNewline, ast.TokBlankLine,
				ast.TokBodyOpen, ast.TokBodyClose, ast.TokNewline, ast.TokEOF}},
		{"xml body", "POST /\n\n<a><b/></a>\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokBlankLine,
				ast.TokBodyOpen, ast.TokBodyClose, ast.TokNewline, ast.TokEOF}},
		{"xml self-closing root", "POST /\n\n<a/>\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokBlankLine,
				ast.TokBodyOpen, ast.TokBodyClose, ast.TokNewline, ast.TokEOF}},
		{"xml prolog", "POST /\n\n<?xml version=\"1.0\"?>\n<a>hi</a>\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokBlankLine,
				ast.TokBodyOpen, ast.TokBodyClose, ast.TokNewline, ast.TokEOF}},
		{"raw body", "POST /\n\nline1\nline2\n\n###\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokBlankLine,
				ast.TokBodyOpen, ast.TokBlankLine, ast.TokSeparator, ast.TokNewline, ast.TokEOF}},
		{"crlf", "GET /\r\nA: b\r\n\r\n{}\r\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokHeaderName,
				ast.TokHeaderValue, ast.TokNewline, ast.TokBlankLine,
				ast.TokBodyOpen, ast.TokBodyClose, ast.TokNewline, ast.TokEOF}},
		{"separator only", "###\n",
			[]ast.TokenKind{ast.TokSeparator, ast.TokNewline, ast.TokEOF}},
		{"variable", "@a = 1\n",
			[]ast.TokenKind{ast.TokVariable, ast.TokNewline, ast.TokEOF}},
		{"comment", "# hi\n",
			[]ast.TokenKind{ast.TokComment, ast.TokNewline, ast.TokEOF}},
		{"empty", "", []ast.TokenKind{ast.TokEOF}},
		{"blank lines", "\n\n",
			[]ast.TokenKind{ast.TokBlankLine, ast.TokBlankLine, ast.TokEOF}},
		{"garbage", "hello\n", []ast.TokenKind{ast.TokIllegal}},
		{"unknown method", "FETCH /\n", []ast.TokenKind{ast.TokIllegal}},
		{"method no target", "GET\n", []ast.TokenKind{ast.TokIllegal}},
		{"bad header", "GET /\nabc\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokIllegal}},
		{"unterminated json", "POST /\n\n{\"a\":\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokBlankLine,
				ast.TokBodyOpen, ast.TokIllegal}},
		{"unterminated xml", "POST /\n\n<a><b>\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokNewline, ast.TokBlankLine,
				ast.TokBodyOpen, ast.TokIllegal}},
		{"unterminated interp", "GET {{x\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokIllegal}},
		{"version missing prefix", "GET / HTTP1.1\n",
			[]ast.TokenKind{ast.TokMethod, ast.TokURL, ast.TokIllegal}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toks, d, hasDiag := lexAll([]byte(tc.src))
			if len(toks) != len(tc.want) {
				t.Fatalf("got %d tokens %v, want %v", len(toks), kinds(toks), tc.want)
			}
			for i := range toks {
				if toks[i].Kind != tc.want[i] {
					t.Fatalf("token %d: got %v, want %v (stream: %v)", i, toks[i].Kind, tc.want[i], kinds(toks))
				}
			}
			if tc.want[len(tc.want)-1] == ast.TokIllegal {
				if !hasDiag {
					t.Fatal("illegal token without a diagnostic (fail-closed violated)")
				}
				if d.Kind == ast.ErrNone {
					t.Fatal("diagnostic kind is ErrNone")
				}
			} else if hasDiag {
				t.Fatalf("unexpected diagnostic %v", d)
			}
		})
	}
}

func kinds(toks []ast.Token) []ast.TokenKind {
	out := make([]ast.TokenKind, len(toks))
	for i, t := range toks {
		out[i] = t.Kind
	}
	return out
}

// TestInvariants verifies the lexer's structural invariants over the corpus
// and the behavior cases: monotonic non-overlapping spans, in-bounds offsets,
// sane positions, and the termination bound (tokens <= len(src)+1).
func TestInvariants(t *testing.T) {
	var srcs []struct {
		path string
		src  []byte
	}
	root := filepath.Join("..", "..", "testdata", "http")
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".http") {
			if b, err := os.ReadFile(path); err == nil {
				srcs = append(srcs, struct {
					path string
					src  []byte
				}{path, b})
			}
		}
		return nil
	})
	if len(srcs) == 0 {
		t.Fatal("no corpus files")
	}
	for _, f := range srcs {
		toks, _, _ := lexAll(f.src)
		if len(toks) > len(f.src)+1 {
			t.Fatalf("%s: %d tokens for %d bytes violates the termination bound", f.path, len(toks), len(f.src))
		}
		checkSpans(t, f.path, f.src, toks)
	}
}

func checkSpans(t *testing.T, path string, src []byte, toks []ast.Token) {
	t.Helper()
	var prevEnd int32
	for i, tok := range toks {
		if tok.Kind != ast.TokEOF {
			if tok.End <= tok.Start {
				t.Fatalf("%s token %d: zero-width %v at offset %d", path, i, tok.Kind, tok.Start)
			}
			if tok.Start < 0 || tok.End > int32(len(src)) {
				t.Fatalf("%s token %d: span %d..%d out of bounds (len %d)", path, i, tok.Start, tok.End, len(src))
			}
			if tok.Line < 1 || tok.Col < 1 || tok.EndLine < tok.Line {
				t.Fatalf("%s token %d: insane position %+v", path, i, tok)
			}
			// Grammar tokens tile or gap the input; a TokIllegal is a terminal
			// diagnostic report and may share a span with the construct it
			// condemns (e.g. an unterminated body spans its opening brace).
			if tok.Kind != ast.TokIllegal && tok.Start < prevEnd {
				t.Fatalf("%s token %d: overlap (prev end %d, start %d): %v spans %d..%d after prev %d..%d",
					path, i, prevEnd, tok.Start, tok.Kind, tok.Start, tok.End, toks[i-1].Start, toks[i-1].End)
			}
		}
		if tok.Start > tok.End {
			t.Fatalf("%s token %d: inverted span %d..%d", path, i, tok.Start, tok.End)
		}
		prevEnd = tok.End
	}
}

// TestTokenText verifies allocation-free span access.
func TestTokenText(t *testing.T) {
	src := []byte("GET /x\n")
	toks, _, _ := lexAll(src)
	if toks[0].Kind != ast.TokMethod || string(toks[0].Text(src)) != "GET" {
		t.Fatalf("method text: %q", toks[0].Text(src))
	}
	if toks[1].Kind != ast.TokURL || string(toks[1].Text(src)) != "/x" {
		t.Fatalf("url text: %q", toks[1].Text(src))
	}
}

// TestResetReuse verifies the lexer can be rebound without allocation.
func TestResetReuse(t *testing.T) {
	lx := ast.New([]byte("GET /a\n"))
	for range lx.Tokens() {
	}
	lx.Reset([]byte("POST /b\n\n{}\n"))
	var kinds []ast.TokenKind
	for tok := range lx.Tokens() {
		kinds = append(kinds, tok.Kind)
	}
	if len(kinds) == 0 || kinds[0] != ast.TokMethod {
		t.Fatalf("reset did not rebind: %v", kinds)
	}
	if d, ok := lx.Diag(); ok {
		t.Fatalf("unexpected diagnostic after reset: %v", d)
	}
}
