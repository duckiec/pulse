package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pulse/internal/ast"
)

// TestInitTemplateParses pins the scaffold contract: the template pulse
// init writes must parse cleanly, or every fresh workspace starts broken.
func TestInitTemplateParses(t *testing.T) {
	arena := ast.AcquireArena()
	defer arena.Release()
	arena.Reset([]byte(quickstartTemplate))
	doc, err := ast.Parse(arena)
	if err != nil {
		t.Fatalf("quickstartTemplate does not parse: %v", err)
	}
	defer doc.Release()
	n := 0
	for range doc.Requests() {
		n++
	}
	if n != 4 {
		t.Fatalf("scaffold holds %d requests, want 4 (GET, POST, authed, signed)", n)
	}
	for _, want := range []string{"{{base}}", "{{DEMO_TOKEN}}", "@wasm-sign"} {
		if !strings.Contains(quickstartTemplate, want) {
			t.Errorf("scaffold omits %q", want)
		}
	}
}

// TestInitRefusesToOverwriteWithoutForce pins the safe-abort: an existing
// file survives `pulse init` unless --force is passed.
func TestInitRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "quickstart.http")
	if err := os.WriteFile(target, []byte("original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	if code := runInit([]string{target}, &stdout, &stderr); code == exitOK {
		t.Fatal("init over an existing file exited 0; want a refusal")
	}
	raw, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "original\n" {
		t.Fatalf("existing file was modified without --force: %q", raw)
	}
	var stdout2, stderr2 strings.Builder
	if code := runInit([]string{target, "--force"}, &stdout2, &stderr2); code != exitOK {
		t.Fatalf("init --force exited %d: %s", code, stderr2.String())
	}
	if _, err := ast.ParseFile(target); err != nil {
		t.Fatalf("overwritten scaffold does not parse: %v", err)
	}
}

// TestInitDefaultsToQuickstart pins the no-arg form: `pulse init` in an
// empty directory scaffolds ./quickstart.http.
func TestInitDefaultsToQuickstart(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	var stdout, stderr strings.Builder
	if code := runInit(nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("init exited %d: %s", code, stderr.String())
	}
	if _, err := ast.ParseFile(filepath.Join(dir, "quickstart.http")); err != nil {
		t.Fatalf("default scaffold does not parse: %v", err)
	}
}

// TestResolveVaultSecretFromPipe pins the non-TTY path: with no VALUE
// argument and a piped stdin, the value is read to EOF and trimmed.
func TestResolveVaultSecretFromPipe(t *testing.T) {
	var stderr strings.Builder
	got, err := resolveVaultSecret(strings.NewReader("piped-value\n"), "K", "", &stderr)
	if err != nil {
		t.Fatalf("pipe read: %v", err)
	}
	if got != "piped-value" {
		t.Fatalf("pipe value = %q, want trimmed input", got)
	}
	if _, err := resolveVaultSecret(strings.NewReader("   \n"), "K", "", &stderr); err == nil {
		t.Fatal("blank piped input was accepted; want fail-closed")
	}
	got, err = resolveVaultSecret(strings.NewReader("ignored"), "K", "explicit", &stderr)
	if err != nil || got != "explicit" {
		t.Fatalf("explicit VALUE = %q, %v; want it to win over stdin", got, err)
	}
}

// TestFileDefinedVarsPinsTheGrammar pins the doctor helper: only `@name =`
// lines define variables; a `@wasm-sign <path>` line and a bare `@var`
// line define nothing.
func TestFileDefinedVarsPinsTheGrammar(t *testing.T) {
	got := fileDefinedVars("@base = http://x\n@wasm-sign ./sign/auth.wasm\n@bare\n# @name r\n")
	seen := map[string]bool{}
	for _, v := range got {
		seen[v] = true
	}
	if !seen["base"] || seen["wasm-sign"] || seen["bare"] {
		t.Fatalf("defined vars = %q, want only [base]", got)
	}
}

// TestDoctorWarnsOnMissingSecrets pins the cross-check: a workspace whose
// .http file references a secret set nowhere exits 3 and names the secret
// — while a file-local @var reference stays silent.
func TestDoctorWarnsOnMissingSecrets(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("PULSE_HOME", home)
	t.Setenv("PULSE_SKIP_SYNC", "1")
	t.Setenv("PULSE_VAULT_FILE", filepath.Join(dir, ".pulse.vault"))
	t.Setenv("PULSE_VAULT_KEY_ID", "doctor-missing")
	if err := os.WriteFile(filepath.Join(dir, "api.http"), []byte(
		"@base = http://localhost:8080\n\n# @name authed\nGET {{base}}/me HTTP/1.1\nAuthorization: Bearer {{DOCTOR_MISSING_XYZ}}\n\n###\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	var stdout, stderr strings.Builder
	code := run(context.Background(), []string{"doctor"}, strings.NewReader(""), &stdout, &stderr, false)
	if code != exitSec {
		t.Fatalf("doctor exited %d, want %d (stderr: %s)", code, exitSec, stderr.String())
	}
	if !strings.Contains(stderr.String(), "DOCTOR_MISSING_XYZ") {
		t.Fatalf("doctor did not name the missing secret (stderr: %s)", stderr.String())
	}
	if strings.Contains(stdout.String(), "DOCTOR_MISSING_XYZ") && strings.Contains(stdout.String(), "value") {
		t.Fatal("doctor stdout leaks secret-adjacent content")
	}
	// The MCP block is always printed: the absolute binary path must appear
	// on stdout even when the workspace has findings.
	if !strings.Contains(stdout.String(), "mcpServers") {
		t.Fatalf("doctor stdout carries no mcpServers block: %q", stdout.String())
	}
}

// TestDoctorIsCleanOnInitScaffold pins the green path: `pulse init` output
// with the demo token defined file-locally (rewritten here as a @var so no
// vault is needed) reports healthy — proving file-local variables never
// count as missing secrets.
func TestDoctorIsCleanOnInitScaffold(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("PULSE_HOME", home)
	t.Setenv("PULSE_SKIP_SYNC", "1")
	t.Setenv("PULSE_VAULT_FILE", filepath.Join(dir, ".pulse.vault"))
	t.Setenv("PULSE_VAULT_KEY_ID", "doctor-clean")
	if err := os.WriteFile(filepath.Join(dir, "api.http"), []byte(
		"@base = http://localhost:8080\n@tok = fixture\n\n# @name authed\nGET {{base}}/me HTTP/1.1\nAuthorization: Bearer {{tok}}\n\n###\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	var stdout, stderr strings.Builder
	code := run(context.Background(), []string{"doctor"}, strings.NewReader(""), &stdout, &stderr, false)
	if code != exitOK {
		t.Fatalf("doctor exited %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "healthy") {
		t.Fatalf("clean doctor did not report healthy (stderr: %s)", stderr.String())
	}
}

// TestDoctorReportsSyntaxErrorsAsExitTwo pins the exit mapping: a broken
// .http file is exit 2 (parse error, AGENTS.md §4.4), not a generic 1.
func TestDoctorReportsSyntaxErrorsAsExitTwo(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("PULSE_HOME", home)
	t.Setenv("PULSE_SKIP_SYNC", "1")
	t.Setenv("PULSE_VAULT_FILE", filepath.Join(dir, ".pulse.vault"))
	t.Setenv("PULSE_VAULT_KEY_ID", "doctor-syntax")
	if err := os.WriteFile(filepath.Join(dir, "bad.http"), []byte("@broken-without-equals\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	var stdout, stderr strings.Builder
	code := run(context.Background(), []string{"doctor"}, strings.NewReader(""), &stdout, &stderr, false)
	if code != exitUsage {
		t.Fatalf("doctor over a syntax error exited %d, want %d (stderr: %s)", code, exitUsage, stderr.String())
	}
	if !strings.Contains(stderr.String(), "syntax:") {
		t.Fatalf("doctor did not report the syntax line (stderr: %s)", stderr.String())
	}
}
