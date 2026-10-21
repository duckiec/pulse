package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pulse/internal/engine"
)

const snapshotCollection = `{
  "id": 1,
  "user": {"name": "ada", "roles": ["admin", "dev"]},
  "active": true,
  "score": 9.5,
  "note": null
}`

func snapshotDir(t *testing.T) (httpFile, snapFile string) {
	t.Helper()
	dir := t.TempDir()
	httpFile = filepath.Join(dir, "api.http")
	if err := os.WriteFile(httpFile, []byte("GET https://example.invalid HTTP/1.1\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return httpFile, engine.SnapshotPath(httpFile)
}

// TestFirstRunCreatesAndSecondMatches pins the whole first-run contract: the
// check writes an adjacent file, passes, and a repeat run compares instead of
// rewriting.
func TestFirstRunCreatesAndSecondMatches(t *testing.T) {
	httpFile, snapFile := snapshotDir(t)
	opts := engine.SnapshotOptions{HTTPFile: httpFile, Request: "get-user", Body: snapshotCollection}

	first, err := engine.CheckSnapshot(opts)
	if err != nil {
		t.Fatalf("first CheckSnapshot: %v", err)
	}
	if !first.OK || first.Action != engine.SnapshotCreated {
		t.Fatalf("first = %+v, want created+ok", first)
	}
	raw, err := os.ReadFile(snapFile)
	if err != nil {
		t.Fatalf("snapshot file missing after creation: %v", err)
	}
	if !strings.Contains(string(raw), "### @name get-user") {
		t.Errorf("snapshot file is not namespaced by @name:\n%s", raw)
	}

	before := string(raw)
	second, err := engine.CheckSnapshot(opts)
	if err != nil {
		t.Fatalf("second CheckSnapshot: %v", err)
	}
	if !second.OK || second.Action != engine.SnapshotMatched {
		t.Fatalf("second = %+v, want matched+ok", second)
	}
	after, err := os.ReadFile(snapFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != before {
		t.Error("a matching run rewrote the snapshot file")
	}
}

// TestJSONMismatchNamesThePath is the readability gate: a changed value is
// reported at its structural path, not as a byte offset or a whole-body blob.
func TestJSONMismatchNamesThePath(t *testing.T) {
	httpFile, _ := snapshotDir(t)
	if _, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "get-user", Body: snapshotCollection,
	}); err != nil {
		t.Fatal(err)
	}

	live := `{"id":2,"user":{"name":"ada","roles":["admin"]},"active":true,"score":9.5,"note":null,"extra":"x"}`
	res, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "get-user", Body: live,
	})
	if err != nil {
		t.Fatalf("CheckSnapshot: %v", err)
	}
	if res.OK || res.Action != engine.SnapshotMismatch {
		t.Fatalf("res = %+v, want mismatch", res)
	}
	for _, want := range []string{
		"$.id: snapshot 1, live 2",
		"$.user.roles[1]: missing from the live response",
		"$.extra: unexpected in the live response",
	} {
		if !strings.Contains(res.Diff, want) {
			t.Errorf("diff missing %q:\n%s", want, res.Diff)
		}
	}
	if strings.Contains(res.Diff, "$.active") {
		t.Errorf("diff reports an unchanged field:\n%s", res.Diff)
	}
}

// TestStructuralEqualityIgnoresFormatting proves a reindented or re-ordered
// JSON response passes. It also guards the empty-diff bug: a structural match
// must not be reported as a mismatch.
func TestStructuralEqualityIgnoresFormatting(t *testing.T) {
	httpFile, _ := snapshotDir(t)
	if _, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "r", Body: `{"b":2,"a":1}`,
	}); err != nil {
		t.Fatal(err)
	}
	res, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "r", Body: "{\n  \"a\": 1,\n  \"b\": 2\n}\n",
	})
	if err != nil {
		t.Fatalf("CheckSnapshot: %v", err)
	}
	if !res.OK || res.Action != engine.SnapshotMatched {
		t.Fatalf("res = %+v, want a structural match", res)
	}
	if res.Diff != "" {
		t.Errorf("a match produced a diff: %q", res.Diff)
	}
}

// TestUpdateReplacesTheBaseline pins -u: the failure is accepted, the file is
// rewritten, and the NEXT check matches the new body.
func TestUpdateReplacesTheBaseline(t *testing.T) {
	httpFile, snapFile := snapshotDir(t)
	if _, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "r", Body: `{"v":1}`,
	}); err != nil {
		t.Fatal(err)
	}
	mismatch, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "r", Body: `{"v":2}`,
	})
	if err != nil || mismatch.OK {
		t.Fatalf("mismatch = %+v, %v", mismatch, err)
	}
	updated, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "r", Body: `{"v":2}`, Update: true,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !updated.OK || updated.Action != engine.SnapshotUpdated {
		t.Fatalf("update = %+v, want updated+ok", updated)
	}
	raw, err := os.ReadFile(snapFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `{"v":2}`) {
		t.Errorf("updated file does not hold the new baseline:\n%s", raw)
	}
	again, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "r", Body: `{"v":2}`,
	})
	if err != nil || !again.OK || again.Action != engine.SnapshotMatched {
		t.Fatalf("after update = %+v, %v", again, err)
	}
}

// TestNonJSONUsesALineDiff pins the fallback so a text endpoint is still
// reviewable.
func TestNonJSONUsesALineDiff(t *testing.T) {
	httpFile, _ := snapshotDir(t)
	if _, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "r", Body: "alpha\nbeta\ngamma",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "r", Body: "alpha\nBETA\ngamma",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK {
		t.Fatal("a changed text body matched")
	}
	for _, want := range []string{"@@", "- beta", "+ BETA"} {
		if !strings.Contains(res.Diff, want) {
			t.Errorf("line diff missing %q:\n%s", want, res.Diff)
		}
	}
}

// TestUnnamedSnapshotFailsClosed pins the namespace requirement.
func TestUnnamedSnapshotFailsClosed(t *testing.T) {
	httpFile, _ := snapshotDir(t)
	_, err := engine.CheckSnapshot(engine.SnapshotOptions{HTTPFile: httpFile, Body: "x"})
	if !errors.Is(err, engine.ErrSnapshotUnnamed) {
		t.Fatalf("err = %v, want ErrSnapshotUnnamed", err)
	}
}

// TestMalformedSnapshotFileIsNotOverwritten is the data-loss gate: a file
// from another tool (or a corrupted one) must be refused, not clobbered.
func TestMalformedSnapshotFileIsNotOverwritten(t *testing.T) {
	httpFile, snapFile := snapshotDir(t)
	corrupt := "not a pulse snapshot\n### @name r\nold\n"
	if err := os.WriteFile(snapFile, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "r", Body: "new", Update: true,
	})
	if !errors.Is(err, engine.ErrSnapshotFormat) {
		t.Fatalf("err = %v, want ErrSnapshotFormat", err)
	}
	raw, rerr := os.ReadFile(snapFile)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(raw) != corrupt {
		t.Error("a malformed snapshot file was overwritten")
	}
}

// TestUnrepresentableBodyIsRejected pins the envelope's one ambiguity.
func TestUnrepresentableBodyIsRejected(t *testing.T) {
	httpFile, _ := snapshotDir(t)
	_, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "r", Body: "ok\n### @name evil\nnope",
	})
	if err == nil {
		t.Fatal("a body containing the namespace marker was accepted")
	}
	if _, statErr := os.Stat(engine.SnapshotPath(httpFile)); !os.IsNotExist(statErr) {
		t.Error("a rejected body still created a snapshot file")
	}
}

// TestMultipleNamespacesRoundTrip pins the file format, including deterministic
// ordering and duplicate rejection.
func TestMultipleNamespacesRoundTrip(t *testing.T) {
	httpFile, snapFile := snapshotDir(t)
	for name, body := range map[string]string{
		"z-last":  `{"z":1}`,
		"a-first": "plain\ntext",
	} {
		if _, err := engine.CheckSnapshot(engine.SnapshotOptions{
			HTTPFile: httpFile, Request: name, Body: body,
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	file, err := engine.LoadSnapshotFile(snapFile)
	if err != nil {
		t.Fatalf("LoadSnapshotFile: %v", err)
	}
	if got := file.Names(); len(got) != 2 {
		t.Fatalf("names = %v, want two", got)
	}
	if got, ok := file.Get("a-first"); !ok || got != "plain\ntext" {
		t.Errorf("a-first = %q, %v", got, ok)
	}
	encoded := file.Encode()
	if strings.Index(encoded, "a-first") > strings.Index(encoded, "z-last") {
		t.Errorf("Encode is not sorted:\n%s", encoded)
	}
	if _, err := engine.ParseSnapshotFile([]byte(encoded+snapshotDuplicate), "x"); err == nil {
		t.Error("a duplicate namespace parsed")
	}
}

// snapshotDuplicate appends a second block for an existing name.
const snapshotDuplicate = "### @name a-first\ndup\n"

// TestSnapshotTooLargeFailsClosed proves the body cap is enforced at the
// engine boundary, not only by the caller's collector.
func TestSnapshotTooLargeFailsClosed(t *testing.T) {
	httpFile, _ := snapshotDir(t)
	_, err := engine.CheckSnapshot(engine.SnapshotOptions{
		HTTPFile: httpFile, Request: "r",
		Body: strings.Repeat("x", engine.MaxCollectedBytes+1),
	})
	if !errors.Is(err, engine.ErrSnapshotTooLarge) {
		t.Fatalf("err = %v, want ErrSnapshotTooLarge", err)
	}
}

// TestDiffIsBounded pins the display cap: a pathological change is summarised,
// not pasted whole into a terminal.
func TestDiffIsBounded(t *testing.T) {
	var a, b strings.Builder
	for i := 0; i < engine.MaxDiffLines*2; i++ {
		a.WriteString("old line\n")
		b.WriteString("new line\n")
	}
	diff := engine.DiffSnapshot(a.String(), b.String())
	if got := strings.Count(diff, "\n") + 1; got > engine.MaxDiffLines+1 {
		t.Errorf("diff has %d lines, want at most %d + summary", got, engine.MaxDiffLines)
	}
	if !strings.Contains(diff, "more differences") {
		t.Errorf("capped diff does not summarise the remainder:\n%s", diff)
	}
}
