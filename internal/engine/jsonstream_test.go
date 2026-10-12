package engine

import (
	"bytes"
	"strings"
	"testing"

	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// This file is an in-package test so it can observe the encoder's working-set
// high-water mark. That mark is the only honest way to prove the binary→JSON
// direction does not materialize a whole message before dispatching it: the
// externally visible symptom (chunk size, chunk count) is identical whether the
// encoder streams or buffers-then-chops.

// wideDescriptor builds a message with many fields, so its JSON is large
// without any single field being large.
func wideDescriptor(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	const n = 400
	fields := make([]*descriptorpb.FieldDescriptorProto, n)
	for i := range n {
		fields[i] = &descriptorpb.FieldDescriptorProto{
			Name:     protoString("f" + itoa(i)),
			JsonName: protoString("f" + itoa(i)),
			Number:   protoInt32(int32(i + 1)),
			Type:     descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		}
	}
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name:    protoString("pulse/test/wide.proto"),
		Package: protoString("pulse.test"),
		Syntax:  protoString("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: protoString("Wide"), Field: fields},
		},
	}, nil)
	if err != nil {
		t.Fatalf("build descriptor: %v", err)
	}
	return fd.Messages().Get(0)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}

// TestJSONStreamDoesNotMaterializeTheMessage is the zero-allocation invariant
// for the binary→JSON direction (Move 6): working memory must be independent of
// message size.
func TestJSONStreamDoesNotMaterializeTheMessage(t *testing.T) {
	md := wideDescriptor(t)
	msg := dynamicpb.NewMessage(md)
	fields := md.Fields()
	for i := range fields.Len() {
		msg.Set(fields.Get(i), protoreflect.ValueOfString(strings.Repeat("x", 512)))
	}

	var out bytes.Buffer
	enc := newJSONStream(&out, DefaultJSONOptions())
	if err := enc.Emit(msg.ProtoReflect()); err != nil {
		t.Fatalf("encode: %v", err)
	}

	encoded := out.Len()
	if encoded < 200_000 {
		t.Fatalf("fixture too small to prove the point: %d encoded bytes", encoded)
	}

	// The whole point: the encoder's working buffer stayed at chunk geometry
	// while producing a payload an order of magnitude larger. A
	// buffer-then-chop implementation would show a high-water mark equal to the
	// full payload.
	hw := enc.highWater
	if hw > chunkSize+4096 {
		t.Errorf("encoder working set peaked at %d bytes for a %d-byte document; "+
			"the response direction is buffering the message instead of streaming it",
			hw, encoded)
	}
	t.Logf("encoded %d bytes with a working set peaking at %d bytes (chunk ceiling %d)",
		encoded, hw, chunkSize)

	// And the output is still valid, complete JSON.
	var back map[string]any
	if err := jsonUnmarshal(out.Bytes(), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(back) != fields.Len() {
		t.Errorf("round-tripped %d fields, want %d", len(back), fields.Len())
	}
}

// TestJSONStreamMatchesProtoJSONSemantics proves the streaming encoder and
// protojson agree on VALUES — the claim the streaming encoder rests on. Byte
// equality is deliberately not asserted: protojson's whitespace is unstable by
// design.
func TestJSONStreamMatchesProtoJSONSemantics(t *testing.T) {
	md := wideDescriptor(t)
	msg := dynamicpb.NewMessage(md)
	fields := md.Fields()
	for i := range fields.Len() {
		f := fields.Get(i)
		switch i % 5 {
		case 0:
			msg.Set(f, protoreflect.ValueOfString("plain"))
		case 1:
			msg.Set(f, protoreflect.ValueOfString("quote\" backslash\\ newline\n tab\t ctl\x01"))
		case 2:
			msg.Set(f, protoreflect.ValueOfString(strings.Repeat("é☃", 64)))
		case 3:
			msg.Set(f, protoreflect.ValueOfString(""))
		default:
			msg.Set(f, protoreflect.ValueOfString(strings.Repeat("z", 300)))
		}
	}

	var streamed bytes.Buffer
	if err := newJSONStream(&streamed, DefaultJSONOptions()).Emit(msg.ProtoReflect()); err != nil {
		t.Fatalf("stream encode: %v", err)
	}

	reference, err := protojsonMarshal(msg)
	if err != nil {
		t.Fatalf("protojson: %v", err)
	}

	var a, b map[string]any
	if err := jsonUnmarshal(streamed.Bytes(), &a); err != nil {
		t.Fatalf("streamed output is not JSON: %v", err)
	}
	if err := jsonUnmarshal(reference, &b); err != nil {
		t.Fatalf("protojson output is not JSON: %v", err)
	}
	if len(a) != len(b) {
		t.Fatalf("streamed %d fields, protojson %d", len(a), len(b))
	}
	for k, want := range b {
		got, ok := a[k]
		if !ok {
			t.Errorf("field %q missing from the streamed output", k)
			continue
		}
		if got != want {
			t.Errorf("field %q: streamed %q, protojson %q", k, got, want)
		}
	}
}

// TestJSONQuotingRejectsGoEscapes pins the escaping rule: strconv.AppendQuote
// emits \x00, which is not valid JSON, so the encoder must not use it.
func TestJSONQuotingRejectsGoEscapes(t *testing.T) {
	got := string(appendJSONQuoted(nil, "a\x00b\x1fc"))
	want := "\"a\\u0000b\\u001fc\""
	if got != want {
		t.Errorf("appendJSONQuoted = %s, want %s", got, want)
	}
	var back string
	if err := jsonUnmarshal([]byte(got), &back); err != nil {
		t.Errorf("output does not decode as JSON: %v (%s)", err, got)
	}
}

// TestJSONQuotingSubstitutesInvalidUTF8 proves malformed input still produces
// decodable output rather than corrupting the document.
func TestJSONQuotingSubstitutesInvalidUTF8(t *testing.T) {
	// 0xff 0xfe are not valid UTF-8: each must become U+FFFD rather than
	// corrupting the document.
	got := string(appendJSONQuoted(nil, "ok\xff\xfeend"))
	var back string
	if err := jsonUnmarshal([]byte(got), &back); err != nil {
		t.Errorf("output does not decode as JSON: %v (%s)", err, got)
	}
	if !strings.HasPrefix(back, "ok") || !strings.HasSuffix(back, "end") {
		t.Errorf("substitution damaged the surrounding text: %q", back)
	}
}
