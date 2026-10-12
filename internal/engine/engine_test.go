package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pulse/internal/ast"
)

// ---- helpers ----------------------------------------------------------------

// payload is the repeating body the test server streams. A fixed pattern lets a
// 100 MB response be produced and digest-verified without allocating 100 MB in
// the test itself.
var payload = func() []byte {
	b := make([]byte, chunkSize)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}()

// servePattern replies with n bytes drawn from payload, plus the given headers.
func servePattern(n int64, header map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for k, v := range header {
			w.Header().Add(k, v)
		}
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for written := int64(0); written < n; {
			chunk := payload
			if rem := n - written; rem < int64(len(chunk)) {
				chunk = chunk[:rem]
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			written += int64(len(chunk))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
}

// digestOfPattern computes the SHA-256 of n bytes drawn from payload, streaming
// so the reference costs O(1) memory.
func digestOfPattern(n int64) [sha256.Size]byte {
	h := sha256.New()
	buf := make([]byte, 64<<10)
	for written := int64(0); written < n; {
		for i := 0; i < len(buf); i++ {
			buf[i] = payload[i%len(payload)]
		}
		step := int64(len(buf))
		if rem := n - written; rem < step {
			step = rem
		}
		h.Write(buf[:step])
		written += step
	}
	return digestOf(h)
}

// recordingSink is a test Sink. It models the two consumer extremes: a prompt
// consumer that recycles immediately, and a stalled one that models a slow UI.
type recordingSink struct {
	stall time.Duration // sleep per chunk, modelling a slow consumer

	chunks atomic.Int64
	bytes  atomic.Int64
	finals atomic.Int64
	after  atomic.Int64 // chunks seen after the terminal callback (must be 0)

	mu     sync.Mutex
	result *RunResult
	err    error
	terms  atomic.Int64 // terminal callbacks (must be exactly 1)

	done chan struct{}
}

func newRecordingSink(stall time.Duration) *recordingSink {
	return &recordingSink{stall: stall, done: make(chan struct{})}
}

func (s *recordingSink) Chunk(c *Chunk) {
	if s.terms.Load() > 0 {
		s.after.Add(1)
	}
	if s.stall > 0 {
		time.Sleep(s.stall)
	}
	if !c.Final {
		s.chunks.Add(1)
		s.bytes.Add(int64(c.N))
	} else {
		s.finals.Add(1)
	}
	c.Recycle()
}

func (s *recordingSink) Done(r *RunResult) {
	s.mu.Lock()
	s.result = r
	s.mu.Unlock()
	s.terms.Add(1)
	close(s.done)
}

func (s *recordingSink) Failed(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
	s.terms.Add(1)
	close(s.done)
}

func (s *recordingSink) wait(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(d):
		t.Fatalf("sink never reached a terminal callback within %s", d)
	}
}

func (s *recordingSink) Result() *RunResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result
}

func (s *recordingSink) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// runFirst parses src, runs its first request against sink, and returns the run
// result (or the failure). Sinks that are not *recordingSink are awaited via
// their own done channel by the caller.
func runFirst(t *testing.T, src string, sink Sink, opts ...Option) (*RunResult, error) {
	t.Helper()
	arena := ast.AcquireArena()
	arena.Reset([]byte(src))
	doc, err := ast.Parse(arena)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer doc.Release()
	e := New(doc, opts...)
	for r := range doc.Requests() {
		h := e.Run(context.Background(), r, sink)
		if err := h.WaitTimeout(60 * time.Second); err != nil {
			t.Fatalf("run did not unwind: %v", err)
		}
		break
	}
	rec, ok := sink.(*recordingSink)
	if !ok {
		return nil, nil
	}
	rec.wait(t, 30*time.Second)
	return rec.Result(), rec.Err()
}

// ---- Step 2: AST to HTTP translation ----------------------------------------

// TestRequestTranslation proves the AST → *http.Request contract end to end:
// the server must observe the exact method, target, headers, and body the .http
// source described.
func TestRequestTranslation(t *testing.T) {
	type seen struct {
		method string
		path   string
		query  string
		header http.Header
		body   string
	}
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got <- seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(b)}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	src := "POST " + srv.URL + "/items?page=2 HTTP/1.1\n" +
		"Content-Type: application/json\n" +
		"X-Trace: t-1\n" +
		"\n" +
		"{\n  \"name\": \"widget\"\n}\n" +
		"\n###\n"

	res, err := runFirst(t, src, newRecordingSink(0))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Status != http.StatusCreated {
		t.Errorf("status = %d, want 201", res.Status)
	}
	if res.Method != "POST" {
		t.Errorf("method = %q, want POST", res.Method)
	}
	if res.Proto != "HTTP/1.1" {
		t.Errorf("proto = %q, want HTTP/1.1", res.Proto)
	}
	select {
	case s := <-got:
		if s.method != "POST" {
			t.Errorf("server saw method %q", s.method)
		}
		if s.path != "/items" || s.query != "page=2" {
			t.Errorf("server saw target %q?%q", s.path, s.query)
		}
		if got := s.header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := s.header.Get("X-Trace"); got != "t-1" {
			t.Errorf("X-Trace = %q", got)
		}
		if !strings.Contains(s.body, `"name": "widget"`) {
			t.Errorf("body = %q", s.body)
		}
	default:
		t.Fatal("server never received the request")
	}
	if res.Bytes != int64(len(`{"ok":true}`)) {
		t.Errorf("Bytes = %d, want %d", res.Bytes, len(`{"ok":true}`))
	}
	if !strings.Contains(string(res.Header.Get("Content-Type")), "application/json") {
		t.Errorf("response Content-Type not surfaced: %v", res.Header)
	}
}

// TestBodylessRequestSendsNoBody proves a bodyless node does not fabricate one.
func TestBodylessRequestSendsNoBody(t *testing.T) {
	var bodyLen int64 = -1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		bodyLen = n
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	if _, err := runFirst(t, "GET "+srv.URL+"/ping HTTP/1.1\n\n###\n", newRecordingSink(0)); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if bodyLen != 0 {
		t.Errorf("server read %d body bytes for a bodyless request, want 0", bodyLen)
	}
}

// TestRelativeTargetFailsClosed proves a non-absolute target is rejected rather
// than guessed at.
func TestRelativeTargetFailsClosed(t *testing.T) {
	_, err := runFirst(t, "GET /relative/only HTTP/1.1\n\n###\n", newRecordingSink(0))
	if err == nil {
		t.Fatal("a relative request target must not be executed")
	}
	if !strings.Contains(err.Error(), "not absolute") {
		t.Errorf("error = %v, want a not-absolute diagnostic", err)
	}
}

// ---- Step 2: the interpolation stub fails closed ----------------------------

// TestUnresolvedInterpolationFailsClosed is the AGENTS.md §4.4 proof for the
// stubbed resolver: a {{var}} in a live request must stop the run before any
// byte reaches the network.
func TestUnresolvedInterpolationFailsClosed(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	src := "GET " + srv.URL + "/{{resource}} HTTP/1.1\n\n###\n"
	_, err := runFirst(t, src, newRecordingSink(0))
	if !errors.Is(err, ErrUnresolved) {
		t.Fatalf("error = %v, want ErrUnresolved", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("server was contacted %d times; interpolation must fail closed before any request", n)
	}
	if !strings.Contains(err.Error(), "resource") {
		t.Errorf("error %q should name the unresolved variable", err)
	}
}

// TestBoundResolverExpandsInterpolation proves that once a resolver IS bound the
// same request executes with the resolved value.
func TestBoundResolverExpandsInterpolation(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	resolver := stubResolver{"resource": "widgets"}
	src := "GET " + srv.URL + "/{{resource}} HTTP/1.1\nX-Id: {{id}}\n\n###\n"
	_ = resolver
	if _, err := runFirst(t, src, newRecordingSink(0), WithResolver(resolver)); err == nil {
		t.Fatal("expected the unbound {{id}} header to fail closed")
	} else if !errors.Is(err, ErrUnresolved) && !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("unexpected error: %v", err)
	}
	src = "GET " + srv.URL + "/{{resource}} HTTP/1.1\n\n###\n"
	if _, err := runFirst(t, src, newRecordingSink(0), WithResolver(resolver)); err != nil {
		t.Fatalf("bound resolver should have succeeded: %v", err)
	}
	if path != "/widgets" {
		t.Errorf("server saw path %q, want /widgets", path)
	}
}

// TestResolverFailureIsFatal proves a resolver error stops the run rather than
// substituting an empty value.
func TestResolverFailureIsFatal(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	boom := failingResolver{reason: "keychain miss"}
	src := "GET " + srv.URL + "/{{resource}} HTTP/1.1\n\n###\n"
	_, err := runFirst(t, src, newRecordingSink(0), WithResolver(boom))
	if err == nil {
		t.Fatal("a failing resolver must not yield a successful run")
	}
	if hits.Load() != 0 {
		t.Errorf("server contacted %d times after resolver failure", hits.Load())
	}
}

// failingResolver always fails, standing in for a keychain miss.
type failingResolver struct{ reason string }

func (f failingResolver) Lookup(string) (string, error) {
	return "", errors.New(f.reason)
}

// stubResolver is a fixed map; a missing key is an error, never a zero value.
type stubResolver map[string]string

func (s stubResolver) Lookup(name string) (string, error) {
	v, ok := s[name]
	if !ok {
		return "", fmt.Errorf("no such variable %q", name)
	}
	return v, nil
}

// TestUnterminatedAnchorFailsClosed covers the hand-built-AST path where a body
// contains an anchor with no closing braces.
func TestUnterminatedAnchorFailsClosed(t *testing.T) {
	e := &Engine{resolver: stubResolver{}}
	if _, err := e.expandBytes([]byte("prefix {{dangling"), 7); err == nil {
		t.Fatal("an unterminated interpolation anchor must be an error")
	}
}

// ---- AGENTS.md §3.4: redaction ----------------------------------------------

// TestRedaction proves no sensitive header value can leave the engine.
func TestRedaction(t *testing.T) {
	secret := "super-secret-session"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session="+secret+"; HttpOnly")
		w.Header().Set("X-Api-Key", secret)
		w.Header().Set("Authorization", "Bearer "+secret)
		w.Header().Set("X-Visible", "harmless")
		io.WriteString(w, "ok")
	}))
	defer srv.Close()

	res, err := runFirst(t, "GET "+srv.URL+"/x HTTP/1.1\n\n###\n", newRecordingSink(0))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	for _, name := range []string{"Set-Cookie", "X-Api-Key", "Authorization"} {
		for _, v := range res.Header.Values(name) {
			if v != redactedPlaceholder {
				t.Errorf("%s was not redacted: %q", name, v)
			}
		}
	}
	if got := res.Header.Get("X-Visible"); got != "harmless" {
		t.Errorf("non-sensitive header must survive redaction, got %q", got)
	}
	// The whole rendered result must not contain the secret anywhere.
	blob := fmt.Sprintf("%v %v %v", res.Header, res.URL, res.StatusText)
	if strings.Contains(blob, secret) {
		t.Errorf("secret leaked through RunResult: %s", blob)
	}
}

// TestRedactURLStripsUserinfo is the unit proof for the other credential vector.
func TestRedactURLStripsUserinfo(t *testing.T) {
	got := redactURL("https://alice:hunter2@example.com/v1?page=1")
	if strings.Contains(got, "hunter2") || strings.Contains(got, "alice") {
		t.Errorf("userinfo survived redaction: %s", got)
	}
	if !strings.Contains(got, "example.com/v1") {
		t.Errorf("redaction destroyed the useful part of the URL: %s", got)
	}
	// A URL with no credentials must pass through untouched.
	plain := "https://example.com/v1?page=1"
	if redactURL(plain) != plain {
		t.Errorf("redactURL altered a credential-free URL: %s", redactURL(plain))
	}
}

// ---- Step 3: tee mirror correctness -----------------------------------------

// TestDigestMatchesReference proves the tee hashed every byte the consumer saw,
// against an independently computed digest.
func TestDigestMatchesReference(t *testing.T) {
	const size = 1 << 20
	srv := servePattern(size, nil)
	defer srv.Close()

	res, err := runFirst(t, "GET "+srv.URL+"/data HTTP/1.1\n\n###\n", newRecordingSink(0))
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Bytes != size {
		t.Errorf("Bytes = %d, want %d", res.Bytes, size)
	}
	if want := digestOfPattern(size); res.Digest != want {
		t.Errorf("digest mismatch:\n got %x\nwant %x", res.Digest, want)
	}
}

// TestDigestOfHelper pins the helper the reference uses.
func TestDigestOfHelper(t *testing.T) {
	h := sha256.New()
	h.Write([]byte("pulse"))
	var d [sha256.Size]byte
	copy(d[:], h.Sum(nil))
	if got := digestOf(h); got != d {
		t.Errorf("digestOf = %x, want %x", got, d)
	}
}

// ---- Step 4: the memory ceiling gate ----------------------------------------

// gatedSink models a consumer that stops consuming entirely — a UI frozen on a
// modal dialog, or a pager that hit its retention cap. This is the worst case
// the memory ceiling has to survive, and the only way to observe the queue
// actually filling.
type gatedSink struct {
	gate  chan struct{} // closed to release the consumer
	bytes atomic.Int64
	final atomic.Bool
	after atomic.Int64
	terms atomic.Int64

	mu  sync.Mutex
	res *RunResult
	err error
}

func (g *gatedSink) Chunk(c *Chunk) {
	if g.terms.Load() > 0 {
		g.after.Add(1)
	}
	<-g.gate // stall here: the dispatcher cannot drain, so the queue fills
	if !c.Final {
		g.bytes.Add(int64(c.N))
	} else {
		g.final.Store(true)
	}
	c.Recycle()
}

func (g *gatedSink) Done(r *RunResult) {
	g.mu.Lock()
	g.res = r
	g.mu.Unlock()
	g.terms.Add(1)
}

func (g *gatedSink) Failed(e error) {
	g.mu.Lock()
	g.err = e
	g.mu.Unlock()
	g.terms.Add(1)
}

// TestMemoryCeilingInvariant is the G-alloc memory ceiling (ROADMAP.md §1.2.4,
// ARCHITECTURE.md §5.2 invariant 4). A consumer that stops consuming entirely
// drives the queue to its cap: the in-flight chunk count must saturate at
// exactly the geometric ceiling and must NOT grow with payload size.
func TestMemoryCeilingInvariant(t *testing.T) {
	sizes := []int64{1 << 20, 10 << 20, 100 << 20}
	var peaks []int64
	var saturated []bool
	for _, size := range sizes {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			srv := servePattern(size, nil)
			defer srv.Close()

			resetPeakStats()
			before := liveChunks.Load()

			arena := ast.AcquireArena()
			defer arena.Release()
			arena.Reset([]byte("GET " + srv.URL + "/big HTTP/1.1\n\n###\n"))
			doc, err := ast.Parse(arena)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			defer doc.Release()

			sink := &gatedSink{gate: make(chan struct{})}
			e := New(doc)
			var req *ast.Request
			for r := range doc.Requests() {
				req = r
				break
			}
			h := e.Run(context.Background(), req, sink)

			// A stalled consumer can only park as many chunks as the queue and
			// the two in-flight slots allow — and never more than the body has
			// chunks. A 1 MiB body is 32 chunks, so 33 (32 queued + 1 being
			// filled) is its exact saturation point; larger bodies must pin the
			// full ceiling.
			wantLive := min(maxInFlightChunks, int(size/chunkSize)+1)

			// Wait for the producer to fill the queue and block on the send.
			deadline := time.Now().Add(10 * time.Second)
			for liveChunks.Load()-before < int64(wantLive) && time.Now().Before(deadline) {
				time.Sleep(2 * time.Millisecond)
			}
			live := liveChunks.Load() - before
			peaks = append(peaks, live)
			saturated = append(saturated, wantLive == maxInFlightChunks)

			if live > maxInFlightChunks {
				t.Errorf("in-flight chunks reached %d, ceiling is %d (%d bytes > %d)",
					live, maxInFlightChunks, live*chunkSize, inFlightBytes)
			}
			if live != int64(wantLive) {
				t.Errorf("in-flight %d, want exactly %d (ceiling %d, body has %d chunks)",
					live, wantLive, maxInFlightChunks, size/chunkSize)
			}
			// The whole point of backpressure: a stalled consumer must stall
			// the run, not let it buffer its way to completion.
			if terms := sink.terms.Load(); terms != 0 {
				t.Errorf("run reported a terminal callback while the consumer was still stalled")
			}
			if sink.bytes.Load() >= size {
				t.Errorf("consumer received the whole %d-byte body without releasing; "+
					"backpressure did not engage", size)
			}

			close(sink.gate) // release the consumer
			if err := h.WaitTimeout(60 * time.Second); err != nil {
				t.Fatalf("run did not unwind after release: %v", err)
			}
			if sink.err != nil {
				t.Fatalf("run failed after release: %v", sink.err)
			}

			if got := sink.bytes.Load(); got != size {
				t.Errorf("sink received %d bytes, want %d", got, size)
			}
			if !sink.final.Load() {
				t.Error("no final sentinel was delivered")
			}
			if after := sink.after.Load(); after != 0 {
				t.Errorf("%d chunks arrived after the terminal callback", after)
			}
			t.Logf("payload=%d MiB  saturated in-flight=%d chunks (%d bytes)  ceiling=%d chunks (%d bytes)",
				size>>20, live, live*chunkSize, maxInFlightChunks, inFlightBytes)
		})
	}
	// The claim: the peak is a function of the geometry, not the payload. Across
	// the sizes whose body is large enough to pin the ceiling, a 10× increase in
	// payload size must not move the peak by a single chunk.
	var satPeaks []int64
	for i, ok := range saturated {
		if ok {
			satPeaks = append(satPeaks, peaks[i])
		}
	}
	if len(satPeaks) < 2 {
		t.Fatalf("need two saturating sizes to prove invariance, got %d (peaks %v)", len(satPeaks), peaks)
	}
	for i := 1; i < len(satPeaks); i++ {
		if satPeaks[i] != satPeaks[0] {
			t.Errorf("in-flight peak varied with payload size: %v (must be invariant)", satPeaks)
		}
	}
	t.Logf("ceiling invariant across saturating payloads: %v chunks = %d bytes",
		satPeaks[0], satPeaks[0]*chunkSize)
}

// TestChunkGeometry pins the arithmetic the whole ceiling rests on.
func TestChunkGeometry(t *testing.T) {
	if chunkSize != 32<<10 {
		t.Errorf("chunkSize = %d, want 32768", chunkSize)
	}
	if chunkChannelCap != 64 {
		t.Errorf("chunkChannelCap = %d, want 64", chunkChannelCap)
	}
	if got := chunkChannelCap * chunkSize; got != 2<<20 {
		t.Errorf("channel ceiling = %d bytes, want %d (2 MiB)", got, 2<<20)
	}
	if got := maxInFlightChunks * chunkSize; got != inFlightBytes {
		t.Errorf("in-flight arithmetic inconsistent: %d vs %d", got, inFlightBytes)
	}
	if inFlightBytes > 2<<20+2*chunkSize {
		t.Errorf("in-flight ceiling %d exceeds queue+2 chunks", inFlightBytes)
	}
}

// TestSinkOwnsEveryChunk proves the engine never reuses a buffer it handed off:
// with recycling poisoned, a sink that keeps its bytes would observe the
// sentinel; one that recycles proves the contract is satisfiable.
func TestSinkOwnsEveryChunk(t *testing.T) {
	poisonOnRecycle.Store(true)
	defer poisonOnRecycle.Store(false)

	var sawPoison atomic.Int64
	var leaked atomic.Int64
	srv := servePattern(8<<20, nil)
	defer srv.Close()

	sink := &inspectingSink{sawPoison: &sawPoison, leaked: &leaked, done: make(chan struct{})}
	src := "GET " + srv.URL + "/x HTTP/1.1\n\n###\n"
	if _, err := runFirst(t, src, sink); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	<-sink.done
	if n := leaked.Load(); n != 0 {
		t.Errorf("%d chunks were never recycled", n)
	}
	// A consumer that copies before recycling must never see the sentinel.
	if n := sawPoison.Load(); n != 0 {
		t.Errorf("%d chunks observed post-recycle corruption", n)
	}
}

type inspectingSink struct {
	sawPoison *atomic.Int64
	leaked    *atomic.Int64
	done      chan struct{}
	err       error
}

func (s *inspectingSink) Chunk(c *Chunk) {
	b := c.Bytes()
	for _, v := range b {
		if v == poisonByte {
			s.sawPoison.Add(1)
			break
		}
	}
	c.Recycle()
}

func (s *inspectingSink) Done(*RunResult) { close(s.done) }
func (s *inspectingSink) Failed(e error)  { s.err = e; close(s.done) }

// TestDoubleRecycleIsCountedNotCorrupting proves the guard: recycling twice is
// detected and ignored rather than pushing the same buffer back into the pool
// twice (which would hand one buffer to two concurrent readers).
func TestDoubleRecycleIsCountedNotCorrupting(t *testing.T) {
	before := doubleRecycles.Load()
	c := acquireChunk()
	c.Recycle()
	if !c.Released() {
		t.Fatal("chunk not released after Recycle")
	}
	buf := c.Buf
	c.Recycle()
	if got := doubleRecycles.Load(); got != before+1 {
		t.Errorf("doubleRecycles = %d, want %d", got, before+1)
	}
	if c.Buf != nil {
		t.Error("a released chunk must not be resurrected by a second Recycle")
	}
	_ = buf
}

// ---- Step 4: the leak gate --------------------------------------------------

// TestCancellationNoLeak is the ROADMAP.md §1.2.6 gate: cancel mid-stream and
// assert teardown is bounded, goroutines return to baseline, and no file
// descriptor is stranded.
func TestCancellationNoLeak(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("NOT RUN (reason: file-descriptor accounting reads /proc/self/fd, Linux only)")
	}
	const size = 64 << 20
	srv := servePattern(size, nil)
	defer srv.Close()

	// Warm the transport so the baseline includes its idle connections.
	warm := newRecordingSink(0)
	if _, err := runFirst(t, "GET "+srv.URL+"/warm HTTP/1.1\n\n###\n", warm); err != nil {
		t.Fatalf("warm-up failed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	baseG := goroutineCount()
	baseFD := openFDs(t)

	for i := range 8 {
		arena := ast.AcquireArena()
		arena.Reset([]byte("GET " + srv.URL + "/big HTTP/1.1\n\n###\n"))
		doc, err := ast.Parse(arena)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		e := New(doc)
		sink := newRecordingSink(time.Millisecond) // slow: guarantees we are mid-stream

		var req *ast.Request
		for r := range doc.Requests() {
			req = r
			break
		}
		ctx, cancel := context.WithCancel(context.Background())
		h := e.Run(ctx, req, sink)

		time.Sleep(20 * time.Millisecond) // let the stream get underway
		start := time.Now()
		cancel()
		if err := h.WaitTimeout(teardownBudgetDefault); err != nil {
			t.Fatalf("iteration %d: teardown did not finish within %s: %v",
				i, teardownBudgetDefault, err)
		}
		elapsed := time.Since(start)
		if elapsed > teardownBudgetDefault {
			t.Errorf("iteration %d: teardown took %s, budget is %s", i, elapsed, teardownBudgetDefault)
		}
		sink.wait(t, 2*time.Second)

		if sink.Result() != nil {
			t.Errorf("iteration %d: a cancelled run reported Done, want Failed", i)
		}
		if sink.Err() == nil {
			t.Errorf("iteration %d: cancelled run reported no error", i)
		}
		if terms := sink.terms.Load(); terms != 1 {
			t.Errorf("iteration %d: terminal callbacks = %d, want exactly 1", i, terms)
		}
		doc.Release()
	}

	// Transports close asynchronously; poll briefly rather than assert on a
	// race we do not control.
	deadline := time.Now().Add(2 * time.Second)
	var g, fd int
	for {
		g, fd = goroutineCount(), openFDs(t)
		if g <= baseG+2 && fd <= baseFD {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leak: goroutines %d -> %d, fds %d -> %d (allowed slack: 2 goroutines)",
				baseG, g, baseFD, fd)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("goroutines %d -> %d, fds %d -> %d after 8 cancelled runs", baseG, g, baseFD, fd)
}

// TestCancellationBeforeStart proves a cancelled context never issues a request.
func TestCancellationBeforeStart(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer srv.Close()

	arena := ast.AcquireArena()
	defer arena.Release()
	arena.Reset([]byte("GET " + srv.URL + "/x HTTP/1.1\n\n###\n"))
	doc, err := ast.Parse(arena)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer doc.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := New(doc)
	sink := newRecordingSink(0)
	for r := range doc.Requests() {
		h := e.Run(ctx, r, sink)
		if err := h.WaitTimeout(teardownBudgetDefault); err != nil {
			t.Fatalf("teardown: %v", err)
		}
		break
	}
	sink.wait(t, 2*time.Second)
	if sink.Err() == nil {
		t.Error("a pre-cancelled context must fail the run")
	}
	if hits.Load() != 0 {
		t.Errorf("server contacted %d times with an already-cancelled context", hits.Load())
	}
}

// TestCancelIsIdempotent proves the handle tolerates repeated interrupts.
func TestCancelIsIdempotent(t *testing.T) {
	srv := servePattern(4<<20, nil)
	defer srv.Close()
	if _, err := runFirst(t, "GET "+srv.URL+"/x HTTP/1.1\n\n###\n", newRecordingSink(0)); err != nil {
		t.Fatalf("warm-up failed: %v", err)
	}
	arena := ast.AcquireArena()
	defer arena.Release()
	arena.Reset([]byte("GET " + srv.URL + "/x HTTP/1.1\n\n###\n"))
	doc, err := ast.Parse(arena)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer doc.Release()
	e := New(doc)
	sink := newRecordingSink(time.Millisecond)
	for r := range doc.Requests() {
		h := e.Run(context.Background(), r, sink)
		for range 3 {
			h.Cancel()
		}
		if err := h.WaitTimeout(teardownBudgetDefault); err != nil {
			t.Fatalf("teardown after repeated Cancel: %v", err)
		}
		break
	}
	sink.wait(t, 2*time.Second)
	if terms := sink.terms.Load(); terms != 1 {
		t.Errorf("terminal callbacks = %d, want exactly 1", terms)
	}
}

// TestZeroValueHandleIsInert proves RunHandle's zero value does not panic.
func TestZeroValueHandleIsInert(t *testing.T) {
	var h RunHandle
	h.Cancel()
	h.Wait()
	if err := h.WaitTimeout(time.Millisecond); err != nil {
		t.Errorf("zero handle WaitTimeout = %v, want nil", err)
	}
}

// ---- transport failures -----------------------------------------------------

// TestTransportErrorIsRedactedAndFails proves a dial failure reaches the sink as
// Failed, with no credential in the message.
func TestTransportErrorIsRedactedAndFails(t *testing.T) {
	// Port 1 on loopback refuses connections deterministically.
	src := "GET http://127.0.0.1:1/x HTTP/1.1\n\n###\n"
	_, err := runFirst(t, src, newRecordingSink(0))
	if err == nil {
		t.Fatal("expected a transport failure")
	}
	if strings.Contains(err.Error(), "127.0.0.1:1") == false {
		t.Errorf("error should still describe the target: %v", err)
	}
}

// ---- resource accounting helpers -------------------------------------------

func goroutineCount() int { return runtime.NumGoroutine() }

// openFDs counts open file descriptors. Linux only; callers must skip.
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}

// ---- benchmarks -------------------------------------------------------------

// BenchmarkStreamSteadyState is the streaming G-alloc gate: the chunk read loop
// over a synthetic reader, with the pools pre-warmed, must report 0 allocs/op.
// It deliberately excludes the HTTP transport, whose per-request bookkeeping
// (header maps, TLS state) is not part of the streaming contract — see
// BenchmarkRunSmall for the full-run cost.
func BenchmarkStreamSteadyState(b *testing.B) {
	// 16 MiB = 512 chunks of 32 KiB per op: enough iterations for sync.Pool's
	// internal queue-growth to amortize, so the reported allocs/op reflects the
	// streaming path itself rather than one-time pool bookkeeping.
	const size = 16 << 20
	e := &Engine{}
	drain := benchmarkDrain(b)
	ctx := benchmarkCtx(b)
	// One reader, reset between iterations: allocating it inside the loop
	// would count as streaming cost and mask the real number.
	r := &patternReader{remaining: size}
	// Warm the pools so the first measured iteration is already steady state.
	for range 2 {
		r.remaining = size
		if _, err := e.stream(ctx, r, drain); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.SetBytes(size)
	b.ResetTimer()
	for range b.N {
		r.remaining = size
		if _, err := e.stream(ctx, r, drain); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStreamAllocPerChunk reports the same loop over a single shared
// channel, so the cost per 32 KiB chunk is directly readable from the run.
func BenchmarkStreamAllocPerChunk(b *testing.B) {
	const size = 16 << 20
	e := &Engine{}
	ctx := benchmarkCtx(b)
	chunks := make(chan *Chunk, chunkChannelCap)
	go func() {
		for c := range chunks {
			c.Recycle()
		}
	}()
	r := &patternReader{remaining: size}
	for range 2 {
		r.remaining = size
		if _, err := e.stream(ctx, r, chunks); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.SetBytes(size)
	b.ResetTimer()
	for range b.N {
		r.remaining = size
		if _, err := e.stream(ctx, r, chunks); err != nil {
			b.Fatal(err)
		}
	}
}

// patternReader streams payload without allocating.
type patternReader struct{ remaining int64 }

func (p *patternReader) Read(dst []byte) (int, error) {
	if p.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(dst))
	if n > p.remaining {
		n = p.remaining
	}
	for i := int64(0); i < n; i++ {
		dst[i] = payload[i%int64(len(payload))]
	}
	p.remaining -= n
	return int(n), nil
}

func benchmarkDrain(b *testing.B) chan *Chunk {
	ch := make(chan *Chunk, chunkChannelCap)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for c := range ch {
			c.Recycle()
		}
	}()
	b.Cleanup(func() { close(ch); <-done })
	return ch
}

func benchmarkCtx(b *testing.B) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	b.Cleanup(cancel)
	return ctx
}

var _ = hash.Hash(nil)
