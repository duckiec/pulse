package engine_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pulse/internal/ast"
	"pulse/internal/engine"
)

// fakeRedactor is the engine's Redactor contract in miniature: it replaces one
// registered value and reports whether a value changed.
type fakeRedactor struct{ secret string }

func (f fakeRedactor) String(s string) string {
	if f.secret == "" {
		return s
	}
	return strings.ReplaceAll(s, f.secret, "[REDACTED]")
}

func (f fakeRedactor) Headers(h http.Header) http.Header {
	out := h.Clone()
	if out == nil {
		return nil
	}
	for k, vals := range out {
		for i, v := range vals {
			out[k][i] = f.String(v)
		}
	}
	return out
}

func (f fakeRedactor) Error(err error) error {
	if err == nil || f.secret == "" {
		return err
	}
	return &redactedError{msg: f.String(err.Error())}
}

type redactedError struct{ msg string }

func (e *redactedError) Error() string { return e.msg }

// TestRecorderWritesParsableRedactedBlocks is the contract `pulse record`
// exists for: traffic in, a collection Pulse itself can run out.
func TestRecorderWritesParsableRedactedBlocks(t *testing.T) {
	const (
		token  = "record-secret-token-2f9c"
		secret = "query-secret-value-77aa"
	)
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("upstream Authorization = %q, want the original bearer", got)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"name":"ada"}` {
			t.Errorf("upstream body = %q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":7}`)
	}))
	defer upstream.Close()

	out := filepath.Join(t.TempDir(), "generated.http")
	rec, err := engine.NewRecorder(engine.RecordConfig{
		Target:   upstream.URL,
		Out:      out,
		Redactor: fakeRedactor{secret: secret},
		Logf:     t.Logf,
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	proxy := httptest.NewServer(rec)
	defer proxy.Close()

	send := func() {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost,
			proxy.URL+"/api/users?debug=1&token="+secret, strings.NewReader(`{"name":"ada"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept-Encoding", "gzip")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy request: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusCreated || string(body) != `{"id":7}` {
			t.Fatalf("proxied response = %d %q, want 201 {\"id\":7}", resp.StatusCode, body)
		}
	}
	send()
	send()

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read generated collection: %v", err)
	}
	text := string(raw)
	for _, leaked := range []string{token, secret} {
		if strings.Contains(text, leaked) {
			t.Errorf("generated file contains the raw secret %q:\n%s", leaked, text)
		}
	}
	for _, want := range []string{
		"# @name record-post-api-users",
		"# @assert status == 201",
		"Authorization: {{AUTH_TOKEN}}",
		"POST " + upstream.URL + "/api/users?debug=1&token=[REDACTED]",
		`{"name":"ada"}`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("generated file missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Accept-Encoding") {
		t.Errorf("generated file captured Accept-Encoding, which the proxy negotiated:\n%s", text)
	}

	// The file must be a valid collection: this is the actual acceptance
	// criterion, not string matching. Both requests share method+path, so the
	// dedupe leaves exactly one block.
	doc, err := ast.ParseFile(out)
	if err != nil {
		t.Fatalf("the generated collection does not parse: %v\n%s", err, text)
	}
	defer doc.Release()
	var requests int
	for req := range doc.Requests() {
		requests++
		if got := doc.Str(req.URLStart, req.URLEnd); !strings.HasPrefix(got, upstream.URL) {
			t.Errorf("request URL = %q, want it to target the upstream, not the recorder", got)
		}
		if assertions := len(doc.AssertsOf(req)); assertions != 1 {
			t.Errorf("request carries %d assertions, want the captured status", assertions)
		}
	}
	if requests != 1 {
		t.Errorf("generated %d requests for one repeated exchange, want 1 (dedupe)", requests)
	}
	if upstreamHits != 2 {
		t.Errorf("upstream saw %d requests, want both forwarded", upstreamHits)
	}
}

// TestRecorderContinuesAnExistingFile proves append semantics: an existing
// collection is neither truncated nor glued onto without a separator, and a
// name already present is not reused.
func TestRecorderContinuesAnExistingFile(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	out := filepath.Join(t.TempDir(), "generated.http")
	if err := os.WriteFile(out, []byte("# @name record-get-health\nGET /health"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := engine.NewRecorder(engine.RecordConfig{Target: upstream.URL, Out: out})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	proxy := httptest.NewServer(rec)
	defer proxy.Close()
	resp, err := http.Get(proxy.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "\n### GET") || !strings.Contains(text, "# @name record-get-health-2") {
		t.Errorf("existing file was not continued with a fresh name and a newline:\n%s", text)
	}
	doc, err := ast.ParseFile(out)
	if err != nil {
		t.Fatalf("continued collection does not parse: %v\n%s", err, text)
	}
	defer doc.Release()
	var n int
	for range doc.Requests() {
		n++
	}
	if n != 2 {
		t.Errorf("collection holds %d requests, want the original plus the capture", n)
	}
}

func TestNewRecorderRejectsBadTarget(t *testing.T) {
	cases := []string{"", "localhost:8080", "ftp://example.com", "http://"}
	for _, target := range cases {
		if _, err := engine.NewRecorder(engine.RecordConfig{
			Target: target, Out: filepath.Join(t.TempDir(), "x.http"),
		}); err == nil {
			t.Errorf("NewRecorder accepted target %q", target)
		}
	}
}
