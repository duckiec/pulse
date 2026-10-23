package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mergeWorkspace writes the three sides and returns their paths plus the
// current file.
func mergeWorkspace(t *testing.T, base, ours, theirs string) (dir, ancestor, current, other string) {
	t.Helper()
	dir = t.TempDir()
	ancestor = filepath.Join(dir, "base.http")
	current = filepath.Join(dir, "current.http")
	other = filepath.Join(dir, "other.http")
	for path, body := range map[string]string{ancestor: base, current: ours, other: theirs} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, ancestor, current, other
}

func runMergeCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run(context.Background(), append([]string{"merge"}, args...),
		strings.NewReader(""), &stdout, &stderr, false)
	return code, stdout.String(), stderr.String()
}

func TestMergeCLICleanWritesCurrent(t *testing.T) {
	_, base, current, other := mergeWorkspace(t,
		"# @name list\nGET https://api.example.com/items\nAccept: application/json\n",
		"# @name list\nGET https://api.example.com/items\nAccept: application/json\nX-Trace: 1\n",
		"# @name list\n# @assert status == 200\nGET https://api.example.com/items\nAccept: application/json\n")

	code, out, errOut := runMergeCLI(t, base, current, other)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "clean semantic merge") {
		t.Fatalf("stdout = %q, want a clean-merge report", out)
	}
	got, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"X-Trace: 1", "# @assert status == 200", "Accept: application/json"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("current file is missing %q:\n%s", want, got)
		}
	}
}

func TestMergeCLIConflictExitsOneAndWritesMarkers(t *testing.T) {
	_, base, current, other := mergeWorkspace(t,
		"GET https://api.example.com/items\nAccept: application/json\n",
		"GET https://api.example.com/items\nAccept: application/vnd.pulse+json\n",
		"GET https://api.example.com/items\nAccept: text/plain\n")

	code, out, errOut := runMergeCLI(t, base, current, other)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitFail, out, errOut)
	}
	if !strings.Contains(out, "unresolved conflict") {
		t.Fatalf("stdout = %q, want an unresolved-conflict report", out)
	}
	got, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"<<<<<<< current", "=======", ">>>>>>> other"} {
		if !strings.Contains(string(got), marker) {
			t.Fatalf("current file is missing the %q marker:\n%s", marker, got)
		}
	}
}

func TestMergeCLIUsageAndInputErrorsAreExitTwo(t *testing.T) {
	t.Run("two args", func(t *testing.T) {
		code, _, errOut := runMergeCLI(t, "a", "b")
		if code != exitUsage {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitUsage, errOut)
		}
		if !strings.Contains(errOut, "three files are required") {
			t.Fatalf("stderr = %q", errOut)
		}
	})
	t.Run("missing ancestor", func(t *testing.T) {
		dir := t.TempDir()
		code, _, errOut := runMergeCLI(t,
			filepath.Join(dir, "nope.http"),
			filepath.Join(dir, "current.http"),
			filepath.Join(dir, "other.http"))
		if code != exitUsage {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitUsage, errOut)
		}
	})
	t.Run("unparseable current is not overwritten", func(t *testing.T) {
		_, base, current, other := mergeWorkspace(t,
			"GET https://x.local/a\n", "this is not a request\n", "GET https://x.local/a\n")
		code, _, _ := runMergeCLI(t, base, current, other)
		if code != exitUsage {
			t.Fatalf("exit = %d, want %d", code, exitUsage)
		}
		got, _ := os.ReadFile(current)
		if string(got) != "this is not a request\n" {
			t.Fatalf("current was modified on a failed merge: %q", got)
		}
	})
}

func TestMergeHelp(t *testing.T) {
	for _, args := range [][]string{{"merge", "help"}, {"merge", "--help"}, {"help", "merge"}} {
		var stdout, stderr strings.Builder
		code := run(context.Background(), args, strings.NewReader(""), &stdout, &stderr, false)
		if code != exitOK {
			t.Fatalf("run(%q) = %d, want 0 (stderr: %s)", args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "merge.pulse.driver") {
			t.Fatalf("run(%q) help is missing the driver binding:\n%s", args, stdout.String())
		}
	}
}
