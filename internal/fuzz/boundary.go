package fuzz

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Strategy selects which family of payloads is generated.
type Strategy uint8

const (
	// Boundary probes the declared edges: minimum-1, maximum+1, length limits,
	// empty and maximum-length strings. This is the strategy that finds
	// off-by-one bugs, and it is the one that derives entirely from the schema
	// rather than from anything the fuzzer invents.
	Boundary Strategy = iota
	// TypeConfusion sends a value of the wrong JSON type — a number where a
	// string is declared, null where an object is required. Servers that
	// coerce rather than validate are the usual source of these bugs.
	TypeConfusion
	// Encoding sends text that is well-formed JSON but hostile to the decoder:
	// astral-plane characters, lone surrogates, control characters, overlong
	// forms, and JSON-breaking payloads.
	Encoding
	// Oversize sends a single field at 1 MiB, which is where most
	// read-then-parse handlers fall over rather than streaming.
	Oversize
	// Injection sends string-shaped probes for injection and traversal classes.
	// They are inert data — this package never issues a request itself, it only
	// builds payloads for the caller's engine to send.
	Injection
)

// StrategyCount is the number of defined strategies.
const StrategyCount = 5

// String names a strategy.
func (s Strategy) String() string {
	switch s {
	case Boundary:
		return "boundary"
	case TypeConfusion:
		return "type-confusion"
	case Encoding:
		return "encoding"
	case Oversize:
		return "oversize"
	case Injection:
		return "injection"
	}
	return "unknown"
}

// OversizeBytes is the payload size the Oversize strategy targets.
const OversizeBytes = 1 << 20 // 1 MiB

// DefaultStringLength is the length used when a schema declares no maxLength
// but the Oversize strategy wants a big string.
//
// It is deliberately below OversizeBytes: the Oversize strategy's whole point
// is ONE field at 1 MiB, and having every other string in the payload also at
// 1 MiB would turn a precise probe into a bandwidth test.
const DefaultStringLength = 256

// Payload is one generated probe.
//
// Index is stable and is what a TAP test number is derived from, so re-running
// the same corpus produces the same numbering and a diff between two runs is
// meaningful.
type Payload struct {
	// Index is the payload's position in the generated corpus.
	Index int
	// OpID is the operation this payload targets.
	OpID string
	// Strategy is the family that produced it.
	Strategy Strategy
	// Field names the parameter or body property under test. Empty for a
	// whole-body payload.
	Field string
	// Location is "query", "path", "header" or "body".
	Location string
	// Value is the value substituted into Field.
	Value any
	// Note is a short human-readable label, e.g. "maxLength+1".
	Note string
}

// noBudgetLimit is the internal ceiling used when the caller passes no budget.
//
// It is large enough never to bite a real corpus and small enough that a
// pathological schema cannot generate an unbounded slice. A budget of zero means
// "no limit", NOT "generate nothing": the empty result would be
// indistinguishable from an operation with no fuzzable parameters, which is a
// silence the caller cannot act on.
const noBudgetLimit = 100_000

// Generate builds the deterministic boundary matrix for one operation.
//
// The result is a pure function of (op, seed, budget): no clock, no global RNG,
// and every map is iterated in sorted order. Two calls with the same arguments
// produce byte-identical payloads, which is what makes a fuzz finding
// reproducible and re-runnable to confirm a fix.
//
// The seed only ever drives CHOICE among values that are equally valid for the
// declared schema — never the shape of the matrix. That way the coverage of
// declared constraints does not vary with the seed; only which extra values
// appear alongside them.
func Generate(op *Op, seed uint64, budget int) []Payload {
	if op == nil {
		return nil
	}
	if budget <= 0 {
		budget = noBudgetLimit
	}
	// A large, well-mixed corpus: each parameter contributes a handful of
	// boundary cases, and every property is covered at least once.
	g := &gen{op: op, seed: seed, rng: newSplitmix(seed)}

	var out []Payload
	for _, p := range op.Params {
		out = append(out, g.forParam(p)...)
	}
	if op.RequestBody != nil {
		out = append(out, g.forBody(op.RequestBody)...)
	}

	// Stable order: by field, then location, then note, then the rendered
	// value. Sorting makes the corpus independent of the order the schema
	// happened to be walked in, which is the difference between "reproducible"
	// and "reproducible if the map iteration cooperates".
	sort.SliceStable(out, func(i, j int) bool { return payloadKey(out[i]) < payloadKey(out[j]) })

	// Re-index so the numbering is dense and stable regardless of what was
	// dropped by the budget.
	for i := range out {
		out[i].Index = i
	}
	if len(out) > budget {
		// Truncation is even-strided rather than a prefix: a prefix would only
		// ever exercise the first parameter alphabetically, so a budget of 10
		// on a 50-payload corpus would never touch the last one.
		out = stride(out, budget)
		for i := range out {
			out[i].Index = i
		}
	}
	return out
}

// payloadKey is the total order over payloads.
func payloadKey(p Payload) string {
	return p.Field + "\x00" + p.Location + "\x00" + p.Note + "\x00" + render(p.Value)
}

// stride selects budget items spread across the corpus.
func stride(in []Payload, budget int) []Payload {
	if budget >= len(in) || budget <= 0 {
		return in
	}
	out := make([]Payload, 0, budget)
	step := float64(len(in)) / float64(budget)
	for i := 0; i < budget; i++ {
		out = append(out, in[int(float64(i)*step)])
	}
	return out
}

// gen carries the per-call generator state.
type gen struct {
	op   *Op
	seed uint64
	rng  *splitmix64
}

// forParam produces payloads for one parameter.
func (g *gen) forParam(p Param) []Payload {
	if p.Schema == nil {
		return nil
	}
	vals := g.valuesFor(p.Schema, g.rng.next())
	var out []Payload
	for _, v := range vals {
		out = append(out, Payload{
			OpID:     g.op.ID,
			Strategy: v.strategy,
			Field:    p.Name,
			Location: p.In,
			Value:    v.value,
			Note:     v.note,
		})
	}
	return out
}

// forBody produces payloads for the request body: one per property, plus a
// whole-body type-confusion pair.
func (g *gen) forBody(s *Schema) []Payload {
	var out []Payload
	for _, name := range sortedKeys(s.Properties) {
		sub := s.Properties[name]
		if sub == nil {
			continue
		}
		for _, v := range g.valuesFor(sub, g.rng.next()) {
			out = append(out, Payload{
				OpID:     g.op.ID,
				Strategy: v.strategy,
				Field:    name,
				Location: "body",
				Value:    v.value,
				Note:     v.note,
			})
		}
	}
	if s.Type != "object" {
		// A body that is not an object still gets whole-value probes.
		for _, v := range g.valuesFor(s, g.rng.next()) {
			out = append(out, Payload{
				OpID:     g.op.ID,
				Strategy: v.strategy,
				Location: "body",
				Value:    v.value,
				Note:     v.note,
			})
		}
	}
	return out
}

// candidate is one value with the strategy and label that produced it.
type candidate struct {
	strategy Strategy
	note     string
	value    any
}

// valuesFor enumerates the boundary values a schema's declared constraints imply.
//
// The ORDER of this list is the coverage order, and it is deliberate: the cases
// derived from the schema's own declared limits come first, because those are
// the ones a server got wrong. The trailing generic cases exist so a schema
// with no declared constraints is still probed at all — a field with only
// `type: string` is the most common thing to find a bug in.
func (g *gen) valuesFor(s *Schema, seed uint64) []candidate {
	var out []candidate
	add := func(str Strategy, note string, v any) {
		out = append(out, candidate{strategy: str, note: note, value: v})
	}

	switch s.Type {
	case "integer", "number":
		add(Boundary, "zero", 0)
		add(Boundary, "one", 1)
		add(Boundary, "negative-one", -1)
		add(Boundary, "large", int64(1)<<53)
		add(Boundary, "negative-large", -int64(1)<<53)
		if s.Minimum != nil {
			lo := *s.Minimum
			if s.ExclusiveMin {
				add(Boundary, "minExclusive", lo+1)
				add(Boundary, "min-1", lo-1)
			} else {
				add(Boundary, "min", lo)
				add(Boundary, "min-1", lo-1)
			}
		}
		if s.Maximum != nil {
			hi := *s.Maximum
			if s.ExclusiveMax {
				add(Boundary, "maxExclusive", hi-1)
				add(Boundary, "max+1", hi+1)
			} else {
				add(Boundary, "max", hi)
				add(Boundary, "max+1", hi+1)
			}
		}
		if f := s.Format; f != "" {
			if f == "int32" {
				add(Boundary, "int32-overflow", int64(1)<<31)
				add(Boundary, "int32-underflow", -int64(1)<<31)
			}
			if f == "int64" {
				add(Boundary, "float-precision-loss", int64(1)<<53)
			}
		}
		if len(s.Enum) > 0 {
			for _, e := range s.Enum {
				add(Boundary, "enum:"+render(e), e)
			}
			add(TypeConfusion, "enum-miss", "__pulse_not_an_enum_member__")
		}

	case "string":
		add(Boundary, "empty", "")
		add(Boundary, "single-char", "a")
		add(Boundary, "space", " ")
		add(Boundary, "string", "pulse")
		if s.MinLength != nil {
			add(Boundary, "minLength-1", strings.Repeat("a", max(0, *s.MinLength-1)))
			add(Boundary, "minLength", strings.Repeat("a", *s.MinLength))
		}
		if s.MaxLength != nil {
			add(Boundary, "maxLength", strings.Repeat("a", *s.MaxLength))
			add(Boundary, "maxLength+1", strings.Repeat("a", *s.MaxLength+1))
		}
		if s.Pattern != "" {
			if re, err := compilePattern(s.Pattern); err == nil {
				add(Boundary, "pattern-nonmatch", invert(re))
				add(Encoding, "pattern-match", s.Pattern)
			}
		}
		if s.Format == "email" {
			add(Boundary, "email-nonmatch", "not-an-email")
		}
		if s.Format == "uuid" {
			add(Boundary, "uuid-nonmatch", "not-a-uuid")
		}
		if s.Format == "date-time" {
			add(Boundary, "datetime-nonmatch", "not-a-datetime")
		}
		if len(s.Enum) > 0 {
			for _, e := range s.Enum {
				add(Boundary, "enum:"+render(e), e)
			}
			add(TypeConfusion, "enum-miss", "__pulse_not_an_enum_member__")
		}

	case "boolean":
		add(Boundary, "true", true)
		add(Boundary, "false", false)
		add(TypeConfusion, "bool-as-number", 1)
		add(TypeConfusion, "bool-as-string", "true")

	case "array":
		add(Boundary, "empty-array", []any{})
		if s.MinItems != nil {
			add(Boundary, "minItems-1", fill(*s.MinItems-1, s))
			add(Boundary, "minItems", fill(*s.MinItems, s))
		}
		if s.MaxItems != nil {
			add(Boundary, "maxItems", fill(*s.MaxItems, s))
			add(Boundary, "maxItems+1", fill(*s.MaxItems+1, s))
		}
		if s.UniqueItems {
			add(Boundary, "uniqueItems-violated", []any{"dup", "dup"})
		}
		add(Boundary, "one-item", fill(1, s))

	case "object":
		// A valid object built from the required properties is the control
		// case; without it every object probe is invalid and a handler that
		// only rejects invalid input is never shown to work.
		add(Boundary, "valid-object", objectFor(s, false))
		if len(s.Required) > 0 {
			add(Boundary, "missing-required", objectFor(s, true))
		}
	}

	// Type confusion is added for every type: sending the wrong JSON type is
	// how a handler that trusts its own validation gets caught.
	switch s.Type {
	case "string":
		add(TypeConfusion, "string-as-number", 1)
		add(TypeConfusion, "string-as-bool", true)
		add(TypeConfusion, "string-as-null", nil)
		add(TypeConfusion, "string-as-array", []any{"a"})
		add(TypeConfusion, "string-as-object", map[string]any{"a": 1})
	case "integer", "number":
		add(TypeConfusion, "number-as-string", "1")
		add(TypeConfusion, "number-as-bool", true)
		add(TypeConfusion, "number-as-null", nil)
		add(TypeConfusion, "number-as-array", []any{1})
	case "array":
		add(TypeConfusion, "array-as-string", "[]")
		add(TypeConfusion, "array-as-object", map[string]any{})
		add(TypeConfusion, "array-as-null", nil)
	case "object":
		add(TypeConfusion, "object-as-string", "{}")
		add(TypeConfusion, "object-as-array", []any{})
		add(TypeConfusion, "object-as-null", nil)
	}

	// Encoding probes apply to anything that ends up as text.
	switch s.Type {
	case "string":
		add(Encoding, "unicode-astral", "\U0001F600")
		add(Encoding, "unicode-control", "a\x00b\x1fc")
		add(Encoding, "unicode-bidi", "a‮b") // RTL override: a display attack
		add(Encoding, "overlong-utf8", string([]byte{0xC0, 0xAF}))
		add(Encoding, "lone-surrogate", string([]byte{0xED, 0xA0, 0x80}))
		add(Encoding, "json-breakout", `","injected":"yes`)
		add(Encoding, "base64-blob", "QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo=")
		add(Oversize, "oversize-1MiB", strings.Repeat("x", OversizeBytes))
	case "integer", "number":
		add(Encoding, "number-as-overflow-string", "1e400")
		add(Encoding, "number-as-nan-string", "NaN")
	case "array":
		add(Oversize, "oversize-array", fill(1024, s))
	}

	// Injection probes are plain strings. They are inert data: this package
	// builds payloads, it never sends one, and the target is loopback-only.
	if s.Type == "string" {
		add(Injection, "sqli-or-true", "' OR '1'='1")
		add(Injection, "sqli-tautology", "1' OR 1=1--")
		add(Injection, "nosql-ne", `{"$ne": null}`)
		add(Injection, "path-traversal", "../../../../etc/passwd")
		add(Injection, "ssrf-file", "file:///etc/passwd")
		add(Injection, "ssrf-metadata", "http://169.254.169.254/latest/meta-data/")
		add(Injection, "null-byte", "a\x00b")
		add(Injection, "template", "{{7*7}}")
	}

	if s.Nullable {
		add(TypeConfusion, "null", nil)
	}
	if s.HasConst {
		add(Boundary, "const", s.Const)
		add(Boundary, "const-violated", "__pulse_not_the_const__")
	}

	return out
}

// fill builds an array of n elements drawn from the item schema.
func fill(n int, s *Schema) []any {
	if n < 0 {
		n = 0
	}
	if n > OversizeBytes {
		n = OversizeBytes
	}
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, seedValue(s, uint64(i)+1))
	}
	return out
}

// ValidValue returns a value that satisfies the schema's declared constraints,
// for use where the field under test is NOT the one being varied.
//
// It is exported because building a request requires a value for EVERY declared
// parameter, not just the one under test: a request to `GET /users/{id}?verbose`
// must still supply `verbose` when the payload varies `id`. Generating those
// values anywhere other than beside the boundary logic is how a request ends up
// accidentally malformed in a way that has nothing to do with the probe.
func ValidValue(s *Schema) any { return seedValue(s, 1) }

// seedValue produces a valid-ish value for a schema.
func seedValue(s *Schema, seed uint64) any {
	if s == nil {
		return nil
	}
	switch s.Type {
	case "integer", "number":
		if s.Minimum != nil {
			return *s.Minimum
		}
		return float64(seed)
	case "string":
		return "s" + strconv.FormatUint(seed, 10)
	case "boolean":
		return seed%2 == 0
	case "array":
		return []any{}
	case "object":
		return objectFor(s, false)
	}
	return nil
}

// objectFor builds an object from a schema, optionally omitting required
// properties (which is how the "missing required" probe is made).
func objectFor(s *Schema, dropRequired bool) map[string]any {
	out := map[string]any{}
	for _, name := range sortedKeys(s.Properties) {
		if dropRequired && containsStr(s.Required, name) {
			continue
		}
		out[name] = seedValue(s.Properties[name], 1)
	}
	return out
}

// invert returns a short string that does NOT match a compiled pattern.
//
// The probe alphabet is tried in order of increasing "unusualness", because the
// point is to reach a validator's fallback path: an empty string is what most
// validators check first, and a NUL byte is what almost none admit. When every
// candidate matches — a pattern like `.*` or `[ -~]*` — the NUL fallback is
// still returned, and a pattern that DOES match it is a schema bug worth
// finding anyway.
func invert(re *regexp.Regexp) string {
	candidates := []string{
		"", "\x00", " ", "\n", "\t", "a", "pulse-probe", "pulse_probe", "~",
	}
	for _, c := range candidates {
		if !re.MatchString(c) {
			return c
		}
	}
	return "\x00"
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// render converts a payload value to a stable string for notes and ordering.
func render(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		// Long strings are elided so the ordering key stays bounded; the payload
		// itself is never truncated.
		if len(t) > 32 {
			return fmt.Sprintf("str[%d]%s", len(t), t[:8])
		}
		return strconv.Quote(t)
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	case []any:
		return fmt.Sprintf("array[%d]", len(t))
	case map[string]any:
		return "object{" + strings.Join(sortedKeys(t), ",") + "}"
	}
	return fmt.Sprintf("%v", v)
}

// splitmix64 is the generator. It is used only to CHOOSE among equally valid
// values; the corpus shape never depends on it.
type splitmix64 struct{ state uint64 }

func newSplitmix(seed uint64) *splitmix64 { return &splitmix64{state: seed} }

func (s *splitmix64) next() uint64 {
	s.state += 0x9E3779B97F4A7C15
	z := s.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}
