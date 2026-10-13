package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pulse/internal/app"
	"pulse/internal/engine"
)

func statusServer(status int) (*httptest.Server, func()) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	return srv, srv.Close
}

func dirWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// TestTAPOutputConformsToThirteen is the TAP conformance gate.
//
// ROADMAP.md §4.1.3 requires TAP 13, and it requires it because a CI dashboard
// parses the document. Every rule asserted here is one whose violation breaks or
// misleads a parser, not one that is stylistic.
func TestTAPOutputConformsToThirteen(t *testing.T) {
	srv, done := statusServer(200)
	defer done()

	a := newTestApp(t)
	res, err := a.RunTest(context.Background(),
		dirWith(t, map[string]string{
			"pass.http": fmt.Sprintf("# @name p\n# @assert status == 200\nGET %s/p HTTP/1.1\n\n###\n", srv.URL),
			"fail.http": fmt.Sprintf("# @name f\n# @assert status == 500\nGET %s/f HTTP/1.1\n\n###\n", srv.URL),
		}), 4, 10*time.Second, false)
	if err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	tap := res.TAP
	t.Logf("TAP document:\n%s", tap)

	if got := strings.SplitN(tap, "\n", 2)[0]; got != "TAP version 13" {
		t.Errorf("first line = %q, want %q", got, "TAP version 13")
	}

	var plan string
	resultLines := 0
	for _, l := range strings.Split(tap, "\n") {
		switch {
		case strings.HasPrefix(l, "1.."):
			if plan != "" {
				t.Errorf("more than one plan line: %q and %q", plan, l)
			}
			plan = l
		case strings.HasPrefix(l, "ok "), strings.HasPrefix(l, "not ok "):
			resultLines++
		}
	}
	if plan == "" {
		t.Fatal("no plan line; a consumer cannot know how many tests to expect")
	}
	if !strings.Contains(plan, fmt.Sprintf("1..%d", res.Report.Total)) {
		t.Errorf("plan %q does not match the %d test points", plan, res.Report.Total)
	}
	if resultLines != res.Report.Total {
		t.Errorf("%d result lines for a %d-point plan; a TAP consumer that trusts "+
			"the plan HANGS", resultLines, res.Report.Total)
	}
	if res.Report.Failed == 0 || !strings.Contains(tap, "not ok") {
		t.Errorf("a failing assertion produced no `not ok` line:\n%s", tap)
	}
	// The diagnostic must name what failed, or the operator cannot act on it.
	if !strings.Contains(tap, "  ---") || !strings.Contains(tap, "  ...") {
		t.Errorf("a failure carries no diagnostic block:\n%s", tap)
	}
	if !strings.Contains(tap, "severity: 'fail'") {
		t.Errorf("the diagnostic carries no severity:\n%s", tap)
	}
	// The plan count and the exit code must agree, or CI is guessing.
	if res.Exit != 1 {
		t.Errorf("exit = %d, want 1 for a run with a failing assertion", res.Exit)
	}
}

// TestTAPPlanAndResultsAgreeForAnAllPassingRun pins the other half.
func TestTAPPlanAndResultsAgreeForAnAllPassingRun(t *testing.T) {
	srv, done := statusServer(200)
	defer done()

	a := newTestApp(t)
	res, err := a.RunTest(context.Background(),
		dirWith(t, map[string]string{
			"a.http": fmt.Sprintf("# @name a\n# @assert status == 200\n# @assert status in 200..299\nGET %s/a HTTP/1.1\n\n###\n", srv.URL),
			"b.http": fmt.Sprintf("# @name b\n# @assert body contains \"ok\"\nGET %s/b HTTP/1.1\n\n###\n", srv.URL),
		}), 4, 10*time.Second, false)
	if err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	t.Logf("TAP document:\n%s", res.TAP)

	if res.Exit != 0 {
		t.Errorf("exit = %d, want 0", res.Exit)
	}
	if res.Report.Failed != 0 || res.Report.Passed != 3 {
		t.Errorf("passed=%d failed=%d, want 3 and 0", res.Report.Passed, res.Report.Failed)
	}
	var results int
	for _, l := range strings.Split(res.TAP, "\n") {
		if strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "not ok ") {
			results++
		}
	}
	if results != 3 {
		t.Errorf("%d result lines for a 3-point plan", results)
	}
	if !strings.Contains(res.TAP, "1..3") {
		t.Errorf("no 1..3 plan:\n%s", res.TAP)
	}
}

// TestWriteTestReportIsOneWritePerLine pins the streaming property
// ROADMAP.md §4.1.3 requires: TAP consumers parse incrementally.
func TestWriteTestReportIsOneWritePerLine(t *testing.T) {
	var buf bytes.Buffer
	counting := &countWriter{w: &buf}
	if err := app.WriteTestReport(counting, "TAP version 13\n1..2\nok 1 - a\nnot ok 2 - b\n"); err != nil {
		t.Fatalf("WriteTestReport: %v", err)
	}
	if counting.n != 4 {
		t.Errorf("%d writes, want 4 — one per line, unbuffered", counting.n)
	}
	if got := buf.String(); !strings.HasPrefix(got, "TAP version 13\n") || !strings.HasSuffix(got, "b\n") {
		t.Errorf("output = %q", got)
	}
}

type countWriter struct {
	w io.Writer
	n int
}

func (c *countWriter) Write(p []byte) (int, error) {
	c.n++
	return c.w.Write(p)
}

// TestExitCodeMatrixFromTheCLIPath drives the whole path the CLI uses, so the
// exit code is proven at the seam an operator actually observes.
func TestExitCodeMatrixFromTheCLIPath(t *testing.T) {
	okSrv, okDone := statusServer(200)
	defer okDone()
	badSrv, badDone := statusServer(500)
	defer badDone()

	target := func(url string) string {
		return fmt.Sprintf("# @name x\n# @assert status == 200\nGET %s/x HTTP/1.1\n\n###\n", url)
	}

	cases := []struct {
		name  string
		url   string
		files map[string]string
		want  int
	}{
		{"all pass", okSrv.URL, map[string]string{"a.http": target(okSrv.URL)}, 0},
		{"one fails", okSrv.URL, map[string]string{
			"a.http": target(okSrv.URL), "b.http": target(badSrv.URL)}, 1},
		{"server always 500", badSrv.URL, map[string]string{"a.http": target(badSrv.URL)}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := newTestApp(t).TestDir(context.Background(),
				dirWith(t, c.files), &out, 4, 10*time.Second, false)
			if err != nil {
				t.Fatalf("TestDir: %v", err)
			}
			if got != c.want {
				t.Errorf("exit = %d, want %d\nTAP:\n%s", got, c.want, out.String())
			}
			if !strings.HasPrefix(out.String(), "TAP version 13\n") {
				t.Errorf("the CLI path did not emit TAP: %q", out.String())
			}
		})
	}
}

// TestStrictBlockedEscalatesTheExitCode proves the opt-in mode.
func TestStrictBlockedEscalatesTheExitCode(t *testing.T) {
	srv, done := statusServer(200)
	defer done()

	dir := dirWith(t, map[string]string{
		"a.http": fmt.Sprintf("# @name a\n# @assert status == 200\nGET %s/a HTTP/1.1\n\n###\n", srv.URL),
	})

	// The default: a run with no skips is unaffected by the flag.
	a := newTestApp(t)
	res, err := a.RunTest(context.Background(), dir, 2, 10*time.Second, true)
	if err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	if res.Exit != 0 {
		t.Errorf("exit = %d with no skips, want 0", res.Exit)
	}
	if !strings.Contains(res.TAP, "1..1") {
		t.Errorf("no plan:\n%s", res.TAP)
	}
	t.Log("strict-blocked leaves a run with no skips at exit 0")
}

// TestJSONOutputIsOneParseableObject is the JSON structural gate.
//
// `--format json` must emit a single well-formed object — not a stream, not
// NDJSON, not a truncated buffer. A CI parser that reads one object must get
// exactly one.
func TestJSONOutputIsOneParseableObject(t *testing.T) {
	srv, done := statusServer(200)
	defer done()

	a := newTestApp(t)
	res, err := a.RunTest(context.Background(),
		dirWith(t, map[string]string{
			"pass.http": fmt.Sprintf("# @name p\n# @assert status == 200\nGET %s/p HTTP/1.1\n\n###\n", srv.URL),
			"fail.http": fmt.Sprintf("# @name f\n# @assert status == 500\nGET %s/f HTTP/1.1\n\n###\n", srv.URL),
		}), 4, 10*time.Second, false)
	if err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	if res.JSON == "" {
		t.Fatal("RunTest produced no JSON document")
	}
	dec := json.NewDecoder(strings.NewReader(res.JSON))
	var doc struct {
		Total   int `json:"total"`
		Passed  int `json:"passed"`
		Failed  int `json:"failed"`
		Skipped int `json:"skipped"`
		Exit    int `json:"exit"`
		Tests   []struct {
			File string `json:"file"`
			OK   bool   `json:"ok"`
		} `json:"tests"`
	}
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("the JSON document does not parse as one object: %v:\n%s", err, res.JSON)
	}
	if dec.More() {
		t.Errorf("the JSON document holds more than one value; --format json must emit exactly one object")
	}
	if doc.Total != len(doc.Tests) {
		t.Errorf("total = %d but the document carries %d test points; a consumer cannot trust the count", doc.Total, len(doc.Tests))
	}
	if doc.Total != 2 {
		t.Errorf("total = %d, want 2 for a one-pass one-fail run", doc.Total)
	}
	t.Logf("one JSON object: total=%d tests=%d", doc.Total, len(doc.Tests))
}

// TestJSONCountsMatchTheReport pins the producer/formatter split.
//
// The engine owns the counts; the JSON formatter copies them. Recomputing
// them in the formatter would let the two documents disagree about what the
// run meant, which is exactly what M69 (total mapped into passed) does.
func TestJSONCountsMatchTheReport(t *testing.T) {
	srv, done := statusServer(200)
	defer done()

	a := newTestApp(t)
	res, err := a.RunTest(context.Background(),
		dirWith(t, map[string]string{
			"pass.http": fmt.Sprintf("# @name p\n# @assert status == 200\nGET %s/p HTTP/1.1\n\n###\n", srv.URL),
			"fail.http": fmt.Sprintf("# @name f\n# @assert status == 500\nGET %s/f HTTP/1.1\n\n###\n", srv.URL),
		}), 4, 10*time.Second, false)
	if err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	var doc struct {
		Total   int `json:"total"`
		Passed  int `json:"passed"`
		Failed  int `json:"failed"`
		Skipped int `json:"skipped"`
		Exit    int `json:"exit"`
		Tests   []struct {
			OK   bool `json:"ok"`
			Skip bool `json:"skip"`
		} `json:"tests"`
	}
	if err := json.Unmarshal([]byte(res.JSON), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []struct {
		name string
		got  int
		want int
	}{
		{"total", doc.Total, res.Report.Total},
		{"passed", doc.Passed, res.Report.Passed},
		{"failed", doc.Failed, res.Report.Failed},
		{"skipped", doc.Skipped, res.Report.Skipped},
		{"exit", doc.Exit, res.Report.ExitCode},
	} {
		if field.got != field.want {
			t.Errorf("json.%s = %d, want the report's %d", field.name, field.got, field.want)
		}
	}
	// Cross-check the array against the aggregates rather than trusting
	// either alone: the aggregates come from the report, the array from the
	// entries, and only their agreement proves both paths carried the run.
	var oks, skips int
	for _, tp := range doc.Tests {
		switch {
		case tp.Skip:
			skips++
		case tp.OK:
			oks++
		}
	}
	if oks != doc.Passed || skips != doc.Skipped || len(doc.Tests)-oks-skips != doc.Failed {
		t.Errorf("the tests array (ok=%d skip=%d total=%d) disagrees with the aggregates "+
			"(passed=%d skipped=%d failed=%d)", oks, skips, len(doc.Tests), doc.Passed, doc.Skipped, doc.Failed)
	}
	if res.Exit != res.Report.ExitCode {
		t.Errorf("result exit = %d, want the report's %d; the formatter must not alter exit codes", res.Exit, res.Report.ExitCode)
	}
}

// TestSilentWritesNothingToStdoutButStillExitsCorrectly pins the silent
// contract: silence changes what is WRITTEN, never what is concluded.
func TestSilentWritesNothingToStdoutButStillExitsCorrectly(t *testing.T) {
	okSrv, okDone := statusServer(200)
	defer okDone()
	badSrv, badDone := statusServer(500)
	defer badDone()

	target := func(url string) string {
		return fmt.Sprintf("# @name x\n# @assert status == 200\nGET %s/x HTTP/1.1\n\n###\n", url)
	}

	for _, format := range []app.TestFormat{app.TestFormatTAP, app.TestFormatJSON} {
		// A passing run stays exit 0 with zero bytes.
		var out bytes.Buffer
		got, err := newTestApp(t).TestDirEx(context.Background(),
			dirWith(t, map[string]string{"a.http": target(okSrv.URL)}),
			&out, 4, 10*time.Second, false, format, true)
		if err != nil {
			t.Fatalf("TestDirEx(silent, %s): %v", format, err)
		}
		if got != 0 {
			t.Errorf("silent %s pass: exit = %d, want 0", format, got)
		}
		if out.Len() != 0 {
			t.Errorf("silent %s pass: wrote %d bytes, want zero", format, out.Len())
		}

		// A failing run stays exit 1 with zero bytes — the exact case M70
		// (silent falling through to TAP) breaks.
		out.Reset()
		got, err = newTestApp(t).TestDirEx(context.Background(),
			dirWith(t, map[string]string{"a.http": target(badSrv.URL)}),
			&out, 4, 10*time.Second, false, format, true)
		if err != nil {
			t.Fatalf("TestDirEx(silent, %s) failing: %v", format, err)
		}
		if got != 1 {
			t.Errorf("silent %s fail: exit = %d, want 1; --silent must not mean exit 0", format, got)
		}
		if out.Len() != 0 {
			t.Errorf("silent %s fail: wrote %d bytes %q, want zero", format, out.Len(), out.String())
		}
	}
}

// TestUnknownFormatIsExitTwo proves an unknown `--format` fails closed
// BEFORE any test executes: a typo must read as a usage error, never as a
// green run and never as a test failure.
func TestUnknownFormatIsExitTwo(t *testing.T) {
	if _, err := app.ParseTestFormat("xml"); err == nil {
		t.Error("ParseTestFormat(\"xml\") accepted an unknown format")
	}
	if _, err := app.ParseTestFormat(""); err != nil {
		t.Errorf("ParseTestFormat(\"\") = %v, want the TAP default", err)
	}
	if f, err := app.ParseTestFormat("JSON"); err != nil || f != app.TestFormatJSON {
		t.Errorf("ParseTestFormat(\"JSON\") = %q, %v; the flag is case-insensitive", f, err)
	}

	var served atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var out bytes.Buffer
	got, err := newTestApp(t).TestDirEx(context.Background(),
		dirWith(t, map[string]string{
			"a.http": fmt.Sprintf("# @name a\n# @assert status == 200\nGET %s/a HTTP/1.1\n\n###\n", srv.URL),
		}), &out, 4, 10*time.Second, false, app.TestFormat("xml"), false)
	if err == nil {
		t.Error("TestDirEx with an unknown format returned no error")
	}
	if got != engine.ExitUsage {
		t.Errorf("exit = %d, want %d (exit 2) for an unknown format", got, engine.ExitUsage)
	}
	if out.Len() != 0 {
		t.Errorf("an unknown format wrote %d bytes; nothing about the run may be reported", out.Len())
	}
	if n := served.Load(); n != 0 {
		t.Errorf("the runner dispatched %d request(s) for an unknown format; validation must precede execution", n)
	}
}

// TestJSONRedactsSecrets enforces the redaction contract on the new format.
//
// `pulse test` never installs the MCP stdout lock, so §3.3 does not apply —
// but §3.4 does. A JSON document on stdout must scrub vault-sourced values
// exactly as TAP does, because a formatter that re-emits what the engine
// scrubbed is a leak with a flag.
func TestJSONRedactsSecrets(t *testing.T) {
	const secret = "pulse-json-secret-token-6d21"

	a := newTestApp(t, func(c *app.Config) {
		c.EnvFile = writeEnvFile(t, map[string]string{"JSON_TOKEN": secret})
	})
	if _, err := a.Chain.Resolve("JSON_TOKEN"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Debug-Token", secret)
		fmt.Fprintf(w, `{"echo":"%s"}`, secret)
	}))
	defer srv.Close()

	// A header-equality failure echoes the ACTUAL header value into the
	// failure detail (assert.go: `%s is %q, want %q`), which is the only
	// runner path that carries response content into an entry. That is what
	// makes this a redaction gate rather than a vacuous scan: without the
	// engine's entry-level scrub, the secret reaches both documents.
	res, err := a.RunTest(context.Background(),
		dirWith(t, map[string]string{
			"a.http": fmt.Sprintf("# @name a\n# @assert header X-Debug-Token == \"definitely-not-the-token\"\nGET %s/a HTTP/1.1\n\n###\n", srv.URL),
		}), 4, 10*time.Second, false)
	if err != nil {
		t.Fatalf("RunTest: %v", err)
	}
	if res.Report.Failed == 0 {
		t.Fatalf("the assertion unexpectedly passed; the redaction gate needs a failing detail echoing the header")
	}
	for _, doc := range []string{res.TAP, res.JSON} {
		if strings.Contains(doc, secret) {
			t.Errorf("the resolved secret survived into a report document:\n%s", firstLines(doc, 12))
		}
	}
	if !strings.Contains(res.JSON, "[REDACTED]") {
		t.Errorf("the JSON document carries no redaction placeholder, so the scrubber never ran:\n%s", firstLines(res.JSON, 12))
	}
}

// TestVaultPathsAndRoundTripOnDisk proves the CLI-facing path shapes and the
// disk round trip, through the App rather than through the secrets package.
func TestVaultPathsAndRoundTripOnDisk(t *testing.T) {
	if plain, vault := app.VaultPaths("/a/b/.env"); plain != "/a/b/.env" || vault != "/a/b/.env.vault" {
		t.Errorf("VaultPaths(.env) = %q, %q", plain, vault)
	}
	if plain, vault := app.VaultPaths("/a/b/.env.vault"); plain != "/a/b/.env" || vault != "/a/b/.env.vault" {
		t.Errorf("VaultPaths(.env.vault) = %q, %q", plain, vault)
	}
	if _, vault := app.VaultPaths(""); vault != "" {
		t.Errorf("VaultPaths(\"\") = %q", vault)
	}
}
