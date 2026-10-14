package ui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"pulse/internal/ast"
	"pulse/internal/engine"
)

// floodServer serves N numbered lines as an event-stream flood: many small
// flushes back-to-back, the shape a rapid SSE payload takes on localhost.
// Each line is exactly lineSize bytes so the test's byte accounting is exact.
// floodServer serves N numbered lines as an event-stream flood: many small
// flushes back-to-back, the shape a rapid SSE payload takes on localhost.
// Each line is exactly lineSize bytes so the test's byte accounting is exact.
// When pace is true, flushes pause 5 ms per 100 lines so the flood spans many
// frames (proving the governor ticks under load); when false, the server
// blasts at full speed (proving accounting under backlog pressure, including
// the 50 MiB ROADMAP §1.2.5 load, which would take minutes paced).
func floodServer(t *testing.T, n int, pace bool) (*httptest.Server, int64) {
	t.Helper()
	const lineSize = 64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("no flusher")
			return
		}
		for i := 0; i < n; i++ {
			line := fmt.Sprintf("evt-%06d-%s\n", i, strings.Repeat("x", lineSize-12))
			if len(line) != lineSize {
				t.Errorf("line %d is %d bytes, want %d", i, len(line), lineSize)
				return
			}
			if _, err := io.WriteString(w, line); err != nil {
				return
			}
			if pace && i%100 == 99 {
				fl.Flush()
				time.Sleep(5 * time.Millisecond)
			}
		}
		fl.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv, int64(n) * lineSize
}

func parseDoc(t *testing.T, src string) *ast.Document {
	t.Helper()
	arena := ast.AcquireArena()
	arena.Reset([]byte(src))
	doc, err := ast.Parse(arena)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	t.Cleanup(doc.Release)
	return doc
}

// runHeadless drives a full program without a terminal: no renderer, input
// that never EOFs (an EOF-ing input can end the loop before the stream
// does), output discarded. It returns the final model, or fails closed on a
// timeout — a wedged update loop is a deadlock, and a deadlock that hangs
// the suite is worse than one that names itself.
func runHeadless(t *testing.T, m *Model, timeout time.Duration) *Model {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close(); _ = pw.Close() })
	p := tea.NewProgram(m, tea.WithoutRenderer(), tea.WithInput(pr), tea.WithOutput(io.Discard))
	m.Bind(p.Send)
	type outcome struct {
		model tea.Model
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		fin, err := p.Run()
		done <- outcome{fin, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("program: %v", o.err)
		}
		fin, ok := o.model.(*Model)
		if !ok {
			t.Fatalf("final model is %T, want *Model", o.model)
		}
		return fin
	case <-time.After(timeout):
		p.Kill()
		t.Fatalf("program did not exit in %s (update loop wedged or stream never terminated)", timeout)
		return nil
	}
}

// TestGFrameStreamIsExactUnderFlood is the G-frame correctness gate: a rapid
// event-stream flood must arrive whole (every line, every byte, in order),
// the engine pool must drain (recycled == received), and the program must
// exit by itself. M80 (dropping every second chunk) turns the line count
// from 4000 to 2000 here — there is no fuzz in the assertion to hide in.
func TestGFrameStreamIsExactUnderFlood(t *testing.T) {
	const n = 4000
	srv, wantBytes := floodServer(t, n, true)
	doc := parseDoc(t, "GET "+srv.URL+"/events HTTP/1.1\n\n###\n")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	m := New(ctx, engine.New(doc), doc, true)
	m.autoQuit = true
	fin := runHeadless(t, m, 30*time.Second)

	phase, err := fin.Outcome()
	if phase != phaseDone || err != nil {
		t.Fatalf("outcome = %s, %v; want DONE, nil", phase, err)
	}
	if got := fin.LineCount(); got != n {
		t.Errorf("lines = %d, want %d (stream truncated or reordered)", got, n)
	}
	if got := fin.ByteCount(); got != wantBytes {
		t.Errorf("bytes = %d, want %d", got, wantBytes)
	}
	joined := strings.Join(fin.body, "\n")
	if !strings.Contains(joined, "evt-000000") || !strings.Contains(joined, fmt.Sprintf("evt-%06d", n-1)) {
		t.Errorf("body holds neither the first nor the last event (flood mis-ordered)")
	}
	if got := fin.RetainedBytes(); got > 2<<20 {
		t.Errorf("retained = %d bytes, exceeds the 2 MiB invariant", got)
	}
	if d, _ := fin.Dropped(); d != 0 {
		t.Errorf("dropped %d lines on a %d KiB flood that fits the tail; the bound is for pressure, not for weather", d, wantBytes>>10)
	}
	if rec, cyc := fin.SinkStats(); rec == 0 || rec != cyc {
		t.Errorf("sink received=%d recycled=%d; every handed chunk must return to the pool exactly once", rec, cyc)
	}
	if fin.Frames() == 0 {
		t.Errorf("frames rendered = 0; bytes arrived but the governor never ticked")
	}
	if fin.Report() == nil {
		t.Errorf("no telemetry report attached to the run")
	}
}

// TestGFrameTailIsBoundedUnderPressure proves the other half of the memory
// argument: a 3 MiB flood cannot fit the 1 MiB tail, so lines MUST drop —
// with an exact count — while every byte stays accounted for and retention
// stays under the 2 MiB ceiling. A test that only ever fits is not a bound.
func TestGFrameTailIsBoundedUnderPressure(t *testing.T) {
	const n = 48000 // 48000 × 64B = 3 MiB
	srv, wantBytes := floodServer(t, n, true)
	doc := parseDoc(t, "GET "+srv.URL+"/big HTTP/1.1\n\n###\n")

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	m := New(ctx, engine.New(doc), doc, true)
	m.autoQuit = true
	fin := runHeadless(t, m, 60*time.Second)

	if phase, err := fin.Outcome(); phase != phaseDone || err != nil {
		t.Fatalf("outcome = %s, %v; want DONE, nil", phase, err)
	}
	if got := fin.ByteCount(); got != wantBytes {
		t.Fatalf("bytes = %d, want %d (bytes vanished outside the accounting)", got, wantBytes)
	}
	if got := fin.LineCount(); got != n {
		t.Fatalf("lines = %d, want %d", got, n)
	}
	dropped, _ := fin.Dropped()
	if dropped == 0 {
		t.Errorf("nothing dropped on a 3 MiB flood against a 1 MiB tail; the bound did not bind")
	}
	if got := fin.RetainedBytes(); got > 2<<20 {
		t.Errorf("retained = %d bytes, exceeds the 2 MiB invariant", got)
	}
	if rec, cyc := fin.SinkStats(); rec != cyc {
		t.Errorf("sink received=%d recycled=%d under pressure", rec, cyc)
	}
}

// TestGFrameFiftyMegabyteStreamStaysBounded is the ROADMAP §1.2.5 load in
// full: 50 MiB unpaced, asserting every byte is accounted for (rendered or
// dropped-with-count) while retention never exceeds the 2 MiB ceiling. It is
// deliberately unpaced: pacing would stretch it over minutes, and throughput
// under backlog pressure is exactly the shape the arrival-accounting rule
// exists for (an earlier revision accounted bytes at render time and
// undercounted by the backlog overflow — this test names that defect).
func TestGFrameFiftyMegabyteStreamStaysBounded(t *testing.T) {
	const n = 819200 // 819200 × 64B = 50 MiB exactly
	srv, wantBytes := floodServer(t, n, false)
	if wantBytes != 50<<20 {
		t.Fatalf("fixture is %d bytes, want exactly 50 MiB", wantBytes)
	}
	doc := parseDoc(t, "GET "+srv.URL+"/fifty HTTP/1.1\n\n###\n")

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	m := New(ctx, engine.New(doc), doc, true)
	m.autoQuit = true
	fin := runHeadless(t, m, 120*time.Second)

	if phase, err := fin.Outcome(); phase != phaseDone || err != nil {
		t.Fatalf("outcome = %s, %v; want DONE, nil", phase, err)
	}
	dropped, droppedBytes := fin.Dropped()
	rec, cyc := fin.SinkStats()
	t.Logf("diag: bytes=%d lines=%d droppedLines=%d droppedBytes=%d retained=%d received=%d recycled=%d frames=%d",
		fin.ByteCount(), fin.LineCount(), dropped, droppedBytes, fin.RetainedBytes(), rec, cyc, fin.Frames())
	if got := fin.ByteCount(); got != wantBytes {
		t.Fatalf("bytes = %d, want %d (bytes vanished outside the accounting)", got, wantBytes)
	}
	if got := fin.LineCount(); got != n {
		t.Fatalf("lines = %d, want %d", got, n)
	}
	if dropped == 0 {
		t.Errorf("nothing dropped on a 50 MiB flood against a 1 MiB tail; the bound did not bind")
	}
	if got := fin.RetainedBytes(); got > 2<<20 {
		t.Errorf("retained = %d bytes, exceeds the 2 MiB invariant", got)
	}
	if rec != cyc {
		t.Errorf("sink received=%d recycled=%d on the 50 MiB flood", rec, cyc)
	}
	t.Logf("50 MiB flood: %d lines rendered, %d dropped (%d bytes), retained %d bytes over %d frames",
		fin.LineCount()-dropped, dropped, droppedBytes, fin.RetainedBytes(), fin.Frames())
}

// TestGFrameUpdateAndViewHoldTheFrameBudget is the p99 gate (ROADMAP.md
// §1.2.5): 25 full-size chunks driven straight through Update+View, each
// timed, asserting the 95th percentile under 16 ms. Elm purity is what makes
// this a unit test — no program, no socket, no clock inside the code under
// test. M79 (a 100 ms sleep in Update) lands here at ~100 ms per sample.
func TestGFrameUpdateAndViewHoldTheFrameBudget(t *testing.T) {
	doc := parseDoc(t, "GET http://127.0.0.1:9/none HTTP/1.1\n\n###\n")
	m := New(context.Background(), nil, doc, false)
	if _, cmd := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40}); cmd != nil {
		// The size message arms no command; anything else is a surprise.
		t.Errorf("WindowSizeMsg returned a command")
	}
	const samples = 25
	durs := make([]time.Duration, 0, samples)
	line := strings.Repeat("y", 63) + "\n"
	for i := 0; i < samples; i++ {
		data := []byte(strings.Repeat(line, 512)) // 32 KiB
		start := time.Now()
		updated, _ := m.Update(chunkMsg{data: data})
		m = updated.(*Model)
		_ = m.View()
		durs = append(durs, time.Since(start))
	}
	sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
	p95 := durs[int(float64(len(durs))*0.95)]
	t.Logf("Update+View over 25 full-size chunks: median %s, p95 %s", durs[len(durs)/2], p95)
	if p95 >= frameInterval {
		t.Errorf("p95 Update+View = %s, exceeds the %s frame budget", p95, frameInterval)
	}
}

// TestKeymapIsComplete pins DESIGN_SYSTEM.md §5: every registered binding has
// keys to press and words to show for it, and the help overlay renders every
// one. A binding invisible to help is undiscoverable.
func TestKeymapIsComplete(t *testing.T) {
	k := DefaultKeyMap()
	for _, b := range k.All() {
		if len(b.Keys) == 0 || b.Desc == "" {
			t.Errorf("binding %+v has no keys or no description", b)
		}
	}
	doc := parseDoc(t, "GET http://127.0.0.1:9/none HTTP/1.1\n\n###\n")
	m := New(context.Background(), nil, doc, false)
	m.showHelp = true
	m.width, m.height = 120, 40
	help := m.View()
	for _, b := range k.All() {
		if !strings.Contains(help, b.Desc) {
			t.Errorf("help overlay omits %q", b.Desc)
		}
	}
	// The footer advertises only primary tokens; every token names a binding.
	for _, tok := range k.FooterHelp() {
		if !strings.Contains(m.viewFooter(), tok) {
			t.Errorf("footer dropped its own token %q", tok)
		}
	}
}

// TestTelemetryComponentRendersPhasesAndStatus pins the dashboard contract:
// status line with code and size, microsecond phases beside it, and an
// honest dash for a phase that never ran (reused connection, no DNS).
func TestTelemetryComponentRendersPhasesAndStatus(t *testing.T) {
	epoch := time.Unix(0, 0)
	snap := engine.TelemetrySnapshot{
		DNS:     engine.TracePoint{Op: engine.OpDNS, Start: epoch, End: epoch.Add(123 * time.Microsecond)},
		Connect: engine.TracePoint{Op: engine.OpConnect, Start: epoch, End: epoch.Add(2400 * time.Microsecond)},
	}
	res := &engine.RunResult{Proto: "HTTP/1.1", Status: 200, StatusText: "200 OK", Bytes: 2048, Elapsed: 1500 * time.Microsecond}
	out := RenderTelemetry(snap, res)
	for _, want := range []string{"200", "2.0KiB", "dns 123µs", "tcp 2.4ms", "tls —"} {
		if !strings.Contains(out, want) {
			t.Errorf("dashboard %q omits %q", out, want)
		}
	}
	if out := RenderTelemetry(engine.TelemetrySnapshot{}, nil); strings.Contains(out, "0µs") || strings.Contains(out, "0.0ms") {
		t.Errorf("empty snapshot fabricated a zero measurement: %q", out)
	}
}
