package app_test

// Live interception gate for @wasm-sign (ROADMAP.md §3.4.7).
//
// A hand-assembled guest (no compiler, no wat2wasm — `go test` is the only
// tool) exercises all four pulse/env host functions in one signing flow:
// now_ns for the timestamp, header_get for a pre-flight header check,
// hmac_sha256 for the keyed MAC, log for the diagnostic. The guest appends
// X-Pulse-Timestamp / X-Pulse-Signature to the envelope; the test server
// verifies the timestamped HMAC with the vault secret and answers 200 only
// then. A 200 therefore proves the whole chain: engine interception, sandbox
// execution, host-function wiring, and header mutation.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pulse/internal/app"
	"pulse/internal/mcp"
	"pulse/internal/wasmguest"
)

// --- the live gate ---

const wasmGateSecret = "vault-hmac-secret-for-wasm-gate-7f3a"

// hmacGateServer strictly requires the timestamped signature. Anything else
// — missing headers, stale timestamp, wrong MAC — is a 401. A 200 therefore
// proves the guest ran with the live secret and a fresh clock.
func hmacGateServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tsHex := r.Header.Get("X-Pulse-Timestamp")
		sigHex := r.Header.Get("X-Pulse-Signature")
		if tsHex == "" || sigHex == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		ts, err := strconv.ParseUint(tsHex, 16, 64)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		now := uint64(time.Now().UnixNano())
		if ts > now+5_000_000_000 || now-ts > 120_000_000_000 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mac := hmac.New(sha256.New, []byte(wasmGateSecret))
		mac.Write([]byte(tsHex + "\n" + r.Method + " " + r.URL.RequestURI()))
		want := make([]byte, 64)
		hex.Encode(want, mac.Sum(nil))
		if !hmac.Equal([]byte(sigHex), want) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"signed":true}`))
	}))
}

// writeWasmGuest stores the assembled signer and returns its path.
func writeWasmGuest(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sign.wasm")
	if err := os.WriteFile(p, wasmguest.SigningGuest(), 0o600); err != nil {
		t.Fatalf("write guest: %v", err)
	}
	return p
}

// writeWasmEnv stores a .env binding the key id the guest hardcodes.
func writeWasmEnv(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "wasm.env")
	if err := os.WriteFile(p, []byte("WASM_KEY="+wasmGateSecret+"\n"), 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	return p
}

// TestWasmSignInterceptionIsLive is THE gate for this wave.
//
// It drives the production path end to end and nothing else: a real App
// (whose chain resolves WASM_KEY from an ephemeral .env), the real MCP stdio
// session, a .http document carrying @wasm-sign, the real engine dispatch,
// the real Wasm sandbox executing a real guest against a real httptest
// target. The 200 asserts, cumulatively:
//
//   - the engine intercepted the request before dispatch (no signer → 401);
//   - the sandbox instantiated the guest with all four host imports;
//   - the guest called now_ns (the timestamp is fresh, checked ±window);
//   - the guest read a pre-flight header via header_get (absent → guest -1);
//   - hmac_sha256 resolved the Vault key dynamically (wrong key → 401);
//   - the host applied the returned headers to the outbound request.
func TestWasmSignInterceptionIsLive(t *testing.T) {
	srv := hmacGateServer(t)
	defer srv.Close()

	guest := writeWasmGuest(t)
	env := writeWasmEnv(t)
	a := newTestApp(t, func(c *app.Config) { c.EnvFile = env })
	s := startSession(t, a)

	src := fmt.Sprintf("@wasm-sign %s\nGET %s/signed\nAccept: application/json\n", guest, srv.URL)
	text, structured := s.callTool(mcp.ToolExecuteRequest, map[string]any{"payload": src})
	if status, _ := structured["status"].(float64); status != 200 {
		t.Fatalf("signed run reported status %v, want 200:\n%s", status, firstLines(text, 12))
	}
	if body, _ := structured["body"].(string); !strings.Contains(body, `"signed":true`) {
		t.Errorf("the signed body did not come back:\n%s", firstLines(text, 12))
	}
	t.Log("200 OK with a fresh timestamped HMAC: interception, sandbox, and header mutation all proven")
}
