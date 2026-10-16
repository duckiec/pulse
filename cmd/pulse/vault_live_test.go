//go:build integration

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pulse/internal/secrets"
)

// TestVaultAutoBootstrapLive proves the one-step pipeline against a real
// Secret Service under scripts/ci/run-with-keyring.sh: `vault set` on a
// fresh machine mints the device identity and seals the vault, `list`
// prints sorted names (never values), a piped second set lands, and
// `unset` removes exactly one key. Skipped without a keychain (BLOCKED
// with reason, AGENTS.md §4.5) — never faked.
func TestVaultAutoBootstrapLive(t *testing.T) {
	if err := secrets.NewStore().Available(); err != nil {
		t.Skipf("BLOCKED (reason: no keychain write path: %v)", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("PULSE_HOME", home)
	t.Setenv("PULSE_SKIP_SYNC", "1")
	t.Setenv("PULSE_VAULT_FILE", filepath.Join(dir, ".pulse.vault"))
	t.Setenv("PULSE_VAULT_KEY_ID", "live-bootstrap")
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	ctx := context.Background()
	run := func(args []string, stdin string) (int, string, string) {
		var stdout, stderr strings.Builder
		code := run(ctx, args, strings.NewReader(stdin), &stdout, &stderr, false)
		return code, stdout.String(), stderr.String()
	}

	// Fresh machine: no identity, no vault — one set bootstraps both.
	if code, _, stderr := run([]string{"vault", "set", "LIVE_B", "second"}, ""); code != 0 {
		t.Fatalf("vault set LIVE_B exited %d: %s", code, stderr)
	}
	// Piped value form: no VALUE argument, stdin carries the secret.
	if code, _, stderr := run([]string{"vault", "set", "LIVE_A"}, "first\n"); code != 0 {
		t.Fatalf("vault set LIVE_A (piped) exited %d: %s", code, stderr)
	}

	code, stdout, stderr := run([]string{"vault", "list"}, "")
	if code != 0 {
		t.Fatalf("vault list exited %d: %s", code, stderr)
	}
	if stdout != "LIVE_A\nLIVE_B\n" {
		t.Fatalf("vault list = %q, want sorted names", stdout)
	}
	if strings.Contains(stdout, "first") || strings.Contains(stdout, "second") {
		t.Fatalf("vault list leaks values: %q", stdout)
	}

	if code, _, stderr := run([]string{"vault", "unset", "LIVE_A"}, ""); code != 0 {
		t.Fatalf("vault unset exited %d: %s", code, stderr)
	}
	if code, stdout, _ := run([]string{"vault", "list"}, ""); code != 0 || stdout != "LIVE_B\n" {
		t.Fatalf("vault list after unset = %q (code %d), want LIVE_B only", stdout, code)
	}

	// Doctor sees the workspace vault and reports no missing secrets for
	// a file whose only placeholder is now sealed.
	if err := os.WriteFile("live.http", []byte(
		"GET http://localhost:9/x HTTP/1.1\nAuthorization: Bearer {{LIVE_B}}\n\n###\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The runtime chain only opens the configured vault tier at boot when
	// PULSE_VAULT_FILE names it — which it does here — so doctor resolves
	// LIVE_B and stays green.
	if code, _, stderr := run([]string{"doctor"}, ""); code != 0 {
		t.Fatalf("doctor over the bootstrapped vault exited %d: %s", code, stderr)
	}
}
