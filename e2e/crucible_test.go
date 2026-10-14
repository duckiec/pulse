//go:build e2e

// Package e2e_test is the Bare-Metal End-to-End Crucible: the compiled
// pulse binary driven as a real OS process across the entire pipeline —
// real keyring, real loopback sockets, real stdio pipes, real SQLite,
// real Wasm guests, real mTLS sync.
//
// No mocks, no in-process shortcuts. Every stage shells out to a binary
// built from ./cmd/pulse in a TempDir workspace, and every workspace is
// scanned for the canary at the end: one leaked byte fails the run.
//
// The suite needs a live Secret Service daemon (scripts/ci/run-with-keyring.sh
// provides the isolated session). Without one, Stage 1 fails closed and
// the suite reports SKIP with the reason — never a false pass.
package e2e_test

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pulse/internal/wasmguest"
)

// ---------------------------------------------------------------------------
// harness
// ---------------------------------------------------------------------------

// env pairs a PULSE_* variable with its value.
func env(k, v string) string { return k + "=" + v }

// buildPulse compiles ./cmd/pulse into dir and returns the binary path.
// CGO_ENABLED=0 is mandatory (AGENTS.md §2.1); the build is offline.
func buildPulse(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "pulse")
	if os.Getenv("GOOS") == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", bin, "./cmd/pulse")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build pulse: %v:\n%s", err, out)
	}
	return bin
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// e2e/ lives one level below the module root.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

// result is one finished process.
type result struct {
	exit   int
	stdout string
	stderr string
}

// run executes bin synchronously with stdin closed immediately.
func run(t *testing.T, bin string, penv []string, args ...string) result {
	t.Helper()
	return runStdin(t, bin, penv, "", args...)
}

// runStdin executes bin, feeding stdin before closing it.
func runStdin(t *testing.T, bin string, penv []string, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
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

// liveProc is a running child with pipes held open.
type liveProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *syncBuffer
	mu     sync.Mutex
	lines  []string // every stdout line, for the purity audit
}

// syncBuffer is a bytes.Buffer safe for the child's writer goroutine and
// the test's polling reader at once. A bare buffer here is a data race
// that -race would find and a diagnose-on-failure read would trip.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// start spawns bin with stdin/stdout piped and stderr captured. The child
// is killed when the test ends: a serve left running past its stage is a
// leaked bind on a fixed port, not background progress.
func start(t *testing.T, bin string, penv []string, args ...string) *liveProc {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), penv...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	so, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	se := &syncBuffer{}
	cmd.Stderr = se
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	p := &liveProc{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(so), stderr: se}
	t.Cleanup(func() {
		if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
			_ = stdin.Close()
		_ = cmd.Wait()
		}
	})
	return p
}

// kill closes stdin (EOF shutdown) and waits with a deadline. A process
// that will not exit on stdin EOF is a shutdown defect, not a slow test.
func (p *liveProc) kill(t *testing.T) (int, string) {
	t.Helper()
	_ = p.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return 0, p.stderr.String()
		}
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), p.stderr.String()
		}
		t.Fatalf("wait: %v (stderr: %s)", err, p.stderr.String())
		return -1, ""
	case <-time.After(20 * time.Second):
		_ = p.cmd.Process.Kill()
		t.Fatalf("process did not exit on stdin EOF (stderr: %s)", p.stderr.String())
		return -1, ""
	}
}

// ---------------------------------------------------------------------------
// strict TAP validator
// ---------------------------------------------------------------------------

// validateTAP enforces the TAP 13 grammar the dashboard parses: version
// line first, exactly one plan, result lines numbered contiguously from
// 1, plan count == result count.
func validateTAP(t *testing.T, doc string) int {
	t.Helper()
	lines := strings.Split(strings.ReplaceAll(doc, "\r\n", "\n"), "\n")
	// Drop the single trailing empty string a final newline produces.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 || lines[0] != "TAP version 13" {
		t.Fatalf("TAP must start with the version line, got %q", firstOf(lines))
	}
	plan := -1
	plans := 0
	nums := []int{}
	for _, l := range lines[1:] {
		if strings.HasPrefix(l, "1..") {
			plans++
			n, err := strconv.Atoi(strings.TrimPrefix(l, "1.."))
			if err != nil {
				t.Fatalf("plan line %q does not parse", l)
			}
			plan = n
			continue
		}
		rest, ok := strings.CutPrefix(l, "ok ")
		if !ok {
			rest, ok = strings.CutPrefix(l, "not ok ")
			if !ok {
				continue // diagnostics and comments are not results
			}
		}
		n, err := strconv.Atoi(strings.SplitN(rest, " ", 2)[0])
		if err != nil {
			t.Fatalf("result line %q has no number", l)
		}
		nums = append(nums, n)
	}
	if plans != 1 {
		t.Fatalf("want exactly one plan line, got %d", plans)
	}
	if plan != len(nums) {
		t.Fatalf("plan 1..%d carries %d results; a consumer trusting the plan hangs", plan, len(nums))
	}
	for i, n := range nums {
		if n != i+1 {
			t.Fatalf("result numbers are not contiguous from 1: %v", nums)
		}
	}
	return plan
}

func firstOf(lines []string) string {
	if len(lines) == 0 {
		return "<empty>"
	}
	return lines[0]
}

// ---------------------------------------------------------------------------
// JSON-RPC client over pipes
// ---------------------------------------------------------------------------

type mcpConn struct {
	t      *testing.T
	p      *liveProc
	nextID int
}

// rpc sends one request and returns the result object of the response
// bearing its id, skipping any interleaved notifications.
func (c *mcpConn) rpc(method string, params any) map[string]any {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	fmt.Fprintf(c.p.stdin, "%s\n", msg)
	for {
		line, err := c.p.stdout.ReadString('\n')
		if err != nil {
			c.t.Fatalf("mcp %s: read: %v (stderr: %s)", method, err, c.p.stderr.String())
		}
		c.p.mu.Lock()
		c.p.lines = append(c.p.lines, strings.TrimRight(line, "\r\n"))
		c.p.mu.Unlock()
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			c.t.Fatalf("mcp %s: non-JSON line on stdout: %q", method, line)
		}
		got, _ := m["id"].(float64)
		if int(got) != id {
			continue // notification or foreign response: keep reading
		}
		if e, ok := m["error"]; ok {
			c.t.Fatalf("mcp %s: error: %v", method, e)
		}
		res, _ := m["result"].(map[string]any)
		if res == nil {
			c.t.Fatalf("mcp %s: no result object: %s", method, line)
		}
		return res
	}
}

func (c *mcpConn) notify(method string, params any) {
	c.t.Helper()
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	fmt.Fprintf(c.p.stdin, "%s\n", msg)
}

// callTool invokes tools/call and returns text + structured content.
func (c *mcpConn) callTool(name string, args map[string]any) (string, map[string]any) {
	c.t.Helper()
	res := c.rpc("tools/call", map[string]any{"name": name, "arguments": args})
	var text string
	if content, ok := res["content"].([]any); ok {
		for _, ci := range content {
			if cm, ok := ci.(map[string]any); ok {
				if s, ok := cm["text"].(string); ok {
					text += s + "\n"
				}
			}
		}
	}
	sc, _ := res["structuredContent"].(map[string]any)
	if sc == nil {
		sc = map[string]any{}
	}
	if isErr, _ := res["isError"].(bool); isErr {
		c.t.Fatalf("tool %s returned isError:\n%s", name, text)
	}
	return text, sc
}

// ---------------------------------------------------------------------------
// the crucible
// ---------------------------------------------------------------------------

func TestCrucible(t *testing.T) {
	if testing.Short() {
		t.Skip("SKIP (reason: the crucible drives the compiled binary; -short runs unit gates only)")
	}
	binDir := t.TempDir()
	bin := buildPulse(t, binDir)
	ws := t.TempDir() // the main workspace; every artifact below is scanned

	canary := "sk_live_pulse_e2e_" + randHex(t, 16)
	hmacKey := randHex(t, 32)

	stageVaultAndKeyring(t, bin, ws, canary, hmacKey)
	srv := stageTargetServer(t, canary, hmacKey)
	stagePulseTest(t, bin, ws, srv)
	tr := &transcriptLog{}
	stageMCP(t, bin, ws, srv, tr)
	stageSync(t, bin, ws, tr)
	stageTUI(t, bin, ws, tr)
	stageLeakScan(t, ws, tr, canary)
}

func randHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// stage 1: keyring + vault
// ---------------------------------------------------------------------------

func stageVaultAndKeyring(t *testing.T, bin, ws, canary, hmacKey string) {
	t.Helper()
	home := filepath.Join(ws, "home")
	penv := []string{env("PULSE_HOME", home), env("PULSE_VAULT_KEY_ID", "e2e-main")}

	r := run(t, bin, penv, "vault", "keygen", "--key-id", "e2e-main")
	if r.exit != 0 {
		t.Skipf("SKIP (reason: no live keyring: vault keygen exited %d: %s)", r.exit, r.stderr)
	}
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "pulse-pub-") {
		t.Fatalf("keygen printed %d lines, want the two public halves:\n%s", len(lines), r.stdout)
	}
	recipients := filepath.Join(ws, "recipients")
	r = run(t, bin, penv, "vault", "add-recipient", lines[0], "--recipients", recipients)
	if r.exit != 0 {
		t.Fatalf("vault add-recipient: exit %d: %s", r.exit, r.stderr)
	}

	r = run(t, bin, penv, "vault", "init", "--key-id", "e2e-main")
	if r.exit != 0 {
		t.Fatalf("vault init: exit %d: %s", r.exit, r.stderr)
	}

	plain := filepath.Join(ws, "secrets.env")
	content := "WASM_KEY=" + hmacKey + "\nCANARY_SECRET=" + canary + "\n"
	if err := os.WriteFile(plain, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	r = run(t, bin, penv, "vault", "encrypt", plain, "--key-id", "e2e-main", "--recipients", recipients)
	if r.exit != 0 {
		t.Fatalf("vault encrypt: exit %d: %s", r.exit, r.stderr)
	}
	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Fatal("plaintext survived encrypt: the CLI must remove it")
	}
	vault := plain + ".vault"
	if _, err := os.Stat(vault); err != nil {
		t.Fatalf("no vault container at %s: %v", vault, err)
	}
	t.Logf("stage 1: identity + sealed vault in place")
}

// ---------------------------------------------------------------------------
// stage 2 fixtures: target server, corpus, collection
// ---------------------------------------------------------------------------

type targetServer struct {
	url        string
	mu         sync.Mutex
	hmacSeen   bool
	canarySeen bool
}

func stageTargetServer(t *testing.T, canary, hmacKey string) *targetServer {
	t.Helper()
	ts := &targetServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		tsHex := r.Header.Get("X-Pulse-Timestamp")
		sigHex := r.Header.Get("X-Pulse-Signature")
		mac := hmac.New(sha256.New, []byte(hmacKey))
		mac.Write([]byte(tsHex + "\n" + r.Method + " " + r.URL.RequestURI()))
		want := make([]byte, 64)
		hex.Encode(want, mac.Sum(nil))
		if tsHex == "" || !hmac.Equal([]byte(sigHex), want) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ts.mu.Lock()
		ts.hmacSeen = true
		if r.Header.Get("X-Canary") == canary {
			ts.canarySeen = true
		}
		ts.mu.Unlock()
		// Echo the canary-bearing header back, the way a server echoes a
		// token into a JSON error: the transcript must carry [REDACTED],
		// never the value. This is what makes the confinement proof
		// non-vacuous.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"confirmed","echo":%q}`, r.Header.Get("X-Canary"))
	})
	mux.HandleFunc("/v1/checkout", func(w http.ResponseWriter, r *http.Request) {
		if d := discountOf(r); d != nil && (*d < 0 || *d > 100) {
			// applyDiscount divides by (100 - discount): nonsense in,
			// panic out. The boundary matrix WILL send 101 (max+1).
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("panic: division by zero in applyDiscount"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("loopback listen: %v", err)
	}
	go func() { _ = http.Serve(ln, mux) }()
	ts.url = "http://" + ln.Addr().String()
	t.Cleanup(func() { _ = ln.Close() })
	return ts
}

// discountOf reads the discount from the query or a JSON body.
func discountOf(r *http.Request) *int64 {
	if q := r.URL.Query().Get("discount"); q != "" {
		if n, err := strconv.ParseInt(q, 10, 64); err == nil {
			return &n
		}
	}
	var body struct {
		Discount *int64 `json:"discount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.Discount != nil {
		return body.Discount
	}
	return nil
}

// ---------------------------------------------------------------------------
// stage 2: pulse test over the real binary
// ---------------------------------------------------------------------------

func stagePulseTest(t *testing.T, bin, ws string, srv *targetServer) {
	t.Helper()
	// The RAG corpus source: the handler whose chunk must explain the 500.
	srcDir := filepath.Join(ws, "src")
	if err := os.MkdirAll(srcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	handler := `package checkout

// applyDiscount divides by (100 - discount): nonsense discounts panic.
func applyDiscount(priceCents, discount int64) int64 {
	return priceCents * 100 / (100 - discount)
}
`
	if err := os.WriteFile(filepath.Join(srcDir, "checkout_handler.go"), []byte(handler), 0o600); err != nil {
		t.Fatal(err)
	}
	guest := filepath.Join(ws, "sign.wasm")
	if err := os.WriteFile(guest, wasmguest.SigningGuest(), 0o600); err != nil {
		t.Fatal(err)
	}
	coll := filepath.Join(ws, "coll")
	if err := os.MkdirAll(coll, 0o700); err != nil {
		t.Fatal(err)
	}
	orders := "@wasm-sign " + guest + "\n" +
		"# @name create-order\n" +
		"# @assert status == 200\n" +
		"POST " + srv.url + "/v1/orders HTTP/1.1\n" +
		"Content-Type: application/json\n" +
		"X-Canary: {{CANARY_SECRET}}\n" +
		"\n" +
		`{"item":"book"}` + "\n"
	if err := os.WriteFile(filepath.Join(coll, "orders.http"), []byte(orders), 0o600); err != nil {
		t.Fatal(err)
	}
	penv := []string{
		env("PULSE_HOME", filepath.Join(ws, "home")),
		env("PULSE_VAULT_KEY_ID", "e2e-main"),
		env("PULSE_VAULT_FILE", filepath.Join(ws, "secrets.env.vault")),
		env("PULSE_SKIP_SYNC", "1"),
	}
	r := run(t, bin, penv, "test", coll)
	if r.exit != 0 {
		t.Fatalf("pulse test: exit %d\nstdout:\n%s\nstderr:\n%s", r.exit, r.stdout, r.stderr)
	}
	if n := validateTAP(t, r.stdout); n != 1 {
		t.Fatalf("want a 1-point plan, got %d", n)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.hmacSeen {
		t.Fatal("server never saw a valid timestamped HMAC")
	}
	if !srv.canarySeen {
		t.Fatal("server never saw the resolved CANARY_SECRET: resolution is unproven")
	}
	if strings.Contains(r.stdout, "sk_live_pulse_e2e_") {
		t.Fatal("raw canary on the TAP stream")
	}
	// No [REDACTED] is demanded here: passing TAP points carry no body,
	// so there is nothing to scrub on this path. The non-vacuous
	// redaction proof lives in Stage 3, where the MCP outcome renders
	// the echoed body and the transcript must carry [REDACTED].
	t.Logf("stage 2: TAP valid, HMAC proven, stream clean")
}
