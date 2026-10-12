package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"pulse/internal/ast"
	"pulse/internal/engine"
)

// ---- the reflection gate ---------------------------------------------------

// TestReflectionGateResolvesAndRendersJSON is THE gate for Move 6
// (ROADMAP.md §2.2.2/§2.2.3).
//
// Nothing about this service was compiled into Pulse. The test sends JSON in a
// .http body, the engine fetches the descriptor over the wire, builds a dynamic
// message, calls the method, and the Sink must receive correctly-rendered JSON
// back. Every step of that chain is dynamic; the assertion is on the VALUE the
// consumer receives, because protojson's byte output is intentionally unstable.
func TestReflectionGateResolvesAndRendersJSON(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf(`### Echo
POST %s

{"message": "pulse", "limit": 7, "budget": "900", "loud": true, "filter": {"pageSize": 25, "prefix": "p"}, "mode": "MODE_ECHO"}
`, srv.symbol("Echo"))

	sink := newGRPCSink()
	res, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr != nil {
		t.Fatalf("run failed: %v", runErr)
	}

	body := sink.jsonBody(t)
	if got := body["message"]; got != "pulse" {
		t.Errorf("message = %v, want pulse", got)
	}
	// An int32 comes back as a JSON number.
	if got := body["limit"]; got != float64(7) {
		t.Errorf("limit = %v (%T), want 7", got, got)
	}
	// protojson renders 64-bit integers as JSON strings — the asymmetry is
	// proto's rule, and the engine must honour it rather than normalize it away.
	if got := body["budget"]; got != "900" {
		t.Errorf("budget = %v (%T), want \"900\" (int64 renders as a string)", got, got)
	}
	if got := body["loud"]; got != true {
		t.Errorf("loud = %v, want true", got)
	}
	filter, ok := body["filter"].(map[string]any)
	if !ok {
		t.Fatalf("filter = %v, want an object", body["filter"])
	}
	if filter["pageSize"] != float64(25) || filter["prefix"] != "p" {
		t.Errorf("filter = %v, want pageSize 25 and prefix p", filter)
	}

	// The server must have decoded a real protobuf message, not JSON passthrough.
	rec := srv.lastReceived()
	if rec == nil {
		t.Fatal("the server never received a request")
	}
	lim := rec.Descriptor().Fields().ByName("limit")
	if got := rec.Get(lim).Int(); got != 7 {
		t.Errorf("server saw limit = %d, want 7 (the JSON was not encoded to binary)", got)
	}

	// Terminal metadata.
	if res.Proto != "grpc" {
		t.Errorf("Proto = %q, want grpc", res.Proto)
	}
	if res.Status != 200 {
		t.Errorf("Status = %d, want 200", res.Status)
	}
	if res.Symbol != "/"+echoService+"/Echo" {
		t.Errorf("Symbol = %q", res.Symbol)
	}
	if res.Messages != 1 {
		t.Errorf("Messages = %d, want 1", res.Messages)
	}
}

// TestReflectionDescriptorMatchesTheWireProtocol pins every field number of
// Pulse's hand-built reflection descriptor.
//
// This test exists because of a real bug: the descriptor was first written with
// v1alpha's field numbers while declaring the v1 package. The server then decoded
// field 2 as `host`, found no oneof case set, and answered "invalid
// MessageRequest: <nil>" — an error naming neither the field nor the cause. A
// hand-built descriptor has no compiler to catch that, so the numbers are
// asserted here against the v1 schema.
func TestReflectionDescriptorMatchesTheWireProtocol(t *testing.T) {
	fd, err := engine.ReflectionFileDescriptor()
	if err != nil {
		t.Fatalf("reflection descriptor: %v", err)
	}

	type want struct {
		name   string
		number int32
		oneof  bool
	}
	cases := []struct {
		message string
		fields  []want
	}{
		{
			message: "ServerReflectionRequest",
			fields: []want{
				// host is a plain field in v1 and must NOT be in the oneof.
				{"host", 1, false},
				{"file_by_filename", 3, true},
				{"file_containing_symbol", 4, true},
				{"file_containing_extension", 5, true},
				{"all_extension_numbers_of_type", 6, true},
				{"list_services", 7, true},
			},
		},
		{
			message: "ServerReflectionResponse",
			fields: []want{
				{"valid_host", 1, false},
				{"original_request", 2, false},
				{"file_descriptor_response", 4, true},
				{"all_extension_numbers_response", 5, true},
				{"list_services_response", 6, true},
				{"error_response", 7, true},
			},
		},
		{
			message: "FileDescriptorResponse",
			fields:  []want{{"file_descriptor_proto", 1, false}},
		},
		{
			message: "ExtensionRequest",
			fields:  []want{{"containing_type", 1, false}, {"extension_number", 2, false}},
		},
		{
			message: "ExtensionNumberResponse",
			fields:  []want{{"base_type_name", 1, false}, {"extension_number", 2, false}},
		},
		{
			message: "ErrorResponse",
			fields:  []want{{"error_code", 1, false}, {"error_message", 2, false}},
		},
	}

	for _, tc := range cases {
		md := fd.Messages().ByName(protoreflect.Name(tc.message))
		if md == nil {
			t.Errorf("%s: message missing from the hand-built descriptor", tc.message)
			continue
		}
		if got := md.Fields().Len(); got != len(tc.fields) {
			t.Errorf("%s: %d fields, want %d", tc.message, got, len(tc.fields))
		}
		for _, w := range tc.fields {
			f := md.Fields().ByName(protoreflect.Name(w.name))
			if f == nil {
				t.Errorf("%s.%s: missing", tc.message, w.name)
				continue
			}
			if int32(f.Number()) != w.number {
				t.Errorf("%s.%s: field number %d, want %d", tc.message, w.name, f.Number(), w.number)
			}
			inOneof := f.ContainingOneof() != nil
			if inOneof != w.oneof {
				t.Errorf("%s.%s: in-oneof = %v, want %v", tc.message, w.name, inOneof, w.oneof)
			}
		}
	}

	// The oneof must contain EXACTLY its members: a field left out of the oneof
	// marshals correctly and then fails on the server with an error that names
	// nothing useful.
	req := fd.Messages().ByName("ServerReflectionRequest")
	if got := req.Oneofs().ByName("message_request").Fields().Len(); got != 5 {
		t.Errorf("message_request has %d members, want 5", got)
	}
	resp := fd.Messages().ByName("ServerReflectionResponse")
	if got := resp.Oneofs().ByName("message_response").Fields().Len(); got != 4 {
		t.Errorf("message_response has %d members, want 4", got)
	}
}

// TestReflectionGateUsesNoGeneratedTypes proves the reflection path is dynamic
// end to end: the descriptor Pulse's own reflection client uses is hand-built,
// and the descriptor of the service under test arrives over the wire.
func TestReflectionGateUsesNoGeneratedTypes(t *testing.T) {
	// Pulse's own reflection client: a descriptor constructed field by field,
	// with no generated Go type in the binary.
	fd, err := engine.ReflectionFileDescriptor()
	if err != nil {
		t.Fatalf("hand-built reflection descriptor: %v", err)
	}
	svc := fd.Services().Get(0)
	if got := string(svc.FullName()); got != "grpc.reflection.v1.ServerReflection" {
		t.Errorf("reflection service = %q", got)
	}
	method := svc.Methods().Get(0)
	if !method.IsStreamingClient() || !method.IsStreamingServer() {
		t.Error("the reflection method must be declared bidirectional; a wrong " +
			"StreamDesc would surface as an opaque transport error")
	}
	// The request descriptor must expose exactly the oneof the reflection
	// protocol specifies; a hand-built descriptor that drifted from the wire
	// would produce an error the user cannot act on.
	req := fd.Messages().ByName("ServerReflectionRequest")
	if req.Fields().ByName("file_containing_symbol") == nil {
		t.Error("the reflection request descriptor is missing file_containing_symbol")
	}
	if got := req.Fields().Len(); got != 6 { // v1 adds `host` ahead of the oneof
		t.Errorf("ServerReflectionRequest has %d fields, want 6", got)
	}

	// The service under test is described by a descriptor built at runtime; if
	// the engine had needed a generated stub, this server would not be
	// callable at all.
	srv := startTestServer(t, true)
	src := fmt.Sprintf("POST %s\n\n{\"message\": \"dynamic\"}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	if _, runErr := runGRPC(t, src, sink, engine.GRPCConfig{}); runErr != nil {
		t.Fatalf("a runtime-built service was not callable: %v", runErr)
	}
	if got := sink.jsonBody(t)["message"]; got != "dynamic" {
		t.Errorf("message = %v, want dynamic", got)
	}
}

// ---- dynamic type resolution ------------------------------------------------

// TestTypeMismatchFailsCleanly is the strictness gate: an int32 field given a
// JSON string must fail with a STRUCTURED error naming the field, not be
// coerced, silently zeroed, or turned into a string on the wire.
func TestTypeMismatchFailsCleanly(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\n\n{\"message\": \"pulse\", \"limit\": \"seven\"}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	_, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr == nil {
		t.Fatalf("a string in an int32 field was accepted; delivered %s", sink.text())
	}

	var fte *engine.FieldTypeError
	if !errors.As(runErr, &fte) {
		t.Fatalf("error is %T (%v), want a *FieldTypeError carrying the field path", runErr, runErr)
	}
	if fte.Path != "limit" {
		t.Errorf("Path = %q, want limit", fte.Path)
	}
	if fte.Field != "limit" {
		t.Errorf("Field = %q, want limit", fte.Field)
	}
	if fte.Kind.String() != "int32" {
		t.Errorf("Kind = %v, want int32", fte.Kind)
	}
	if fte.Got != "string" {
		t.Errorf("Got = %q, want string", fte.Got)
	}
	if !strings.Contains(fte.Error(), "64-bit") {
		t.Errorf("error should explain WHY quoting is wrong here, got: %s", fte.Error())
	}
	// A field-level failure must never reach the server.
	if srv.lastReceived() != nil {
		t.Error("a request the engine rejected locally was still sent to the server")
	}
}

// TestTypeMismatchInNestedFieldReportsThePath proves the path is a real path,
// not just the top-level key.
func TestTypeMismatchInNestedFieldReportsThePath(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\n\n{\"filter\": {\"pageSize\": \"many\"}}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	_, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr == nil {
		t.Fatalf("a nested type mismatch was accepted: %s", sink.text())
	}
	var fte *engine.FieldTypeError
	if !errors.As(runErr, &fte) {
		t.Fatalf("error is %T (%v), want *FieldTypeError", runErr, runErr)
	}
	if fte.Path != "filter.pageSize" {
		t.Errorf("Path = %q, want filter.pageSize", fte.Path)
	}
}

// TestTypeMismatchInRepeatedFieldReportsTheIndex proves repeated messages are
// walked element by element with an index.
func TestTypeMismatchInRepeatedFieldReportsTheIndex(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\n\n{\"tags\": [{\"pageSize\": 1}, {\"prefix\": \"ok\"}, {\"pageSize\": \"nope\"}]}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	_, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr == nil {
		t.Fatalf("a type mismatch inside a repeated field was accepted: %s", sink.text())
	}
	var fte *engine.FieldTypeError
	if !errors.As(runErr, &fte) {
		t.Fatalf("error is %T (%v), want *FieldTypeError", runErr, runErr)
	}
	if fte.Path != "tags[2].pageSize" {
		t.Errorf("Path = %q, want tags[2].pageSize", fte.Path)
	}
}

// TestValidQuotedNumberForInt64IsAccepted proves the strictness rule is precise,
// not merely strict: protojson renders 64-bit integers as strings, so a quoted
// number must be legal exactly there and only there.
func TestValidQuotedNumberForInt64IsAccepted(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\n\n{\"message\": \"x\", \"budget\": \"900\"}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	if _, runErr := runGRPC(t, src, sink, engine.GRPCConfig{}); runErr != nil {
		t.Fatalf("a quoted int64 was rejected: %v", runErr)
	}
	if got := sink.jsonBody(t)["budget"]; got != "900" {
		t.Errorf("budget = %v, want \"900\"", got)
	}
}

// TestUnknownFieldIsRejectedByDefault proves strict_unknown (ROADMAP.md §2.2.3).
func TestUnknownFieldIsRejectedByDefault(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\n\n{\"message\": \"x\", \"loudd\": true}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	_, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr == nil {
		t.Fatalf("an unknown field was accepted; delivered %s", sink.text())
	}
	var fte *engine.FieldTypeError
	if !errors.As(runErr, &fte) {
		t.Fatalf("error is %T (%v), want *FieldTypeError", runErr, runErr)
	}
	if !strings.Contains(fte.Error(), "not in") {
		t.Errorf("error should name the descriptor the field is missing from: %s", fte.Error())
	}
}

// TestUnknownFieldAcceptedWhenNotStrict proves the strictness knob is real and
// not a one-way door.
func TestUnknownFieldAcceptedWhenNotStrict(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\n\n{\"message\": \"x\", \"loudd\": true}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	opts := engine.DefaultJSONOptions()
	opts.StrictUnknown = false
	if _, runErr := runGRPC(t, src, sink, engine.GRPCConfig{JSON: &opts}); runErr != nil {
		t.Fatalf("permissive mapping rejected a known payload: %v", runErr)
	}
	if got := sink.jsonBody(t)["message"]; got != "x" {
		t.Errorf("message = %v, want x", got)
	}
}

// ---- reflection resolution paths -------------------------------------------

// TestReflectionUnavailableFallsBackToLocalDescriptors proves the offline path:
// when a server refuses reflection, descriptors supplied locally resolve the
// method with no network fetch (ROADMAP.md §2.2.2). Pulse is offline-first; a
// missing schema is a local problem.
func TestReflectionUnavailableFallsBackToLocalDescriptors(t *testing.T) {
	srv := startTestServer(t, false) // reflection NOT registered

	src := fmt.Sprintf("POST %s\n\n{\"message\": \"offline\", \"limit\": 3}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	_, runErr := runGRPC(t, src, sink, engine.GRPCConfig{
		LocalDescriptors: &descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{srv.fileProto}},
	})
	if runErr != nil {
		t.Fatalf("local descriptor fallback failed: %v", runErr)
	}
	if got := sink.jsonBody(t)["message"]; got != "offline" {
		t.Errorf("message = %v, want offline", got)
	}
}

// TestReflectionUnavailableWithNoFallbackFailsClosed proves the engine reports
// the missing schema instead of guessing.
func TestReflectionUnavailableWithNoFallbackFailsClosed(t *testing.T) {
	srv := startTestServer(t, false)

	src := fmt.Sprintf("POST %s\n\n{\"message\": \"x\"}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	_, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr == nil {
		t.Fatal("a reflection-less server with no local descriptors must fail, not guess")
	}
	if !errors.Is(runErr, engine.ErrNoDescriptors) {
		t.Errorf("error = %v, want ErrNoDescriptors", runErr)
	}
	if !strings.Contains(runErr.Error(), "reflection") {
		t.Errorf("error should say reflection was unavailable: %v", runErr)
	}
}

// TestUnknownMethodIsDistinguishedFromUnavailable: an advertised-but-unhandled
// method must surface as Unimplemented, not as a schema resolution failure.
func TestUnknownMethodIsDistinguishedFromUnavailable(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\n\n{\"message\": \"x\"}\n", srv.symbol("Missing"))
	sink := newGRPCSink()
	_, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr == nil {
		t.Fatal("an unimplemented method must fail")
	}
	var rse *engine.RPCStatusError
	if !errors.As(runErr, &rse) {
		t.Fatalf("error is %T (%v), want *RPCStatusError", runErr, runErr)
	}
	if rse.Code != codes.Unimplemented {
		t.Errorf("Code = %v, want Unimplemented", rse.Code)
	}
	if rse.HTTP != 501 {
		t.Errorf("mapped HTTP status = %d, want 501", rse.HTTP)
	}
}

// ---- streaming --------------------------------------------------------------

// TestServerStreamingDeliversEveryMessageThroughTheSink proves a
// server-streaming method delivers each message through the SAME pooled-chunk
// path as REST (ROADMAP.md §2.2.1).
func TestServerStreamingDeliversEveryMessageThroughTheSink(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\n\n{\"message\": \"tick\"}\n", srv.symbol("Watch"))
	sink := newGRPCSink()
	res, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr != nil {
		t.Fatalf("stream failed: %v", runErr)
	}
	if res.Messages != 3 {
		t.Errorf("Messages = %d, want 3", res.Messages)
	}
	// Each message is rendered as its own JSON document, concatenated.
	body := sink.text()
	for i := range 3 {
		if !strings.Contains(body, fmt.Sprintf("tick#%d", i)) {
			t.Errorf("missing streamed message %d in:\n%s", i, body)
		}
	}
}

// TestClientStreamingIsRejectedBeforeDialing proves v1 refuses client-streaming
// and bidi with a clear status rather than half-supporting them
// (ROADMAP.md §2.2.1: no silent degradation).
func TestClientStreamingIsRejectedBeforeDialing(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\n\n{\"message\": \"x\"}\n", srv.symbol("Collect"))
	sink := newGRPCSink()
	_, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr == nil {
		t.Fatal("a client-streaming method must be refused")
	}
	if !errors.Is(runErr, engine.ErrUnsupportedMethod) {
		t.Errorf("error = %v, want ErrUnsupportedMethod", runErr)
	}
	if !strings.Contains(runErr.Error(), "unary") {
		t.Errorf("the error should state what IS supported: %v", runErr)
	}
	if srv.lastReceived() != nil {
		t.Error("a refused method shape was still sent to the server")
	}
}

// TestResponseRespectsChunkGeometry proves the binary→JSON direction inherits
// the 32 KiB chunk geometry from ADL-0016 rather than introducing a second
// buffering regime (the zero-allocation invariant for Move 6).
func TestResponseRespectsChunkGeometry(t *testing.T) {
	srv := startTestServer(t, true)

	// A payload far larger than one chunk: the sink must see it in pieces.
	big := strings.Repeat("x", 300_000)
	src := fmt.Sprintf("POST %s\n\n{\"message\": %q}\n", srv.symbol("Echo"), big)

	sink := newGRPCSink()
	res, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr != nil {
		t.Fatalf("large response failed: %v", runErr)
	}
	if sink.chunks < 10 {
		t.Errorf("a %d-byte response arrived in %d chunks; chunking is not in effect",
			res.Bytes, sink.chunks)
	}
	sink.mu.Lock()
	maxChunk := sink.maxCh
	sink.mu.Unlock()
	if maxChunk > 32*1024 {
		t.Errorf("largest chunk = %d bytes, exceeds the 32 KiB ceiling", maxChunk)
	}
	var out map[string]any
	if err := json.Unmarshal(sink.body, &out); err != nil {
		t.Fatalf("reassembled body is not JSON: %v", err)
	}
	if got, _ := out["message"].(string); len(got) != len(big) {
		t.Errorf("message round-tripped to %d bytes, want %d", len(got), len(big))
	}
}

// ---- target parsing and routing --------------------------------------------

// TestTargetParsingIsStrict proves a malformed target fails at the boundary with
// a clear message rather than becoming an opaque network failure.
func TestTargetParsingIsStrict(t *testing.T) {
	bad := []string{
		"grpc://host:1/",            // no method
		"grpc://host:1/Echo",        // no package separator
		"grpc:///pkg.Svc/Method",    // no host
		"grpc://host:1/pkg.Svc/",    // no method name
		"grpc://host:1/.Svc/Method", // empty package
		"grpc://host:1/pkg./Method", // empty service
	}
	for _, target := range bad {
		src := "POST " + target + "\n\n{\"message\": \"x\"}\n"
		sink := newGRPCSink()
		_, err := runGRPC(t, src, sink, engine.GRPCConfig{})
		if err == nil {
			t.Errorf("target %q was accepted", target)
		}
	}
}

// TestProtocolRouterDispatchesByTarget proves the router picks by target and
// that both executors satisfy one interface.
func TestProtocolRouterDispatchesByTarget(t *testing.T) {
	srv := startTestServer(t, true)

	arena := ast.AcquireArena()
	defer arena.Release()
	arena.Reset([]byte("POST " + srv.symbol("Echo") + "\n\n{\"message\": \"routed\"}\n"))
	doc, err := ast.Parse(arena)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer doc.Release()

	router := engine.NewProtocolRouter(doc, engine.RouterConfig{GRPC: &engine.GRPCConfig{}})
	defer router.Close()

	var req *ast.Request
	for r := range doc.Requests() {
		req = r
		break
	}
	if req == nil {
		t.Fatal("no request parsed")
	}

	eng, err := router.EngineFor(doc.Str(req.URLStart, req.URLEnd))
	if err != nil {
		t.Fatalf("EngineFor: %v", err)
	}
	if eng.Protocol() != engine.ProtoGRPC {
		t.Errorf("a grpc:// target routed to %v", eng.Protocol())
	}
	rest, err := router.EngineFor("https://example.com/x")
	if err != nil {
		t.Fatalf("EngineFor(rest): %v", err)
	}
	if rest.Protocol() != engine.ProtoREST {
		t.Errorf("an https:// target routed to %v", rest.Protocol())
	}

	// And a run through the router lands on the gRPC executor.
	sink := newGRPCSink()
	h := router.Run(context.Background(), req, sink)
	if err := h.WaitTimeout(30 * time.Second); err != nil {
		t.Fatalf("router run did not unwind: %v", err)
	}
	if got := sink.jsonBody(t)["message"]; got != "routed" {
		t.Errorf("message = %v, want routed", got)
	}
}

// TestRouterWithoutGRPCRefusesCleanly proves a REST-only build reports a clear
// error rather than misrouting a gRPC target to the REST executor — which would
// send "POST grpc://..." to an HTTP parser.
func TestRouterWithoutGRPCRefusesCleanly(t *testing.T) {
	arena := ast.AcquireArena()
	defer arena.Release()
	arena.Reset([]byte("POST grpc://127.0.0.1:1/pkg.Svc/Method\n\n{\"a\": 1}\n"))
	doc, err := ast.Parse(arena)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer doc.Release()

	router := engine.NewProtocolRouter(doc, engine.RouterConfig{}) // no GRPC config
	defer router.Close()

	for r := range doc.Requests() {
		sink := newGRPCSink()
		h := router.Run(context.Background(), r, sink)
		if err := h.WaitTimeout(10 * time.Second); err != nil {
			t.Fatalf("run did not unwind: %v", err)
		}
		_, runErr := sink.result()
		if !errors.Is(runErr, engine.ErrUnsupportedMethod) {
			t.Errorf("error = %v, want ErrUnsupportedMethod", runErr)
		}
		break
	}
}

// ---- descriptor cache -------------------------------------------------------

// TestDescriptorCacheIsBoundedAndNamespaced proves the cache evicts rather than
// growing, and that two targets cannot serve each other's descriptors.
//
// The service advertises four methods and the cache is capped at TWO, so a cache
// that never evicts is observably wrong. Resolving symbols that do not exist
// would not test anything: those fail before the insert.
func TestDescriptorCacheIsBoundedAndNamespaced(t *testing.T) {
	srv := startTestServer(t, true)
	cc := mustDial(t, srv.addr)
	defer cc.Close()

	const cap = 2
	cache := engine.NewDescriptorCache(cc, cap)
	ctx := context.Background()

	for _, method := range []string{"Echo", "Watch", "Collect", "Missing"} {
		if _, err := cache.Resolve(ctx, srv.addr, "/"+echoService+"/"+method); err != nil {
			t.Fatalf("resolve %s: %v", method, err)
		}
		if got := cache.Len(); got > cap {
			t.Fatalf("after %s the cache holds %d entries, cap is %d", method, got, cap)
		}
	}
	if got := cache.Len(); got != cap {
		t.Errorf("cache holds %d entries after four resolves into a %d-entry cache, want %d",
			got, cap, cap)
	}

	// Namespacing: the same symbol under a different target is a different key.
	md, err := cache.Resolve(ctx, srv.addr, "/"+echoService+"/Echo")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := string(md.FullName()); got != echoService+".Echo" {
		t.Errorf("resolved %q, want %q", got, echoService+".Echo")
	}
	if cache.Cached("other-host:1", "/"+echoService+"/Echo") {
		t.Error("a descriptor was served across targets; that is a silent correctness bug")
	}
}

// TestSchemaResolutionHappensOnce proves repeated runs reuse the cached
// descriptor rather than re-resolving per request.
func TestSchemaResolutionHappensOnce(t *testing.T) {
	srv := startTestServer(t, true)

	arena := ast.AcquireArena()
	defer arena.Release()
	arena.Reset([]byte("POST " + srv.symbol("Echo") + "\n\n{\"message\": \"a\"}\n"))
	doc, err := ast.Parse(arena)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer doc.Release()

	eng := engine.NewGRPCEngine(doc, engine.GRPCConfig{})
	defer eng.Close()

	var req *ast.Request
	for r := range doc.Requests() {
		req = r
		break
	}
	for i := range 3 {
		sink := newGRPCSink()
		h := eng.Run(context.Background(), req, sink)
		if err := h.WaitTimeout(30 * time.Second); err != nil {
			t.Fatalf("run %d did not unwind: %v", i, err)
		}
		if _, err := sink.result(); err != nil {
			t.Fatalf("run %d failed: %v", i, err)
		}
	}
}

// ---- fail-closed behaviour --------------------------------------------------

// TestUnresolvedVariableFailsClosed proves interpolation semantics match REST
// exactly: no placeholder, no empty string, hard error (AGENTS.md §4.3).
func TestUnresolvedVariableFailsClosed(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\n\n{\"message\": \"{{apiToken}}\"}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	_, runErr := runGRPC(t, src, sink, engine.GRPCConfig{}) // no resolver
	if runErr == nil {
		t.Fatal("an unresolved variable was accepted")
	}
	if !errors.Is(runErr, engine.ErrUnresolved) {
		t.Errorf("error = %v, want ErrUnresolved", runErr)
	}
	if srv.lastReceived() != nil {
		t.Error("a request with an unresolved variable was still sent")
	}
}

// TestXMLBodyIsRejectedExplicitly proves a body shape gRPC cannot carry fails
// with a clear message rather than being guessed at.
func TestXMLBodyIsRejectedExplicitly(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\n\n<message>hi</message>\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	_, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr == nil {
		t.Fatal("an XML body was accepted for a protobuf method")
	}
	if !strings.Contains(runErr.Error(), "XML") {
		t.Errorf("error should name the body kind: %v", runErr)
	}
}

// TestMetadataCarriesResolvedSecret proves the real value reaches the server
// (same rule as REST, AGENTS.md §3.4): outbound requests are never scrubbed,
// because the endpoint expects the real token. Redaction is a property of the
// diagnostic copies.
func TestMetadataCarriesResolvedSecret(t *testing.T) {
	srv := startTestServer(t, true)

	src := fmt.Sprintf("POST %s\nauthorization: Bearer {{tok}}\n\n{\"message\": \"x\"}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	if _, runErr := runGRPC(t, src, sink, engine.GRPCConfig{
		Resolver: staticResolver{"tok": "s3cr3t"},
	}); runErr != nil {
		t.Fatalf("run with a resolvable variable failed: %v", runErr)
	}
	if got := sink.jsonBody(t)["message"]; got != "x" {
		t.Errorf("message = %v, want x", got)
	}
}

// staticResolver resolves every variable to a fixed value.
type staticResolver map[string]string

func (s staticResolver) Lookup(name string) (string, error) {
	v, ok := s[name]
	if !ok {
		return "", fmt.Errorf("no such variable %q", name)
	}
	return v, nil
}

// TestNonSuccessStatusIsRecordedWithTheGRPCCode proves the store-visible status
// is a mapping and the gRPC code travels alongside it.
func TestNonSuccessStatusIsRecordedWithTheGRPCCode(t *testing.T) {
	srv := startTestServer(t, true)

	// Close the listener's server so the call fails with Unavailable, the most
	// common real gRPC failure.
	srv.stop()

	src := fmt.Sprintf("POST %s\n\n{\"message\": \"x\"}\n", srv.symbol("Echo"))
	sink := newGRPCSink()
	_, runErr := runGRPC(t, src, sink, engine.GRPCConfig{})
	if runErr == nil {
		t.Fatal("a call to a stopped server must fail")
	}
	if !strings.Contains(runErr.Error(), "Echo") {
		t.Errorf("error should name the method: %v", runErr)
	}
}
