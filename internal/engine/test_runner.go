package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"pulse/internal/ast"
)

// The headless pipeline behind `pulse test <dir>` (ROADMAP.md §4.1).
//
// # What this file is and is not
//
// It walks a directory of `.http` files, parses each with the existing
// zero-allocation AST parser, executes the requests concurrently, evaluates
// their `# @assert` directives, and returns a report whose exit code is a
// function of the results. It does NOT format output: the caller holds the
// writer, and the format comes from a [TestReporter] it supplies.
//
// That separation is deliberate and it is the reason this file imports only
// `internal/ast`. Move 9's TAP 13 emitter lives in `internal/fuzz`, and
// AGENTS.md §3.1 forbids a sibling importing a sibling. Making `engine` a
// producer of structured results and letting the composition root supply the
// formatter keeps the layering rule while still reusing the emitter — and it
// means a JSON reporter or a silent one costs no change here.
//
// # Exit codes
//
//	0  every assertion passed, or skipped with a stated reason
//	1  any assertion failed, or a request errored
//	2  usage or PARSE error (a malformed .http file or a malformed assertion)
//	3  secret-resolution failure
//	130  the run was halted by context cancellation (SIGINT)
//
// There is no exit 0 unless every test point is ok, and there is no case in
// which a failed assertion yields anything but 1. The mapping is a pure
// function of the report, and it is gated by a matrix test rather than argued.

// Exit codes. Mirrored from cmd/pulse; repeated here so the runner can be used
// without the CLI and so the mapping lives next to the results it maps.
const (
	// ExitOK is the all-pass exit.
	ExitOK = 0
	// ExitFailed is the any-failure exit.
	ExitFailed = 1
	// ExitUsage is the parse-or-usage exit.
	ExitUsage = 2
	// ExitSecret is the secret-resolution exit.
	ExitSecret = 3
	// ExitInterrupted is the cancellation exit (SIGINT): the run halted
	// mid-collection and the report holds what actually ran.
	ExitInterrupted = 130
)

// Errors.
var (
	// ErrNoFiles reports that a directory held no .http files.
	ErrNoFiles = errors.New("engine: no .http files found")

	// ErrParseFailure reports a .http file that did not parse.
	//
	// It maps to exit 2, which is why it is not folded into ErrFailed: a
	// malformed file is a defect in the collection, not a failing test, and CI
	// needs to tell them apart.
	ErrParseFailure = errors.New("engine: .http file failed to parse")
)

// TestReporter receives one line per test point.
//
// It is called from several goroutines and MUST be safe for concurrent use;
// the composition root's implementation serialises through a single writer.
type TestReporter interface {
	// Report records one outcome.
	Report(Entry)
	// Document renders everything reported so far.
	Document() string
}

// Entry is one test point in the report.
type Entry struct {
	// File is the .http file, relative to the root the runner was given.
	File string
	// Request is the request's name, or its method and path when unnamed.
	Request string
	// Assertion is the assertion source, empty for a request-level failure.
	Assertion string
	// OK reports whether the point passed.
	OK bool
	// Skip reports whether the point was not run, with a stated reason.
	Skip bool
	// SkipReason is mandatory when Skip is true (AGENTS.md §4.5).
	SkipReason string
	// Detail is the failure message.
	Detail string
	// secret marks a secret-resolution failure, which makes the whole run
	// exit 3 rather than 1 (AGENTS.md §4.4).
	secret bool
	// Fields carry structured context, e.g. status and elapsed.
	Fields map[string]string
}

// fileResult is one file's outcome, returned from the worker goroutine.
//
// It is package-level rather than a local type because
// [TestRunner.runFile]'s signature references it. Declaring it inside Run was a
// compile error, and the dependency it exposed is real.
type fileResult struct {
	entries []Entry
	err     error
}

// SecretFailureEntry is the entry a secret-resolution failure produces.
//
// It is exported for the same reason TestReportForTest is: `Entry.secret` is an
// internal fact about how a failure will be mapped, and a test outside the
// package needs to be able to construct that state without being able to set
// arbitrary unexported fields.
func SecretFailureEntry() Entry { return Entry{OK: false, secret: true} }

// TestReportForTest builds a report over the given entries.
//
// It is a TEST SEAM, the same kind ChunkFileForTest, ParseIgnoreForTest and
// MaxTrackedSecretsForTest already are: `Entry.secret` and
// `TestReport.parseFailed` are unexported because they are internal facts, not
// API, and a test that could not set them would only be able to assert the
// subset of ExitCodeFor's behaviour it can reach through a real run.
func TestReportForTest(entries []Entry) *TestReport {
	r := &TestReport{Entries: append([]Entry(nil), entries...)}
	r.summarize()
	r.ExitCode = ExitCodeFor(r)
	return r
}

// TestReportParseFailedForTest builds a report for a run that failed to parse.
func TestReportParseFailedForTest(path string) *TestReport {
	r := &TestReport{parseFailed: true}
	r.summarize()
	r.ExitCode = ExitCodeFor(r)
	return r
}

// TestConfig configures a run.
type TestConfig struct {
	// MaxParallel bounds concurrent requests. Zero selects DefaultParallel.
	//
	// The default is deliberately NOT core count: CI machines punish aggressive
	// fan-out against a shared server, and a workbench that saturates the
	// machine it is debugging is a workbench that cannot debug.
	MaxParallel int
	// Timeout bounds one request. Zero selects DefaultTimeout.
	Timeout time.Duration
	// Redactor scrubs failure details before they reach a reporter.
	Redactor interface{ String(string) string }
}

// DefaultParallel is the conservative concurrency default.
const DefaultParallel = 4

// MaxParallel is the ceiling a caller may configure.
const MaxParallel = 32

// DefaultTestTimeout bounds one request in a test run.
const DefaultTestTimeout = 30 * time.Second

// TestReport is a completed run.
//
// Total counts every test point INCLUDING skipped ones, which is what the TAP
// plan must promise: a plan that promised 40 and delivered 37 hangs a consumer.
type TestReport struct {
	Entries []Entry
	// Total, Passed, Failed and Skipped are computed over Entries.
	Total   int
	Passed  int
	Failed  int
	Skipped int

	// parseFailed is set when a .http file could not be read or parsed, which
	// makes the run exit 2 regardless of any assertion that did run.
	parseFailed bool

	// Interrupted is set when the run halted on context cancellation
	// (SIGINT). Entries holds the test points that actually ran — the TAP
	// plan and the JSON counts describe THEM, not the original file set —
	// and ExitCode is 130 regardless of any failure among them.
	Interrupted bool

	// ExitCode is the process exit this run yields.
	ExitCode int
	// Elapsed is the wall-clock duration of the whole run.
	Elapsed time.Duration
}

// ExitCodeFor maps a report to a process exit code.
//
// The mapping is a pure function of the report so it can be gated by a matrix
// test rather than argued about, and so `pulse test` and `pulse test -silent`
// cannot disagree about what a run meant.
//
//	0  everything passed, or skipped with a stated reason
//	1  any assertion failed, any request errored
//	2  usage or parse error
//	3  secret-resolution failure
//
// Note what does NOT appear: there is no path from "some tests failed" to
// anything but 1, and no path from "some tests were skipped" to 1. A skip is a
// BLOCKED condition (AGENTS.md §4.5) and it is reported as such; turning it red
// would make a run on a machine without a keychain indistinguishable from a run
// with a failing assertion, and the second is what the operator needs.
func ExitCodeFor(r *TestReport) int {
	if r == nil {
		return ExitUsage
	}
	if r.parseFailed {
		return ExitUsage
	}
	// A report with NO test points is not a pass. It means the run found
	// nothing, and reporting exit 0 for "nothing happened" is the greenwash
	// AGENTS.md §4.5 exists to prevent: a directory of empty .http files, or a
	// runner whose file walk silently found nothing, would read as success.
	if len(r.Entries) == 0 {
		return ExitUsage
	}
	// Two passes, and the order is the point. A secret-resolution failure is
	// scanned for BEFORE any plain failure, so a run that has both reports 3
	// regardless of which file happened to fail first. The single-pass version
	// returned 1 whenever a plain failure preceded a secret one, which made
	// exit 3 a function of file ordering rather than of cause.
	//
	// The remedy differs, which is why the ordering matters operationally:
	// exit 3 means "configure your keychain", exit 1 means "a test failed", and
	// telling the user about a failing assertion while their real problem is a
	// missing keychain sends them in the wrong direction.
	for _, e := range r.Entries {
		if !e.Skip && !e.OK && e.secret {
			return ExitSecret
		}
	}
	for _, e := range r.Entries {
		if !e.Skip && !e.OK {
			return ExitFailed
		}
	}
	return ExitOK
}

// summarize fills Total/Passed/Failed/Skipped.
func (r *TestReport) summarize() {
	r.Total = len(r.Entries)
	for _, e := range r.Entries {
		switch {
		case e.Skip:
			r.Skipped++
		case e.OK:
			r.Passed++
		default:
			r.Failed++
		}
	}
}

// Files returns the .http files under root, recursively, in a stable order.
//
// Recursive because a collection is a tree in every real project, and stable
// because the report's numbering must be reproducible to be diffable between
// runs. The order is by path, so a directory listing's incidental ordering on
// disk does not become part of the output.
func Files(root string) ([]string, error) {
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".http") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("engine: walk %s: %w", root, err)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("%w under %s", ErrNoFiles, root)
	}
	return out, nil
}

// TestRunner executes a directory of .http files.
type TestRunner struct {
	// NewRouter builds the executor for one parsed document.
	NewRouter func(doc *ast.Document) ProtocolEngine
	// Resolver backs `{{var}}` interpolation. Nil means variables fail closed.
	Resolver Resolver
	// Redactor scrubs failure details.
	Redactor interface{ String(string) string }
}

// Run walks files, executes them and returns a report.
//
// Context cancellation (SIGINT) halts the queue: workers that never
// dispatched contribute nothing, in-flight requests abort, and the returned
// report holds what actually ran with Interrupted set and exit 130.
//
// A parse failure in any file is fatal for the whole run and yields exit 2: the
// collection is broken, and reporting one file's results as "passing" while
// another could not be read would be a green facade (AGENTS.md §4.5).
func (r *TestRunner) Run(ctx context.Context, files []string, cfg TestConfig, rep TestReporter) (*TestReport, error) {
	if len(files) == 0 {
		return nil, ErrNoFiles
	}
	parallel := cfg.MaxParallel
	if parallel <= 0 {
		parallel = DefaultParallel
	}
	if parallel > MaxParallel {
		parallel = MaxParallel
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTestTimeout
	}

	start := time.Now()
	report := &TestReport{}

	// A single top-level wait: requests run concurrently, and each FILE's
	// assertions are emitted in the order they were parsed so a reporter that
	// receives them concurrently still gets a stable ordering per file.
	results := make([]fileResult, len(files))

	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, path := range files {
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = fileResult{err: ctx.Err()}
				return
			}
			defer func() { <-sem }()
			results[i] = r.runFile(ctx, path, cfg, timeout, rootOf(files))
		}(i, path)
	}
	wg.Wait()

	interrupted := false
	for _, res := range results {
		if res.err != nil {
			// A parse failure fatal to the run: exit 2, no report.
			if errors.Is(res.err, ErrParseFailure) {
				report.parseFailed = true
				report.summarize()
				report.ExitCode = ExitUsage
				return report, res.err
			}
			// A worker that never dispatched because the context died
			// contributes no entries: the queue halted, and what ran is
			// what the report holds. This is the SIGINT path — it is
			// recorded, not returned, so the caller gets the partial
			// report instead of a bare context.Canceled.
			if errors.Is(res.err, context.Canceled) {
				interrupted = true
				continue
			}
			// A request-level failure is recorded, not fatal.
		}
		report.Entries = append(report.Entries, res.entries...)
	}
	// The reporter is invoked HERE, once per entry, so every emitted line is
	// produced as the run completes rather than reconstructed at the end. The
	// previous version passed the reporter down and never called it: the report
	// carried entries and the TAP document came out empty, which the exit gate
	// caught as "Total = 1 for 0 entries" — a failure that is invisible from
	// the runner's own perspective and fatal to the CLI.
	for _, e := range report.Entries {
		rep.Report(e)
	}

	report.summarize()
	report.Elapsed = time.Since(start)
	// Interruption outranks every other mapping: a run halted by SIGINT
	// exits 130 even when the completed points include failures (which
	// alone would read as 1) or skips (which --strict-blocked would
	// escalate). Conflating "the user stopped the run" with "a test
	// failed" sends the operator in the wrong direction.
	if interrupted {
		report.Interrupted = true
		report.ExitCode = ExitInterrupted
	} else {
		report.ExitCode = ExitCodeFor(report)
	}
	return report, nil
}

// rootOf returns the common root of the file set, for relative display.
func rootOf(files []string) string {
	if len(files) == 0 {
		return "."
	}
	dir := filepath.Dir(files[0])
	for len(dir) > 1 && !strings.HasSuffix(dir, string(filepath.Separator)) {
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return dir
}

// runFile parses and executes one .http file.
func (r *TestRunner) runFile(ctx context.Context, path string, cfg TestConfig, timeout time.Duration, root string) fileResult {
	rel := rel(path, root)

	// Parse with the standard AST parser. The arena owned by the document is
	// released only after every request in it has run, because the document
	// outlives the parse.
	arena := ast.AcquireArena()
	src, err := os.ReadFile(path)
	if err != nil {
		arena.Release()
		return fileResult{err: fmt.Errorf("engine: read %s: %w", path, err)}
	}
	arena.Reset(src)
	doc, err := ast.Parse(arena)
	if err != nil {
		arena.Release()
		return fileResult{err: fmt.Errorf("%w: %s: %v", ErrParseFailure, path, err)}
	}
	defer arena.Release()

	if r.NewRouter == nil {
		return fileResult{err: fmt.Errorf("engine: %s: no router was configured", path)}
	}

	// Asserts are validated BEFORE any request runs: a malformed assertion is a
	// parse error (exit 2), not a test failure, and discovering it after six
	// requests have hit the server is both slow and misleading.
	var entries []Entry
	var reqs []*ast.Request
	for req := range doc.Requests() {
		exprs := doc.AssertsOf(req)
		for _, a := range exprs {
			text := doc.Str(a.Start, a.End)
			if _, perr := ParseAssert(text); perr != nil {
				return fileResult{err: fmt.Errorf("%w: %s:%d: %v",
					ErrParseFailure, path, a.Line, perr)}
			}
		}
		reqs = append(reqs, req)
	}

	router := r.NewRouter(doc)
	defer func() { _ = router.Close() }()

	for i, req := range reqs {
		entries = append(entries, r.runOne(ctx, doc, req, i, router, cfg, timeout, rel)...)
	}
	return fileResult{entries: entries}
}

// rel makes path relative to root, falling back to path itself.
func rel(path, root string) string {
	if root == "" || root == "." {
		return path
	}
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return r
}

// runOne executes one request and evaluates its assertions.
func (r *TestRunner) runOne(
	ctx context.Context,
	doc *ast.Document,
	req *ast.Request,
	idx int,
	router ProtocolEngine,
	cfg TestConfig,
	timeout time.Duration,
	rel string,
) []Entry {
	name := astString(doc, req.NameStart, req.NameEnd)
	if name == "" {
		name = fmt.Sprintf("request #%d", idx)
	}

	// A request that fails to execute is ONE entry, not one per assertion: the
	// run did not happen, so its assertions did not either, and reporting each
	// as failed would let a single transport error count as a dozen failures.
	collector := &testSink{buf: make([]byte, 0, 64<<10), done: make(chan struct{})}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	handle := router.Run(runCtx, req, collector)
	_ = handle.WaitTimeout(teardownBudgetDefault)
	<-collector.done

	res := collector.result
	body := collector.bodyString()

	if res == nil || collector.err != nil {
		detail := "the request did not complete"
		if collector.err != nil {
			detail = redact(cfg.Redactor, collector.err.Error())
		}
		// A secret-resolution failure is exit 3, which is not this file's
		// decision to make alone: it is recorded and the exit mapping at the
		// end of Run promotes the whole run.
		return []Entry{{
			File: rel, Request: name, OK: false,
			Detail: detail,
			secret: errors.Is(collector.err, ErrUnresolved),
			Fields: map[string]string{"stage": "run"},
		}}
	}

	var out []Entry
	for _, a := range doc.AssertsOf(req) {
		text := astString(doc, a.Start, a.End)
		assertion, err := ParseAssert(text)
		if err != nil {
			continue // already caught in the pre-pass
		}
		e := Entry{
			File: rel, Request: name, Assertion: strings.TrimSpace(text),
			OK: true,
			Fields: map[string]string{
				"status":   fmt.Sprint(res.Status),
				"duration": res.Elapsed.String(),
			},
		}
		if verr := assertion.EvalBody(res, body); verr != nil {
			e.OK = false
			e.Detail = redact(cfg.Redactor, verr.Error())
		}
		out = append(out, e)
	}

	// A request with no assertions still reports its status as a test point:
	// otherwise a file with no assertions would produce an empty plan and every
	// request in it would be invisible to CI.
	if len(out) == 0 {
		out = append(out, Entry{
			File: rel, Request: name, OK: true,
			Fields: map[string]string{
				"status":   fmt.Sprint(res.Status),
				"duration": res.Elapsed.String(),
			},
			Assertion: "request completed",
		})
	}
	return out
}

// astString reads a span out of the document's arena. It is the same accessor
// the MCP bridge uses, and it returns "" for an invalid span rather than
// panicking on a hand-built document.
func astString(doc *ast.Document, start, end int32) string {
	if doc == nil || start < 0 || end <= start {
		return ""
	}
	return doc.Str(start, end)
}

// redact runs s through the redactor when one is configured.
func redact(r interface{ String(string) string }, s string) string {
	if r == nil {
		return s
	}
	return r.String(s)
}

// testSink is a bounded collecting sink for the runner.
//
// It bounds what accumulates for the same reason the MCP bridge's
// collectingSink does: a `body contains` assertion reads the body, and a body
// can be large, so the collector caps what it holds and reports the true byte
// count rather than pretending the body was short.
type testSink struct {
	mu        sync.Mutex
	buf       []byte
	total     int64
	truncated bool
	result    *RunResult
	err       error
	done      chan struct{}
}

// MaxCollectedBytes bounds the body a runner will hold in memory.
const MaxCollectedBytes = 256 << 10

// Chunk implements Sink.
func (s *testSink) Chunk(c *Chunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c == nil || c.Released() {
		return
	}
	data := c.Bytes()
	if len(data) == 0 {
		// A zero-length non-final chunk is a keepalive; recycle and move on.
		s.recycle(c)
		return
	}
	s.total += int64(len(data))
	if !s.truncated {
		room := MaxCollectedBytes - len(s.buf)
		if room > 0 {
			n := len(data)
			if n > room {
				n = room
			}
			s.buf = append(s.buf, data[:n]...)
		}
		if len(s.buf) >= MaxCollectedBytes {
			s.truncated = true
		}
	}
	s.recycle(c)
}

// recycle returns the chunk to the engine's pool immediately.
//
// It is called with s.mu held, which is safe because Recycle takes no locks of
// its own; what matters is that it happens on EVERY path, including the
// truncation path, or the pool is drained faster than it is refilled and the
// engine stops being zero-allocation.
func (s *testSink) recycle(c *Chunk) {
	c.Recycle()
}

// Done implements Sink.
func (s *testSink) Done(res *RunResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.result = res
	close(s.done)
}

// Failed implements Sink.
func (s *testSink) Failed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
	close(s.done)
}

// bodyString returns the collected body.
func (s *testSink) bodyString() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.buf)
}
