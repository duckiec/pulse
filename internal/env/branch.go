package env

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// BranchTimeout bounds the git subprocess. A repository on a slow network
// filesystem must never hold up Pulse's boot: the context kills the child and
// the caller falls back to [default] with a degradation it can report.
const BranchTimeout = 2 * time.Second

// commandContext is the test seam. It is exec.CommandContext in production, so
// a test can prove the deadline is installed without needing a hanging git.
var commandContext = exec.CommandContext

// Branch returns the current branch in dir, or "" when dir is not a Git
// repository (or HEAD is detached or unborn).
//
// The empty string is not an error: "not in Git" is a normal condition the
// caller answers with the [default] block. An error is returned only when git
// could not be executed at all or the bounded subprocess timed out — both are
// conditions worth reporting rather than hiding, because they mean the
// branch-aware layer is silently inactive.
func Branch(ctx context.Context, dir string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, BranchTimeout)
	defer cancel()

	cmd := commandContext(runCtx, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if dir != "" {
		cmd.Dir = dir
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	cmd.Stdin = nil

	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			return "", fmt.Errorf("env: git rev-parse did not finish within %s: %w", BranchTimeout, runCtx.Err())
		}
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return "", fmt.Errorf("env: cannot run git: %w", execErr)
		}
		// Any other exit is git saying "not a repository" (or an unborn
		// HEAD, which has no branch yet). Fall back to [default].
		return "", nil
	}
	branch := strings.TrimSpace(out.String())
	// `rev-parse --abbrev-ref HEAD` prints "HEAD" on a detached checkout.
	// That is not a branch name and must not become one.
	if branch == "" || branch == "HEAD" {
		return "", nil
	}
	return branch, nil
}
