// Package engine executes .http requests against real endpoints and streams
// the response back to a decoupled consumer.
//
// The engine is deliberately transport-agnostic and UI-agnostic: it knows how to
// turn an [ast.Request] into an *http.Request, how to stream the response body
// in bounded memory, and how to tear a run down deterministically. It knows
// nothing about Bubble Tea, JSON-RPC, or the wire format either mode speaks
// (ARCHITECTURE.md §3.1).
//
// Concurrency contract (ARCHITECTURE.md §5, ROADMAP.md §1.2):
//
//	reader goroutine ──▶ chan *Chunk (cap 64) ──▶ dispatch goroutine ──▶ Sink
//	         │
//	         └── io.TeeReader ──▶ SHA-256 mirror (the store write-ahead mirror)
//
// The reader goroutine is the only socket owner. It blocks on a full channel —
// which propagates back through the TCP window to the server — so a slow
// consumer slows the network read instead of growing memory (invariant 2 and 4).
package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pulse/internal/ast"
)

// redactedPlaceholder replaces any value that must never reach a UI, a log, or
// the store (AGENTS.md §3.4).
const redactedPlaceholder = "[REDACTED]"

// sensitiveHeaders are the header names whose values are always redacted before
// they leave the engine (AGENTS.md §3.4). Compared case-insensitively, since
// header names are canonicalized to Title-Case by net/http but an AST header may
// be spelled any way.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
	"x-api-key":           true,
}

// Resolver resolves `{{name}}` interpolation. It is nil in Phase 1: the engine
// then fails closed rather than sending a literal `{{...}}` to the network.
// Phase 2 (internal/secrets) supplies the keychain-backed implementation
// (ROADMAP.md §1.3, TASKS.md P1.4.2).
type Resolver interface {
	// Lookup returns the value for name, or an error. It MUST NOT log the
	// value and MUST NOT return a placeholder on failure (AGENTS.md §4.3).
	Lookup(name string) (string, error)
}

// ErrUnresolved marks a request that needs interpolation while no resolver is
// bound. It is deliberately a hard failure: an unresolved `{{var}}` sent
// verbatim would silently corrupt the request target.
var ErrUnresolved = errors.New("engine: unresolved variable (no resolver bound)")

// Sink consumes a run's results. Every callback is invoked from an engine
// goroutine, never from the caller's, and the terminal callback (Done or
// Failed) fires exactly once, after the final Chunk.
//
// Ownership: Chunk transfers ownership of the chunk's buffer to the sink. A sink
// that wants to keep the bytes must copy them; otherwise it MUST call
// [Chunk.Recycle] before returning. The engine never touches a recycled buffer.
type Sink interface {
	// Chunk delivers one chunk of response body. It is called from the
	// dispatch goroutine, serially, and may block — that backpressure is what
	// bounds engine memory.
	Chunk(*Chunk)
	// Done reports a completed run. Never called after Failed.
	Done(*RunResult)
	// Failed reports a run that did not complete, including user
	// cancellation. Never called after Done.
	Failed(error)
}

// RunResult is the terminal, redacted summary of a completed run.
//
// For a gRPC run Proto is "grpc" and the gRPC-specific fields below are set;
// the numeric Status is the mapped equivalent documented on grpcHTTPStatus, not
// a status line the server sent.
type RunResult struct {
	Method     string
	URL        string // userinfo redacted
	Status     int
	StatusText string
	Proto      string
	Header     http.Header // a redacted COPY of the response headers
	Bytes      int64
	Digest     [sha256.Size]byte // SHA-256 of the full response body
	Elapsed    time.Duration
	Started    time.Time

	// Symbol is the gRPC method address for a gRPC run ("/pkg.Svc/Method").
	Symbol string
	// Service is the dial authority for a gRPC run.
	Service string
	// Messages counts decoded protobuf messages for a server-streaming call.
	Messages int
}

// RunHandle tracks an in-flight run. It is returned by value and is safe to
// copy; the zero value is inert.
type RunHandle struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// Cancel aborts the run. It is safe to call more than once and from any
// goroutine. Cancellation unwinds through the teardown path (see teardownBudget).
func (h RunHandle) Cancel() {
	if h.cancel != nil {
		h.cancel()
	}
}

// Done is closed exactly once, after the terminal Sink callback has been made.
func (h RunHandle) Done() <-chan struct{} { return h.done }

// Wait blocks until the run has fully unwound.
func (h RunHandle) Wait() {
	if h.done != nil {
		<-h.done
	}
}

// WaitTimeout blocks until the run unwinds or d elapses, reporting whether it
// finished. A non-nil return means teardown exceeded its budget.
func (h RunHandle) WaitTimeout(d time.Duration) error {
	if h.done == nil {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-h.done:
		return nil
	case <-t.C:
		return fmt.Errorf("engine: run did not unwind within %s", d)
	}
}

// Redactor scrubs resolved secrets and sensitive header values from anything on
// its way to a Sink, a store, or a user (AGENTS.md §3.4).
//
// The engine defines this interface rather than importing internal/secrets:
// ARCHITECTURE.md §3.1 keeps domain packages siblings, so the concrete
// implementation is injected by internal/app. A nil Redactor is legal and means
// "no value scrubbing configured" — header-name redaction still applies.
type Redactor interface {
	// String returns s with every registered secret replaced.
	String(s string) string
	// Headers returns a redacted COPY of h.
	Headers(h http.Header) http.Header
	// Error returns err with any secret in its message replaced, preserving
	// errors.Is/As through Unwrap.
	Error(err error) error
}

// WithRedactor installs the redaction chokepoint. Every outbound request header
// and every inbound response payload is piped through it before it can reach a
// Sink or the store.
func WithRedactor(r Redactor) Option { return func(e *Engine) { e.redactor = r } }

// Option configures an Engine.
type Option func(*Engine)

// WithTimeout bounds every run. Zero (the default) means the caller's context
// is the only bound.
func WithTimeout(d time.Duration) Option { return func(e *Engine) { e.timeout = d } }

// WithResolver binds the `{{var}}` resolver. Until one is bound, interpolation
// fails closed with ErrUnresolved.
func WithResolver(r Resolver) Option { return func(e *Engine) { e.resolver = r } }

// WithHTTPClient supplies the client. The default shares http.DefaultTransport
// and has no client-level Timeout: per-run deadlines are context-based, so a
// client timeout could not be distinguished from a user interrupt.
func WithHTTPClient(c *http.Client) Option { return func(e *Engine) { e.client = c } }

// WithUserAgent sets the User-Agent header on every run.
func WithUserAgent(ua string) Option { return func(e *Engine) { e.userAgent = ua } }

// WithTeardownBudget bounds how long cleanup may take after cancellation
// (ROADMAP.md §1.2.6; the default is teardownBudgetDefault).
func WithTeardownBudget(d time.Duration) Option { return func(e *Engine) { e.teardownBudget = d } }

// Engine executes requests from one parsed document. It is safe for concurrent
// use: Run holds no mutable state of its own.
type Engine struct {
	doc            *ast.Document
	client         *http.Client
	timeout        time.Duration
	resolver       Resolver
	redactor       Redactor
	signer         Signer
	userAgent      string
	teardownBudget time.Duration
}

// New binds an engine to a parsed document. The document owns every byte the
// engine reads, so it must outlive every run started against this engine.
func New(doc *ast.Document, opts ...Option) *Engine {
	e := &Engine{
		doc:            doc,
		client:         &http.Client{Transport: http.DefaultTransport},
		teardownBudget: teardownBudgetDefault,
	}
	for _, o := range opts {
		o(e)
	}
	return e
}

// Document returns the document this engine executes.
func (e *Engine) Document() *ast.Document { return e.doc }

// Protocol implements ProtocolEngine. The REST executor answers ProtoREST; the
// ProtocolRouter is what dispatches, so this reports the executor's identity
// rather than performing any negotiation.
func (e *Engine) Protocol() ProtocolKind { return ProtoREST }

// Close releases engine-owned resources. The REST executor owns only an
// http.Client, whose idle pool is otherwise process-wide, so Close exists to
// satisfy the ProtocolEngine contract and to give callers one uniform teardown
// call. It is idempotent and cannot fail: failing to close an idle connection
// has no bearing on correctness.
func (e *Engine) Close() error {
	e.client.CloseIdleConnections()
	return nil
}

// Redactor returns the installed redaction chokepoint (nil when none).
func (e *Engine) Redactor() Redactor { return e.redactor }

// scrub pipes a string through the redaction chokepoint.
func (e *Engine) scrub(s string) string {
	if e.redactor == nil {
		return s
	}
	return e.redactor.String(s)
}

// scrubErr pipes an error's message through the redaction chokepoint.
func (e *Engine) scrubErr(err error) error {
	if e.redactor == nil {
		return err
	}
	return e.redactor.Error(err)
}

// Run starts req and streams its response to sink. It returns immediately with
// a handle; every outcome — success, transport failure, or cancellation — is
// reported through the sink, never through a return value, so the caller has
// exactly one place to observe results.
func (e *Engine) Run(ctx context.Context, req *ast.Request, sink Sink) RunHandle {
	if ctx == nil {
		panic("engine: Run requires a non-nil context")
	}
	if req == nil || sink == nil {
		panic("engine: Run requires a non-nil request and sink")
	}
	runCtx, cancel := context.WithCancel(ctx)
	if e.timeout > 0 {
		var tcancel context.CancelFunc
		runCtx, tcancel = context.WithTimeout(runCtx, e.timeout)
		outer := cancel
		cancel = func() { tcancel(); outer() }
	}
	h := RunHandle{cancel: cancel, done: make(chan struct{})}
	go e.execute(runCtx, req, sink, h)
	return h
}

// execute owns one run end to end and guarantees that RunHandle.done is closed
// exactly once, whatever happens.
func (e *Engine) execute(ctx context.Context, req *ast.Request, sink Sink, h RunHandle) {
	defer close(h.done)

	httpReq, err := e.buildRequest(ctx, req)
	if err != nil {
		sink.Failed(err)
		return
	}

	// Pre-dispatch signing seam (ROADMAP.md §3.4): only a request carrying
	// @wasm-sign reaches the signer, and a missing signer fails closed —
	// dispatching unsigned when a signature was asked for would report a
	// secure endpoint as merely "failing auth".
	if path := e.doc.WasmSign(req); path != "" {
		if e.signer == nil {
			sink.Failed(e.scrubErr(
				fmt.Errorf("engine: line %d: @wasm-sign %q needs a signer and none is bound", req.Line, path)))
			return
		}
		if err := e.signer.Sign(ContextWithSignerPath(ctx, path), httpReq); err != nil {
			sink.Failed(e.scrubErr(fmt.Errorf("engine: line %d: sign the request: %w", req.Line, err)))
			return
		}
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		// net/http embeds the full URL in transport errors, so scrub for both
		// credentials and resolved secrets before the message can reach a UI or
		// a log (AGENTS.md §3.4).
		sink.Failed(e.scrubErr(redactError(err)))
		return
	}

	// The response header map is copied and redacted before it can reach a
	// Sink or the store; the transport keeps the original.
	respHeader := redactHeaders(resp.Header)
	if e.redactor != nil {
		respHeader = e.redactor.Headers(respHeader)
	}
	res := &RunResult{
		Method:     httpReq.Method,
		URL:        redactURL(httpReq.URL.String()),
		Status:     resp.StatusCode,
		StatusText: resp.Status,
		Proto:      resp.Proto,
		Header:     respHeader,
		Started:    time.Now(),
	}

	chunks := make(chan *Chunk, chunkChannelCap)
	dispatched := make(chan struct{})
	go e.dispatch(ctx, chunks, sink, dispatched)

	// The tee is the store's write-ahead mirror: it hashes every byte the
	// consumer sees while retaining none of them (ROADMAP.md §1.2.2). sha256
	// Write cannot fail and cannot block, so the mirror never applies
	// backpressure to the read path.
	mirror := sha256.New()
	streamed, streamErr := e.stream(ctx, io.TeeReader(resp.Body, mirror), chunks)
	res.Digest = digestOf(mirror)
	res.Bytes = streamed

	// Teardown runs on a context detached from the caller's cancellation, so a
	// user interrupt mid-stream can never abort cleanup (ROADMAP.md §1.2.6,
	// AGENTS.md §3.2). It is bounded by its own budget instead — but only where
	// bounding is free. On SUCCESS every promised chunk must still reach the
	// sink ("stream without dropping a single frame"), and how long that takes
	// is the consumer's business, not the engine's; capping it would mean
	// discarding data the sink was already promised. On ABORT the dispatcher
	// discards instead of delivering, so the unwind is the engine's own work and
	// is bounded.
	abort := streamErr != nil || ctx.Err() != nil
	tctx, tcancel := context.WithTimeout(context.WithoutCancel(ctx), e.teardownBudget)
	defer tcancel()

	// Stop the consumer path first: once the queue is closed and drained, the
	// body can be closed without the transport competing with a live sink for
	// the socket.
	close(chunks)
	closeBody(resp.Body)

	if abort {
		select {
		case <-dispatched:
		case <-tctx.Done():
			// Bounded teardown expired; the dispatch goroutine is still
			// unwinding. Surfaced rather than swallowed (AGENTS.md §4.4).
			sink.Failed(fmt.Errorf("engine: teardown exceeded %s: %w", e.teardownBudget, tctx.Err()))
			return
		}
	} else {
		<-dispatched
	}

	res.Elapsed = time.Since(res.Started)
	switch {
	case streamErr != nil:
		sink.Failed(e.scrubErr(redactError(streamErr)))
	case ctx.Err() != nil:
		sink.Failed(fmt.Errorf("engine: run cancelled: %w", ctx.Err()))
	default:
		sink.Done(res)
	}
}

// buildRequest translates an AST node into an *http.Request. Unresolved
// interpolation is a hard failure: we never send a literal `{{var}}`.
func (e *Engine) buildRequest(ctx context.Context, ar *ast.Request) (*http.Request, error) {
	rawURL := e.doc.Str(ar.URLStart, ar.URLEnd)
	target, err := e.expandString(rawURL, ar.Line)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("engine: line %d: malformed request target %q: %w", ar.Line, redactURL(target), err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("engine: line %d: request target %q is not absolute (need scheme://host)", ar.Line, redactURL(target))
	}

	var body io.ReadCloser
	if ar.Body.Kind != ast.BodyNone {
		expanded, err := e.expandBytes(e.doc.BodyText(ar.Body), ar.Body.Line)
		if err != nil {
			return nil, err
		}
		body = io.NopCloser(bytes.NewReader(expanded))
	}

	method := ar.Method.String()
	if method == "UNKNOWN" {
		return nil, fmt.Errorf("engine: line %d: request has no recognised HTTP method", ar.Line)
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, parsed.String(), body)
	if err != nil {
		return nil, fmt.Errorf("engine: line %d: build request: %w", ar.Line, redactError(err))
	}
	if e.userAgent != "" {
		httpReq.Header.Set("User-Agent", e.userAgent)
	}
	for _, h := range ar.Headers {
		name := e.doc.Str(h.NameStart, h.NameEnd)
		if name == "" {
			return nil, fmt.Errorf("engine: line %d: header with no name", h.Line)
		}
		val, err := e.expandString(e.doc.Str(h.ValStart, h.ValEnd), h.Line)
		if err != nil {
			return nil, err
		}
		// Set, not Add: an AST header is a single field line, and a repeated
		// name in one line is a source defect rather than two fields.
		//
		// NOTE: the value on the wire is the REAL resolved secret — that is
		// what the endpoint expects, and redacting it here would send
		// "[REDACTED]" to the server and break authentication. Redaction is
		// applied to diagnostic COPIES only (RunResult, errors, store rows),
		// never to the outbound request (AGENTS.md §3.4).
		httpReq.Header.Set(name, val)
	}
	if body != nil {
		httpReq.ContentLength = int64(len(e.doc.BodyText(ar.Body)))
	}
	return httpReq, nil
}

// expandString returns src with every `{{name}}` replaced by its resolved value.
// The common case — no interpolation — returns src itself, allocating nothing.
func (e *Engine) expandString(src string, line int32) (string, error) {
	if !strings.Contains(src, "{{") {
		return src, nil
	}
	out, err := e.expandBytes([]byte(src), line)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// interpolate resolves `{{name}}` spans against a resolver, failing closed when
// none is bound (AGENTS.md §4.3: no placeholder, no empty string, no silent
// literal).
//
// It is package-level rather than a method because both executors need it and
// the resolution semantics must not be allowed to drift between wires: a REST
// request that fails closed on an unresolved variable and a gRPC one that
// quietly sent "{{token}}" would be two very different products.
func interpolate(src string, r Resolver, line int32) (string, error) {
	if !strings.Contains(src, "{{") {
		return src, nil
	}
	out, err := interpolateBytes([]byte(src), r, line)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// interpolateBytes is the byte-oriented twin of interpolate, for request bodies.
// With no interpolation it returns src itself, so a body made of arena bytes
// reaches the transport without a copy.
func interpolateBytes(src []byte, r Resolver, line int32) ([]byte, error) {
	if !bytes.Contains(src, []byte("{{")) {
		return src, nil
	}
	if r == nil {
		return nil, fmt.Errorf("%w: {{%s}} at line %d", ErrUnresolved, variableName(src), line)
	}
	var out bytes.Buffer
	out.Grow(len(src) + 16)
	rest := src
	for {
		idx := bytes.Index(rest, []byte("{{"))
		if idx < 0 {
			out.Write(rest)
			return out.Bytes(), nil
		}
		out.Write(rest[:idx])
		rest = rest[idx+2:]
		end := bytes.Index(rest, []byte("}}"))
		if end < 0 {
			// An unterminated anchor is a source defect; the lexer rejects it,
			// so reaching here means the AST was built by hand.
			return nil, fmt.Errorf("engine: line %d: unterminated interpolation anchor", line)
		}
		name := string(rest[:end])
		val, err := r.Lookup(name)
		if err != nil {
			// The resolver's error may quote the secret; wrap without adding it.
			return nil, fmt.Errorf("engine: line %d: resolve %q: %w", line, name, redactError(err))
		}
		out.WriteString(val)
		rest = rest[end+2:]
	}
}

// expandBytes is the Engine's byte-oriented twin of expandString.
func (e *Engine) expandBytes(src []byte, line int32) ([]byte, error) {
	return interpolateBytes(src, e.resolver, line)
}

// variableName extracts the name from a `{{name}}...` span, for diagnostics.
func variableName(src []byte) string {
	rest := src[2:]
	end := bytes.Index(rest, []byte("}}"))
	if end < 0 {
		if len(rest) > 32 {
			rest = rest[:32]
		}
		return string(rest)
	}
	return string(rest[:end])
}

// ---- redaction (AGENTS.md §3.4) --------------------------------------------

// redactHeaders returns a copy of h with every sensitive value replaced. The
// original is left untouched so the transport can still finish with it.
func redactHeaders(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		if sensitiveHeaders[strings.ToLower(k)] {
			out[k] = []string{redactedPlaceholder}
			continue
		}
		cp := make([]string, len(vs))
		copy(cp, vs)
		out[k] = cp
	}
	return out
}

// redactURL strips any userinfo, which is the one credential form a URL can
// carry. The path and query are preserved: at this layer they contain only what
// the .http file itself wrote, and redaction of interpolated query secrets
// belongs to the resolver (Move 4).
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.UserPassword(redactedPlaceholder, redactedPlaceholder)
	return u.String()
}

// redactError strips embedded URLs (which net/http puts in every transport
// error) so a credential in a URL cannot reach a log or the TUI.
func redactError(err error) error {
	if err == nil {
		return nil
	}
	msg := redactURL(err.Error())
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

func closeBody(body io.ReadCloser) {
	if body == nil {
		return
	}
	// Close on a detached goroutine so a body whose Close blocks on a wedged
	// peer cannot pin teardown past its budget (ROADMAP.md §1.2.6).
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = body.Close()
	}()
	select {
	case <-done:
	case <-time.After(teardownBudgetDefault):
	}
}
