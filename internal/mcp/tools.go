package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"pulse/internal/ast"
	"pulse/internal/engine"
	"pulse/internal/fuzz"
)

// ToolExecuteRequest is the single tool this bridge exposes.
//
// The name is `execute_request` (snake_case) rather than the roadmap's
// illustrative `run-request`: the MCP convention for tool names is snake_case,
// and agents match on the advertised name exactly. The deviation from
// ROADMAP.md §3.1.3's example is recorded in MEMORY.md ADL-0020.
const ToolExecuteRequest = "execute_request"

// maxPayloadBytes bounds an inline .http payload. An agent's context window is
// finite and a 64 MiB "one-line request" is an attack or a mistake, not a use
// case; failing with a clear limit beats accepting it.
const maxPayloadBytes = 4 << 20 // 4 MiB

// MaxResponseBytes bounds what is handed back to the agent. The engine streams
// without bound; the MODEL has a context window, and a 50 MB body pushed into
// one tool result is how an agent's session dies. The truncation is explicit in
// the response rather than silent.
//
// It is exported because it is a contract the agent is told about, and a test
// that asserts against a copied constant proves nothing about the value in
// force.
const MaxResponseBytes = 256 << 10 // 256 KiB

// maxResponseBytes is the internal alias used by the sink.
const maxResponseBytes = MaxResponseBytes

// ToolInput is the validated argument set for execute_request.
type ToolInput struct {
	// Payload is inline .http source.
	Payload string `json:"payload,omitempty"`
	// Path is a .http file on disk.
	Path string `json:"path,omitempty"`
	// Index selects which request in the file to run (0-based, default 0).
	Index int `json:"index,omitempty"`
	// TimeoutMS bounds the run.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// BodyMode selects how the response body is rendered: "text" (default) or
	// "json" (pretty-printed, falling back to text if the body is not JSON).
	BodyMode string `json:"body_mode,omitempty"`
	// IncludeBody set false to return headers and status only.
	IncludeBody *bool `json:"include_body,omitempty"`
	// Warnings record values Pulse converted on the caller's behalf. They are
	// never silent: an agent that believes its input was honoured verbatim must
	// be able to see that it was not.
	Warnings []string `json:"-"`
}

// ExecuteRequestArgs is the raw, unvalidated argument map an agent sent.
type ExecuteRequestArgs = map[string]any

// Executor runs one parsed .http request. It is the seam between the MCP layer
// and the domain: the tool validates and formats, the engine executes, and
// neither reaches into the other.
type Executor interface {
	// Execute parses src, runs the selected request, and returns the collected
	// outcome. It must never write to stdout.
	Execute(ctx context.Context, src, origin string, index int, timeout time.Duration) (*Outcome, error)
}

// Outcome is a completed run, already redacted.
type Outcome struct {
	Status     int                       `json:"status"`
	StatusText string                    `json:"status_text"`
	Proto      string                    `json:"proto"`
	Method     string                    `json:"method"`
	URL        string                    `json:"url"`
	Headers    http.Header               `json:"headers"`
	Body       string                    `json:"body"`
	BodyBytes  int64                     `json:"body_bytes"`
	Truncated  bool                      `json:"truncated"`
	Digest     string                    `json:"digest"`
	ElapsedMS  int64                     `json:"elapsed_ms"`
	RequestIdx int                       `json:"request_index"`
	Telemetry  *engine.TelemetrySnapshot `json:"telemetry,omitempty"`
	Warnings   []string                  `json:"warnings,omitempty"`

	// Context carries backend source bound to a 5xx failure (ROADMAP.md
	// §3.2.4). nil on every non-5xx outcome and on a 5xx with nothing to
	// attach — which are different states, and the difference is the point:
	// nil means "no context was bound", and the agent is told nothing about
	// whether that is because the run succeeded or because the index is empty.
	Context *Binding `json:"context,omitempty"`
}

// EngineExecutor is the production Executor, backed by the AST parser and the
// engine's Sink pipeline.
type EngineExecutor struct {
	// Redactor is applied to everything in the outcome. It is the Goal 1
	// chokepoint; the MCP layer holds the interface so it stays protocol- and
	// wiring-agnostic (ARCHITECTURE.md §3.1).
	Redactor engine.Redactor
	// Resolver backs `{{var}}` interpolation. Nil means variables fail closed.
	Resolver engine.Resolver
	// NewEngine builds the executor for one parsed document. Injected so tests
	// can substitute a local server; production uses the real engine.
	NewEngine func(doc *ast.Document) engine.ProtocolEngine
	// ExecutionRecorder receives one record per executed request. Nil disables
	// recording: history and collection tools then serve nothing, which
	// is honest but useless — production always binds the store-backed
	// recorder through the composition root.
	Recorder ExecutionRecorder
}

// ExecutionRecord is one executed request's metadata for the history.
//
// Secrets never belong here: the URL template is recorded RAW (anchors
// unresolved), and the error text is scrubbed by the recorder before it
// touches the store — the store itself refuses interpolation anchors.
type ExecutionRecord struct {
	Origin        string // file path, or "<inline payload>"
	Index         int    // request ordinal in the document
	Name          string
	Method        string
	URLTemplate   string // raw, anchors intact
	Status        int
	RequestBytes  int64
	ResponseBytes int64
	Digest        string
	ElapsedMS     int64
	Err           string // engine error text, possibly empty
}

// ExecutionRecorder persists execution records. Implementations must never fail
// the run: a recording error is logged and dropped, because losing one
// history row is recoverable and failing the request is not.
type ExecutionRecorder interface {
	RecordExecution(ctx context.Context, r ExecutionRecord)
}

// Execute implements Executor.
//
// The document owns every byte the engine reads, so it is released only after
// the run has fully unwound — releasing earlier would free the arena out from
// under an in-flight request (the same ownership rule the engine itself
// documents).
func (e *EngineExecutor) Execute(ctx context.Context, src, origin string, index int, timeout time.Duration) (*Outcome, error) {
	if len(src) > maxPayloadBytes {
		return nil, WrapProtocolError(CodeInvalidParams, "request source exceeds the inline payload limit",
			fmt.Errorf("%d bytes exceeds the %d-byte limit", len(src), maxPayloadBytes))
	}

	arena := ast.AcquireArena()
	arena.Reset([]byte(src))
	doc, err := ast.Parse(arena)
	if err != nil {
		// The arena holds the source the parser rejected; release it here
		// because no document was produced.
		arena.Release()
		return nil, WrapProtocolError(CodeParseFailed, "the .http source did not parse", err)
	}
	defer doc.Release()

	requests := collect(doc)
	if index < 0 || index >= len(requests) {
		return nil, WrapProtocolError(CodeInvalidParams,
			fmt.Sprintf("request index %d is out of range", index),
			fmt.Errorf("the file contains %d request(s)", len(requests)))
	}

	eng := e.NewEngine(doc)
	if eng == nil {
		return nil, NewProtocolError(CodeInternalError, "no protocol engine is configured")
	}
	defer func() { _ = eng.Close() }()

	collector := newCollectingSink(maxResponseBytes)
	runCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	handle := eng.Run(runCtx, requests[index], collector)
	// A run that will not unwind inside the budget leaves the client hanging,
	// which is worse than a clear error: the teardown budget bounds aborts, and
	// the timeout already gave the run a reason to stop.
	_ = handle.WaitTimeout(engineTeardownBudget)
	collector.Wait()
	out, execErr := collector.outcome(origin, index, e.Redactor), collector.Err()
	// A policy refusal is not a run. Recording it would put a request Pulse
	// never sent into history, where `get_history` and the TUI would show it
	// as an executed (status 0) request.
	if blocked := errors.Is(execErr, engine.ErrPolicyBlocked); !blocked && out != nil && e.Recorder != nil {
		rec := ExecutionRecord{
			Origin: origin, Index: index,
			Method: out.Method, URLTemplate: out.URL,
			Status: out.Status, RequestBytes: int64(len(src)),
			ResponseBytes: out.BodyBytes, Digest: out.Digest,
			ElapsedMS: out.ElapsedMS,
		}
		if execErr != nil {
			rec.Err = execErr.Error()
		}
		if rq := requests[index]; rq != nil {
			rec.Name = doc.Str(rq.NameStart, rq.NameEnd)
		}
		e.Recorder.RecordExecution(ctx, rec)
	}
	return out, execErr
}

// collect gathers the document's requests into a slice so a tool argument can
// index them. The Document owns them, so the slice borrows.
func collect(doc *ast.Document) []*ast.Request {
	var out []*ast.Request
	for r := range doc.Requests() {
		out = append(out, r)
	}
	return out
}

// engineTeardownBudget bounds how long the MCP handler waits for a run to
// unwind before reporting a timeout.
const engineTeardownBudget = 2 * time.Second

// ---- collecting sink -------------------------------------------------------

// collectingSink implements engine.Sink and bounds what reaches the agent.
//
// The engine's own chunk pool already bounds in-flight memory; this bounds what
// ACCUMULATES, which is a different and equally necessary limit once the
// consumer is a language model with a finite context window.
type collectingSink struct {
	mu        sync.Mutex
	buf       []byte
	total     int64
	truncated bool
	headers   http.Header
	res       *engine.RunResult
	err       error
	done      chan struct{}
	once      sync.Once
	max       int
}

func newCollectingSink(max int) *collectingSink {
	return &collectingSink{done: make(chan struct{}), max: max}
}

// Chunk implements engine.Sink. The chunk is recycled immediately: the engine
// owns its pool and must not wait on us.
func (s *collectingSink) Chunk(c *engine.Chunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total += int64(c.N)
	if len(s.buf) >= s.max {
		s.truncated = true
	} else {
		room := s.max - len(s.buf)
		if room < c.N {
			s.buf = append(s.buf, (*c.Buf)[:room]...)
			s.truncated = true
		} else {
			s.buf = append(s.buf, (*c.Buf)[:c.N]...)
		}
	}
	c.Recycle()
}

func (s *collectingSink) Done(r *engine.RunResult) {
	s.mu.Lock()
	s.res = r
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
}

func (s *collectingSink) Failed(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
}

// Wait blocks until a terminal callback arrives. It is a no-op once one has.
func (s *collectingSink) Wait() { <-s.done }

// Err returns the run's terminal error, if any.
func (s *collectingSink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// outcome assembles a redacted view of the run.
func (s *collectingSink) outcome(origin string, index int, r engine.Redactor) *Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := &Outcome{
		RequestIdx: index,
		BodyBytes:  s.total,
		Truncated:  s.truncated,
		Body:       string(s.buf),
	}
	if s.res != nil {
		out.Status = s.res.Status
		out.StatusText = s.res.StatusText
		out.Proto = s.res.Proto
		out.Method = s.res.Method
		out.URL = s.res.URL
		out.Headers = s.res.Header
		out.ElapsedMS = s.res.Elapsed.Milliseconds()
		if s.res.Digest != ([sha256.Size]byte{}) {
			out.Digest = fmt.Sprintf("sha256:%x", s.res.Digest)
		}
	}
	if s.truncated {
		// Silence would read as "that was the whole body". Say so, with the
		// true size, so the agent can decide whether to narrow the request.
		out.Warnings = append(out.Warnings,
			fmt.Sprintf("response body truncated at %d bytes of %d", len(s.buf), s.total))
	}

	// The redaction chokepoint. Everything an agent sees passes through it —
	// a resolved secret in a tool result is a secret in the model's context
	// window, and from there in every log the agent writes (AGENTS.md §3.4).
	if r != nil {
		out.Body = r.String(out.Body)
		out.URL = r.String(out.URL)
		if out.Headers != nil {
			out.Headers = r.Headers(out.Headers)
		}
		if s.err != nil {
			s.err = r.Error(s.err)
		}
	}
	return out
}

// ---- tool registration -----------------------------------------------------

// RegisterTools installs every tool on srv.
func RegisterTools(srv *mcpserver.MCPServer, exec Executor, opts ...Option) error {
	var ro registerOptions
	for _, fn := range opts {
		if fn != nil {
			fn(&ro)
		}
	}
	binder := ro.binder

	tool := mcp.NewTool(ToolExecuteRequest,
		mcp.WithDescription(
			"Execute an HTTP request written in the .http file format. "+
				"Supply EITHER `payload` (inline .http source) OR `path` (a .http file to read), "+
				"not both. Returns the status, redacted headers, and the response body."),
		mcp.WithString("payload",
			mcp.Description("Inline .http source to execute.")),
		mcp.WithString("path",
			mcp.Description("Path to a .http file to execute.")),
		mcp.WithNumber("index",
			mcp.Description("Zero-based index of the request to run when the source contains several. Default 0.")),
		mcp.WithNumber("timeout_ms",
			mcp.Description("Maximum run duration in milliseconds. Default 30000.")),
		mcp.WithString("body_mode",
			mcp.Description("`text` (default) or `json` to pretty-print a JSON response body.")),
		mcp.WithBoolean("include_body",
			mcp.Description("Set false to return status and headers only. Default true.")),
	)
	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return handleExecuteRequest(ctx, exec, binder, req)
	})
	return nil
}

// Option configures tool registration.
type Option func(*registerOptions)

// ---- tool registration -----------------------------------------------------

// WithFailureBinder installs contextual error binding for execute_request.
//
// A nil binder is legal and means "this server does not index code": the tool
// omits the attachment entirely rather than emitting an empty one.
func WithFailureBinder(b *FailureBinder) Option {
	return func(o *registerOptions) { o.binder = b }
}

// WithFuzzFindings installs the fuzz finding sink (Audit 1B). Nil disables
// persistence; production passes the store so every 5xx/assertion failure is
// idempotently persisted to fuzz_findings.
func WithFuzzFindings(st fuzz.FindingStore) Option {
	return func(o *registerOptions) { o.findings = st }
}

type registerOptions struct {
	binder *FailureBinder
	// findings is the fuzz finding sink (Audit 1B). Nil disables
	// persistence; production sets the store via WithFuzzFindings.
	findings fuzz.FindingStore
}

// handleExecuteRequest validates, dispatches, and formats.
//
// Every failure below the dispatch point is a structured, coded result. There is
// no path where a malformed argument from an agent becomes a crash: the agent
// is an untrusted input source, and a panic in a server the agent did not write
// is a denial of service it can trigger by accident.
func handleExecuteRequest(ctx context.Context, exec Executor, binder *FailureBinder, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	input, err := ValidateExecuteRequest(req.GetArguments())
	if err != nil {
		// No partial dispatch: the engine is never touched.
		return errorResult(err), nil
	}
	if exec == nil {
		return errorResult(NewProtocolError(CodeInternalError, "no executor is configured")), nil
	}

	src := input.Payload
	origin := "<inline payload>"
	if input.Path != "" {
		b, readErr := os.ReadFile(input.Path)
		if readErr != nil {
			return errorResult(WrapProtocolError(CodeInvalidParams,
				"the request file could not be read", redactPathErr(readErr))), nil
		}
		if len(b) > maxPayloadBytes {
			return errorResult(WrapProtocolError(CodeInvalidParams,
				"the request file exceeds the size limit",
				fmt.Errorf("%s is %d bytes; the limit is %d", input.Path, len(b), maxPayloadBytes))), nil
		}
		src = string(b)
		origin = input.Path
	}

	timeout := time.Duration(input.TimeoutMS) * time.Millisecond
	out, execErr := exec.Execute(ctx, src, origin, input.Index, timeout)

	// A policy refusal is not a failed request: nothing was sent. Return the
	// reason as plain text the model can read and act on, instead of routing
	// it through the transport error mapper, which would describe a request
	// that never happened.
	if res, ok := policyResult(execErr); ok {
		return res, nil
	}

	// A failure must always explain itself. Returning a rendered outcome with a
	// zero status and no error text would tell the agent the server answered
	// nothing, which is both false and unfalsifiable. When the run failed and
	// produced no status, the response IS the error — with whatever partial body
	// arrived attached, because a truncated 500 is still evidence.
	if execErr != nil && (out == nil || out.Status == 0) {
		pe := mapEngineError(execErr)
		// mapEngineError returns a plain error if the failure is somehow
		// uncoded; wrap it so the structured payload always exists.
		coded, ok := AsProtocolError(pe)
		if !ok {
			coded = WrapProtocolError(CodeInternalError, "the request failed", pe)
		}
		if out != nil && out.Body != "" {
			coded = coded.WithData(map[string]any{
				"code":     coded.Code,
				"category": categoryFor(coded.Code),
				"detail":   coded.Message,
				"partial":  out.Body,
			})
		}
		return errorResult(coded), nil
	}
	if out == nil {
		return errorResult(mapEngineError(execErr)), nil
	}
	// Surface every coercion the validator performed alongside the result, so
	// an agent can tell what it actually asked for from what Pulse ran.
	out.Warnings = append(out.Warnings, input.Warnings...)

	// Contextual error binding (ROADMAP.md §3.2.4). This runs ONLY for a 5xx,
	// and only when a binder is configured. It is enrichment: if retrieval
	// fails the request result stands on its own, with a warning recording the
	// degradation.
	if binder != nil && out.Status >= 500 && out.Status <= 599 {
		binder.ExplainFailure(ctx, out, SymbolHintFromURL(out.URL))
	}

	payload := renderOutcome(out, input)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{mcp.NewTextContent(payload)},
		StructuredContent: out,
		IsError:           execErr != nil,
	}, nil
}

// renderOutcome formats a run for an agent's context window: compact, labelled,
// and honest about what was omitted.
func renderOutcome(out *Outcome, input *ToolInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s -> %d %s\n", out.Method, out.URL, out.Status, out.StatusText)
	if out.ElapsedMS > 0 {
		fmt.Fprintf(&b, "elapsed: %dms\n", out.ElapsedMS)
	}
	if len(out.Headers) > 0 {
		b.WriteString("headers:\n")
		for k, vs := range out.Headers {
			for _, v := range vs {
				fmt.Fprintf(&b, "  %s: %s\n", k, v)
			}
		}
	}
	if input.IncludeBody != nil && !*input.IncludeBody {
		b.WriteString("body: (omitted by request)\n")
		return b.String()
	}
	body := out.Body
	if input.BodyMode == "json" {
		if pretty, ok := prettyJSON(body); ok {
			body = pretty
		}
	}
	fmt.Fprintf(&b, "body (%d bytes):\n%s\n", out.BodyBytes, body)
	for _, w := range out.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", w)
	}
	if out.Context != nil && len(out.Context.Chunks) > 0 {
		renderBinding(&b, out.Context)
	}
	return b.String()
}

// renderBinding writes the backend-context attachment for an agent's context
// window.
//
// It is plain text with path:line prefixes rather than a JSON blob because the
// agent is reading this in a context window and a reviewer is reading it in a
// transcript; both want `internal/handlers/user.go:88` more than they want
// `{"path":"internal/handlers/user.go","start_ln":88}`.
func renderBinding(b *strings.Builder, bind *Binding) {
	fmt.Fprintf(b, "\nbecause this request failed with %d, the most relevant local code:\n",
		bind.Status)
	if bind.ErrorText != "" {
		fmt.Fprintf(b, "  error: %s\n", singleLine(bind.ErrorText))
	}
	if bind.Degraded {
		fmt.Fprintf(b, "  (DEGRADED: %s retrieval only, no embedding model)\n", bind.Embedder)
	}
	for _, c := range bind.Chunks {
		fmt.Fprintf(b, "\n  --- %s:%d-%d", c.Path, c.StartLn, c.EndLn)
		if c.Symbol != "" {
			fmt.Fprintf(b, " (%s)", c.Symbol)
		}
		if len(c.Sources) > 0 {
			fmt.Fprintf(b, " [matched by %s]", strings.Join(c.Sources, "+"))
		}
		b.WriteString("\n")
		for _, line := range strings.Split(strings.TrimRight(c.Text, "\n"), "\n") {
			fmt.Fprintf(b, "    %s\n", line)
		}
	}
	for _, n := range bind.Notes {
		fmt.Fprintf(b, "\n  note: %s\n", n)
	}
}

// singleLine collapses a multi-line fragment so it cannot break the labelled
// layout above.
func singleLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func prettyJSON(body string) (string, bool) {
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return "", false
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", false
	}
	return string(out), true
}

// errorResult renders a coded failure as a tool result carrying its code, so an
// agent can branch without parsing prose.
//
// The text carries the CAUSE, not just the summary: "the arguments do not
// satisfy the tool schema" tells an agent nothing it can act on, whereas
// "…; index: number is not valid" or "…; sneaky: additional properties not
// allowed" does. A summary-only error is the failure mode this line exists to
// prevent.
// policyResult renders a `.pulse-guard` refusal for the agent. ok is false for
// every other error, including a wrapped protocol error, so the caller keeps
// its normal failure path.
//
// The text is prefixed exactly once. `BLOCKED BY POLICY:` is the signal a model
// can key on and the reason is a complete sentence naming the file and the
// rule, so a self-correcting agent has everything it needs in one block.
func policyResult(err error) (*mcp.CallToolResult, bool) {
	var blocked *engine.PolicyBlockedError
	if !errors.As(err, &blocked) {
		return nil, false
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewTextContent("BLOCKED BY POLICY: " + blocked.Reason),
		},
		StructuredContent: map[string]any{
			"blocked":  true,
			"category": CategoryPolicy,
			"reason":   blocked.Reason,
		},
		IsError: true,
	}, true
}

func errorResult(err error) *mcp.CallToolResult {
	pe, ok := AsProtocolError(err)
	if !ok {
		pe = WrapProtocolError(CodeInternalError, "the request failed", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.NewTextContent("request failed: " + pe.Error()),
			mcp.NewTextContent(renderErrorData(pe)),
		},
		StructuredContent: map[string]any{
			"error": pe.payload(),
		},
		IsError: true,
	}
}

// mapEngineError translates a domain error into its JSON-RPC code. Sentinels are
// checked with errors.Is so wrapped errors classify correctly.
func mapEngineError(err error) error {
	if err == nil {
		return nil
	}
	if pe, ok := AsProtocolError(err); ok {
		return pe
	}
	switch {
	case errors.Is(err, engine.ErrUnresolved):
		return WrapProtocolError(CodeSecretUnresolved,
			"a variable could not be resolved; Pulse does not send placeholders or empty values", err)
	case errors.Is(err, engine.ErrPolicyBlocked):
		return WrapProtocolError(CodePolicyBlocked,
			"the request was blocked by .pulse-guard and nothing was sent", err)
	case errors.Is(err, context.DeadlineExceeded):
		return WrapProtocolError(CodeTimeout, "the request exceeded its time budget", err)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return WrapProtocolError(CodeTransport, "the connection ended before a complete response", err)
	default:
		return WrapProtocolError(CodeTransport, "the request could not be completed", err)
	}
}

// redactPathErr removes the file path from a read error. A path can carry a
// user name and a directory layout; neither belongs in a model context window
// unless the caller already knows it (they supplied it).
func redactPathErr(err error) error {
	var pe *os.PathError
	if ok := errors.As(err, &pe); ok {
		return fmt.Errorf("%s: %w", pe.Op, pe.Err)
	}
	return err
}
