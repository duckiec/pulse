package app_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pulse/internal/app"
	"pulse/internal/engine"
)

const mockOpenAPI = `{
  "openapi": "3.0.3",
  "info": {"title": "mock", "version": "1.0.0"},
  "paths": {
    "/users": {
      "get": {
        "parameters": [
          {"name": "limit", "in": "query", "required": true, "schema": {"type": "integer"}},
          {"name": "X-Trace", "in": "header", "required": true, "schema": {"type": "string"}}
        ],
        "responses": {"200": {"description": "ok"}}
      }
    },
    "/legacy": {
      "get": {"deprecated": true, "responses": {"200": {"description": "ok"}}}
    }
  }
}`

// writeMockSpec writes the OpenAPI document and returns its path.
func writeMockSpec(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "openapi.json")
	if err := os.WriteFile(path, []byte(mockOpenAPI), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return path
}

// TestDriftSpecAgainstMockOpenAPI is the feature gate at the composition root:
// a real spec file is loaded, projected onto engine.SpecOp, and matched against
// .http source. Each kind of drift is represented exactly once.
func TestDriftSpecAgainstMockOpenAPI(t *testing.T) {
	dir := t.TempDir()
	spec := writeMockSpec(t, dir)
	collection := filepath.Join(dir, "collection")
	if err := os.MkdirAll(collection, 0o700); err != nil {
		t.Fatal(err)
	}
	src := "# @name healthy\n" +
		"GET {{base}}/users?limit=5 HTTP/1.1\n" +
		"X-Trace: abc\n\n###\n" +
		"# @name no-header\n" +
		"GET {{base}}/users?limit=5 HTTP/1.1\n\n###\n" +
		"# @name stale\n" +
		"GET {{base}}/legacy HTTP/1.1\n\n###\n" +
		"# @name gone\n" +
		"GET {{base}}/removed HTTP/1.1\n\n###\n"
	if err := os.WriteFile(filepath.Join(collection, "api.http"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := app.DriftSpec(spec, collection)
	if err != nil {
		t.Fatalf("DriftSpec: %v", err)
	}
	if res.SpecVersion != "3.0.3" {
		t.Errorf("SpecVersion = %q, want 3.0.3", res.SpecVersion)
	}
	rep := res.Report
	if rep.Clean() {
		t.Fatal("mock collection drifted but no findings were produced")
	}
	if rep.Requests != 4 || rep.Files != 1 {
		t.Errorf("counts = %d requests / %d files, want 4/1", rep.Requests, rep.Files)
	}

	want := []struct {
		kind engine.DriftKind
		line int32
	}{
		{engine.DriftMissingParam, 7},     // no-header
		{engine.DriftDeprecated, 11},      // stale
		{engine.DriftUnknownEndpoint, 15}, // gone
	}
	if len(rep.Findings) != len(want) {
		t.Fatalf("findings = %v, want %d", rep.Findings, len(want))
	}
	for i, w := range want {
		if rep.Findings[i].Kind != w.kind || rep.Findings[i].Line != w.line {
			t.Errorf("finding %d = %s, want kind %s at line %d",
				i, rep.Findings[i], w.kind, w.line)
		}
	}
	if got := rep.Findings[0].Detail; !strings.Contains(got, `"X-Trace"`) {
		t.Errorf("missing-header detail = %q", got)
	}
	if !strings.Contains(rep.Findings[2].String(), "removed") {
		t.Errorf("unknown-endpoint finding = %q, want the target", rep.Findings[2])
	}
}

// TestDriftSpecReportsInputProblems ensures a drift check that could not run
// says so, rather than returning an empty (clean-looking) report.
func TestDriftSpecReportsInputProblems(t *testing.T) {
	dir := t.TempDir()
	if _, err := app.DriftSpec(filepath.Join(dir, "missing.json"), dir); err == nil {
		t.Error("DriftSpec accepted a missing spec file")
	}
	spec := writeMockSpec(t, dir)
	if _, err := app.DriftSpec(spec, filepath.Join(dir, "no-collection")); err == nil {
		t.Error("DriftSpec accepted a collection path that does not exist")
	}
	if _, err := app.DriftSpec("", dir); err == nil {
		t.Error("DriftSpec accepted an empty spec path")
	}
}
