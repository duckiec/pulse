package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pulse/internal/secrets"
)

// stubRetriever returns a fixed corpus and records what it was asked.
type stubRetriever struct {
	hits     []ContextChunk
	gotQuery string
	gotLimit int
	calls    int
	err      error
	degraded bool
	name     string
}

func (s *stubRetriever) Retrieve(_ context.Context, query string, limit int) ([]ContextChunk, error) {
	s.calls++
	s.gotQuery = query
	s.gotLimit = limit
	if s.err != nil {
		return nil, s.err
	}
	return s.hits, nil
}

func (s *stubRetriever) EmbedderName() string {
	if s.name == "" {
		return "structural-hash-v1"
	}
	return s.name
}

func (s *stubRetriever) Degraded() bool { return s.degraded }

func chunk(path, symbol, text string) ContextChunk {
	return ContextChunk{Path: path, Symbol: symbol, Text: text, Score: 0.5, Sources: []string{"fts"}}
}

// TestFiveHundredBindingIsNotSuppressedOnSuccess is THE mutation gate.
//
// It asserts BOTH directions of the 5xx predicate at once, because both are
// failure modes worth pinning and they fail differently:
//
//	5xx must bind   a 500 with a usable body gets backend context attached
//	2xx must NOT    a 200 gets none; source code attached to a success is noise
//	                that costs context window and teaches the agent to ignore
//	                the attachment
func TestFiveHundredBindingIsNotSuppressedOnSuccess(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		wantBind bool
	}{
		{"500 internal server error", 500, true},
		{"501 not implemented", 501, true},
		{"503 service unavailable", 503, true},
		{"599 last of the 5xx range", 599, true},
		{"200 ok", 200, false},
		{"201 created", 201, false},
		{"301 moved", 301, false},
		{"404 not found", 404, false},
		{"418 teapot", 418, false},
		// Above the range: a synthetic or proxied status is not a server
		// error this binder can explain, and binding to it would be a
		// confident wrong answer.
		{"600 beyond the range", 600, false},
		{"0 no response at all", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := &stubRetriever{hits: []ContextChunk{
				chunk("internal/handler.go", "Handler", "func Handler() {}"),
			}}
			b := &FailureBinder{Retrieve: rt}
			out := &Outcome{
				Status: tc.status,
				Body:   `{"error":{"message":"undefined method ListUsers"}}`,
			}
			b.ExplainFailure(context.Background(), out, "users")

			got := out.Context != nil && len(out.Context.Chunks) > 0
			if got != tc.wantBind {
				t.Errorf("status %d: bound = %v, want %v (backend context %s)",
					tc.status, got, tc.wantBind,
					map[bool]string{true: "attached", false: "absent"}[got])
			}
			if !tc.wantBind && rt.calls != 0 {
				t.Errorf("status %d queried the index %d times; the retriever must not be "+
					"touched for a non-5xx", tc.status, rt.calls)
			}
		})
	}
}

// TestBindingFailsClosedOnEmptyErrorBody proves a 5xx with nothing to search
// for returns no binding rather than searching for the status code.
func TestBindingFailsClosedOnEmptyErrorBody(t *testing.T) {
	rt := &stubRetriever{hits: []ContextChunk{chunk("a.go", "F", "func F() {}")}}
	b := &FailureBinder{Retrieve: rt}
	for _, body := range []string{"", "   ", "\n\n"} {
		out := &Outcome{Status: 502, Body: body}
		bind, err := b.Bind(context.Background(), out, "")
		if err != nil {
			t.Fatalf("body %q: unexpected error: %v", body, err)
		}
		if bind != nil {
			t.Errorf("body %q produced a binding; an empty error body has nothing to search for", body)
		}
	}
	if rt.calls != 0 {
		t.Errorf("the index was queried %d times for empty bodies", rt.calls)
	}
}

// TestErrorTextExtraction covers the sequence of attempts, since real error
// bodies are heterogeneous and the binder has to cope with all of them.
func TestErrorTextExtraction(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		want  []string // substrings that must appear in the extracted text
		avoid []string
	}{
		{
			name: "JSON error object with message",
			body: `{"error":{"message":"column user_id does not exist"}}`,
			want: []string{"column user_id does not exist"},
		},
		{
			name: "JSON error as a plain string",
			body: `{"error":"upstream refused connection"}`,
			want: []string{"upstream refused connection"},
		},
		{
			name: "top-level message field",
			body: `{"message":"bad request","status":400}`,
			want: []string{"bad request"},
		},
		{
			name: "detail field",
			body: `{"detail":"model User missing"}`,
			want: []string{"model User missing"},
		},
		{
			name: "Rails-style exception envelope",
			body: `{"exception":"ActiveRecord::RecordNotFound","message":"Couldn't find User"}`,
			want: []string{"Couldn't find User"},
		},
		{
			name: "stack trace is carried, since that names the file",
			body: `{"error":{"message":"boom","stack":"at app/models/user.rb:42:in ` + "`find`" + `"}}`,
			want: []string{"boom", "user.rb"},
		},
		{
			name: "python traceback",
			body: "Traceback (most recent call last):\n" +
				`  File "/app/views.py", line 42, in show` + "\n" +
				`    render(req)` + "\n" +
				`AttributeError: 'NoneType' object has no attribute 'render'`,
			want: []string{"AttributeError", "views.py"},
		},
		{
			name: "plain text body",
			body: "Internal Server Error: the widget exploded",
			want: []string{"widget exploded"},
		},
		{
			name: "html error page falls back to its first line",
			body: "<html><body><h1>502 Bad Gateway</h1></body></html>",
			want: []string{"502 Bad Gateway"},
		},
		{
			name: "not json at all, no quote confusion",
			body: `it's broken, don't try to parse this`,
			want: []string{"broken"},
		},
	}
	b := &FailureBinder{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := b.extractErrorText(tc.body, "")
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("extracted %q, want it to contain %q", got, want)
				}
			}
			for _, avoid := range tc.avoid {
				if strings.Contains(got, avoid) {
					t.Errorf("extracted %q, should not contain %q", got, avoid)
				}
			}
		})
	}
}

// TestExtractionIsBounded proves a huge or adversarial body cannot turn an
// explanation request into an unbounded allocation.
func TestExtractionIsBounded(t *testing.T) {
	b := &FailureBinder{}
	huge := strings.Repeat("A", 4<<20) // 4 MiB of one character
	got := b.extractErrorText(huge, "")
	if len(got) > 1024 {
		t.Errorf("extracted %d bytes from a 4 MiB body; the extract must be bounded", len(got))
	}

	// A deep traceback is bounded by line count too.
	deep := "Traceback (most recent call last):\n" + strings.Repeat("  File \"x.py\", line 1, in f\n", 5000)
	got = b.extractErrorText(deep, "")
	if len(got) > 64<<10 {
		t.Errorf("extracted %d bytes from a 5,000-frame traceback; line count must be bounded", len(got))
	}
}

// TestBindingRedactsErrorText proves a token echoed by a failing server is
// scrubbed before it becomes a SEARCH QUERY. The query reaches an embedding and
// then a store; a secret in either is a secret on disk and in a model context.
func TestBindingRedactsErrorText(t *testing.T) {
	const secret = "super-secret-token-value"
	red := secrets.NewRedactor()
	red.Register(secret)

	rt := &stubRetriever{hits: []ContextChunk{chunk("a.go", "F", "func F() {}")}}
	b := &FailureBinder{Retrieve: rt, Redact: red}
	out := &Outcome{
		Status: 500,
		Body:   `{"error":{"message":"auth failed for token ` + secret + `"}}`,
	}
	b.ExplainFailure(context.Background(), out, "users")

	if out.Context == nil {
		t.Fatal("no binding was produced")
	}
	if strings.Contains(out.Context.Query, secret) {
		t.Errorf("the search query still contains the secret: %q", out.Context.Query)
	}
	if !strings.Contains(out.Context.Query, secrets.Placeholder) {
		t.Errorf("the query does not carry the redaction placeholder: %q", out.Context.Query)
	}
}

// TestBindingRedactsRetrievedSource proves the retrieved CHUNK text is
// scrubbed too. Source is read from the user's machine and can contain a
// hardcoded token in a fixture; attaching it raw puts that in a model's
// context and, from there, in whatever the agent writes.
func TestBindingRedactsRetrievedSource(t *testing.T) {
	const secret = "hardcoded-password-in-source"
	red := secrets.NewRedactor()
	red.Register(secret)

	rt := &stubRetriever{hits: []ContextChunk{
		chunk("internal/config.go", "loadConfig", `password := "`+secret+`"`),
	}}
	b := &FailureBinder{Retrieve: rt, Redact: red}
	out := &Outcome{Status: 500, Body: `{"error":{"message":"config load failed"}}`}
	b.ExplainFailure(context.Background(), out, "")

	if out.Context == nil || len(out.Context.Chunks) == 0 {
		t.Fatal("no binding was produced")
	}
	if strings.Contains(out.Context.Chunks[0].Text, secret) {
		t.Errorf("the retrieved source still contains the secret: %q", out.Context.Chunks[0].Text)
	}
}

// TestRetrievalFailureIsDegradedNotFatal proves a broken index does not turn a
// 500 into a transport failure. The agent asked to run a request and got a 500;
// that is the answer. Losing the context attachment is a degradation and is
// reported in Warnings, where the agent will see it.
func TestRetrievalFailureIsDegradedNotFatal(t *testing.T) {
	rt := &stubRetriever{err: errors.New("database is locked")}
	b := &FailureBinder{Retrieve: rt}
	out := &Outcome{Status: 500, Body: `{"error":{"message":"boom"}}`}

	b.ExplainFailure(context.Background(), out, "")

	if out.Context != nil {
		t.Error("a failed retrieval produced a binding")
	}
	var warned bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "backend context unavailable") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("the degradation was not reported; warnings = %v", out.Warnings)
	}
	if out.Status != 500 {
		t.Errorf("status = %d; the run's own result must be untouched by a retrieval failure", out.Status)
	}
}

// TestDegradedRetrievalIsLabelled proves a lexical-only result says so.
func TestDegradedRetrievalIsLabelled(t *testing.T) {
	rt := &stubRetriever{
		hits:     []ContextChunk{chunk("a.go", "F", "func F() {}")},
		degraded: true,
		name:     "lexical-only",
	}
	b := &FailureBinder{Retrieve: rt}
	out := &Outcome{Status: 500, Body: `{"error":{"message":"boom"}}`}
	b.ExplainFailure(context.Background(), out, "")

	if out.Context == nil || !out.Context.Degraded {
		t.Fatal("a degraded retrieval was not labelled")
	}
	var mentioned bool
	for _, w := range out.Warnings {
		if strings.Contains(w, "DEGRADED") {
			mentioned = true
		}
	}
	if !mentioned {
		t.Errorf("DEGRADED does not appear in warnings %v", out.Warnings)
	}
}

// TestBindingIsBounded proves the attachment cannot flood the agent's context
// window, and that dropping chunks is stated rather than silent.
func TestBindingIsBounded(t *testing.T) {
	rt := &stubRetriever{hits: []ContextChunk{
		chunk("a.go", "A", strings.Repeat("x", 4096)),
		chunk("b.go", "B", strings.Repeat("y", 4096)),
		chunk("c.go", "C", strings.Repeat("z", 4096)),
		chunk("d.go", "D", strings.Repeat("w", 4096)),
		chunk("e.go", "E", strings.Repeat("v", 4096)),
	}}
	b := &FailureBinder{Retrieve: rt}
	out := &Outcome{Status: 500, Body: `{"error":{"message":"boom"}}`}
	b.ExplainFailure(context.Background(), out, "")

	if out.Context == nil {
		t.Fatal("no binding")
	}
	if len(out.Context.Chunks) > MaxContextChunks {
		t.Errorf("%d chunks attached, over the %d ceiling", len(out.Context.Chunks), MaxContextChunks)
	}
	total := 0
	for _, c := range out.Context.Chunks {
		total += len(c.Text)
	}
	if total > MaxContextBytes {
		t.Errorf("%d bytes of source attached, over the %d ceiling", total, MaxContextBytes)
	}
	if rt.gotLimit != MaxContextChunks {
		t.Errorf("asked the index for %d results, want %d; over-fetching wastes the scan",
			rt.gotLimit, MaxContextChunks)
	}
}

// TestNilBinderIsSilent proves a server with no index omits the attachment
// entirely rather than emitting an empty one.
func TestNilBinderIsSilent(t *testing.T) {
	var b *FailureBinder
	out := &Outcome{Status: 500, Body: `{"error":{"message":"boom"}}`}
	b.ExplainFailure(context.Background(), out, "") // must not panic
	if out.Context != nil {
		t.Error("a nil binder produced a binding")
	}

	// A binder with no retriever is the same case.
	b2 := &FailureBinder{}
	b2.ExplainFailure(context.Background(), out, "")
	if out.Context != nil {
		t.Error("a binder with no retriever produced a binding")
	}
}

// TestSymbolHintFromURL proves the URL contributes a useful term and that a
// numeric id is not mistaken for one.
func TestSymbolHintFromURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"http://localhost:8080/api/users/42", "users"},
		{"https://api.example.com/v2/orders/99/items", "items"},
		{"http://x/refundOrder", "refundOrder"},
		{"http://x/42", ""},
		{"http://x/", ""},
		{"not a url at all", "not a url at all"},
	}
	for _, tc := range tests {
		if got := SymbolHintFromURL(tc.in); got != tc.want {
			t.Errorf("SymbolHintFromURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestStoreRetrieverMapsEveryField proves the adapter copies every field. It
// exists because internal/mcp cannot import internal/store, so the two hit
// shapes are written twice; a dropped field is a silently-empty column in an
// agent's explanation.
func TestStoreRetrieverMapsEveryField(t *testing.T) {
	src := RagHit{
		Path: "internal/a.go", StartLn: 10, EndLn: 20,
		Symbol: "Handler", Lang: "go", Text: "func Handler() {}",
		Score: 0.75, Sources: []string{"fts", "vec"},
	}
	r := &StoreRetriever{
		EmbedderName_: "structural-hash-v1",
		Available:     false, // force the lexical-only path so SearchFn is the only input
		SearchFn: func(_ context.Context, _ string, _ []int8, limit int) ([]RagHit, error) {
			if limit != MaxContextChunks {
				t.Errorf("SearchFn got limit %d, want %d", limit, MaxContextChunks)
			}
			return []RagHit{src}, nil
		},
	}
	got, err := r.Retrieve(context.Background(), "boom", MaxContextChunks)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(got))
	}
	c := got[0]
	if c.Path != src.Path || c.StartLn != src.StartLn || c.EndLn != src.EndLn ||
		c.Symbol != src.Symbol || c.Lang != src.Lang || c.Text != src.Text ||
		c.Score != src.Score || len(c.Sources) != 2 {
		t.Errorf("a field was dropped in the mapping:\n got %+v\nwant %+v", c, src)
	}
	if r.EmbedderName() != "structural-hash-v1" {
		t.Errorf("EmbedderName = %q", r.EmbedderName())
	}
	if !r.Degraded() {
		t.Error("a retriever with Available=false must report itself degraded")
	}
}

// TestStoreRetrieverFallsBackToLexical proves an embedding failure degrades the
// query rather than failing it. A worse answer beats no answer.
func TestStoreRetrieverFallsBackToLexical(t *testing.T) {
	var sawNilVector bool
	r := &StoreRetriever{
		Available: true,
		EmbedFn: func(context.Context, string) ([]int8, float32, error) {
			return nil, 0, errors.New("model gone")
		},
		SearchFn: func(_ context.Context, _ string, vec []int8, _ int) ([]RagHit, error) {
			sawNilVector = vec == nil
			return []RagHit{{Path: "a.go", Symbol: "F", Text: "func F() {}"}}, nil
		},
	}
	got, err := r.Retrieve(context.Background(), "boom", 3)
	if err != nil {
		t.Fatalf("an embedding failure must not fail the retrieval: %v", err)
	}
	if !sawNilVector {
		t.Error("the search ran without a vector; the fallback did not take effect")
	}
	if len(got) != 1 {
		t.Errorf("got %d chunks, want 1", len(got))
	}
	if !r.Degraded() {
		t.Error("after an embedding failure the retriever must report degraded")
	}
}

// TestRenderBindingProducesReadableOutput proves the attachment is formatted
// for a model's context window and a human transcript, with path:line.
func TestRenderBindingProducesReadableOutput(t *testing.T) {
	out := &Outcome{
		Status: 500,
		Body:   `{"error":{"message":"undefined method ListUsers"}}`,
		Context: &Binding{
			Status:    500,
			ErrorText: "undefined method ListUsers",
			Chunks: []ContextChunk{{
				Path: "internal/user/list.go", StartLn: 8, EndLn: 14,
				Symbol: "ListUsers", Text: "func ListUsers() {}\n\treturn nil",
				Sources: []string{"fts", "vec"},
			}},
		},
	}
	got := renderOutcome(out, &ToolInput{})
	for _, want := range []string{
		"failed with 500",
		"internal/user/list.go:8-14",
		"ListUsers",
		"matched by fts+vec",
		"func ListUsers() {}",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered output is missing %q:\n%s", want, got)
		}
	}
}

// TestSymbolHintIsAddedToTheQuery proves a weak body still gets a search term.
func TestSymbolHintIsAddedToTheQuery(t *testing.T) {
	rt := &stubRetriever{hits: []ContextChunk{chunk("a.go", "F", "func F() {}")}}
	b := &FailureBinder{Retrieve: rt}
	out := &Outcome{Status: 500, Body: "500 Internal Server Error"}
	b.ExplainFailure(context.Background(), out, "refundOrder")

	if !strings.Contains(rt.gotQuery, "refundOrder") {
		t.Errorf("query %q does not carry the symbol hint", rt.gotQuery)
	}
}
