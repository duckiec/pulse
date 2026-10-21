package ui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"pulse/internal/engine"
)

const snapshotDoc = `# @name get-user
# @assert status == 200
# @assert body contains "ada"
# @assert snapshot
GET https://example.invalid/user HTTP/1.1

###`

// snapshotModel builds a model over a real (temporary) collection path, which
// is what a snapshot check needs. The runner is nil: these tests drive the
// post-run state directly, and no request is dispatched.
func snapshotModel(t *testing.T) *Model {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "api.http")
	if err := os.WriteFile(path, []byte(snapshotDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	doc := parseDoc(t, snapshotDoc)
	doc.Path = path
	m := New(context.Background(), nil, doc, false)
	m.res = &engine.RunResult{Status: 200}
	m.bodyBuf = []byte(`{"id":1,"name":"ada"}`)
	m.captureRunContext()
	m.collectBody = true
	return m
}

// fakeChecker records the options and returns the next queued result.
type fakeChecker struct {
	calls  []engine.SnapshotOptions
	next   []engine.SnapshotResult
	nextEr []error
}

func (f *fakeChecker) check(opts engine.SnapshotOptions) (engine.SnapshotResult, error) {
	f.calls = append(f.calls, opts)
	i := len(f.calls) - 1
	var res engine.SnapshotResult
	var err error
	if i < len(f.next) {
		res = f.next[i]
	}
	if i < len(f.nextEr) {
		err = f.nextEr[i]
	}
	if res.Name == "" {
		res.Name = opts.Request
	}
	if res.Path == "" {
		res.Path = engine.SnapshotPath(opts.HTTPFile)
	}
	return res, err
}

// TestAssertsEvaluateInTheResponsePane pins the ordinary outcomes: the TUI
// evaluates the same expressions the runner does, against the same body.
func TestAssertsEvaluateInTheResponsePane(t *testing.T) {
	m := snapshotModel(t)
	m.evaluateAsserts()
	if len(m.asserts) != 2 {
		t.Fatalf("asserts = %d, want 2 (snapshot is rendered separately)", len(m.asserts))
	}
	for _, a := range m.asserts {
		if !a.OK {
			t.Errorf("%q failed: %s", a.Source, a.Detail)
		}
	}
	m.bodyBuf = []byte(`{"id":2}`)
	m.evaluateAsserts()
	if m.asserts[1].OK {
		t.Error("a body assertion passed after the body changed")
	}
	lines := strings.Join(m.snapshotLines(80), "\n")
	if !strings.Contains(lines, "[fail]") || !strings.Contains(lines, "does not contain") {
		t.Errorf("failure not rendered:\n%s", lines)
	}
}

// TestSnapshotMismatchRendersADiffAndUAccepts is the TUI half of the feature:
// the diff is visible, `u` re-runs the check with Update, and the verdict flips
// to a pass.
func TestSnapshotMismatchRendersADiffAndUAccepts(t *testing.T) {
	m := snapshotModel(t)
	m.evaluateAsserts()
	fake := &fakeChecker{next: []engine.SnapshotResult{
		{Action: engine.SnapshotMismatch, OK: false, Diff: "$.id: snapshot 1, live 2"},
		{Action: engine.SnapshotUpdated, OK: true},
	}}
	m.SetSnapshotChecker(fake.check)

	cmd := m.snapshotCheckCmd(false)
	if cmd == nil {
		t.Fatal("no snapshot check command")
	}
	msg := cmd()
	if _, ok := msg.(snapshotCheckedMsg); !ok {
		t.Fatalf("cmd returned %T, want snapshotCheckedMsg", msg)
	}
	m.Update(msg)
	if m.snap == nil || m.snap.OK {
		t.Fatalf("snap = %+v, want a failure", m.snap)
	}
	if !strings.Contains(m.toast, "differs") {
		t.Errorf("toast = %q, want the mismatch hint", m.toast)
	}
	rendered := strings.Join(m.snapshotLines(80), "\n")
	for _, want := range []string{"$.id: snapshot 1, live 2", "[u] accept"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("pane missing %q:\n%s", want, rendered)
		}
	}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("u produced no command")
	}
	msg = cmd()
	if len(fake.calls) != 2 || !fake.calls[1].Update {
		t.Fatalf("checker calls = %+v, want a second call with Update=true", fake.calls)
	}
	m.Update(msg)
	if m.snap == nil || !m.snap.OK || m.snap.Action != engine.SnapshotUpdated {
		t.Fatalf("snap after u = %+v, want updated+ok", m.snap)
	}
	if !strings.Contains(m.toast, "updated") {
		t.Errorf("toast = %q, want an update confirmation", m.toast)
	}
}

// TestUDoesNotRewriteAPassingSnapshot pins the guard: a stray `u` after a
// match leaves the baseline alone.
func TestUDoesNotRewriteAPassingSnapshot(t *testing.T) {
	m := snapshotModel(t)
	fake := &fakeChecker{next: []engine.SnapshotResult{{Action: engine.SnapshotMatched, OK: true}}}
	m.SetSnapshotChecker(fake.check)
	m.snap = &snapshotState{Name: "get-user", Path: engine.SnapshotPath(m.doc.Path), Action: engine.SnapshotMatched, OK: true}

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	m = updated.(*Model)
	if cmd != nil {
		t.Fatal("u on a passing snapshot produced a command")
	}
	if len(fake.calls) != 0 {
		t.Errorf("u on a passing snapshot called the checker: %+v", fake.calls)
	}
	if !strings.Contains(m.toast, "already matches") {
		t.Errorf("toast = %q", m.toast)
	}
}

// TestSnapshotWithoutSavedFileFailsClosed pins the scratchpad path: there is
// no adjacent file to write, so the pane says so instead of guessing a path.
func TestSnapshotWithoutSavedFileFailsClosed(t *testing.T) {
	m := snapshotModel(t)
	m.doc.Path = ""
	m.captureRunContext()
	if cmd := m.snapshotCheckCmd(false); cmd != nil {
		t.Fatal("a scratch document produced a snapshot command")
	}
	if m.snap == nil || m.snap.Err == nil {
		t.Fatalf("snap = %+v, want a named error", m.snap)
	}
	if !strings.Contains(strings.Join(m.snapshotLines(80), "\n"), "saved") {
		t.Error("the pane does not explain that a saved file is required")
	}
}

// TestSnapshotCheckErrorIsNotAcceptable pins the malformed-file path: `u` must
// not overwrite a file the operator needs to inspect.
func TestSnapshotCheckErrorIsNotAcceptable(t *testing.T) {
	m := snapshotModel(t)
	fake := &fakeChecker{next: []engine.SnapshotResult{{}}, nextEr: []error{engine.ErrSnapshotFormat}}
	m.SetSnapshotChecker(fake.check)
	m.Update(m.snapshotCheckCmd(false)())
	if m.snap == nil || m.snap.Err == nil {
		t.Fatalf("snap = %+v, want the checker error", m.snap)
	}
	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	m = updated.(*Model)
	if cmd != nil {
		t.Fatal("u on a malformed snapshot produced an update")
	}
	if len(fake.calls) != 1 || fake.calls[0].Update {
		t.Errorf("u called the checker with update: %+v", fake.calls)
	}
	if !strings.Contains(m.toast, "cannot be accepted") {
		t.Errorf("toast = %q", m.toast)
	}
}

// TestSnapshotDiffSurvivesAFullPane pins the render priority: a diff that
// arrived after the viewport was sized must not be pushed out of the pane by
// the stream body. The header (status + verdict + diff) is un-scrollable, so
// it wins the row budget.
func TestSnapshotDiffSurvivesAFullPane(t *testing.T) {
	m := snapshotModel(t)
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	m = updated.(*Model)
	for i := 0; i < 60; i++ {
		m.stream.Append([]byte(fmt.Sprintf("body-line-%02d\n", i)))
	}
	m.stream.Flush()
	m.stream.Finish()
	m.snap = &snapshotState{
		Name: "get-user",
		Path: engine.SnapshotPath(m.doc.Path),
		OK:   false,
		Diff: "$.id: snapshot 1, live 2",
	}
	view := m.View()
	if !strings.Contains(view, "$.id: snapshot 1, live 2") {
		t.Errorf("the diff was pushed out of a full pane:\n%s", view)
	}
}

// TestSnapshotTargetsTheRequestThatRan pins the dispatch-time capture: if the
// composer swaps the document while the stream is in flight, the check must
// still name the request that produced the body, not the new selection.
func TestSnapshotTargetsTheRequestThatRan(t *testing.T) {
	m := snapshotModel(t)
	fake := &fakeChecker{next: []engine.SnapshotResult{{Action: engine.SnapshotMatched, OK: true}}}
	m.SetSnapshotChecker(fake.check)
	runPath, runName := m.runPath, m.runName

	other := parseDoc(t, "GET https://other.invalid HTTP/1.1\n\n###\n")
	m.doc = other
	for r := range other.Requests() {
		m.req = r
		break
	}
	cmd := m.snapshotCheckCmd(false)
	if cmd == nil {
		t.Fatal("no command after a document swap")
	}
	m.Update(cmd())
	if len(fake.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(fake.calls))
	}
	if fake.calls[0].Request != runName || fake.calls[0].HTTPFile != runPath {
		t.Errorf("checked %q in %q, want the dispatched request %q in %q",
			fake.calls[0].Request, fake.calls[0].HTTPFile, runName, runPath)
	}
}

// TestBodyBufferIsBounded pins the memory argument: the assertion copy never
// exceeds the engine cap and is part of RetainedBytes.
func TestBodyBufferIsBounded(t *testing.T) {
	m := snapshotModel(t)
	m.bodyBuf = nil
	m.appendBody(make([]byte, engine.MaxCollectedBytes+4096))
	if len(m.bodyBuf) != engine.MaxCollectedBytes {
		t.Errorf("body buffer = %d bytes, want %d", len(m.bodyBuf), engine.MaxCollectedBytes)
	}
	if !m.bodyTruncated {
		t.Error("an oversized body was not marked truncated")
	}
	if got := m.RetainedBytes(); got > 2<<20 {
		t.Errorf("RetainedBytes = %d, want <= 2 MiB", got)
	}
}

// TestSnapshotCheckIsNotBoundFailsClosed pins the nil seam.
func TestSnapshotCheckIsNotBoundFailsClosed(t *testing.T) {
	m := snapshotModel(t)
	m.SetSnapshotChecker(nil)
	if cmd := m.snapshotCheckCmd(false); cmd != nil {
		t.Fatal("an unbound checker produced a command")
	}
	if m.snap == nil || m.snap.Err == nil {
		t.Fatalf("snap = %+v, want an unbound error", m.snap)
	}
}
