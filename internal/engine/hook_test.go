package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// stubHook is the seam probe: it records invocation, proves it sees the
// INTERPOLATED request, and mutates the outbound request in place.
type stubHook struct {
	calls    atomic.Int64
	sawPath  string
	sawToken string
	rewrite  func(*http.Request)
}

func (h *stubHook) Apply(ctx context.Context, req *http.Request) error {
	h.calls.Add(1)
	path, _ := HookPath(ctx)
	h.sawPath = path
	h.sawToken = req.Header.Get("X-Token")
	if h.rewrite != nil {
		h.rewrite(req)
	}
	req.Header.Set("X-Hooked", "yes")
	return nil
}

// TestHookRunsAfterInterpolationAndBeforeDispatch proves the position of the
// seam: the hook sees the resolved header, its mutation reaches the wire, and a
// server requiring it answers 200.
func TestHookRunsAfterInterpolationAndBeforeDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Hooked") != "yes" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hook := &stubHook{}
	src := "# @wasm ./hooks/auth.wasm\nGET {{base}}/whoami\nX-Token: {{token}}\n\n"
	res, err := runFirst(t, src, newRecordingSink(0),
		WithResolver(stubResolver{"base": srv.URL, "token": "resolved-token"}),
		WithHook(hook))
	if err != nil {
		t.Fatalf("hooked run failed: %v", err)
	}
	if res == nil || res.Status != http.StatusOK {
		t.Fatalf("result = %+v, want 200", res)
	}
	if hook.calls.Load() != 1 {
		t.Fatalf("hook calls = %d, want 1", hook.calls.Load())
	}
	if hook.sawPath != "./hooks/auth.wasm" {
		t.Errorf("hook path = %q, want ./hooks/auth.wasm", hook.sawPath)
	}
	if hook.sawToken != "resolved-token" {
		t.Errorf("hook saw X-Token %q, want the interpolated value: it ran before interpolation", hook.sawToken)
	}
}

// TestHookMissingFailsClosed: a `# @wasm` request with no hook bound must not
// dispatch as an ordinary request. The server counter proves nothing was sent.
func TestHookMissingFailsClosed(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	_, err := runFirst(t, "# @wasm ./hooks/auth.wasm\nGET {{base}}/x\n\n", newRecordingSink(0),
		WithResolver(stubResolver{"base": srv.URL}))
	if err == nil {
		t.Fatal("a request asking for a hook dispatched with no hook bound")
	}
	if !strings.Contains(err.Error(), "needs a request hook") {
		t.Fatalf("error = %v, want it to name the missing hook", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("server saw %d request(s), want 0", hits.Load())
	}
}

// TestNoHookDirectiveNeverTouchesTheHook is the narrowness half: a request
// without `# @wasm` must never pay for the sandbox.
func TestNoHookDirectiveNeverTouchesTheHook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hook := &stubHook{}
	if _, err := runFirst(t, "GET {{base}}/plain\n\n", newRecordingSink(0),
		WithResolver(stubResolver{"base": srv.URL}), WithHook(hook)); err != nil {
		t.Fatalf("plain run failed: %v", err)
	}
	if hook.calls.Load() != 0 {
		t.Fatalf("hook ran %d time(s) for a request that did not ask for it", hook.calls.Load())
	}
}

// TestTheHookCannotSmugglePastTheGate is the ordering that matters: a hook
// rewrites the URL to a path the policy denies, and the firewall must judge the
// REWRITTEN request. A gate that ran before the hook would approve the original
// path and let the rewrite through.
func TestTheHookCannotSmugglePastTheGate(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	hook := &stubHook{rewrite: func(req *http.Request) {
		req.URL.Path = "/admin/delete"
	}}
	gate := gateFunc(func(req GateRequest) error {
		if strings.HasPrefix(req.URL.Path, "/admin") {
			return NewPolicyBlockedError("Path /admin/* is denied by .pulse-guard.")
		}
		return nil
	})
	src := "# @wasm ./hooks/auth.wasm\nGET {{base}}/public\n\n"
	_, err := runFirst(t, src, newRecordingSink(0),
		WithResolver(stubResolver{"base": srv.URL}), WithHook(hook), WithPolicyGate(gate))
	if err == nil {
		t.Fatal("the gate allowed a request the hook rewrote to a denied path")
	}
	if !strings.Contains(err.Error(), "BLOCKED BY POLICY") {
		t.Fatalf("error = %v, want a policy refusal", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("server saw %d request(s); the rewritten request must never be dispatched", hits.Load())
	}
}

// failedHook proves a hook error stops the run before the network.
type failedHook struct{}

func (failedHook) Apply(context.Context, *http.Request) error {
	return fmt.Errorf("guest trapped")
}

func TestHookFailureStopsTheRun(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	_, err := runFirst(t, "# @wasm ./bad.wasm\nGET {{base}}/x\n\n", newRecordingSink(0),
		WithResolver(stubResolver{"base": srv.URL}), WithHook(failedHook{}))
	if err == nil || !strings.Contains(err.Error(), "guest trapped") {
		t.Fatalf("error = %v, want the hook failure", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("server saw %d request(s), want 0", hits.Load())
	}
}
