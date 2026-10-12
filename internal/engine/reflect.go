package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Symbol is a gRPC method address: "/package.Service/Method".
type Symbol struct {
	Package string
	Service string
	Method  string
}

// ParseSymbol splits "/pkg.Svc/Method" into its parts.
//
// The parse is strict and total: a symbol missing its leading slash, its
// package separator, or its method name is rejected here rather than producing a
// request that fails opaquely on the wire. Failing closed at the boundary is
// what makes a bad .http line a clear error instead of a network timeout.
func ParseSymbol(s string) (Symbol, error) {
	if len(s) == 0 || s[0] != '/' {
		return Symbol{}, fmt.Errorf("engine: grpc: malformed symbol %q (want /package.Service/Method)", s)
	}
	slash := -1
	for i := len(s) - 1; i > 0; i-- {
		if s[i] == '/' {
			slash = i
			break
		}
	}
	if slash <= 1 || slash == len(s)-1 {
		return Symbol{}, fmt.Errorf("engine: grpc: malformed symbol %q (want /package.Service/Method)", s)
	}
	svc, method := s[1:slash], s[slash+1:]
	dot := -1
	for i := len(svc) - 1; i >= 0; i-- {
		if svc[i] == '.' {
			dot = i
			break
		}
	}
	if dot <= 0 || dot == len(svc)-1 {
		return Symbol{}, fmt.Errorf("engine: grpc: malformed service %q in symbol %q (want package.Service)", svc, s)
	}
	return Symbol{Package: svc[:dot], Service: svc[dot+1:], Method: method}, nil
}

// String renders the canonical "/package.Service/Method" form.
func (s Symbol) String() string { return "/" + s.Package + "." + s.Service + "/" + s.Method }

// ServiceFullName renders "package.Service".
func (s Symbol) ServiceFullName() string { return s.Package + "." + s.Service }

// Errors from schema resolution. Both are recoverable in the sense that the
// caller may retry with a different resolution source (ROADMAP.md §2.2.2), so
// they are distinct sentinels rather than one opaque failure.
var (
	// ErrReflectionUnavailable means the server does not expose the reflection
	// service, or refused the lookup. A local descriptor set is the fallback.
	ErrReflectionUnavailable = errors.New("engine: grpc: server reflection unavailable")

	// ErrMethodNotFound means descriptors were resolved but carry no such
	// method — a real answer from the server, not a transport problem.
	ErrMethodNotFound = errors.New("engine: grpc: method not found in descriptors")
)

// DescriptorCache resolves method symbols to descriptors over gRPC server
// reflection, caching results per (target, symbol).
//
// The cache exists because resolution is a network round-trip and a request run
// must not repeat it; it is bounded because an unbounded descriptor cache is a
// memory leak with a network-shaped hole in it. Eviction is strict LRU.
type DescriptorCache struct {
	cc grpc.ClientConnInterface

	mu         sync.RWMutex
	cache      map[string]protoreflect.MethodDescriptor
	maxEntries int
	order      *lru
}

const defaultDescriptorCacheEntries = 256

// NewDescriptorCache returns a cache bounded to maxEntries (≤0 takes the
// default of 256).
func NewDescriptorCache(cc grpc.ClientConnInterface, maxEntries int) *DescriptorCache {
	if maxEntries <= 0 {
		maxEntries = defaultDescriptorCacheEntries
	}
	return &DescriptorCache{
		cc:         cc,
		cache:      make(map[string]protoreflect.MethodDescriptor, 8),
		maxEntries: maxEntries,
		order:      newLRU(maxEntries),
	}
}

// cacheKey namespaces by target so one connection can never serve another
// target's descriptors: the same symbol means different things on different
// servers, and a cross-server cache hit would be a silent correctness bug.
func cacheKey(target, symbol string) string { return target + "|" + symbol }

// Resolve returns the method descriptor for symbol, fetching it from the server
// on a cache miss.
func (c *DescriptorCache) Resolve(ctx context.Context, target, symbol string) (protoreflect.MethodDescriptor, error) {
	key := cacheKey(target, symbol)

	c.mu.RLock()
	md, ok := c.cache[key]
	c.mu.RUnlock()
	if ok {
		c.order.get(key)
		return md, nil
	}

	md, err := FetchMethodDescriptor(ctx, c.cc, symbol)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.cache[key]; !exists {
		if evicted, dropped := c.order.push(key); dropped {
			delete(c.cache, evicted)
		}
		c.cache[key] = md
	}
	return md, nil
}

// Len reports how many descriptors are resident (for tests and diagnostics).
func (c *DescriptorCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache)
}

// Cached reports whether a target/symbol pair is already resident, without
// promoting it.
func (c *DescriptorCache) Cached(target, symbol string) bool {
	c.mu.RLock()
	_, ok := c.cache[cacheKey(target, symbol)]
	c.mu.RUnlock()
	return ok
}

// FetchMethodDescriptor resolves a symbol to a method descriptor through the
// server's reflection service.
//
// It deliberately requests by SERVICE and pulls the whole transitive descriptor
// set: protojson cannot resolve a message whose file imports others, and a
// hand-back of only the requested file produces "could not resolve import"
// errors far from their cause. The cost is a few extra descriptors per lookup;
// the benefit is that resolution succeeds exactly when the server can serve the
// method.
func FetchMethodDescriptor(ctx context.Context, cc grpc.ClientConnInterface, symbol string) (protoreflect.MethodDescriptor, error) {
	sym, err := ParseSymbol(symbol)
	if err != nil {
		return nil, err
	}
	req, err := newReflectionRequest(sym.ServiceFullName())
	if err != nil {
		return nil, err
	}
	stream, err := invokeServerReflection(ctx, cc, req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrReflectionUnavailable, err)
	}
	reply, err := recvReflectionReply(stream)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrReflectionUnavailable, err)
	}
	if msg := reflectionErrorMessage(reply); msg != "" {
		return nil, fmt.Errorf("%w: %s", ErrReflectionUnavailable, msg)
	}
	blobs := reflectionFileDescriptorBytes(reply)
	if len(blobs) == 0 {
		return nil, fmt.Errorf("%w: no descriptors returned for %s", ErrReflectionUnavailable, sym.ServiceFullName())
	}
	set, err := DescriptorSetFromBytes(blobs)
	if err != nil {
		return nil, err
	}
	return MethodFromSet(set, symbol)
}

// DescriptorSetFromBytes assembles a FileDescriptorSet from serialized
// FileDescriptorProtos, following transitive imports breadth-first.
//
// Following imports is not optional: a message referring to a type from an
// imported file cannot be resolved from that file alone, and failing to pull
// the import would surface as an unrelated "unknown type" error at marshal time.
func DescriptorSetFromBytes(blobs [][]byte) (*descriptorpb.FileDescriptorSet, error) {
	set := &descriptorpb.FileDescriptorSet{}
	seen := make(map[string]bool, len(blobs))
	for _, blob := range blobs {
		if len(blob) == 0 {
			continue
		}
		fd := &descriptorpb.FileDescriptorProto{}
		if err := proto.Unmarshal(blob, fd); err != nil {
			return nil, fmt.Errorf("engine: grpc: a returned descriptor is not a FileDescriptorProto: %w", err)
		}
		if seen[fd.GetName()] {
			continue
		}
		seen[fd.GetName()] = true
		set.File = append(set.File, fd)
	}
	// A file naming an import the server did not send cannot be linked: the
	// resolver would fail later with an "unknown type" error far from its
	// cause. Report the missing import now, by name.
	missing := map[string]bool{}
	for _, fd := range set.File {
		for _, dep := range fd.GetDependency() {
			if !seen[dep] {
				missing[dep] = true
			}
		}
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for n := range missing {
			names = append(names, n)
		}
		sortStrings(names)
		return nil, fmt.Errorf("engine: grpc: reflection returned %d descriptors but none for the imports %v", len(set.File), names)
	}
	return set, nil
}

// MethodFromSet resolves a symbol against a descriptor set already in hand.
// This is the offline path: no network, no reflection service, just descriptors
// that came from disk or from the binary (ROADMAP.md §2.2.2).
func MethodFromSet(set *descriptorpb.FileDescriptorSet, symbol string) (protoreflect.MethodDescriptor, error) {
	if set == nil {
		return nil, fmt.Errorf("%w: no descriptor set available", ErrReflectionUnavailable)
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		return nil, fmt.Errorf("engine: grpc: descriptor set is not self-consistent: %w", err)
	}
	sym, err := ParseSymbol(symbol)
	if err != nil {
		return nil, err
	}
	return methodFromFiles(files, sym)
}

func methodFromFiles(files *protoregistry.Files, sym Symbol) (protoreflect.MethodDescriptor, error) {
	d, err := files.FindDescriptorByName(protoreflect.FullName(sym.ServiceFullName()))
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrMethodNotFound, sym.ServiceFullName())
	}
	sd, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%w: %q resolves to %s, not a service", ErrMethodNotFound, sym.ServiceFullName(), d.ParentFile().Path())
	}
	md := sd.Methods().ByName(protoreflect.Name(sym.Method))
	if md == nil {
		return nil, fmt.Errorf("%w: %s", ErrMethodNotFound, sym)
	}
	return md, nil
}

// ---- dynamic JSON <-> binary mapping ----------------------------------------

// FieldTypeError is a JSON↔protobuf type mismatch reported with the field path
// that caused it.
//
// It exists because the alternative is unusable in a UI: protojson's own error
// is a string that happens to contain a line and column, which cannot be
// rendered next to the offending field or highlighted in an editor. This type
// carries the path and the expected kind as data (ROADMAP.md §2.2.3: strictness
// surprises users, so the error must say exactly what was wrong).
type FieldTypeError struct {
	// Path is the dotted JSON path, with [i] for repeated elements, e.g.
	// "page.filter.limit".
	Path string
	// Field is the protobuf field name.
	Field string
	// Kind is the field's kind, when the failure is a kind mismatch.
	Kind protoreflect.Kind
	// Want describes the expected JSON type in prose.
	Want string
	// Got is the JSON type name that arrived instead.
	Got string
	// Msg carries the full explanation, including unknown-field errors.
	Msg string
}

// Error implements error.
func (e *FieldTypeError) Error() string {
	if e.Msg != "" {
		return "engine: grpc: " + e.Msg
	}
	if e.Field != "" {
		return fmt.Sprintf("engine: grpc: field %q (%s %s) expects %s, got JSON %s",
			e.Path, e.Kind, e.Field, e.Want, e.Got)
	}
	return fmt.Sprintf("engine: grpc: at %q: expected %s, got JSON %s", e.Path, e.Want, e.Got)
}

// JSONOptions configures the dynamic mapping layer.
type JSONOptions struct {
	// StrictUnknown rejects JSON fields absent from the descriptor. This is the
	// default because a silently dropped field is how a request "succeeded"
	// while doing something other than what the file said (ROADMAP.md §2.2.3).
	StrictUnknown bool
	// EmitUnpopulated renders zero-valued fields instead of omitting them.
	EmitUnpopulated bool
	// UseProtoNames accepts and emits proto field names rather than the
	// lowerCamelCase JSON names.
	UseProtoNames bool
}

// DefaultJSONOptions returns Pulse's defaults: strict, unpopulated fields
// omitted.
func DefaultJSONOptions() JSONOptions { return JSONOptions{StrictUnknown: true} }

// unmarshalOptions builds protojson's decode options.
func (o JSONOptions) unmarshalOptions() protojson.UnmarshalOptions {
	return protojson.UnmarshalOptions{DiscardUnknown: !o.StrictUnknown}
}

// marshalOptions builds protojson's encode options.
func (o JSONOptions) marshalOptions() protojson.MarshalOptions {
	return protojson.MarshalOptions{
		EmitUnpopulated: o.EmitUnpopulated,
		UseProtoNames:   o.UseProtoNames,
	}
}

// DecodeRequest turns a JSON body from a .http file into a DYNAMIC message
// built from the runtime descriptor. No generated type participates: the message
// is allocated by dynamicpb from a descriptor that arrived over the wire.
//
// The pre-validation pass is not redundant with protojson. protojson would
// reject the same payloads, but with an unstructured message; this pass turns
// the rejection into a *FieldTypeError carrying the field path, which is the
// difference between an error a user can act on and one they can only stare at.
func DecodeRequest(md protoreflect.MethodDescriptor, body []byte, opts JSONOptions) (*DynamicMessage, error) {
	msg := NewDynamicMessage(md.Input())
	if len(body) == 0 {
		return msg, nil
	}
	if err := ValidateJSONAgainstMessage(md.Input(), body, opts); err != nil {
		return nil, err
	}
	if err := opts.unmarshalOptions().Unmarshal(body, msg.Message()); err != nil {
		return nil, fmt.Errorf("engine: grpc: request body does not fit %s: %w", md.FullName(), err)
	}
	return msg, nil
}

// EncodeResponse renders a response message as canonical JSON.
//
// This is the binary→JSON chokepoint: whatever bytes reach the Sink are JSON,
// never protobuf wire format, so the UI, the store, and any downstream
// comparison never learn the wire type.
//
// NOTE on determinism: protojson deliberately emits unstable whitespace, so its
// output must NOT be compared byte-for-byte across runs. Tests and consumers
// compare decoded values; the Sink's chunking guarantees are about SIZE, not
// bytes.
func EncodeResponse(msg proto.Message, opts JSONOptions) ([]byte, error) {
	out, err := opts.marshalOptions().Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("engine: grpc: cannot render response as JSON: %w", err)
	}
	return out, nil
}

// ValidateJSONAgainstMessage type-checks a JSON payload against a descriptor
// BEFORE decoding, reporting the first mismatch as a *FieldTypeError.
func ValidateJSONAgainstMessage(md protoreflect.MessageDescriptor, body []byte, opts JSONOptions) error {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("engine: grpc: request body is not a JSON object: %w", err)
	}
	st := acquireMappingState()
	defer releaseMappingState(st)
	return walkJSON("", md, raw, st, opts)
}

// walkJSON type-checks one decoded JSON object against a message descriptor.
func walkJSON(path string, md protoreflect.MessageDescriptor, obj map[string]any, st *mappingState, opts JSONOptions) error {
	fields, ok := st.fields[md.FullName()]
	if !ok {
		fields = md.Fields()
		st.fields[md.FullName()] = fields
	}
	for key, val := range obj {
		fd := fields.ByJSONName(key)
		if fd == nil && opts.UseProtoNames {
			fd = fields.ByName(protoreflect.Name(key))
		}
		if fd == nil {
			if opts.StrictUnknown {
				return &FieldTypeError{
					Path:  joinPath(path, key),
					Field: key,
					Want:  fmt.Sprintf("a field of %s", md.FullName()),
					Got:   jsonTypeOf(val),
					Msg: fmt.Sprintf("field %q is not in %s; strict mapping rejects unknown fields",
						joinPath(path, key), md.FullName()),
				}
			}
			continue // permissive: protojson drops it
		}
		if err := checkValue(joinPath(path, key), fd, val, st, opts); err != nil {
			return err
		}
	}
	return nil
}

// checkValue type-checks one JSON value against one field descriptor. This is
// the dynamic type-resolution boundary: it decides, from a runtime descriptor,
// what a given JSON token is allowed to become.
func checkValue(path string, fd protoreflect.FieldDescriptor, val any, st *mappingState, opts JSONOptions) error {
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		switch v := val.(type) {
		case map[string]any:
			return walkJSON(path, fd.Message(), v, st, opts)
		case []any:
			// A repeated message field arrives as an array of objects.
			for i, elem := range v {
				obj, ok := elem.(map[string]any)
				if !ok {
					return &FieldTypeError{
						Path: fmt.Sprintf("%s[%d]", path, i), Field: string(fd.Name()),
						Kind: fd.Kind(), Want: "a repeated message object", Got: jsonTypeOf(elem),
					}
				}
				if err := walkJSON(fmt.Sprintf("%s[%d]", path, i), fd.Message(), obj, st, opts); err != nil {
					return err
				}
			}
			return nil
		default:
			return &FieldTypeError{
				Path: path, Field: string(fd.Name()), Kind: fd.Kind(),
				Want: "an object or array of objects", Got: jsonTypeOf(val),
			}
		}

	case protoreflect.EnumKind:
		// protojson accepts both names and numbers for enums, and so does this
		// pass (ROADMAP.md §2.2.3). Anything else is a genuine mismatch.
		switch val.(type) {
		case string, float64:
			return nil
		default:
			return &FieldTypeError{
				Path: path, Field: string(fd.Name()), Kind: fd.Kind(),
				Want: "an enum name or number", Got: jsonTypeOf(val),
			}
		}

	case protoreflect.StringKind:
		if _, ok := val.(string); !ok {
			return &FieldTypeError{
				Path: path, Field: string(fd.Name()), Kind: fd.Kind(),
				Want: "a string", Got: jsonTypeOf(val),
			}
		}
		return nil

	case protoreflect.BoolKind:
		if _, ok := val.(bool); !ok {
			return &FieldTypeError{
				Path: path, Field: string(fd.Name()), Kind: fd.Kind(),
				Want: "a boolean", Got: jsonTypeOf(val),
			}
		}
		return nil

	default:
		// Every numeric kind: int32, int64, uint32, uint64, sint*, fixed*,
		// sfixed*, float, double.
		switch v := val.(type) {
		case float64:
			return nil
		case string:
			// protojson renders 64-bit integers as JSON strings, so a quoted
			// number is legal for exactly the 64-bit kinds and illegal for the
			// 32-bit ones. This is the rule that makes `"limit": "3"` a clean
			// error against an int32 field instead of a silent zero.
			if isQuotable64(fd.Kind()) {
				return nil
			}
			return &FieldTypeError{
				Path: path, Field: string(fd.Name()), Kind: fd.Kind(),
				Want: "a JSON number",
				Got:  "string",
				Msg: fmt.Sprintf("field %q is %s; a quoted number is legal only for 64-bit kinds "+
					"(protojson renders int64/uint64 as strings), not for %s",
					path, fd.Kind(), fd.Kind()),
			}
		case nil:
			// JSON null leaves the field unset in proto3; protojson accepts it.
			return nil
		default:
			return &FieldTypeError{
				Path: path, Field: string(fd.Name()), Kind: fd.Kind(),
				Want: "a JSON number", Got: jsonTypeOf(v),
			}
		}
	}
}

// isQuotable64 reports whether protojson renders a kind as a JSON string.
func isQuotable64(k protoreflect.Kind) bool {
	switch k {
	case protoreflect.Int64Kind, protoreflect.Uint64Kind,
		protoreflect.Sint64Kind, protoreflect.Fixed64Kind, protoreflect.Sfixed64Kind:
		return true
	default:
		return false
	}
}

func jsonTypeOf(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// sortStrings is a tiny insertion sort: the lists here are descriptor import
// names, which are short and nearly always already ordered. Calling sort.Strings
// would work too, but this keeps the package's sort dependency at zero and makes
// the intent (deterministic error text) obvious.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
