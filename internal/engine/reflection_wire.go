package engine

import (
	"context"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// This file carries Pulse's own gRPC client machinery with NO generated code.
//
// The reflection service is itself a gRPC method, so resolving a method by
// reflection would normally require the generated reflection client — which is
// exactly the "statically compiled .pb.go" this move forbids. Instead the
// descriptor for grpc.reflection.v1 is CONSTRUCTED here, field-for-field, and
// every reflection request and reply is a dynamicpb message built from it.
//
// The consequence is the point: the binary ships zero protobuf-generated types.
// Schemas arrive at runtime, over the wire, from whatever server is being
// debugged. Nothing about that server was known when Pulse was compiled.

// ReflectionPackage is the proto package of the server reflection service.
const ReflectionPackage = "grpc.reflection.v1"

// ReflectionMethod is the fully-qualified reflection method name.
const ReflectionMethod = "/" + ReflectionPackage + ".ServerReflection/ServerReflectionInfo"

var (
	reflectionFileOnce sync.Once
	reflectionFile     protoreflect.FileDescriptor
	reflectionFileErr  error

	reflectionReqDesc  protoreflect.MessageDescriptor
	reflectionRespDesc protoreflect.MessageDescriptor
)

// field helpers keep the descriptor literal readable: protobuf descriptors are
// verbose by construction, and a wall of zero-valued literals is how subtle
// mistakes survive review.
func field(name, json string, num int32, typ descriptorpb.FieldDescriptorProto_Type, label descriptorpb.FieldDescriptorProto_Label) *descriptorpb.FieldDescriptorProto {
	l := label
	if l == descriptorpb.FieldDescriptorProto_LABEL_REPEATED {
		// A repeated field must not also be marked optional; proto3 repeated
		// implies LABEL_REPEATED and OPTIONAL is rejected by protodesc.
		l = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	}
	return &descriptorpb.FieldDescriptorProto{
		Name:     protoString(name),
		JsonName: protoString(json),
		Number:   protoInt32(num),
		Type:     typ.Enum(),
		Label:    l.Enum(),
	}
}

func msgField(name, json string, num int32, typeName string, oneofIdx *int32) *descriptorpb.FieldDescriptorProto {
	f := field(name, json, num, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE,
		descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
	f.TypeName = protoString(typeName)
	f.OneofIndex = oneofIdx
	return f
}

// oneofStrField builds a string field that is a MEMBER OF A ONEOF.
//
// The oneof index is a parameter rather than something set afterwards on
// purpose: every member of a oneof must carry the index, and forgetting one is
// a silent failure — the descriptor still builds, the message still marshals
// the right bytes, and the server reports "invalid MessageRequest: <nil>"
// because its generated type found no oneof case set. Getting this right for
// all five members is the whole reason this descriptor is written out by hand
// and covered by a test rather than generated.
func oneofStrField(name, json string, num int32, oneofIdx *int32) *descriptorpb.FieldDescriptorProto {
	f := field(name, json, num, descriptorpb.FieldDescriptorProto_TYPE_STRING,
		descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
	f.OneofIndex = oneofIdx
	return f
}

// oneofI32Field is the 32-bit variant, used by ExtensionNumberResponse's
// repeated int32 member below.
func oneofI32Field(name, json string, num int32) *descriptorpb.FieldDescriptorProto {
	f := field(name, json, num, descriptorpb.FieldDescriptorProto_TYPE_INT32,
		descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
	return f
}

func strField(name, json string, num int32) *descriptorpb.FieldDescriptorProto {
	return field(name, json, num, descriptorpb.FieldDescriptorProto_TYPE_STRING,
		descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
}

func bytesField(name, json string, num int32) *descriptorpb.FieldDescriptorProto {
	return field(name, json, num, descriptorpb.FieldDescriptorProto_TYPE_BYTES,
		descriptorpb.FieldDescriptorProto_LABEL_REPEATED)
}

func i32Field(name, json string, num int32) *descriptorpb.FieldDescriptorProto {
	return field(name, json, num, descriptorpb.FieldDescriptorProto_TYPE_INT32,
		descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL)
}

// reflectionFileDescriptor builds the FileDescriptorProto for the reflection
// service.
//
// Field numbers are normative wire protocol, not an implementation detail, and
// they are NOT the same in v1 and v1alpha. Getting them wrong is silent and
// nasty: the server decodes the bytes into its own generated type, finds no
// oneof case set, and answers "invalid MessageRequest: <nil>" — an error that
// names neither the field nor the cause. The numbers below are therefore taken
// from grpc/reflection/v1/reflection.proto, and TestReflectionDescriptorMatches
// the wire protocol pins every one of them.
//
//	v1 request:  host=1  oneof{file_by_filename=3, file_containing_symbol=4,
//	                          file_containing_extension=5,
//	                          all_extension_numbers_of_type=6, list_services=7}
//	v1 response: valid_host=1  original_request=2
//	             oneof{file_descriptor_response=4, all_extension_numbers_response=5,
//	                    list_services_response=6, error_response=7}
func reflectionFileDescriptor() *descriptorpb.FileDescriptorProto {
	const q = "." + ReflectionPackage + "."
	reqOneof := int32(0)
	respOneof := int32(0)

	return &descriptorpb.FileDescriptorProto{
		Name:    protoString("grpc/reflection/v1/reflection.proto"),
		Package: protoString(ReflectionPackage),
		Syntax:  protoString("proto3"),
		Options: &descriptorpb.FileOptions{
			GoPackage: protoString("pulse/internal/engine;engine"),
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: protoString("ExtensionRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					strField("containing_type", "containingType", 1),
					i32Field("extension_number", "extensionNumber", 2),
				},
			},
			{
				Name: protoString("ServerReflectionRequest"),
				Field: []*descriptorpb.FieldDescriptorProto{
					// host is a plain field, deliberately NOT in the oneof.
					strField("host", "host", 1),
					oneofStrField("file_by_filename", "fileByFilename", 3, &reqOneof),
					oneofStrField("file_containing_symbol", "fileContainingSymbol", 4, &reqOneof),
					msgField("file_containing_extension", "fileContainingExtension", 5, q+"ExtensionRequest", &reqOneof),
					oneofStrField("all_extension_numbers_of_type", "allExtensionNumbersOfType", 6, &reqOneof),
					oneofStrField("list_services", "listServices", 7, &reqOneof),
				},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{
					{Name: protoString("message_request")},
				},
			},
			{
				Name: protoString("FileDescriptorResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					bytesField("file_descriptor_proto", "fileDescriptorProto", 1),
				},
			},
			{
				Name: protoString("ExtensionNumberResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					strField("base_type_name", "baseTypeName", 1),
					oneofI32Field("extension_number", "extensionNumber", 2),
				},
			},
			{
				Name: protoString("ServiceResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					strField("name", "name", 1),
				},
			},
			{
				Name: protoString("ListServiceResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					msgField("service", "service", 1, q+"ServiceResponse", nil),
				},
			},
			{
				Name: protoString("ErrorResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					i32Field("error_code", "errorCode", 1),
					strField("error_message", "errorMessage", 2),
				},
			},
			{
				Name: protoString("ServerReflectionResponse"),
				Field: []*descriptorpb.FieldDescriptorProto{
					// valid_host, not v1alpha's valid_hosting_servers.
					strField("valid_host", "validHost", 1),
					msgField("original_request", "originalRequest", 2, q+"ServerReflectionRequest", nil),
					msgField("file_descriptor_response", "fileDescriptorResponse", 4, q+"FileDescriptorResponse", &respOneof),
					msgField("all_extension_numbers_response", "allExtensionNumbersResponse", 5, q+"ExtensionNumberResponse", &respOneof),
					msgField("list_services_response", "listServicesResponse", 6, q+"ListServiceResponse", &respOneof),
					msgField("error_response", "errorResponse", 7, q+"ErrorResponse", &respOneof),
				},
				OneofDecl: []*descriptorpb.OneofDescriptorProto{
					{Name: protoString("message_response")},
				},
			},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{
			{
				Name: protoString("ServerReflection"),
				Method: []*descriptorpb.MethodDescriptorProto{
					{
						Name:       protoString("ServerReflectionInfo"),
						InputType:  protoString(q + "ServerReflectionRequest"),
						OutputType: protoString(q + "ServerReflectionResponse"),
						Options:    &descriptorpb.MethodOptions{},
						// Reflection is bidirectional: the client streams
						// requests and streams replies on one long-lived RPC.
						ClientStreaming: protoBool(true),
						ServerStreaming: protoBool(true),
					},
				},
			},
		},
	}
}

func loadReflectionFile() (protoreflect.FileDescriptor, error) {
	reflectionFileOnce.Do(func() {
		fd, err := protodesc.NewFile(reflectionFileDescriptor(), nil)
		if err != nil {
			reflectionFileErr = err
			return
		}
		reflectionFile = fd
		reflectionReqDesc = fd.Messages().ByName("ServerReflectionRequest")
		reflectionRespDesc = fd.Messages().ByName("ServerReflectionResponse")
	})
	return reflectionFile, reflectionFileErr
}

// ReflectionFileDescriptor returns the hand-built reflection descriptor. It is
// exported so tests can assert the descriptor is well-formed without a server.
func ReflectionFileDescriptor() (protoreflect.FileDescriptor, error) { return loadReflectionFile() }

// newReflectionRequest builds a dynamic FileContainingSymbol request.
func newReflectionRequest(symbol string) (*dynamicpb.Message, error) {
	if _, err := loadReflectionFile(); err != nil {
		return nil, err
	}
	msg := dynamicpb.NewMessage(reflectionReqDesc)
	fd := reflectionReqDesc.Fields().ByName("file_containing_symbol")
	if fd == nil {
		return nil, errReflectionDescriptorIncomplete("file_containing_symbol")
	}
	msg.Set(fd, protoreflect.ValueOfString(symbol))
	return msg, nil
}

type errReflectionDescriptorIncomplete string

func (e errReflectionDescriptorIncomplete) Error() string {
	return "engine: grpc: reflection descriptor is missing field " + string(e) +
		"; the hand-built FileDescriptorProto does not match the wire protocol"
}

// reflectionStreamDesc describes ServerReflectionInfo for the client side.
// grpc-go uses ServerStreams/ClientStreams to frame the HTTP/2 stream, so a
// wrong value here manifests as a mysterious transport error rather than a
// clear one; the descriptor states what the method actually does.
var reflectionStreamDesc = grpc.StreamDesc{
	StreamName:    "ServerReflectionInfo",
	ServerStreams: true,
	ClientStreams: true,
}

// invokeServerReflection opens the bidirectional reflection stream and sends
// one request. The caller reads exactly one reply then closes the send
// direction: a reflection RPC is long-lived by design, and Pulse only needs a
// single lookup per descriptor cache miss.
func invokeServerReflection(ctx context.Context, cc grpc.ClientConnInterface, req *dynamicpb.Message) (grpc.ClientStream, error) {
	stream, err := cc.NewStream(ctx, &reflectionStreamDesc, ReflectionMethod)
	if err != nil {
		return nil, err
	}
	if err := stream.SendMsg(req); err != nil {
		_ = stream.CloseSend()
		return nil, err
	}
	if err := stream.CloseSend(); err != nil {
		return nil, err
	}
	return stream, nil
}

// recvReflectionReply reads one dynamic reply and returns its descriptor bytes
// when the reply carries them.
func recvReflectionReply(stream grpc.ClientStream) (*dynamicpb.Message, error) {
	reply := dynamicpb.NewMessage(reflectionRespDesc)
	if err := stream.RecvMsg(reply); err != nil {
		return nil, err
	}
	return reply, nil
}

// reflectionErrorMessage extracts the server's error string from a reflection
// reply, if it sent one. The server answers an unresolvable symbol with an
// ErrorResponse rather than a gRPC status, so it must be read explicitly.
func reflectionErrorMessage(reply *dynamicpb.Message) string {
	fd := reflectionRespDesc.Fields().ByName("error_response")
	if fd == nil {
		return ""
	}
	errMsg := reply.Get(fd).Message()
	if !errMsg.IsValid() {
		return ""
	}
	efd := errMsg.Descriptor().Fields().ByName("error_message")
	if efd == nil {
		return ""
	}
	return errMsg.Get(efd).String()
}

// reflectionFileDescriptorBytes extracts the serialized FileDescriptorProtos
// from a reflection reply.
func reflectionFileDescriptorBytes(reply *dynamicpb.Message) [][]byte {
	fd := reflectionRespDesc.Fields().ByName("file_descriptor_response")
	if fd == nil {
		return nil
	}
	msg := reply.Get(fd).Message()
	if !msg.IsValid() {
		return nil
	}
	inner := msg.Descriptor().Fields().ByName("file_descriptor_proto")
	if inner == nil {
		return nil
	}
	list := msg.Get(inner).List()
	out := make([][]byte, list.Len())
	for i := range out {
		b := list.Get(i).Bytes()
		out[i] = b
	}
	return out
}
