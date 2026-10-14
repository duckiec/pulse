package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// stubSigner is the seam probe: it records invocation and mutates the
// outbound request, standing in for the Wasm adapter (which lives in
// internal/app and cannot be imported here).
type stubSigner struct {
	calls  atomic.Int64
	header string
	value  string
}

func (s *stubSigner) Sign(_ context.Context, req *http.Request) error {
	s.calls.Add(1)
	req.Header.Set(s.header, s.value)
	return nil
}

// TestSignerRunsPreDispatch proves the seam: a request carrying @wasm-sign
// reaches the signer before dispatch, the mutation lands on the wire, and a
// server requiring it answers 200.
func TestSignerRunsPreDispatch(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-Stub-Signed") != "yes" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stub := &stubSigner{header: "X-Stub-Signed", value: "yes"}
	sink := newRecordingSink(0)
	src := fmt.Sprintf("@wasm-sign ./stub.wasm\nGET %s/signed\n", srv.URL)
	res, err := runFirst(t, src, sink, WithSigner(stub))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res == nil || res.Status != 200 {
		t.Fatalf("status = %v, want 200", res)
	}
	if stub.calls.Load() != 1 {
		t.Fatalf("signer calls = %d, want 1", stub.calls.Load())
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1", hits.Load())
	}
}

// TestSignerMissingFailsClosed proves no unsigned dispatch: with a directive
// but no signer bound, the run fails AND the server is never hit. A server
// hit here would mean the request escaped unsigned — the M85 shape.
func TestSignerMissingFailsClosed(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sink := newRecordingSink(0)
	src := fmt.Sprintf("@wasm-sign ./stub.wasm\nGET %s/signed\n", srv.URL)
	_, err := runFirst(t, src, sink)
	if err == nil {
		t.Fatal("run without a signer succeeded; want a fail-closed error")
	}
	// Give the (never-started) dispatch no chance to hide: the failure is
	// synchronous before client.Do, so the count is exact.
	time.Sleep(100 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatalf("server hits = %d, want 0: the request escaped unsigned", hits.Load())
	}
}

// TestNoDirectiveNeverTouchesTheSigner proves the seam is opt-in: ordinary
// requests dispatch without invoking the signer.
func TestNoDirectiveNeverTouchesTheSigner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	stub := &stubSigner{header: "X-Stub-Signed", value: "yes"}
	sink := newRecordingSink(0)
	src := fmt.Sprintf("GET %s/plain\n", srv.URL)
	res, err := runFirst(t, src, sink, WithSigner(stub))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res == nil || res.Status != 200 {
		t.Fatalf("status = %v, want 200", res)
	}
	if stub.calls.Load() != 0 {
		t.Fatalf("signer calls = %d, want 0 for a request without @wasm-sign", stub.calls.Load())
	}
}
