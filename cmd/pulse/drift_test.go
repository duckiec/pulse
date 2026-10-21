package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cliSpec = `{
  "openapi": "3.0.3",
  "info": {"title": "cli", "version": "1.0.0"},
  "paths": {
    "/users": {
      "get": {
        "parameters": [
          {"name": "limit", "in": "query", "required": true, "schema": {"type": "integer"}}
        ],
        "responses": {"200": {"description": "ok"}}
      }
    },
    "/old": {
      "get": {"deprecated": true, "responses": {"200": {"description": "ok"}}}
    }
  }
}`

// driftWorkspace writes a spec and one collection file; httpBody is the
// collection source.
func driftWorkspace(t *testing.T, httpBody string) (spec, collection string) {
	t.Helper()
	dir := t.TempDir()
	spec = filepath.Join(dir, "openapi.json")
	if err := os.WriteFile(spec, []byte(cliSpec), 0o600); err != nil {
		t.Fatal(err)
	}
	collection = filepath.Join(dir, "api.http")
	if err := os.WriteFile(collection, []byte(httpBody), 0o600); err != nil {
		t.Fatal(err)
	}
	return spec, collection
}

func runDriftCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr strings.Builder
	code := run(context.Background(), append([]string{"drift"}, args...),
		strings.NewReader(""), &stdout, &stderr, false)
	return code, stdout.String(), stderr.String()
}

func TestDriftCLICleanExitsZero(t *testing.T) {
	spec, collection := driftWorkspace(t,
		"GET {{base}}/users?limit=5 HTTP/1.1\n\n###\n")
	code, out, errOut := runDriftCLI(t, spec, collection)
	if code != exitOK {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, "no violations") {
		t.Errorf("stdout = %q, want a clean summary", out)
	}
}

func TestDriftCLIViolationsExitOneWithLines(t *testing.T) {
	spec, collection := driftWorkspace(t,
		"# @name missing\n"+
			"GET {{base}}/users HTTP/1.1\n\n###\n"+
			"# @name stale\n"+
			"GET {{base}}/old HTTP/1.1\n\n###\n")
	code, out, errOut := runDriftCLI(t, spec, collection)
	if code != exitFail {
		t.Fatalf("exit = %d, want 1\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	if !strings.Contains(out, collection+":2:") {
		t.Errorf("stdout = %q, want file:line for the missing parameter", out)
	}
	if !strings.Contains(out, `missing required query parameter "limit"`) {
		t.Errorf("stdout = %q, want the missing-parameter reason", out)
	}
	if !strings.Contains(out, "[deprecated]") {
		t.Errorf("stdout = %q, want the deprecated finding", out)
	}
	if !strings.Contains(out, "2 violation(s)") {
		t.Errorf("stdout = %q, want the violation count", out)
	}
}

func TestDriftCLIUsageAndInputErrorsExitTwo(t *testing.T) {
	if code, _, _ := runDriftCLI(t); code != exitUsage {
		t.Errorf("no arguments: exit = %d, want 2", code)
	}
	if code, _, _ := runDriftCLI(t, "only-one"); code != exitUsage {
		t.Errorf("one argument: exit = %d, want 2", code)
	}
	spec, _ := driftWorkspace(t, "GET {{base}}/users HTTP/1.1\n\n###\n")
	if code, _, stderr := runDriftCLI(t, spec, filepath.Join(t.TempDir(), "absent")); code != exitUsage {
		t.Errorf("missing collection: exit = %d, want 2 (stderr %s)", code, stderr)
	}
	if code, _, stderr := runDriftCLI(t, filepath.Join(t.TempDir(), "absent.json"), spec); code != exitUsage {
		t.Errorf("missing spec: exit = %d, want 2 (stderr %s)", code, stderr)
	}
}

func TestDriftHelp(t *testing.T) {
	code, out, _ := runDriftCLI(t, "--help")
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "pulse drift <openapi-doc> <file-or-dir>") {
		t.Errorf("help = %q", out)
	}
	code, out, _ = runDriftCLI(t, "help")
	if code != exitOK || !strings.Contains(out, "unknown-endpoint") {
		t.Errorf("drift help = %q (exit %d)", out, code)
	}
}
