package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"pulse/internal/ast"
	"pulse/internal/engine"
)

func gateRequest(t *testing.T, method, raw string) engine.GateRequest {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return engine.GateRequest{Method: method, URL: u, Header: make(http.Header)}
}

// ---- parsing ---------------------------------------------------------------

func TestMissingGuardFileIsTheLoopbackBaseline(t *testing.T) {
	p, err := LoadGuard(t.TempDir() + "/.pulse-guard")
	if err != nil {
		t.Fatalf("LoadGuard(missing) = %v, want the default policy", err)
	}
	if !p.IsDefault() {
		t.Fatal("a missing file must produce the default policy")
	}
	g := NewGuard(p)
	if err := g.Check(gateRequest(t, "GET", "http://127.0.0.1:8080/x")); err != nil {
		t.Errorf("loopback blocked: %v", err)
	}
	if err := g.Check(gateRequest(t, "DELETE", "http://localhost/x")); err != nil {
		t.Errorf("localhost blocked: %v", err)
	}
	err = g.Check(gateRequest(t, "GET", "https://api.example.com/x"))
	if err == nil {
		t.Fatal("a remote host was allowed by the default policy")
	}
	if !strings.Contains(err.Error(), "no .pulse-guard exists") {
		t.Errorf("message = %q, want it to explain the default policy", err)
	}
}

func TestParseGuardFullPolicy(t *testing.T) {
	raw := `# agent firewall
[mcp]
allow_methods = GET, POST
deny_methods = DELETE, PUT, PATCH
allow_hosts = localhost, *.internal.net
deny_hosts = api.production.com
rate_limit = 10/minute
`
	p, err := ParseGuard([]byte(raw), ".pulse-guard")
	if err != nil {
		t.Fatalf("ParseGuard: %v", err)
	}
	if got := strings.Join(p.AllowMethods, ","); got != "GET,POST" {
		t.Errorf("allow_methods = %q", got)
	}
	if got := strings.Join(p.DenyHosts, ","); got != "api.production.com" {
		t.Errorf("deny_hosts = %q", got)
	}
	if p.RateLimit.Requests != 10 || p.RateLimit.Window != time.Minute {
		t.Errorf("rate_limit = %+v, want 10/minute", p.RateLimit)
	}
	if p.IsDefault() {
		t.Error("a parsed file must not report itself as the default policy")
	}
}

func TestParseGuardFailsClosed(t *testing.T) {
	cases := map[string]string{
		"no section":        "allow_methods = GET\n",
		"unknown section":   "[http]\nallow_methods = GET\n",
		"unknown key":       "[mcp]\nallow_host = localhost\n",
		"duplicate key":     "[mcp]\nallow_hosts = a\nallow_hosts = b\n",
		"unclosed section":  "[mcp\nallow_hosts = localhost\n",
		"no equals":         "[mcp]\nallow_hosts localhost\n",
		"bad rate unit":     "[mcp]\nrate_limit = 10/fortnight\n",
		"bad rate count":    "[mcp]\nrate_limit = 0/minute\n",
		"rate without unit": "[mcp]\nrate_limit = 10\n",
		"host with scheme":  "[mcp]\nallow_hosts = https://example.com\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseGuard([]byte(raw), ".pulse-guard"); err == nil {
				t.Fatalf("ParseGuard accepted %q", raw)
			}
		})
	}
}

func TestParseGuardEmptyListsMeanNoRule(t *testing.T) {
	p, err := ParseGuard([]byte("[mcp]\nallow_hosts =\ndeny_methods =\n"), ".pulse-guard")
	if err != nil {
		t.Fatalf("ParseGuard: %v", err)
	}
	g := NewGuard(p)
	if err := g.Check(gateRequest(t, "DELETE", "https://anywhere.example/x")); err != nil {
		t.Errorf("empty rules blocked a request: %v", err)
	}
}

// ---- the state machine -----------------------------------------------------

func TestGuardDeniesMethods(t *testing.T) {
	p := &GuardPolicy{Path: ".pulse-guard", DenyMethods: []string{"DELETE", "put"}}
	g := NewGuard(p)
	err := g.Check(gateRequest(t, "DELETE", "https://api.example.com/users/7"))
	if err == nil {
		t.Fatal("DELETE was allowed by deny_methods")
	}
	if !strings.Contains(err.Error(), "BLOCKED BY POLICY: Method DELETE is explicitly denied by .pulse-guard.") {
		t.Errorf("message = %q", err)
	}
	// Deny wins over allow: the same method in both lists is still denied.
	p2 := &GuardPolicy{Path: ".pulse-guard",
		AllowMethods: []string{"DELETE", "GET"}, DenyMethods: []string{"DELETE"}}
	if err := NewGuard(p2).Check(gateRequest(t, "DELETE", "https://api.example.com/x")); err == nil {
		t.Fatal("deny_methods did not win over allow_methods")
	}
}

func TestGuardAllowMethodsIsExhaustive(t *testing.T) {
	p := &GuardPolicy{Path: ".pulse-guard", AllowMethods: []string{"GET", "POST"}}
	g := NewGuard(p)
	if err := g.Check(gateRequest(t, "get", "https://api.example.com/x")); err != nil {
		t.Errorf("GET blocked: %v", err)
	}
	err := g.Check(gateRequest(t, "PATCH", "https://api.example.com/x"))
	if err == nil {
		t.Fatal("PATCH was allowed by an allow_methods list that omits it")
	}
	if !strings.Contains(err.Error(), "not in allow_methods") {
		t.Errorf("message = %q", err)
	}
}

func TestGuardHosts(t *testing.T) {
	p := &GuardPolicy{Path: ".pulse-guard",
		AllowHosts: []string{"localhost", "*.internal.net", "[::1]"},
		DenyHosts:  []string{"api.production.com", "*.secrets.internal.net"},
	}
	g := NewGuard(p)

	allowed := []string{
		"http://localhost:3000/x",
		"https://staging.internal.net/x",
		"https://deep.staging.internal.net/x",
		"http://[::1]:8080/x",
	}
	for _, raw := range allowed {
		if err := g.Check(gateRequest(t, "GET", raw)); err != nil {
			t.Errorf("%s blocked: %v", raw, err)
		}
	}
	blocked := map[string]string{
		"https://api.production.com/x":         "denied by deny_hosts",
		"https://vault.secrets.internal.net/x": "denied by deny_hosts",
		"https://internal.net/x":               "not in allow_hosts",
		"https://example.com/x":                "not in allow_hosts",
	}
	for raw, want := range blocked {
		err := g.Check(gateRequest(t, "GET", raw))
		if err == nil {
			t.Errorf("%s was allowed", raw)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s message = %q, want %q", raw, err, want)
		}
	}
}

func TestGuardWildcardAllowsEverything(t *testing.T) {
	p := &GuardPolicy{Path: ".pulse-guard", AllowHosts: []string{"*"}}
	if err := NewGuard(p).Check(gateRequest(t, "GET", "https://anywhere.example/x")); err != nil {
		t.Fatalf("* did not allow every host: %v", err)
	}
}

// mutableClock is a deterministic clock for the rate window.
type mutableClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *mutableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *mutableClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestGuardRateLimitSlidingWindow(t *testing.T) {
	clock := &mutableClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	p := &GuardPolicy{Path: ".pulse-guard", RateLimit: RateLimit{Requests: 2, Window: time.Minute}}
	g := NewGuard(p)
	g.now = clock.Now

	req := gateRequest(t, "GET", "https://api.example.com/x")
	if err := g.Check(req); err != nil {
		t.Fatalf("request 1 blocked: %v", err)
	}
	clock.Advance(10 * time.Second)
	if err := g.Check(req); err != nil {
		t.Fatalf("request 2 blocked: %v", err)
	}
	err := g.Check(req)
	if err == nil {
		t.Fatal("request 3 was admitted over a 2/minute limit")
	}
	if !strings.Contains(err.Error(), "Rate limit of 2/minute") {
		t.Errorf("message = %q", err)
	}

	// A denied request must not consume budget: after the first request ages
	// out, one more is admitted even though the third attempt was refused.
	clock.Advance(51 * time.Second) // t=61s; request 1 at t=0 has expired
	if err := g.Check(req); err != nil {
		t.Fatalf("request after window expiry blocked: %v", err)
	}
}

func TestGuardRateLimitOnlyCountsAllowedRequests(t *testing.T) {
	clock := &mutableClock{t: time.Unix(0, 0)}
	p := &GuardPolicy{Path: ".pulse-guard",
		DenyMethods: []string{"DELETE"},
		RateLimit:   RateLimit{Requests: 1, Window: time.Minute},
	}
	g := NewGuard(p)
	g.now = clock.Now
	if err := g.Check(gateRequest(t, "DELETE", "https://api.example.com/x")); err == nil {
		t.Fatal("DELETE was allowed")
	}
	// The denied call must not have filled the single slot.
	if err := g.Check(gateRequest(t, "GET", "https://api.example.com/x")); err != nil {
		t.Fatalf("an allowed request was rate-limited by a denied attempt: %v", err)
	}
}

// recordingRecorder counts history writes without a store.
type recordingRecorder struct{ n int }

func (r *recordingRecorder) RecordExecution(context.Context, ExecutionRecord) { r.n++ }

// TestBlockedRequestIsNotRecorded keeps a refusal out of history: a row would
// tell `get_history` and the TUI that a request ran, with status zero.
func TestBlockedRequestIsNotRecorded(t *testing.T) {
	rec := &recordingRecorder{}
	guard := NewGuard(DefaultGuardPolicy())
	exec := &EngineExecutor{
		Recorder: rec,
		NewEngine: func(doc *ast.Document) engine.ProtocolEngine {
			return engine.NewProtocolRouter(doc, engine.RouterConfig{
				REST: []engine.Option{engine.WithPolicyGate(guard)},
			})
		},
	}
	_, err := exec.Execute(context.Background(),
		"GET https://api.example.com/users\n", "<inline>", 0, time.Second)
	if !errors.Is(err, engine.ErrPolicyBlocked) {
		t.Fatalf("error = %v, want ErrPolicyBlocked", err)
	}
	if rec.n != 0 {
		t.Fatalf("recorded %d history row(s) for a blocked request", rec.n)
	}
}

func TestGuardUnparsedTargetIsBlocked(t *testing.T) {
	g := NewGuard(DefaultGuardPolicy())
	err := g.Check(engine.GateRequest{Method: "GET"})
	if err == nil {
		t.Fatal("a request with no parsed URL was allowed")
	}
	if !errors.Is(err, engine.ErrPolicyBlocked) {
		t.Errorf("error = %v, want ErrPolicyBlocked", err)
	}
}
