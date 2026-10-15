package ast

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseBuf parses src and fails the test on any error; the caller releases.
func parseBuf(t *testing.T, src string) *Document {
	t.Helper()
	arena := AcquireArena()
	arena.Reset([]byte(src))
	doc, err := Parse(arena)
	if err != nil {
		arena.Release()
		t.Fatalf("parse: %v", err)
	}
	return doc
}

// canonFacts snapshots everything the canonical form must preserve.
type canonFacts struct {
	vars    []string
	names   []string
	methods []string
	urls    []string
	headers [][]string
	asserts [][]string
	wasm    []string
	bodies  []string
}

func factsOf(d *Document) canonFacts {
	var f canonFacts
	for _, v := range d.Vars {
		f.vars = append(f.vars, d.Str(v.NameStart, v.NameEnd)+"="+d.Str(v.ValStart, v.ValEnd))
	}
	for r := range d.Requests() {
		f.names = append(f.names, d.RequestName(r))
		f.methods = append(f.methods, r.Method.String())
		f.urls = append(f.urls, d.Str(r.URLStart, r.URLEnd))
		var hs []string
		for _, h := range r.Headers {
			v := ""
			if h.ValStart >= 0 && h.ValEnd > h.ValStart {
				v = d.Str(h.ValStart, h.ValEnd)
			}
			hs = append(hs, d.Str(h.NameStart, h.NameEnd)+"="+v)
		}
		f.headers = append(f.headers, hs)
		var as []string
		for _, a := range d.AssertsOf(r) {
			as = append(as, d.Str(a.Start, a.End))
		}
		f.asserts = append(f.asserts, as)
		f.wasm = append(f.wasm, d.WasmSign(r))
		if r.Body.Kind != BodyNone {
			f.bodies = append(f.bodies, string(d.BodyText(r.Body)))
		} else {
			f.bodies = append(f.bodies, "")
		}
	}
	return f
}

func equalFacts(a, b canonFacts) (string, bool) {
	eq := func(x, y []string) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	if !eq(a.vars, b.vars) {
		return "vars", false
	}
	if !eq(a.names, b.names) {
		return "names", false
	}
	if !eq(a.methods, b.methods) {
		return "methods", false
	}
	if !eq(a.urls, b.urls) {
		return "urls", false
	}
	if !eq(a.wasm, b.wasm) {
		return "wasm", false
	}
	if !eq(a.bodies, b.bodies) {
		return "bodies", false
	}
	if len(a.headers) != len(b.headers) || len(a.asserts) != len(b.asserts) {
		return "counts", false
	}
	for i := range a.headers {
		if !eq(a.headers[i], b.headers[i]) {
			return "headers", false
		}
		if !eq(a.asserts[i], b.asserts[i]) {
			return "asserts", false
		}
	}
	return "", true
}

// TestFormatRoundTrip parses every valid fixture, formats the collection,
// re-parses, and requires identical canonical facts plus byte-stable
// re-formatting (format is a fixed point).
func TestFormatRoundTrip(t *testing.T) {
	files, err := filepath.Glob("../../testdata/http/valid/*.http")
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		doc := parseBuf(t, string(src))
		before := factsOf(doc)
		out, err := doc.FormatCollection(make([]byte, 0, len(src)+64))
		if err != nil {
			t.Fatalf("%s: format: %v", f, err)
		}
		// Both documents stay alive through the comparison: facts hold
		// unsafe arena views, and Release recycles the arena out from
		// under them (comparing after Release reads recycled memory).
		doc2 := parseBuf(t, string(out))
		if field, ok := equalFacts(before, factsOf(doc2)); !ok {
			t.Errorf("%s: round trip changed %s", f, field)
		}
		out2, err := doc2.FormatCollection(make([]byte, 0, len(out)+64))
		if err != nil {
			t.Fatalf("%s: re-format: %v", f, err)
		}
		if string(out) != string(out2) {
			t.Errorf("%s: format is not a fixed point:\n--- first ---\n%s\n--- second ---\n%s", f, out, out2)
		}
		doc.Release()
		doc2.Release()
	}
}

// TestFormatEmissionOrder pins the canonical order: vars, name, asserts,
// wasm-sign, request line, headers, blank, body.
func TestFormatEmissionOrder(t *testing.T) {
	// The var sits after a separator, making it request-scoped (a var ahead
	// of the first request would be file-scoped and format with the file
	// head instead).
	src := "###\n@v = 1\n# @assert status == 200\n# @name ordered\n@wasm-sign ./s.wasm\nPOST https://x.local/r HTTP/1.1\nContent-Type: application/json\nX-Empty:\n\n{\"a\":1}\n"
	doc := parseBuf(t, src)
	defer doc.Release()
	var first *Request
	for r := range doc.Requests() {
		first = r
		break
	}
	out, err := doc.FormatRequest(nil, first)
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	want := "@v = 1\n# @name ordered\n# @assert status == 200\n@wasm-sign ./s.wasm\nPOST https://x.local/r\nContent-Type: application/json\nX-Empty:\n\n{\"a\":1}"
	if string(out) != want {
		t.Errorf("emission order:\n got: %q\nwant: %q", out, want)
	}
}

// TestFormatUnknownMethodFailsClosed proves the defensive error arm is live:
// a hand-built UNKNOWN method never serializes into an unparseable file.
func TestFormatUnknownMethodFailsClosed(t *testing.T) {
	if _, err := FormatPlainRequest(nil, PlainRequest{Method: "FETCH", URL: "https://x/"}); err == nil {
		t.Error("FETCH accepted, want error")
	}
	doc := parseBuf(t, "GET https://x/\n")
	defer doc.Release()
	for r := range doc.Requests() {
		r.Method = MethodUnknown
		if _, err := doc.FormatRequest(nil, r); err == nil {
			t.Error("MethodUnknown accepted, want error")
		}
	}
}

// TestFormatZeroAlloc pins the hot-path contract: formatting a parsed request
// into a pre-grown buffer allocates nothing (span views append in place).
func TestFormatZeroAlloc(t *testing.T) {
	doc := parseBuf(t, "# @name bench\n# @assert status == 200\nPOST https://x.local/r\nContent-Type: application/json\nAccept: */*\n\n{\"a\":1}\n")
	defer doc.Release()
	var req *Request
	for r := range doc.Requests() {
		req = r
	}
	buf := make([]byte, 0, 4096)
	if _, err := doc.FormatRequest(buf, req); err != nil {
		t.Fatalf("format: %v", err)
	}
	if n := testing.AllocsPerRun(50, func() {
		b := buf[:0]
		if _, err := doc.FormatRequest(b, req); err != nil {
			t.Fatalf("format: %v", err)
		}
	}); n != 0 {
		t.Errorf("FormatRequest allocated %v times, want 0", n)
	}
}

// TestNormalizeURL covers loopback http, remote https, and passthroughs.
func TestNormalizeURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"localhost:8080/health", "http://localhost:8080/health"},
		{"127.0.0.1:9000/x", "http://127.0.0.1:9000/x"},
		{"[::1]:8080/x", "http://[::1]:8080/x"},
		{"localhost", "http://localhost"},
		{"api.example.com/v1/users", "https://api.example.com/v1/users"},
		{"example.com", "https://example.com"},
		{"https://a.b/c", "https://a.b/c"},
		{"http://localhost:8080/x", "http://localhost:8080/x"},
		{"/just/a/path", "/just/a/path"},
		{"{{base}}/users", "{{base}}/users"},
	}
	for _, c := range cases {
		got, err := NormalizeURL(c.in)
		if err != nil {
			t.Errorf("NormalizeURL(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if _, err := NormalizeURL(""); err == nil {
		t.Error("empty URL accepted, want error")
	}
	if _, err := NormalizeURL("https://"); err == nil {
		t.Error("bare scheme accepted, want error")
	}
}

// TestPlainRoundTrip proves synthetic (span-free) requests survive a
// format→parse cycle. The disabled header leads the block as a comment: it
// is preserved in the file, correctly absent from the AST (a `#` line inside
// the header block is a syntax error — comments live at document level).
func TestPlainRoundTrip(t *testing.T) {
	p := PlainRequest{
		Name: "synth", Method: "POST", URL: "https://x.local/r",
		Headers: []PlainHeader{{Name: "A", Value: "b"}, {Name: "Off", Value: "0", Disabled: true}},
		Vars:    []PlainVar{{Name: "v", Value: "1"}},
		Asserts: []string{"status == 201"},
		Body:    "{}",
	}
	out, err := FormatPlainRequest(nil, p)
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	if strings.Contains(string(out), "\nOff:") {
		t.Errorf("disabled header emitted live:\n%s", out)
	}
	if !strings.HasPrefix(string(out), "# Off: 0\n") {
		t.Errorf("disabled header not leading the block:\n%s", out)
	}
	doc := parseBuf(t, string(out)+"\n")
	defer doc.Release()
	if doc.Len() != 1 {
		t.Fatalf("re-parse found %d requests, want 1", doc.Len())
	}
	for r := range doc.Requests() {
		if doc.RequestName(r) != "synth" {
			t.Errorf("name = %q", doc.RequestName(r))
		}
		if len(r.Headers) != 1 {
			t.Errorf("headers = %d, want 1 (disabled stays a comment)", len(r.Headers))
		}
	}
}
