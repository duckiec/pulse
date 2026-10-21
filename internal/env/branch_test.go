package env

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
)

// gitAvailable reports whether the real git binary is installed. A test that
// silently accepted "" from a missing binary would pass for the wrong reason
// (AGENTS.md §4.5: BLOCKED needs a stated reason).
func gitAvailable(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("SKIP (reason: git is not installed on this host)")
	}
	return path
}

// TestBranchReadsTheCheckedOutBranch proves the real subprocess path: a fresh
// repository with a slash-bearing branch name is read back verbatim.
func TestBranchReadsTheCheckedOutBranch(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=pulse-test", "GIT_AUTHOR_EMAIL=pulse@example.invalid",
			"GIT_COMMITTER_NAME=pulse-test", "GIT_COMMITTER_EMAIL=pulse@example.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("checkout", "-q", "-b", "feat/branch-env")
	run("commit", "-q", "--allow-empty", "-m", "init")

	got, err := Branch(context.Background(), dir)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if got != "feat/branch-env" {
		t.Errorf("Branch = %q, want feat/branch-env", got)
	}
}

// TestBranchOutsideARepositoryIsEmpty pins the fallback contract: no repo is
// "" with no error, so the caller uses [default] without reporting a failure.
func TestBranchOutsideARepositoryIsEmpty(t *testing.T) {
	gitAvailable(t)
	got, err := Branch(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Branch outside a repo returned an error: %v", err)
	}
	if got != "" {
		t.Errorf("Branch = %q, want empty outside a repo", got)
	}
}

// TestBranchMissingGitIsReported pins the other half: a machine without git
// is a degradation worth naming, not a silent fallback.
func TestBranchMissingGitIsReported(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Branch(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Branch with no git on PATH returned nil error")
	}
	var execErr *exec.Error
	if !errors.As(err, &execErr) {
		t.Errorf("error %v does not wrap *exec.Error", err)
	}
}

// TestBranchIsBounded proves the subprocess cannot hang boot. The seam swaps
// in a sleeper; the deadline must kill it and surface the timeout.
func TestBranchIsBounded(t *testing.T) {
	old := commandContext
	commandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "30")
	}
	t.Cleanup(func() { commandContext = old })

	start := time.Now()
	_, err := Branch(context.Background(), t.TempDir())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Branch accepted a hung git invocation")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed > BranchTimeout+2*time.Second {
		t.Errorf("Branch took %s, want ~%s", elapsed, BranchTimeout)
	}
}

// TestBranchHonoursParentCancellation proves the child context composes with
// the caller's, not just its own deadline.
func TestBranchHonoursParentCancellation(t *testing.T) {
	old := commandContext
	commandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sleep", "30")
	}
	t.Cleanup(func() { commandContext = old })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if _, err := Branch(ctx, t.TempDir()); err == nil {
		t.Fatal("Branch ignored a cancelled parent context")
	}
	if elapsed := time.Since(start); elapsed > BranchTimeout {
		t.Errorf("Branch took %s after parent cancellation", elapsed)
	}
}

// TestBranchDetachedHeadIsNotABranch pins the "HEAD" guard: a detached
// checkout has no branch name and must not be named after the sentinel.
func TestBranchDetachedHeadIsNotABranch(t *testing.T) {
	gitAvailable(t)
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=pulse-test", "GIT_AUTHOR_EMAIL=pulse@example.invalid",
			"GIT_COMMITTER_NAME=pulse-test", "GIT_COMMITTER_EMAIL=pulse@example.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "one")
	run("checkout", "-q", "--detach", "HEAD")

	got, err := Branch(context.Background(), dir)
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if got != "" {
		t.Errorf("Branch(detached) = %q, want empty", got)
	}
}
