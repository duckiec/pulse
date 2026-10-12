package engine_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pulse/internal/ast"
	"pulse/internal/engine"
)

// ---- helpers ----------------------------------------------------------------

// sseSink collects everything a stream delivers.
type sseSink struct {
	mu     sync.Mutex
	frames []string
	result *engine.RunResult
	err    error
	done   chan struct{}
	once   sync.Once
}

func newSSESink() *sseSink { return &sseSink{done: make(chan struct{})} }

func (s *sseSink) Chunk(c *engine.Chunk) {
	s.mu.Lock()
	if !c.Final {
		s.frames = append(s.frames, string(c.Bytes()))
	}
	s.mu.Unlock()
	c.Recycle()
}

func (s *sseSink) Done(r *engine.RunResult) {
	s.mu.Lock()
	s.result = r
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
}

func (s *sseSink) Failed(e error) {
	s.mu.Lock()
	s.err = e
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
}

func (s *sseSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.frames, "")
}

func (s *sseSink) wait(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(d):
		t.Fatal("stream never reached a terminal callback")
	}
}

// ---- SSE parser -------------------------------------------------------------

// TestSSEParserFieldRules pins the wire-format rules: field/value split, one
// optional space, multi-line data joining, comments, and unknown fields.
func TestSSEParserFieldRules(t *testing.T) {
	arena := ast.AcquireArena()
	defer arena.Release()
	_ = arena

	// The parser is exercised through the public SSE path; here we drive the
	// stream itself with a synthetic server so the framing is end-to-end.
	body := strings.Join([]string{
		": heartbeat comment",
		"",
		"id: 1",
		"event: tick",
		"data: first",
		"data: second",
		"",
		"id: 2",
		"data:no-space-after-colon",
		"",
		"unknown-field: ignored",
		"retry: 250",
		"id: 3",
		"data: third",
		"",
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	sink := newSSESink()
	st, err := engine.OpenSSE(context.Background(), srv.URL, sink, engine.SSEConfig{
		Idle:          200 * time.Millisecond,
		FrameInterval: 10 * time.Millisecond,
		MinBackoff:    10 * time.Millisecond,
		MaxBackoff:    20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("OpenSSE: %v", err)
	}
	// Stop after the first pass; the server ends the body, the manager
	// reconnects, and Close stops that.
	time.Sleep(80 * time.Millisecond)
	_ = st.Close()

	got := sink.text()
	for _, want := range []string{
		"id: 1", "event: tick", "data: first\nsecond",
		"id: 2",
		"id: 3", "data: third",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in stream:\n%s", want, got)
		}
	}
	// The VALUE of a field written without a space after the colon is preserved
	// verbatim; only the re-serialization is canonical ("data: <value>"), since
	// frames are re-assembled from parsed events rather than copied verbatim.
	if !strings.Contains(got, "no-space-after-colon") {
		t.Errorf("field value from a colon with no space was lost:\n%s", got)
	}
	if strings.Contains(got, "heartbeat comment") {
		t.Error("a comment must not appear as data")
	}
	if strings.Contains(got, "unknown-field") {
		t.Error("an unknown field must be ignored, not echoed")
	}
}

// TestSSEParserSplitAcrossChunks proves the parser is resumable: every event is
// cut at every possible byte boundary and the result must be identical.
func TestSSEParserSplitAcrossChunks(t *testing.T) {
	body := "id: a\ndata: alpha\n\nid: b\ndata: beta\n\n"
	want := []string{"alpha", "beta"}

	for split := 1; split < len(body); split++ {
		t.Run(fmt.Sprintf("split-%d", split), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				// Write in two pieces with a flush between, so the parser sees
				// the event straddle the chunk boundary.
				fmt.Fprint(w, body[:split])
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(10 * time.Millisecond)
				fmt.Fprint(w, body[split:])
			}))
			defer srv.Close()

			sink := newSSESink()
			st, err := engine.OpenSSE(context.Background(), srv.URL, sink, engine.SSEConfig{
				Idle: 300 * time.Millisecond, FrameInterval: 5 * time.Millisecond,
				MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("OpenSSE: %v", err)
			}
			time.Sleep(60 * time.Millisecond)
			_ = st.Close()

			got := sink.text()
			for _, w := range want {
				if !strings.Contains(got, w) {
					t.Fatalf("split %d lost event %q:\n%s", split, w, got)
				}
			}
		})
	}
}

// ---- the reconnect gate -----------------------------------------------------

// TestReconnectReplaysLastEventID is THE gate (ROADMAP.md §2.1.6).
//
// The server emits two events, then drops the TCP connection mid-stream. The
// client must reconnect automatically AND send Last-Event-ID with the last id it
// saw — proving the resume token survived the disconnect.
func TestReconnectReplaysLastEventID(t *testing.T) {
	var (
		conns atomic.Int64
		// seen records the Last-Event-ID of EACH connection, so the assertion
		// reads connection 2 specifically. Reading "the latest" would race with
		// a third reconnect that has already replayed the next id.
		seenMu sync.Mutex
		seen   []string
	)
	record := func(id string) {
		seenMu.Lock()
		seen = append(seen, id)
		seenMu.Unlock()
	}
	idAt := func(i int) string {
		seenMu.Lock()
		defer seenMu.Unlock()
		if i < len(seen) {
			return seen[i]
		}
		return ""
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := conns.Add(1)
		record(r.Header.Get("Last-Event-ID"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)

		if n == 1 {
			// Connection 1: two events, then a hard drop.
			fmt.Fprint(w, "id: evt-1\ndata: first\n\n")
			if f != nil {
				f.Flush()
			}
			fmt.Fprint(w, "id: evt-2\ndata: second\n\n")
			if f != nil {
				f.Flush()
			}
			// Kill the connection without a clean end-of-stream.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			panic("drop")
		}
		// Connection 2+: replay from the client's resume point, then finish.
		if got := r.Header.Get("Last-Event-ID"); got != "" {
			fmt.Fprintf(w, "id: evt-3\ndata: resumed-after-%s\n\n", got)
		} else {
			fmt.Fprint(w, "id: evt-3\ndata: resumed-without-token\n\n")
		}
		if f != nil {
			f.Flush()
		}
	}))
	defer srv.Close()

	sink := newSSESink()
	var attempts atomic.Int64
	st, err := engine.OpenSSE(context.Background(), srv.URL, sink, engine.SSEConfig{
		Idle:          300 * time.Millisecond,
		FrameInterval: 5 * time.Millisecond,
		MinBackoff:    10 * time.Millisecond,
		MaxBackoff:    25 * time.Millisecond,
		OnReconnect:   func(int, string) { attempts.Add(1) },
	})
	if err != nil {
		t.Fatalf("OpenSSE: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for conns.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = st.Close()

	if n := conns.Load(); n < 2 {
		t.Fatalf("client never reconnected (connections=%d)", n)
	}
	// The second connection must have replayed the LAST id the client saw.
	if got := idAt(1); got != "evt-2" {
		t.Errorf("Last-Event-ID on reconnect = %q, want %q", got, "evt-2")
	}
	if got := idAt(0); got != "" {
		t.Errorf("first connection must not send a resume token, got %q", got)
	}
	if a := attempts.Load(); a < 1 {
		t.Errorf("OnReconnect fired %d times, want >= 1", a)
	}
	// Wait for the resumed payload itself, not merely for the second
	// connection: a frame is dispatched on the next frame tick, so asserting
	// immediately after the server logs connection 2 races the dispatcher. This
	// is what made the gate flaky under -count=2.
	bodyDeadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(sink.text(), "resumed-after-evt-2") && time.Now().Before(bodyDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if body := sink.text(); !strings.Contains(body, "resumed-after-evt-2") {
		t.Errorf("reconnect did not replay from the resume point:\n%s", body)
	}
}

// TestLastEventIDSurvivesParserRebuild pins the exact invariant the mutation
// breaks: the resume token lives on the MANAGER, not on the per-connection
// parser, so rebuilding the parser cannot lose it.
func TestLastEventIDSurvivesParserRebuild(t *testing.T) {
	var conns atomic.Int64
	var seenMu sync.Mutex
	var seen []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conns.Add(1)
		seenMu.Lock()
		seen = append(seen, r.Header.Get("Last-Event-ID"))
		seenMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "id: keep-me\ndata: x\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(20 * time.Millisecond)
	}))
	defer srv.Close()

	sink := newSSESink()
	st, err := engine.OpenSSE(context.Background(), srv.URL, sink, engine.SSEConfig{
		Idle: 100 * time.Millisecond, FrameInterval: 5 * time.Millisecond,
		MinBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("OpenSSE: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for conns.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = st.Close()

	seenMu.Lock()
	got := ""
	if len(seen) > 1 {
		got = seen[1]
	}
	seenMu.Unlock()
	if got != "keep-me" {
		t.Errorf("resume token lost across reconnect: Last-Event-ID = %q, want %q", got, "keep-me")
	}
}

// TestStreamIsReadOnly proves a read-only transport refuses writes instead of
// silently accepting them (ROADMAP.md §2.1.1).
func TestStreamIsReadOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: hi\n\n")
	}))
	defer srv.Close()

	st, err := engine.OpenSSE(context.Background(), srv.URL, newSSESink(), engine.SSEConfig{
		Idle: 200 * time.Millisecond, FrameInterval: 5 * time.Millisecond,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("OpenSSE: %v", err)
	}
	defer st.Close()

	if got := st.Kind(); got != engine.KindSSE {
		t.Errorf("Kind() = %v, want SSE", got)
	}
	if err := st.Write([]byte("x")); err != engine.ErrWriteUnsupported {
		t.Errorf("Write on a read-only stream = %v, want ErrWriteUnsupported", err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Close must be idempotent.
	if err := st.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestStateMachineTransitions proves the lifecycle is deterministic and that
// the manager reports Connecting/Streaming/Closed rather than lying.
func TestStateMachineTransitions(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}))
	defer srv.Close()

	mgr := engine.NewSSEStreamManager(engine.SSEConfig{
		Idle: time.Second, FrameInterval: 5 * time.Millisecond,
		MinBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
	})
	if got := mgr.State(); got != engine.StateIdle {
		t.Errorf("initial state = %v, want Idle", got)
	}

	done := make(chan error, 1)
	go func() { done <- mgr.Run(context.Background(), srv.URL, newSSESink()) }()

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("stream never produced its first event")
	}
	// The first event is WRITTEN before the client can have observed the
	// response, so poll for the state rather than sampling it once.
	stateDeadline := time.Now().Add(2 * time.Second)
	for mgr.State() != engine.StateStreaming && time.Now().Before(stateDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := mgr.State(); got != engine.StateStreaming {
		t.Errorf("state while streaming = %v, want Streaming", got)
	}
	if mgr.Attempts() < 1 {
		t.Errorf("attempts = %d, want >= 1", mgr.Attempts())
	}

	close(release)
	_ = mgr.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("manager did not unwind")
	}
	if got := mgr.State(); got != engine.StateClosed {
		t.Errorf("final state = %v, want Closed", got)
	}
	<-mgr.Done()
}

// TestFrameAlignedDispatch proves event floods are coalesced into frames: a
// server emitting many events quickly must not produce one chunk per event.
func TestFrameAlignedDispatch(t *testing.T) {
	const events = 500
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f, _ := w.(http.Flusher)
		for i := range events {
			fmt.Fprintf(w, "id: %d\ndata: payload-%d\n\n", i, i)
			if f != nil {
				f.Flush()
			}
		}
	}))
	defer srv.Close()

	var chunks atomic.Int64
	sink := &countingSink{inner: newSSESink(), chunks: &chunks}
	st, err := engine.OpenSSE(context.Background(), srv.URL, sink, engine.SSEConfig{
		Idle: time.Second, FrameInterval: 20 * time.Millisecond,
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("OpenSSE: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	_ = st.Close()

	if got := chunks.Load(); got == 0 {
		t.Fatal("no frames were dispatched")
	}
	// 500 events coalesced into frames bounded by the frame interval: strictly
	// fewer than one chunk per event, which is the whole point (§2.1.5).
	if got := chunks.Load(); got >= events {
		t.Errorf("dispatched %d chunks for %d events: not frame-coalesced", got, events)
	}
}

type countingSink struct {
	inner  *sseSink
	chunks *atomic.Int64
}

func (c *countingSink) Chunk(ch *engine.Chunk) {
	if !ch.Final {
		c.chunks.Add(1)
	}
	c.inner.Chunk(ch)
}
func (c *countingSink) Done(r *engine.RunResult) { c.inner.Done(r) }
func (c *countingSink) Failed(e error)           { c.inner.Failed(e) }

// TestIdleTimeoutIsRetryable proves a silent server surfaces as ErrIdle and
// triggers a reconnect rather than wedging (§2.1.2).
func TestIdleTimeoutIsRetryable(t *testing.T) {
	var conns atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conns.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Say nothing at all: the client's idle deadline must fire.
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	st, err := engine.OpenSSE(context.Background(), srv.URL, newSSESink(), engine.SSEConfig{
		Idle: 80 * time.Millisecond, FrameInterval: 10 * time.Millisecond,
		MinBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("OpenSSE: %v", err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for conns.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = st.Close()

	if n := conns.Load(); n < 2 {
		t.Errorf("idle server did not trigger a reconnect (connections=%d); "+
			"a silent server must not wedge the stream", n)
	}
}

// TestNonSuccessStatusFails proves a 4xx/5x is terminal, not retried forever.
func TestNonSuccessStatusFails(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()

	sink := newSSESink()
	st, err := engine.OpenSSE(context.Background(), srv.URL, sink, engine.SSEConfig{
		Idle: time.Second, FrameInterval: 5 * time.Millisecond,
		MinBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("OpenSSE: %v", err)
	}
	sink.wait(t, 3*time.Second)
	_ = st.Close()

	if sink.err == nil {
		t.Error("a 401 must produce Failed, not a successful stream")
	}
	if hits.Load() != 1 {
		t.Errorf("a terminal status was retried %d times", hits.Load())
	}
}
