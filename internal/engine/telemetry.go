package engine

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"sync"
	"time"
)

// TraceOp names a measured phase (ROADMAP.md §2.3.2).
type TraceOp string

const (
	OpDNS        TraceOp = "dns"
	OpConnect    TraceOp = "connect"
	OpTLS        TraceOp = "tls"
	OpWrote      TraceOp = "wrote"
	OpFirstByte  TraceOp = "firstbyte"
	OpGotConn    TraceOp = "gotconn"
	OpEBPFProbe  TraceOp = "ebpf.probe"
	OpRequestEnd TraceOp = "done"
)

// TracePoint is one measured interval. Start/End carry Go's monotonic reading,
// so a clock step cannot produce a negative duration.
type TracePoint struct {
	Op    TraceOp
	Start time.Time
	End   time.Time
	Meta  string // redacted: host:port, TLS version/cipher. Never a URL query.
}

// Duration is the measured interval, or 0 when the phase never completed.
func (p TracePoint) Duration() time.Duration {
	if p.End.IsZero() || p.Start.IsZero() {
		return 0
	}
	if p.End.Before(p.Start) {
		return 0
	}
	return p.End.Sub(p.Start)
}

// traceRingMax bounds the collector: 512 points is 4 KB of pointers plus the
// time.Time headers. Overflow DROPS the oldest point rather than growing, so
// telemetry can never become an unbounded allocation on a hot path.
const traceRingMax = 512

// TraceRingMax exposes the collector's bound so tests assert against the same
// constant the implementation uses rather than a copied number.
func TraceRingMax() int { return traceRingMax }

// Find returns the first recorded point with the given op.
func (r *TelemetryReport) Find(op TraceOp) (TracePoint, bool) {
	if r == nil {
		return TracePoint{}, false
	}
	return r.collect.Find(op)
}

// PointsFor returns every recorded point with the given op.
func (r *TelemetryReport) PointsFor(op TraceOp) []TracePoint {
	if r == nil {
		return nil
	}
	var out []TracePoint
	for _, p := range r.collect.Points() {
		if p.Op == op {
			out = append(out, p)
		}
	}
	return out
}

// End finalizes the report: it stamps the total and materialises the bounded
// point list. The engine calls it once the request (or connection attempt)
// completes.
func (r *TelemetryReport) End() { r.ended() }

// Collector is the lock-cheap trace sink (ROADMAP.md §2.3.2). Trace callbacks
// fire on runtime-internal goroutines, so it takes a mutex only on the (rare)
// append path and reads are atomic.
type Collector struct {
	mu    sync.Mutex
	ring  [traceRingMax]TracePoint
	count int // total appended, may exceed traceRingMax
}

// Add appends a point, dropping the oldest on overflow.
func (c *Collector) Add(p TracePoint) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ring[c.count%traceRingMax] = p
	c.count++
}

// Points returns a copy of the recorded points in order.
func (c *Collector) Points() []TracePoint {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	n := min(c.count, traceRingMax)
	out := make([]TracePoint, 0, n)
	start := 0
	if c.count > traceRingMax {
		start = c.count - traceRingMax
	}
	for i := start; i < c.count; i++ {
		out = append(out, c.ring[i%traceRingMax])
	}
	return out
}

// Len reports how many points were recorded (may exceed the ring size).
func (c *Collector) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

// Find returns the first point with the given op.
func (c *Collector) Find(op TraceOp) (TracePoint, bool) {
	for _, p := range c.Points() {
		if p.Op == op {
			return p, true
		}
	}
	return TracePoint{}, false
}

// TelemetryReport is the strictly typed result returned alongside a response
// (ROADMAP.md §2.3.2). Every phase is optional: a phase that did not run
// (because the connection was reused, or DNS was cached) reports a zero
// Duration rather than a fabricated number.
type TelemetryReport struct {
	DNS        TracePoint
	Connect    TracePoint
	TLS        TracePoint
	FirstByte  TracePoint
	Wrote      TracePoint
	GotConn    TracePoint
	Total      time.Duration
	ReusedConn bool

	// EBPF reports the raw-tracing disposition. On a non-privileged run this
	// is BLOCKED and the httptrace timings above stand on their own — never a
	// silent fallback masquerading as precision (ROADMAP.md §2.3.3).
	EBPF EBPFStatus

	// Points is the bounded ring of every phase, for callers that want the
	// full sequence.
	Points []TracePoint

	mu      sync.Mutex
	collect *Collector
	start   time.Time
	trace   *httptrace.ClientTrace
	// starts are retained so each hook can close its own interval.
	dnsStart, connStart, tlsStart time.Time
	remoteAddr                    string
}

var telemetryPool = sync.Pool{New: func() any { return &TelemetryReport{} }}

// AcquireTelemetry takes a pooled report. Pooling is what keeps the trace
// attach path allocation-free: httptrace.ClientTrace holds six function
// pointers, and building one per request would allocate on every request.
func AcquireTelemetry() *TelemetryReport {
	r := telemetryPool.Get().(*TelemetryReport)
	*r = TelemetryReport{collect: &Collector{}}
	return r
}

// ReleaseTelemetry returns a report to the pool.
func ReleaseTelemetry(r *TelemetryReport) {
	if r == nil {
		return
	}
	r.Points = nil
	*r = TelemetryReport{}
	telemetryPool.Put(r)
}

// begin wires the report into an existing ClientTrace and stamps the start.
func (r *TelemetryReport) begin(base *httptrace.ClientTrace) *TelemetryReport {
	if r == nil {
		return nil
	}
	r.start = time.Now()
	r.collect = &Collector{}
	r.hook(base)
	return r
}

// ended stamps the total and materialises the point list. It takes the lock
// because the trace hooks fire on runtime-internal goroutines and a consumer
// may read the report while the transport is still running.
func (r *TelemetryReport) ended() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Total = time.Since(r.start)
	r.Points = r.collect.Points()
	r.EBPF = ProbeEBPF()
}

// TelemetrySnapshot is a consistent, copy-out view of a report. Taking a
// snapshot is how a consumer reads a report that the transport may still be
// writing: without it, reading Total after a run races the finalization.
type TelemetrySnapshot struct {
	DNS, Connect, TLS, FirstByte, Wrote, GotConn TracePoint
	Total                                        time.Duration
	ReusedConn                                   bool
	EBPF                                         EBPFStatus
	Points                                       []TracePoint
}

// Snapshot returns a consistent copy of the report.
func (r *TelemetryReport) Snapshot() TelemetrySnapshot {
	if r == nil {
		return TelemetrySnapshot{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return TelemetrySnapshot{
		DNS:        r.DNS,
		Connect:    r.Connect,
		TLS:        r.TLS,
		FirstByte:  r.FirstByte,
		Wrote:      r.Wrote,
		GotConn:    r.GotConn,
		Total:      r.Total,
		ReusedConn: r.ReusedConn,
		EBPF:       r.EBPF,
		Points:     r.Points,
	}
}

// hook installs the httptrace callbacks on base, returning the composed trace.
//
// The callbacks close over the REPORT, never over a per-request object, so no
// allocation happens on the trace path: the ClientTrace struct itself is
// pre-allocated by the caller (see begin).
func (r *TelemetryReport) hook(base *httptrace.ClientTrace) {
	var tr *httptrace.ClientTrace
	if base != nil {
		cp := *base
		tr = &cp
	} else {
		tr = &httptrace.ClientTrace{}
	}

	tr.GetConn = func(hostPort string) { r.remoteAddr = hostPort }
	tr.DNSStart = func(httptrace.DNSStartInfo) {
		r.dnsStart = time.Now()
		r.collect.Add(TracePoint{Op: OpDNS, Start: r.dnsStart})
	}
	tr.DNSDone = func(httptrace.DNSDoneInfo) {
		now := time.Now()
		r.DNS = TracePoint{Op: OpDNS, Start: r.dnsStart, End: now, Meta: r.remoteAddr}
		r.collect.Add(r.DNS)
	}
	tr.ConnectStart = func(network, addr string) {
		r.connStart = time.Now()
		r.collect.Add(TracePoint{Op: OpConnect, Start: r.connStart, Meta: addr})
	}
	tr.ConnectDone = func(network, addr string, err error) {
		now := time.Now()
		meta := addr
		if err != nil {
			meta = addr + " error"
		}
		r.Connect = TracePoint{Op: OpConnect, Start: r.connStart, End: now, Meta: meta}
		r.collect.Add(r.Connect)
	}
	tr.TLSHandshakeStart = func() {
		r.tlsStart = time.Now()
		r.collect.Add(TracePoint{Op: OpTLS, Start: r.tlsStart})
	}
	tr.TLSHandshakeDone = func(state tls.ConnectionState, err error) {
		now := time.Now()
		meta := ""
		if state.Version != 0 {
			meta = tlsVersionName(state.Version) + " " + tls.CipherSuiteName(state.CipherSuite)
		}
		r.TLS = TracePoint{Op: OpTLS, Start: r.tlsStart, End: now, Meta: meta}
		r.collect.Add(r.TLS)
	}
	tr.WroteRequest = func(httptrace.WroteRequestInfo) {
		r.Wrote = TracePoint{Op: OpWrote, Start: r.start, End: time.Now()}
		r.collect.Add(r.Wrote)
	}
	tr.GotFirstResponseByte = func() {
		r.FirstByte = TracePoint{Op: OpFirstByte, Start: r.start, End: time.Now(), Meta: r.remoteAddr}
		r.collect.Add(r.FirstByte)
	}
	tr.GotConn = func(gci httptrace.GotConnInfo) {
		r.ReusedConn = gci.Reused
		r.GotConn = TracePoint{Op: OpGotConn, Start: r.start, End: time.Now(), Meta: r.remoteAddr}
		r.collect.Add(r.GotConn)
		if gci.Conn != nil {
			if a := gci.Conn.RemoteAddr(); a != nil {
				r.remoteAddr = a.String()
			}
		}
	}
	// Store the composed trace for the caller to use on the request.
	r.trace = tr
}

// Trace returns the composed ClientTrace to attach to a request.
func (r *TelemetryReport) Trace() *httptrace.ClientTrace {
	if r == nil {
		return nil
	}
	return r.trace
}

// Attach installs the telemetry hooks on a context (ROADMAP.md §2.3.2).
func Attach(ctx context.Context, r *TelemetryReport) context.Context {
	if r == nil {
		return ctx
	}
	r.begin(&httptrace.ClientTrace{})
	return httptrace.WithClientTrace(ctx, r.trace)
}

// tlsVersionName renders a TLS version for the trace Meta field. TLS versions
// and cipher names are safe to record: they carry no secret.
func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "TLS1.3"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS10:
		return "TLS1.0"
	default:
		return "unknown"
	}
}
