package engine_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"pulse/internal/engine"
)

// TestTelemetryReportHasRealTimings is the telemetry gate: a live local request
// must yield non-zero phase timings, not zeros dressed up as measurements.
func TestTelemetryReportHasRealTimings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	rep := engine.AcquireTelemetry()
	defer engine.ReleaseTelemetry(rep)

	ctx := engine.Attach(context.Background(), rep)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Close the report the way the engine does once the request completes.
	rep.End()

	if rep.Total <= 0 {
		t.Errorf("Total = %v, want > 0", rep.Total)
	}
	if rep.GotConn.Duration() <= 0 {
		t.Errorf("GotConn duration = %v, want > 0", rep.GotConn.Duration())
	}
	if rep.FirstByte.Duration() <= 0 {
		t.Errorf("FirstByte duration = %v, want > 0 (server answered locally)", rep.FirstByte.Duration())
	}
	if rep.Wrote.Duration() <= 0 {
		t.Errorf("Wrote duration = %v, want > 0", rep.Wrote.Duration())
	}
	// Connect is measured for a fresh connection; a reused one legitimately
	// reports zero, so only assert it when the conn was NOT reused.
	if !rep.ReusedConn {
		if rep.Connect.Duration() <= 0 {
			t.Errorf("Connect duration = %v, want > 0 on a fresh connection", rep.Connect.Duration())
		}
		if rep.PointsFor(engine.OpConnect) == nil {
			t.Error("no connect trace point recorded for a fresh connection")
		}
	}
	if len(rep.Points) == 0 {
		t.Error("no trace points recorded")
	}
}

// TestTelemetryPointsAreMonotonic proves the reported durations are sane: a
// clock step must not produce a negative interval (ROADMAP.md §2.3.1).
func TestTelemetryPointsAreMonotonic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "x")
	}))
	defer srv.Close()

	rep := engine.AcquireTelemetry()
	defer engine.ReleaseTelemetry(rep)

	ctx := engine.Attach(context.Background(), rep)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	rep.End()

	for _, p := range rep.Points {
		if p.Duration() < 0 {
			t.Errorf("point %s has a negative duration %v (Start %v, End %v)",
				p.Op, p.Duration(), p.Start, p.End)
		}
	}
}

// TestTelemetryCollectorRingIsBounded proves the collector cannot grow without
// limit on a hot path (ROADMAP.md §2.3.2: a pre-allocated ring).
func TestTelemetryCollectorRingIsBounded(t *testing.T) {
	c := &engine.Collector{}
	const n = 5000
	for range 5000 {
		c.Add(engine.TracePoint{Op: engine.OpDNS, Start: time.Now()})
	}
	if got := len(c.Points()); got > engine.TraceRingMax() {
		t.Errorf("collector holds %d points, ring cap is %d", got, engine.TraceRingMax())
	}
	if c.Len() != n {
		t.Errorf("Len() = %d, want %d (total recorded, not retained)", c.Len(), n)
	}
	if _, ok := c.Find(engine.OpDNS); !ok {
		t.Error("Find could not locate a recorded op")
	}
	if _, ok := c.Find(engine.OpTLS); ok {
		t.Error("Find invented a point that was never added")
	}
}

// TestEBPFDegradesToBlocked is the telemetry fallback gate: on a run without the
// privileges for raw tracing, the report must say BLOCKED with a reason, and
// the httptrace timings must still stand on their own. It must NEVER pretend.
func TestEBPFDegradesToBlocked(t *testing.T) {
	status := engine.ProbeEBPF()
	t.Logf("ebpf disposition=%s reason=%q kernel=%s buildtag=%q",
		status.Disposition, status.Reason, status.Kernel, status.BuildTag)

	switch status.Disposition {
	case engine.EBPFAvailable:
		t.Log("raw tracing is available on this host (privileged run)")
	case engine.EBPFBlocked, engine.EBPFNotBuilt:
		// The honest-degradation case: a reason MUST be present. A BLOCKED
		// disposition with no explanation is exactly what AGENTS.md 4.5 forbids.
		if status.Reason == "" {
			t.Errorf("disposition %s carries no reason", status.Disposition)
		}
		t.Logf("BLOCKED as expected: %s", status.Reason)
	default:
		t.Errorf("unknown disposition %q", status.Disposition)
	}

	// The probe must be cheap to call on a request path and must not fail the
	// request.
	probe := engine.CurrentEBPFProbe()
	if probe == nil {
		t.Fatal("CurrentEBPFProbe returned nil")
	}
	st, err := probe.Load()
	if err != nil && st.Disposition == engine.EBPFAvailable {
		t.Errorf("Load reported %s AND an error %v", st.Disposition, err)
	}
	if st.Disposition == engine.EBPFNotBuilt && st.Reason == "" {
		t.Error("not-built disposition carries no reason")
	}
}

// TestTelemetryDegradesWithoutBreakingTheRequest proves that failing telemetry
// never fails the request: a report with no phases still returns a 200.
func TestTelemetryDegradesWithoutBreakingTheRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "payload")
	}))
	defer srv.Close()

	rep := engine.AcquireTelemetry()
	defer engine.ReleaseTelemetry(rep)

	ctx := engine.Attach(context.Background(), rep)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("a telemetry failure must not fail the request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	rep.End()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if string(body) != "payload" {
		t.Errorf("body = %q", body)
	}
	// The eBPF disposition is attached to the report and must always be set.
	if rep.EBPF.Disposition == "" {
		t.Error("report carries no eBPF disposition; degradation must be explicit")
	}
}

// TestSSEStreamCarriesTelemetry proves telemetry is attached to the SSE
// transport too, and that a degraded eBPF probe does not stop the stream.
func TestSSEStreamCarriesTelemetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: 1\ndata: telemetry\n\n")
	}))
	defer srv.Close()

	rep := engine.AcquireTelemetry()
	defer engine.ReleaseTelemetry(rep)

	sink := newSSESink()
	st, err := engine.OpenSSE(context.Background(), srv.URL, sink, engine.SSEConfig{
		Idle:          200 * time.Millisecond,
		FrameInterval: 5 * time.Millisecond,
		MinBackoff:    10 * time.Millisecond,
		MaxBackoff:    20 * time.Millisecond,
		Telemetry:     rep,
	})
	if err != nil {
		t.Fatalf("OpenSSE: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	_ = st.Close()

	// The transport owns the report and may still be finalizing it, so read
	// through the locked snapshot rather than touching fields directly.
	snap := rep.Snapshot()
	if snap.Total <= 0 {
		t.Errorf("SSE telemetry Total = %v, want > 0", snap.Total)
	}
	if !strings.Contains(sink.text(), "telemetry") {
		t.Errorf("stream payload missing:\n%s", sink.text())
	}
}

// TestCollectorIsConcurrencySafe proves the trace hooks (which fire on
// runtime-internal goroutines) are safe under concurrent use.
func TestCollectorIsConcurrencySafe(t *testing.T) {
	c := &engine.Collector{}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for range 64 {
				c.Add(engine.TracePoint{Op: engine.OpConnect, Start: time.Now(), End: time.Now()})
				_ = c.Len()
			}
		}(i)
	}
	wg.Wait()
	if got := c.Len(); got != 16*64 {
		t.Errorf("Len() = %d, want %d", got, 16*64)
	}
}
