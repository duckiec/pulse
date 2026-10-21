package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pulse/internal/app"
	"pulse/internal/engine"
)

// TestMCPServerLoadsGuardAndBlocksTheExecutor proves the composition root wires
// the policy where agents actually run: the executor's router is gated, so a
// denied method fails before any dial.
func TestMCPServerLoadsGuardAndBlocksTheExecutor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".pulse-guard")
	if err := os.WriteFile(path, []byte("[mcp]\ndeny_methods = DELETE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newPulseApp(t, func(c *app.Config) { c.GuardFile = path })
	if _, err := a.MCPServer(); err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	if a.Guard == nil {
		t.Fatal("MCPServer did not load a guard")
	}
	if got := a.Guard.Policy().DenyMethods; len(got) != 1 || got[0] != "DELETE" {
		t.Fatalf("DenyMethods = %v, want [DELETE]", got)
	}

	// The target is a closed port: if the gate did not run first, the error
	// would be a dial failure rather than a policy refusal.
	out, err := a.Executor.Execute(context.Background(),
		"DELETE http://127.0.0.1:1/users/7\n", "<inline>", 0, time.Second)
	if err == nil {
		t.Fatalf("DELETE was dispatched (outcome %+v); the guard did not apply", out)
	}
	if !errors.Is(err, engine.ErrPolicyBlocked) {
		t.Fatalf("error = %v, want ErrPolicyBlocked", err)
	}
}

// TestMCPServerRefusesMalformedGuard is the fail-closed gate: a security policy
// Pulse cannot read must stop the agent server, not silently fall back.
func TestMCPServerRefusesMalformedGuard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".pulse-guard")
	if err := os.WriteFile(path, []byte("[mcp]\nallow_host = localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newPulseApp(t, func(c *app.Config) { c.GuardFile = path })
	if _, err := a.MCPServer(); err == nil {
		t.Fatal("MCPServer served with a malformed .pulse-guard")
	}
}

// TestMCPServerDefaultsToLoopbackOnly pins the missing-file baseline.
func TestMCPServerDefaultsToLoopbackOnly(t *testing.T) {
	a := newPulseApp(t, nil) // GuardFile empty: no file
	if _, err := a.MCPServer(); err != nil {
		t.Fatalf("MCPServer: %v", err)
	}
	if a.Guard == nil || !a.Guard.Policy().IsDefault() {
		t.Fatal("a missing .pulse-guard must load the default policy")
	}
	out, err := a.Executor.Execute(context.Background(),
		"GET https://api.example.com/users\n", "<inline>", 0, time.Second)
	if err == nil {
		t.Fatalf("remote host was dispatched (outcome %+v) under the default policy", out)
	}
	if !errors.Is(err, engine.ErrPolicyBlocked) {
		t.Fatalf("error = %v, want ErrPolicyBlocked", err)
	}
}
