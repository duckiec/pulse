package engine_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pulse/internal/engine"
)

// bodyServer serves whatever is currently stored, so one run can change the
// response between checks without restarting the server.
func bodyServer(t *testing.T) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var body atomic.Value
	body.Store(`{"id":1,"name":"ada"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	t.Cleanup(srv.Close)
	return srv, &body
}

func runOneSnapshotFile(t *testing.T, file string, cfg engine.TestConfig) (*engine.TestReport, []engine.Entry, error) {
	t.Helper()
	rep := &recordingReporter{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	report, err := newTestRunner().Run(ctx, []string{file}, cfg, rep)
	return report, rep.entries, err
}

func onlySnapshotEntry(t *testing.T, entries []engine.Entry) engine.Entry {
	t.Helper()
	var found []engine.Entry
	for _, e := range entries {
		if e.Assertion == engine.SnapshotAssertExpr {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d snapshot entries in %+v, want 1", len(found), entries)
	}
	return found[0]
}

// TestSnapshotAssertionLifecycle is the runner's end-to-end gate: create,
// match, fail with a diff, then accept with UpdateSnapshots.
func TestSnapshotAssertionLifecycle(t *testing.T) {
	srv, body := bodyServer(t)
	dir := t.TempDir()
	httpFile := writeFile(t, dir, "api.http", fmt.Sprintf(
		"# @name get-user\n# @assert status == 200\n# @assert snapshot\nGET %s/user HTTP/1.1\n\n###\n", srv.URL))
	snapFile := engine.SnapshotPath(httpFile)

	report, entries, err := runOneSnapshotFile(t, httpFile, engine.TestConfig{})
	if err != nil {
		t.Fatalf("first run: %v (report %+v)", err, report)
	}
	if report.ExitCode != engine.ExitOK {
		t.Fatalf("first run exit = %d, want 0; entries %+v", report.ExitCode, entries)
	}
	created := onlySnapshotEntry(t, entries)
	if !created.OK || created.Fields["action"] != string(engine.SnapshotCreated) {
		t.Fatalf("created entry = %+v", created)
	}
	if _, err := os.Stat(snapFile); err != nil {
		t.Fatalf("snapshot file not created: %v", err)
	}

	report, entries, err = runOneSnapshotFile(t, httpFile, engine.TestConfig{})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if report.ExitCode != engine.ExitOK {
		t.Fatalf("second run exit = %d, entries %+v", report.ExitCode, entries)
	}
	if got := onlySnapshotEntry(t, entries); got.Fields["action"] != string(engine.SnapshotMatched) {
		t.Fatalf("second entry = %+v, want matched", got)
	}

	body.Store(`{"id":2,"name":"grace"}`)
	report, entries, err = runOneSnapshotFile(t, httpFile, engine.TestConfig{})
	if err != nil {
		t.Fatalf("mismatch run: %v", err)
	}
	if report.ExitCode != engine.ExitFailed {
		t.Fatalf("mismatch exit = %d, want 1", report.ExitCode)
	}
	failed := onlySnapshotEntry(t, entries)
	if failed.OK {
		t.Fatal("a changed body passed")
	}
	if !strings.Contains(failed.Diff, "$.id") || !strings.Contains(failed.Diff, "live 2") {
		t.Errorf("diff is not path-qualified:\n%s", failed.Diff)
	}
	if failed.Detail == "" {
		t.Error("a snapshot failure carries no detail")
	}

	report, entries, err = runOneSnapshotFile(t, httpFile, engine.TestConfig{UpdateSnapshots: true})
	if err != nil {
		t.Fatalf("update run: %v", err)
	}
	if report.ExitCode != engine.ExitOK {
		t.Fatalf("update exit = %d, entries %+v", report.ExitCode, entries)
	}
	updated := onlySnapshotEntry(t, entries)
	if !updated.OK || updated.Fields["action"] != string(engine.SnapshotUpdated) {
		t.Fatalf("update entry = %+v", updated)
	}
	raw, err := os.ReadFile(snapFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"id":2`) {
		t.Errorf("snapshot was not updated:\n%s", raw)
	}
}

// TestSnapshotAssertionRequiresName is the exit-2 gate: an unnamed snapshot is
// a usage error discovered before any request runs, not a failed test.
func TestSnapshotAssertionRequiresName(t *testing.T) {
	srv, _ := bodyServer(t)
	httpFile := writeFile(t, t.TempDir(), "api.http", fmt.Sprintf(
		"# @assert snapshot\nGET %s/user HTTP/1.1\n\n###\n", srv.URL))
	report, _, err := runOneSnapshotFile(t, httpFile, engine.TestConfig{})
	if !errors.Is(err, engine.ErrParseFailure) {
		t.Fatalf("err = %v, want ErrParseFailure", err)
	}
	if report == nil || report.ExitCode != engine.ExitUsage {
		t.Fatalf("report = %+v, want exit 2", report)
	}
	if _, statErr := os.Stat(engine.SnapshotPath(httpFile)); !os.IsNotExist(statErr) {
		t.Error("an unnamed snapshot created a file")
	}
}

// TestTruncatedBodyFailsClosed proves the 256 KiB collector cap cannot become
// a snapshot baseline.
func TestTruncatedBodyFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", engine.MaxCollectedBytes+4096)))
	}))
	defer srv.Close()

	httpFile := writeFile(t, t.TempDir(), "big.http", fmt.Sprintf(
		"# @name big\n# @assert snapshot\nGET %s/big HTTP/1.1\n\n###\n", srv.URL))
	report, entries, err := runOneSnapshotFile(t, httpFile, engine.TestConfig{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.ExitCode != engine.ExitFailed {
		t.Fatalf("exit = %d, want 1", report.ExitCode)
	}
	got := onlySnapshotEntry(t, entries)
	if got.OK || !strings.Contains(got.Detail, "exceeds") {
		t.Fatalf("entry = %+v, want a named truncation failure", got)
	}
	if _, statErr := os.Stat(engine.SnapshotPath(httpFile)); !os.IsNotExist(statErr) {
		t.Error("a truncated body created a snapshot file")
	}
}

// TestSnapshotDirectivesDoNotBreakOtherAssertions proves the directive is
// additive: status and body assertions still evaluate in the same request.
func TestSnapshotDirectivesDoNotBreakOtherAssertions(t *testing.T) {
	srv, _ := bodyServer(t)
	httpFile := writeFile(t, t.TempDir(), "api.http", fmt.Sprintf(
		"# @name get-user\n# @assert status == 200\n# @assert body contains \"ada\"\n# @assert snapshot\nGET %s/user HTTP/1.1\n\n###\n", srv.URL))
	report, entries, err := runOneSnapshotFile(t, httpFile, engine.TestConfig{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if report.ExitCode != engine.ExitOK {
		t.Fatalf("exit = %d, entries %+v", report.ExitCode, entries)
	}
	if report.Total != 3 {
		t.Fatalf("Total = %d, want 3 (two expressions plus the snapshot)", report.Total)
	}
	for _, e := range entries {
		if !e.OK {
			t.Errorf("unexpected failure: %+v", e)
		}
	}
}
