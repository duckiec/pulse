// Package fuzz generates deterministic boundary payloads from an OpenAPI
// schema, executes them under a strict rate limit, and reports the result as
// Test Anything Protocol.
//
// # Determinism is the whole point
//
// A fuzzer whose corpus changes between runs cannot tell a new finding from a
// flaky one, and cannot be re-run to confirm a fix. Every payload here is a
// pure function of (schema, seed, index): no wall-clock, no global RNG, no map
// iteration order. Two runs of the same seed produce byte-identical output, and
// the TAP plan's test count is known before the first request is sent.
//
// # Safety rails, not optional extras
//
// Fuzzing a local server from an LLM is a foot-gun in both directions. It can
// hammer a target into the ground (hence the token bucket), and it can be
// pointed at something that is not the user's machine at all (hence the
// loopback-only target rule). Both are enforced structurally here rather than by
// the caller's judgement.
//
// # No dependency, deliberately
//
// ROADMAP.md §3.3.1 names `kin-openapi`, and §3.3 needs YAML. Both YAML modules
// are on the permanently-banned list (AGENTS.md §2.2: no third-party codecs where
// the standard library or a purpose-built parser will do), so this package
// carries its own parser for the YAML subset OpenAPI documents actually use.
// A subset parser that REFUSES what it does not understand is the correct shape
// here: a lenient parser that silently mis-nests a schema produces fuzz payloads
// for constraints that were never declared, which is worse than a clear error.
package fuzz

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// IR is the internal representation a loaded OpenAPI document is reduced to.
//
// It is deliberately much smaller than the OpenAPI specification. A fuzzer needs
// parameter constraints and responses; it does not need callbacks, links,
// security schemes, or vendor extensions, and carrying them would mean more code
// that can be wrong.
type IR struct {
	// Version is the declared `openapi:` value, e.g. "3.0.3".
	Version string
	// Ops are the operations, sorted by (path, method) so the IR — and
	// therefore every payload derived from it — is stable across runs.
	Ops []Op
}

// Op is one operation.
type Op struct {
	// ID is "METHOD path", e.g. "GET /users/{id}".
	ID     string
	Method string
	Path   string
	// Params are the path, query and header parameters, sorted by (in, name).
	Params []Param
	// Deprecated mirrors the operation's `deprecated` flag. It is read here
	// because the same projection serves contract-drift checks, where a
	// deprecated endpoint is a finding rather than a fuzzing target.
	Deprecated bool
	// RequestBody is the request body schema, or nil when the operation has none.
	RequestBody *Schema
	// Responses maps a status code (as written, e.g. "200", "default") to its
	// schema. Sorted iteration is enforced wherever this is read.
	Responses map[string]*Schema
}

// Param is one operation parameter.
type Param struct {
	// Name is the parameter name.
	Name string
	// In is "path", "query", "header" or "cookie".
	In string
	// Required mirrors the OpenAPI `required` flag.
	Required bool
	// Schema is the parameter's value schema.
	Schema *Schema
}

// Schema is the subset of JSON Schema that constrains a value.
//
// Nullable and Deprecated are carried because they change which boundaries are
// meaningful (a nullable integer's "below minimum" case is -1, not -2¹⁵).
type Schema struct {
	Type         string // "string", "integer", "number", "boolean", "array", "object"
	Format       string
	Enum         []any
	Const        any
	HasConst     bool
	Minimum      *float64
	Maximum      *float64
	ExclusiveMin bool
	ExclusiveMax bool
	MinLength    *int
	MaxLength    *int
	Pattern      string
	MinItems     *int
	MaxItems     *int
	UniqueItems  bool
	Items        *Schema
	Properties   map[string]*Schema
	Required     []string
	// Nullable mirrors OpenAPI 3.0's `nullable` and 3.1's `type: [..., "null"]`.
	Nullable bool
}

// ErrRemoteRef is returned for any `$ref` carrying a URI scheme.
//
// Pulse is offline-first (invariant I-3). A remote `$ref` would be a network
// fetch triggered by a document the user merely pointed at, so it is refused
// rather than attempted — and it is refused LOUDLY, because a document whose
// schema silently failed to resolve produces payloads for constraints nobody
// declared.
type ErrRemoteRef struct{ Ref string }

func (e *ErrRemoteRef) Error() string {
	return "fuzz: remote $ref refused (Pulse is offline): " + e.Ref
}

// ErrRefCycle marks a `$ref` that resolves back to itself.
//
// It is a distinct error from a missing ref because the two mean different
// things to a user: a cycle is a property of the document, and a missing
// component is a typo or a truncated file.
type ErrRefCycle struct{ Ref string }

func (e *ErrRefCycle) Error() string { return "fuzz: $ref cycle: " + e.Ref }

// ErrUnsupportedDocument marks input that is not a usable OpenAPI document.
type ErrUnsupportedDocument struct{ Why string }

func (e *ErrUnsupportedDocument) Error() string {
	return "fuzz: not a usable OpenAPI document: " + e.Why
}

// LoadOpenAPI reads and reduces a local OpenAPI v3 document (JSON or YAML).
//
// It never touches the network. A `$ref` with a scheme is refused
// (ErrRemoteRef) and a cycle is refused (ErrRefCycle); both are typed so a
// caller can distinguish "this document is bad" from "this machine cannot
// reach the file".
func LoadOpenAPI(path string) (*IR, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fuzz: read %s: %w", path, err)
	}
	return ParseOpenAPI(raw)
}

// ParseOpenAPI reduces an in-memory OpenAPI document.
func ParseOpenAPI(raw []byte) (*IR, error) {
	root, err := parseDocument(raw)
	if err != nil {
		return nil, err
	}
	m, ok := root.(map[string]any)
	if !ok {
		return nil, &ErrUnsupportedDocument{Why: "the document root is not a mapping"}
	}
	ver, _ := m["openapi"].(string)
	if !strings.HasPrefix(ver, "3.") {
		return nil, &ErrUnsupportedDocument{
			Why: fmt.Sprintf("openapi version %q is not 3.x", ver),
		}
	}
	if _, ok := m["paths"]; !ok {
		return nil, &ErrUnsupportedDocument{Why: "the document has no `paths`"}
	}

	r := &resolver{doc: m}
	ir := &IR{Version: ver}

	paths, _ := m["paths"].(map[string]any)
	for _, p := range sortedKeys(paths) {
		item, ok := paths[p].(map[string]any)
		if !ok {
			continue
		}
		for _, method := range sortedKeys(item) {
			if !isHTTPMethod(method) {
				continue
			}
			opMap, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			op := Op{Method: strings.ToUpper(method), Path: p, ID: strings.ToUpper(method) + " " + p}
			op.Deprecated, _ = opMap["deprecated"].(bool)
			op.Params = r.params(opMap, p)
			if rb, ok := opMap["requestBody"]; ok {
				op.RequestBody = r.bodySchema(rb)
			}
			op.Responses = r.responses(opMap)
			ir.Ops = append(ir.Ops, op)
		}
	}
	sort.Slice(ir.Ops, func(i, j int) bool {
		if ir.Ops[i].Path != ir.Ops[j].Path {
			return ir.Ops[i].Path < ir.Ops[j].Path
		}
		return ir.Ops[i].Method < ir.Ops[j].Method
	})

	// A $ref we could not follow fails the WHOLE load, however much of the
	// document parsed cleanly. Returning the partial IR would produce a corpus
	// that silently omits whatever lived behind the broken ref — a run that
	// reports itself as complete and is not.
	if r.fatal != nil {
		return nil, r.fatal
	}
	return ir, nil
}

// isHTTPMethod reports whether a path-item key names an operation rather than a
// sibling field like `parameters` or `summary`. Getting this wrong is how a
// fuzzer ends up fuzzing the word "description".
func isHTTPMethod(k string) bool {
	switch strings.ToUpper(k) {
	case "GET", "PUT", "POST", "DELETE", "OPTIONS", "HEAD", "PATCH", "TRACE":
		return true
	}
	return false
}

// resolver holds the document for $ref resolution.
//
// fatal is a STICKY error, and that is the point. A resolver that dropped a
// $ref it could not follow would carry on and emit payloads for constraints
// that were never declared — so a document containing a remote or a cyclic ref
// would produce a confident, WRONG corpus instead of a refusal. The first hard
// failure is recorded and the load fails, however much of the rest of the
// document parsed cleanly.
type resolver struct {
	doc   map[string]any
	fatal error
}

// fail records the first hard error. Later ones are ignored: the caller sees the
// root cause rather than a pile of its consequences.
func (r *resolver) fail(err error) {
	if err != nil && r.fatal == nil {
		r.fatal = err
	}
}

// maxRefDepth bounds resolution. A document nesting deeper than this while
// resolving is pathological rather than deep, and the bound turns an unbounded
// recursion into a typed error.
const maxRefDepth = 32

// deref follows local $refs, refusing remote ones and detecting cycles.
//
// The visited set is per-CALL, not global: two sibling fields legitimately
// pointing at the same component is normal reuse, and a global set would reject
// it. Only a ref that points back into its own resolution chain is a cycle.
func (r *resolver) deref(node any, seen []string) (any, error) {
	for depth := 0; depth < maxRefDepth; depth++ {
		m, ok := node.(map[string]any)
		if !ok {
			return node, nil
		}
		refAny, has := m["$ref"]
		if !has {
			return node, nil
		}
		ref, ok := refAny.(string)
		if !ok {
			return nil, &ErrUnsupportedDocument{Why: "$ref is not a string"}
		}
		if hasScheme(ref) {
			return nil, &ErrRemoteRef{Ref: ref}
		}
		for _, s := range seen {
			if s == ref {
				return nil, &ErrRefCycle{Ref: ref}
			}
		}
		target, err := r.resolve(ref)
		if err != nil {
			return nil, err
		}
		node = target
		seen = append(seen, ref)
	}
	return nil, &ErrUnsupportedDocument{Why: "$ref nested deeper than " + strconv.Itoa(maxRefDepth)}
}

// hasScheme reports whether a ref is remote. A bare `#/components/...` is local;
// anything with `scheme://` or a `file:` prefix is not.
//
// The check is on the SCHEME and not on the presence of `//`, because
// `#/components/schemas/X` contains slashes too and treating those as remote
// would refuse every legitimate local ref.
func hasScheme(ref string) bool {
	i := strings.Index(ref, ":")
	if i <= 0 {
		return false
	}
	scheme := ref[:i]
	for _, r := range scheme {
		isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		isDigit := r >= '0' && r <= '9'
		if !isAlpha && !isDigit && r != '+' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

// resolve walks a local JSON pointer.
func (r *resolver) resolve(ref string) (any, error) {
	const prefix = "#/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, &ErrUnsupportedDocument{Why: "unsupported $ref target " + ref}
	}
	var cur any = r.doc
	for _, seg := range strings.Split(strings.TrimPrefix(ref, prefix), "/") {
		// RFC 6901 escaping: `~1` is `/` and `~0` is `~`, in that order.
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, &ErrUnsupportedDocument{Why: "$ref " + ref + " does not resolve"}
		}
		cur, ok = m[seg]
		if !ok {
			return nil, &ErrUnsupportedDocument{Why: "$ref " + ref + " does not resolve"}
		}
	}
	return cur, nil
}

// params collects path, query, header and cookie parameters for one operation,
// including any shared ones declared on the path item.
func (r *resolver) params(op map[string]any, path string) []Param {
	var out []Param
	seen := map[string]bool{}

	add := func(list any) {
		arr, ok := list.([]any)
		if !ok {
			return
		}
		for _, it := range arr {
			pm, ok := it.(map[string]any)
			if !ok {
				continue
			}
			resolved, err := r.deref(pm, nil)
			if err != nil {
				// A $ref that cannot be followed is FATAL, not skippable. This
				// used to `continue`, which meant a document whose only
				// constraint lived behind a remote ref produced a corpus that
				// silently omitted it — the exact confident-and-wrong outcome
				// this package exists to avoid.
				r.fail(err)
				return
			}
			pm, ok = resolved.(map[string]any)
			if !ok {
				continue
			}
			name, _ := pm["name"].(string)
			in, _ := pm["in"].(string)
			if name == "" || in == "" {
				continue
			}
			key := in + ":" + name
			if seen[key] {
				continue
			}
			seen[key] = true
			req, _ := pm["required"].(bool)
			sc, err := r.schema(pm["schema"])
			if err != nil {
				r.fail(err)
				continue
			}
			out = append(out, Param{Name: name, In: in, Required: req, Schema: sc})
		}
	}

	add(op["parameters"])
	// Path-item-level parameters are legal and common; an operation-level
	// declaration with the same (in, name) wins, which is what `seen` gives us
	// by adding the operation's own first.
	if owner, ok := r.ownerOfPathItem(path); ok {
		add(owner)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].In != out[j].In {
			return out[i].In < out[j].In
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ownerOfPathItem finds the path item so its shared `parameters` can be read.
func (r *resolver) ownerOfPathItem(path string) (map[string]any, bool) {
	paths, ok := r.doc["paths"].(map[string]any)
	if !ok {
		return nil, false
	}
	item, ok := paths[path].(map[string]any)
	return item, ok
}

// bodySchema extracts a request body's media-type schema, preferring JSON.
func (r *resolver) bodySchema(body any) *Schema {
	bm, ok := body.(map[string]any)
	if !ok {
		return nil
	}
	content, ok := bm["content"].(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"application/json", "text/json"} {
		if m, ok := content[key].(map[string]any); ok {
			sc, err := r.schema(m["schema"])
			if err != nil {
				r.fail(err)
				return nil
			}
			return sc
		}
	}
	// Fall back to whatever media type is declared, deterministically.
	for _, k := range sortedKeys(content) {
		m, ok := content[k].(map[string]any)
		if !ok {
			continue
		}
		sc, err := r.schema(m["schema"])
		if err != nil {
			r.fail(err)
			continue
		}
		return sc
	}
	return nil
}

// responses maps declared status codes to schemas.
func (r *resolver) responses(op map[string]any) map[string]*Schema {
	out := map[string]*Schema{}
	rm, ok := op["responses"].(map[string]any)
	if !ok {
		return out
	}
	for _, code := range sortedKeys(rm) {
		resolved, err := r.deref(rm[code], nil)
		if err != nil {
			r.fail(err)
			continue
		}
		sc := r.contentSchema(resolved)
		if sc != nil {
			out[code] = sc
		}
	}
	return out
}

// contentSchema picks the JSON schema out of a media-type object.
func (r *resolver) contentSchema(node any) *Schema {
	m, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	content, ok := m["content"].(map[string]any)
	if !ok {
		return nil
	}
	for _, key := range []string{"application/json", "text/json"} {
		if cm, ok := content[key].(map[string]any); ok {
			sc, err := r.schema(cm["schema"])
			if err != nil {
				r.fail(err)
				return nil
			}
			return sc
		}
	}
	for _, k := range sortedKeys(content) {
		cm, ok := content[k].(map[string]any)
		if !ok {
			continue
		}
		sc, err := r.schema(cm["schema"])
		if err != nil {
			r.fail(err)
			continue
		}
		return sc
	}
	return nil
}

// schema converts a raw schema node, resolving refs and normalising the 3.0 /
// 3.1 spellings of nullability.
func (r *resolver) schema(node any) (*Schema, error) {
	resolved, err := r.deref(node, nil)
	if err != nil {
		return nil, err
	}
	m, ok := resolved.(map[string]any)
	if !ok {
		return nil, &ErrUnsupportedDocument{Why: "a schema is not a mapping"}
	}

	s := &Schema{}
	switch t := m["type"].(type) {
	case string:
		s.Type = t
		s.Nullable = t == "null"
	case []any:
		// OpenAPI 3.1: `type: [string, "null"]`.
		for _, it := range t {
			ts, ok := it.(string)
			if !ok {
				continue
			}
			if ts == "null" {
				s.Nullable = true
				continue
			}
			if s.Type == "" {
				s.Type = ts
			}
		}
	}
	if s.Type == "" {
		// 3.1 lets the type be implied by the presence of keywords.
		switch {
		case m["properties"] != nil:
			s.Type = "object"
		case m["items"] != nil:
			s.Type = "array"
		default:
			s.Type = "string"
		}
	}
	if n, ok := m["nullable"].(bool); ok && n {
		s.Nullable = true
	}
	s.Format, _ = m["format"].(string)
	if p, ok := m["pattern"].(string); ok {
		s.Pattern = p
	}
	s.UniqueItems, _ = m["uniqueItems"].(bool)

	if v, ok := m["const"]; ok {
		s.Const, s.HasConst = v, true
	}
	if e, ok := m["enum"].([]any); ok {
		s.Enum = append([]any(nil), e...)
	}

	s.Minimum = numPtr(m["minimum"])
	s.Maximum = numPtr(m["maximum"])
	// 3.0 spells exclusivity as `exclusiveMinimum: true` beside the bound;
	// 3.1 spells it as the bound itself. Both are honoured.
	switch v := m["exclusiveMinimum"].(type) {
	case bool:
		s.ExclusiveMin = v
	case float64:
		s.Minimum = &v
		s.ExclusiveMin = true
	}
	switch v := m["exclusiveMaximum"].(type) {
	case bool:
		s.ExclusiveMax = v
	case float64:
		s.Maximum = &v
		s.ExclusiveMax = true
	}
	s.MinLength = intPtr(m["minLength"])
	s.MaxLength = intPtr(m["maxLength"])
	s.MinItems = intPtr(m["minItems"])
	s.MaxItems = intPtr(m["maxItems"])

	if it, ok := m["items"]; ok {
		sub, err := r.schema(it)
		if err != nil {
			r.fail(err)
			return nil, err
		}
		s.Items = sub
	}
	if props, ok := m["properties"].(map[string]any); ok {
		s.Properties = make(map[string]*Schema, len(props))
		for _, name := range sortedKeys(props) {
			sub, err := r.schema(props[name])
			if err != nil {
				// One unresolvable property must fail the whole load. Returning
				// early with a partial property set is the confident-and-wrong
				// failure: the corpus would omit the constraint and the run would
				// still report itself as covering the operation.
				r.fail(err)
				return nil, err
			}
			s.Properties[name] = sub
		}
	}
	if req, ok := m["required"].([]any); ok {
		for _, v := range req {
			if name, ok := v.(string); ok {
				s.Required = append(s.Required, name)
			}
		}
		sort.Strings(s.Required)
	}
	return s, nil
}

// PropertyNames returns an object's property names in sorted order.
//
// It is exported because a caller that must BUILD a valid body has to walk the
// same set the generator walked, and doing that with its own `for k := range`
// is how a rendered request starts varying with map iteration order.
func PropertyNames(s *Schema) []string {
	if s == nil {
		return nil
	}
	return sortedKeys(s.Properties)
}

// Op returns the operation with the given id, or nil.
func (ir *IR) Op(id string) *Op {
	for i := range ir.Ops {
		if ir.Ops[i].ID == id {
			return &ir.Ops[i]
		}
	}
	return nil
}

// SortedResponses returns the response codes in sorted order, so iteration over
// a map never leaks into the payload stream.
func (op *Op) SortedResponses() []string {
	out := make([]string, 0, len(op.Responses))
	for k := range op.Responses {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func numPtr(v any) *float64 {
	switch t := v.(type) {
	case float64:
		return &t
	case int:
		f := float64(t)
		return &f
	case int64:
		f := float64(t)
		return &f
	}
	return nil
}

func intPtr(v any) *int {
	switch t := v.(type) {
	case float64:
		i := int(t)
		return &i
	case int:
		return &t
	case int64:
		i := int(t)
		return &i
	}
	return nil
}

// sortedKeys returns a map's keys in sorted order.
//
// Go randomises map iteration, and every map walk in this package goes through
// here. That is not tidiness: a fuzzer that emitted payloads in map order would
// produce a different corpus on every run, and determinism is the package's
// entire claim.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// compilePattern compiles a schema's `pattern`, returning a typed error for an
// invalid expression.
//
// An invalid pattern is REFUSED rather than skipped: silently dropping it means
// fuzzing a string field with no knowledge of its shape, which produces noise
// and reports it as coverage.
func compilePattern(p string) (*regexp.Regexp, error) {
	if p == "" {
		return nil, nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, fmt.Errorf("fuzz: schema pattern %q does not compile: %w", p, err)
	}
	return re, nil
}
