package engine

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// StreamKind identifies the wire under a Stream (ROADMAP.md §2.1.1). The UI and
// MCP layers must never learn which one it is from the type.
type StreamKind uint8

const (
	KindHTTP StreamKind = iota
	KindSSE
	KindWS
	KindGRPC
)

func (k StreamKind) String() string {
	switch k {
	case KindSSE:
		return "SSE"
	case KindWS:
		return "WS"
	case KindGRPC:
		return "GRPC"
	default:
		return "HTTP"
	}
}

// Stream is the unified surface every transport terminates in
// (ROADMAP.md §2.1.1). Read-only transports refuse writes with
// ErrWriteUnsupported rather than pretending to accept them.
type Stream interface {
	// Read blocks for the next pool-backed buffer; Final is set at EOF.
	Read() (*Chunk, error)
	// Write returns ErrWriteUnsupported on a read-only transport.
	Write(p []byte) error
	// Close ends the stream; idempotent and safe from any goroutine.
	Close() error
	// SetDeadline bounds subsequent blocking operations.
	SetDeadline(t time.Time) error
	// Kind reports the underlying wire.
	Kind() StreamKind
}

// StreamState is the connection lifecycle (ROADMAP.md §2.1.2/§2.1.6).
// Transitions are deterministic:
//
//	Idle → Connecting → Streaming ⇄ Connecting (reconnect) → Closed
type StreamState uint8

const (
	StateIdle StreamState = iota
	StateConnecting
	StateStreaming
	StateClosed
)

func (s StreamState) String() string {
	switch s {
	case StateConnecting:
		return "connecting"
	case StateStreaming:
		return "streaming"
	case StateClosed:
		return "closed"
	default:
		return "idle"
	}
}

// StreamManager owns a persistent stream's lifecycle: it opens the connection,
// tracks the state machine, coalesces events into frames, and drives
// reconnection with Last-Event-ID replay. The UI observes state and attempt
// count; it never drives them (ROADMAP.md §2.1.2: "backoff state lives in the
// run handle, not in the model").
type StreamManager interface {
	// Run drives the stream on the calling goroutine: connect, pump, and
	// reconnect until the stream ends, delivering the terminal Sink callback.
	// It blocks; callers that want it backgrounded spawn it themselves.
	Run(ctx context.Context, target string, sink Sink) error
	// State reports the current lifecycle state.
	State() StreamState
	// Attempts counts connection attempts, including reconnects (§2.1.6).
	Attempts() int
	// LastEventID is the resume token replayed on the next connect.
	LastEventID() string
	// Close ends the stream and stops reconnection. Idempotent.
	Close() error
	// Done is closed once the stream has fully unwound.
	Done() <-chan struct{}
}

// SSEConfig configures an SSE connection.
type SSEConfig struct {
	// Idle is how long to wait with no bytes before declaring ErrIdle.
	// Heartbeat comments renew it (§2.1.2).
	Idle time.Duration
	// FrameInterval is the repaint cadence: events are coalesced and dispatched
	// to the Sink at most once per frame, so an event flood cannot lock the UI
	// (§2.1.5). Zero means DefaultFrame.
	FrameInterval time.Duration
	// MinBackoff/MaxBackoff bound reconnection with full jitter.
	MinBackoff, MaxBackoff time.Duration
	// Header carries extra request headers (auth, and anything the caller
	// needs). Last-Event-ID is managed by the stream and overrides this.
	Header http.Header
	// Client is the HTTP client; nil uses a shared default.
	Client *http.Client
	// Telemetry, when non-nil, receives a report for each attempt.
	Telemetry *TelemetryReport
	// OnReconnect fires before each reconnect attempt, after the first.
	OnReconnect func(attempt int, lastEventID string)
}

// DefaultFrame is the 60 FPS repaint cadence (ARCHITECTURE.md §5.3).
const DefaultFrame = 16 * time.Millisecond

const (
	defaultMinBackoff = 500 * time.Millisecond
	defaultMaxBackoff = 30 * time.Second
)

// sseManager is the concrete StreamManager for SSE.
type sseManager struct {
	cfg    SSEConfig
	client *http.Client
	sink   Sink

	state    atomic.Uint32
	attempts atomic.Int64
	done     chan struct{}
	stop     chan struct{}
	closeOne sync.Once

	// lastEventID is owned by the MANAGER, not the parser, so it survives a
	// parser rebuild across reconnects. Clearing it on disconnect is exactly
	// the defect the reconnect gate exists to catch.
	lastEventID atomic.Value // string

	// conn is the live connection, captured via httptrace, so reads can carry
	// a deadline: a response body alone cannot set one.
	conn atomic.Value // net.Conn

	chunks chan *Chunk
	parser *sseParser

	// bytesDelivered and the sha256 accumulator mirror the HTTP path's tee:
	// the digest of everything the consumer saw, with O(1) memory.
	bytesDelivered atomic.Int64
	hasher         hash.Hash
}

// NewSSEStreamManager builds a manager for the given configuration.
func NewSSEStreamManager(cfg SSEConfig) StreamManager {
	if cfg.Idle <= 0 {
		cfg.Idle = 30 * time.Second
	}
	if cfg.FrameInterval <= 0 {
		cfg.FrameInterval = DefaultFrame
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = defaultMinBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Transport: http.DefaultTransport}
	}
	m := &sseManager{
		cfg:    cfg,
		client: client,
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
		chunks: make(chan *Chunk, chunkChannelCap),
		hasher: sha256.New(),
	}
	m.lastEventID.Store("")
	return m
}

// tee mirrors delivered bytes into the response digest without buffering them.
func (m *sseManager) tee(b []byte) {
	if m.hasher != nil && len(b) > 0 {
		_, _ = m.hasher.Write(b)
	}
}

// digest returns the digest of everything the consumer saw.
func (m *sseManager) digest() [sha256.Size]byte {
	var d [sha256.Size]byte
	if m.hasher != nil {
		copy(d[:], m.hasher.Sum(d[:0]))
	}
	return d
}

// State implements StreamManager.
func (m *sseManager) State() StreamState { return StreamState(m.state.Load()) }

// Attempts implements StreamManager.
func (m *sseManager) Attempts() int { return int(m.attempts.Load()) }

// LastEventID implements StreamManager.
func (m *sseManager) LastEventID() string {
	s, _ := m.lastEventID.Load().(string)
	return s
}

// Done implements StreamManager.
func (m *sseManager) Done() <-chan struct{} { return m.done }

// Close implements StreamManager: idempotent, stops reconnection.
func (m *sseManager) Close() error {
	m.closeOne.Do(func() {
		close(m.stop)
		m.setState(StateClosed)
	})
	return nil
}

func (m *sseManager) setState(s StreamState) { m.state.Store(uint32(s)) }

func (m *sseManager) setID(id string) { m.lastEventID.Store(id) }

func (m *sseManager) stopped() bool {
	select {
	case <-m.stop:
		return true
	default:
		return false
	}
}

// Run drives the whole lifecycle on the calling goroutine and delivers the
// terminal Sink callback. It blocks until the stream ends.
func (m *sseManager) Run(ctx context.Context, target string, sink Sink) error {
	m.sink = sink
	started := time.Now()
	dispatchDone := make(chan struct{})
	go m.dispatch(dispatchDone)

	res := &RunResult{Method: "SSE", Started: started}
	var runErr error
	defer func() {
		close(m.chunks)
		<-dispatchDone
		close(m.done)
		res.Elapsed = time.Since(started)
		res.Bytes = m.bytesDelivered.Load()
		res.Digest = m.digest()
		if runErr != nil {
			sink.Failed(runErr)
			return
		}
		sink.Done(res)
	}()

	for {
		if ctx.Err() != nil || m.stopped() {
			m.setState(StateClosed)
			if runErr != nil {
				return runErr
			}
			runErr = ctx.Err()
			return ctx.Err()
		}

		attempt := int(m.attempts.Add(1))
		if attempt > 1 && m.cfg.OnReconnect != nil {
			m.cfg.OnReconnect(attempt, m.LastEventID())
		}
		m.setState(StateConnecting)

		err := m.connectOnce(ctx, target)
		if err == nil {
			m.setState(StateClosed)
			runErr = nil
			return nil
		}
		runErr = err
		if !retryable(err) || ctx.Err() != nil || m.stopped() {
			m.setState(StateClosed)
			return err
		}
		// Retryable (idle, EOF, dropped connection). The resume token is
		// deliberately NOT cleared.
		if !sleepCtx(ctx, backoff(m.cfg, attempt)) {
			m.setState(StateClosed)
			runErr = ctx.Err()
			return ctx.Err()
		}
	}
}

// dispatch hands coalesced frame chunks to the Sink, serially.
func (m *sseManager) dispatch(done chan<- struct{}) {
	defer close(done)
	for c := range m.chunks {
		m.bytesDelivered.Add(int64(c.N))
		m.tee(c.Bytes())
		m.sink.Chunk(c)
	}
}

// connectOnce performs one connection attempt: dial, then pump to EOF/error.
func (m *sseManager) connectOnce(ctx context.Context, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("engine: sse: build request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	for k, vs := range m.cfg.Header {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	// Replay the resume token: the whole point of tracking event ids.
	if id := m.LastEventID(); id != "" {
		req.Header.Set("Last-Event-ID", id)
	}

	var report *TelemetryReport
	trace := &httptrace.ClientTrace{
		GotConn: func(gci httptrace.GotConnInfo) {
			// The live connection is the only handle that can carry a read
			// deadline for a streaming body.
			if gci.Conn != nil {
				m.conn.Store(gci.Conn)
			}
		},
	}
	if m.cfg.Telemetry != nil {
		report = m.cfg.Telemetry.begin(trace)
	}

	resp, err := m.client.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
	if report != nil {
		report.ended()
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Terminal: a rejected request will be rejected again, and retrying
		// would hammer an endpoint that already said no.
		return fmt.Errorf("%w: %d", ErrUnexpectedStatus, resp.StatusCode)
	}

	parser := newSSEParser()
	m.parser = parser
	m.setState(StateStreaming)
	m.renewDeadline()

	err = m.pump(ctx, resp.Body, parser)
	if err != nil && errors.Is(err, io.EOF) {
		// A clean EOF is retryable: the server may continue later.
		return io.EOF
	}
	return err
}

// renewDeadline sets the read deadline on the live connection. Per-read
// renewal plus heartbeat renewal is what stops a silent server from wedging the
// stream (§2.1.2). Deadline setting never blocks.
func (m *sseManager) renewDeadline() {
	c, _ := m.conn.Load().(net.Conn)
	if c == nil {
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(m.cfg.Idle))
}

// setDeadlineForwards implements Stream.SetDeadline.
func (m *sseManager) setDeadlineForwards(t time.Time) error {
	c, _ := m.conn.Load().(net.Conn)
	if c == nil {
		return nil // no connection yet; the per-read renewal still applies
	}
	return c.SetReadDeadline(t)
}

// pump reads the body, parses events, and coalesces them into frame chunks.
func (m *sseManager) pump(ctx context.Context, body io.Reader, parser *sseParser) error {
	scratch := make([]byte, chunkSize)
	frame := make([]byte, 0, chunkSize)
	ticker := time.NewTicker(m.cfg.FrameInterval)
	defer ticker.Stop()

	emit := func(final bool) error {
		if len(frame) == 0 && !final {
			return nil
		}
		c := acquireChunk()
		c.N = copy((*c.Buf)[:chunkSize], frame)
		c.Final = final
		select {
		case m.chunks <- c:
			frame = frame[:0]
			return nil
		case <-ctx.Done():
			c.Recycle()
			return ctx.Err()
		case <-m.stop:
			c.Recycle()
			return nil
		}
	}

	for {
		// Frame boundary: dispatch whatever accumulated, so the UI repaints at
		// most once per frame regardless of event rate (§2.1.5).
		select {
		case <-ticker.C:
			if err := emit(false); err != nil {
				return err
			}
			continue
		case <-ctx.Done():
			_ = emit(false)
			return ctx.Err()
		case <-m.stop:
			_ = emit(false)
			return io.EOF
		default:
		}

		n, err := body.Read(scratch)
		if n > 0 {
			// Renew before consuming: a read that blocks past the deadline is
			// the idle case, and renewal is what keeps a healthy stream alive.
			m.renewDeadline()
			for ev := range parser.Feed(scratch[:n]) {
				frame = appendEvent(frame, ev)
				if ev.ID != "" {
					m.setID(ev.ID)
				}
			}
			if parser.Heartbeats() > 0 {
				// Heartbeat: alive, no event.
				m.renewDeadline()
			}
			if perr := parser.Err(); perr != nil {
				_ = emit(false)
				return perr
			}
		}
		if err != nil {
			for ev := range parser.Flush() {
				frame = appendEvent(frame, ev)
				if ev.ID != "" {
					m.setID(ev.ID)
				}
			}
			_ = emit(false)
			// A read that timed out on our own deadline is the idle case, and
			// must be reported as such: ErrIdle is the documented, retryable
			// outcome of silence (ROADMAP.md §2.1.2). Without this mapping a
			// silent server would surface as an opaque transport error and be
			// retried for the wrong reason.
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return ErrIdle
			}
			return err
		}
	}
}

// appendEvent re-serializes an event into the frame buffer in wire shape, so a
// coalesced frame is faithful to what the server sent.
func appendEvent(dst []byte, ev Event) []byte {
	if ev.ID != "" {
		dst = append(dst, "id: "...)
		dst = append(dst, ev.ID...)
		dst = append(dst, '\n')
	}
	if ev.Name != "" {
		dst = append(dst, "event: "...)
		dst = append(dst, ev.Name...)
		dst = append(dst, '\n')
	}
	dst = append(dst, "data: "...)
	dst = append(dst, ev.Data...)
	dst = append(dst, '\n', '\n')
	return dst
}

// retryable reports whether an error should trigger a reconnect.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrIdle):
		return true // silence is retryable: reconnect and replay
	case errors.Is(err, ErrOversizeLine), errors.Is(err, ErrOversizeEvent):
		return false // broken framing: no reconnect can fix it
	case errors.Is(err, ErrUnexpectedStatus):
		return false // the endpoint already refused; retrying would hammer it
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return false
	case errors.Is(err, ErrWriteUnsupported):
		return false
	default:
		// A dropped connection surfaces as a transport error: retryable.
		return true
	}
}

// backoff returns a full-jitter delay for the given attempt (§2.1.2).
func backoff(cfg SSEConfig, attempt int) time.Duration {
	d := cfg.MinBackoff
	for i := 1; i < attempt && d < cfg.MaxBackoff; i++ {
		d *= 2
	}
	if d > cfg.MaxBackoff {
		d = cfg.MaxBackoff
	}
	return time.Duration(rand.Int64N(int64(d) + 1))
}

// sleepCtx sleeps unless the context or stop fires first; it reports whether
// the full sleep elapsed.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// sseStream adapts a manager to the unified Stream interface.
type sseStream struct {
	mgr  *sseManager
	once sync.Once
}

// OpenSSE opens a persistent SSE stream (ROADMAP.md §2.1.2). The returned
// Stream reads COALESCED FRAMES; per-event granularity is the Sink's business.
func OpenSSE(ctx context.Context, target string, sink Sink, cfg SSEConfig) (Stream, error) {
	if _, err := url.Parse(target); err != nil {
		return nil, fmt.Errorf("engine: sse: parse %q: %w", target, err)
	}
	m := NewSSEStreamManager(cfg)
	s := &sseStream{mgr: m.(*sseManager)}
	go func() { _ = s.mgr.Run(ctx, target, sink) }()
	return s, nil
}

// Read implements Stream: next coalesced frame.
func (s *sseStream) Read() (*Chunk, error) {
	select {
	case c, ok := <-s.mgr.chunks:
		if !ok {
			return nil, io.EOF
		}
		return c, nil
	case <-s.mgr.done:
		select {
		case c := <-s.mgr.chunks:
			return c, nil
		default:
			return nil, io.EOF
		}
	}
}

// Write always fails: SSE is read-only (ROADMAP.md §2.1.1).
func (s *sseStream) Write(p []byte) error { return ErrWriteUnsupported }

// Close implements Stream.
func (s *sseStream) Close() error {
	s.once.Do(func() { _ = s.mgr.Close() })
	return nil
}

// SetDeadline implements Stream.
func (s *sseStream) SetDeadline(t time.Time) error { return s.mgr.setDeadlineForwards(t) }

// Kind implements Stream.
func (s *sseStream) Kind() StreamKind { return KindSSE }

var _ Stream = (*sseStream)(nil)
var _ StreamManager = (*sseManager)(nil)
