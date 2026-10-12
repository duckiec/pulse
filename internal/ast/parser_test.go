package ast_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"pulse/internal/ast"
)

var updateAST = flag.Bool("update-ast", false, "regenerate AST golden snapshots")

// renderAST produces the deterministic, human-readable AST snapshot used by the
// golden files. It is the executable spec of the parser's output shape.
func renderAST(doc *ast.Document) string {
	var b strings.Builder
	fmt.Fprintf(&b, "DOC requests=%d file-vars=%d\n", doc.Len(), len(doc.Vars))
	for i, v := range doc.Vars {
		fmt.Fprintf(&b, "  FILEVAR[%d] %s=%q line=%d\n", i,
			doc.Str(v.NameStart, v.NameEnd), doc.Str(v.ValStart, v.ValEnd), v.Line)
	}
	idx := 0
	for req := range doc.Requests() {
		fmt.Fprintf(&b, "REQ[%d] method=%s line=%d name=%q url=%q\n", idx, req.Method, req.Line,
			doc.Str(req.NameStart, req.NameEnd), doc.Str(req.URLStart, req.URLEnd))
		for _, h := range req.Headers {
			fmt.Fprintf(&b, "  HDR %s=%q line=%d\n",
				doc.Str(h.NameStart, h.NameEnd), doc.Str(h.ValStart, h.ValEnd), h.Line)
		}
		for _, v := range doc.VarsOf(req) {
			fmt.Fprintf(&b, "  VAR %s=%q line=%d\n",
				doc.Str(v.NameStart, v.NameEnd), doc.Str(v.ValStart, v.ValEnd), v.Line)
		}
		for _, a := range doc.AssertsOf(req) {
			fmt.Fprintf(&b, "  ASSERT %q line=%d\n", doc.Str(a.Start, a.End), a.Line)
		}
		if req.Body.Kind == ast.BodyNone {
			b.WriteString("  BODY NONE\n")
		} else {
			fmt.Fprintf(&b, "  BODY %s [%d,%d)\n", req.Body.Kind, req.Body.Start, req.Body.End)
			text := string(doc.BodyText(req.Body))
			for _, line := range strings.Split(text, "\n") {
				fmt.Fprintf(&b, "  | %s\n", line)
			}
		}
		idx++
	}
	return b.String()
}

func parseBuf(t *testing.T, src []byte) *ast.Document {
	t.Helper()
	arena := ast.AcquireArena()
	arena.Reset(src)
	doc, err := ast.Parse(arena)
	if err != nil {
		arena.Release()
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

// TestGoldenAST verifies the parser against every valid corpus fixture.
// Regenerate with: go test ./internal/ast -run TestGoldenAST -update-ast
func TestGoldenAST(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "http", "valid")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	goldenDir := filepath.Join("..", "..", "testdata", "http", "golden", "ast")
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".http") {
			continue
		}
		found++
		path := filepath.Join(root, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		doc := parseBuf(t, src)
		got := renderAST(doc)
		golden := filepath.Join(goldenDir, strings.TrimSuffix(e.Name(), ".http")+".ast")

		if *updateAST {
			if err := os.MkdirAll(goldenDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			doc.Release()
			continue
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("read golden %s (regenerate with -update-ast): %v", golden, err)
		}
		if diff := cmp.Diff(string(want), got); diff != "" {
			t.Errorf("%s AST mismatch (-want +got):\n%s", e.Name(), diff)
		}
		doc.Release()
	}
	if found == 0 {
		t.Fatal("no valid corpus files found")
	}
}

// TestParseFailClosed asserts the fail-closed contract on every invalid fixture:
// a non-nil error MUST come with a nil Document (AGENTS.md §4.4).
func TestParseFailClosed(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "http", "invalid")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".http") {
			continue
		}
		found++
		path := filepath.Join(root, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		arena := ast.AcquireArena()
		arena.Reset(src)
		doc, perr := ast.Parse(arena)
		if perr == nil {
			doc.Release()
			t.Fatalf("%s: expected a parse error, got none", e.Name())
		}
		if doc != nil {
			doc.Release()
			t.Fatalf("%s: fail-closed violated — got a non-nil *Document alongside error %v", e.Name(), perr)
		}
		se, ok := perr.(*ast.SyntaxError)
		if !ok {
			t.Fatalf("%s: expected *ast.SyntaxError, got %T", e.Name(), perr)
		}
		if se.Line <= 0 && se.Kind != ast.ErrSyntax {
			t.Fatalf("%s: diagnostic has no location: %+v", e.Name(), se)
		}
		arena.Release()
	}
	if found == 0 {
		t.Fatal("no invalid corpus files found")
	}
}

// TestParseDiagCollects proves ParseDiag accumulates multiple diagnostics with
// resync while Parse still stops at the first.
func TestParseDiagCollects(t *testing.T) {
	// Three independent defects: a variable with no '=' at file scope, a
	// malformed header inside a request, and a second bad variable in the next
	// request block. Note the second '@' line sits directly after a separator —
	// after a blank line it would legitimately be lexed as a raw body.
	src := []byte("@bad-var\n\n###\n\nGET /a\nnot-a-header\n\n###\n\n@also-bad\n\n###\n")
	arena := ast.AcquireArena()
	arena.Reset(src)
	defer arena.Release()

	diags, err := ast.ParseDiag(arena)
	if err == nil {
		t.Fatal("ParseDiag must return a non-nil error when diagnostics exist")
	}
	if len(diags) < 3 {
		t.Fatalf("expected >= 3 diagnostics, got %d: %+v", len(diags), diags)
	}
	// The two '@var' lines have no '=' separator. The missing-separator rule is
	// the boundary this test pins: it must be reported as ErrMissingAssign, not
	// as some downstream symptom of accepting the line.
	var missingAssign int
	for _, d := range diags {
		if d.Kind == ast.ErrMissingAssign {
			missingAssign++
		}
	}
	if missingAssign != 2 {
		t.Errorf("expected 2 ErrMissingAssign diagnostics, got %d: %+v", missingAssign, diags)
	}
	// Diagnostics must be ordered by position and carry locations.
	for i := 1; i < len(diags); i++ {
		if diags[i].Offset < diags[i-1].Offset {
			t.Fatalf("diagnostics out of order: %+v then %+v", diags[i-1], diags[i])
		}
	}
	if diags[len(diags)-1].Offset >= int32(len(src)) {
		t.Fatal("last diagnostic offset out of range")
	}
}

// TestASTStructural is the cmp.Diff structural check: a parsed fixture compared
// field-by-field against an expected literal.
func TestASTStructural(t *testing.T) {
	src := []byte("# @name health\nGET https://api.example.com/health HTTP/1.1\nAccept: application/json\n\n")
	doc := parseBuf(t, src)
	defer doc.Release()

	type headerView struct {
		Name, Value string
	}
	type reqView struct {
		Method, Name, URL string
		Headers           []headerView
		BodyKind          ast.BodyKind
	}
	var got []reqView
	for r := range doc.Requests() {
		rv := reqView{
			Method:   r.Method.String(),
			Name:     doc.Str(r.NameStart, r.NameEnd),
			URL:      doc.Str(r.URLStart, r.URLEnd),
			BodyKind: r.Body.Kind,
		}
		for _, h := range r.Headers {
			rv.Headers = append(rv.Headers, headerView{
				Name:  doc.Str(h.NameStart, h.NameEnd),
				Value: doc.Str(h.ValStart, h.ValEnd),
			})
		}
		got = append(got, rv)
	}
	want := []reqView{{
		Method:   "GET",
		Name:     "health",
		URL:      "https://api.example.com/health",
		BodyKind: ast.BodyNone,
		Headers: []headerView{
			{Name: "Accept", Value: "application/json"},
		},
	}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("structural AST mismatch (-want +got):\n%s", diff)
	}
}

// TestVariableScopes proves the file-scope pre-pass: variables declared before
// the first request land in Document.Vars, request-scoped ones in VarsOf.
func TestVariableScopes(t *testing.T) {
	src := []byte("@file = 1\n\n###\n\n@local = 2\nGET /x\n\n###\n")
	doc := parseBuf(t, src)
	defer doc.Release()

	if len(doc.Vars) != 1 || doc.Str(doc.Vars[0].NameStart, doc.Vars[0].NameEnd) != "file" {
		t.Fatalf("file-scoped vars = %+v", doc.Vars)
	}
	for r := range doc.Requests() {
		vs := doc.VarsOf(r)
		if len(vs) != 1 || doc.Str(vs[0].NameStart, vs[0].NameEnd) != "local" {
			t.Fatalf("request-scoped vars = %+v", vs)
		}
	}
}

// TestAssertsAttached proves `# @assert` directives bind to the following request.
func TestAssertsAttached(t *testing.T) {
	src := []byte("# @assert status == 200\nGET /x\n\n###\n\n# @assert status == 404\nGET /missing\n\n###\n")
	doc := parseBuf(t, src)
	defer doc.Release()
	var texts []string
	for r := range doc.Requests() {
		for _, a := range doc.AssertsOf(r) {
			texts = append(texts, doc.Str(a.Start, a.End))
		}
	}
	if diff := cmp.Diff([]string{"status == 200", "status == 404"}, texts); diff != "" {
		t.Errorf("assert attachment mismatch (-want +got):\n%s", diff)
	}
}

// FuzzParse checks the parser's two standing invariants on arbitrary input:
// it never panics, and its result is fail-closed — a nil Document exactly when
// the error is non-nil (AGENTS.md §4.4). ParseDiag must likewise never panic
// and must report at least one diagnostic whenever it returns an error.
func FuzzParse(f *testing.F) {
	seeds := []string{
		"GET /a\n\n###\n",
		"@v = 1\n\n###\n\nPOST {{v}}/x\nA: b\n\n{}\n\n###\n",
		"# @name n\n# @assert status == 200\nGET /a HTTP/1.1\n\n###\n",
		"###\n\nPOST /a\n\n<r><c/></r>\n\n###\n",
		"@broken\n\n###\n\nGET /a\nbad-header\n\n###\n",
		"", "\n\n\n", "###", "@", "=", "GET", "{", "}}", "###\n###\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		arena := ast.AcquireArena()
		arena.Reset(data)
		doc, err := ast.Parse(arena)
		if err != nil && doc != nil {
			t.Fatalf("fail-closed violated: non-nil Document with error %v", err)
		}
		if doc != nil {
			// A returned Document must be internally consistent: every
			// accessor range must stay inside its backing array.
			checkDocumentBounds(t, doc)
			doc.Release()
		} else {
			arena.Release()
		}

		arena2 := ast.AcquireArena()
		arena2.Reset(data)
		diags, derr := ast.ParseDiag(arena2)
		arena2.Release()
		if derr != nil && len(diags) == 0 {
			t.Fatalf("ParseDiag returned an error with no diagnostics: %v", derr)
		}
		for _, d := range diags {
			if d.Expected == "internal parser invariant" {
				t.Fatalf("invariant violation surfaced as a source diagnostic: %+v", d)
			}
		}
	})
}

// checkDocumentBounds validates every index range a Document hands out. The
// accessors slice the backing arrays directly, so an out-of-range index panics
// here and is reported by the fuzzer as a crash.
func checkDocumentBounds(t *testing.T, doc *ast.Document) {
	t.Helper()
	for _, v := range doc.Vars {
		_ = doc.Str(v.NameStart, v.NameEnd)
		_ = doc.Str(v.ValStart, v.ValEnd)
	}
	n := 0
	for r := range doc.Requests() {
		n++
		if r.VarStart < 0 || r.VarCount < 0 {
			t.Fatalf("request %d: negative var range: %+v", n, r)
		}
		if r.AsrtStart < 0 || r.AsrtCount < 0 {
			t.Fatalf("request %d: negative assert range: %+v", n, r)
		}
		_ = doc.VarsOf(r)
		_ = doc.AssertsOf(r)
		_ = doc.Str(r.URLStart, r.URLEnd)
		for _, h := range r.Headers {
			_ = doc.Str(h.NameStart, h.NameEnd)
			_ = doc.Str(h.ValStart, h.ValEnd)
		}
	}
}

// TestMemoryCeiling is the memory-ceiling gate (ROADMAP.md §1.1.5): parsing a
// 10,000-request fixture must not allocate more than 2x the source size.
func TestMemoryCeiling(t *testing.T) {
	src := genRequests(10_000)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	arena := ast.AcquireArena()
	arena.Reset(src)
	doc, err := ast.Parse(arena)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	runtime.ReadMemStats(&after)

	if doc.Len() < 9000 {
		t.Fatalf("expected ~10k requests, got %d", doc.Len())
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	budget := uint64(2 * len(src))
	if allocated > budget {
		t.Errorf("memory ceiling exceeded: allocated %d bytes for %d-byte source (budget %d = 2x source)",
			allocated, len(src), budget)
	}
	t.Logf("parsed %d requests, source=%d bytes, allocated=%d bytes (%.2fx source)",
		doc.Len(), len(src), allocated, float64(allocated)/float64(len(src)))
	doc.Release()
}
