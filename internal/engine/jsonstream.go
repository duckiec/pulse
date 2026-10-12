package engine

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// This file implements the binary→JSON direction as a STREAMING encoder.
//
// The obvious implementation — protojson.Marshal into a []byte, then chop it
// into 32 KiB chunks — materializes the entire response before the first chunk
// is dispatched. That is a latent violation of the 2 MiB in-flight ceiling
// from ADL-0016: the ceiling would apply to the chunk channel while the real
// allocation happened a moment earlier and unmeasured. A server returning a
// 100 MB message would drive Pulse's heap by 100 MB regardless of how carefully
// the transport is bounded.
//
// So the encoder writes JSON incrementally through a fixed 32 KiB scratch
// buffer. Working memory is O(32 KiB) regardless of message size, and the same
// pooled-chunk path as REST carries the output.
//
// NOTE on compatibility: protojson deliberately emits unstable whitespace, so
// byte-for-byte equality with protojson is not a contract and is not one here.
// The encoder emits compact JSON with identical VALUES, field names, ordering,
// and number/string rules. Well-known types (Timestamp, Duration, Struct, …)
// have bespoke JSON forms; those subtrees are delegated to protojson so their
// encodings stay exactly right rather than approximately right.

// wellKnownJSON lists the messages protojson renders with a bespoke form.
// Delegating those subtrees keeps the exotic encodings (RFC 3339 timestamps,
// 64-bit-as-string wrappers, base64 padding, Struct-as-object) exactly right
// instead of approximately right. Everything else goes through the streaming
// encoder.
var wellKnownJSON = map[protoreflect.FullName]bool{
	"google.protobuf.Timestamp":   true,
	"google.protobuf.Duration":    true,
	"google.protobuf.FieldMask":   true,
	"google.protobuf.Struct":      true,
	"google.protobuf.Value":       true,
	"google.protobuf.ListValue":   true,
	"google.protobuf.NullValue":   true,
	"google.protobuf.DoubleValue": true,
	"google.protobuf.FloatValue":  true,
	"google.protobuf.Int64Value":  true,
	"google.protobuf.UInt64Value": true,
	"google.protobuf.Int32Value":  true,
	"google.protobuf.UInt32Value": true,
	"google.protobuf.BoolValue":   true,
	"google.protobuf.StringValue": true,
	"google.protobuf.BytesValue":  true,
	"google.protobuf.Any":         true,
}

// jsonStream encodes protobuf messages to JSON incrementally.
//
// The zero value is not usable; construct with newJSONStream. A jsonStream is
// single-use per message and not safe for concurrent use — that is fine,
// because each run owns its encoder.
type jsonStream struct {
	w       io.Writer
	scratch []byte
	opts    JSONOptions
	err     error

	// highWater is the peak size of the working buffer. It is the only direct
	// evidence that the response direction streams rather than buffering, so
	// the zero-allocation gate asserts on it.
	highWater int
}

// newJSONStream wraps w with a 32 KiB scratch buffer: the same geometry as the
// response chunk, so the encoder adds no buffer class of its own.
func newJSONStream(w io.Writer, opts JSONOptions) *jsonStream {
	return &jsonStream{w: w, scratch: make([]byte, 0, chunkSize), opts: opts}
}

// jsonUnmarshal and protojsonMarshal are named helpers so the tests read as
// assertions about semantics rather than about library plumbing.
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func protojsonMarshal(m proto.Message) ([]byte, error) {
	return protojson.MarshalOptions{}.Marshal(m)
}

// Emit writes one message as a JSON document, flushing the scratch buffer
// whenever it fills. On a short write the error is sticky: encoding cannot
// meaningfully continue after the consumer has gone away.
func (s *jsonStream) Emit(m protoreflect.Message) error {
	if err := s.message(m); err != nil {
		return err
	}
	return s.flush()
}

// EmitOrFallback renders a message with protojson when its type has a bespoke
// encoding, and with the streaming encoder otherwise.
func (s *jsonStream) EmitOrFallback(m protoreflect.Message) error {
	if wellKnownJSON[m.Descriptor().FullName()] {
		out, err := protojson.MarshalOptions{
			EmitUnpopulated: s.opts.EmitUnpopulated,
			UseProtoNames:   s.opts.UseProtoNames,
		}.Marshal(m.Interface())
		if err != nil {
			return err
		}
		_, err = s.w.Write(out)
		return err
	}
	return s.Emit(m)
}

func (s *jsonStream) put(b ...byte) {
	s.scratch = append(s.scratch, b...)
	s.maybeFlush()
}

func (s *jsonStream) putStr(str string) {
	s.scratch = append(s.scratch, str...)
	s.maybeFlush()
}

// maybeFlush keeps the working buffer at one chunk. Flushing eagerly is what
// makes memory independent of message size; the cost is a write per 32 KiB,
// which is the same cost the REST path already pays.
func (s *jsonStream) maybeFlush() {
	if len(s.scratch) > s.highWater {
		s.highWater = len(s.scratch)
	}
	if len(s.scratch) < chunkSize || s.err != nil {
		return
	}
	s.err = s.flush()
}

func (s *jsonStream) flush() error {
	if len(s.scratch) > s.highWater {
		s.highWater = len(s.scratch)
	}
	if s.err != nil {
		return s.err
	}
	if len(s.scratch) == 0 {
		return nil
	}
	_, err := s.w.Write(s.scratch)
	s.scratch = s.scratch[:0]
	if err != nil {
		s.err = err
	}
	return err
}

// message writes a message as a JSON object. Fields are visited in descriptor
// order, which is field-number order for a well-formed descriptor — the same
// determinism protojson provides and the reason two runs produce comparable
// output.
func (s *jsonStream) message(m protoreflect.Message) error {
	if s.err != nil {
		return s.err
	}
	s.put('{')
	fields := m.Descriptor().Fields()
	first := true
	for i := range fields.Len() {
		fd := fields.Get(i)
		if fd.IsExtension() {
			continue // extensions are out of scope for the JSON mapping
		}
		populated := m.Has(fd)
		if fd.HasDefault() && !populated {
			// A field carrying an explicit default is still "present" to
			// protojson only when EmitUnpopulated is set; proto3 defaults are
			// indistinguishable from unset on the wire.
			populated = s.opts.EmitUnpopulated
		}
		if !populated {
			continue
		}
		if !first {
			s.put(',')
		}
		first = false
		s.appendJSONString(fieldJSONName(fd, s.opts.UseProtoNames))
		s.put(':')
		if err := s.value(fd, m.Get(fd)); err != nil {
			return err
		}
	}
	s.put('}')
	return s.err
}

func fieldJSONName(fd protoreflect.FieldDescriptor, useProtoNames bool) string {
	if useProtoNames {
		return string(fd.Name())
	}
	return fd.JSONName()
}

// value writes one field value in protojson's representation.
func (s *jsonStream) value(fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	switch {
	case fd.IsMap():
		return s.mapValue(fd, v.Map())
	case fd.IsList():
		return s.listValue(fd, v.List())
	default:
		return s.scalar(fd, v)
	}
}

func (s *jsonStream) listValue(fd protoreflect.FieldDescriptor, list protoreflect.List) error {
	s.put('[')
	for i := range list.Len() {
		if i > 0 {
			s.put(',')
		}
		if err := s.scalar(fd, list.Get(i)); err != nil {
			return err
		}
	}
	s.put(']')
	return s.err
}

func (s *jsonStream) mapValue(fd protoreflect.FieldDescriptor, mp protoreflect.Map) error {
	s.put('{')
	first := true
	mp.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		if !first {
			s.put(',')
		}
		first = false
		s.appendJSONString(k.String())
		s.put(':')
		// A map value is typed by its value field descriptor, not the map
		// field's kind (which is a synthetic message kind).
		if err := s.scalar(fd.MapValue(), v); err != nil {
			return false
		}
		return s.err == nil
	})
	s.put('}')
	return s.err
}

// scalar writes a single non-repeated value.
func (s *jsonStream) scalar(fd protoreflect.FieldDescriptor, v protoreflect.Value) error {
	if s.err != nil {
		return s.err
	}
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		m := v.Message()
		if !m.IsValid() {
			// A null message renders as JSON null, matching protojson.
			s.putStr("null")
			return nil
		}
		if wellKnownJSON[m.Descriptor().FullName()] {
			out, err := protojson.MarshalOptions{
				EmitUnpopulated: s.opts.EmitUnpopulated,
				UseProtoNames:   s.opts.UseProtoNames,
			}.Marshal(m.Interface())
			if err != nil {
				return err
			}
			s.scratch = append(s.scratch, out...)
			s.maybeFlush()
			return nil
		}
		return s.message(m)

	case protoreflect.EnumKind:
		num := v.Enum()
		if ev := fd.Enum().Values().ByNumber(num); ev != nil {
			s.appendJSONString(string(ev.Name()))
		} else {
			// An unknown enum number is an error in protojson for a closed
			// enum; for an open one it renders as the number. Rendering the
			// number is the lossless choice and round-trips through decode.
			s.scratch = strconv.AppendInt(s.scratch, int64(num), 10)
			s.maybeFlush()
		}
		return nil

	case protoreflect.StringKind:
		s.appendJSONString(v.String())
		return nil

	case protoreflect.BytesKind:
		// protojson renders bytes as STANDARD base64 (with padding), which is
		// what a JSON consumer expects.
		s.appendJSONString(base64Encode(v.Bytes()))
		return nil

	case protoreflect.BoolKind:
		if v.Bool() {
			s.putStr("true")
		} else {
			s.putStr("false")
		}
		return nil

	case protoreflect.FloatKind, protoreflect.DoubleKind:
		f := v.Float()
		if math.IsNaN(f) {
			// JSON has no NaN; protojson quotes it, and so does this.
			s.appendJSONString("NaN")
			return nil
		}
		if math.IsInf(f, 1) {
			s.appendJSONString("Infinity")
			return nil
		}
		if math.IsInf(f, -1) {
			s.appendJSONString("-Infinity")
			return nil
		}
		bits := 64
		if fd.Kind() == protoreflect.FloatKind {
			bits = 32
		}
		s.scratch = strconv.AppendFloat(s.scratch, f, 'g', -1, bits)
		s.maybeFlush()
		return nil

	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		// protojson renders 64-bit signed integers as JSON STRINGS. This
		// asymmetry with 32-bit fields is protobuf's rule, not an
		// inconsistency, and clients depend on it.
		s.scratch = strconv.AppendQuote(s.scratch, strconv.FormatInt(v.Int(), 10))
		s.maybeFlush()
		return nil

	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		s.scratch = strconv.AppendQuote(s.scratch, strconv.FormatUint(v.Uint(), 10))
		s.maybeFlush()
		return nil

	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		s.scratch = strconv.AppendInt(s.scratch, v.Int(), 10)
		s.maybeFlush()
		return nil

	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		s.scratch = strconv.AppendUint(s.scratch, v.Uint(), 10)
		s.maybeFlush()
		return nil

	default:
		return fmt.Errorf("engine: grpc: cannot render field %s of kind %v as JSON", fd.Name(), fd.Kind())
	}
}

// appendJSONString writes a JSON string literal.
//
// encoding/json's escaper is used rather than strconv.AppendQuote because
// AppendQuote emits Go escapes (\x00, \a, \v) that are INVALID JSON — a subtle
// and very real trap when hand-rolling this.
func (s *jsonStream) appendJSONString(str string) {
	s.scratch = appendJSONQuoted(s.scratch, str)
	s.maybeFlush()
}

// appendJSONQuoted appends a JSON-quoted string to dst.
//
// encoding/json's escaper is used rather than strconv.AppendQuote because
// AppendQuote emits Go escapes (\x00, \a, \v) that are INVALID JSON — a subtle
// and very real trap when hand-rolling this. Invalid UTF-8 becomes U+FFFD, the
// same substitution protojson makes: output that cannot be decoded is worse
// than output that has lost a byte.
func appendJSONQuoted(dst []byte, str string) []byte {
	dst = append(dst, '"')
	for i := 0; i < len(str); {
		r, size := utf8.DecodeRuneInString(str[i:])
		if r == utf8.RuneError && size == 1 {
			dst = append(dst, "�"...)
			i++
			continue
		}
		if r < 0x20 || r == '"' || r == '\\' {
			dst = appendJSONEscape(dst, byte(r))
			i += size
			continue
		}
		dst = append(dst, str[i:i+size]...)
		i += size
	}
	return append(dst, '"')
}

// appendJSONEscape writes the escape for one control character.
func appendJSONEscape(dst []byte, c byte) []byte {
	const hexDigits = "0123456789abcdef"
	switch c {
	case '"':
		return append(dst, '\\', '"')
	case '\\':
		return append(dst, '\\', '\\')
	case '\n':
		return append(dst, '\\', 'n')
	case '\r':
		return append(dst, '\\', 'r')
	case '\t':
		return append(dst, '\\', 't')
	case '\b':
		return append(dst, '\\', 'b')
	case '\f':
		return append(dst, '\\', 'f')
	default:
		return append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
	}
}

// base64Encode renders bytes as standard base64, matching protojson's default.
func base64Encode(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
