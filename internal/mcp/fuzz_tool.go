package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"pulse/internal/fuzz"
)

// ToolExecuteFuzz is the fuzzing tool's name.
//
// snake_case, matching ToolExecuteRequest: MCP convention, and generated client
// bindings assume it (MEMORY.md ADL-0020).
const ToolExecuteFuzz = "execute_fuzz_run"

// Fuzz limits. They are DEFAULTS with a documented rationale rather than knobs
// with no argument, because the failure mode of a fuzzer pointed at a developer's
// machine is a hung machine.
//
//   - budget    bounds total payloads, so a miscounted schema cannot run for ever
//   - rate      bounds load ON THE TARGET
//   - concurrency bounds load ON PULSE (open sockets, goroutines)
//
// They are separate because they bound different things. A high rate with
// unbounded concurrency opens hundreds of sockets; a low rate with unbounded
// concurrency holds them open for the whole run.
const (
	DefaultFuzzBudget      = 200
	DefaultFuzzRate        = fuzz.DefaultRate
	DefaultFuzzConcurrency = fuzz.DefaultConcurrency
	MaxFuzzBudget          = 5_000
	MaxFuzzConcurrency     = 32
)

// FuzzRequest is one payload together with the operation it targets and the
// base URL to send it to.
//
// The three travel together because none is dispatchable alone. A payload names
// a FIELD, not a request: "query.limit = 9999" is a probe only once you know
// which operation declares `limit`, and an operation is only a request once you
// know where it goes. Handing a runner a bare payload pushes the join — and with
// it every judgment about which other parameters a valid request needs — onto
// each implementation.
type FuzzRequest struct {
	// BaseURL is the loopback target, without a trailing slash.
	BaseURL string
	// Op is the operation the payload was generated for. Never nil.
	Op *fuzz.Op
	// Payload is the probe.
	Payload fuzz.Payload
}

// FuzzRunner executes one payload against the target and reports what happened.
//
// It is an interface so the tool handler can be tested against a stub that
// returns a scripted status, and so the tool does not hard-code a transport:
// [EngineFuzzRunner] dispatches through internal/engine, and a caller can supply
// anything else.
type FuzzRunner interface {
	// Run sends one payload. The returned status is the HTTP status; a non-nil
	// error means the request never produced a response, which counts as a
	// finding rather than a pass.
	Run(ctx context.Context, req FuzzRequest) (int, error)
}

// FuzzOptions are the tool's resolved inputs.
type FuzzOptions struct {
	// SchemaPath is a local OpenAPI document.
	SchemaPath string
	// OpID is the operation to fuzz, "METHOD /path". Empty means every
	// operation in the document.
	OpID string
	// Target is the base URL. Restricted to loopback.
	Target string
	// Seed makes the corpus reproducible. Zero means "derive from the schema and
	// the clock", which is NOT reproducible — so a caller wanting reproducibility
	// passes it explicitly.
	Seed uint64
	// Budget bounds the number of payloads.
	Budget int
	// Rate is requests per second.
	Rate float64
	// Concurrency bounds in-flight requests.
	Concurrency int
	// Progress receives one notification per payload as it completes. Optional,
	// and always non-blocking: a nil callback disables streaming.
	Progress func(fuzz.Progress)
	// PersistTo receives every failing payload (5xx or transport failure)
	// via fuzz.PersistFindings. Nil disables persistence: unit gates that
	// assert verdicts without booting SQLite set nothing. Production sets
	// the store through WithFuzzFindings so every finding is idempotently
	// persisted to fuzz_findings (Audit 1B).
	PersistTo fuzz.FindingStore
}

// ValidateFuzzOptions checks and defaults the options.
//
// The target check is the one that matters: a fuzz run sends hundreds of
// deliberately malformed requests, and pointing that at a host that is not the
// user's own machine is an attack rather than a test. It is refused here, at the
// boundary, rather than trusted from the tool description.
func ValidateFuzzOptions(in FuzzOptions) (FuzzOptions, error) {
	var out FuzzOptions

	if strings.TrimSpace(in.SchemaPath) == "" {
		return out, NewProtocolError(CodeInvalidParams, "schema_path is required")
	}
	out.SchemaPath = in.SchemaPath
	out.OpID = in.OpID
	out.Target = strings.TrimSpace(in.Target)
	out.Seed = in.Seed

	if err := RequireLoopback(out.Target); err != nil {
		return out, WrapProtocolError(CodeInvalidParams,
			"the fuzz target must be a loopback address", err)
	}

	budget := in.Budget
	if budget <= 0 {
		budget = DefaultFuzzBudget
	}
	if budget > MaxFuzzBudget {
		budget = MaxFuzzBudget
	}
	out.Budget = budget

	rate := in.Rate
	if rate <= 0 {
		rate = DefaultFuzzRate
	}
	if rate > MaxFuzzConcurrency*100 {
		// A rate far above the concurrency bound cannot be used anyway — the
		// semaphore is the tighter limit — so clamping here keeps the configured
		// rate and the achieved rate from disagreeing in the report.
		rate = float64(MaxFuzzConcurrency * 100)
	}
	out.Rate = rate

	conc := in.Concurrency
	if conc <= 0 {
		conc = DefaultFuzzConcurrency
	}
	if conc > MaxFuzzConcurrency {
		conc = MaxFuzzConcurrency
	}
	out.Concurrency = conc
	// The progress callback and the persistence sink are caller-owned
	// infrastructure, not values to validate: dropping them here would
	// silently disable live streaming (caught by
	// TestFuzzProgressStreamsEveryPayload) or finding persistence for every
	// validated run.
	out.Progress = in.Progress
	out.PersistTo = in.PersistTo
	return out, nil
}

// RequireLoopback refuses a non-loopback fuzz target.
//
// The check resolves the host rather than string-matching "localhost": a target
// spelled `http://127.0.0.1` and one spelled `http://localhost` are the same
// machine, and a name-based check would reject the former's resolved form or
// accept a DNS name that resolves elsewhere.
func RequireLoopback(target string) error {
	if target == "" {
		return fmt.Errorf("%w: no target given", fuzz.ErrTargetNotLoopback)
	}
	u, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("%w: %v", fuzz.ErrTargetNotLoopback, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%w: scheme %q is not http or https", fuzz.ErrTargetNotLoopback, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: no host in %q", fuzz.ErrTargetNotLoopback, target)
	}
	// An IP literal must be a loopback address, checked numerically so a
	// decimal-encoded 127.0.0.1 or an IPv4-mapped ::ffff:127.0.0.1 cannot slip
	// past a prefix comparison.
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("%w: %s is not a loopback address", fuzz.ErrTargetNotLoopback, ip)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		// A name that does not resolve is refused rather than assumed benign.
		return fmt.Errorf("%w: %q does not resolve: %v", fuzz.ErrTargetNotLoopback, host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: %q resolved to nothing", fuzz.ErrTargetNotLoopback, host)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("%w: %s resolves to %s, which is not loopback",
				fuzz.ErrTargetNotLoopback, host, ip)
		}
	}
	return nil
}

// FuzzOutcome is the structured result of a fuzz run.
type FuzzOutcome struct {
	// OpID is the operation fuzzed.
	OpID string `json:"op_id"`
	// Seed is the seed the corpus was derived from.
	Seed uint64 `json:"seed"`
	// Total is how many payloads the corpus held before the budget was applied.
	Total int `json:"total"`
	// Rate is the configured requests-per-second.
	Rate float64 `json:"rate"`
	// Concurrency is the configured in-flight bound.
	Concurrency int `json:"concurrency"`
	// Passed, Failed and Skipped are the outcome counts.
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
	// Elapsed is how long the run took, which is what makes the achieved rate
	// checkable from outside.
	ElapsedMS int64 `json:"elapsed_ms"`
	// AchievedRate is Total / Elapsed. It is reported so a caller can see the
	// limiter working rather than having to trust that it was installed.
	AchievedRate float64 `json:"achieved_rate"`
	// TAP is the Test Anything Protocol document for the run.
	TAP string `json:"tap"`
	// Warnings carries degradations.
	Warnings []string `json:"warnings,omitempty"`
	// Findings are the deduplicated failing payloads.
	Findings []fuzz.Result `json:"findings,omitempty"`
}

// ExecuteFuzz runs a boundary fuzz against a loopback target and renders the
// result as TAP.
//
// Every request passes the limiter BEFORE it is dispatched. The order matters:
// acquiring the slot first means the rate bound holds for the requests actually
// sent, rather than for the requests actually started.
func ExecuteFuzz(ctx context.Context, opts FuzzOptions, runner FuzzRunner, red Redactor) (*FuzzOutcome, error) {
	opts, err := ValidateFuzzOptions(opts)
	if err != nil {
		return nil, err
	}
	if runner == nil {
		return nil, NewProtocolError(CodeInternalError, "no fuzz runner is configured")
	}

	ir, err := fuzz.LoadOpenAPI(opts.SchemaPath)
	if err != nil {
		return nil, WrapProtocolError(CodeInvalidParams,
			"the OpenAPI document could not be read", err)
	}

	ops := selectOps(ir, opts.OpID)
	if len(ops) == 0 {
		return nil, NewProtocolError(CodeInvalidParams,
			fmt.Sprintf("no operation matches %q", opts.OpID))
	}

	limiter, err := fuzz.NewLimiter(opts.Rate, fuzz.DefaultBurst)
	if err != nil {
		return nil, WrapProtocolError(CodeInvalidParams, "the rate limit is not usable", err)
	}
	sem, err := fuzz.NewSemaphore(opts.Concurrency)
	if err != nil {
		return nil, WrapProtocolError(CodeInvalidParams, "the concurrency limit is not usable", err)
	}

	// The corpus is generated BEFORE any request is sent, so the plan count is
	// known up front and the TAP document cannot disagree with itself.
	var payloads []fuzz.Payload
	byID := make(map[string]*fuzz.Op, len(ops))
	for _, op := range ops {
		byID[op.ID] = op
		for _, p := range fuzz.Generate(op, opts.Seed, opts.Budget) {
			payloads = append(payloads, p)
		}
	}
	if len(payloads) == 0 {
		return nil, NewProtocolError(CodeInvalidParams,
			"the selected operations declare no fuzzable parameters")
	}
	if len(payloads) > opts.Budget {
		payloads = payloads[:opts.Budget]
		for i := range payloads {
			payloads[i].Index = i
		}
	}

	out := &FuzzOutcome{
		OpID:        describeOps(ops),
		Seed:        opts.Seed,
		Total:       len(payloads),
		Rate:        opts.Rate,
		Concurrency: opts.Concurrency,
	}

	start := time.Now()
	results := make([]fuzz.Result, len(payloads))

	for i := range payloads {
		p := payloads[i]
		res := fuzz.Result{
			Number:      p.Index + 1,
			Description: describePayload(p),
		}

		// A cancelled run still produces a COMPLETE TAP document: the remaining
		// cases are emitted as SKIP. A plan promising 200 tests followed by 3
		// results hangs a TAP consumer, and this is the one place that promise
		// is made.
		if err := ctx.Err(); err != nil {
			res.Skip = true
			res.OK = true
			res.Description += " (not run: " + err.Error() + ")"
			results[i] = res
			if opts.Progress != nil {
				opts.Progress(fuzz.Progress{
					Index:   p.Index,
					Total:   len(payloads),
					Passed:  res.OK && !res.Skip,
					Failed:  !res.OK && !res.Skip,
					Skipped: res.Skip,
				})
			}
			continue
		}

		if err := sem.Acquire(ctx); err != nil {
			res.Skip = true
			res.OK = true
			res.Description += " (not run: " + err.Error() + ")"
			results[i] = res
			if opts.Progress != nil {
				opts.Progress(fuzz.Progress{
					Index:   p.Index,
					Total:   len(payloads),
					Passed:  res.OK && !res.Skip,
					Failed:  !res.OK && !res.Skip,
					Skipped: res.Skip,
				})
			}
			continue
		}

		waitErr := limiter.Wait(ctx)
		var status int
		var runErr error
		if waitErr == nil {
			status, runErr = runner.Run(ctx, FuzzRequest{
				BaseURL: opts.Target,
				Op:      byID[p.OpID],
				Payload: p,
			})
		} else {
			runErr = waitErr
		}
		sem.Release()

		// A probe the transport cannot carry never reached the target, so the
		// target said nothing about it. Counting it as a failure would report a
		// correct server as broken; dropping it silently would shrink the corpus
		// while the plan still promised the case. SKIP with a stated reason is
		// the only disposition that is both honest and complete.
		if errors.Is(runErr, errProbeUndeliverable) {
			res.Skip = true
			res.OK = true
			res.Description += " (not run: " + runErr.Error() + ")"
			results[i] = res
			if opts.Progress != nil {
				opts.Progress(fuzz.Progress{
					Index:   p.Index,
					Total:   len(payloads),
					Passed:  res.OK && !res.Skip,
					Failed:  !res.OK && !res.Skip,
					Skipped: res.Skip,
				})
			}
			continue
		}

		res.OK = classify(status, runErr)
		if !res.OK {
			msg := "the target rejected the payload"
			if runErr != nil {
				msg = "the request failed before a response"
			} else {
				msg = fmt.Sprintf("unexpected status %d", status)
			}
			res.Diagnostic = fuzz.Diagnostic{
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
			if red != nil {
				// The diagnostic embeds the triggering payload, so it carries
				// whatever the schema put there. It is redacted for the same
				// reason any output is (AGENTS.md §3.4).
				res.Diagnostic.Message = red.String(res.Diagnostic.Message)
				for k, v := range res.Diagnostic.Fields {
					res.Diagnostic.Fields[k] = red.String(v)
				}
			}
		}
		results[i] = res
		if opts.Progress != nil {
			opts.Progress(fuzz.Progress{
				Index:   p.Index,
				Total:   len(payloads),
				Passed:  res.OK && !res.Skip,
				Failed:  !res.OK && !res.Skip,
				Skipped: res.Skip,
			})
		}
	}

	out.ElapsedMS = time.Since(start).Milliseconds()
	if out.ElapsedMS > 0 {
		out.AchievedRate = float64(out.Total) / (float64(out.ElapsedMS) / 1000)
	}

	for _, r := range results {
		switch {
		case r.Skip:
			out.Skipped++
		case r.OK:
			out.Passed++
		default:
			out.Failed++
			out.Findings = append(out.Findings, r)
		}
	}
	if out.Failed > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"%d of %d boundary payloads produced an unexpected response", out.Failed, out.Total))
	}
	out.TAP = fuzz.Render(results)
	// Production seam (Audit 1B): every failing payload is idempotently
	// persisted. A persistence failure fails the run closed: silently
	// dropping a 5xx would report a broken server as merely "warned".
	if opts.PersistTo != nil && len(out.Findings) > 0 {
		if err := fuzz.PersistFindings(ctx, opts.PersistTo, out.OpID, out.Findings); err != nil {
			return nil, WrapProtocolError(CodeInternalError, "the fuzz findings could not be persisted", err)
		}
	}
	return out, nil
}

// classify reports whether a run of one payload counts as a pass.
//
// A 2xx or a 4xx PASSES. A 4xx is the server correctly rejecting a malformed
// boundary value, which is the behaviour being tested for; treating a 400 as a
// fuzz finding would mean every fuzzer reports every well-written server.
//
// A 5xx FAILS. That is the whole signal: a boundary value that crashes a handler
// is a defect, and the defect is what this tool exists to surface.
func classify(status int, err error) bool {
	if err != nil {
		return false
	}
	if status >= 500 && status <= 599 {
		return false
	}
	return true
}

// selectOps returns the operations to fuzz.
func selectOps(ir *fuzz.IR, opID string) []*fuzz.Op {
	if opID != "" {
		if op := ir.Op(opID); op != nil {
			return []*fuzz.Op{op}
		}
		return nil
	}
	out := make([]*fuzz.Op, 0, len(ir.Ops))
	for i := range ir.Ops {
		out = append(out, &ir.Ops[i])
	}
	return out
}

// describeOps names the operations a run covered.
func describeOps(ops []*fuzz.Op) string {
	names := make([]string, 0, len(ops))
	for _, op := range ops {
		names = append(names, op.ID)
	}
	return strings.Join(names, ", ")
}

// describePayload builds the TAP test description.
func describePayload(p fuzz.Payload) string {
	where := p.Location
	if p.Field != "" {
		where += "." + p.Field
	}
	return fmt.Sprintf("%s %s [%s]", p.OpID, where, p.Note)
}

// renderPayload renders the triggering payload for the diagnostic block.
//
// It is rendered from the VALUE rather than the note because the note says what
// the fuzzer intended and the value is what was actually sent — and the gap
// between those two is exactly what a reader needs when a finding looks wrong.
func renderPayload(p fuzz.Payload) string {
	switch v := p.Value.(type) {
	case string:
		// Long payloads are elided with their real length, never truncated
		// silently: a diagnostic showing 80 of 1,048,576 bytes without saying so
		// is a lie about what was sent.
		const limit = 200
		if len(v) > limit {
			return fmt.Sprintf("%q (truncated, %d bytes total)", v[:limit], len(v))
		}
		return fmt.Sprintf("%q", v)
	default:
		return fuzzRenderJSON(v)
	}
}

// fuzzRenderJSON renders a non-string payload value as compact JSON.
//
// A raw `%v` would produce Go syntax (`map[a:1]`), which is not what was sent
// and misleads anyone reading the diagnostic.
func fuzzRenderJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// RegisterFuzzTool installs the fuzzing tool.
//
// It is separate from RegisterTools because it has an extra dependency — a
// runner — and a caller serving only `execute_request` should not be forced to
// supply one.
func RegisterFuzzTool(srv *mcpserver.MCPServer, runner FuzzRunner, red Redactor, opts ...Option) error {
	if srv == nil {
		return errors.New("mcp: nil server")
	}
	ro := registerOptions{}
	for _, fn := range opts {
		if fn != nil {
			fn(&ro)
		}
	}

	tool := mcp.NewTool(ToolExecuteFuzz,
		mcp.WithDescription(
			"Fuzz a loopback HTTP API using boundary payloads derived from a local "+
				"OpenAPI schema. Generates off-by-one integers, over-length strings, "+
				"type confusions and encoding hostile payloads, sends them under a "+
				"strict rate limit, and returns a TAP 13 report. Any payload that "+
				"produces a 5xx is reported as a finding with the payload embedded. "+
				"The target MUST be a loopback address."),
		mcp.WithString("schema_path",
			mcp.Required(),
			mcp.Description("Path to a local OpenAPI 3.x document (JSON or YAML).")),
		mcp.WithString("op",
			mcp.Description("Operation to fuzz as \"METHOD /path\". Omit to fuzz every operation in the document.")),
		mcp.WithString("target",
			mcp.Required(),
			mcp.Description("Base URL of the API to fuzz. Must be loopback (127.0.0.1, ::1, or a name resolving to one).")),
		mcp.WithNumber("seed",
			mcp.Description("Seed for the payload corpus. The same seed and schema produce a byte-identical corpus; pass one explicitly to make a finding reproducible.")),
		mcp.WithNumber("budget",
			mcp.Description("Maximum payloads to send. Default 200, capped at 5000.")),
		mcp.WithNumber("rate_per_second",
			mcp.Description("Maximum requests per second against the target. Default 20. This is a hard ceiling, not an average.")),
		mcp.WithNumber("concurrency",
			mcp.Description("Maximum requests in flight at once. Default 4, capped at 32.")),
	)
	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		in, err := parseFuzzArgs(req.GetArguments())
		if err != nil {
			return errorResult(err), nil
		}
		in.PersistTo = ro.findings
		out, err := ExecuteFuzz(ctx, in, runner, red)
		if err != nil {
			return errorResult(err), nil
		}
		return &mcp.CallToolResult{
			Content:           []mcp.Content{mcp.NewTextContent(out.TAP)},
			StructuredContent: out,
			IsError:           out.Failed > 0,
		}, nil
	})
	return nil
}

// parseFuzzArgs converts tool arguments into FuzzOptions.
//
// Malformed arguments produce a structured -32602 rather than a panic: the agent
// is an untrusted input source, and a panic in a server it did not write is a
// denial of service it can trigger by accident.
func parseFuzzArgs(args map[string]any) (FuzzOptions, error) {
	var out FuzzOptions
	get := func(k string) string {
		if v, ok := args[k].(string); ok {
			return v
		}
		return ""
	}
	out.SchemaPath = get("schema_path")
	out.OpID = get("op")
	out.Target = get("target")

	// Numbers arrive as float64 from JSON and sometimes as a string from a
	// client that stringifies everything. Both are accepted; a wrong TYPE is
	// rejected rather than coerced, so a caller passing an object where an
	// integer belongs hears about it.
	num := func(k string, dst *float64) error {
		v, ok := args[k]
		if !ok || v == nil {
			return nil
		}
		switch t := v.(type) {
		case float64:
			*dst = t
		case int:
			*dst = float64(t)
		case string:
			f, err := parseFloat(t)
			if err != nil {
				return WrapProtocolError(CodeInvalidParams, k+" must be a number", err)
			}
			*dst = f
		default:
			return NewProtocolError(CodeInvalidParams, k+" must be a number")
		}
		return nil
	}
	var seed, budget, rate, conc float64
	for _, f := range []struct {
		key string
		dst *float64
	}{
		{"seed", &seed}, {"budget", &budget},
		{"rate_per_second", &rate}, {"concurrency", &conc},
	} {
		if err := num(f.key, f.dst); err != nil {
			return out, err
		}
	}
	out.Seed = uint64(seed)
	out.Budget = int(budget)
	out.Rate = rate
	out.Concurrency = int(conc)
	return out, nil
}

// parseFloat parses a decimal number, wrapping a failure in the structured
// error the tool contract promises rather than in a bare strconv error.
func parseFloat(s string) (float64, error) {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number: %w", s, err)
	}
	return f, nil
}
