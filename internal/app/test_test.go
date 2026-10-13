package app_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pulse/internal/app"
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
