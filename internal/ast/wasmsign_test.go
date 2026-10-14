package ast_test

import (
	"testing"

	"pulse/internal/ast"
)

// TestWasmSignAttachesToTheNextRequest proves the directive binds like @name:
// a bare `@wasm-sign <path>` line before a request attaches to it, and a
// request without one reports "".
func TestWasmSignAttachesToTheNextRequest(t *testing.T) {
	src := []byte("@wasm-sign ./sign/auth.wasm\nGET https://api.example.com/v1/charge\n\n###\n\nGET https://api.example.com/plain\n")
	doc := parseBuf(t, src)
	defer doc.Release()

	var got []string
	for r := range doc.Requests() {
		got = append(got, doc.WasmSign(r))
	}
	if len(got) != 2 || got[0] != "./sign/auth.wasm" || got[1] != "" {
		t.Fatalf("wasm-sign attachment = %q, want [./sign/auth.wasm \"\"]", got)
	}
}

// TestWasmSignInsideTheHeaderBlock proves the directive also works inside the
// header block (after at least one header line), where signing naturally reads
// as part of the request. A `@` line before any header terminates the request
// and is document-level — the grammar's long-standing rule for `@var` lines,
// which this directive follows rather than special-cases.
func TestWasmSignInsideTheHeaderBlock(t *testing.T) {
	src := []byte("GET https://api.example.com/v1/charge\nContent-Type: application/json\n@wasm-sign ./sign/auth.wasm\n")
	doc := parseBuf(t, src)
	defer doc.Release()

	for r := range doc.Requests() {
		if got := doc.WasmSign(r); got != "./sign/auth.wasm" {
			t.Fatalf("wasm-sign = %q, want ./sign/auth.wasm", got)
		}
		return
	}
	t.Fatal("no requests parsed")
}

// TestWasmSignFailClosed proves every malformed shape is refused: a bare
// directive with no path, two directives on one request, and the assignment
// misspelling `@wasm-sign = x` (which must never become a silent variable).
func TestWasmSignFailClosed(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"no path", "@wasm-sign\nGET https://api.example.com/x\n"},
		{"two directives", "@wasm-sign ./a.wasm\n@wasm-sign ./b.wasm\nGET https://api.example.com/x\n"},
		{"assignment misspelling", "@wasm-sign = ./a.wasm\nGET https://api.example.com/x\n"},
		{"two in header block", "GET https://api.example.com/x\nAccept: */*\n@wasm-sign ./a.wasm\n@wasm-sign ./b.wasm\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			arena := ast.AcquireArena()
			defer arena.Release()
			arena.Reset([]byte(tc.src))
			if _, err := ast.Parse(arena); err == nil {
				t.Fatalf("Parse(%q) succeeded, want a fail-closed error", tc.src)
			}
		})
	}
}

// TestWasmSignLookalikesAreVariables proves `wasm-signup = x` and friends
// keep their historic meaning: the directive matches only the exact name
// followed by a blank. (Before the first separator, variables are
// file-scoped — doc.Vars, not VarsOf — per TestVariableScopes.)
func TestWasmSignLookalikesAreVariables(t *testing.T) {
	src := []byte("@wasm-signup = 1\nGET https://api.example.com/x\n")
	doc := parseBuf(t, src)
	defer doc.Release()
	if len(doc.Vars) != 1 || doc.Str(doc.Vars[0].NameStart, doc.Vars[0].NameEnd) != "wasm-signup" {
		t.Fatalf("lookalike variable lost: vars = %+v", doc.Vars)
	}
	for r := range doc.Requests() {
		if got := doc.WasmSign(r); got != "" {
			t.Fatalf("wasm-sign = %q, want empty (lookalike must stay a variable)", got)
		}
		return
	}
	t.Fatal("no requests parsed")
}

// TestMisplacedVariableStillFails proves the header-block exception is
// exactly one directive wide: `@x = 1` after a header line is still a syntax
// error, not a silently scoped variable. (A `@` line before any header ends
// the request and parses as document-level — the pre-existing rule.)
func TestMisplacedVariableStillFails(t *testing.T) {
	arena := ast.AcquireArena()
	defer arena.Release()
	arena.Reset([]byte("GET https://api.example.com/x\nAccept: */*\n@oops = 1\n"))
	if _, err := ast.Parse(arena); err == nil {
		t.Fatal("a variable inside the header block parsed; want ErrSyntax")
	}
}
