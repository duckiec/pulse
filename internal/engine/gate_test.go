package engine

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// gateFunc adapts a function to PolicyGate so a test can decide per request.
type gateFunc func(GateRequest) error

func (f gateFunc) Check(r GateRequest) error { return f(r) }

// TestPolicyGateBlocksBeforeDispatch is the security gate: a refusal must
// happen while the request is still an in-memory struct. The server's hit
// counter is the assertion that matters — an error message is not proof that
// nothing reached the network.
func TestPolicyGateBlocksBeforeDispatch(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var seen GateRequest
	gate := gateFunc(func(req GateRequest) error {
		seen = req
		if req.Method == http.MethodDelete {
			return NewPolicyBlockedError("Method DELETE is explicitly denied by .pulse-guard.")
		}
		return nil
	})
	resolver := stubResolver{"base": srv.URL, "token": "super-secret-token"}

	src := "DELETE {{base}}/users/7?force=true HTTP/1.1\n" +
		"Authorization: Bearer {{token}}\n\n###\n"
	rec := newRecordingSink(0)
	_, err := runFirst(t, src, rec, WithResolver(resolver), WithPolicyGate(gate))
	if err == nil {
		t.Fatal("blocked run reported no failure")
	}
	var blocked *PolicyBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("error = %T %v, want *PolicyBlockedError", err, err)
	}
	if !errors.Is(err, ErrPolicyBlocked) {
		t.Fatalf("errors.Is(err, ErrPolicyBlocked) = false for %v", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("server saw %d request(s); a blocked request must never be dispatched", got)
	}

	// The gate must see the EXPANDED request: a guard that inspects the raw
	// template would check the host `{{base}}` and wave everything through.
	if seen.Method != http.MethodDelete {
		t.Errorf("gate method = %q, want DELETE", seen.Method)
	}
	if seen.URL == nil || seen.URL.Host != srv.Listener.Addr().String() {
		t.Errorf("gate URL = %v, want host %s", seen.URL, srv.Listener.Addr())
	}
	if seen.URL != nil && seen.URL.Query().Get("force") != "true" {
		t.Errorf("gate query = %q, want force=true", seen.URL.RawQuery)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer super-secret-token" {
		t.Errorf("gate header = %q, want the resolved value", got)
	}
}

// TestPolicyGateAllowsWhenItReturnsNil proves the gate is not a blanket denial:
// an allowed request reaches the server and completes normally.
func TestPolicyGateAllowsWhenItReturnsNil(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	gate := gateFunc(func(req GateRequest) error {
		if req.URL.Path != "/allowed" {
			t.Errorf("path = %q, want /allowed", req.URL.Path)
		}
		return nil
	})
	rec := newRecordingSink(0)
	res, err := runFirst(t, "GET {{base}}/allowed HTTP/1.1\n\n###\n", rec,
		WithResolver(stubResolver{"base": srv.URL}), WithPolicyGate(gate))
	if err != nil {
		t.Fatalf("allowed run failed: %v", err)
	}
	if res == nil || res.Status != http.StatusOK {
		t.Fatalf("result = %+v, want 200", res)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("server saw %d request(s), want 1", got)
	}
}

// TestPolicyGateUntypedErrorStillBlocks closes the defect path: a gate that
// returns a plain error must still be classified as a policy refusal, because
// the alternative is that a bug in the gate reads as a transport failure.
func TestPolicyGateUntypedErrorStillBlocks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server was reached despite an untyped gate error")
	}))
	defer srv.Close()

	gate := gateFunc(func(GateRequest) error { return errors.New("bad rule") })
	rec := newRecordingSink(0)
	_, err := runFirst(t, "GET {{base}}/x HTTP/1.1\n\n###\n", rec,
		WithResolver(stubResolver{"base": srv.URL}), WithPolicyGate(gate))
	if !errors.Is(err, ErrPolicyBlocked) {
		t.Fatalf("error = %v, want ErrPolicyBlocked", err)
	}
}
