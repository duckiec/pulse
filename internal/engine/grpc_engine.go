package engine

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"pulse/internal/ast"
)

// systemTLS returns the platform's TLS credentials for grpcs:// targets. The
// host trust store is used, so certificate verification is the OS decision
// rather than one Pulse reinvents — and a TLS target never silently downgrades
// to plaintext.
func systemTLS() credentials.TransportCredentials {
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
}

// gRPC scheme prefixes accepted in a .http target. They are deliberately not
// http/https: a gRPC method is addressed as /package.Service/Method and speaks
// HTTP/2 framing, so it must never be mistaken for a REST endpoint by either
// engine.
const (
	schemeGRPC  = "grpc://"
	schemeGRPCS = "grpcs://"
)

// IsGRPCTarget reports whether a raw request target addresses a gRPC method.
func IsGRPCTarget(rawURL string) bool {
	return strings.HasPrefix(rawURL, schemeGRPC) || strings.HasPrefix(rawURL, schemeGRPCS)
}

// ErrUnsupportedMethod marks a method shape Pulse deliberately refuses rather
// than half-implements (ROADMAP.md §2.2.1: client-streaming and bidi return a
// clear error; no silent degradation).
var ErrUnsupportedMethod = errors.New("engine: grpc: method shape is not supported")

// ErrNoDescriptors means neither reflection nor a local descriptor set could
// describe the target method.
var ErrNoDescriptors = errors.New("engine: grpc: no schema source available for the target method")

// GRPCConfig configures the gRPC executor.
type GRPCConfig struct {
	// Resolver backs `{{var}}` interpolation, exactly as for REST.
	Resolver Resolver
	// Redactor scrubs secrets from everything surfaced to a Sink.
	Redactor Redactor
	// JSON controls the dynamic mapping layer. It is a POINTER so that "unset"
	// and "permissive" are distinguishable: JSONOptions{} with
	// StrictUnknown=false is a legitimate configuration, and collapsing it into
	// the zero value would silently turn a user's request for leniency into
	// strictness. Nil means "use DefaultJSONOptions".
	JSON *JSONOptions
	// DescriptorCacheEntries bounds the runtime descriptor cache.
	DescriptorCacheEntries int
	// LocalDescriptors is the offline schema source. When set, a target whose
	// server refuses reflection is resolved from these descriptors instead —
	// with no network fetch (Pulse is offline-first; a missing schema is a
	// local problem, not a reason to go looking on the internet).
	LocalDescriptors *descriptorpb.FileDescriptorSet
	// DialOptions are appended to the default credentials.
	DialOptions []grpc.DialOption
	// MaxMessageBytes bounds a single decoded protobuf message. A server that
	// streams an unbounded message must not be able to drive Pulse's heap.
	MaxMessageBytes int
	// UserAgent identifies Pulse to the server.
	UserAgent string
	// TeardownBudget bounds post-cancellation cleanup.
	TeardownBudget time.Duration
	// Telemetry, when non-nil, records httptrace-equivalent transport
	// timings for each RPC. gRPC's own metrics are not exposed per-call without
	// interceptors, so the same disposition policy applies.
	Telemetry *TelemetryReport
}

const (
	defaultMaxMessageBytes = 4 << 20 // 4 MiB
	defaultGRPCDialTimeout = 10 * time.Second
)

// GRPCEngine executes gRPC methods described by schemas fetched at runtime.
//
// It satisfies the same ProtocolEngine contract as the REST executor: it takes
// the same AST node and delivers to the same Sink. Nothing above this type can
// tell the two apart, which is the whole point of the abstraction — the UI and
// MCP layers speak one interface, and the wire is a property of the target.
type GRPCEngine struct {
	cfg   GRPCConfig
	doc   *ast.Document
	json  JSONOptions
	cache *DescriptorCache

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn // keyed by dial target

	closeOnce sync.Once
	closed    chan struct{}
}

// NewGRPCEngine binds a gRPC executor to a parsed document.
func NewGRPCEngine(doc *ast.Document, cfg GRPCConfig) *GRPCEngine {
	if cfg.JSON == nil {
		d := DefaultJSONOptions()
		cfg.JSON = &d
	}
	if cfg.MaxMessageBytes <= 0 {
		cfg.MaxMessageBytes = defaultMaxMessageBytes
	}
	if cfg.TeardownBudget <= 0 {
		cfg.TeardownBudget = teardownBudgetDefault
	}
	return &GRPCEngine{
		cfg:    cfg,
		doc:    doc,
		json:   *cfg.JSON,
		conns:  make(map[string]*grpc.ClientConn),
		closed: make(chan struct{}),
	}
}

// Protocol implements ProtocolEngine.
func (g *GRPCEngine) Protocol() ProtocolKind { return ProtoGRPC }

// Close releases every pooled connection. It is idempotent.
func (g *GRPCEngine) Close() error {
	var firstErr error
	g.closeOnce.Do(func() {
		close(g.closed)
		g.mu.Lock()
		conns := g.conns
		g.conns = nil
		g.mu.Unlock()
		for _, c := range conns {
			if err := c.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	})
	return firstErr
}

// GRPCOption is a functional option for NewGRPCEngine.
type GRPCOption func(*GRPCConfig)

// WithGRPCResolver binds the interpolation resolver.
func WithGRPCResolver(r Resolver) GRPCOption {
	return func(c *GRPCConfig) { c.Resolver = r }
}

// WithGRPCRedactor installs the redaction chokepoint.
func WithGRPCRedactor(r Redactor) GRPCOption {
	return func(c *GRPCConfig) { c.Redactor = r }
}

// WithGRPCLocalDescriptors supplies the offline schema source.
func WithGRPCLocalDescriptors(set *descriptorpb.FileDescriptorSet) GRPCOption {
	return func(c *GRPCConfig) { c.LocalDescriptors = set }
}

// WithGRPCJSON sets the dynamic mapping options.
func WithGRPCJSON(o JSONOptions) GRPCOption {
	return func(c *GRPCConfig) { c.JSON = &o }
}

// WithGRPCTelemetry attaches a telemetry report.
func WithGRPCTelemetry(t *TelemetryReport) GRPCOption {
	return func(c *GRPCConfig) { c.Telemetry = t }
}

// gRPCTarget is a parsed .http target: where to dial, and what to call.
type gRPCTarget struct {
	dialTarget string
	tls        bool
	symbol     string
	authority  string
}

// parseGRPCTarget splits "grpc://host:port/pkg.Service/Method".
func parseGRPCTarget(raw string) (gRPCTarget, error) {
	var t gRPCTarget
	switch {
	case strings.HasPrefix(raw, schemeGRPCS):
		t.tls = true
		raw = strings.TrimPrefix(raw, schemeGRPCS)
	case strings.HasPrefix(raw, schemeGRPC):
		raw = strings.TrimPrefix(raw, schemeGRPC)
	default:
		return t, fmt.Errorf("engine: grpc: target %q does not use the grpc:// or grpcs:// scheme", raw)
	}
	slash := strings.IndexByte(raw, '/')
	if slash < 0 {
		return t, fmt.Errorf("engine: grpc: target %q names no method (want grpc://host:port/package.Service/Method)", raw)
	}
	authority, symbol := raw[:slash], raw[slash+1:]
	if authority == "" {
		return t, fmt.Errorf("engine: grpc: target %q has no host", raw)
	}
	if _, err := ParseSymbol("/" + symbol); err != nil {
		return t, err
	}
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		// A bare host is legal; grpc defaults the port.
		host, port = authority, ""
	}
	dial := host
	if port != "" {
		dial = net.JoinHostPort(host, port)
	}
	t.dialTarget = dial
	t.symbol = "/" + symbol
	t.authority = authority
	return t, nil
}

// Run implements ProtocolEngine. It returns a handle that cancels the run; every
// outcome, including setup failure, reaches the Sink exactly once.
func (g *GRPCEngine) Run(ctx context.Context, ar *ast.Request, sink Sink) RunHandle {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.execute(runCtx, ar, sink)
	}()
	return RunHandle{cancel: cancel, done: done}
}

// execute performs one method call and delivers the result to the Sink.
func (g *GRPCEngine) execute(ctx context.Context, ar *ast.Request, sink Sink) {
	started := time.Now()

	rawTarget := g.doc.Str(ar.URLStart, ar.URLEnd)
	target, err := parseGRPCTarget(rawTarget)
	if err != nil {
		g.fail(sink, err)
		return
	}

	// Resolve the schema: reflection first, local descriptors second. A server
	// that refuses reflection is not a dead end as long as the user supplied a
	// descriptor; it is only an error when neither source can answer.
	md, err := g.resolve(ctx, target)
	if err != nil {
		g.fail(sink, err)
		return
	}

	if md.IsStreamingClient() {
		g.fail(sink, fmt.Errorf("%w: %s is client-streaming; v1 of the gRPC executor supports unary and server-streaming methods only",
			ErrUnsupportedMethod, target.symbol))
		return
	}

	body, err := g.requestBody(ar, md)
	if err != nil {
		g.fail(sink, err)
		return
	}

	mdConn, err := g.connect(ctx, target)
	if err != nil {
		g.fail(sink, err)
		return
	}

	md2 := metadata.New(nil)
	for _, h := range ar.Headers {
		name := strings.ToLower(g.doc.Str(h.NameStart, h.NameEnd))
		if name == "" {
			g.fail(sink, fmt.Errorf("engine: grpc: line %d: metadata entry with no name", h.Line))
			return
		}
		val, err := g.expand(g.doc.Str(h.ValStart, h.ValEnd), h.Line)
		if err != nil {
			g.fail(sink, err)
			return
		}
		// The value on the wire is the REAL resolved secret, matching the REST
		// path: redaction applies to diagnostic copies, never to the outbound
		// request (AGENTS.md §3.4).
		md2.Append(name, val)
	}

	callCtx := metadata.NewOutgoingContext(ctx, md2)
	if g.cfg.UserAgent != "" {
		callCtx = metadata.AppendToOutgoingContext(callCtx, "user-agent", g.cfg.UserAgent)
	}
	if g.cfg.Telemetry != nil {
		callCtx = Attach(callCtx, g.cfg.Telemetry)
	}

	if md.IsStreamingServer() {
		g.runServerStreaming(callCtx, mdConn, target, md, body, sink, started)
		return
	}
	g.runUnary(callCtx, mdConn, target, md, body, sink, started)
}

// requestBody extracts and decodes the JSON payload from the AST.
func (g *GRPCEngine) requestBody(ar *ast.Request, md protoreflect.MethodDescriptor) (*DynamicMessage, error) {
	if ar.Body.Kind == ast.BodyNone {
		return DecodeRequest(md, nil, g.json)
	}
	if ar.Body.Kind == ast.BodyXML {
		return nil, fmt.Errorf("engine: grpc: line %d: XML request bodies cannot be mapped to protobuf; "+
			"a gRPC method takes a JSON body", ar.Body.Line)
	}
	raw, err := g.expandBytes(g.doc.BodyText(ar.Body), ar.Body.Line)
	if err != nil {
		return nil, err
	}
	return DecodeRequest(md, raw, g.json)
}

// resolve finds the method descriptor, preferring runtime reflection and falling
// back to locally supplied descriptors.
func (g *GRPCEngine) resolve(ctx context.Context, target gRPCTarget) (protoreflect.MethodDescriptor, error) {
	cc, err := g.connect(ctx, target)
	if err != nil {
		return nil, err
	}
	md, err := g.cacheFor(cc).Resolve(ctx, target.dialTarget, target.symbol)
	if err == nil {
		return md, nil
	}
	if g.cfg.LocalDescriptors != nil {
		local, lerr := MethodFromSet(g.cfg.LocalDescriptors, target.symbol)
		if lerr == nil {
			return local, nil
		}
		return nil, fmt.Errorf("engine: grpc: %s: reflection failed (%w) and local descriptors could not resolve it (%w)",
			target.symbol, err, lerr)
	}
	if errors.Is(err, ErrReflectionUnavailable) {
		return nil, fmt.Errorf("%w: %s refused reflection and no local descriptor set was supplied",
			ErrNoDescriptors, target.symbol)
	}
	return nil, err
}

func (g *GRPCEngine) cacheFor(cc grpc.ClientConnInterface) *DescriptorCache {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cache == nil {
		g.cache = NewDescriptorCache(cc, g.cfg.DescriptorCacheEntries)
	}
	return g.cache
}

// connect returns (and pools) a connection per dial target.
func (g *GRPCEngine) connect(ctx context.Context, target gRPCTarget) (*grpc.ClientConn, error) {
	select {
	case <-g.closed:
		return nil, fmt.Errorf("engine: grpc: engine is closed")
	default:
	}

	g.mu.Lock()
	if c, ok := g.conns[target.dialTarget]; ok {
		g.mu.Unlock()
		return c, nil
	}
	g.mu.Unlock()

	opts := []grpc.DialOption{grpc.WithUserAgent(g.cfg.UserAgent)}
	if target.tls {
		opts = append(opts, grpc.WithTransportCredentials(systemTLS()))
	} else {
		// An insecure dial is a deliberate, visible choice for plaintext local
		// servers; TLS is the default the moment the target says grpcs://.
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	opts = append(opts, g.cfg.DialOptions...)

	// grpc.NewClient performs no I/O; the first RPC establishes the
	// connection, so there is no dial context to bound here. Cancellation of
	// ctx still aborts the call itself.
	cc, err := grpc.NewClient(target.dialTarget, opts...)
	if err != nil {
		return nil, fmt.Errorf("engine: grpc: dial %s: %w", target.dialTarget, err)
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if existing, ok := g.conns[target.dialTarget]; ok {
		// Lost a race with another goroutine; keep the winner and drop ours so
		// the loser does not leak a connection.
		_ = cc.Close()
		return existing, nil
	}
	if g.conns == nil {
		_ = cc.Close()
		return nil, fmt.Errorf("engine: grpc: engine is closed")
	}
	g.conns[target.dialTarget] = cc
	return cc, nil
}

// runUnary performs a unary call and delivers its single response message.
func (g *GRPCEngine) runUnary(ctx context.Context, cc *grpc.ClientConn, target gRPCTarget,
	md protoreflect.MethodDescriptor, body *DynamicMessage, sink Sink, started time.Time) {

	in := body.Message()
	out := NewDynamicMessage(md.Output())

	err := cc.Invoke(ctx, target.symbol, in, out.Message())
	if err != nil {
		g.fail(sink, g.rpcError(target, err))
		return
	}

	res := g.newResult(target, started, md, out)
	n, derr := g.deliver(sink, res, out, true)
	if derr != nil {
		return // the failure already reached the sink
	}
	res.Bytes = int64(n)
	res.Status = http.StatusOK
	res.StatusText = "OK"
	res.Messages = 1
	sink.Done(res)
}

// runServerStreaming performs a server-streaming call, delivering each message
// through the SAME pooled-chunk path as REST, so a consumer cannot tell the
// transports apart (ROADMAP.md §2.1.1/§2.2.1).
func (g *GRPCEngine) runServerStreaming(ctx context.Context, cc *grpc.ClientConn, target gRPCTarget,
	md protoreflect.MethodDescriptor, body *DynamicMessage, sink Sink, started time.Time) {

	desc := &grpc.StreamDesc{StreamName: string(md.Name()), ServerStreams: true}
	stream, err := cc.NewStream(ctx, desc, target.symbol)
	if err != nil {
		g.fail(sink, g.rpcError(target, err))
		return
	}
	if err := stream.SendMsg(body.Proto()); err != nil {
		g.fail(sink, g.rpcError(target, err))
		return
	}
	if err := stream.CloseSend(); err != nil {
		g.fail(sink, g.rpcError(target, err))
		return
	}

	res := g.newResult(target, started, md, nil)
	total := int64(0)
	messages := 0

	for {
		msg := NewDynamicMessage(md.Output())
		rerr := stream.RecvMsg(msg.Proto())
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break // a clean end of stream
			}
			g.fail(sink, g.rpcError(target, rerr))
			return
		}
		messages++
		n, derr := g.deliver(sink, res, msg, false)
		if derr != nil {
			return // the consumer went away; the failure already reached it
		}
		total += int64(n)
	}
	if g.cfg.Telemetry != nil {
		g.cfg.Telemetry.End()
	}
	res.Bytes = total
	res.Status = http.StatusOK
	res.StatusText = "OK"
	res.Messages = messages
	sink.Done(res)
}

// deliver renders one message to JSON and pushes it through the pooled chunk
// channel, exactly as the REST path does. The response direction therefore
// inherits the 32 KiB chunk geometry and the bounded in-flight ceiling from
// ADL-0016 instead of introducing a second buffering regime.
//
// The encoding is STREAMED, not buffered-then-chopped: a chunk is handed to the
// sink as soon as the scratch buffer fills, so the working set stays at one
// chunk no matter how large the message is. Marshalling first and chunking
// afterwards would measure nothing — the ceiling would hold for the channel
// while the real allocation happened just before it, unmeasured.
func (g *GRPCEngine) deliver(sink Sink, res *RunResult, msg *DynamicMessage, final bool) (int, error) {
	emitter := &chunkEmitter{sink: sink, onError: func(err error) { g.fail(sink, err) }}
	enc := newJSONStream(emitter, g.json)
	if err := enc.Emit(msg.Proto().ProtoReflect()); err != nil {
		if res == nil || emitter.n > 0 {
			// Some bytes already reached the consumer; the failure is still
			// terminal and is reported through the Sink.
			g.fail(sink, err)
		}
		return emitter.n, err
	}
	// A final message gets a zero-length terminator so the consumer sees an
	// explicit end-of-body, matching the REST path's Final chunk.
	if final {
		c := acquireChunk()
		c.N = 0
		c.Final = true
		sink.Chunk(c)
	}
	return emitter.n, nil
}

// chunkEmitter turns the encoder's byte stream into pooled 32 KiB chunks.
type chunkEmitter struct {
	sink    Sink
	n       int
	onError func(error)
}

// Write is called by the encoder whenever its scratch buffer fills.
//
// It accepts a write of ANY size and splits it into 32 KiB chunks. The encoder
// appends whole strings before checking the buffer, so a single Write can carry
// a 300 KB value; copying only the first chunk of that would silently truncate
// the JSON document, which is exactly the class of bug this direction is most
// exposed to.
func (e *chunkEmitter) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += chunkSize {
		end := min(off+chunkSize, len(p))
		c := acquireChunk()
		c.N = copy((*c.Buf)[:chunkSize], p[off:end])
		e.n += c.N
		e.sink.Chunk(c) // the sink owns (and recycles) the chunk
	}
	return len(p), nil
}

// interpolate helpers live in engine.go and are shared with the REST executor:
// resolution semantics must not drift between wires.

func (g *GRPCEngine) newResult(target gRPCTarget, started time.Time, md protoreflect.MethodDescriptor, _ *DynamicMessage) *RunResult {
	return &RunResult{
		Method:  "GRPC",
		URL:     target.symbol,
		Proto:   "grpc",
		Header:  http.Header{},
		Started: started,
		Symbol:  target.symbol,
		Service: target.authority,
	}
}

// rpcError converts a gRPC status into a clean engine error, preserving the code
// so the UI can distinguish "not found" from "unimplemented".
func (g *GRPCEngine) rpcError(target gRPCTarget, err error) error {
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	if st.Code() == codes.Unavailable || st.Code() == codes.Canceled {
		return fmt.Errorf("engine: grpc: %s: %s: %w", target.symbol, st.Code(), err)
	}
	return &RPCStatusError{
		Symbol:  target.symbol,
		Code:    st.Code(),
		Message: st.Message(),
		HTTP:    grpcHTTPStatus(st.Code()),
	}
}

// RPCStatusError is a non-OK gRPC status, reported with both the gRPC code and
// the HTTP-equivalent status Pulse maps it to (ROADMAP.md §2.2.1: a structured
// *GRPCStatus{Code, Message}).
type RPCStatusError struct {
	Symbol  string
	Code    codes.Code
	Message string
	// HTTP is the numeric status the store and UI record. The Proto field of
	// the RunResult is "grpc", so the mapping is never mistaken for the
	// server's own status line.
	HTTP int
}

// Error implements error.
func (e *RPCStatusError) Error() string {
	return fmt.Sprintf("engine: grpc: %s: %s (%s): %s", e.Symbol, e.Code, e.Code.String(), e.Message)
}

// grpcHTTPStatus maps a gRPC code to the numeric status Pulse records. The
// mapping is a display/storage convenience only; the authoritative outcome is
// always the gRPC code carried alongside it.
func grpcHTTPStatus(c codes.Code) int {
	switch c {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return 499 // client closed request (nginx convention; not in net/http)
	case codes.InvalidArgument, codes.OutOfRange, codes.FailedPrecondition:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	default: // Internal, DataLoss, Unknown
		return http.StatusInternalServerError
	}
}

// fail scrubs and reports a terminal error exactly once.
func (g *GRPCEngine) fail(sink Sink, err error) {
	if g.cfg.Redactor != nil {
		err = g.cfg.Redactor.Error(err)
	}
	sink.Failed(err)
}

// expand resolves `{{name}}` in a metadata value.
func (g *GRPCEngine) expand(src string, line int32) (string, error) {
	return interpolate(src, g.cfg.Resolver, line)
}

// expandBytes resolves `{{name}}` in a body.
func (g *GRPCEngine) expandBytes(src []byte, line int32) ([]byte, error) {
	if !bytes.Contains(src, []byte("{{")) {
		return src, nil
	}
	s, err := interpolate(string(src), g.cfg.Resolver, line)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}
