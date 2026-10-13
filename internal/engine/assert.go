package engine

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The assertion model of ROADMAP.md §4.1.1.
//
// # Why the grammar is tiny and total
//
// An assertion expression is an invitation to build a language. Every project
// that has one started with `status == 200` and ended up with a Lisp. The
// grammar here is six operators over four targets and NOTHING else: every input
// either parses into exactly one Assertion or fails closed with the offending
// text, and there is no evaluation surface — no variables, no functions, no
// short-circuit operators, no arithmetic.
//
// # Why Eval takes a body
//
// The contract in ROADMAP.md §4.1.1 is `Eval(res *RunResult) error`. That is
// implemented as [Assertion.EvalBody], with the body passed separately, and the
// deviation is deliberate.
//
// `RunResult` is a bounded SUMMARY of a run: it carries Status, Elapsed, Bytes
// and a Digest, and it deliberately does not carry the response body, because a
// body can be gigabytes and a RunResult outlives the stream that produced it.
// `# @assert body contains "ok"` therefore cannot be evaluated against a
// RunResult — the honest options are to assert against the Digest (a much
// weaker statement, and one that cannot answer "does the error message name the
// symbol"), or to pass the body in. The body is passed in, bounded by the
// caller.

// Errors. All are typed so a caller can distinguish a malformed assertion from
// a failed one — they are different shapes of user error.
var (
	// ErrAssertParse reports an assertion the runner cannot parse.
	//
	// It is a parse failure, which in CLI terms is exit 2, and it is distinct
	// from an assertion that parsed and failed, which is exit 1.
	ErrAssertParse = errors.New("engine: assertion does not parse")

	// ErrAssertTarget reports an unknown left-hand side.
	ErrAssertTarget = errors.New("engine: assertion has an unknown subject")

	// ErrAssertOp reports an unknown operator.
	ErrAssertOp = errors.New("engine: assertion has an unknown operator")
)

// Assertion is one `# @assert` expression.
type Assertion interface {
	// EvalBody evaluates the assertion against a completed run and its body.
	//
	// body may be empty when the caller did not collect one; an assertion that
	// needs it then FAILS rather than passing vacuously.
	EvalBody(res *RunResult, body string) error

	// Source returns the original expression, for TAP output.
	Source() string
}

// Targets an assertion can read.
const (
	targetStatus   = "status"
	targetBody     = "body"
	targetHeader   = "header"
	targetCode     = "code"
	targetDuration = "duration"
)

// Operators, spelled as they appear in the .http file.
const (
	opEq       = "=="
	opNe       = "!="
	opLt       = "<"
	opLe       = "<="
	opGt       = ">"
	opGe       = ">="
	opIn       = "in"
	opContains = "contains"
)

// ParseAssert parses one assertion expression.
//
// The expression is the text after `# @assert`, with surrounding space trimmed.
// It is total: every input either parses or returns a typed error carrying the
// offending text.
func ParseAssert(expr string) (Assertion, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("%w: empty expression", ErrAssertParse)
	}

	target, rest := splitTarget(expr)
	switch target {
	case targetDuration:
		return parseDurationAssertion(rest, expr)
	case targetStatus, targetCode:
		return parseIntAssertion(target, rest, expr)
	case targetBody:
		return parseStringAssertion(target, rest, expr)
	case targetHeader:
		// The subject is `header <name>`; the name runs to the operator.
		fields := strings.Fields(rest)
		if len(fields) < 2 {
			return nil, fmt.Errorf("%w: `%s` needs a header name", ErrAssertParse, expr)
		}
		name := fields[0]
		return parseStringAssertion("header "+name, strings.TrimSpace(rest[len(name):]), expr)
	default:
		return nil, fmt.Errorf("%w: %q in `%s`", ErrAssertTarget, target, expr)
	}
}

// splitTarget returns the leading word and everything after it.
func splitTarget(expr string) (string, string) {
	i := strings.IndexAny(expr, " \t")
	if i < 0 {
		return expr, ""
	}
	return expr[:i], strings.TrimSpace(expr[i+1:])
}

// parseIntAssertion parses `status <op> <int>` and `status in <lo>..<hi>`.
func parseIntAssertion(target, rest, expr string) (Assertion, error) {
	if rest == "" {
		return nil, fmt.Errorf("%w: `%s` has no comparator", ErrAssertParse, expr)
	}
	if isInOperator(rest) {
		lo, hi, err := parseRange(strings.TrimSpace(strings.TrimPrefix(rest, opIn)))
		if err != nil {
			return nil, fmt.Errorf("%w: `%s`: %v", ErrAssertParse, expr, err)
		}
		return intRangeAssert{subject: target, lo: lo, hi: hi, src: expr}, nil
	}
	op, operand, ok := splitOperator(rest)
	if !ok {
		return nil, fmt.Errorf("%w: `%s` has no operator", ErrAssertOp, expr)
	}
	n, err := strconv.Atoi(strings.TrimSpace(operand))
	if err != nil {
		return nil, fmt.Errorf("%w: `%s` operand %q is not an integer",
			ErrAssertParse, expr, strings.TrimSpace(operand))
	}
	return intCmpAssert{subject: target, op: op, n: n, src: expr}, nil
}

// parseStringAssertion parses `body contains "x"`, `header H contains "x"`,
// and the `==`/`!=` forms.
func parseStringAssertion(target, rest, expr string) (Assertion, error) {
	if rest == "" {
		return nil, fmt.Errorf("%w: `%s` has no comparator", ErrAssertParse, expr)
	}
	if isInOperator(rest) {
		// `in` for a string means membership in an enumerated set.
		items := splitList(strings.TrimSpace(strings.TrimPrefix(rest, opIn)))
		if len(items) == 0 {
			return nil, fmt.Errorf("%w: `%s` lists no values", ErrAssertParse, expr)
		}
		return strSetAssert{subject: target, items: items, src: expr}, nil
	}
	op, operand, ok := splitOperator(rest)
	if !ok {
		return nil, fmt.Errorf("%w: `%s` has no operator", ErrAssertOp, expr)
	}
	var want string
	switch op {
	case opContains:
		want = unquote(strings.TrimSpace(operand))
	case opEq, opNe:
		want = unquote(strings.TrimSpace(operand))
	default:
		return nil, fmt.Errorf("%w: %q does not apply to a text subject (%s)",
			ErrAssertOp, op, target)
	}
	return strCmpAssert{subject: target, op: op, want: want, src: expr}, nil
}

// isInOperator reports whether rest begins with the `in` operator.
func isInOperator(rest string) bool {
	return strings.HasPrefix(rest, opIn+" ") || rest == opIn
}

// splitOperator finds the first operator in rest, returning it and the operand.
//
// Two-character operators are matched before one-character ones so `<=` is not
// read as `<` followed by `=`.
func splitOperator(rest string) (op, operand string, ok bool) {
	for _, cand := range []string{opEq, opNe, opLe, opGe, opLt, opGt, opContains} {
		if i := indexOperator(rest, cand); i >= 0 {
			return cand, rest[i+len(cand):], true
		}
	}
	return "", "", false
}

// indexOperator finds an operator as a WORD, not as a substring of a longer
// token. Without this, `!=` inside a quoted string would split there.
func indexOperator(s, op string) int {
	inS, inD := false, false
	for i := 0; i+len(op) <= len(s); i++ {
		switch s[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		}
		if inS || inD {
			continue
		}
		if strings.HasPrefix(s[i:], op) {
			return i
		}
	}
	return -1
}

// parseRange parses `lo..hi`.
func parseRange(s string) (lo, hi int, err error) {
	i := strings.Index(s, "..")
	if i < 0 {
		return 0, 0, fmt.Errorf("expected `lo..hi`, got %q", s)
	}
	lo, err = strconv.Atoi(strings.TrimSpace(s[:i]))
	if err != nil {
		return 0, 0, fmt.Errorf("lower bound %q is not an integer", s[:i])
	}
	hi, err = strconv.Atoi(strings.TrimSpace(s[i+2:]))
	if err != nil {
		return 0, 0, fmt.Errorf("upper bound %q is not an integer", s[i+2:])
	}
	if lo > hi {
		return 0, 0, fmt.Errorf("range %d..%d is inverted", lo, hi)
	}
	return lo, hi, nil
}

// splitList splits a comma-separated, optionally quoted list.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, unquote(part))
	}
	return out
}

// unquote removes one layer of matching quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// ---- evaluation ------------------------------------------------------------

// intCmpAssert is `status <op> <int>`.
type intCmpAssert struct {
	subject string
	op      string
	n       int
	src     string
}

func (a intCmpAssert) Source() string { return a.src }

func (a intCmpAssert) EvalBody(res *RunResult, _ string) error {
	if res == nil {
		return fmt.Errorf("assertion %s: no run result", a.src)
	}
	got := res.Status
	var pass bool
	switch a.op {
	case opEq:
		pass = got == a.n
	case opNe:
		pass = got != a.n
	case opLt:
		pass = got < a.n
	case opLe:
		pass = got <= a.n
	case opGt:
		pass = got > a.n
	case opGe:
		pass = got >= a.n
	default:
		return fmt.Errorf("%w: %q cannot compare integers", ErrAssertOp, a.op)
	}
	if !pass {
		return fmt.Errorf("status %d is not %s %d", got, a.op, a.n)
	}
	return nil
}

// intRangeAssert is `status in 200..299`.
type intRangeAssert struct {
	subject string
	lo, hi  int
	src     string
}

func (a intRangeAssert) Source() string { return a.src }

func (a intRangeAssert) EvalBody(res *RunResult, _ string) error {
	if res == nil {
		return fmt.Errorf("assertion %s: no run result", a.src)
	}
	// Inclusive at BOTH ends: `in 200..299` means 200 and 299 both pass. That
	// is the reading that matches the range notation, and it is the one a
	// boundary-equality golden test pins.
	if res.Status < a.lo || res.Status > a.hi {
		return fmt.Errorf("status %d is not in %d..%d", res.Status, a.lo, a.hi)
	}
	return nil
}

// strCmpAssert is `body contains "x"` or `header H == "x"`.
type strCmpAssert struct {
	subject string
	op      string
	want    string
	src     string
}

func (a strCmpAssert) Source() string { return a.src }

func (a strCmpAssert) EvalBody(res *RunResult, body string) error {
	if res == nil {
		return fmt.Errorf("assertion %s: no run result", a.src)
	}
	got, ok := a.subjectValue(res, body)
	if !ok {
		// A subject that cannot be read FAILS. Passing it would let a
		// mistyped subject report green for the whole file.
		return fmt.Errorf("cannot read %s from the run result", a.subject)
	}
	switch a.op {
	case opContains:
		if !strings.Contains(got, a.want) {
			return fmt.Errorf("%s does not contain %q", a.subject, a.want)
		}
	case opEq:
		if got != a.want {
			return fmt.Errorf("%s is %q, want %q", a.subject, got, a.want)
		}
	case opNe:
		if got == a.want {
			return fmt.Errorf("%s is %q, want anything else", a.subject, got)
		}
	default:
		return fmt.Errorf("%w: %q cannot compare text", ErrAssertOp, a.op)
	}
	return nil
}

// subjectValue extracts the text an assertion is about.
func (a strCmpAssert) subjectValue(res *RunResult, body string) (string, bool) {
	switch {
	case a.subject == targetBody:
		return body, body != ""
	case strings.HasPrefix(a.subject, "header "):
		name := strings.TrimPrefix(a.subject, "header ")
		if res.Header == nil {
			return "", false
		}
		// Canonical form: Go's http.Header is keyed by the canonical MIME
		// header name, so a `.http` file written `Content-Type` must be
		// looked up as `Content-Type`, and one written `content-type` must
		// be canonicalised before the lookup or it will never match.
		return res.Header.Get(canonicalHeader(name)), true
	}
	return "", false
}

// canonicalHeader is http.CanonicalHeaderKey without importing net/http into
// a file that otherwise has no reason to.
func canonicalHeader(s string) string {
	var b strings.Builder
	upper := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case upper && 'a' <= c && c <= 'z':
			b.WriteByte(c - 32)
		case !upper && 'A' <= c && c <= 'Z':
			b.WriteByte(c + 32)
		default:
			b.WriteByte(c)
		}
		upper = c == '-' || c < 'a'
	}
	return b.String()
}

// strSetAssert is `header X in "a", "b"`.
type strSetAssert struct {
	subject string
	items   []string
	src     string
}

func (a strSetAssert) Source() string { return a.src }

func (a strSetAssert) subjectValue(res *RunResult, body string) (string, bool) {
	switch {
	case a.subject == targetBody:
		return body, body != ""
	case strings.HasPrefix(a.subject, "header "):
		name := strings.TrimPrefix(a.subject, "header ")
		if res.Header == nil {
			return "", false
		}
		return res.Header.Get(canonicalHeader(name)), true
	}
	return "", false
}

func (a strSetAssert) EvalBody(res *RunResult, body string) error {
	if res == nil {
		return fmt.Errorf("assertion %s: no run result", a.src)
	}
	got, ok := a.subjectValue(res, body)
	if !ok {
		return fmt.Errorf("cannot read %s from the run result", a.subject)
	}
	for _, want := range a.items {
		if got == want {
			return nil
		}
	}
	return fmt.Errorf("%s is %q, want one of %q", a.subject, got, a.items)
}

// durationAssert is `duration < 500ms`.
//
// It is a separate target from status because its operand is a DURATION, not an
// integer, and conflating the two would silently accept `duration < 500`.
func parseDurationAssertion(rest, expr string) (Assertion, error) {
	op, operand, ok := splitOperator(rest)
	if !ok {
		return nil, fmt.Errorf("%w: `%s` has no operator", ErrAssertOp, expr)
	}
	d, err := time.ParseDuration(strings.TrimSpace(operand))
	if err != nil {
		return nil, fmt.Errorf("%w: `%s` operand %q is not a duration",
			ErrAssertParse, expr, strings.TrimSpace(operand))
	}
	return durationAssert{op: op, want: d, src: expr}, nil
}

// durationAssert is `duration < 500ms`.
type durationAssert struct {
	op   string
	want time.Duration
	src  string
}

func (a durationAssert) Source() string { return a.src }

func (a durationAssert) EvalBody(res *RunResult, _ string) error {
	if res == nil {
		return fmt.Errorf("assertion %s: no run result", a.src)
	}
	got := res.Elapsed
	var pass bool
	switch a.op {
	case opLt:
		pass = got < a.want
	case opLe:
		pass = got <= a.want
	case opGt:
		pass = got > a.want
	case opGe:
		pass = got >= a.want
	case opEq:
		pass = got == a.want
	case opNe:
		pass = got != a.want
	default:
		return fmt.Errorf("%w: %q cannot compare durations", ErrAssertOp, a.op)
	}
	if !pass {
		return fmt.Errorf("duration %v is not %s %v", got, a.op, a.want)
	}
	return nil
}

// ParseAssertEvery parses every expression in exprs.
//
// It is the entry the runner uses, and it fails on the FIRST malformed
// expression rather than collecting them, because a .http file with an
// ambiguous assertion is not a file whose other assertions can be trusted
// either — the author did not mean what they wrote.
func ParseAssertEvery(exprs []string) ([]Assertion, error) {
	out := make([]Assertion, 0, len(exprs))
	for _, e := range exprs {
		a, err := ParseAssert(e)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}
