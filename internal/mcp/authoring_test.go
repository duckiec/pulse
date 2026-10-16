package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// scanRedactor is the test double proving the chokepoint's probe is an
// interface assertion, not a concrete-type coupling: it implements
// String (the Redactor contract) plus Contains (the optional probe).
type scanRedactor struct {
	registered map[string]bool
}

func (s *scanRedactor) String(v string) string { return v }

func (s *scanRedactor) Contains(v string) bool {
	for k := range s.registered {
		if strings.Contains(v, k) {
			return true
		}
	}
	return false
}

// callAuthoringTool drives one authoring tool through the real server
// dispatch — handler, chokepoint, jail, and framing — and returns the
// decoded result envelope.
func callAuthoringTool(t *testing.T, srv *Server, tool string, args map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := srv.HandleMessage(context.Background(), raw)
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("response is not a JSON object: %v (%s)", err, out)
	}
	res, ok := decoded["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result: %s", out)
	}
	return res
}

// toolText concatenates a result's text blocks; isError reports the flag.
func toolText(t *testing.T, res map[string]any) (string, bool) {
	t.Helper()
	content, ok := res["content"].([]any)
	if !ok {
		t.Fatalf("result has no content array: %v", res)
	}
	var b strings.Builder
	for _, blk := range content {
		if m, ok := blk.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
			}
		}
	}
	isErr, _ := res["isError"].(bool)
	return b.String(), isErr
}

func authoringServer(t *testing.T, dir string, red Redactor, lookup func(string) (string, error)) *Server {
	t.Helper()
	srv, err := NewServerWithConfig(Config{
		Exec:      stubExecutorForHistory{},
		Authoring: &Authoring{Root: dir, Redactor: red, Lookup: lookup},
	})
	if err != nil {
		t.Fatalf("NewServerWithConfig: %v", err)
	}
	return srv
}

// TestAuthoringSpecIsAuthoritative pins the LLM contract: the spec names
// every grammar element an author must not invent.
func TestAuthoringSpecIsAuthoritative(t *testing.T) {
	srv := authoringServer(t, t.TempDir(), nil, nil)
	text, isErr := toolText(t, callAuthoringTool(t, srv, ToolGetHTTPSyntaxSpec, map[string]any{}))
	if isErr {
		t.Fatalf("spec call is an error: %s", text)
	}
	for _, want := range []string{"###", "{{", "@wasm-sign", "@name", "@assert", "HTTP/1.1", "pulse vault set"} {
		if !strings.Contains(text, want) {
			t.Errorf("spec omits %q", want)
		}
	}
}

// TestAuthoringNilOmitsAllSix pins the degradation rule: no Authoring
// config means no authoring tools advertised.
func TestAuthoringNilOmitsAllSix(t *testing.T) {
	srv, err := NewServerWithConfig(Config{Exec: stubExecutorForHistory{}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	resp := srv.HandleMessage(context.Background(), raw)
	out, _ := json.Marshal(resp)
	for _, name := range []string{ToolGetHTTPSyntaxSpec, ToolCreateCollection, ToolUpsertRequest, ToolDeleteRequest, ToolImportCurl, ToolValidateCollect} {
		if strings.Contains(string(out), name) {
			t.Errorf("tools/list advertises %q with no Authoring configured", name)
		}
	}
}

// TestAuthoringCreateUpsertDeleteRoundTrip pins the write path: create,
// append-new, replace, and delete all land through the Phase 2.1
// serializer with neighbors intact.
func TestAuthoringCreateUpsertDeleteRoundTrip(t *testing.T) {
	dir := t.TempDir()
	srv := authoringServer(t, dir, nil, nil)

	first := "# @name one\nGET http://localhost:8080/one HTTP/1.1\n\n###\n"
	if text, isErr := toolText(t, callAuthoringTool(t, srv, ToolCreateCollection,
		map[string]any{"path": "api.http", "content": first})); isErr {
		t.Fatalf("create: %s", text)
	}
	second := "# @name two\nGET http://localhost:8080/two HTTP/1.1\n\n###\n"
	if text, isErr := toolText(t, callAuthoringTool(t, srv, ToolUpsertRequest,
		map[string]any{"path": "api.http", "name": "two", "block": second})); isErr {
		t.Fatalf("upsert-new: %s", text)
	}
	replacement := "# @name one\n# @assert status == 200\nGET http://localhost:8080/one-v2 HTTP/1.1\n\n###\n"
	if text, isErr := toolText(t, callAuthoringTool(t, srv, ToolUpsertRequest,
		map[string]any{"path": "api.http", "name": "one", "block": replacement})); isErr {
		t.Fatalf("upsert-replace: %s", text)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "api.http"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "one-v2") || !strings.Contains(string(raw), "two") {
		t.Fatalf("spliced file lost a request: %q", raw)
	}
	if text, isErr := toolText(t, callAuthoringTool(t, srv, ToolDeleteRequest,
		map[string]any{"path": "api.http", "name": "two"})); isErr {
		t.Fatalf("delete: %s", text)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "api.http"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "two") {
		t.Fatalf("deleted request survives: %q", raw)
	}
}

// TestAuthoringChokepointRefusesCanary is THE Phase 2.4 gate: a write
// carrying a registered secret value (header and body shapes) is refused
// with the actionable message and leaves zero bytes behind — a refused
// create creates nothing, a refused upsert changes nothing.
func TestAuthoringChokepointRefusesCanary(t *testing.T) {
	const canary = "CANARY-9f31-live-proof-token"
	dir := t.TempDir()
	red := &scanRedactor{registered: map[string]bool{canary: true}}
	srv := authoringServer(t, dir, red, nil)

	headerBlock := "# @name evil\nGET http://localhost:8080/x HTTP/1.1\nAuthorization: Bearer " + canary + "\n\n###\n"
	text, isErr := toolText(t, callAuthoringTool(t, srv, ToolCreateCollection,
		map[string]any{"path": "evil.http", "content": headerBlock}))
	if !isErr {
		t.Fatal("a header carrying the canary was written; want a refusal")
	}
	if !strings.Contains(text, "Refused to write literal credential") || !strings.Contains(text, "pulse vault set") {
		t.Fatalf("refusal is not actionable: %q", text)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.http")); !os.IsNotExist(err) {
		t.Fatal("the refused create left a file behind: zero bytes means zero bytes")
	}

	// A clean file exists; a refused upsert must leave it byte-identical.
	clean := "# @name ok\nGET http://localhost:8080/ok HTTP/1.1\n\n###\n"
	if text, isErr := toolText(t, callAuthoringTool(t, srv, ToolCreateCollection,
		map[string]any{"path": "ok.http", "content": clean})); isErr {
		t.Fatalf("clean create: %s", text)
	}
	bodyBlock := "# @name ok\nPOST http://localhost:8080/ok HTTP/1.1\nContent-Type: application/json\n\n{\"token\": \"" + canary + "\"}\n\n###\n"
	if text, isErr := toolText(t, callAuthoringTool(t, srv, ToolUpsertRequest,
		map[string]any{"path": "ok.http", "name": "ok", "block": bodyBlock})); !isErr {
		t.Fatalf("a body carrying the canary was written: %s", text)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ok.http"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != clean {
		t.Fatalf("the refused upsert modified the file: %q", raw)
	}
	if strings.Contains(string(raw), canary) {
		t.Fatal("the canary reached the disk")
	}
}

// TestAuthoringHeuristicsFireWithoutAScanner pins the defense-in-depth
// legs: with NO redactor bound, a pasted Bearer token, an api-key header,
// and a smuggled .env payload are still refused.
func TestAuthoringHeuristicsFireWithoutAScanner(t *testing.T) {
	dir := t.TempDir()
	srv := authoringServer(t, dir, nil, nil)
	cases := map[string]string{
		"bearer":   "# @name b\nGET http://localhost:8080/x HTTP/1.1\nAuthorization: Bearer abcdefghijklmnopqrst1234\n\n###\n",
		"api-key":  "# @name k\nGET http://localhost:8080/x HTTP/1.1\nX-Api-Key: live-key-abcdef1234567890\n\n###\n",
		"dotenv":   "# @name e\nPOST http://localhost:8080/x HTTP/1.1\nContent-Type: text/plain\n\nDB_USER=root\nDB_PASS=s3cr3t-value-here\n\n###\n",
		"userinfo": "# @name u\nGET https://admin:s3cr3t-pw@internal.local/x HTTP/1.1\n\n###\n",
	}
	for name, block := range cases {
		text, isErr := toolText(t, callAuthoringTool(t, srv, ToolCreateCollection,
			map[string]any{"path": name + ".http", "content": block}))
		if !isErr || !strings.Contains(text, "Refused to write literal credential") {
			t.Errorf("%s: written or wrongly refused: isErr=%v text=%q", name, isErr, text)
		}
	}
}

// TestAuthoringPlaceholdersPass pins the non-refusal: placeholder
// references (the shape the importer emits) sail through every leg.
func TestAuthoringPlaceholdersPass(t *testing.T) {
	dir := t.TempDir()
	srv := authoringServer(t, dir, nil, nil)
	block := "# @name authed\nGET http://localhost:8080/me HTTP/1.1\nAuthorization: Bearer {{DEMO_TOKEN}}\nX-Api-Key: {{API_KEY}}\n\n###\n"
	if text, isErr := toolText(t, callAuthoringTool(t, srv, ToolCreateCollection,
		map[string]any{"path": "auth.http", "content": block})); isErr {
		t.Fatalf("placeholder references were refused: %s", text)
	}
}

// TestAuthoringJailConfinesWrites pins the confinement: absolute paths
// and `..` escapes never reach the serializer.
func TestAuthoringJailConfinesWrites(t *testing.T) {
	dir := t.TempDir()
	srv := authoringServer(t, dir, nil, nil)
	for _, rel := range []string{"../evil.http", "/tmp/evil.http", "sub/../../evil.http"} {
		text, isErr := toolText(t, callAuthoringTool(t, srv, ToolCreateCollection,
			map[string]any{"path": rel, "content": "# @name x\nGET http://localhost:8080/x HTTP/1.1\n\n###\n"}))
		if !isErr {
			t.Errorf("escape path %q was written: %s", rel, text)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "evil.http")); !os.IsNotExist(err) {
		t.Fatal("an escape write landed inside the workspace")
	}
}

// TestImportCurlSanitizes pins the import flow: credentials in the command
// become placeholders on disk, and the response carries placeholder NAMES
// and seal instructions — never raw values.
func TestImportCurlSanitizes(t *testing.T) {
	dir := t.TempDir()
	srv := authoringServer(t, dir, nil, nil)
	res := callAuthoringTool(t, srv, ToolImportCurl, map[string]any{
		"command": "curl -H 'Authorization: Bearer raw-token-abc123' http://localhost:8080/me",
		"path":    "imp.http",
		"name":    "imported",
	})
	text, isErr := toolText(t, res)
	if isErr {
		t.Fatalf("sanitized import refused: %s", text)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "imp.http"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "raw-token-abc123") {
		t.Fatalf("the raw token reached the disk: %q", raw)
	}
	if !strings.Contains(string(raw), "{{BEARER_TOKEN}}") {
		t.Fatalf("the import carries no placeholder: %q", raw)
	}
	out, _ := json.Marshal(res)
	if strings.Contains(string(out), "raw-token-abc123") {
		t.Fatal("the raw token reached the tool response")
	}
	if !strings.Contains(string(out), "BEARER_TOKEN") || !strings.Contains(string(out), "pulse vault set") {
		t.Fatalf("the response names no seal instruction: %s", out)
	}
}

// TestValidateCollectionPresentVsMissing pins the cross-check: file-local
// @vars and Lookup-sealed names report present, unknown names report
// missing with the seal remedy, and syntax errors surface as diagnostics.
func TestValidateCollectionPresentVsMissing(t *testing.T) {
	dir := t.TempDir()
	content := "@base = http://localhost:8080\n\n" +
		"# @name authed\nGET {{base}}/me HTTP/1.1\nAuthorization: Bearer {{SEALED_TOKEN}}\nX-Api-Key: {{MISSING_XYZ}}\n\n###\n"
	if err := os.WriteFile(filepath.Join(dir, "api.http"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.http"), []byte("@broken-without-equals\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (string, error) {
		if name == "SEALED_TOKEN" {
			return "the-actual-secret-value", nil
		}
		return "", errMissingForTest(name)
	}
	srv := authoringServer(t, dir, nil, lookup)
	res := callAuthoringTool(t, srv, ToolValidateCollect, map[string]any{})
	text, isErr := toolText(t, res)
	if isErr {
		t.Fatalf("validate is an error result: %s", text)
	}
	out, _ := json.Marshal(res["structuredContent"])
	got := string(out)
	for _, want := range []string{"MISSING_XYZ", "bad.http", "SEALED_TOKEN", "base"} {
		if !strings.Contains(got, want) {
			t.Errorf("validate report omits %q: %s", want, got)
		}
	}
	if strings.Contains(got, "the-actual-secret-value") {
		t.Fatal("validate leaked a resolved value: key names only")
	}
	if !strings.Contains(text, "missing: MISSING_XYZ") || !strings.Contains(text, "pulse vault set MISSING_XYZ") {
		t.Fatalf("validate text names no remedy: %s", text)
	}
}

// errMissingForTest is a non-nil lookup miss. It must NOT wrap
// ErrUnresolved or any secret text: the value boundary is key-names-only.
type testLookupMiss struct{ key string }

func (e *testLookupMiss) Error() string { return "no such variable " + e.key }

func errMissingForTest(key string) error { return &testLookupMiss{key: key} }
