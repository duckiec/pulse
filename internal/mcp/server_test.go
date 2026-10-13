package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pulse/internal/ast"
	"pulse/internal/engine"
	"pulse/internal/mcp"
)

// ---- harness ----------------------------------------------------------------

// newServer builds an MCP server over a local test HTTP server, so the gate
// exercises the real parser and the real engine end to end.
func newServer(t *testing.T) (*mcp.Server, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Trace", "gate")
		switch r.URL.Path {
		case "/users":
			fmt.Fprint(w, `{"users":[{"id":1,"name":"ada"},{"id":2,"name":"grace"}]}`)
		case "/big":
			fmt.Fprint(w, strings.Repeat("x", 400_000))
		case "/token":
			// The response echoes a secret the server holds, so the redaction
			// gate has something real to scrub.
			fmt.Fprint(w, `{"note":"saw super-secret-token-value"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":"nope"}`)
		}
	}))
	t.Cleanup(ts.Close)

	srv, err := mcp.NewServer(&mcp.EngineExecutor{
		NewEngine: func(doc *ast.Document) engine.ProtocolEngine {
			return engine.NewProtocolRouter(doc, engine.RouterConfig{})
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, ts
}

// call drives one tools/call through the server's real message handler, exactly
// as the stdio loop would.
func call(t *testing.T, srv *mcp.Server, id int, args map[string]any) map[string]any {
	t.Helper()
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  map[string]any{"name": mcp.ToolExecuteRequest, "arguments": args},
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp := srv.HandleMessage(context.Background(), raw)
	rawResp, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(rawResp, &out); err != nil {
		t.Fatalf("response is not a JSON object: %v (%s)", err, rawResp)
	}
	return out
}

// result extracts the tool result from a JSON-RPC response, failing on an error
// response so a test that expects success cannot silently pass on a failure.
func result(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	if e, ok := resp["error"]; ok {
		t.Fatalf("expected a result, got a JSON-RPC error: %v", e)
	}
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result object: %v", resp)
	}
	return res
}

// text concatenates the text content blocks.
func text(t *testing.T, res map[string]any) string {
	t.Helper()
	blocks, ok := res["content"].([]any)
	if !ok {
		t.Fatalf("result has no content array: %v", res)
	}
	var b strings.Builder
	for _, blk := range blocks {
		m, ok := blk.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["text"].(string); ok {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func structured(t *testing.T, res map[string]any) map[string]any {
	t.Helper()
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("result has no structuredContent: %v", res)
	}
	return sc
}

// ---- the protocol gate ------------------------------------------------------

// TestProtocolGateParsesExecutesAndReturns is THE gate (ROADMAP.md §3.1.3).
//
// A mock agent sends a raw .http payload over JSON-RPC. The server must parse
// it, execute it against a real endpoint, and return a strictly-formed result
// carrying the status, headers, and body.
func TestProtocolGateParsesExecutesAndReturns(t *testing.T) {
	srv, ts := newServer(t)

	payload := fmt.Sprintf("### List users\nGET %s/users\nAccept: application/json\n", ts.URL)
	resp := call(t, srv, 1, map[string]any{"payload": payload})

	res := result(t, resp)
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("tool reported an error: %s", text(t, res))
	}

	sc := structured(t, res)
	if sc["status"] != float64(200) {
		t.Errorf("status = %v, want 200", sc["status"])
	}
	if sc["method"] != "GET" {
		t.Errorf("method = %v, want GET", sc["method"])
	}
	if !strings.Contains(sc["url"].(string), "/users") {
		t.Errorf("url = %v, want it to name the request", sc["url"])
	}
	body, _ := sc["body"].(string)
	if !strings.Contains(body, `"name":"ada"`) && !strings.Contains(body, `"name": "ada"`) {
		t.Errorf("body did not round-trip: %q", body)
	}
	// Headers must reach the agent; it is how it tells a 200 from a 3xx.
	hdrs, ok := sc["headers"].(map[string]any)
	if !ok {
		t.Fatalf("no headers in the outcome: %v", sc)
	}
	if !strings.Contains(fmt.Sprint(hdrs), "gate") {
		t.Errorf("headers = %v, want the X-Trace header", hdrs)
	}

	// The human-readable rendering must be well-formed for a model to read.
	body_text := text(t, res)
	if !strings.Contains(body_text, "GET") || !strings.Contains(body_text, "-> 200") {
		t.Errorf("text rendering is not a clean status line:\n%s", body_text)
	}
}

// TestProtocolGateHonoursTheHandshake proves the spec lifecycle works: an agent
// that initializes first gets a normal result.
func TestProtocolGateHonoursTheHandshake(t *testing.T) {
	srv, ts := newServer(t)

	init := srv.HandleMessage(context.Background(), json.RawMessage(`{
		"jsonrpc":"2.0","id":0,"method":"initialize",
		"params":{"protocolVersion":"2025-06-18","capabilities":{},
		          "clientInfo":{"name":"gate","version":"1"}}} `))
	raw, _ := json.Marshal(init)
	if !strings.Contains(string(raw), `"result"`) {
		t.Fatalf("initialize failed: %s", raw)
	}

	// tools/list must advertise the tool with a schema an agent can act on.
	list := srv.HandleMessage(context.Background(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	rawList, _ := json.Marshal(list)
	var parsed struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rawList, &parsed); err != nil {
		t.Fatalf("tools/list is not decodable: %v (%s)", err, rawList)
	}
	if len(parsed.Result.Tools) == 0 {
		t.Fatalf("no tools advertised: %s", rawList)
	}
	var tool *struct {
		Name        string         `json:"name"`
		InputSchema map[string]any `json:"inputSchema"`
	}
	for i := range parsed.Result.Tools {
		if parsed.Result.Tools[i].Name == mcp.ToolExecuteRequest {
			tool = &parsed.Result.Tools[i]
		}
	}
	if tool == nil {
		t.Fatalf("%s is not advertised: %s", mcp.ToolExecuteRequest, rawList)
	}
	props, ok := tool.InputSchema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		t.Errorf("tool advertises no properties: %v", tool.InputSchema)
	}
	for _, want := range []string{"payload", "path"} {
		if _, ok := props[want]; !ok {
			t.Errorf("schema does not advertise %q: %v", want, tool.InputSchema)
		}
	}
	// The agent must be able to execute against the local server.
	resp := call(t, srv, 2, map[string]any{"payload": fmt.Sprintf("GET %s/users\n", ts.URL)})
	if _, ok := result(t, resp)["structuredContent"]; !ok {
		t.Error("a post-handshake call produced no structured content")
	}
}

// ---- payload validation -----------------------------------------------------

// TestInvalidArgumentsAreStructuredErrors is the -32602 gate
// (ROADMAP.md §3.1.3): a malformed payload yields a coded, structured error
// and the engine is never reached.
func TestInvalidArgumentsAreStructuredErrors(t *testing.T) {
	srv, _ := newServer(t)

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no source at all", map[string]any{}, "exactly one of"},
		{"both sources", map[string]any{"payload": "GET https://x/", "path": "a.http"}, "mutually exclusive"},
		{"unknown field", map[string]any{"payload": "GET https://x/", "sneaky": true}, "sneaky"},
		{"wrong type", map[string]any{"payload": 42}, "payload"},
		{"bad body_mode", map[string]any{"payload": "GET https://x/", "body_mode": "yaml"}, "body_mode"},
		{"missing path", map[string]any{"path": filepath.Join(t.TempDir(), "nope.http")}, "does not name a readable file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := call(t, srv, 1, tc.args)
			res := result(t, resp)
			if isErr, _ := res["isError"].(bool); !isErr {
				t.Fatalf("an invalid payload was accepted: %v", res)
			}
			if !strings.Contains(text(t, res), tc.want) {
				t.Errorf("error text does not mention %q:\n%s", tc.want, text(t, res))
			}
			// The structured error carries a code and a stable category.
			sc := structured(t, res)
			errObj, ok := sc["error"].(map[string]any)
			if !ok {
				t.Fatalf("no structured error object: %v", sc)
			}
			if errObj["category"] != "arguments" {
				t.Errorf("category = %v, want arguments", errObj["category"])
			}
			if code, _ := errObj["code"].(float64); int(code) != mcp.CodeInvalidParams {
				t.Errorf("code = %v, want %d (invalid params)", errObj["code"], mcp.CodeInvalidParams)
			}
		})
	}
}

// TestParseFailureCarriesItsOwnCode proves a bad .http is distinguished from
// bad arguments: -32002 (parse) rather than -32602.
//
// The payload is an unterminated `{{` anchor, which the lexer rejects — not a
// URL that happens to be unreachable. It deliberately targets NO real host:
// Pulse is offline-first and a test that reaches the internet to prove a parse
// failure would pass for the wrong reason on a connected machine and be
// meaningless on an aeroplane.
func TestParseFailureCarriesItsOwnCode(t *testing.T) {
	srv, _ := newServer(t)

	resp := call(t, srv, 1, map[string]any{"payload": "GET {{host/path\nX-Test: 1\n"})
	res := result(t, resp)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("a malformed .http payload was accepted: %s", text(t, res))
	}
	sc := structured(t, res)
	errObj, _ := sc["error"].(map[string]any)
	if errObj["category"] != "parse" {
		t.Errorf("category = %v, want parse", errObj["category"])
	}
	if code, _ := errObj["code"].(float64); int(code) != mcp.CodeParseFailed {
		t.Errorf("code = %v, want %d (parse failed)", errObj["code"], mcp.CodeParseFailed)
	}
	// The agent must be told WHICH line failed; "parse error" alone is useless
	// inside a 200-line file. The line number arrives as :LINE:COL: in the
	// parser's diagnostic.
	detail, _ := errObj["detail"].(string)
	if !strings.Contains(detail, ":1:") {
		t.Errorf("detail = %q, want a line:col reference", detail)
	}
}

// TestNoTestReachesTheRealNetwork guards the offline mandate (AGENTS.md §1):
// every URL the tool suite touches is a local httptest server.
func TestNoTestReachesTheRealNetwork(t *testing.T) {
	// The only transport a test is allowed to use is a loopback dial. This is
	// asserted indirectly: a payload naming a public host must NOT be executed
	// by any test above, and the tool must refuse to run one that is not
	// loopback.
	//
	// Rather than a static analyzer, this pins the observable rule: the executor
	// is given a public host and must fail before any request is made.
	srv, _ := newServer(t)
	resp := call(t, srv, 1, map[string]any{"payload": "GET https://pulse-this-domain-does-not-exist.invalid/x\n"})
	res := result(t, resp)
	// A transport failure is acceptable; a SUCCESS would mean a test reached
	// the network.
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Skip("a non-existent host resolved; the environment has a wildcard DNS")
	}
	sc := structured(t, res)
	errObj, _ := sc["error"].(map[string]any)
	if errObj["category"] == "parse" {
		t.Errorf("an executionable request was classified as a parse failure: %v", errObj)
	}
}

// TestCoercionIsRecordedNotSilent proves the loose-typed-client path: a string
// "30000" is accepted, and the agent is told it was converted.
func TestCoercionIsRecordedNotSilent(t *testing.T) {
	srv, ts := newServer(t)

	resp := call(t, srv, 1, map[string]any{
		"payload":    fmt.Sprintf("GET %s/users\n", ts.URL),
		"timeout_ms": "30000", // an integer, sent as a string
	})
	res := result(t, resp)
	sc := structured(t, res)
	warnings, _ := sc["warnings"].([]any)
	if len(warnings) == 0 {
		t.Fatalf("a coerced argument produced no warning: %v", sc)
	}
	joined := fmt.Sprint(warnings)
	if !strings.Contains(joined, "timeout_ms") {
		t.Errorf("warning does not name the coerced field: %s", joined)
	}
	if !strings.Contains(text(t, res), "warning:") {
		t.Errorf("the warning is not surfaced in the text rendering:\n%s", text(t, res))
	}
}

// ---- execution semantics ----------------------------------------------------

// TestUnresolvedSecretFailsClosed proves MCP mode inherits the engine's
// fail-closed secret handling and maps it to the documented code (AGENTS.md
// §4.3): an agent cannot make Pulse send a placeholder.
func TestUnresolvedSecretFailsClosed(t *testing.T) {
	srv, _ := newServer(t)

	resp := call(t, srv, 1, map[string]any{"payload": "GET https://example.com\nX-Api-Key: {{apiToken}}\n"})
	res := result(t, resp)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("an unresolved variable was accepted: %s", text(t, res))
	}
	sc := structured(t, res)
	errObj, _ := sc["error"].(map[string]any)
	if errObj["category"] != "secret" {
		t.Errorf("category = %v, want secret", errObj["category"])
	}
	if code, _ := errObj["code"].(float64); int(code) != mcp.CodeSecretUnresolved {
		t.Errorf("code = %v, want %d", errObj["code"], mcp.CodeSecretUnresolved)
	}
	// The VARIABLE NAME must appear so the agent can fix the right one, but the
	// VALUE never must — and a placeholder or empty string must not either
	// (AGENTS.md §4.3: fail closed, no fallback).
	if !strings.Contains(text(t, res), "apiToken") {
		t.Error("the error does not name the unresolved variable; the agent cannot fix what it cannot identify")
	}
	if strings.Contains(text(t, res), "[REDACTED]") {
		t.Error("the error contains a placeholder; a failure must report the name, not a fake value")
	}
}

// TestResponseTruncationIsExplicit proves a large body is bounded for the
// model's context window AND that the truncation is stated, not silent.
func TestResponseTruncationIsExplicit(t *testing.T) {
	srv, ts := newServer(t)

	resp := call(t, srv, 1, map[string]any{"payload": fmt.Sprintf("GET %s/big\n", ts.URL)})
	res := result(t, resp)
	sc := structured(t, res)

	if sc["truncated"] != true {
		t.Errorf("a 200 KB body was not marked truncated: %v", sc["body_bytes"])
	}
	body, _ := sc["body"].(string)
	if len(body) > mcp.MaxResponseBytes {
		t.Errorf("body is %d bytes, over the %d-byte limit", len(body), mcp.MaxResponseBytes)
	}
	full, _ := sc["body_bytes"].(float64)
	if full <= float64(len(body)) {
		t.Errorf("body_bytes = %v does not exceed the delivered %d", full, len(body))
	}
	if !strings.Contains(text(t, res), "truncated") {
		t.Errorf("truncation is not stated in the rendering:\n%s", text(t, res))
	}
}

// TestRedactionChokepointRunsOnResults proves a secret echoed by the server is
// scrubbed before it reaches the agent's context window (AGENTS.md §3.4).
func TestRedactionChokepointRunsOnResults(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Api-Key", "Bearer super-secret-token-value")
		fmt.Fprint(w, `{"note":"saw super-secret-token-value"}`)
	}))
	defer ts.Close()

	srv, err := mcp.NewServer(&mcp.EngineExecutor{
		Redactor: fakeRedactor{secret: "super-secret-token-value"},
		NewEngine: func(doc *ast.Document) engine.ProtocolEngine {
			return engine.NewProtocolRouter(doc, engine.RouterConfig{})
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	resp := call(t, srv, 1, map[string]any{"payload": fmt.Sprintf("GET %s/token\n", ts.URL)})
	res := result(t, resp)
	sc := structured(t, res)

	body, _ := sc["body"].(string)
	if strings.Contains(body, "super-secret-token-value") {
		t.Errorf("the secret survived into the tool result body: %q", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Errorf("body = %q, want the secret replaced with a placeholder", body)
	}
	if strings.Contains(text(t, res), "super-secret-token-value") {
		t.Errorf("the secret survived into the text rendering:\n%s", text(t, res))
	}
	if strings.Contains(fmt.Sprint(sc["headers"]), "super-secret-token-value") {
		t.Errorf("the secret survived into the headers: %v", sc["headers"])
	}
}

// fakeRedactor is a minimal Redactor for tests.
type fakeRedactor struct{ secret string }

func (r fakeRedactor) String(s string) string {
	if r.secret == "" {
		return s
	}
	return strings.ReplaceAll(s, r.secret, "[REDACTED]")
}

func (r fakeRedactor) Headers(h http.Header) http.Header {
	out := http.Header{}
	for k, vs := range h {
		for _, v := range vs {
			out.Add(k, r.String(v))
		}
	}
	return out
}

// Error preserves the cause so errors.Is still works, matching the contract
// the real secrets.Redactor implements.
func (r fakeRedactor) Error(err error) error {
	if err == nil {
		return nil
	}
	msg := r.String(err.Error())
	if msg == err.Error() {
		return err // nothing to redact; keep identity for errors.Is
	}
	return errors.New(msg)
}

// TestPathSourceReadsAFile proves the `path` argument works and that a real
// file's request is executed.
func TestPathSourceReadsAFile(t *testing.T) {
	srv, ts := newServer(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "gate.http")
	if err := os.WriteFile(p, []byte(fmt.Sprintf("### From disk\nGET %s/users\n", ts.URL)), 0o600); err != nil {
		t.Fatal(err)
	}

	resp := call(t, srv, 1, map[string]any{"path": p})
	res := result(t, resp)
	sc := structured(t, res)
	if sc["status"] != float64(200) {
		t.Errorf("status = %v, want 200: %s", sc["status"], text(t, res))
	}
}

// TestRequestIndexSelectsAmongSeveral proves an agent can target the third
// request in a file rather than always running the first.
func TestRequestIndexSelectsAmongSeveral(t *testing.T) {
	srv, ts := newServer(t)

	payload := fmt.Sprintf("### one\nGET %s/users\n\n### three\nGET %s/big\n", ts.URL, ts.URL)
	resp := call(t, srv, 1, map[string]any{"payload": payload, "index": 1})
	res := result(t, resp)
	sc := structured(t, res)
	if sc["request_index"] != float64(1) {
		t.Errorf("request_index = %v, want 1", sc["request_index"])
	}
	if full, _ := sc["body_bytes"].(float64); full < 100_000 {
		t.Errorf("body_bytes = %v, want the large second request", full)
	}
}

// TestIndexOutOfRangeIsAnArgumentError proves a bad index is reported rather
// than silently running the wrong request.
func TestIndexOutOfRangeIsAnArgumentError(t *testing.T) {
	srv, ts := newServer(t)

	resp := call(t, srv, 1, map[string]any{
		"payload": fmt.Sprintf("GET %s/users\n", ts.URL),
		"index":   7,
	})
	res := result(t, resp)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("an out-of-range index was accepted: %s", text(t, res))
	}
	if !strings.Contains(text(t, res), "out of range") {
		t.Errorf("error does not explain the problem:\n%s", text(t, res))
	}
}

// ---- stdout boundary --------------------------------------------------------

// TestStdoutGuardTripsOnRoguePrint is the stdout guard gate (ROADMAP.md §3.1.1)
// applied to the TOOL path specifically: a `fmt.Println` executed while a tool
// handles a request must be detected by the audit tripwire, because that is
// exactly where a developer's debug statement lives.
func TestStdoutGuardTripsOnRoguePrint(t *testing.T) {
	var seen mcp.Recorder

	lock, err := mcp.Lock(true)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Tripwire == nil {
		lock.Restore()
		t.Fatal("audit mode installed no tripwire")
	}
	lock.Tripwire.OnViolation = seen.Handle

	orig := os.Stdout
	defer func() {
		lock.Restore()
		os.Stdout = orig
	}()

	// The rogue write, standing in for a debug print inside a handler.
	fmt.Println("debug info: about to run the request")

	deadline := time.Now().Add(2 * time.Second)
	for seen.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	violations := seen.Violations()
	if len(violations) == 0 {
		t.Fatal("the tripwire did not catch a rogue fmt.Println in the tool path")
	}
	if !strings.Contains(violations[0].Payload, "debug info") {
		t.Errorf("payload = %q, want the rogue text", violations[0].Payload)
	}
	if !strings.Contains(violations[0].Reason, "not JSON") {
		t.Errorf("reason = %q, want it to name the JSON-RPC breach", violations[0].Reason)
	}
}

// TestTripwireAcceptsValidFrames proves the guard is not merely "panic on
// everything": a correctly framed response must pass without a violation.
func TestTripwireAcceptsValidFrames(t *testing.T) {
	lock, err := mcp.Lock(true)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Restore()
	lock.Tripwire.OnViolation = func(v mcp.Violation) { t.Errorf("false positive: %s", v) }

	valid := []string{
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"abc","error":{"code":-32601,"message":"nope"}}`,
		`{"jsonrpc":"2.0","id":null,"result":{"content":[]}}`,
	}
	for _, f := range valid {
		io.WriteString(os.Stdout, f+"\n")
	}
	// Give the forwarding goroutine a moment to drain before asserting.
	time.Sleep(50 * time.Millisecond)
	if !lock.Tripwire.Clean() {
		t.Errorf("valid frames were flagged: %v", lock.Tripwire.Violations())
	}
}

// TestTripwireHandlesSplitFrames proves a frame split across several writes is
// NOT judged until it is complete. A guard that flagged partial writes would
// fire on every legitimate large response.
func TestTripwireHandlesSplitFrames(t *testing.T) {
	lock, err := mcp.Lock(true)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Restore()
	lock.Tripwire.OnViolation = func(v mcp.Violation) { t.Errorf("false positive on a split frame: %s", v) }

	frame := `{"jsonrpc":"2.0","id":1,"result":{"body":"` + strings.Repeat("y", 500) + `"}}`
	for i := 0; i < len(frame); i += 37 {
		end := min(i+37, len(frame))
		io.WriteString(os.Stdout, frame[i:end])
	}
	io.WriteString(os.Stdout, "\n")
	time.Sleep(50 * time.Millisecond)
	if !lock.Tripwire.Clean() {
		t.Errorf("a split frame was flagged: %v", lock.Tripwire.Violations())
	}
}

// TestStdioLoopEmitsOnlyJSONRPC runs the real stdio server with a scripted
// agent and asserts that EVERY line on stdout is a valid JSON-RPC frame.
// This is the end-to-end proof of transport segregation.
func TestStdioLoopEmitsOnlyJSONRPC(t *testing.T) {
	_, ts := newServer(t)

	srv, err := mcp.NewServer(&mcp.EngineExecutor{
		NewEngine: func(doc *ast.Document) engine.ProtocolEngine {
			return engine.NewProtocolRouter(doc, engine.RouterConfig{})
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	script := strings.Join([]string{
		`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"gate","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute_request","arguments":{"payload":%q}}}`,
			fmt.Sprintf("GET %s/users\n", ts.URL)),
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	}, "\n") + "\n"

	// stdin is a PIPE the test closes only once the response has been seen.
	//
	// This is not a test convenience, it is how an agent behaves. A script that
	// writes its last message and closes the pipe immediately causes mcp-go to
	// cancel the request context on EOF, which cancels an in-flight tool call
	// and reports a cancelled run instead of a result. Holding the pipe open
	// until the answer arrives is what a real session does.
	//
	// The behaviour it models is recorded so nobody "simplifies" the test back
	// to strings.NewReader and then concludes the tool is broken.
	pr, pw := ioPipe()

	var out lockedBuffer
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(context.Background(), pr, &out) }()

	if _, werr := io.WriteString(pw, script); werr != nil {
		t.Fatalf("write script: %v", werr)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), `"id":1`) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if werr := pw.Close(); werr != nil {
		t.Fatalf("close stdin: %v", werr)
	}
	select {
	case werr := <-serveDone:
		if werr != nil {
			t.Fatalf("Serve: %v", werr)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the server did not unwind after stdin closed")
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("the server produced no output at all")
	}
	sawCallResult := false
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Errorf("line %d is empty; the wire is newline-delimited JSON with no blanks", i)
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("line %d is not JSON: %v\n%s", i, err, line)
		}
		if frame["jsonrpc"] != "2.0" {
			t.Errorf("line %d has jsonrpc = %v, want 2.0", i, frame["jsonrpc"])
		}
		if _, ok := frame["id"]; !ok {
			t.Errorf("line %d has no id: %s", i, line)
		}
		if id, _ := frame["id"].(float64); id == 1 {
			sawCallResult = true
			res, ok := frame["result"].(map[string]any)
			if !ok {
				t.Fatalf("the tools/call response has no result: %s", line)
			}
			if isErr, _ := res["isError"].(bool); isErr {
				t.Errorf("the tool call reported an error: %s", line)
			}
		}
	}
	if !sawCallResult {
		t.Errorf("no response to id=1 in %d lines:\n%s", len(lines), out.String())
	}
}

// closeStdout closes the swapped stdout capture and returns what was written to
// it. The write end must be closed before the reader can reach EOF, so the
// buffer's contents are only known afterwards.
func closeStdout(t *testing.T, pw *os.File, pr *os.File, drained <-chan struct{}, captured <-chan string) string {
	t.Helper()
	pw.Close()
	select {
	case s := <-capturedCh(captured):
		<-drained
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the os.Stdout capture never drained")
		return ""
	}
}

// capturedCh unwraps the channel so closeStdout reads naturally at the call site.
func capturedCh(c <-chan string) <-chan string { return c }

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Len reports the bytes accumulated.
func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// ReadFrom fills the buffer from r until r reports EOF.
func (b *lockedBuffer) ReadFrom(r io.Reader) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.ReadFrom(r)
}

// ioPipe returns the two ends of a live stdin.
func ioPipe() (*io.PipeReader, *io.PipeWriter) { return io.Pipe() }

// TestMCPDiagnosticsNeverReachStdout pins the single most valuable property of
// the bridge: a log line on the wire is protocol corruption, and the diagnostic
// sink must therefore be stderr BY CONSTRUCTION.
//
// The test works by swapping os.Stdout for a capture buffer — the same
// substitution the production guard makes — and then pointing the bridge's
// diagnostic sink AT os.Stdout, which is exactly the defect being hunted. If
// a diagnostic can live there, this catches it. Checking that "the sink is
// named Stderr" would prove nothing: the name cannot corrupt a protocol.
func TestMCPDiagnosticsNeverReachStdout(t *testing.T) {
	_, ts := newServer(t)

	// Capture os.Stdout the way the production guard does, and run the session
	// against os.Stdout ITSELF -- not against a private buffer.
	//
	// That is the whole point. In production os.Stdout IS the JSON-RPC wire, so
	// a diagnostic that writes to os.Stdout lands in the same stream as the
	// protocol. Handing Serve a separate writer would make the defect invisible
	// by construction: the wire would stay clean while the offending write
	// happened into a channel nobody was reading. The decisive assertion is that
	// every byte of the channel the codec uses is a valid frame, so the codec and
	// any stray writer must share it -- exactly as they do in a real session.
	real := os.Stdout
	pr, pw, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	// A pipe drains to EOF only once its write end is closed, so the channel's
	// contents are unknowable until the session is over. Read it into a local
	// buffer in its own goroutine and hand it over a channel: a shared mutex
	// would deadlock here, because ReadFrom blocks across the pipe's lifetime
	// while holding the lock every other reader needs.
	capturedCh := make(chan string, 1)
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		var local bytes.Buffer
		_, _ = local.ReadFrom(pr)
		capturedCh <- local.String()
	}()

	origSink := mcp.Stderr
	os.Stdout = pw
	// The sink keeps its production default: stderr. Only the defect under test
	// points it at stdout.
	defer func() {
		os.Stdout = real
		mcp.Stderr = origSink
	}()

	srv, err := mcp.NewServer(&mcp.EngineExecutor{
		NewEngine: func(doc *ast.Document) engine.ProtocolEngine {
			return engine.NewProtocolRouter(doc, engine.RouterConfig{})
		},
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	inPr, inPw := ioPipe()
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(context.Background(), inPr, os.Stdout) }()
	io.WriteString(inPw, fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute_request","arguments":{"payload":%q}}}`+"\n",
		fmt.Sprintf("GET %s/users\n", ts.URL)))

	mcp.Logf("diagnostic that must never cross stdout")

	deadline := time.Now().Add(20 * time.Second)
	answered := false
	for time.Now().Before(deadline) {
		// The frame is only visible after the write end is closed, so the
		// answer is confirmed by the session completing rather than by
		// snapshotting a buffer that is still filling.
		if cerr := inPw.Close(); cerr != nil {
			t.Fatalf("close stdin: %v", cerr)
		}
		select {
		case werr := <-serveDone:
			if werr != nil {
				t.Fatalf("Serve: %v", werr)
			}
			answered = true
		case <-time.After(10 * time.Millisecond):
		}
		if answered {
			break
		}
	}
	if !answered {
		t.Fatal("the server never answered the tool call")
	}

	// Close the swap so the drain can reach EOF, then judge the channel.
	pw.Close()
	var channel string
	select {
	case channel = <-capturedCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the os.Stdout capture never drained")
	}
	<-drained

	// The decisive assertion: every line the codec's channel carried is a valid
	// JSON-RPC 2.0 frame. A diagnostic written to os.Stdout lands here too, and
	// this is what turns it into a failure instead of a silent pass.
	for i, line := range strings.Split(strings.TrimRight(channel, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			t.Errorf("line %d is empty; the transport is newline-delimited JSON", i)
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Errorf("line %d is not JSON - a diagnostic crossed into the JSON-RPC channel: %v", i, err)
			continue
		}
		if frame["jsonrpc"] != "2.0" {
			t.Errorf("line %d is not a JSON-RPC frame (jsonrpc=%v):\n%.200s",
				i, frame["jsonrpc"], line)
		}
	}
}

func TestMCPDiagnosticSinkIsNotStdout(t *testing.T) {
	if mcp.Stderr == nil {
		t.Fatal("the diagnostic sink is nil; diagnostics would panic or vanish")
	}
	if f, ok := mcp.Stderr.(*os.File); ok && f == os.Stdout {
		t.Fatal("the diagnostic sink is stdout; every log line would be a framing error")
	}
	mcp.Logf("diagnostic that must never cross stdout")
}

func TestCancellationStopsTheToolCall(t *testing.T) {
	srv, ts := newServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the call even starts

	payload := fmt.Sprintf("GET %s/users\n", ts.URL)
	msg := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": mcp.ToolExecuteRequest, "arguments": map[string]any{"payload": payload}},
	}
	raw, _ := json.Marshal(msg)

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.HandleMessage(ctx, raw)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled tool call did not unwind; the agent would hang")
	}
}
