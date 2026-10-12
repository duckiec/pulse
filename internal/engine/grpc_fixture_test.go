package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"pulse/internal/ast"
	"pulse/internal/engine"
)

// This file builds a gRPC server whose ENTIRE schema is constructed at runtime.
//
// There is no .proto file, no protoc, and no generated Go type anywhere in this
// repository — only in the fixture below, and only as descriptor structs. That
// is the point: it proves Pulse can drive a service for which nothing was
// compiled, which is the property Move 6 exists to establish. If these tests
// passed against generated stubs they would prove nothing.

const (
	testPackage = "pulse.test.v1"
	echoService = testPackage + ".EchoService"
)

// echoFileDescriptor is the test service's schema, built field by field.
//
// The field kinds are chosen to exercise every branch of the dynamic type
// mapping: a string, an int32 (the mutation target), an int64 (which protojson
// renders as a JSON string), an enum, a bool, a nested message, and a repeated
// message.
func echoFileDescriptor() *descriptorpb.FileDescriptorProto {
	q := "." + testPackage + "."
	str := func(n, j string, num int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: protoStr(n), JsonName: protoStr(j), Number: protoInt(num),
			Type:  descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}
	}
	i32 := func(n, j string, num int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: protoStr(n), JsonName: protoStr(j), Number: protoInt(num),
			Type:  descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}
	}
	i64 := func(n, j string, num int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: protoStr(n), JsonName: protoStr(j), Number: protoInt(num),
			Type:  descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}
	}
	bl := func(n, j string, num int32) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: protoStr(n), JsonName: protoStr(j), Number: protoInt(num),
			Type:  descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum(),
			Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}
	}
	msgf := func(n, j string, num int32, typeName string) *descriptorpb.FieldDescriptorProto {
		return &descriptorpb.FieldDescriptorProto{
			Name: protoStr(n), JsonName: protoStr(j), Number: protoInt(num),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
			TypeName: protoStr(typeName),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}
	}
	repeated := func(f *descriptorpb.FieldDescriptorProto) *descriptorpb.FieldDescriptorProto {
		f.Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		return f
	}

	return &descriptorpb.FileDescriptorProto{
		Name:    protoStr("pulse/test/v1/echo.proto"),
		Package: protoStr(testPackage),
		Syntax:  protoStr("proto3"),
		Options: &descriptorpb.FileOptions{GoPackage: protoStr("pulse.test.v1")},
		EnumType: []*descriptorpb.EnumDescriptorProto{
			{
				Name: protoStr("Mode"),
				Value: []*descriptorpb.EnumValueDescriptorProto{
					{Name: protoStr("MODE_UNSPECIFIED"), Number: protoInt(0)},
					{Name: protoStr("MODE_ECHO"), Number: protoInt(1)},
					{Name: protoStr("MODE_SHOUT"), Number: protoInt(2)},
				},
			},
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: protoStr("Filter"),
				Field: []*descriptorpb.FieldDescriptorProto{
					i32("page_size", "pageSize", 1),
					str("prefix", "prefix", 2),
				},
			},
			{
				Name: protoStr("EchoRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					str("message", "message", 1),
					i32("limit", "limit", 2),
					i64("budget", "budget", 3),
					bl("loud", "loud", 4),
					msgf("filter", "filter", 5, q+"Filter"),
					repeated(msgf("tags", "tags", 6, q+"Filter")),
					{
						Name: protoStr("mode"), JsonName: protoStr("mode"), Number: protoInt(7),
						Type:     descriptorpb.FieldDescriptorProto_TYPE_ENUM.Enum(),
						TypeName: protoStr("." + testPackage + ".Mode"),
						Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
					},
				},
			},
			{
				Name: protoStr("EchoReply"),
				Field: []*descriptorpb.FieldDescriptorProto{
					str("message", "message", 1),
					i32("limit", "limit", 2),
					i64("budget", "budget", 3),
					bl("loud", "loud", 4),
					msgf("filter", "filter", 5, q+"Filter"),
					repeated(msgf("tags", "tags", 6, q+"Filter")),
				},
			},
			{
				Name: protoStr("Blob"),
				Field: []*descriptorpb.FieldDescriptorProto{
					str("payload", "payload", 1),
				},
			},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name: protoStr("EchoService"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{
						Name:       protoStr("Echo"),
						InputType:  protoStr(q + "EchoRequest"),
						OutputType: protoStr(q + "EchoReply"),
						Options:    &descriptorpb.MethodOptions{},
					},
					{
						Name:            protoStr("Watch"),
						InputType:       protoStr(q + "EchoRequest"),
						OutputType:      protoStr(q + "EchoReply"),
						ServerStreaming: protoBool(true),
						Options:         &descriptorpb.MethodOptions{},
					},
					{
						Name:            protoStr("Collect"),
						InputType:       protoStr(q + "EchoRequest"),
						OutputType:      protoStr(q + "EchoReply"),
						ClientStreaming: protoBool(true),
						Options:         &descriptorpb.MethodOptions{},
					},
					{
						Name:       protoStr("Missing"),
						InputType:  protoStr(q + "EchoRequest"),
						OutputType: protoStr(q + "EchoReply"),
						Options:    &descriptorpb.MethodOptions{},
					},
				},
			},
		},
	}
}

func protoStr(s string) *string { return &s }
func protoInt(i int32) *int32   { return &i }
func protoBool(b bool) *bool    { return &b }

// testServer is a gRPC server whose service is registered dynamically.
type testServer struct {
	addr      string
	stop      func()
	fd        protoreflect.FileDescriptor
	reqDescs  map[string]protoreflect.MessageDescriptor
	fileProto *descriptorpb.FileDescriptorProto

	mu       sync.Mutex
	received []*dynamicpb.Message // request messages as the server decoded them
	watchN   int
}

var (
	registerOnce sync.Once
	registerFD   protoreflect.FileDescriptor
	registerErr  error
)

// startTestServer builds and starts the dynamic gRPC server.
//
// The service implementation is a plain Go struct with a hand-written
// grpc.ServiceDesc: no interface satisfaction check, no generated code, and a
// handler that decodes into a dynamicpb message. That is the entire server.
//
// The file is registered in the global registry ONCE — grpc's reflection service
// reads from that registry, and re-registering the same name panics. Each call
// therefore starts a fresh server over the same shared descriptor.
func startTestServer(t *testing.T, registerReflection bool) *testServer {
	t.Helper()

	fdp := echoFileDescriptor()
	registerOnce.Do(func() {
		fd, err := protodesc.NewFile(fdp, nil)
		if err != nil {
			registerErr = err
			return
		}
		registerFD = fd
		registerErr = protoregistry.GlobalFiles.RegisterFile(fd)
	})
	if registerErr != nil {
		t.Fatalf("register dynamic descriptor: %v", registerErr)
	}
	fd := registerFD

	ts := &testServer{
		fd:        fd,
		reqDescs:  map[string]protoreflect.MessageDescriptor{},
		fileProto: fdp,
	}
	for i := range fd.Messages().Len() {
		m := fd.Messages().Get(i)
		ts.reqDescs[string(m.Name())] = m
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: echoService,
		// (*any) means "every implementation satisfies this", which is exactly
		// the contract here: there is no Go interface for a service that has no
		// generated type.
		HandlerType: (*any)(nil),
		Methods: []grpc.MethodDesc{
			{MethodName: "Echo", Handler: ts.echoHandler},
			// "Missing" is declared in the descriptor but NOT registered as a
			// handler, so the server returns Unimplemented while reflection
			// still advertises it — the case that distinguishes "method missing"
			// from "server says no".
		},
		Streams: []grpc.StreamDesc{
			{StreamName: "Watch", Handler: ts.watchHandler, ServerStreams: true},
			{StreamName: "Collect", Handler: ts.collectHandler, ClientStreams: true, ServerStreams: true},
		},
		Metadata: "pulse/test/v1/echo.proto",
	}, ts)

	if registerReflection {
		reflection.Register(srv)
	}

	go func() { _ = srv.Serve(ln) }()
	ts.addr = ln.Addr().String()
	ts.stop = func() { srv.Stop() }
	t.Cleanup(ts.stop)
	return ts
}

func (ts *testServer) symbol(method string) string {
	return "grpc://" + ts.addr + "/" + echoService + "/" + method
}

func (ts *testServer) record(m *dynamicpb.Message) {
	ts.mu.Lock()
	ts.received = append(ts.received, m)
	ts.mu.Unlock()
}

func (ts *testServer) lastReceived() *dynamicpb.Message {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.received) == 0 {
		return nil
	}
	return ts.received[len(ts.received)-1]
}

// echoHandler answers a unary call by echoing the request fields back.
func (ts *testServer) echoHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	reqDesc := ts.reqDescs["EchoRequest"]
	in := dynamicpb.NewMessage(reqDesc)
	if err := dec(in); err != nil {
		return nil, err
	}
	ts.record(in)
	out := dynamicpb.NewMessage(ts.reqDescs["EchoReply"])
	copyCommon(reqDesc, in, out)
	return out, nil
}

// watchHandler streams n replies, each tagged with its index.
func (ts *testServer) watchHandler(srv any, stream grpc.ServerStream) error {
	reqDesc := ts.reqDescs["EchoRequest"]
	in := dynamicpb.NewMessage(reqDesc)
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	ts.record(in)
	n := ts.watchCount()
	reqFd := reqDesc.Fields().ByName("message") // the field descriptor must come from the message it is read from
	for i := range n {
		out := dynamicpb.NewMessage(ts.reqDescs["EchoReply"])
		copyCommon(reqDesc, in, out)
		out.Set(out.Descriptor().Fields().ByName("message"),
			protoreflect.ValueOfString(fmt.Sprintf("%v#%d", in.Get(reqFd).String(), i)))
		if err := stream.SendMsg(out); err != nil {
			return err
		}
	}
	return nil
}

func (ts *testServer) watchCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	n := ts.watchN
	if n == 0 {
		n = 3
	}
	return n
}

// collectHandler accepts a client stream and replies once. Pulse must refuse
// this shape before dialing it.
func (ts *testServer) collectHandler(srv any, stream grpc.ServerStream) error {
	reqDesc := ts.reqDescs["EchoRequest"]
	for {
		in := dynamicpb.NewMessage(reqDesc)
		if err := stream.RecvMsg(in); err != nil {
			return nil // EOF or cancellation
		}
		ts.record(in)
	}
}

// copyCommon copies the shared EchoRequest fields onto an EchoReply.
//
// Two dynamicpb rules drive this: an unset field must not be copied at all (its
// zero value panics on Set), and a repeated field must be appended element by
// element rather than assigned wholesale.
func copyCommon(reqDesc protoreflect.MessageDescriptor, in *dynamicpb.Message, out *dynamicpb.Message) {
	for _, name := range []string{"message", "limit", "budget", "loud", "filter", "tags"} {
		inFd := reqDesc.Fields().ByName(protoreflect.Name(name))
		outFd := out.Descriptor().Fields().ByName(protoreflect.Name(name))
		if inFd == nil || outFd == nil || !in.Has(inFd) {
			continue // an absent field stays absent on the reply
		}
		switch {
		case inFd.IsList():
			src := in.Get(inFd).List()
			dst := out.Mutable(outFd).List()
			for i := range src.Len() {
				dst.Append(src.Get(i))
			}
		case inFd.Kind() == protoreflect.MessageKind || inFd.Kind() == protoreflect.GroupKind:
			// Both sides are the same Filter descriptor, so a field-by-field
			// copy is exact and avoids depending on proto.Merge's cross-type
			// semantics. The source must be ranged over: ranging the
			// destination copies an empty message onto itself.
			src := in.Get(inFd).Message()
			dst := out.Mutable(outFd).Message()
			src.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
				dst.Set(fd, v)
				return true
			})
		default:
			out.Set(outFd, in.Get(inFd))
		}
	}
}

// ---- sink ------------------------------------------------------------------

// grpcSink records what the engine delivered.
type grpcSink struct {
	mu     sync.Mutex
	body   []byte
	res    *engine.RunResult
	err    error
	done   chan struct{}
	once   sync.Once
	chunks int
	maxCh  int
}

func newGRPCSink() *grpcSink { return &grpcSink{done: make(chan struct{})} }

func (s *grpcSink) Chunk(c *engine.Chunk) {
	s.mu.Lock()
	s.body = append(s.body, c.Bytes()...)
	s.chunks++
	if c.N > s.maxCh {
		s.maxCh = c.N
	}
	s.mu.Unlock()
	c.Recycle()
}

func (s *grpcSink) Done(r *engine.RunResult) {
	s.mu.Lock()
	s.res = r
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
}

func (s *grpcSink) Failed(e error) {
	s.mu.Lock()
	s.err = e
	s.mu.Unlock()
	s.once.Do(func() { close(s.done) })
}

func (s *grpcSink) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.done:
	case <-time.After(30 * time.Second):
		t.Fatal("gRPC run never reached a terminal callback")
	}
}

func (s *grpcSink) result() (*engine.RunResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.res, s.err
}

// text returns the delivered body.
func (s *grpcSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.body)
}

// jsonBody decodes the delivered body as JSON.
//
// Comparison is by VALUE, never by bytes: protojson deliberately emits unstable
// whitespace, so a byte comparison would fail intermittently for reasons that
// have nothing to do with correctness.
func (s *grpcSink) jsonBody(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(s.body, &out); err != nil {
		t.Fatalf("delivered body is not JSON (%v): %q", err, s.text())
	}
	return out
}

// mustDial opens a plaintext ClientConn to a test server.
func mustDial(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// runGRPC parses src, runs its first request through a gRPC executor, and waits.
func runGRPC(t *testing.T, src string, sink engine.Sink, cfg engine.GRPCConfig) (*engine.RunResult, error) {
	t.Helper()
	arena := ast.AcquireArena()
	arena.Reset([]byte(src))
	doc, err := ast.Parse(arena)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	defer doc.Release()

	eng := engine.NewGRPCEngine(doc, cfg)
	defer eng.Close()

	for r := range doc.Requests() {
		h := eng.Run(context.Background(), r, sink)
		if err := h.WaitTimeout(30 * time.Second); err != nil {
			t.Fatalf("run did not unwind: %v", err)
		}
		break
	}
	return sink.(*grpcSink).result()
}
