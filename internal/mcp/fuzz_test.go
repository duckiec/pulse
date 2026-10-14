package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pulse/internal/fuzz"
	"pulse/internal/secrets"
)

// stubRunner returns a scripted status per payload note, and counts dispatches.
type stubRunner struct {
	mu        sync.Mutex
	calls     int
	notes     []string
	statusFor func(note string) int
	failFor   func(note string) error
	delay     time.Duration
}

func (s *stubRunner) Run(_ context.Context, req FuzzRequest) (int, error) {
	s.mu.Lock()
	s.calls++
	s.notes = append(s.notes, req.Payload.Note)
	s.mu.Unlock()
	if s.failFor != nil {
		if err := s.failFor(req.Payload.Note); err != nil {
			return 0, err
		}
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	return s.statusFor(req.Payload.Note), nil
}

func (s *stubRunner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

const fuzzSchema = `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /users:
    post:
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                name:
                  type: string
                  minLength: 1
                  maxLength: 8
      responses:
        '200': {description: ok}
`

func writeFuzzSchema(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "api.yaml")
	if err := os.WriteFile(p, []byte(fuzzSchema), 0o600); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	return p
}

// TestFuzzToolRejectsNonLoopbackTargets is the safety gate.
//
// A fuzz run sends hundreds of deliberately malformed requests. Pointed at a
// host that is not the user's own machine, that is an attack rather than a test,
// and the tool description saying "must be loopback" is not a control.
func TestFuzzToolRejectsNonLoopbackTargets(t *testing.T) {
	nonLoopback := []string{
		"http://example.com",
		"https://api.github.com",
		"http://8.8.8.8",
		"http://10.0.0.1:8080",
		"http://192.168.1.1",
		"http://[2001:db8::1]",
	}
	for _, target := range nonLoopback {
		if _, err := ValidateFuzzOptions(FuzzOptions{
			SchemaPath: writeFuzzSchema(t),
			Target:     target,
		}); err == nil {
			t.Errorf("%s was accepted as a fuzz target", target)
		} else if !errors.Is(err, fuzz.ErrTargetNotLoopback) &&
			!strings.Contains(err.Error(), "loopback") {
			t.Errorf("%s: error %v does not name the loopback restriction", target, err)
		}
	}

	loopback := []string{
		"http://127.0.0.1:8080",
		"http://localhost:3000",
		"http://[::1]:8080",
		"https://127.0.0.1",
	}
	for _, target := range loopback {
		if _, err := ValidateFuzzOptions(FuzzOptions{
			SchemaPath: writeFuzzSchema(t),
			Target:     target,
		}); err != nil {
			t.Errorf("%s was refused: %v", target, err)
		}
	}
}

// TestRequireLoopbackRejectsNumericSneaks proves the check is numeric.
//
// A prefix comparison on the string "127." is defeated by a decimal-encoded
// integer and by an IPv4-mapped IPv6 address, both of which name loopback in one
// spelling and a routable host in another.
func TestRequireLoopbackRejectsNumericSneaks(t *testing.T) {
	// 2130706433 is 127.0.0.1 in decimal; it is loopback.
	if err := RequireLoopback("http://2130706433:8080"); err != nil {
		t.Logf("decimal loopback refused (acceptable, stricter than required): %v", err)
	}
	// ::ffff:127.0.0.1 is loopback and must be accepted.
	if err := RequireLoopback("http://[::ffff:127.0.0.1]:8080"); err != nil {
		t.Errorf("an IPv4-mapped loopback address was refused: %v", err)
	}
	// A routable IPv4-mapped address must be refused.
	if err := RequireLoopback("http://[::ffff:8.8.8.8]:8080"); err == nil {
		t.Error("an IPv4-mapped routable address was accepted")
	}
	if err := RequireLoopback("ftp://127.0.0.1"); err == nil {
		t.Error("a non-HTTP scheme was accepted")
	}
	if err := RequireLoopback(""); err == nil {
		t.Error("an empty target was accepted")
	}
}

// TestFuzzToolDefaultsAreBounded proves the tool cannot be talked into running
// unbounded.
//
// The failure mode of a fuzzer pointed at a developer's laptop is a hung
// laptop, and the bounds are what stand between an agent's "more, harder" and a
// denial of service against the user's own machine.
func TestFuzzToolDefaultsAreBounded(t *testing.T) {
	opts, err := ValidateFuzzOptions(FuzzOptions{
		SchemaPath: writeFuzzSchema(t),
		Target:     "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("ValidateFuzzOptions: %v", err)
	}
	if opts.Budget <= 0 || opts.Budget > MaxFuzzBudget {
		t.Errorf("default budget = %d, want 1..%d", opts.Budget, MaxFuzzBudget)
	}
	if opts.Rate <= 0 {
		t.Errorf("default rate = %v; a zero rate must never reach the limiter", opts.Rate)
	}
	if opts.Concurrency <= 0 || opts.Concurrency > MaxFuzzConcurrency {
		t.Errorf("default concurrency = %d, want 1..%d", opts.Concurrency, MaxFuzzConcurrency)
	}

	// Absurd values are clamped, not honoured.
	opts, err = ValidateFuzzOptions(FuzzOptions{
		SchemaPath:  writeFuzzSchema(t),
		Target:      "http://127.0.0.1:1",
		Budget:      1_000_000,
		Rate:        1e9,
		Concurrency: 10_000,
	})
	if err != nil {
		t.Fatalf("ValidateFuzzOptions: %v", err)
	}
	if opts.Budget != MaxFuzzBudget {
		t.Errorf("budget %d was not clamped to %d", opts.Budget, MaxFuzzBudget)
	}
	if opts.Concurrency != MaxFuzzConcurrency {
		t.Errorf("concurrency %d was not clamped to %d", opts.Concurrency, MaxFuzzConcurrency)
	}
	t.Logf("clamped: budget %d, rate %.0f/s, concurrency %d",
		opts.Budget, opts.Rate, opts.Concurrency)
}

// TestFuzzToolReportsFiveHundredAsNotOk is the failure-correlation gate: a
// payload that crashes the handler must be `not ok` with the payload attached.
func TestFuzzToolReportsFiveHundredAsNotOk(t *testing.T) {
	runner := &stubRunner{
		statusFor: func(note string) int {
			if note == "maxLength+1" {
				return 500
			}
			return 200
		},
	}
	out, err := ExecuteFuzz(context.Background(), FuzzOptions{
		SchemaPath:  writeFuzzSchema(t),
		Target:      "http://127.0.0.1:1",
		OpID:        "POST /users",
		Seed:        42,
		Budget:      500,
		Rate:        5000, // fast: this gate is about the verdict, not the rate
		Concurrency: 8,
	}, runner, nil)
	if err != nil {
		t.Fatalf("ExecuteFuzz: %v", err)
	}
	t.Logf("%s\n%s", fuzz.Summarize(out.Findings), firstLinesOf(out.TAP, 20))

	if out.Failed != 1 {
		t.Errorf("%d failures, want exactly 1", out.Failed)
	}
	if !strings.Contains(out.TAP, "not ok") {
		t.Errorf("a 500 was not reported as `not ok`:\n%s", out.TAP)
	}
	if !strings.Contains(out.TAP, "unexpected status 500") {
		t.Errorf("the diagnostic does not state the status:\n%s", out.TAP)
	}
	// The TRIGGERING payload must be recoverable from the diagnostic.
	blockStart := strings.Index(out.TAP, "  ---")
	blockEnd := strings.Index(out.TAP, "  ...")
	if blockStart < 0 || blockEnd < blockStart {
		t.Fatalf("no diagnostic block in:\n%s", out.TAP)
	}
	block := out.TAP[blockStart:blockEnd]
	if !strings.Contains(block, "payload:") {
		t.Errorf("the diagnostic does not embed the payload:\n%s", block)
	}
	if !strings.Contains(block, "maxLength+1") {
		t.Errorf("the diagnostic does not name which probe triggered it:\n%s", block)
	}
	if !strings.Contains(block, "    status: '500'") {
		t.Errorf("the diagnostic does not carry the status:\n%s", block)
	}
	// The TAP plan must still match the number of results.
	if !strings.Contains(out.TAP, "1.."+itoa(out.Total)) {
		t.Errorf("the plan does not match the %d payloads sent", out.Total)
	}
}

// TestFuzzToolTreatsFourHundredAsPass proves 4xx is NOT a finding.
//
// A boundary value the server correctly rejects is the behaviour under test.
// Treating a 400 as a finding would make every fuzzer report every well-written
// server, and the user would learn to ignore the output entirely.
func TestFuzzToolTreatsFourHundredAsPass(t *testing.T) {
	for _, status := range []int{200, 201, 204, 400, 401, 403, 404, 409, 422, 429} {
		runner := &stubRunner{statusFor: func(string) int { return status }}
		out, err := ExecuteFuzz(context.Background(), FuzzOptions{
			SchemaPath:  writeFuzzSchema(t),
			Target:      "http://127.0.0.1:1",
			Seed:        1,
			Budget:      25,
			Rate:        5000,
			Concurrency: 4,
		}, runner, nil)
		if err != nil {
			t.Fatalf("ExecuteFuzz: %v", err)
		}
		if out.Failed != 0 {
			t.Errorf("status %d was reported as %d failures; 2xx and 4xx both "+
				"mean the server handled the boundary value", status, out.Failed)
		}
		if strings.Contains(out.TAP, "not ok") {
			t.Errorf("status %d produced a `not ok` line", status)
		}
	}
}

// TestFuzzToolTreatsTransportFailureAsNotOk proves a request that never got a
// response is a finding, not a pass.
func TestFuzzToolTreatsTransportFailureAsNotOk(t *testing.T) {
	runner := &stubRunner{
		statusFor: func(string) int { return 0 },
		failFor:   func(string) error { return errors.New("connection refused") },
	}
	out, err := ExecuteFuzz(context.Background(), FuzzOptions{
		SchemaPath:  writeFuzzSchema(t),
		Target:      "http://127.0.0.1:1",
		Seed:        1,
		Budget:      20,
		Rate:        5000,
		Concurrency: 4,
	}, runner, nil)
	if err != nil {
		t.Fatalf("ExecuteFuzz: %v", err)
	}
	if out.Failed == 0 {
		t.Error("a transport failure was reported as a pass")
	}
	if !strings.Contains(out.TAP, "before a response") {
		t.Errorf("the diagnostic does not distinguish a transport failure:\n%s", out.TAP)
	}
}

// TestFuzzToolRedactsDiagnostics proves the embedded payload is scrubbed.
//
// The diagnostic carries whatever the schema put in the payload, and the
// diagnostic is what an LLM reads. A secret in it is a secret in the model's
// context window (AGENTS.md §3.4).
func TestFuzzToolRedactsDiagnostics(t *testing.T) {
	const secret = "super-secret-value-abc"
	doc := strings.Replace(fuzzSchema, "maxLength: 8", "maxLength: 8\n                  default: "+secret, 1)
	p := filepath.Join(t.TempDir(), "api.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	red := secrets.NewRedactor()
	red.Register(secret)

	runner := &stubRunner{statusFor: func(string) int { return 500 }}
	out, err := ExecuteFuzz(context.Background(), FuzzOptions{
		SchemaPath:  p,
		Target:      "http://127.0.0.1:1",
		Seed:        1,
		Budget:      30,
		Rate:        5000,
		Concurrency: 4,
	}, runner, red)
	if err != nil {
		t.Fatalf("ExecuteFuzz: %v", err)
	}
	if strings.Contains(out.TAP, secret) {
		t.Errorf("the TAP output still contains the secret:\n%s", out.TAP)
	}
}

// TestFuzzToolReportsATargetServerRate proves the limiter is INSTALLED, not
// merely configured.
//
// The rate gate in internal/fuzz proves the limiter's own arithmetic. This gate
// proves the tool actually routes every request through it — a limiter that is
// built and then bypassed passes the first gate perfectly.
func TestFuzzToolReportsATargetServerRate(t *testing.T) {
	var served int64
	var timestamps []int64
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&served, 1)
		mu.Lock()
		timestamps = append(timestamps, time.Now().UnixNano())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	runner := newEngineRunner()
	out, err := ExecuteFuzz(context.Background(), FuzzOptions{
		SchemaPath:  writeFuzzSchema(t),
		Target:      srv.URL,
		Seed:        7,
		Budget:      120,
		Rate:        100,
		Concurrency: 4,
	}, runner, nil)
	if err != nil {
		t.Fatalf("ExecuteFuzz: %v", err)
	}
	if got := atomic.LoadInt64(&served); got != int64(out.Total) {
		t.Fatalf("the target served %d requests but the plan declared %d; "+
			"every planned payload must actually be dispatched", got, out.Total)
	}
	// The budget is a CAP, not a target: a schema declaring one bounded string
	// property yields a few dozen probes. Inflating the corpus to hit the budget
	// would mean inventing payloads no declared constraint implies.
	if out.Total < 20 {
		t.Fatalf("the corpus held only %d payloads; the gate needs enough to "+
			"span a rate-limit window", out.Total)
	}
	t.Logf("%d payloads, %dms, achieved %.1f/s (configured 100/s) [passed=%d failed=%d]",
		out.Total, out.ElapsedMS, out.AchievedRate, out.Passed, out.Failed)
	for _, f := range out.Findings {
		t.Logf("  finding: %s :: %s", f.Description, f.Diagnostic.Message)
	}

	// The achieved rate must sit near the configured one: comfortably above it
	// means the limiter was bypassed, and the burst allowance accounts for the
	// small excess.
	ceiling := float64(MaxFuzzConcurrency) // burst tolerance
	if out.AchievedRate > 100*2+ceiling {
		t.Errorf("achieved %.1f/s against a configured 100/s; the limiter is not "+
			"on the dispatch path", out.AchievedRate)
	}
}

// TestFuzzToolCompletesThePlanOnCancellation proves an aborted run still emits
// a complete TAP document.
//
// The plan promises N tests before the first request is sent. Emitting fewer
// result lines than the plan declares is the classic TAP hang, and cancellation
// is exactly when it would happen.
func TestFuzzToolCompletesThePlanOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	runner := &stubRunner{
		statusFor: func(string) int { return 200 },
		delay:     2 * time.Millisecond,
	}
	cancel() // cancelled before anything runs

	out, err := ExecuteFuzz(ctx, FuzzOptions{
		SchemaPath:  writeFuzzSchema(t),
		Target:      "http://127.0.0.1:1",
		Seed:        1,
		Budget:      100,
		Rate:        5000,
		Concurrency: 4,
	}, runner, nil)
	if err != nil {
		t.Fatalf("a cancelled run returned an error instead of a report: %v", err)
	}
	if !strings.Contains(out.TAP, "1.."+itoa(out.Total)) {
		t.Errorf("the plan does not cover the skipped payloads:\n%s", firstLinesOf(out.TAP, 8))
	}
	var resultLines int
	for _, l := range strings.Split(out.TAP, "\n") {
		if strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "not ok ") {
			resultLines++
		}
	}
	if resultLines != out.Total {
		t.Errorf("%d result lines for a plan of %d", resultLines, out.Total)
	}
	if runner.count() != 0 {
		t.Errorf("%d requests were dispatched on a cancelled run", runner.count())
	}
	t.Logf("cancelled before start: %d payloads planned, %d dispatched", out.Total, runner.count())
}

// TestParseFuzzArgsRejectsWrongTypes proves malformed arguments are structured
// errors, never panics.
//
// The agent is an untrusted input source. A panic in a server it did not write
// is a denial of service it can trigger by accident.
func TestParseFuzzArgsRejectsWrongTypes(t *testing.T) {
	good := map[string]any{
		"schema_path":     "api.yaml",
		"target":          "http://127.0.0.1:8080",
		"op":              "POST /users",
		"seed":            float64(3),
		"budget":          float64(10),
		"rate_per_second": float64(5),
		"concurrency":     float64(2),
	}
	if _, err := parseFuzzArgs(good); err != nil {
		t.Fatalf("valid arguments were rejected: %v", err)
	}
	// Numbers arriving as strings are accepted: clients stringify everything.
	asStrings := map[string]any{
		"schema_path": "api.yaml", "target": "http://127.0.0.1:1",
		"budget": "25", "seed": "9",
	}
	if _, err := parseFuzzArgs(asStrings); err != nil {
		t.Errorf("string-encoded numbers were rejected: %v", err)
	}
	bad := []map[string]any{
		{"schema_path": "a", "target": "http://127.0.0.1:1", "budget": []any{1}},
		{"schema_path": "a", "target": "http://127.0.0.1:1", "seed": map[string]any{}},
		{"schema_path": "a", "target": "http://127.0.0.1:1", "budget": "not-a-number"},
	}
	for i, args := range bad {
		if _, err := parseFuzzArgs(args); err == nil {
			t.Errorf("case %d: malformed arguments were accepted", i)
		}
	}
}

// TestFuzzToolRejectsAnUnusableDocument proves a bad schema is a structured
// error rather than an empty run.
//
// An empty corpus and a clean run are indistinguishable to the caller, and a
// fuzzer that reports "0 tests, all passed" for a document it could not read is
// worse than one that says nothing.
func TestFuzzToolRejectsAnUnusableDocument(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(p, []byte("swagger: \"2.0\"\npaths: {}\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	runner := &stubRunner{statusFor: func(string) int { return 200 }}
	_, err := ExecuteFuzz(context.Background(), FuzzOptions{
		SchemaPath: p, Target: "http://127.0.0.1:1", Budget: 10,
	}, runner, nil)
	if err == nil {
		t.Fatal("a non-OpenAPI document produced a run")
	}
	coded, ok := AsProtocolError(err)
	if !ok {
		t.Errorf("error %v is not a ProtocolError; the agent cannot branch on it", err)
		return
	}
	if coded.Code != CodeInvalidParams {
		t.Errorf("code = %d, want %d (invalid params)", coded.Code, CodeInvalidParams)
	}
	if !strings.Contains(coded.Message, "OpenAPI") {
		t.Errorf("message %q does not say what was wrong", coded.Message)
	}
}

// TestFuzzToolRejectsAnUnknownOperation proves the operation selector is checked
// rather than silently fuzzing nothing.
func TestFuzzToolRejectsAnUnknownOperation(t *testing.T) {
	runner := &stubRunner{statusFor: func(string) int { return 200 }}
	_, err := ExecuteFuzz(context.Background(), FuzzOptions{
		SchemaPath: writeFuzzSchema(t),
		Target:     "http://127.0.0.1:1",
		OpID:       "DELETE /nope",
		Budget:     10,
	}, runner, nil)
	if err == nil {
		t.Fatal("an unknown operation produced a run")
	}
	if runner.count() != 0 {
		t.Errorf("%d requests were dispatched for an unknown operation", runner.count())
	}
}

// TestFuzzToolRequiresARunner proves the tool fails closed without one, rather
// than reporting a run it cannot perform.
func TestFuzzToolRequiresARunner(t *testing.T) {
	_, err := ExecuteFuzz(context.Background(), FuzzOptions{
		SchemaPath: writeFuzzSchema(t),
		Target:     "http://127.0.0.1:1",
		Budget:     10,
	}, nil, nil)
	if err == nil {
		t.Fatal("a run with no runner succeeded")
	}
}

// TestFuzzToolIsDeterministicAcrossRuns proves the same seed produces the same
// corpus, with the same numbering, so two runs can be compared.
func TestFuzzToolIsDeterministicAcrossRuns(t *testing.T) {
	run := func() string {
		runner := &stubRunner{statusFor: func(string) int { return 200 }}
		out, err := ExecuteFuzz(context.Background(), FuzzOptions{
			SchemaPath:  writeFuzzSchema(t),
			Target:      "http://127.0.0.1:1",
			Seed:        12345,
			Budget:      60,
			Rate:        5000,
			Concurrency: 4,
		}, runner, nil)
		if err != nil {
			t.Fatalf("ExecuteFuzz: %v", err)
		}
		return out.TAP
	}
	a, b := run(), run()
	if a != b {
		t.Errorf("two runs with the same seed produced different TAP documents:\n%s\n---\n%s",
			firstLinesOf(a, 12), firstLinesOf(b, 12))
	}
}

// newEngineRunner builds the production engine-backed runner with the user agent
// a real `pulse --mcp` process would install.
func newEngineRunner() *EngineFuzzRunner {
	return &EngineFuzzRunner{UserAgent: "pulse-fuzz", Timeout: DefaultFuzzTimeout}
}

func firstLinesOf(s string, n int) string {
	rows := strings.Split(s, "\n")
	if len(rows) > n {
		rows = rows[:n]
	}
	return strings.Join(rows, "\n")
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// TestFuzzProgressStreamsEveryPayload gates the live-progress seam (Move 15):
// with a Progress callback installed, every dispatched payload produces
// exactly one event, indices stay inside the run, and the event verdicts sum
// to the outcome counts. A hook that drops, duplicates, or mislabels events
// would drive a live UI to disagree with the final report.
func TestFuzzProgressStreamsEveryPayload(t *testing.T) {
	runner := &stubRunner{statusFor: func(string) int { return 200 }}
	var mu sync.Mutex
	var events []fuzz.Progress
	out, err := ExecuteFuzz(context.Background(), FuzzOptions{
		SchemaPath:  writeFuzzSchema(t),
		Target:      "http://127.0.0.1:1",
		Seed:        7,
		Budget:      25,
		Rate:        5000,
		Concurrency: 4,
		Progress: func(p fuzz.Progress) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, p)
		},
	}, runner, nil)
	if err != nil {
		t.Fatalf("ExecuteFuzz: %v", err)
	}
	if len(events) != runner.count() {
		t.Fatalf("progress events = %d, payloads dispatched = %d; the live stream must mirror the run exactly",
			len(events), runner.count())
	}
	var passed, failed, skipped int
	for _, e := range events {
		if e.Total != len(events) {
			t.Fatalf("event Total = %d, want %d (every event describes the same run)", e.Total, len(events))
		}
		if e.Index < 0 || e.Index >= len(events) {
			t.Errorf("event Index = %d out of range", e.Index)
		}
		verdicts := 0
		if e.Passed {
			verdicts++
			passed++
		}
		if e.Failed {
			verdicts++
			failed++
		}
		if e.Skipped {
			verdicts++
			skipped++
		}
		if verdicts != 1 {
			t.Errorf("event %+v carries %d verdicts, want exactly one", e, verdicts)
		}
	}
	if passed != out.Passed || failed != out.Failed || skipped != out.Skipped {
		t.Errorf("streamed (passed=%d failed=%d skipped=%d) != outcome (passed=%d failed=%d skipped=%d)",
			passed, failed, skipped, out.Passed, out.Failed, out.Skipped)
	}
}
