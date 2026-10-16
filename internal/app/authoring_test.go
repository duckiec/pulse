package app_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// callToolRaw invokes a tool and decodes the result envelope WITHOUT
// failing on isError: refusal paths are results, not protocol errors, and
// the gate must observe them rather than die on them.
func callToolRaw(s *rpcSession, name string, args map[string]any) (text string, isErr bool, structured map[string]any) {
	s.t.Helper()
	res := s.call("tools/call", map[string]any{"name": name, "arguments": args})
	if res.Error != nil {
		s.t.Fatalf("tools/call %s returned a protocol error: %d %s", name, res.Error.Code, res.Error.Message)
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent map[string]any `json:"structuredContent"`
		IsError           bool           `json:"isError"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		s.t.Fatalf("decode the tool result: %v (%s)", err, res.raw)
	}
	for _, c := range out.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	return text, out.IsError, out.StructuredContent
}

// TestAuthoringLoopEndToEnd drives the complete Phase 2.4 loop over real
// stdin/stdout pipes against a live target: spec → create → upsert →
// execute → validate. Every step travels the production path (stdio loop,
// real server, real engine), so the loop proves the wiring, not the units.
func TestAuthoringLoopEndToEnd(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"hello":"world"}`))
	}))
	defer backend.Close()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.Chdir(ws); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	a := newTestApp(t)
	s := startSession(t, a)

	text, _, _ := callToolRaw(s, "get_http_syntax_spec", map[string]any{})
	if !strings.Contains(text, "###") {
		t.Fatalf("spec carries no grammar: %q", text)
	}

	createBlock := "# @name hello\n# @assert status == 200\nGET " + backend.URL + "/hello HTTP/1.1\nAccept: application/json\n\n###\n"
	if text, isErr, _ := callToolRaw(s, "create_collection",
		map[string]any{"path": "api.http", "content": createBlock}); isErr {
		t.Fatalf("create_collection: %s", text)
	}

	upsertBlock := "# @name echo\n# @assert status == 200\nGET " + backend.URL + "/echo HTTP/1.1\nAccept: application/json\n\n###\n"
	if text, isErr, _ := callToolRaw(s, "upsert_request",
		map[string]any{"path": "api.http", "name": "echo", "block": upsertBlock}); isErr {
		t.Fatalf("upsert_request: %s", text)
	}

	text, _, structured := callToolRaw(s, "execute_request",
		map[string]any{"path": "api.http", "index": 1})
	if !strings.Contains(text, "200") {
		t.Fatalf("execute_request did not run the upserted block: %s", text)
	}
	_ = structured

	text, isErr, _ := callToolRaw(s, "validate_collection", map[string]any{"path": "api.http"})
	if isErr {
		t.Fatalf("validate_collection: %s", text)
	}
	if !strings.Contains(text, "valid") {
		t.Fatalf("authored workspace does not validate: %s", text)
	}

	// The on-disk file holds both requests exactly as authored.
	raw, err := os.ReadFile(filepath.Join(ws, "api.http"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "hello") || !strings.Contains(string(raw), "echo") {
		t.Fatalf("workspace file lost a request: %q", raw)
	}
}

// TestAuthoringLoopRefusesCanaryEndToEnd proves the chokepoint over the
// wire: an AI write carrying a live vault value is refused with the
// actionable message, and the workspace file is byte-identical afterwards.
func TestAuthoringLoopRefusesCanaryEndToEnd(t *testing.T) {
	const canary = "CANARY-e2e-7d42-live-vault-value"
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	if err := os.Chdir(ws); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(cwd) }()

	a := newTestApp(t)
	a.Redactor.Register(canary)
	s := startSession(t, a)

	clean := "# @name ok\nGET http://localhost:8080/ok HTTP/1.1\n\n###\n"
	if text, isErr, _ := callToolRaw(s, "create_collection",
		map[string]any{"path": "ok.http", "content": clean}); isErr {
		t.Fatalf("clean create: %s", text)
	}

	evil := "# @name ok\nGET http://localhost:8080/ok HTTP/1.1\nAuthorization: Bearer " + canary + "\n\n###\n"
	text, isErr, _ := callToolRaw(s, "upsert_request",
		map[string]any{"path": "ok.http", "name": "ok", "block": evil})
	if !isErr {
		t.Fatal("the canary write succeeded over the wire; want a refusal")
	}
	if !strings.Contains(text, "Refused to write literal credential") || !strings.Contains(text, "pulse vault set") {
		t.Fatalf("refusal is not actionable: %q", text)
	}
	raw, err := os.ReadFile(filepath.Join(ws, "ok.http"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != clean {
		t.Fatalf("the refused write modified the workspace: %q", raw)
	}
	if strings.Contains(string(raw), canary) {
		t.Fatal("the canary reached the disk")
	}
}
