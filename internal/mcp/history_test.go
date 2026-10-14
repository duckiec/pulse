package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"pulse/internal/secrets"
)

// stubExecutorForHistory satisfies Executor without touching the engine: the
// history gates never dispatch a request, they only need the server to build.
type stubExecutorForHistory struct{}

func (stubExecutorForHistory) Execute(context.Context, string, string, int, time.Duration) (*Outcome, error) {
	return &Outcome{Status: 200, StatusText: "OK"}, nil
}

// stubHistory is a HistoryStore with scripted rows.
type stubHistory struct {
	collections []CollectionInfo
	requests    []RequestInfo
	runs        map[int64]RunRecord
	order       []int64
}

func (s *stubHistory) ListCollections(context.Context) ([]CollectionInfo, error) {
	return s.collections, nil
}

func (s *stubHistory) ListRequests(context.Context, string) ([]RequestInfo, error) {
	return s.requests, nil
}

func (s *stubHistory) GetRequest(_ context.Context, id string) (RequestInfo, error) {
	for _, r := range s.requests {
		if r.ID == id {
			return r, nil
		}
	}
	return RequestInfo{}, NewProtocolError(CodeInvalidParams, "no such request")
}

func (s *stubHistory) GetRun(_ context.Context, id int64) (RunRecord, error) {
	r, ok := s.runs[id]
	if !ok {
		return RunRecord{}, NewProtocolError(CodeInvalidParams, "no such run")
	}
	return r, nil
}

func (s *stubHistory) RecentHistory(_ context.Context, limit, offset int) ([]RunRecord, error) {
	var out []RunRecord
	for i := offset; i < len(s.order) && len(out) < limit; i++ {
		out = append(out, s.runs[s.order[i]])
	}
	return out, nil
}

// callHistoryTool drives one history tool through the real server dispatch so
// the gate exercises the production path — handler, chokepoint, and framing —
// rather than a direct function call that could bypass any of them.
func callHistoryTool(t *testing.T, srv *Server, tool string, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := srv.HandleMessage(context.Background(), raw)
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// TestSecretConfinementGateIsTheHistoryChokepoint proves that a vault value
// persisted in history is redacted when served through get_run AND
// get_history. The history tables keep metadata only, but the error column is
// free text an engine failure can echo a request into — so the bridge scrubs
// every string before transmission, and this gate names the raw value in both
// response payloads.
//
// Mutation M87 bypasses scrubRunRecord in either handler; the gate fails on
// the leaked value.
func TestSecretConfinementGateIsTheHistoryChokepoint(t *testing.T) {
	const vaultValue = "hunter2-confinement-proof-9f31"

	red := secrets.NewRedactor()
	red.Register(vaultValue)

	st := &stubHistory{runs: map[int64]RunRecord{
		7: {
			ID: 7, RequestID: "req-1", StartedAt: "2026-10-14T00:00:00Z",
			StatusCode: 500, SHA256: "abc123",
			Error:     "dial tcp: token " + vaultValue + " refused",
			RunSource: "mcp:test-client",
		},
	}, order: []int64{7}}

	exec := &stubExecutorForHistory{}
	srv, err := NewServerWithConfig(Config{Exec: exec, History: st, Redactor: red})
	if err != nil {
		t.Fatalf("NewServerWithConfig: %v", err)
	}

	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{ToolGetRun, map[string]any{"run_id": 7}},
		{ToolGetHistory, map[string]any{"limit": 10}},
	} {
		got := callHistoryTool(t, srv, tool.name, tool.args)
		if strings.Contains(got, vaultValue) {
			t.Errorf("%s leaked the raw vault value: %s", tool.name, got)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("%s: expected the redaction marker, got: %s", tool.name, got)
		}
	}
}

// TestHistoryToolsAreAdvertised proves the 5 tools appear on tools/list: a
// handler that exists but is not registered is unreachable, and the gate that
// matters is the one the agent sees.
func TestHistoryToolsAreAdvertised(t *testing.T) {
	exec := &stubExecutorForHistory{}
	srv, err := NewServerWithConfig(Config{Exec: exec, History: &stubHistory{}})
	if err != nil {
		t.Fatalf("NewServerWithConfig: %v", err)
	}
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{},
	})
	got := string(mustMarshal(t, srv.HandleMessage(context.Background(), raw)))
	for _, name := range []string{
		ToolListCollections, ToolListRequests, ToolGetRequest, ToolGetRun, ToolGetHistory,
	} {
		if !strings.Contains(got, name) {
			t.Errorf("tools/list omits %s", name)
		}
	}
}

// TestGetRunMissIsStructured proves an unknown run id is an invalid-params
// error naming the miss — never an empty record the agent could mistake for
// "nothing ran".
func TestGetRunMissIsStructured(t *testing.T) {
	exec := &stubExecutorForHistory{}
	srv, err := NewServerWithConfig(Config{Exec: exec, History: &stubHistory{runs: map[int64]RunRecord{}}})
	if err != nil {
		t.Fatalf("NewServerWithConfig: %v", err)
	}
	got := callHistoryTool(t, srv, ToolGetRun, map[string]any{"run_id": 999})
	if !strings.Contains(got, "no such") {
		t.Errorf("expected a miss explaining itself, got: %s", got)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
