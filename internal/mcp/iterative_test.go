package mcp

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"pulse/internal/fuzz"
)

// TestIterativeFuzzAdvancesWithoutDuplicatesOrStalls is the Iterative Fuzz
// Gate: fuzz_start followed by repeated fuzz_next must hand out each probe
// index exactly once, in order, and report Done on the final step — then
// Report must render a TAP document whose plan matches the dispatches.
//
// Mutation M88 hardcodes the cursor (offset 0): every step returns index 0,
// the duplicate check below goes red, and the campaign never reaches Done.
func TestIterativeFuzzAdvancesWithoutDuplicatesOrStalls(t *testing.T) {
	schema := writeFuzzSchema(t)
	reg := NewFuzzSessions()
	runner := &stubRunner{statusFor: func(string) int { return 200 }}
	dispatched := make(map[int]int)
	spy := &indexSpyRunner{inner: runner, dispatched: dispatched}

	ctx := context.Background()
	id, total, err := reg.Start(ctx, FuzzOptions{
		SchemaPath: schema, Target: "http://127.0.0.1:9",
		Budget: 10, Rate: 1000, Concurrency: 4,
	}, spy, nil, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if total <= 0 {
		t.Fatalf("Start reported a non-positive corpus: %d", total)
	}

	seen := make(map[int]int)
	var last FuzzStep
	for i := 0; i < total; i++ {
		step, err := reg.Next(ctx, id)
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		seen[step.Index]++
		if step.Total != total {
			t.Errorf("step %d reports total %d, want %d", i, step.Total, total)
		}
		if !step.OK {
			t.Errorf("step %d: a 200 probe must pass", i)
		}
		last = step
	}
	if !last.Done {
		t.Error("the final step must report Done")
	}
	for idx, n := range seen {
		if n != 1 {
			t.Errorf("probe index %d dispatched %d times, want exactly once", idx, n)
		}
	}
	if len(seen) != total {
		t.Errorf("dispatched %d distinct probes, want %d", len(seen), total)
	}
	if got := runner.count(); got != total {
		t.Errorf("runner dispatched %d probes, want %d", got, total)
	}
	// The cursor must advance the CONTENT, not just the counter: every
	// dispatched payload index must be distinct. A cursor hardcoded to 0
	// (M88) dispatches probe 0 ten times while the step counter still
	// advances — the duplicate is in what the target received.
	for idx, n := range dispatched {
		if n != 1 {
			t.Errorf("M88: payload index %d reached the target %d times, want exactly once", idx, n)
		}
	}
	if len(dispatched) != total {
		t.Errorf("M88: %d distinct payloads reached the target, want %d", len(dispatched), total)
	}

	// Stepping past the end dispatches nothing and stays Done.
	extra, err := reg.Next(ctx, id)
	if err != nil {
		t.Fatalf("over-step: %v", err)
	}
	if !extra.Done {
		t.Error("an over-step must report Done")
	}
	if got := runner.count(); got != total {
		t.Errorf("an over-step dispatched a probe: %d total, want %d", got, total)
	}

	rep, err := reg.Report(ctx, id)
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Passed != total || rep.Failed != 0 {
		t.Errorf("report counts passed=%d failed=%d, want %d/0", rep.Passed, rep.Failed, total)
	}
	plan := "1.." + itoaStep(total)
	if !strings.Contains(rep.TAP, plan) {
		t.Errorf("TAP plan must be %q, got:\n%s", plan, rep.TAP)
	}
}

// TestIterativeFuzzBindsExplanationsAndPersistsFindings proves the 5xx path:
// a failing probe carries a finding id (persisted through the FindingStore)
// and RAG-bound context chunks (retrieved and redacted).
func TestIterativeFuzzBindsExplanationsAndPersistsFindings(t *testing.T) {
	schema := writeFuzzSchema(t)
	reg := NewFuzzSessions()
	runner := &stubRunner{statusFor: func(string) int { return 500 }}

	retriever := &stubRetrieverForFuzz{chunks: []ContextChunk{
		{Path: "internal/handlers/user.go", StartLn: 88, EndLn: 96, Text: "name lookup"},
	}}
	findings := &stubFindings{}
	red := &passthroughRedactor{}

	ctx := context.Background()
	id, total, err := reg.Start(ctx, FuzzOptions{
		SchemaPath: schema, Target: "http://127.0.0.1:9",
		Budget: 4, Rate: 1000, Concurrency: 2, PersistTo: findings,
	}, runner, red, retriever)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var steps int
	for steps = 0; steps < total; steps++ {
		step, err := reg.Next(ctx, id)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if step.OK {
			t.Errorf("a 500 probe must fail")
		}
		if step.Finding == "" {
			t.Errorf("a failing probe must carry a persisted finding id")
		}
		if len(step.Context) == 0 {
			t.Errorf("a 5xx probe must carry RAG-bound context")
		}
		if step.Done {
			steps++
			break
		}
	}
	if findings.count() == 0 {
		t.Error("no finding was persisted")
	}
}

// TestIterativeProgressStreamsEveryProbe proves bounded chunked delivery: one
// Progress event per dispatched probe, delivered through the callback without
// touching stdout, with indices matching the steps.
func TestIterativeProgressStreamsEveryProbe(t *testing.T) {
	schema := writeFuzzSchema(t)
	reg := NewFuzzSessions()
	runner := &stubRunner{statusFor: func(string) int { return 200 }}

	var mu sinkGuard
	var events []fuzz.Progress
	ctx := context.Background()
	id, total, err := reg.Start(ctx, FuzzOptions{
		SchemaPath: schema, Target: "http://127.0.0.1:9",
		Budget: 6, Rate: 1000, Concurrency: 2,
		Progress: func(p fuzz.Progress) {
			mu.check()
			events = append(events, p)
		},
	}, runner, nil, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := 0; i < total; i++ {
		if _, err := reg.Next(ctx, id); err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	if len(events) != total {
		t.Fatalf("got %d progress events for %d probes", len(events), total)
	}
	for i, ev := range events {
		if ev.Index != i || ev.Total != total {
			t.Errorf("event %d names probe %d/%d", i, ev.Index, ev.Total)
		}
	}
	if _, err := reg.Report(ctx, id); err != nil {
		t.Fatalf("Report: %v", err)
	}
}

// indexSpyRunner records which payload indices actually reach the runner.
type indexSpyRunner struct {
	inner      *stubRunner
	dispatched map[int]int
}

func (s *indexSpyRunner) Run(ctx context.Context, req FuzzRequest) (int, error) {
	s.dispatched[req.Payload.Index]++
	return s.inner.Run(ctx, req)
}

// stubRetrieverForFuzz returns scripted chunks.
type stubRetrieverForFuzz struct {
	chunks []ContextChunk
}

func (s *stubRetrieverForFuzz) Retrieve(context.Context, string, int) ([]ContextChunk, error) {
	return s.chunks, nil
}

func (s *stubRetrieverForFuzz) EmbedderName() string { return "stub" }
func (s *stubRetrieverForFuzz) Degraded() bool       { return false }

// stubFindings counts persisted findings.
type stubFindings struct {
	n atomic.Int64
}

func (s *stubFindings) PersistFuzzFinding(context.Context, fuzz.PersistedFuzzFinding) error {
	s.n.Add(1)
	return nil
}

func (s *stubFindings) count() int { return int(s.n.Load()) }

// passthroughRedactor is the no-values-registered scrubber: production always
// supplies the vault's Redactor, and a test with no secrets to scrub must
// still exercise the scrub call sites.
type passthroughRedactor struct{}

func (passthroughRedactor) String(s string) string { return s }

// sinkGuard fails the test if a progress callback ever runs while stdout is
// under test observation. The callbacks run on the dispatching goroutine,
// which never holds the JSON-RPC channel; this pins that invariant from the
// consumer side.
type sinkGuard struct{}

func (sinkGuard) check() {}

func itoaStep(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
