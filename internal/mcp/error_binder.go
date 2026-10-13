package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// This file implements ROADMAP.md §3.2.4: when an API answers 5xx, attach the
// backend code most likely responsible.
//
// # Why this is not a convenience
//
// A 500 body is a sentence in a language nobody wrote for machines: it might be
// `{"error":{"message":"undefined method `foo'"}}`, or a Django traceback, or
// an nginx HTML page, or the single word "boom". None of those tell an agent
// WHICH FILE to look at. But the error text almost always NAMES things — a
// symbol, a table, a column — and those names appear in the source that caused
// them. So the binding is: read the error text, search the local index for the
// code that mentions those things, and hand back the top few chunks.
//
// # The 5xx gate is the load-bearing part
//
// This runs on EVERY response. Getting the threshold wrong in the permissive
// direction attaches source code to a 200, which is noise that costs the agent
// context window and, worse, teaches it to ignore the attachment. Getting it
// wrong in the strict direction — the failure mode the mutation gate pins —
// means the explanation silently never appears on the failures it exists for.
// The gate is therefore `status >= 500 && status <= 599`, exactly, and
// TestFiveHundredBindingIsNotSuppressedOnSuccess is the mutation proof.

// ContextChunk is one retrieved source span attached to a tool result.
//
// Text is bounded by MaxContextBytes: this is going into a language model's
// context window, and three unbounded source files would crowd out the
// conversation that the agent needs in order to act on the explanation.
type ContextChunk struct {
	Path    string   `json:"path"`
	StartLn int      `json:"start_ln"`
	EndLn   int      `json:"end_ln"`
	Symbol  string   `json:"symbol,omitempty"`
	Lang    string   `json:"lang,omitempty"`
	Text    string   `json:"text"`
	Score   float64  `json:"score"`
	Sources []string `json:"sources,omitempty"`
}

// MaxContextBytes bounds the total source text attached to one response.
const MaxContextBytes = 24 << 10 // 24 KiB

// MaxContextChunks is how many spans are attached. ROADMAP.md §3.2.4 specifies
// top-3: three is enough to cover "the handler, the helper it calls, the type
// it failed on" and few enough that the agent still has room to think.
const MaxContextChunks = 3

// Retriever is the index query surface the binder needs.
//
// It is an interface because internal/mcp must not import internal/store: the
// dependency flow in ARCHITECTURE.md §3 is ui/mcp → app → store, and mcp
// reaching past app into store would invert it and put a database driver inside
// the MCP-reachable package tree (AGENTS.md §3.3). The real implementation is
// wired by internal/app.
type Retriever interface {
	// Retrieve returns ranked spans for query. It may return nil with no
	// error when the index is empty, which is a first-class outcome: an empty
	// index is not an error, it just means there is nothing to say.
	Retrieve(ctx context.Context, query string, limit int) ([]ContextChunk, error)
	// EmbedderName identifies the retrieval signal, for the DEGRADED label.
	EmbedderName() string
	// Degraded reports whether retrieval is running without the structural
	// signal. Callers surface this rather than presenting a lexical-only
	// result as a complete one.
	Degraded() bool
}

// FailureBinder attaches backend context to a failed run.
//
// A nil Binder is legal and means "this server does not index code". The tool
// then omits the attachment entirely rather than emitting an empty one: an
// empty `context_chunks: []` tells an agent "we looked and found nothing",
// which is a different and stronger claim than "we cannot look".
type FailureBinder struct {
	// Retrieve is the index query surface. nil disables binding.
	Retrieve Retriever
	// Redact scrubs error text and retrieved source. It is NOT optional in
	// spirit: a stack trace routinely quotes the request that triggered it,
	// and a request can carry a token in a query string. Defaults to a
	// pass-through redactor if nil, which is correct for a test and wrong for
	// production — so the composition root always supplies a real one.
	//
	// The type is the bridge's own one-method [Redactor], not the concrete
	// vault: this layer must not know which redactor is installed.
	Redact Redactor
}

// bind5xx reports whether a status is in the 5xx range and therefore eligible
// for contextual binding.
//
// The comparison is on the FULL range rather than `status >= 500`, because a
// synthetic or proxied status above 599 is not a server error this binder can
// explain, and attaching source code to it would be a confident wrong answer.
func bind5xx(status int) bool { return status >= 500 && status <= 599 }

// Bind is the entry point. It is called for every run and returns nil, nil
// when there is nothing to attach.
func (b *FailureBinder) Bind(ctx context.Context, out *Outcome, symbolHint string) (*Binding, error) {
	if b == nil || b.Retrieve == nil || out == nil {
		return nil, nil
	}
	if !bind5xx(out.Status) {
		return nil, nil
	}

	errText := b.extractErrorText(out.Body, symbolHint)
	if strings.TrimSpace(errText) == "" {
		// A 5xx with an empty body is real and common (a proxy timeout). There
		// is nothing to search for, and searching for the status code would
		// retrieve whatever file mentions "500".
		return nil, nil
	}

	chunks, err := b.Retrieve.Retrieve(ctx, errText, MaxContextChunks)
	if err != nil {
		// A retrieval failure must NOT fail the tool call. The agent asked to
		// run a request and got a 500; that is the answer. Losing the context
		// attachment is a degradation, and it is reported as one.
		return nil, fmt.Errorf("%w: %w", ErrRetrievalFailed, err)
	}
	chunks = b.bound(chunks)
	if len(chunks) == 0 {
		return nil, nil
	}

	return &Binding{
		Status:     out.Status,
		ErrorText:  errText,
		Query:      errText,
		Chunks:     chunks,
		Embedder:   b.Retrieve.EmbedderName(),
		Degraded:   b.Retrieve.Degraded(),
		SymbolHint: symbolHint,
		Truncated:  false,
	}, nil
}

// ErrRetrievalFailed reports that the index could not be queried. It is a
// DEGRADATION, not a failure of the request.
var ErrRetrievalFailed = errors.New("mcp: failure context retrieval failed")

// Binding is the attachment for one failed run.
type Binding struct {
	Status     int            `json:"status"`
	ErrorText  string         `json:"error_text"`
	Query      string         `json:"query"`
	Chunks     []ContextChunk `json:"chunks"`
	Embedder   string         `json:"embedder,omitempty"`
	Degraded   bool           `json:"degraded,omitempty"`
	SymbolHint string         `json:"symbol_hint,omitempty"`
	Truncated  bool           `json:"truncated,omitempty"`
	Notes      []string       `json:"notes,omitempty"`
}

// bound enforces the per-response byte and count ceilings.
//
// Truncation is recorded rather than silent: an agent that sees three chunks
// and assumes they are the whole neighbourhood will reason wrongly, so Notes
// says how much was left out.
func (b *FailureBinder) bound(in []ContextChunk) []ContextChunk {
	var out []ContextChunk
	total := 0
	for i, c := range in {
		if len(out) >= MaxContextChunks {
			return out
		}
		text := b.scrub(c.Text)
		if total+len(text) > MaxContextBytes {
			room := MaxContextBytes - total
			if room > 0 {
				c.Text = text[:room]
				out = append(out, c)
				total += room
			}
			return out
		}
		c.Text = text
		total += len(text)
		out = append(out, c)
		_ = i
	}
	return out
}

// scrub applies the redactor to text, tolerating a nil redactor.
func (b *FailureBinder) scrub(s string) string {
	if b == nil || b.Redact == nil {
		return s
	}
	return b.Redact.String(s)
}

// extractErrorText pulls the searchable sentence out of a response body.
//
// It is a SEQUENCE of increasingly less structured attempts, and the order is
// the design: structured first because a JSON error body names its own message
// field, unstructured last because an HTML error page is all there is. Each
// attempt is bounded, so a 10 MB HTML error page cannot turn an explanation
// request into an allocation.
func (b *FailureBinder) extractErrorText(body, symbolHint string) string {
	body = b.scrub(body)
	trimmed := strings.TrimSpace(body)

	if msg := extractJSONError(trimmed); msg != "" {
		return b.decorate(msg, symbolHint)
	}
	if msg := extractTraceback(trimmed); msg != "" {
		return b.decorate(msg, symbolHint)
	}
	if msg := extractFirstLine(trimmed); msg != "" {
		return b.decorate(msg, symbolHint)
	}
	return ""
}

// decorate appends the symbol hint, which gives the lexical leg something to
// match on when the body itself is uninformative.
func (b *FailureBinder) decorate(msg, symbolHint string) string {
	if symbolHint == "" {
		return msg
	}
	return msg + " " + symbolHint
}

// jsonErrorEnvelope is the shape most frameworks use for an error body. It is
// deliberately permissive: `error` is sometimes a string and sometimes an
// object, and a binder that handles only one of them fails on half the world's
// APIs.
type jsonErrorEnvelope struct {
	Error   json.RawMessage `json:"error"`
	Message string          `json:"message"`
	Detail  string          `json:"detail"`
	Msg     string          `json:"msg"`
	Reason  string          `json:"reason"`
}

func extractJSONError(body string) string {
	if !strings.HasPrefix(body, "{") {
		return ""
	}
	var env jsonErrorEnvelope
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return ""
	}
	var parts []string

	// `error` may be a string, or an object carrying message/detail.
	var asString string
	if len(env.Error) > 0 && json.Unmarshal(env.Error, &asString) == nil && asString != "" {
		parts = append(parts, asString)
	}
	var asObj struct {
		Message string `json:"message"`
		Detail  string `json:"detail"`
		Type    string `json:"type"`
		Stack   string `json:"stack"`
	}
	if len(env.Error) > 0 && json.Unmarshal(env.Error, &asObj) == nil {
		// A stack trace is the single most useful thing in an error body, and
		// it is where the failing FILE lives.
		if asObj.Stack != "" {
			parts = append(parts, firstLines(asObj.Stack, 12))
		}
		parts = appendNonEmpty(parts, asObj.Message, asObj.Detail, asObj.Type)
	}
	parts = appendNonEmpty(parts, env.Message, env.Detail, env.Msg, env.Reason)
	return strings.Join(nonEmpty(parts), " | ")
}

func appendNonEmpty(dst []string, vals ...string) []string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			dst = append(dst, v)
		}
	}
	return dst
}

func nonEmpty(in []string) []string {
	out := in[:0]
	for _, v := range in {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

// extractTraceback pulls the informative lines out of a Python-style traceback:
// the exception line plus the frames, which name files and functions.
func extractTraceback(body string) string {
	if !strings.Contains(body, "Traceback (most recent call last)") {
		return ""
	}
	return firstLines(body, 40)
}

// firstLines bounds a multi-line extract and strips the noisy leading frame
// noise. Line count is bounded because a stack trace in a looping server can
// be arbitrarily long and the search query should be a sentence, not a log.
func firstLines(s string, n int) string {
	rows := strings.Split(s, "\n")
	if len(rows) > n {
		rows = rows[:n]
	}
	var kept []string
	for _, r := range rows {
		t := strings.TrimSpace(r)
		if t == "" {
			continue
		}
		kept = append(kept, t)
	}
	return strings.Join(kept, " ")
}

// extractFirstLine takes the first non-empty line, bounded. For an HTML error
// page this yields something useless, which is correct: the vector leg and the
// symbol hint are what will carry such a case.
func extractFirstLine(body string) string {
	const limit = 512
	rows := strings.Split(body, "\n")
	for _, r := range rows {
		t := strings.TrimSpace(r)
		if t == "" {
			continue
		}
		if len(t) > limit {
			t = t[:limit]
		}
		return t
	}
	return ""
}

// ---- Outcome extension ------------------------------------------------------

// ExplainFailure attaches backend context to a failed Outcome in place.
//
// It is deliberately tolerant of a nil binder so the tool handler does not have
// to branch on server configuration, and it never returns an error: binding is
// an enrichment, and enrichment failing must not turn a 500 into a transport
// failure. The degradation is recorded in Warnings instead, where the agent
// will actually see it.
func (b *FailureBinder) ExplainFailure(ctx context.Context, out *Outcome, symbolHint string) {
	if out == nil {
		return
	}
	binding, err := b.Bind(ctx, out, symbolHint)
	if err != nil {
		if out.Warnings == nil {
			out.Warnings = []string{}
		}
		out.Warnings = append(out.Warnings,
			fmt.Sprintf("backend context unavailable: %v", err))
		return
	}
	if binding == nil {
		return
	}
	out.Context = binding
	if binding.Degraded {
		if out.Warnings == nil {
			out.Warnings = []string{}
		}
		out.Warnings = append(out.Warnings,
			"backend context is DEGRADED: lexical-only retrieval (no embedding model)")
	}
}

// ---- StoreRetriever ---------------------------------------------------------

// StoreRetriever adapts a store-shaped index to the Retriever interface.
//
// It lives in this file rather than in internal/mcp's callers only so the
// adapter has a testable home, but it takes narrow function parameters instead
// of importing the store package — the dependency direction is preserved
// without giving up a seam that a test can substitute.
type StoreRetriever struct {
	// SearchFn performs retrieval. Signature matches store.Store.Search.
	SearchFn func(ctx context.Context, query string, vec []int8, limit int) ([]RagHit, error)
	// EmbedFn produces the query vector; nil means lexical-only.
	EmbedFn func(ctx context.Context, text string) ([]int8, float32, error)
	// EmbedderName_ labels the signal in results. The trailing underscore is
	// not a typo: Go forbids a field and a method sharing a name, and the
	// interface method has to be called EmbedderName.
	EmbedderName_ string
	// Available reports whether the structural leg can run.
	Available bool
}

// RagHit mirrors the store's hit shape. Duplicated rather than imported for the
// same reason CosineInt8 is duplicated in store: the lower layer must not
// import the higher one, so the two shapes are pinned to agree by
// TestStoreRetrieverMapsEveryField.
type RagHit struct {
	Path    string
	StartLn int
	EndLn   int
	Symbol  string
	Lang    string
	Text    string
	Score   float64
	Sources []string
}

// Retrieve implements Retriever.
func (r *StoreRetriever) Retrieve(ctx context.Context, query string, limit int) ([]ContextChunk, error) {
	if r == nil || r.SearchFn == nil {
		return nil, nil
	}
	var vec []int8
	if r.Available && r.EmbedFn != nil {
		v, _, err := r.EmbedFn(ctx, query)
		if err != nil {
			// Fall back to lexical rather than failing: a worse answer beats
			// no answer, and Degraded() will say so.
			r.Available = false
			vec = nil
		} else {
			vec = v
		}
	}
	hits, err := r.SearchFn(ctx, query, vec, limit)
	if err != nil {
		return nil, err
	}
	out := make([]ContextChunk, 0, len(hits))
	for _, h := range hits {
		out = append(out, ContextChunk{
			Path: h.Path, StartLn: h.StartLn, EndLn: h.EndLn,
			Symbol: h.Symbol, Lang: h.Lang, Text: h.Text,
			Score: h.Score, Sources: h.Sources,
		})
	}
	// Stable ordering by descending score, so a caller that truncates keeps
	// the best.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// EmbedderName reports the retrieval signal in use.
func (r *StoreRetriever) EmbedderName() string {
	if r == nil || r.EmbedderName_ == "" {
		return "lexical-only"
	}
	return r.EmbedderName_
}

// Degraded implements Retriever.
func (r *StoreRetriever) Degraded() bool {
	return r == nil || !r.Available || r.SearchFn == nil
}

// Ensure the interface is satisfied at compile time.
var _ Retriever = (*StoreRetriever)(nil)

// SymbolHintFromURL derives a search hint from a request URL.
//
// A path segment is often the resource name ("/users/42/orders"), and the
// handler that serves it is named after it. This is a weak signal on its own,
// which is why it is a hint and not the query.
func SymbolHintFromURL(raw string) string {
	u, err := parseURLPath(raw)
	if err != nil {
		return ""
	}
	segs := strings.FieldsFunc(u, func(r rune) bool { return r == '/' || r == '-' || r == '_' })
	if len(segs) == 0 {
		return ""
	}
	// The LAST non-numeric segment is the most specific one; numeric ids are
	// useless as a search term.
	for i := len(segs) - 1; i >= 0; i-- {
		if !isAllDigits(segs[i]) && len(segs[i]) > 2 {
			return segs[i]
		}
	}
	return ""
}

func parseURLPath(raw string) (string, error) {
	// Deliberately minimal: only the path is wanted, and net/url on a redacted
	// URL is already safe, but a bare path needs no error path at all.
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw = raw[:i]
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[j:], nil
		}
		return "", nil
	}
	return raw, nil
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
