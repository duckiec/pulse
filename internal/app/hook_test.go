package app_test

// Live gate for `# @wasm` (the request-rewrite hook).
//
// The guest is hand-assembled (no compiler, no wat2wasm) and ignores its input
// document: it overwrites the request with a fixed JSON document. The test
// server answers 201 only when the METHOD, path, query, headers AND body all
// match what the guest produced, so one status proves the whole chain: parser
// directive, engine ordering, sandbox instantiation, JSON ABI, and the host
// applying the rewrite before dispatch.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"pulse/internal/mcp"
	"pulse/internal/wasmguest"
)

// writeHookGuest assembles a hook guest that returns payload and stores it.
func writeHookGuest(t *testing.T, payload string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hook.wasm")
	if err := os.WriteFile(p, wasmguest.HookGuest([]byte(payload)), 0o600); err != nil {
		t.Fatalf("write hook guest: %v", err)
	}
	return p
}

// hookGateServer answers 201 only for the exact rewritten request.
func hookGateServer(t *testing.T, seen *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/hooked" || r.URL.Query().Get("x") != "1" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("Authorization") != "Bearer from-wasm" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["ok"] != true {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"hooked":true}`))
	}))
}

// TestWasmHookRewritesTheRequestLive is the feature's end-to-end gate.
func TestWasmHookRewritesTheRequestLive(t *testing.T) {
	var seen atomic.Int64
	srv := hookGateServer(t, &seen)
	defer srv.Close()

	payload := fmt.Sprintf(`{"method":"POST","url":%q,"headers":[["Authorization","Bearer from-wasm"],["Content-Type","application/json"]],"body":"{\"ok\":true}"}`,
		srv.URL+"/hooked?x=1")
	guest := writeHookGuest(t, payload)

	a := newTestApp(t)
	s := startSession(t, a)

	src := fmt.Sprintf("# @wasm %s\nGET %s/original\nAccept: application/json\n\n", guest, srv.URL)
	text, structured := s.callTool(mcp.ToolExecuteRequest, map[string]any{"payload": src})
	if status, _ := structured["status"].(float64); status != 201 {
		t.Fatalf("hooked run reported status %v, want 201:\n%s", status, firstLines(text, 14))
	}
	if seen.Load() != 1 {
		t.Fatalf("server saw %d request(s), want 1", seen.Load())
	}
	if body, _ := structured["body"].(string); !strings.Contains(body, `"hooked":true`) {
		t.Errorf("the hooked body did not come back:\n%s", firstLines(text, 14))
	}
}

// TestWasmHookBadOutputFailsClosed proves a guest cannot dispatch an invalid
// document: the run fails and the target is never reached.
func TestWasmHookBadOutputFailsClosed(t *testing.T) {
	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
	}))
	defer srv.Close()

	guest := writeHookGuest(t, `{"method":"GET"`) // truncated JSON
	a := newTestApp(t)
	s := startSession(t, a)

	src := fmt.Sprintf("# @wasm %s\nGET %s/x\n\n", guest, srv.URL)
	text, _ := s.callTool(mcp.ToolExecuteRequest, map[string]any{"payload": src})
	if !strings.Contains(text, "invalid JSON") {
		t.Fatalf("a hook returning invalid JSON was accepted:\n%s", firstLines(text, 14))
	}
	if seen.Load() != 0 {
		t.Fatalf("server saw %d request(s); a failed hook must not dispatch", seen.Load())
	}
}

// TestWasmHookRejectsHeaderInjection is the host-side validation gate: a guest
// that returns a header value containing a line break must be refused before
// the request reaches the wire, because the transport would treat the second
// line as a new field (response-splitting shape).
func TestWasmHookRejectsHeaderInjection(t *testing.T) {
	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
	}))
	defer srv.Close()

	payload := fmt.Sprintf(`{"method":"GET","url":%q,"headers":[["X-Evil","a\r\nX-Injected: 1"]],"body":""}`, srv.URL+"/x")
	guest := writeHookGuest(t, payload)
	a := newTestApp(t)
	s := startSession(t, a)

	src := fmt.Sprintf("# @wasm %s\nGET %s/x\n\n", guest, srv.URL)
	text, _ := s.callTool(mcp.ToolExecuteRequest, map[string]any{"payload": src})
	if !strings.Contains(text, "line break") {
		t.Fatalf("a CRLF header value was accepted:\n%s", firstLines(text, 14))
	}
	if seen.Load() != 0 {
		t.Fatalf("server saw %d request(s), want 0", seen.Load())
	}
	if !strings.Contains(text, "line break") {
		t.Errorf("the refusal does not explain the line break:\n%s", firstLines(text, 14))
	}
}

// TestWasmHookIsAvailableToTheTestPath checks the composition root wires the
// hook into the router `pulse test` uses, not just the MCP one. Without this,
// a hook would work for an agent and silently fail for CI.
func TestWasmHookIsAvailableToTheTestPath(t *testing.T) {
	var seen atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	payload := fmt.Sprintf(`{"method":"POST","url":%q,"headers":[],"body":""}`, srv.URL+"/hooked")
	guest := writeHookGuest(t, payload)
	dir := t.TempDir()
	file := filepath.Join(dir, "hook.http")
	src := fmt.Sprintf("# @wasm %s\n# @assert status == 201\nGET %s/original\n\n", guest, srv.URL)
	if err := os.WriteFile(file, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	a := newTestApp(t)
	var out strings.Builder
	code, err := a.TestDir(t.Context(), dir, &out, 1, 0, false)
	if err != nil {
		t.Fatalf("TestDir: %v", err)
	}
	if code != 0 {
		t.Fatalf("pulse test exit = %d, want 0:\n%s", code, out.String())
	}
	if seen.Load() != 1 {
		t.Fatalf("server saw %d request(s), want 1", seen.Load())
	}
}
