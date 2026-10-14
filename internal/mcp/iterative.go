package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"pulse/internal/fuzz"
)

// Iterative tool names. snake_case per ADL-0020, matching the existing tools.
const (
	ToolFuzzStart  = "fuzz_start"
	ToolFuzzNext   = "fuzz_next"
	ToolFuzzReport = "fuzz_report"
)

// maxProgressEvents bounds the per-session progress ring. A fuzz campaign can
// hold thousands of probes; the session keeps the trailing window so
// fuzz_report can show the tail without holding the whole run in memory.
const maxProgressEvents = 64

// FuzzStep is the outcome of one fuzz_next call.
type FuzzStep struct {
	Session string         `json:"session"`
	Index   int            `json:"index"`
	Total   int            `json:"total"`
	OK      bool           `json:"ok"`
	Skipped bool           `json:"skipped,omitempty"`
	Status  int            `json:"status,omitempty"`
	Payload string         `json:"payload"`
	Note    string         `json:"note"`
	Finding string         `json:"finding_id,omitempty"`
	Context []ContextChunk `json:"context,omitempty"`
	Done    bool           `json:"done"`
}

// FuzzReport is the terminal summary of a campaign, returned by fuzz_report.
// The session is discarded when the report is produced: a report describes a
// finished campaign, and re-reporting a live cursor would disagree with every
// step already handed out.
type FuzzReport struct {
	Session      string          `json:"session"`
	OpID         string          `json:"op_id"`
	Seed         uint64          `json:"seed"`
	Total        int             `json:"total"`
	Passed       int             `json:"passed"`
	Failed       int             `json:"failed"`
	Skipped      int             `json:"skipped"`
	TAP          string          `json:"tap"`
	Findings     []FuzzFinding   `json:"findings,omitempty"`
	ProgressTail []fuzz.Progress `json:"progress_tail,omitempty"`
}

// FuzzFinding is one deduplicated failing payload with its persisted id.
type FuzzFinding struct {
	ID      string `json:"id"`
	Status  int    `json:"status"`
	Payload string `json:"payload"`
	Op      string `json:"op"`
	Field   string `json:"field"`
}

// fuzzSession is one live campaign. The cursor is the whole state machine:
// it starts at 0, advances by exactly one per dispatched probe, and Done is
// cursor >= len(payloads). There is no other way to move it, which is what
// makes M88 (a cursor that never advances) detectable as duplicate probes.
type fuzzSession struct {
	id        string
	opID      string
	seed      uint64
	target    string
	payloads  []fuzz.Payload
	byOp      map[string]*fuzz.Op
	cursor    int
	runner    FuzzRunner
	red       Redactor
	retriever Retriever
	findings  fuzz.FindingStore
	limiter   *fuzz.Limiter
	sem       *fuzz.Semaphore
	results   []fuzz.Result
	progress  []fuzz.Progress
	// callerProgress is the FuzzOptions.Progress callback the starter
	// supplied. It runs alongside the session recorder, never instead.
	callerProgress func(fuzz.Progress)
	mu             sync.Mutex
	done           bool
}

// FuzzSessions is the registry of live campaigns, shared by the three tools.
// Sessions are independent: one mutex guards the map, and each session has
// its own for the cursor, so two agents stepping two campaigns never block
// each other on anything but map lookup.
type FuzzSessions struct {
	mu       sync.Mutex
	sessions map[string]*fuzzSession
}

// NewFuzzSessions returns an empty registry.
func NewFuzzSessions() *FuzzSessions {
	return &FuzzSessions{sessions: make(map[string]*fuzzSession)}
}

// newSessionID mints an unpredictable 128-bit handle. Predictable session ids
// let one agent step another's campaign; crypto/rand failure fails closed
// rather than falling back to a counter.
func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mcp: cannot mint a fuzz session: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Start validates the options, generates the deterministic corpus, and
// registers the session. The corpus is generated up front (like ExecuteFuzz)
// so the plan count is known before the first probe is sent.
func (r *FuzzSessions) Start(ctx context.Context, opts FuzzOptions, runner FuzzRunner, red Redactor, retriever Retriever) (id string, total int, err error) {
	if r == nil {
		return "", 0, NewProtocolError(CodeInternalError, "no fuzz session registry is configured")
	}
	opts, err = ValidateFuzzOptions(opts)
	if err != nil {
		return "", 0, err
	}
	// Progress callbacks are caller-owned streaming infrastructure: a caller
	// that set one keeps it, and the session recorder runs alongside rather
	// than instead, so validation dropping the callback (the ADL-0035 class
	// of bug) shows up as a missing recorder event, not a silent stream.
	if runner == nil {
		return "", 0, NewProtocolError(CodeInternalError, "no fuzz runner is configured")
	}
	ir, err := fuzz.LoadOpenAPI(opts.SchemaPath)
	if err != nil {
		return "", 0, WrapProtocolError(CodeInvalidParams,
			"the OpenAPI document could not be read", err)
	}
	ops := selectOps(ir, opts.OpID)
	if len(ops) == 0 {
		return "", 0, NewProtocolError(CodeInvalidParams,
			fmt.Sprintf("no operation matches %q", opts.OpID))
	}
	limiter, err := fuzz.NewLimiter(opts.Rate, fuzz.DefaultBurst)
	if err != nil {
		return "", 0, WrapProtocolError(CodeInvalidParams, "the rate limit is not usable", err)
	}
	sem, err := fuzz.NewSemaphore(opts.Concurrency)
	if err != nil {
		return "", 0, WrapProtocolError(CodeInvalidParams, "the concurrency limit is not usable", err)
	}
	var payloads []fuzz.Payload
	byID := make(map[string]*fuzz.Op, len(ops))
	for _, op := range ops {
		byID[op.ID] = op
		for _, p := range fuzz.Generate(op, opts.Seed, opts.Budget) {
			payloads = append(payloads, p)
		}
	}
	if len(payloads) == 0 {
		return "", 0, NewProtocolError(CodeInvalidParams,
			"the selected operations declare no fuzzable parameters")
	}
	if len(payloads) > opts.Budget {
		payloads = payloads[:opts.Budget]
		for i := range payloads {
			payloads[i].Index = i
		}
	}
	id, err = newSessionID()
	if err != nil {
		return "", 0, err
	}
	s := &fuzzSession{
		id: id, opID: describeOps(ops), seed: opts.Seed, target: opts.Target,
		payloads: payloads, byOp: byID, runner: runner, red: red,
		retriever: retriever, findings: opts.PersistTo,
		limiter: limiter, sem: sem, results: make([]fuzz.Result, 0, len(payloads)),
	}
	s.callerProgress = opts.Progress
	r.mu.Lock()
	r.sessions[id] = s
	r.mu.Unlock()
	return id, len(payloads), nil
}

// Next dispatches the probe at the cursor and advances by exactly one.
//
// The advance happens on every path that consumed a probe — pass, fail, or
// skip — so a failing probe cannot wedge the campaign on the same index
// forever. Only an exhausted session short-circuits: it reports Done without
// touching the runner, so stepping past the end dispatches nothing.
func (r *FuzzSessions) Next(ctx context.Context, id string) (FuzzStep, error) {
	if r == nil {
		return FuzzStep{}, NewProtocolError(CodeInternalError, "no fuzz session registry is configured")
	}
	r.mu.Lock()
	s, ok := r.sessions[id]
	r.mu.Unlock()
	if !ok {
		return FuzzStep{}, WrapProtocolError(CodeInvalidParams,
			"no such fuzz session", fmt.Errorf("session %q is unknown or already reported", id))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done || s.cursor >= len(s.payloads) {
		return FuzzStep{Session: id, Index: len(s.payloads), Total: len(s.payloads), Done: true}, nil
	}
	p := s.payloads[s.cursor]
	stepIndex := s.cursor

	res := fuzz.Result{Number: p.Index + 1, Description: describePayload(p)}
	status, runErr := s.dispatch(ctx, p)
	switch {
	case runErr != nil && errors.Is(runErr, errProbeUndeliverable):
		res.Skip = true
		res.OK = true
		res.Description += " (not run: " + runErr.Error() + ")"
	default:
		res.OK = classify(status, runErr)
		if !res.OK {
			res.Diagnostic = s.diagnose(p, status, runErr)
		}
	}
	s.results = append(s.results, res)
	s.cursor++
	s.recordProgress(fuzz.Progress{
		Index: stepIndex, Total: len(s.payloads),
		Passed: res.OK && !res.Skip, Failed: !res.OK && !res.Skip,
		Skipped: res.Skip, Status: status,
	})

	step := FuzzStep{
		Session: id, Index: stepIndex, Total: len(s.payloads),
		OK: res.OK, Skipped: res.Skip, Status: status,
		Payload: renderPayload(p), Note: p.Note,
		Done: s.cursor >= len(s.payloads),
	}
	if !res.OK {
		step.Finding = res.Diagnostic.Fields["finding_id"]
		step.Context = s.explain(ctx, p, status)
	}
	return step, nil
}

// Report renders the TAP document and discards the session. Reporting twice
// is an invalid-params error, not an empty report: the second report would
// describe a campaign whose probes were already consumed.
func (r *FuzzSessions) Report(ctx context.Context, id string) (FuzzReport, error) {
	if r == nil {
		return FuzzReport{}, NewProtocolError(CodeInternalError, "no fuzz session registry is configured")
	}
	r.mu.Lock()
	s, ok := r.sessions[id]
	if ok {
		delete(r.sessions, id)
	}
	r.mu.Unlock()
	if !ok {
		return FuzzReport{}, WrapProtocolError(CodeInvalidParams,
			"no such fuzz session", fmt.Errorf("session %q is unknown or already reported", id))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = true
	rep := FuzzReport{
		Session: id, OpID: s.opID, Seed: s.seed, Total: len(s.payloads),
		ProgressTail: append([]fuzz.Progress(nil), s.progress...),
	}
	var results []fuzz.Result
	for _, res := range s.results {
		results = append(results, res)
		switch {
		case res.Skip:
			rep.Skipped++
		case res.OK:
			rep.Passed++
		default:
			rep.Failed++
			rep.Findings = append(rep.Findings, FuzzFinding{
				ID:      res.Diagnostic.Fields["finding_id"],
				Status:  fieldStatus(res),
				Payload: res.Diagnostic.Fields["payload"],
				Op:      res.Diagnostic.Fields["op"],
				Field:   res.Diagnostic.Fields["field"],
			})
		}
	}
	// A cancelled campaign still produces a COMPLETE TAP document: the
	// remaining cases are SKIP, so the plan never overpromises (the same rule
	// ExecuteFuzz honours).
	for i := len(results); i < len(s.payloads); i++ {
		p := s.payloads[i]
		results = append(results, fuzz.Result{
			Number: p.Index + 1, OK: true, Skip: true,
			Description: describePayload(p) + " (not run: session reported early)",
		})
		rep.Skipped++
	}
	if s.red != nil {
		for i := range rep.Findings {
			rep.Findings[i].ID = s.red.String(rep.Findings[i].ID)
			rep.Findings[i].Payload = s.red.String(rep.Findings[i].Payload)
		}
	}
	rep.TAP = fuzz.Render(results)
	_ = ctx
	return rep, nil
}

// dispatch sends one probe under the limiter and the concurrency semaphore.
// The limiter is acquired BEFORE dispatch so the rate bound describes traffic
// actually sent — the same ordering ExecuteFuzz guarantees.
func (s *fuzzSession) dispatch(ctx context.Context, p fuzz.Payload) (int, error) {
	if err := s.sem.Acquire(ctx); err != nil {
		return 0, err
	}
	defer s.sem.Release()
	if err := s.limiter.Wait(ctx); err != nil {
		return 0, err
	}
	return s.runner.Run(ctx, FuzzRequest{BaseURL: s.target, Op: s.byOp[p.OpID], Payload: p})
}

// diagnose builds the redacted failure diagnostic and persists the finding,
// returning the diagnostic for the result. Persistence failure fails closed:
// silently dropping a 5xx would report a broken server as merely "warned"
// (the ExecuteFuzz rule, applied per probe).
func (s *fuzzSession) diagnose(p fuzz.Payload, status int, runErr error) fuzz.Diagnostic {
	msg := fmt.Sprintf("unexpected status %d", status)
	if runErr != nil {
		msg = "the request failed before a response"
	}
	d := fuzz.Diagnostic{
		Severity: "fail",
		Message:  msg,
		Fields: map[string]string{
			"status":  fmt.Sprint(status),
			"op":      p.OpID,
			"field":   p.Field,
			"where":   p.Location,
			"note":    p.Note,
			"payload": renderPayload(p),
		},
	}
	if s.red != nil {
		d.Message = s.red.String(d.Message)
		for k, v := range d.Fields {
			d.Fields[k] = s.red.String(v)
		}
	}
	if s.findings != nil {
		sig := fuzz.FindingSignature(status, []byte(d.Fields["payload"]))
		rec := fuzz.PersistedFuzzFinding{
			OpID: s.opID, Signature: sig, Status: status,
			Payload: d.Fields["payload"], CreatedAt: time.Now(),
		}
		if err := s.findings.PersistFuzzFinding(context.Background(), rec); err != nil {
			d.Fields["persist_error"] = err.Error()
		} else {
			d.Fields["finding_id"] = sig
		}
	}
	return d
}

// explain binds RAG context to a 5xx probe failure. A nil retriever means
// "this server does not index code": the step omits the attachment rather
// than emitting an empty one (the FailureBinder rule). Retrieval failure
// degrades to no attachment — the probe result stands on its own.
func (s *fuzzSession) explain(ctx context.Context, p fuzz.Payload, status int) []ContextChunk {
	if s.retriever == nil || status < 500 || status > 599 {
		return nil
	}
	query := p.OpID + " " + p.Field + " " + p.Note
	chunks, err := s.retriever.Retrieve(ctx, query, MaxContextChunks)
	if err != nil || len(chunks) == 0 {
		return nil
	}
	if s.red != nil {
		for i := range chunks {
			chunks[i].Text = s.red.String(chunks[i].Text)
			chunks[i].Path = s.red.String(chunks[i].Path)
			chunks[i].Symbol = s.red.String(chunks[i].Symbol)
		}
	}
	return chunks
}

// recordProgress appends one event to the bounded ring and forwards it to the
// caller's Progress callback when one is installed. The callback runs
// synchronously on the dispatching goroutine — which never holds the stdout
// lock's Real channel — and must itself never write to stdout (AGENTS.md
// §3.3); production installs only the session recorder.
func (s *fuzzSession) recordProgress(ev fuzz.Progress) {
	s.progress = append(s.progress, ev)
	if len(s.progress) > maxProgressEvents {
		s.progress = s.progress[len(s.progress)-maxProgressEvents:]
	}
	if s.callerProgress != nil {
		s.callerProgress(ev)
	}
}

// fieldStatus reads the status back out of a rendered diagnostic.
func fieldStatus(r fuzz.Result) int {
	var status int
	fmt.Sscan(r.Diagnostic.Fields["status"], &status)
	return status
}

// RegisterIterativeFuzzTools installs fuzz_start, fuzz_next, and fuzz_report.
// A nil registry omits all three; a nil runner fails Start closed (the
// registry itself never fabricates results).
func RegisterIterativeFuzzTools(srv *mcpserver.MCPServer, reg *FuzzSessions, runner FuzzRunner, red Redactor, retriever Retriever, findings fuzz.FindingStore) error {
	if srv == nil {
		return fmt.Errorf("mcp: nil server")
	}
	if reg == nil {
		return nil
	}
	start := mcp.NewTool(ToolFuzzStart,
		mcp.WithDescription("Start a deterministic fuzz campaign against a loopback target. Returns a session handle; advance it with fuzz_next."),
		mcp.WithString("schema_path", mcp.Required(),
			mcp.Description("Path to a local OpenAPI 3.x document (JSON or YAML).")),
		mcp.WithString("op",
			mcp.Description("Operation to fuzz as \"METHOD /path\". Omit to fuzz every operation.")),
		mcp.WithString("target", mcp.Required(),
			mcp.Description("Base URL. Must be loopback.")),
		mcp.WithNumber("seed",
			mcp.Description("Corpus seed. The same seed and schema produce a byte-identical corpus.")),
		mcp.WithNumber("budget",
			mcp.Description("Maximum payloads. Default 200, capped at 5000.")),
		mcp.WithNumber("rate_per_second",
			mcp.Description("Maximum requests per second. Default 20.")),
		mcp.WithNumber("concurrency",
			mcp.Description("Maximum in-flight requests. Default 4, capped at 32.")))
	srv.AddTool(start, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		opts, err := parseFuzzArgs(req.GetArguments())
		if err != nil {
			return errorResult(err), nil
		}
		opts.PersistTo = findings
		id, total, err := reg.Start(ctx, opts, runner, red, retriever)
		if err != nil {
			return errorResult(err), nil
		}
		body := map[string]any{"session": id, "total": total}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent("fuzz session " + id + " (" + fmt.Sprint(total) + " probes)\n")},
			StructuredContent: body,
			IsError:           false,
		}, nil
	})

	next := mcp.NewTool(ToolFuzzNext,
		mcp.WithDescription("Dispatch the next probe in a fuzz campaign. Returns the probe verdict; repeat until done is true."),
		mcp.WithString("session", mcp.Required(),
			mcp.Description("Session handle from fuzz_start.")))
	srv.AddTool(next, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := stringArg(req.GetArguments(), "session", true)
		if err != nil {
			return errorResult(err), nil
		}
		step, err := reg.Next(ctx, id)
		if err != nil {
			return errorResult(err), nil
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent(renderStep(step))},
			StructuredContent: step,
			IsError:           !step.OK && !step.Skipped && !step.Done,
		}, nil
	})

	report := mcp.NewTool(ToolFuzzReport,
		mcp.WithDescription("Terminate a fuzz campaign and return the TAP 13 report with deduplicated findings."),
		mcp.WithString("session", mcp.Required(),
			mcp.Description("Session handle from fuzz_start.")))
	srv.AddTool(report, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		id, err := stringArg(req.GetArguments(), "session", true)
		if err != nil {
			return errorResult(err), nil
		}
		rep, err := reg.Report(ctx, id)
		if err != nil {
			return errorResult(err), nil
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent(rep.TAP)},
			StructuredContent: rep,
			IsError:           rep.Failed > 0,
		}, nil
	})
	return nil
}

func renderStep(s FuzzStep) string {
	if s.Done && s.Total == s.Index {
		return "session " + s.Session + ": campaign complete (" + fmt.Sprint(s.Total) + " probes)\n"
	}
	verdict := "ok"
	if s.Skipped {
		verdict = "skip"
	} else if !s.OK {
		verdict = "FAIL"
	}
	out := fmt.Sprintf("probe %d/%d: %s [%s] %s\n", s.Index+1, s.Total, verdict, s.Note, s.Payload)
	if s.Finding != "" {
		out += "finding: " + s.Finding + "\n"
	}
	for _, c := range s.Context {
		out += "context: " + c.Path + ":" + fmt.Sprint(c.StartLn) + "\n"
	}
	if s.Done {
		out += "(campaign complete)\n"
	}
	return out
}
