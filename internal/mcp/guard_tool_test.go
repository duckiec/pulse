package mcp_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"pulse/internal/ast"
	"pulse/internal/engine"
	"pulse/internal/mcp"
)

// newGuardedServer builds a server whose router carries a policy gate, the same
// shape the composition root wires for `pulse mcp`.
func newGuardedServer(t *testing.T, policy *mcp.GuardPolicy) *mcp.Server {
	t.Helper()
	guard := mcp.NewGuard(policy)
	exec := &mcp.EngineExecutor{
		NewEngine: func(doc *ast.Document) engine.ProtocolEngine {
			return engine.NewProtocolRouter(doc, engine.RouterConfig{
				REST: []engine.Option{engine.WithPolicyGate(guard)},
			})
		},
	}
	srv, err := mcp.NewServer(exec)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv
}

// TestGuardBlockedRequestReachesTheAgentAsText is the self-correction gate: a
// blocked call must come back as a normal tool result whose text names the rule
// and the file, so the model can pick a different method or host. The server
// hit counter proves the request was never sent.
func TestGuardBlockedRequestReachesTheAgentAsText(t *testing.T) {
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer ts.Close()

	srv := newGuardedServer(t, &mcp.GuardPolicy{
		Path:        ".pulse-guard",
		DenyMethods: []string{"DELETE"},
	})
	resp := call(t, srv, 1, map[string]any{
		"payload": fmt.Sprintf("DELETE %s/users/7\n", ts.URL),
	})
	res := result(t, resp)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("a blocked request must mark the result as an error: %s", text(t, res))
	}
	got := text(t, res)
	want := "BLOCKED BY POLICY: Method DELETE is explicitly denied by .pulse-guard."
	if !strings.Contains(got, want) {
		t.Fatalf("tool text = %q, want it to contain %q", got, want)
	}
	sc := structured(t, res)
	if sc["blocked"] != true {
		t.Errorf("structuredContent.blocked = %v, want true", sc["blocked"])
	}
	if sc["category"] != mcp.CategoryPolicy {
		t.Errorf("structuredContent.category = %v, want %q", sc["category"], mcp.CategoryPolicy)
	}
	if hits.Load() != 0 {
		t.Fatalf("the target server saw %d request(s); a blocked request must not be sent", hits.Load())
	}
}

// TestGuardAllowedRequestStillExecutes guards against the obvious failure mode
// of a firewall: blocking everything and calling it security.
func TestGuardAllowedRequestStillExecutes(t *testing.T) {
	var hits atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer ts.Close()

	srv := newGuardedServer(t, &mcp.GuardPolicy{
		Path:         ".pulse-guard",
		AllowMethods: []string{"GET"},
		AllowHosts:   []string{"127.0.0.1"},
	})
	resp := call(t, srv, 1, map[string]any{
		"payload": fmt.Sprintf("GET %s/users?limit=1\n", ts.URL),
	})
	res := result(t, resp)
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("an allowed request was refused: %s", text(t, res))
	}
	sc := structured(t, res)
	if sc["status"] != float64(http.StatusOK) {
		t.Errorf("status = %v, want 200", sc["status"])
	}
	if hits.Load() != 1 {
		t.Fatalf("target server saw %d request(s), want 1", hits.Load())
	}
}

// TestGuardDefaultPolicyBlocksRemoteHostThroughTheTool proves the missing-file
// baseline is enforced on the real tool path, not only in the gate's unit test.
func TestGuardDefaultPolicyBlocksRemoteHostThroughTheTool(t *testing.T) {
	// The default policy is what a missing file produces. The target is a
	// public host, so a dispatch would be a real outbound request; the gate
	// must refuse before that can happen.
	srv := newGuardedServer(t, mcp.DefaultGuardPolicy())
	resp := call(t, srv, 1, map[string]any{
		"payload": "GET https://api.example.com/users\n",
	})
	res := result(t, resp)
	if isErr, _ := res["isError"].(bool); !isErr {
		t.Fatalf("the default policy allowed a remote host: %s", text(t, res))
	}
	if got := text(t, res); !strings.Contains(got, "BLOCKED BY POLICY:") {
		t.Errorf("tool text = %q, want the policy prefix", got)
	}
}
