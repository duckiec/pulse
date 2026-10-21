package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDriftHTTP writes one .http file into dir and returns its path.
func writeDriftHTTP(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

// usersSpec is the fixture shared by the matching cases: one deprecated
// operation, one required query/header pair, and a templated path.
func usersSpec() []SpecOp {
	return []SpecOp{
		{
			Method: "GET", Path: "/users",
			RequiredQuery:  []string{"limit"},
			RequiredHeader: []string{"X-Trace"},
		},
		{
			Method: "DELETE", Path: "/users/{id}",
			Deprecated: true,
		},
		{Method: "GET", Path: "/users/{id}"},
	}
}

func TestDriftCleanRequestPasses(t *testing.T) {
	dir := t.TempDir()
	file := writeDriftHTTP(t, dir, "api.http",
		"# @name list\n"+
			"GET {{base}}/users?limit=10 HTTP/1.1\n"+
			"X-Trace: abc\n\n###\n")
	rep, err := CheckDrift(usersSpec(), []string{file})
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("findings = %v, want none", rep.Findings)
	}
	if rep.Requests != 1 || rep.Files != 1 || rep.Operations != 3 {
		t.Fatalf("counts = %d requests, %d files, %d ops; want 1/1/3",
			rep.Requests, rep.Files, rep.Operations)
	}
}

func TestDriftMissingRequiredQueryAndHeader(t *testing.T) {
	dir := t.TempDir()
	file := writeDriftHTTP(t, dir, "api.http", "GET {{base}}/users HTTP/1.1\n\n###\n")
	rep, err := CheckDrift(usersSpec(), []string{file})
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(rep.Findings) != 2 {
		t.Fatalf("findings = %v, want 2", rep.Findings)
	}
	want := map[string]string{"limit": "query", "X-Trace": "header"}
	for _, f := range rep.Findings {
		if f.Kind != DriftMissingParam {
			t.Errorf("kind = %q, want %q", f.Kind, DriftMissingParam)
		}
		if got, ok := want[f.Param]; !ok || f.In != got {
			t.Errorf("finding = %+v, want param %s in %s", f, f.Param, got)
		}
		delete(want, f.Param)
	}
	if len(want) != 0 {
		t.Errorf("missing findings for %v", want)
	}
	if !strings.Contains(rep.Findings[0].Detail, `missing required`) {
		t.Errorf("detail = %q, want a readable sentence", rep.Findings[0].Detail)
	}
}

func TestDriftEmptyQueryValueCountsAsPresent(t *testing.T) {
	dir := t.TempDir()
	file := writeDriftHTTP(t, dir, "api.http",
		"GET {{base}}/users?limit= HTTP/1.1\nX-Trace: t\n\n###\n")
	rep, err := CheckDrift(usersSpec(), []string{file})
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("findings = %v: a present name with an empty value satisfies a required parameter", rep.Findings)
	}
}

func TestDriftDeprecatedEndpoint(t *testing.T) {
	dir := t.TempDir()
	file := writeDriftHTTP(t, dir, "api.http", "DELETE {{base}}/users/7 HTTP/1.1\n\n###\n")
	rep, err := CheckDrift(usersSpec(), []string{file})
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != DriftDeprecated {
		t.Fatalf("findings = %v, want one deprecated finding", rep.Findings)
	}
	if got := rep.Findings[0].String(); !strings.Contains(got, "DELETE /users/{id} is marked deprecated") {
		t.Errorf("string = %q, want it to name the deprecated operation", got)
	}
}

func TestDriftUnknownPathAndMethodMismatch(t *testing.T) {
	dir := t.TempDir()
	unknown := writeDriftHTTP(t, dir, "a.http", "GET {{base}}/nope HTTP/1.1\n\n###\n")
	method := writeDriftHTTP(t, dir, "b.http", "POST {{base}}/users HTTP/1.1\n\n###\n")
	rep, err := CheckDrift(usersSpec(), []string{unknown, method})
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(rep.Findings) != 2 {
		t.Fatalf("findings = %v, want 2", rep.Findings)
	}
	if rep.Findings[0].Kind != DriftUnknownEndpoint ||
		!strings.Contains(rep.Findings[0].Detail, "no path in the spec matches") {
		t.Errorf("unknown-path finding = %+v", rep.Findings[0])
	}
	if rep.Findings[1].Kind != DriftUnknownEndpoint ||
		!strings.Contains(rep.Findings[1].Detail, "declares GET for /users, not POST") {
		t.Errorf("method-mismatch finding = %+v", rep.Findings[1])
	}
}

func TestDriftTemplatedPathAndTrailingSlash(t *testing.T) {
	dir := t.TempDir()
	file := writeDriftHTTP(t, dir, "api.http",
		"GET {{base}}/users/42/ HTTP/1.1\n\n###\n")
	rep, err := CheckDrift(usersSpec(), []string{file})
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("findings = %v: /users/42/ must match GET /users/{id}", rep.Findings)
	}
}

func TestDriftUnresolvedTargetIsReportedNotSkipped(t *testing.T) {
	dir := t.TempDir()
	file := writeDriftHTTP(t, dir, "api.http", "GET users/42 HTTP/1.1\n\n###\n")
	rep, err := CheckDrift(usersSpec(), []string{file})
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(rep.Findings) != 1 || rep.Findings[0].Kind != DriftUnresolvedTarget {
		t.Fatalf("findings = %v, want one unresolved-target finding", rep.Findings)
	}
}

func TestDriftLineNumbersAndOrder(t *testing.T) {
	dir := t.TempDir()
	file := writeDriftHTTP(t, dir, "api.http",
		"# @name ok\n"+
			"GET {{base}}/users?limit=1 HTTP/1.1\n"+
			"X-Trace: t\n\n###\n"+
			"# @name stale\n"+
			"GET {{base}}/missing HTTP/1.1\n\n###\n")
	rep, err := CheckDrift(usersSpec(), []string{file})
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(rep.Findings) != 1 {
		t.Fatalf("findings = %v, want 1", rep.Findings)
	}
	f := rep.Findings[0]
	if f.Line != 7 {
		t.Errorf("line = %d, want 7", f.Line)
	}
	if f.Name != "stale" {
		t.Errorf("name = %q, want stale", f.Name)
	}
}

func TestDriftAbsoluteURLContributesItsPath(t *testing.T) {
	dir := t.TempDir()
	file := writeDriftHTTP(t, dir, "api.http",
		"GET https://api.example.com/users?limit=1 HTTP/1.1\nX-Trace: t\n\n###\n")
	rep, err := CheckDrift(usersSpec(), []string{file})
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if !rep.Clean() {
		t.Fatalf("findings = %v: an absolute URL must be checked by its path", rep.Findings)
	}
}

func TestDriftParseErrorIsFatal(t *testing.T) {
	dir := t.TempDir()
	file := writeDriftHTTP(t, dir, "broken.http", "GET {{base}/users HTTP/1.1\n\n###\n")
	if _, err := CheckDrift(usersSpec(), []string{file}); err == nil {
		t.Fatal("CheckDrift accepted a file that does not parse")
	}
}

func TestDriftNoFiles(t *testing.T) {
	if _, err := CheckDrift(usersSpec(), nil); err == nil {
		t.Fatal("CheckDrift accepted an empty file list")
	}
}

func TestDriftMultipleFilesAreSortedByLine(t *testing.T) {
	dir := t.TempDir()
	a := writeDriftHTTP(t, dir, "a.http", "GET {{base}}/one HTTP/1.1\n\n###\n")
	b := writeDriftHTTP(t, dir, "sub/b.http", "GET {{base}}/two HTTP/1.1\n\n###\n")
	rep, err := CheckDrift(usersSpec(), []string{a, b})
	if err != nil {
		t.Fatalf("CheckDrift: %v", err)
	}
	if len(rep.Findings) != 2 {
		t.Fatalf("findings = %v, want 2", rep.Findings)
	}
	if rep.Findings[0].File != a || rep.Findings[1].File != b {
		t.Fatalf("order = %s then %s, want submission order", rep.Findings[0].File, rep.Findings[1].File)
	}
}
