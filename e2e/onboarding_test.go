//go:build e2e

package e2e_test

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This file is Phase 2.5's black-box consolidation: the Phase 2 flows
// (init, doctor, one-step vaulting, MCP authoring surface) driven as a
// real OS process against the compiled binary — no in-process shortcuts.
// It runs as stage 7 of the crucible, in its own subdirectory, so its
// deliberate syntax errors and missing secrets can never pollute another
// stage's workspace.

// runIn mirrors runStdin but inside dir: init/doctor/vault resolve the
// workspace from the process directory, exactly as a user experiences it.
func runIn(t *testing.T, dir, stdin, bin string, penv []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), penv...)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	_, _ = io.WriteString(in, stdin)
	_ = in.Close()
	err = cmd.Wait()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("wait %v: %v", args, err)
		}
	}
	return result{exit: code, stdout: so.String(), stderr: se.String()}
}

func stageOnboarding(t *testing.T, bin, ws string, tr *transcriptLog) {
	t.Helper()
	dir := filepath.Join(ws, "onboard")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Fresh key id: stage 1 proved the keyring works, and a fresh id
	// proves the auto-bootstrap (no identity + no vault → mint + seal).
	// PULSE_VAULT_FILE is deliberately UNSET: the default .pulse.vault
	// in the invocation directory is what must appear.
	penv := []string{
		env("PULSE_HOME", filepath.Join(ws, "home")),
		env("PULSE_VAULT_KEY_ID", "e2e-onboard"),
		env("PULSE_SKIP_SYNC", "1"),
	}

	stageInit(t, bin, dir, penv, tr)
	stageDoctor(t, bin, dir, penv, tr)
	stageVaultSetStdin(t, bin, dir, penv, tr)
	stageHeadlessMCP(t, bin, penv, tr)
	t.Logf("stage 7: init, doctor (0/2/3), vault set over stdin, headless mcp")
}

// stageInit pins `pulse init`: scaffold creation, safe-abort refusal,
// and --force overwrite — all against the real binary.
func stageInit(t *testing.T, bin, dir string, penv []string, tr *transcriptLog) {
	t.Helper()
	r := runIn(t, dir, "", bin, penv, "init")
	if r.exit != 0 {
		t.Fatalf("init: exit %d:\n%s", r.exit, r.stderr)
	}
	target := filepath.Join(dir, "quickstart.http")
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("init created no scaffold: %v", err)
	}
	for _, want := range []string{"###", "{{DEMO_TOKEN}}", "@wasm-sign", "# @assert"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("scaffold omits %q", want)
		}
	}
	tr.add(r.stderr)

	r = runIn(t, dir, "", bin, penv, "init")
	if r.exit == 0 {
		t.Fatal("second init over an existing file exited 0; want a refusal")
	}
	r = runIn(t, dir, "", bin, penv, "init", "--force")
	if r.exit != 0 {
		t.Fatalf("init --force: exit %d:\n%s", r.exit, r.stderr)
	}
}

// stageDoctor pins the exit-code contract black-box: 0 on a clean
// workspace, 2 on a syntax error, 3 on a missing secret — each in its
// own subdirectory so findings cannot leak across cases.
func stageDoctor(t *testing.T, bin, dir string, penv []string, tr *transcriptLog) {
	t.Helper()
	clean := filepath.Join(dir, "clean")
	bad := filepath.Join(dir, "bad")
	missing := filepath.Join(dir, "missing")
	for _, d := range []string{clean, bad, missing} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(clean, "ok.http"), []byte(
		"@base = http://localhost:8080\n\n# @name ok\nGET {{base}}/ok HTTP/1.1\n\n###\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "bad.http"), []byte("@broken-without-equals\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(missing, "secret.http"), []byte(
		"GET http://localhost:8080/me HTTP/1.1\nAuthorization: Bearer {{ONBOARD_MISSING_XYZ}}\n\n###\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := runIn(t, clean, "", bin, penv, "doctor")
	if r.exit != 0 {
		t.Fatalf("doctor on a clean workspace: exit %d:\n%s", r.exit, r.stderr)
	}
	if !strings.Contains(r.stderr, "healthy") {
		t.Fatalf("clean doctor did not report healthy:\n%s", r.stderr)
	}
	if !strings.Contains(r.stdout, "mcpServers") {
		t.Fatal("clean doctor printed no mcpServers block on stdout")
	}
	tr.add(r.stderr)

	r = runIn(t, bad, "", bin, penv, "doctor")
	if r.exit != 2 {
		t.Fatalf("doctor over a syntax error: exit %d, want 2:\n%s", r.exit, r.stderr)
	}
	if !strings.Contains(r.stderr, "syntax:") {
		t.Fatalf("doctor named no syntax line:\n%s", r.stderr)
	}

	r = runIn(t, missing, "", bin, penv, "doctor")
	if r.exit != 3 {
		t.Fatalf("doctor over a missing secret: exit %d, want 3:\n%s", r.exit, r.stderr)
	}
	if !strings.Contains(r.stderr, "ONBOARD_MISSING_XYZ") || !strings.Contains(r.stderr, "pulse vault set") {
		t.Fatalf("doctor named no secret and no remedy:\n%s", r.stderr)
	}
	tr.add(r.stderr)
}

// stageVaultSetStdin pins one-step vaulting over a pipe: no VALUE
// argument, the secret on stdin — then list proves names-only output.
func stageVaultSetStdin(t *testing.T, bin, dir string, penv []string, tr *transcriptLog) {
	t.Helper()
	r := runIn(t, dir, "piped-secret-value\n", bin, penv, "vault", "set", "ONBOARD_TOKEN")
	if r.exit != 0 {
		t.Fatalf("vault set over stdin: exit %d:\n%s", r.exit, r.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".pulse.vault")); err != nil {
		t.Fatalf("no .pulse.vault after the bootstrapping set: %v", err)
	}
	tr.add(r.stderr)

	r = runIn(t, dir, "", bin, penv, "vault", "list")
	if r.exit != 0 {
		t.Fatalf("vault list: exit %d:\n%s", r.exit, r.stderr)
	}
	if strings.TrimSpace(r.stdout) != "ONBOARD_TOKEN" {
		t.Fatalf("vault list = %q, want the sealed name", r.stdout)
	}
	if strings.Contains(r.stdout, "piped-secret-value") {
		t.Fatal("vault list leaks the value")
	}

	r = runIn(t, dir, "", bin, penv, "vault", "set", "ONBOARD_B", "second-value")
	if r.exit != 0 {
		t.Fatalf("vault set with an argument value: exit %d:\n%s", r.exit, r.stderr)
	}
	r = runIn(t, dir, "", bin, penv, "vault", "list")
	if r.stdout != "ONBOARD_B\nONBOARD_TOKEN\n" {
		t.Fatalf("vault list = %q, want both names sorted", r.stdout)
	}
}

// stageHeadlessMCP pins the autonomous surface black-box: tools/list
// advertises the six authoring tools and get_http_syntax_spec answers —
// then stdin EOF ends the session cleanly with exit 0.
func stageHeadlessMCP(t *testing.T, bin string, penv []string, tr *transcriptLog) {
	t.Helper()
	p := start(t, bin, penv, "mcp")
	c := &mcpConn{t: t, p: p}
	c.rpc("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "crucible-onboard", "version": "0"},
	})
	c.notify("notifications/initialized", map[string]any{})

	res := c.rpc("tools/list", map[string]any{})
	names := map[string]bool{}
	if tools, ok := res["tools"].([]any); ok {
		for _, ti := range tools {
			if tm, ok := ti.(map[string]any); ok {
				if n, ok := tm["name"].(string); ok {
					names[n] = true
				}
			}
		}
	}
	for _, want := range []string{"execute_request", "get_http_syntax_spec",
		"create_collection", "upsert_request", "delete_request",
		"import_curl", "validate_collection"} {
		if !names[want] {
			t.Fatalf("headless tools/list is missing %q: %v", want, names)
		}
	}
	text, _ := c.callTool("get_http_syntax_spec", map[string]any{})
	if !strings.Contains(text, "###") || !strings.Contains(text, "pulse vault set") {
		t.Fatalf("headless spec carries no grammar: %q", text)
	}
	code, stderr := p.kill(t)
	if code != 0 {
		t.Fatalf("headless mcp exited %d on stdin EOF, want 0 (stderr: %s)", code, stderr)
	}
	tr.add(stderr)
}
