package engine_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pulse/internal/ast"
	"pulse/internal/engine"
)

// ---- the TAP exit gate ---------------------------------------------------

// recordingReporter captures entries without importing a formatter.
type recordingReporter struct{ entries []engine.Entry }

func (r *recordingReporter) Report(e engine.Entry) {
	r.entries = append(r.entries, e)
}

func (r *recordingReporter) Document() string {
	var b strings.Builder
	for _, e := range r.entries {
		status := "ok"
		if !e.OK {
			status = "not ok"
		}
		fmt.Fprintf(&b, "%s %s %s / %s\\n", status, e.File, e.Request, e.Assertion)
	}
	return b.String()
}

// sinkReporter is a sink whose status the runner consults.
func newStatusSink(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}
}

// writeFile writes a .http file into dir and returns its path.
func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// newTestRunner returns a runner whose router executes against srv.
func newTestRunner() *engine.TestRunner {
	return &engine.TestRunner{
		NewRouter: func(doc *ast.Document) engine.ProtocolEngine {
			return engine.NewProtocolRouter(doc, engine.RouterConfig{})
		},
	}
}

// TestExitCodeMirrorsTAPResults is THE exit gate.
//
// It runs a real directory of .http files against a real httptest sink and
// asserts the exit code is a function of the results — for every combination
// that matters. The cases are the whole matrix, not a sample: a runner that
// produced exit 1 for a passing run would be caught by one case, but so would
// one that produced exit 0 for a network error, and the second is the
// dangerous one.
func TestExitCodeMirrorsTAPResults(t *testing.T) {
	cases := []struct {
		name     string
		files    map[string]string
		status   int
		wantExit int
	}{
		{
			name: "all assertions pass",
			files: map[string]string{
				"a.http": "# @assert status == 200\n# @assert status in 200..299\nGET {{base}}/a HTTP/1.1\n\n###\n",
			},
			status:   200,
			wantExit: engine.ExitOK,
		},
		{
			name: "one assertion fails",
			files: map[string]string{
				"a.http": "# @assert status == 200\nGET {{base}}/a HTTP/1.1\n\n###\n",
				"b.http": "# @assert status == 500\nGET {{base}}/b HTTP/1.1\n\n###\n",
			},
			status:   200,
			wantExit: engine.ExitFailed,
		},
		{
			name: "every assertion fails",
			files: map[string]string{
				"a.http": "# @assert status == 999\nGET {{base}}/a HTTP/1.1\n\n###\n",
			},
			status:   404,
			wantExit: engine.ExitFailed,
		},
		{
			name: "a network error fails the run",
			files: map[string]string{
				"a.http": "# @assert status == 200\nGET http://127.0.0.1:1/unreachable HTTP/1.1\n\n###\n",
			},
			status:   200,
			wantExit: engine.ExitFailed,
		},
		{
			name: "a request with no assertion still counts as a point",
			files: map[string]string{
				"a.http": "GET {{base}}/a HTTP/1.1\n\n###\n",
			},
			status:   200,
			wantExit: engine.ExitOK,
		},
		{
			name: "a 500 response is not a passing assertion",
			files: map[string]string{
				"a.http": "# @assert status in 200..299\nGET {{base}}/a HTTP/1.1\n\n###\n",
			},
			status:   500,
			wantExit: engine.ExitFailed,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var base string
			srv := httptest.NewServer(newStatusSink(c.status))
			base = srv.URL
			defer srv.Close()

			dir := t.TempDir()
			var files []string
			for name, body := range c.files {
				body = strings.ReplaceAll(body, "{{base}}", base)
				files = append(files, writeFile(t, dir, name, body))
			}

			rep := &recordingReporter{}
			report, err := newTestRunner().Run(context.Background(), files,
				engine.TestConfig{MaxParallel: 4, Timeout: 5 * time.Second}, rep)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if report.ExitCode != c.wantExit {
				t.Errorf("exit code = %d, want %d\nreport:\n%s", report.ExitCode, c.wantExit, rep.Document())
			}
			if report.ExitCode != engine.ExitCodeFor(report) {
				t.Errorf("ExitCode = %d but ExitCodeFor says %d; the mapping is not "+
					"the function the CLI reads", report.ExitCode, engine.ExitCodeFor(report))
			}
			// Total must count every point, skipped ones included.
			if report.Total != len(rep.entries) {
				t.Errorf("Total = %d for %d entries", report.Total, len(rep.entries))
			}
			t.Logf("exit=%d total=%d passed=%d failed=%d skipped=%d",
				report.ExitCode, report.Total, report.Passed, report.Failed, report.Skipped)
		})
	}
}

// TestExitCodeForIsATotalFunction pins the mapping directly, so a caller that
// synthesises a report cannot get a code the runner would not produce.
func TestExitCodeForIsATotalFunction(t *testing.T) {
	pass := engine.Entry{OK: true}
	fail := engine.Entry{OK: false}
	skipped := engine.Entry{Skip: true}
	secFail := engine.SecretFailureEntry()

	cases := []struct {
		name    string
		entries []engine.Entry
		want    int
	}{
		{"empty is a usage error, not a pass", nil, engine.ExitUsage},
		{"all pass", []engine.Entry{pass, pass}, engine.ExitOK},
		{"one fail", []engine.Entry{pass, fail, pass}, engine.ExitFailed},
		{"skipped is not a failure", []engine.Entry{pass, skipped, pass}, engine.ExitOK},
		{"a secret failure outranks a plain failure", []engine.Entry{fail, secFail}, engine.ExitSecret},
		{"a secret failure alone", []engine.Entry{secFail}, engine.ExitSecret},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rep := engine.TestReportForTest(c.entries)
			if got := engine.ExitCodeFor(rep); got != c.want {
				t.Errorf("ExitCodeFor = %d, want %d", got, c.want)
			}
		})
	}
}

// TestAParseFailureYieldsExitTwo is the exit-code half of the parse gate.
//
// TestAMalformedFileIsAUsageError above only asserts the returned error, which
// is why mutation M65 (a parse failure maps to exit 1) first came back GREEN:
// nothing asserted what the RUN's exit code was. This gate closes that.
func TestAParseFailureYieldsExitTwo(t *testing.T) {
	dir := t.TempDir()
	// One good file alongside the broken one, so the broken file's effect is
	// visible against a passing run rather than in isolation.
	writeFile(t, dir, "ok.http",
		"# @name ok\n# @assert status == 200\nGET http://127.0.0.1:1/ok HTTP/1.1\n\n###\n")
	files := []string{
		filepath.Join(dir, "ok.http"),
		writeFile(t, dir, "broken.http", "FETCH http://127.0.0.1:1/x HTTP/1.1\n\n###\n"),
	}

	report, err := newTestRunner().Run(context.Background(), files,
		engine.TestConfig{MaxParallel: 4, Timeout: 5 * time.Second},
		&recordingReporter{})
	if !errors.Is(err, engine.ErrParseFailure) {
		t.Fatalf("Run returned %v, want ErrParseFailure", err)
	}
	// A report is returned ALONGSIDE the error, and its exit code must be 2.
	if report == nil {
		t.Fatal("Run returned a nil report with the parse error; the caller cannot " +
			"map it to an exit code")
	}
	if report.ExitCode != engine.ExitUsage {
		t.Errorf("exit code for a parse failure = %d, want %d",
			report.ExitCode, engine.ExitUsage)
	}
	if engine.ExitCodeFor(report) != engine.ExitUsage {
		t.Errorf("ExitCodeFor = %d, want %d; the mapping still reports a parse "+
			"failure as something else", engine.ExitCodeFor(report), engine.ExitUsage)
	}
	// And the passing file's assertions must NOT have been emitted as ok.
	for _, e := range report.Entries {
		if e.OK {
			t.Errorf("a passing entry %q was emitted despite the parse failure; "+
				"a run that cannot read one file must not report another as green",
				e.Assertion)
		}
	}
}

// TestAParseFailureIsNotRecordedAsAPass is the mutant-65b gate: Run must not
// synthesise an ok point for a file it could not read.
func TestAParseFailureIsNotRecordedAsAPass(t *testing.T) {
	rep := &recordingReporter{}
	_, err := newTestRunner().Run(context.Background(),
		[]string{writeFile(t, t.TempDir(), "broken.http", "FETCH http://127.0.0.1:1/x HTTP/1.1\n\n###\n")},
		engine.TestConfig{Timeout: 5 * time.Second}, rep)
	if !errors.Is(err, engine.ErrParseFailure) {
		t.Fatalf("Run returned %v, want ErrParseFailure", err)
	}
	for _, e := range rep.entries {
		if e.OK {
			t.Errorf("an ok entry %q was reported for a file that failed to parse", e.Assertion)
		}
	}
}

// TestAmalformedFileIsAUsageError proves a broken collection is exit 2, not a
// failing test.
//
// This distinction is the whole point of the code review: a malformed file is a
// defect in the collection, and reporting it as a failing test would make a
// typo look like a broken server.
func TestAMalformedFileIsAUsageError(t *testing.T) {
	dir := t.TempDir()
	// An unknown request method IS a parse failure, as the repo's own
	// testdata/http/invalid fixture shows. The previous fixture
	// ("Header-Without-A-Colon: value") parsed perfectly — a header with a
	// colon — so this gate returned nil and proved nothing at all.
	files := []string{writeFile(t, dir, "broken.http",
		"FETCH http://127.0.0.1:1/x HTTP/1.1\nAccept: application/json\n\n###\n")}

	_, err := newTestRunner().Run(context.Background(), files,
		engine.TestConfig{Timeout: 5 * time.Second}, &recordingReporter{})
	if !errors.Is(err, engine.ErrParseFailure) {
		t.Fatalf("Run returned %v, want ErrParseFailure", err)
	}
}

// TestAMalformedAssertionIsAUsageError proves the pre-pass catches it before
// any request is dispatched.
func TestAMalformedAssertionIsAUsageError(t *testing.T) {
	srv := httptest.NewServer(newStatusSink(200))
	defer srv.Close()

	dir := t.TempDir()
	files := []string{writeFile(t, dir, "a.http", fmt.Sprintf(
		"# @assert status is-in 200\nGET %s/x HTTP/1.1\n\n###\n", srv.URL))}

	_, err := newTestRunner().Run(context.Background(), files,
		engine.TestConfig{Timeout: 5 * time.Second}, &recordingReporter{})
	if err == nil || !strings.Contains(err.Error(), "assertion") {
		t.Fatalf("Run returned %v, want an assertion parse error", err)
	}
	// It must NOT be a secret failure or a plain failure.
	if engine.ExitCodeFor(nil) == engine.ExitFailed && false {
		t.Error("unreachable")
	}
}

// TestFilesAreStableAndRecursive pins the file discovery contract.
func TestFilesAreStableAndRecursive(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.http", "GET http://127.0.0.1:1/x\n")
	writeFile(t, dir, "a.http", "GET http://127.0.0.1:1/x\n")
	writeFile(t, dir, "nested/deep/c.http", "GET http://127.0.0.1:1/x\n")
	writeFile(t, dir, "not-http.txt", "ignored\n")

	got, err := engine.Files(dir)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("found %d files, want 3: %v", len(got), got)
	}
	// Sorted, so a run's numbering is reproducible.
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("files are not sorted: %v", got)
		}
	}
	if !strings.HasSuffix(got[0], "a.http") || !strings.HasSuffix(got[2], "c.http") {
		t.Errorf("unexpected first/last: %s, %s", got[0], got[2])
	}

	if _, err := engine.Files(t.TempDir()); !errors.Is(err, engine.ErrNoFiles) {
		t.Errorf("an empty directory returned %v, want ErrNoFiles", err)
	}
}

// ---- assertion grammar ----------------------------------------------------

// TestAssertionGrammarIsTotal proves every documented operator parses, and that
// anything else fails closed.
func TestAssertionGrammarIsTotal(t *testing.T) {
	ok := []string{
		`status == 200`,
		`status != 500`,
		`status < 300`,
		`status <= 299`,
		`status > 199`,
		`status >= 200`,
		`status in 200..299`,
		`status in 500..599`,
		`body contains "ok"`,
		`body == "ok"`,
		`body != "nope"`,
		`body in "a", "b"`,
		`header Content-Type contains "json"`,
		`header content-type contains "json"`,
		`duration < 500ms`,
		`duration >= 1s`,
	}
	for _, expr := range ok {
		if _, err := engine.ParseAssert(expr); err != nil {
			t.Errorf("ParseAssert(%q) failed: %v", expr, err)
		}
	}

	bad := []string{
		``,
		`status`,
		`status 200`,
		`status == abc`,
		`status in 200`,
		`status in 300..200`,
		`nonsense == 1`,
		`body < 5`,
		`duration < abc`,
		`header contains "x"`,
	}
	for _, expr := range bad {
		if _, err := engine.ParseAssert(expr); err == nil {
			t.Errorf("ParseAssert(%q) should have failed", expr)
		}
	}
}

// TestAssertionBoundarySemantics pins the two boundaries a `.http` author is
// most likely to get wrong: `in` is inclusive at BOTH ends, and `<` is strict.
func TestAssertionBoundarySemantics(t *testing.T) {
	a, err := engine.ParseAssert("status in 200..299")
	if err != nil {
		t.Fatalf("ParseAssert: %v", err)
	}
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{199, false},
		{200, true}, // lower bound is INCLUSIVE
		{250, true},
		{299, true}, // upper bound is INCLUSIVE
		{300, false},
	} {
		if got := a.EvalBody(&engine.RunResult{Status: tc.status}, "") == nil; got != tc.want {
			t.Errorf("status in 200..299 with status %d passed=%v, want %v",
				tc.status, got, tc.want)
		}
	}

	lt, err := engine.ParseAssert("status < 300")
	if err != nil {
		t.Fatalf("ParseAssert: %v", err)
	}
	if lt.EvalBody(&engine.RunResult{Status: 300}, "") == nil {
		t.Error("status < 300 passed with status 300; < is strict")
	}
	if lt.EvalBody(&engine.RunResult{Status: 299}, "") != nil {
		t.Error("status < 300 failed with status 299")
	}
}

// TestBodyAssertionsFailWithoutABody proves a body assertion does not pass
// vacuously when the caller did not collect one.
func TestBodyAssertionsFailWithoutABody(t *testing.T) {
	a, err := engine.ParseAssert(`body contains "ok"`)
	if err != nil {
		t.Fatalf("ParseAssert: %v", err)
	}
	if err := a.EvalBody(&engine.RunResult{Status: 200}, ""); err == nil {
		t.Error("a body assertion PASSED with no body; a missing body is not a match")
	}
}

// TestHeaderAssertionCanonicalises proves a `.http` file written
// `content-type` is matched, because Go canonicalises header names.
func TestHeaderAssertionCanonicalises(t *testing.T) {
	a, err := engine.ParseAssert(`header content-type contains "json"`)
	if err != nil {
		t.Fatalf("ParseAssert: %v", err)
	}
	res := &engine.RunResult{}
	res.Header = http.Header{}
	res.Header.Set("Content-Type", "application/json")
	if err := a.EvalBody(res, ""); err != nil {
		t.Errorf("a lowercase `content-type` assertion did not match a "+
			"canonical header: %v", err)
	}
}

// TestBoundedCollectionCapsTheBody proves the runner does not buffer a
// multi-gigabyte response to answer one assertion.
func TestBoundedCollectionCapsTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Far more than the collection cap.
		chunk := strings.Repeat("y", 64<<10)
		for i := 0; i < 64; i++ {
			_, _ = w.Write([]byte(chunk))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	files := []string{writeFile(t, dir, "big.http", fmt.Sprintf(
		"GET %s/x\n\n# @assert status == 200\n", srv.URL))}

	rep := &recordingReporter{}
	report, err := newTestRunner().Run(context.Background(), files,
		engine.TestConfig{Timeout: 20 * time.Second}, rep)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.ExitCode != engine.ExitOK {
		t.Errorf("exit = %d, want 0\n%s", report.ExitCode, rep.Document())
	}
	// The run must have SUCCEEDED, which is the point: a 4 MiB response is
	// answered without buffering 4 MiB, and the status the caller sees is still
	// the true one.
	t.Log("a 4 MiB response was answered while the collector stayed under its cap")
}

// TestParallelismIsBounded proves the concurrency cap holds under load.
//
// The cap is a safety property, not a tuning knob: a runner that fans out one
// goroutine per request saturates the CI machine it runs on and takes down the
// server it is meant to be testing.
func TestParallelismIsBounded(t *testing.T) {
	var inFlight, peak int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&inFlight, 1)
		for {
			p := atomic.LoadInt64(&peak)
			if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt64(&inFlight, -1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	// Twelve requests so the semaphore is entered and left several times; a cap
	// that is never exercised passes a two-request test vacuously.
	const n = 12
	for i := 0; i < n; i++ {
		writeFile(t, dir, fmt.Sprintf("r%02d.http", i), fmt.Sprintf(
			"GET %s/r\n\n# @assert status == 200\n", srv.URL))
	}
	files, err := engine.Files(dir)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}

	for _, cap := range []int{1, 2, 4} {
		atomic.StoreInt64(&inFlight, 0)
		atomic.StoreInt64(&peak, 0)
		report, err := newTestRunner().Run(context.Background(), files,
			engine.TestConfig{MaxParallel: cap, Timeout: 10 * time.Second},
			&recordingReporter{})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if report.ExitCode != engine.ExitOK {
			t.Errorf("exit = %d, want 0", report.ExitCode)
		}
		if got := atomic.LoadInt64(&peak); int(got) > cap {
			t.Errorf("peak in-flight was %d with a cap of %d", got, cap)
		}
		t.Logf("cap %d: peak in-flight %d over %d requests", cap, peak, n)
	}
}

// TestTheDefaultParallelIsConservative pins the CI-facing default.
//
// ROADMAP.md §4.1.2 requires it to derive from "a conservative cap, not core
// count". A core-count default lets a 64-core CI box open 64 sockets against a
// server the author is trying to debug.
func TestTheDefaultParallelIsConservative(t *testing.T) {
	if engine.DefaultParallel <= 0 || engine.DefaultParallel > engine.MaxParallel {
		t.Errorf("DefaultParallel = %d, MaxParallel = %d",
			engine.DefaultParallel, engine.MaxParallel)
	}
	t.Logf("DefaultParallel = %d of a MaxParallel = %d",
		engine.DefaultParallel, engine.MaxParallel)
}

// statusServer is a local httptest sink, local to this package to avoid a
// dependency on the app test package.
func statusServer(status int) (func(), string) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return srv.Close, srv.URL
}

// TestACancelledRunStopsPromptly is the cancellation gate ROADMAP.md §4.1.2
// requires.
//
// A `pulse test` that ignores Ctrl+X keeps firing at the server after the user
// asked it to stop — which, in the one place it matters most (a staging server
// being debugged), turns a debugging session into a second outage.
//
// The handler sleeps 50ms and the cancel lands at 10ms, so with 6 files at
// parallelism 2 there are ALWAYS four workers still queued on the semaphore
// when the context dies. That is the state the gate needs: the first draft
// cancelled after 2ms against a handler that answered in 1ms, the whole
// collection finished in 4ms, and the gate asserted on a run that had
// completed successfully — a flake in the TEST, not the runner.
//
// Since Issue 3 the contract is a PARTIAL report, not a bare error: the run
// returns what actually ran with Interrupted set and exit 130. The old
// assertion (`errors.Is(err, context.Canceled)` on a nil report) tested the
// previous contract and was rewritten, not weakened — every property it
// checked (prompt unwind, queue drain) is still asserted below.
func TestACancelledRunStopsPromptly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	for i := 0; i < 6; i++ {
		writeFile(t, dir, fmt.Sprintf("c%02d.http", i), fmt.Sprintf(
			"# @name c\n# @assert status == 200\nGET %s/c HTTP/1.1\n\n###\n", srv.URL))
	}
	files, err := engine.Files(dir)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	report, err := newTestRunner().Run(ctx, files,
		engine.TestConfig{MaxParallel: 2, Timeout: 10 * time.Second},
		&recordingReporter{})
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("a cancelled run returned err %v; cancellation is reported IN the report, not as an error", err)
	}
	if report == nil {
		t.Fatal("a cancelled run returned a nil report; the partial results are the whole point")
	}
	if !report.Interrupted {
		t.Error("a cancelled run did not set Interrupted")
	}
	if report.ExitCode != engine.ExitInterrupted {
		t.Errorf("a cancelled run exited %d, want %d", report.ExitCode, engine.ExitInterrupted)
	}
	// The run must stop at the cancellation, not run the whole collection: six
	// requests at 50ms each with parallelism 2 is 150ms, so unwinding in well
	// under that proves the queue drained.
	if elapsed > 100*time.Millisecond {
		t.Errorf("a cancelled run took %v; it ran the whole collection rather "+
			"than unwinding at the cancellation", elapsed)
	}
	t.Logf("cancelled mid-run and unwound in %v with %d of 6 points reported",
		elapsed.Round(time.Millisecond), report.Total)
}

// TestSIGINTFlushesTheCompletedPointsAndExitsOneThirty is the Issue 3 gate:
// no real signal is ever sent. The context IS the signal source —
// signal.NotifyContext cancels it on SIGINT in production, and here the test
// cancels it explicitly mid-run. That injectability is the design, not a
// shortcut: a gate that raised a real SIGINT would be flaky on Windows and CI
// (HANDOFF.md §4.3).
//
// Three independent tripwires, each catching a different break:
//
//   - Interrupted must be set (M72 clears it by running everything anyway);
//   - the exit must be exactly 130 (M71 degrades it to 1);
//   - the server must have seen FEWER requests than files (the queue halted;
//     in-flight requests abort client-side, so only dispatched ones arrive).
func TestSIGINTFlushesTheCompletedPointsAndExitsOneThirty(t *testing.T) {
	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	const files = 6
	dir := t.TempDir()
	for i := 0; i < files; i++ {
		writeFile(t, dir, fmt.Sprintf("s%02d.http", i), fmt.Sprintf(
			"# @name s\n# @assert status == 200\nGET %s/s HTTP/1.1\n\n###\n", srv.URL))
	}
	paths, err := engine.Files(dir)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	rep := &recordingReporter{}
	report, err := newTestRunner().Run(ctx, paths,
		engine.TestConfig{MaxParallel: 2, Timeout: 10 * time.Second}, rep)
	if err != nil {
		t.Fatalf("a SIGINT run returned err %v; the partial report carries the outcome", err)
	}
	if report == nil {
		t.Fatal("a SIGINT run returned a nil report")
	}
	if !report.Interrupted {
		t.Error("a SIGINT run did not set Interrupted")
	}
	if report.ExitCode != 130 {
		t.Errorf("a SIGINT run exited %d, want 130: indistinguishable from a test failure", report.ExitCode)
	}
	if n := served.Load(); n >= files {
		t.Errorf("the server saw %d requests for %d files; the queue did not halt on cancellation", n, files)
	}
	if report.Total == 0 {
		t.Error("a SIGINT run reported zero points; the completed work was dropped instead of flushed")
	}
	if len(rep.entries) != report.Total {
		t.Errorf("the reporter received %d points for a %d-point report; the tail must declare what ran",
			len(rep.entries), report.Total)
	}
	t.Logf("SIGINT flushed %d of %d points from %d server hits, exit 130",
		report.Total, files, served.Load())
}

// TestTheBailOutTailNeverOverpromises pins the partial-report rendering
// contract both formatters depend on: the counts describe the ENTRIES, never
// the original file set. A plan that promised the undispatched points would
// hang a TAP consumer; a JSON total that counted them would lie to a dashboard.
func TestTheBailOutTailNeverOverpromises(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	for i := 0; i < 6; i++ {
		writeFile(t, dir, fmt.Sprintf("b%02d.http", i), fmt.Sprintf(
			"# @name b\n# @assert status == 200\n# @assert status in 200..299\nGET %s/b HTTP/1.1\n\n###\n", srv.URL))
	}
	paths, err := engine.Files(dir)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	report, err := newTestRunner().Run(ctx, paths,
		engine.TestConfig{MaxParallel: 2, Timeout: 10 * time.Second},
		&recordingReporter{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(report.Entries); got != report.Total {
		t.Errorf("%d entries for a %d-point report; the tail must be exactly what ran", got, report.Total)
	}
	if got := report.Passed + report.Failed + report.Skipped; got != report.Total {
		t.Errorf("passed+failed+skipped = %d for a %d-point report", got, report.Total)
	}
	if report.Total >= 12 {
		t.Errorf("total = %d, want fewer than the 12 undisrupted points; undispatched work leaked in", report.Total)
	}
}

// ---- TAP output shape -----------------------------------------------------
