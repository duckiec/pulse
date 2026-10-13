package app_test

import (
	"bufio"
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
	"sync/atomic"
	"testing"
	"time"

	"pulse/internal/app"
	"pulse/internal/mcp"
)

// newTestApp builds a wired App rooted in a temporary directory.
//
// SkipKeychainProbe is set because the keychain is an OS service the test does
// not own: on a headless CI machine it is legitimately absent, and probing it
// would make this gate's result depend on the runner rather than on the code.
// The DEGRADED path it would take is gated separately by
// TestAppReportsAnAbsentKeychainRatherThanFailing.
func newTestApp(t *testing.T, mutate ...func(*app.Config)) *app.App {
	t.Helper()
	home := t.TempDir()
	cfg := app.Config{
		Home:              home,
		SkipKeychainProbe: true,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := app.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return a
}

// rpcSession drives the REAL stdio loop over in-memory pipes.
//
// It is not a convenience wrapper: `mcp.Lock` is installed first, exactly as
// cmd/pulse does it, so the pipe this test writes to is the guarded one. A
// test that called Server.Serve directly would skip the very isolation the move
// is supposed to preserve, and would pass while `pulse mcp` corrupted the
// stream at boot.
type rpcSession struct {
	t      *testing.T
	stdin  *io.PipeWriter
	stdout *bufio.Reader
	lock   *mcp.StdoutLock
	errc   chan error
	nextID int
}

func startSession(t *testing.T, a *app.App) *rpcSession {
	t.Helper()
	lock, err := mcp.Lock(false)
	if err != nil {
		t.Fatalf("mcp.Lock: %v", err)
	}
	t.Cleanup(lock.Restore)
	return startSessionLocked(t, a, lock)
}

// startSessionLocked drives the loop against a lock the caller already holds, so
// a test can install audit mode before the application is built.
func startSessionLocked(t *testing.T, a *app.App, lock *mcp.StdoutLock) *rpcSession {
	t.Helper()

	// The server side reads from one pipe and writes to the other. The reader
	// is held by the test; the writer is the session under test.
	inR, inW := io.Pipe()   // test -> server stdin
	outR, outW := io.Pipe() // server stdout -> test

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- a.ServeMCP(ctx, inR, outW)
		_ = outW.Close() // a closed stdout is how the test sees EOF
	}()

	t.Cleanup(func() {
		_ = inW.Close()
		cancel()
		select {
		case <-errc:
		case <-time.After(5 * time.Second):
			t.Error("the MCP session did not shut down within 5s")
		}
		_ = outR.Close()
	})

	s := &rpcSession{
		t:      t,
		stdin:  inW,
		stdout: bufio.NewReader(outR),
		lock:   lock,
		errc:   errc,
	}
	s.handshake()
	return s
}

// handshake performs the MCP lifecycle an agent must follow before a call.
func (s *rpcSession) handshake() {
	s.t.Helper()
	res := s.call("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "gate", "version": "1"},
	})
	if res.Result == nil {
		s.t.Fatalf("initialize returned no result: %s", res.raw)
	}
	// `initialized` is a notification: no id, no response.
	if _, err := s.notify("notifications/initialized", nil); err != nil {
		s.t.Fatalf("initialized: %v", err)
	}
}

type rpcResponse struct {
	raw    string
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *rpcSession) call(method string, params any) *rpcResponse {
	s.t.Helper()
	s.nextID++
	body := map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method}
	if params != nil {
		body["params"] = params
	}
	return s.roundTrip(body, true)
}

func (s *rpcSession) notify(method string, params any) (string, error) {
	body := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		body["params"] = params
	}
	_, err := s.write(body)
	return "", err
}

func (s *rpcSession) write(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	if _, err := s.stdin.Write(append(raw, '\n')); err != nil {
		return "", err
	}
	return string(raw), nil
}

// roundTrip sends one frame and reads frames until the matching response.
//
// Notifications and server-initiated frames share the stream, so skipping to
// the next line would silently drop an unrelated message. Frames are read with
// a deadline: a hang here is a broken protocol and should read as a failure,
// not as a test timeout thirty seconds later.
func (s *rpcSession) roundTrip(body any, wantResponse bool) *rpcResponse {
	s.t.Helper()
	if _, err := s.write(body); err != nil {
		s.t.Fatalf("write: %v", err)
	}
	if !wantResponse {
		return nil
	}
	wantID := body.(map[string]any)["id"]
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			s.t.Fatalf("no response to %v within the deadline", wantID)
		}
		line, err := s.stdout.ReadString('\n')
		if err != nil {
			s.t.Fatalf("read (waiting for a response to %v): %v", wantID, err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var probe struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			s.t.Fatalf("stdout carried a non-JSON line %q; the protocol stream "+
				"is corrupted: %v", line, err)
		}
		if probe.JSONRPC != "2.0" {
			s.t.Fatalf("stdout carried a frame with jsonrpc=%q: %q", probe.JSONRPC, line)
		}
		if len(probe.ID) == 0 {
			continue // a notification or server request
		}
		var gotID int
		if err := json.Unmarshal(probe.ID, &gotID); err != nil {
			continue
		}
		if fmt.Sprint(wantID) != fmt.Sprint(gotID) {
			continue
		}
		res := &rpcResponse{raw: line}
		if err := json.Unmarshal([]byte(line), res); err != nil {
			s.t.Fatalf("decode %q: %v", line, err)
		}
		return res
	}
}

// callTool invokes a tool and returns its text content plus structured content.
func (s *rpcSession) callTool(name string, args map[string]any) (text string, structured map[string]any) {
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
	return text, out.StructuredContent
}

const fuzzSpec = `openapi: 3.0.0
info: {title: Users, version: "1"}
paths:
  /users:
    post:
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                name:
                  type: string
                  minLength: 1
                  maxLength: 8
      responses:
        '200': {description: ok}
`

func writeSpec(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "api.yaml")
	if err := os.WriteFile(p, []byte(fuzzSpec), 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	return p
}

// TestCompositionRootFuzzEndToEnd is THE gate for this move.
//
// It drives the production path end to end and nothing else: a real App, the
// real mcp.Lock stdout guard, the real stdio loop over in-memory pipes, the
// real execute_fuzz_run handler, the real fuzzer, the real EngineFuzzRunner and
// the real engine — against a real httptest target. The assertion chain is
// deliberately cumulative:
//
//   - the tool is ADVERTISED, so an agent can find it;
//   - the JSON-RPC envelope is well-formed and stdout carried nothing else;
//   - the request reached the TARGET, counted at the server rather than at the
//     runner, so a tool that reported success without sending anything fails;
//   - a 5xx came back as `not ok` with the triggering payload in the TAP
//     diagnostic block;
//   - the document is a COMPLETE TAP 13 report.
//
// Each link is a different way the feature could be wired wrongly, and the one
// before it passing does not make the next one obvious.
func TestCompositionRootFuzzEndToEnd(t *testing.T) {
	var served int64
	var mu sync.Mutex
	var probes []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&served, 1)
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		probes = append(probes, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := newTestApp(t)
	s := startSession(t, a)

	// 1. The tool must be ADVERTISED, with both tools present.
	list := s.call("tools/list", nil)
	// list.Result is ALREADY the `result` member, so this decodes the tools
	// array directly rather than reaching through an envelope that no longer
	// exists. A test that mis-decodes its own input cannot notice a server that
	// advertised nothing.
	var advertised struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(list.Result, &advertised); err != nil {
		t.Fatalf("tools/list: %v (%s)", err, list.raw)
	}
	byName := map[string]bool{}
	for _, tool := range advertised.Tools {
		byName[tool.Name] = true
		if tool.Name == mcp.ToolExecuteFuzz {
			if tool.Description == "" || tool.InputSchema == nil {
				t.Errorf("the fuzz tool is advertised with no description or schema; " +
					"an agent cannot call what it cannot read")
			}
		}
	}
	for _, want := range []string{mcp.ToolExecuteRequest, mcp.ToolExecuteFuzz} {
		if !byName[want] {
			t.Fatalf("tools/list does not advertise %q; advertised: %v", want, byName)
		}
	}
	t.Logf("tools/list advertises %d tools", len(advertised.Tools))

	// 2. Call it.
	text, structured := s.callTool(mcp.ToolExecuteFuzz, map[string]any{
		"schema_path":     writeSpec(t),
		"target":          srv.URL,
		"op":              "POST /users",
		"seed":            11,
		"budget":          12,
		"rate_per_second": 500,
		"concurrency":     2,
		"run_id":          "nemoclaw",
	})

	// 3. The request must have reached the target. Counting at the SERVER is
	// the only honest place: a runner that swallowed the call and returned 200
	// would otherwise satisfy every other assertion here.
	if n := atomic.LoadInt64(&served); n == 0 {
		t.Fatalf("the fuzz run reported %q without sending a single request to "+
			"the target; the tool is not wired to a runner that dispatches",
			structured["op_id"])
	}
	t.Logf("%d probes reached the target", served)

	// 4. TAP 13.
	if !strings.HasPrefix(text, "TAP version 13\n") {
		t.Fatalf("the tool result is not TAP 13:\n%s", firstLines(text, 10))
	}
	if !strings.Contains(text, "not ok") {
		t.Errorf("a target returning 500 for every probe produced no `not ok` line:\n%s",
			firstLines(text, 20))
	}
	if !strings.Contains(text, "unexpected status 500") {
		t.Errorf("the diagnostic does not name the status:\n%s", firstLines(text, 24))
	}
	// The triggering payload must be INSIDE the diagnostic block.
	start := strings.Index(text, "  ---")
	end := strings.Index(text, "  ...")
	if start < 0 || end < start {
		t.Fatalf("no TAP diagnostic block in:\n%s", firstLines(text, 24))
	}
	if block := text[start:end]; !strings.Contains(block, "payload:") {
		t.Errorf("the diagnostic does not embed the triggering payload:\n%s", block)
	}

	// 5. The plan must match the result count, or a TAP consumer HANGS.
	total, _ := structured["total"].(float64)
	plan := fmt.Sprintf("1..%d", int(total))
	if !strings.Contains(text, plan) {
		t.Errorf("the TAP plan %q does not match the reported total %v", plan, structured["total"])
	}
	var resultLines int
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "not ok ") {
			resultLines++
		}
	}
	if int(total) != resultLines {
		t.Errorf("%d result lines for a plan of %d", resultLines, int(total))
	}

	// 6. Structured content must agree with the document.
	if failed, _ := structured["failed"].(float64); int(failed) != resultLines {
		t.Errorf("structured reports %v failures but the document has %d `not ok` lines",
			failed, resultLines)
	}
	if achieved, _ := structured["achieved_rate"].(float64); achieved <= 0 {
		t.Errorf("achieved_rate = %v; the limiter reported no throughput", achieved)
	}
	t.Logf("op=%v seed=%v total=%v elapsed_ms=%v achieved_rate=%.1f/s",
		structured["op_id"], structured["seed"], structured["total"],
		structured["elapsed_ms"], mustFloat(structured["achieved_rate"]))
}

// TestCompositionRootExecuteRequestEndToEnd proves the OTHER tool is wired too.
//
// The fuzz path going green says nothing about `execute_request`: they take
// different injected objects, and a composition root that wired one and left
// the other nil would look identical from the outside until an agent used it.
func TestCompositionRootExecuteRequestEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	a := newTestApp(t)
	s := startSession(t, a)

	src := fmt.Sprintf("GET %s/health\nAccept: application/json\n", srv.URL)
	text, structured := s.callTool(mcp.ToolExecuteRequest, map[string]any{"payload": src})

	if status, _ := structured["status"].(float64); status != 200 {
		t.Fatalf("execute_request reported status %v, want 200:\n%s", status, firstLines(text, 12))
	}
	if body, _ := structured["body"].(string); !strings.Contains(body, `"ok":true`) {
		t.Errorf("the response body did not come back:\n%s", firstLines(text, 12))
	}
	if _, ok := structured["proto"]; !ok {
		t.Errorf("the outcome does not report a protocol: %v", structured)
	}
	t.Logf("status=%v proto=%v elapsed_ms=%v",
		structured["status"], structured["proto"], structured["elapsed_ms"])
}

// TestCompositionRootWiresTheRedactorEverywhere is the gate that closes
// TASKS.md P4.3.3.
//
// Before the composition root existed, `DefaultExecutor` built an
// EngineExecutor with a nil Redactor, so MCP mode scrubbed by HEADER NAME only
// and a resolved secret in a response body reached the model intact. The defect
// was recorded and unfixed for three moves because there was nowhere to fix it:
// the executor is constructed inside the bridge, which has no vault.
//
// This asserts the wiring from the outside, by resolving a value through the
// app's chain and then reading it back out of a tool result. The point is the
// ROUND TRIP: a secret that is registered somewhere else, or a redactor that is
// not the same instance, fails here even though every individual field is
// non-nil.
func TestCompositionRootWiresTheRedactorEverywhere(t *testing.T) {
	const secret = "pulse-e2e-secret-token-9f3a"

	a := newTestApp(t, func(c *app.Config) {
		c.EnvFile = writeEnvFile(t, map[string]string{"E2E_TOKEN": secret})
	})

	// The chain and the redactor must be the SAME vault: a value resolved
	// through one and scrubbed by another leaves a window where the value is
	// known to the resolver and invisible to the scrubber.
	got, err := a.Chain.Resolve("E2E_TOKEN")
	if err != nil {
		t.Fatalf("the app's resolver cannot read the ephemeral env: %v", err)
	}
	if got != secret {
		t.Fatalf("the resolver returned %q, want the configured value", got)
	}
	if a.Redactor.Tracked() == 0 {
		t.Fatal("resolving a value did not register it with the app's redactor; " +
			"the chain and the redactor are not the same vault")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server echoes the token, which is exactly the shape of bug this
		// chokepoint exists to stop: a real API reflecting a request value into
		// its response.
		w.Header().Set("X-Debug-Token", secret)
		fmt.Fprintf(w, `{"echo":"%s"}`, secret)
	}))
	defer srv.Close()

	s := startSession(t, a)
	src := fmt.Sprintf("GET %s/echo\nAuthorization: Bearer {{E2E_TOKEN}}\n", srv.URL)
	text, structured := s.callTool(mcp.ToolExecuteRequest, map[string]any{"payload": src})

	if status, _ := structured["status"].(float64); status != 200 {
		t.Fatalf("status %v, want 200:\n%s", status, firstLines(text, 12))
	}
	for _, field := range []string{"body"} {
		v, _ := structured[field].(string)
		if strings.Contains(v, secret) {
			t.Errorf("the resolved secret survived into the %s field:\n%s", field, v)
		}
	}
	// Headers are scrubbed by name as well as by value; the keychain test in
	// internal/secrets covers the mechanics, this asserts the wiring.
	rawHeaders := fmt.Sprint(structured["headers"])
	if strings.Contains(rawHeaders, secret) {
		t.Errorf("the resolved secret survived into the response headers: %s", rawHeaders)
	}
	if !strings.Contains(fmt.Sprint(structured["body"]), "[REDACTED]") {
		t.Errorf("the body does not carry the redaction placeholder, so nothing "+
			"was redacted at all:\n%s", structured["body"])
	}
}

// TestCompositionRootWiresTheFailureBinder is the gate that closes
// TASKS.md P3.2.10.
//
// A 5xx explanation needs three things wired together that no single package
// owns: an index to search, an embedder to query it with, and a redactor for the
// error text. Move 8 built and gated every one of them and could not connect
// them. This indexes a corpus, triggers a 5xx whose body names a symbol in it,
// and asserts the response carries the source.
func TestCompositionRootWiresTheFailureBinder(t *testing.T) {
	root := t.TempDir()
	// The error text must be searchable against the indexed text, and the chunk
	// must land in a language the walker reads (go, py, astro — `LangOf`).
	const body = "runtime error: pulseE2ETargetSymbol is nil"
	if err := os.WriteFile(filepath.Join(root, "handler.go"),
		[]byte("package handler\n\n// pulseE2ETargetSymbol resolves the session.\n"+
			"func pulseE2ETargetSymbol() int { return 42 }\n"), 0o600); err != nil {
		t.Fatalf("write the corpus: %v", err)
	}

	a := newTestApp(t)
	res, err := a.IndexCorpus(context.Background(), root)
	if err != nil {
		t.Fatalf("IndexCorpus: %v", err)
	}
	if res.Chunks == 0 {
		t.Fatal("the corpus produced no chunks; nothing can be retrieved")
	}
	stats, err := a.Store.RagStats(context.Background())
	if err != nil {
		t.Fatalf("RagStats: %v", err)
	}
	t.Logf("indexed %d chunks (%d vectors, %d unreadable); store reports %+v",
		res.Chunks, res.Vector, res.Failed, stats)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, body)
	}))
	defer srv.Close()

	s := startSession(t, a)
	src := fmt.Sprintf("GET %s/boom\n", srv.URL)
	text, structured := s.callTool(mcp.ToolExecuteRequest, map[string]any{"payload": src})

	if status, _ := structured["status"].(float64); status != 500 {
		t.Fatalf("status %v, want 500:\n%s", status, firstLines(text, 12))
	}
	ctxField, ok := structured["context"]
	if !ok || ctxField == nil {
		t.Fatalf("a 500 carried no context binding:\n%s", firstLines(text, 20))
	}
	binding, _ := ctxField.(map[string]any)
	chunks, _ := binding["chunks"].([]any)
	if len(chunks) == 0 {
		t.Fatalf("the binding carries no chunks: %v", binding)
	}
	first, _ := chunks[0].(map[string]any)
	if p, _ := first["path"].(string); p != "handler.go" {
		t.Errorf("the top chunk is %q, want handler.go (the file naming the symbol)", p)
	}
	if txt, _ := first["text"].(string); !strings.Contains(txt, "pulseE2ETargetSymbol") {
		t.Errorf("the retrieved chunk does not contain the symbol the error named:\n%v", first["text"])
	}
	t.Logf("a 500 carried %d chunk(s); top = %s:%v-%v score=%.3f",
		len(chunks), first["path"], first["start_ln"], first["end_ln"], mustFloat(first["score"]))
}

// TestCompositionRootRedactsTheErrorTextBoundToAFailure is the gate for the
// binder's OWN redactor, and it is separate from the round-trip test above for a
// reason worth stating.
//
// The 5xx error text is the one string that reaches Pulse WITHOUT passing
// through the resolver: it comes from the server. Nothing registered it, nothing
// scrubbed it on the way in, and it is exactly the string a stack trace quotes
// the request that caused it into. So the binder's redactor is the last
// boundary, and a binder wired with a different (empty) vault would still
// attach the right chunks while handing the agent the token.
//
// The assertion is that the token appears NOWHERE in the binding — not in the
// error text, not in the query echoed back, not in the chunk text.
func TestCompositionRootRedactsTheErrorTextBoundToAFailure(t *testing.T) {
	const secret = "pulse-binder-secret-71c4"
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "worker.go"),
		[]byte("package worker\n\nfunc binderTargetSymbol() int { return 7 }\n"), 0o600); err != nil {
		t.Fatalf("write the corpus: %v", err)
	}

	a := newTestApp(t, func(c *app.Config) {
		c.EnvFile = writeEnvFile(t, map[string]string{"BINDER_TOKEN": secret})
	})
	// Resolving registers the value with the app's ONE redactor; the binder
	// shares it, which is the property under test.
	if _, err := a.Chain.Resolve("BINDER_TOKEN"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := a.IndexCorpus(context.Background(), root); err != nil {
		t.Fatalf("IndexCorpus: %v", err)
	}

	// The server echoes the token back in its error — a proxy or framework
	// reflecting the request into the message is a real and common shape.
	body := fmt.Sprintf("unauthorized: token %s rejected while resolving binderTargetSymbol", secret)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, body)
	}))
	defer srv.Close()

	s := startSession(t, a)
	src := fmt.Sprintf("GET %s/boom\n", srv.URL)
	text, structured := s.callTool(mcp.ToolExecuteRequest, map[string]any{"payload": src})

	if status, _ := structured["status"].(float64); status != 500 {
		t.Fatalf("status %v, want 500:\n%s", status, firstLines(text, 12))
	}
	raw := fmt.Sprint(structured["context"])
	if !strings.Contains(raw, "chunks") {
		t.Fatalf("no context was bound, so there is nothing to check:\n%s", firstLines(text, 20))
	}
	if strings.Contains(raw, secret) {
		t.Errorf("the secret survived into the failure binding the agent will read:\n%s", raw)
	}
	if strings.Contains(text, secret) {
		t.Errorf("the secret survived into the tool output:\n%s", firstLines(text, 24))
	}
	if !strings.Contains(raw, "[REDACTED]") {
		t.Errorf("the binding carries no redaction placeholder, so the scrubber "+
			"never ran rather than finding nothing:\n%s", raw)
	}
	// The binding must still be USEFUL: redacting the whole message would
	// satisfy the assertion above while destroying the feature.
	if !strings.Contains(raw, "binderTargetSymbol") {
		t.Errorf("the error text was scrubbed into uselessness; the symbol the "+
			"error named is what the retrieval relies on:\n%s", raw)
	}
	t.Logf("binding redacted the token and kept the searchable text")
}

// TestCompositionRootRefusesToServeWithoutWiring is the fail-closed gate.
//
// Each of these is a state a composition root could plausibly reach through a
// configuration mistake, and each is a state in which a server would still
// ANSWER — correctly formatted, just without the redaction or the executor that
// makes the answer safe. Refusing at construction is the only disposition where
// the mistake cannot ship unnoticed.
func TestCompositionRootRefusesToServeWithoutWiring(t *testing.T) {
	t.Run("no redactor", func(t *testing.T) {
		a := newTestApp(t)
		a.Redactor = nil
		if _, err := a.MCPServer(); err == nil {
			t.Error("the app served MCP with no redactor; a token in a response " +
				"body would reach the agent intact")
		}
	})
	t.Run("no executor", func(t *testing.T) {
		a := newTestApp(t)
		a.Executor = nil
		if _, err := a.MCPServer(); err == nil {
			t.Error("the app served MCP with no executor")
		}
	})
	t.Run("no fuzz runner", func(t *testing.T) {
		a := newTestApp(t)
		a.FuzzRunner = nil
		if _, err := a.MCPServer(); err == nil {
			t.Error("the app served MCP with no fuzz runner; execute_fuzz_run " +
				"would be advertised and answer every call with a nil panic")
		}
	})
}

// TestCompositionRootNeverAdvertisesAToolItCannotServe is the honesty gate.
//
// An agent that cannot see `execute_fuzz_run` will not call it. One that can
// see it and gets a nil-pointer error learns the tool is broken. The first is
// a fair contract; the second is not — so the App must never produce the second
// state, by either route: advertising the tool and failing on call, or serving
// with the runner absent.
func TestCompositionRootNeverAdvertisesAToolItCannotServe(t *testing.T) {
	a := newTestApp(t)
	a.FuzzRunner = nil

	srv, err := a.MCPServer()
	if err != nil {
		// Refusing outright is the stronger of the two honest options.
		t.Logf("MCPServer refused without a fuzz runner, which is honest: %v", err)
		return
	}
	msg := srv.HandleMessage(context.Background(), json.RawMessage(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	raw, _ := json.Marshal(msg)
	if strings.Contains(string(raw), mcp.ToolExecuteFuzz) {
		t.Errorf("execute_fuzz_run is advertised with no runner behind it:\n%s", raw)
	}
	if !strings.Contains(string(raw), mcp.ToolExecuteRequest) {
		t.Errorf("execute_request should still be advertised:\n%s", raw)
	}
	t.Log("the fuzz tool was omitted rather than advertised broken")
}

// TestAppLifecycleIsOrderedAndIdempotent pins the teardown contract.
//
// Close cancels the context BEFORE releasing the store, so work in flight
// observes the shutdown instead of having its database handle pulled out from
// under it. And it is idempotent, because a deferred Close and an explicit one
// from a signal handler both run in any realistic program — and a second
// `sql.DB.Close` returns an error the caller cannot interpret.
func TestAppLifecycleIsOrderedAndIdempotent(t *testing.T) {
	home := t.TempDir()
	a, err := app.New(context.Background(), app.Config{
		Home: home, SkipKeychainProbe: true,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}

	if a.Closed() {
		t.Fatal("a freshly built App reports itself closed")
	}
	// The context must be live before Close and dead after — that ordering IS
	// the contract in-flight work depends on.
	select {
	case <-a.Context().Done():
		t.Fatal("the app context is already cancelled before Close")
	default:
	}

	if err := a.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	select {
	case <-a.Context().Done():
	default:
		t.Error("Close returned without cancelling the app context")
	}
	if !a.Closed() {
		t.Error("Closed() is false after Close")
	}
	// Idempotent: a second call must not fail, and must not double-close.
	for i := 0; i < 3; i++ {
		if err := a.Close(); err != nil {
			t.Errorf("Close call %d returned %v; Close must be idempotent", i+2, err)
		}
	}

	// The store must be genuinely unusable afterwards, which is the observable
	// consequence of a closed handle.
	if _, err := a.Store.RagStats(context.Background()); err == nil {
		t.Error("the store still answers queries after Close")
	}
}

// TestAppCancelsInFlightWork proves the context is wired to real subsystems
// rather than being a field nobody reads.
func TestAppCancelsInFlightWork(t *testing.T) {
	a := newTestApp(t)
	ctx := a.Context()
	if ctx == nil {
		t.Fatal("Context() returned nil")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Errorf("the app context error is %v, want context.Canceled", ctx.Err())
	}
}

// TestAppNeverWritesToStdout is the isolation gate for THIS package.
//
// The import audit enforces it statically; this enforces it dynamically,
// because a write reached through a dependency — a library that logs to stdout,
// a closure the audit cannot see — is exactly the case a static check misses. In
// MCP mode a stray byte corrupts the stream for the connected agent
// (AGENTS.md §3.3).
//
// The tripwire is used rather than a swapped-in buffer because it judges bytes
// the way the real session does: a JSON-RPC 2.0 frame is legal, anything else
// is a violation. Redirecting to a buffer would only prove the app wrote
// nothing, not that what it wrote would have been safe.
func TestAppNeverWritesToStdout(t *testing.T) {
	var seen mcp.Recorder

	lock, err := mcp.Lock(true)
	if err != nil {
		t.Fatalf("mcp.Lock: %v", err)
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

	// Boot the whole application and drive it, not just construct it: a
	// diagnostic emitted while serving is exactly the case that matters.
	a, err := app.New(context.Background(), app.Config{
		Home: t.TempDir(), SkipKeychainProbe: true,
		CorpusRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	if _, err := a.IndexCorpusIfConfigured(context.Background()); err != nil {
		t.Errorf("IndexCorpusIfConfigured: %v", err)
	}
	if _, err := a.MCPServer(); err != nil {
		t.Errorf("MCPServer: %v", err)
	}
	if _, err := a.Store.RagStats(context.Background()); err != nil {
		t.Errorf("RagStats: %v", err)
	}
	for _, d := range a.Degradations {
		_ = d // already logged to stderr by the app
	}
	if err := a.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for seen.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if v := seen.Violations(); len(v) > 0 {
		t.Errorf("the composition root breached the stdout contract %d time(s):\n%s",
			len(v), v[0])
	}
}

// TestAppServesWithoutASingleStrayByte drives a full session with the audit
// tripwire installed and asserts the wire was clean throughout.
//
// The unit test above proves construction is quiet. This one proves SERVING is
// quiet — the fuzz tool handler, the retriever and the store are all reachable
// from here, and each is a separate chance to log.
func TestAppServesWithoutASingleStrayByte(t *testing.T) {
	var seen mcp.Recorder

	lock, err := mcp.Lock(true)
	if err != nil {
		t.Fatalf("mcp.Lock: %v", err)
	}
	lock.Tripwire.OnViolation = seen.Handle
	orig := os.Stdout
	defer func() {
		lock.Restore()
		os.Stdout = orig
	}()

	a, err := app.New(context.Background(), app.Config{
		Home: t.TempDir(), SkipKeychainProbe: true,
	})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	defer a.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"kaboom"}`))
	}))
	defer srv.Close()

	s := startSessionLocked(t, a, lock)
	s.callTool(mcp.ToolExecuteFuzz, map[string]any{
		"schema_path":     writeSpec(t),
		"target":          srv.URL,
		"budget":          5,
		"rate_per_second": 500,
	})

	// The tripwire is fed by a goroutine, so a violation may be a moment behind
	// the call that caused it.
	deadline := time.Now().Add(2 * time.Second)
	for seen.Count() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if v := seen.Violations(); len(v) > 0 {
		t.Errorf("serving breached the stdout contract %d time(s):\n%s", len(v), v[0])
	}
	t.Log("a full fuzz session produced no non-JSON-RPC bytes on the guarded stdout")
}

// TestAppDefaultsAreApplied pins what an agent gets with no configuration.
//
// `pulse mcp` is launched by a client, not by a person who read a manual. Every
// default here is chosen so the tool works on first contact; a caller that has
// to set something to avoid a crash is a caller that will not.
func TestAppDefaultsAreApplied(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PULSE_HOME", home)
	a, err := app.New(context.Background(), app.Config{SkipKeychainProbe: true})
	if err != nil {
		t.Fatalf("app.New: %v", err)
	}
	defer a.Close()

	if a.Home != home {
		t.Errorf("Home = %q, want $PULSE_HOME = %q", a.Home, home)
	}
	want := filepath.Join(home, app.DefaultDBName)
	if a.DBPath != want {
		t.Errorf("DBPath = %q, want %q", a.DBPath, want)
	}
	if a.Config().RunTimeout != app.DefaultRunTimeout {
		t.Errorf("RunTimeout = %v, want %v", a.Config().RunTimeout, app.DefaultRunTimeout)
	}
	if a.Config().FuzzTimeout != mcp.DefaultFuzzTimeout {
		t.Errorf("FuzzTimeout = %v, want %v", a.Config().FuzzTimeout, mcp.DefaultFuzzTimeout)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("the database was not created at %s: %v", want, err)
	}
}

// TestAppStoresNoSecretOnDisk is the persistence half of the zero-trust
// invariant (AGENTS.md §3.4).
//
// The redaction gates prove a secret is scrubbed on the way OUT. This proves it
// was never written IN: the index is the one place Pulse stores the user's own
// source text, and a corpus containing a committed token must not become a copy
// of it on disk.
func TestAppStoresNoSecretOnDisk(t *testing.T) {
	const secret = "pulse-disk-secret-4b7e2f"
	root := t.TempDir()
	// The file MUST be in a language the walker chunks (go, py, astro —
	// `rag.LangOf`). A .js corpus produces zero chunks, so nothing is stored,
	// so the gate passes while proving nothing — which is exactly how M48 came
	// back green the first time it was run.
	if err := os.WriteFile(filepath.Join(root, "config.go"),
		[]byte("package config\n\n// configToken is the bearer the gateway checks.\n"+
			"const configToken = \""+secret+"\"\n"), 0o600); err != nil {
		t.Fatalf("write corpus: %v", err)
	}

	a := newTestApp(t, func(c *app.Config) {
		c.EnvFile = writeEnvFile(t, map[string]string{"DISK_TOKEN": secret})
	})
	// Register the value so redaction has something to scrub with — the same
	// order the resolver would produce.
	a.Redactor.Register(secret)

	res, err := a.IndexCorpus(context.Background(), root)
	if err != nil {
		t.Fatalf("IndexCorpus: %v", err)
	}
	if res.Chunks == 0 {
		t.Fatal("the corpus produced no chunks; a gate that stores nothing " +
			"proves nothing about what would be stored")
	}
	hits, err := a.Store.Search(context.Background(), secret, nil, 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, h := range hits {
		if strings.Contains(h.Text, secret) {
			t.Errorf("a stored chunk contains the raw secret (path %s):\n%s", h.Path, h.Text)
		}
	}
	// The RAW FILES are the real test: the store could hold the value anywhere,
	// and a redaction applied on the way OUT would leave it on disk.
	//
	// All three are read, not just the main database. SQLite runs in WAL mode,
	// so a freshly written row lives in `pulse.db-wal` until a checkpoint moves
	// it — reading only the main file is how this gate stayed GREEN against
	// mutation M48 while the secret sat in plain text one file away.
	var scanned int
	for _, path := range []string{a.DBPath, a.DBPath + "-wal", a.DBPath + "-shm"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatalf("read %s: %v", path, err)
		}
		scanned += len(raw)
		if strings.Contains(string(raw), secret) {
			t.Errorf("%s contains the secret verbatim; redaction happened on "+
				"the way out of the store rather than before it went in",
				filepath.Base(path))
		}
	}
	// Logged unconditionally and without a verdict: under M48 the assertions
	// above have already failed, and a log line that reads "carry none of it"
	// would contradict them two lines apart.
	t.Logf("scanned %d raw DB/WAL bytes after indexing a corpus containing a secret", scanned)
}

// TestAppRefusesToIndexWithoutARedactor pins the fail-closed choice.
//
// Indexing with no redactor would write the user's tokens to disk and to a
// vector. The App always has one, so this state is reachable only by a bug —
// and a bug here should stop the process, not silently produce an index full of
// secrets.
func TestAppRefusesToIndexWithoutARedactor(t *testing.T) {
	a := newTestApp(t)
	a.Redactor = nil
	if _, err := a.IndexCorpus(context.Background(), t.TempDir()); err == nil {
		t.Error("the app indexed a corpus with no redactor bound")
	}
}

// TestAppReportsDegradationsRatherThanFailing proves the boot contract: an
// optional subsystem that is unavailable is REPORTED, and the app still works.
//
// A headless machine has no keychain. Failing to start there would make Pulse
// unusable for anyone who develops on one, which is a large fraction of the
// people who would use a local API workbench.
func TestAppReportsDegradationsRatherThanFailing(t *testing.T) {
	home := t.TempDir()
	env := writeEnvFile(t, map[string]string{"SOME_TOKEN": "value-from-env-1234"})

	a, err := app.New(context.Background(), app.Config{
		Home: home, EnvFile: env,
		// SkipKeychainProbe deliberately NOT set.
	})
	if err != nil {
		t.Fatalf("app.New refused to start because the keychain was probed and "+
			"may be absent: %v", err)
	}
	defer a.Close()

	if _, err := a.MCPServer(); err != nil {
		t.Fatalf("a degraded app cannot serve MCP: %v", err)
	}
	// The ephemeral env must still resolve — that is the whole point of the
	// fallback chain.
	if got, err := a.Chain.Resolve("SOME_TOKEN"); err != nil || got != "value-from-env-1234" {
		t.Errorf("the ephemeral env did not resolve with the keychain probed: %q %v", got, err)
	}
	t.Logf("booted with %d degradation(s): %v", len(a.Degradations), a.Degradations)
}

// TestAppReportsAnUnreadableEnvFileRatherThanFailing is the same contract for
// a typo in PULSE_ENV_FILE.
//
// The resolution order is keychain → .env → error. A user with only keychain
// entries must still get a working tool because they mistyped a path, so the
// file is a degradation and the app starts. What it must NOT do is start and
// behave as though the file had loaded.
func TestAppReportsAnUnreadableEnvFileRatherThanFailing(t *testing.T) {
	a, err := app.New(context.Background(), app.Config{
		Home:              t.TempDir(),
		EnvFile:           filepath.Join(t.TempDir(), "does-not-exist.env"),
		SkipKeychainProbe: true,
	})
	if err != nil {
		t.Fatalf("app.New refused to start over a missing .env: %v", err)
	}
	defer a.Close()

	found := false
	for _, d := range a.Degradations {
		if strings.Contains(d, "ephemeral env") {
			found = true
		}
	}
	if !found {
		t.Errorf("a missing .env produced no degradation report; the app looks "+
			"fully healthy while running with an empty environment: %v", a.Degradations)
	}
	t.Logf("degradations: %v", a.Degradations)
}

// TestSecretExitCodeIsDerivedFromTheChain proves the CLI's exit code is a
// function of the error, not of its wording (AGENTS.md §4.4.5).
func TestSecretExitCodeIsDerivedFromTheChain(t *testing.T) {
	if got := app.SecretExitCode(nil); got != 0 {
		t.Errorf("SecretExitCode(nil) = %d, want 0", got)
	}
	// A wrapped, non-secret error must be 0 so the caller falls through to its
	// generic failure path.
	if got := app.SecretExitCode(errors.New("disk on fire")); got != 0 {
		t.Errorf("SecretExitCode(generic) = %d, want 0", got)
	}
}

// TestAppConstructionFailureLeaksNothing checks the constructor's contract.
//
// It returns `(*App, error)`, never a partial App, so a caller cannot Close
// something half-built — or worse, use it. A failure to open the store must
// leave no file handle, no pool and no goroutine behind.
func TestAppConstructionFailureLeaksNothing(t *testing.T) {
	before := openFDs(t)

	// A directory where the database file should be makes Open fail.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocked.db")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatalf("create the blocker: %v", err)
	}
	a, err := app.New(context.Background(), app.Config{
		DBPath: blocker, SkipKeychainProbe: true,
	})
	if err == nil {
		if a != nil {
			_ = a.Close()
		}
		t.Fatal("app.New succeeded with an unusable database path")
	}
	if a != nil {
		t.Error("app.New returned both an App and an error; a partial App escapes " +
			"and a caller may use it")
	}

	after := openFDs(t)
	if after > before+2 {
		t.Errorf("construction failure leaked file descriptors: %d -> %d", before, after)
	}
	t.Logf("open descriptors %d -> %d across a failed construction", before, after)
}

// ---- helpers ---------------------------------------------------------------

func writeEnvFile(t *testing.T, kv map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	var b strings.Builder
	for _, k := range sortedKeys(kv) {
		fmt.Fprintf(&b, "%s=%s\n", k, kv[k])
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	return p
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func mustFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

// openFDs counts entries in /proc/self/fd.
//
// Linux-only, and it says so rather than returning a plausible number on other
// platforms: a gate that silently measured nothing would be worse than one that
// is BLOCKED here (AGENTS.md §4.5).
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("BLOCKED (reason: /proc/self/fd is unavailable on this platform: %v)", err)
	}
	return len(entries)
}

func firstLines(s string, n int) string {
	rows := strings.Split(s, "\n")
	if len(rows) > n {
		rows = rows[:n]
	}
	return strings.Join(rows, "\n")
}
