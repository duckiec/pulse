package ast

import (
	"strings"
	"testing"
)

// TestWasmHookAttachesToTheNextRequest proves `# @wasm` binds like `# @name`:
// it belongs to the request that follows it, and it cannot be confused with the
// `@wasm-sign` variable-form directive that starts with the same characters.
func TestWasmHookAttachesToTheNextRequest(t *testing.T) {
	src := "# @wasm ./hooks/sign.wasm\n" +
		"# @name signed\n" +
		"POST https://api.example.com/v1/items\n" +
		"Authorization: Bearer {{token}}\n" +
		"@wasm-sign ./signer.wasm\n\n" +
		`{"a":1}` + "\n\n###\n" +
		"GET https://api.example.com/v1/items\n"
	doc := parseBuf(t, src)
	defer doc.Release()

	var got []string
	for r := range doc.Requests() {
		got = append(got, doc.WasmHook(r))
	}
	if len(got) != 2 || got[0] != "./hooks/sign.wasm" || got[1] != "" {
		t.Fatalf("hook paths = %q, want [./hooks/sign.wasm \"\"]", got)
	}
	first := doc.reqs[0]
	if sign := doc.WasmSign(&first); sign != "./signer.wasm" {
		t.Errorf("@wasm-sign = %q, want ./signer.wasm (the two directives must not collide)", sign)
	}
}

// TestWasmHookFailsClosed covers the three malformed forms. Each would either
// silently skip the hook or resolve an empty path against the working directory,
// so each is refused at parse time.
func TestWasmHookFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"no path", "# @wasm\nGET https://x.local/a\n", "wasm hook script path"},
		{"blank path", "# @wasm   \nGET https://x.local/a\n", "wasm hook script path"},
		{"two directives", "# @wasm ./a.wasm\n# @wasm ./b.wasm\nGET https://x.local/a\n", "one @wasm per request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			arena := AcquireArena()
			arena.Reset([]byte(c.src))
			_, err := Parse(arena)
			arena.Release()
			if err == nil {
				t.Fatalf("parse accepted %q", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// TestWasmSignIsNotAHook pins the next-byte rule: `@wasm-sign` and `# @wasm`
// share a prefix, and a naive prefix test would read the signing directive as a
// hook pointing at "-sign ./x.wasm".
func TestWasmSignIsNotAHook(t *testing.T) {
	doc := parseBuf(t, "@wasm-sign ./signer.wasm\nGET https://x.local/a\n")
	defer doc.Release()
	for r := range doc.Requests() {
		if h := doc.WasmHook(r); h != "" {
			t.Fatalf("@wasm-sign produced hook path %q", h)
		}
	}
}

// TestWasmHookSurvivesTheCanonicalRoundTrip is the writer half: FormatRequest
// must emit the directive it parsed, or editing a request through the composer
// would silently drop its hook.
func TestWasmHookSurvivesTheCanonicalRoundTrip(t *testing.T) {
	src := "# @wasm ./hooks/presign.wasm\n# @assert status == 201\nPOST https://x.local/items\nContent-Type: application/json\n\n{\"a\":1}\n"
	doc := parseBuf(t, src)
	formatted, err := doc.FormatCollection(nil)
	doc.Release()
	if err != nil {
		t.Fatalf("FormatCollection: %v", err)
	}
	if !strings.Contains(string(formatted), "# @wasm ./hooks/presign.wasm\n") {
		t.Fatalf("formatted document lost the hook directive:\n%s", formatted)
	}
	reparsed := parseBuf(t, string(formatted))
	defer reparsed.Release()
	for r := range reparsed.Requests() {
		if h := reparsed.WasmHook(r); h != "./hooks/presign.wasm" {
			t.Fatalf("reparsed hook = %q", h)
		}
	}
}

// TestRequestTextOwnsItsDirectivesAndBody is the merge driver's copy path: an
// unchanged request must be reproducible byte for byte, including the hook line
// and the body, so a semantic merge never reformats what it did not touch.
func TestRequestTextOwnsItsDirectivesAndBody(t *testing.T) {
	src := "@base = https://x.local\n\n" +
		"# @wasm ./h.wasm\n# @name one\nGET {{base}}/a\nAccept: */*\n\n###\n" +
		"# @name two\nGET {{base}}/b\n"
	doc := parseBuf(t, src)
	defer doc.Release()
	var texts []string
	for r := range doc.Requests() {
		texts = append(texts, string(doc.RequestText(r)))
	}
	if len(texts) != 2 {
		t.Fatalf("got %d request texts", len(texts))
	}
	if want := "# @wasm ./h.wasm\n# @name one\nGET {{base}}/a\nAccept: */*"; texts[0] != want {
		t.Errorf("first request text = %q, want %q", texts[0], want)
	}
	if want := "# @name two\nGET {{base}}/b"; texts[1] != want {
		t.Errorf("second request text = %q, want %q", texts[1], want)
	}
}
