package mcp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"pulse/internal/ast"
	"pulse/internal/engine"
	"pulse/internal/fuzz"
)

// EngineFuzzRunner dispatches fuzz payloads through internal/engine.
//
// # Why the engine, and not a bare http.Client
//
// Pulse already has one transport: it applies variable expansion, redaction,
// per-run deadlines, teardown budgeting and bounded response reads. A fuzz
// runner built on a raw http.Client would be a SECOND transport with none of
// those, and the difference shows up exactly where it hurts most — a 1 MiB
// oversize probe answered with 1 GiB of JSON would be read into memory by the
// fuzzer, while the engine's bounded reader refuses it.
//
// # Why a synthetic .http document per payload
//
// The engine executes requests from a parsed ast.Document, and a Document's
// request list is unexported: the only way to obtain one is to parse text. So
// each payload is rendered to `.http` syntax and parsed. That is not a
// workaround — it is the point. The probe travels the identical path a user's
// hand-written request does, including the parts most likely to be broken.
//
// # Why this type owns its engine construction
//
// See [roundTripResolver]. Binding it is a correctness requirement of the
// renderer, so the runner constructs its own engines rather than accepting an
// injected factory: an injected factory is one more thing that can be wired
// without the resolver, and a fuzz run that silently fails to deliver half its
// corpus reports itself as complete. Callers needing exotic engine
// configuration implement [FuzzRunner] themselves; the three knobs below cover
// the ones that actually matter here.
type EngineFuzzRunner struct {
	// Redactor scrubs resolved secrets out of anything the run reports.
	// Optional; nil means header-name redaction only.
	Redactor engine.Redactor
	// UserAgent is sent on every probe. Empty means the engine default.
	UserAgent string
	// HTTPClient supplies the transport. Nil means the engine default.
	HTTPClient *http.Client
	// Timeout bounds each individual run. Zero means DefaultFuzzTimeout.
	Timeout time.Duration
}

// DefaultFuzzTimeout bounds a single fuzz request.
//
// It is generous for a loopback target and still finite: a payload that hangs a
// handler must cost the run one timeout, not the whole run. There is no "no
// timeout" option — an unbounded probe turns a boundary fuzzer into a hang
// detector that never reports anything.
const DefaultFuzzTimeout = 10 * time.Second

// NewEngineFuzzRunner returns a runner with the default configuration.
func NewEngineFuzzRunner(red engine.Redactor) *EngineFuzzRunner {
	return &EngineFuzzRunner{Redactor: red, Timeout: DefaultFuzzTimeout}
}

// engine builds the engine for one payload.
//
// A fresh engine per payload is deliberate. The Document's bytes live in an
// arena that is released when this call returns, so an engine outliving the call
// would hold a reference into pooled memory.
func (r *EngineFuzzRunner) engine(doc *ast.Document) *engine.Engine {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultFuzzTimeout
	}
	opts := []engine.Option{
		engine.WithTimeout(timeout),
		// See fuzzTransport. This is not a tuning choice; it is what makes the
		// loopback guarantee and the rate guarantee true.
		engine.WithHTTPClient(&http.Client{Transport: fuzzTransport()}),
	}
	if r.UserAgent != "" {
		opts = append(opts, engine.WithUserAgent(r.UserAgent))
	}
	if r.HTTPClient != nil {
		opts = append(opts, engine.WithHTTPClient(r.HTTPClient))
	}
	if r.Redactor != nil {
		opts = append(opts, engine.WithRedactor(r.Redactor))
	}
	// The round-trip resolver goes on LAST and unconditionally: it is what makes
	// the payload reach the target byte-for-byte.
	opts = append(opts, engine.WithResolver(roundTripResolver{}))
	return engine.New(doc, opts...)
}

// fuzzTransport is the transport every probe uses unless a caller overrides it.
//
// Both of its settings correct a way the DEFAULT transport would quietly break
// this tool's two promises.
//
// # DisableKeepAlives: one payload, one request
//
// net/http TRANSPARENTLY RETRIES an idempotent request when it reuses a
// connection the server has already closed. That retry is invisible to the
// caller, so a `GET` probe can arrive at the target twice for one payload. It
// breaks two things at once: the run's request count stops matching what was
// dispatched, and — the part that matters — the target receives MORE requests
// than the rate limiter ever scheduled, so a ceiling proven against the limiter
// no longer describes the traffic the server sees.
//
// Disabling keep-alives removes the pooled-connection race entirely. Against
// loopback the extra handshake costs microseconds, which is irrelevant next to
// the rate limit every probe waits for anyway.
//
// # Proxy: nil: "loopback" has to mean loopback
//
// http.DefaultTransport uses ProxyFromEnvironment, so an `HTTP_PROXY` in the
// environment sends `http://127.0.0.1:8080` to a proxy host that is NOT
// loopback — through which the request is no longer local, and to which the
// full payload, injection probes included, is handed. The loopback check in
// ValidateFuzzOptions would then be a promise about a URL rather than about the
// traffic. Pulse is offline-first (invariant I-3); honouring a proxy environment
// is a phone-home this tool must not perform.
//
// TLS is configured with InsecureSkipVerify on purpose and ONLY because the
// target is loopback and self-signed by nature: a developer fuzzing
// `https://localhost:8443` has a certificate for a name that will never validate
// in the wild, and refusing to test it would be the fuzzer's decision to make
// about the user's own server.
func fuzzTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DisableKeepAlives:     true,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // loopback-only; see above
		MaxIdleConns:          1,
		DisableCompression:    false,
		MaxConnsPerHost:       4,
		ResponseHeaderTimeout: DefaultFuzzTimeout,
	}
}

// roundTripResolver turns interpolation into a byte-exact identity.
//
// # The problem
//
// The engine expands `{{name}}` in URLs, headers and bodies. A fuzz corpus
// deliberately contains template-injection probes — `{{7*7}}` is one of them —
// and they would be consumed by Pulse's own expander before reaching the target.
// The run would then report a 4xx-shaped failure that says nothing about the
// server, and the injection strategy would be untested while appearing to have
// run.
//
// # Why not just leave the resolver nil
//
// Nil makes the engine FAIL CLOSED on `{{`, which is correct for a hand-written
// request and wrong here. The difference is who authored the braces: in a `.http`
// file they are the user's placeholders, and failing closed stops a request
// going out with a literal `{{token}}`. In a fuzz probe they are ATTACK DATA,
// and the whole point is to deliver them intact.
//
// # Why returning the original text works
//
// The expander calls Lookup(name) and writes the result in place of `{{name}}`.
// Returning `"{{" + name + "}}"` therefore reproduces the input exactly, for
// every input: `{{7*7}}` → `{{7*7}}`, `a{{x}}b{{y}}` → `a{{x}}b{{y}}`. It is a true
// identity, not an approximation.
//
// # The one input it does not survive
//
// An ODD number of `{{` anchors — a payload containing `{{` with no matching
// `}}` — makes the expander report an unterminated anchor and the run fails with
// a diagnostic that names the line. That is a real, reported outcome rather than
// a silent corruption, and no resolver-based approach can do better: the engine
// offers no escape syntax and no literal mode.
type roundTripResolver struct{}

// Lookup returns the original span unchanged.
func (roundTripResolver) Lookup(name string) (string, error) {
	return "{{" + name + "}}", nil
}

// errProbeUndeliverable marks a payload the HTTP transport cannot carry AT ALL.
//
// It exists because "the probe never left Pulse" and "the server mishandled the
// probe" are opposite verdicts, and a runner that returns the same error for both
// reports the first as the second. That is not a cosmetic problem: an
// undeliverable probe reported as a finding makes a correct server look broken,
// and a user who has learned that the tool cries wolf stops reading it.
//
// The case that produces it in practice is a header-boundary payload carrying a
// NUL or a C0 control byte. Those cannot appear in an HTTP header value by
// construction (RFC 7230 §3.2), so the client refuses to encode them. The probe
// is SKIPPED with this reason attached, which keeps the TAP plan complete and the
// verdict honest.
var errProbeUndeliverable = errors.New("fuzz: probe cannot be encoded for the HTTP transport")

// Run renders, parses and executes one payload, returning the HTTP status.
//
// A non-nil error means no response was produced. An error wrapping
// [errProbeUndeliverable] means the probe never left Pulse at all, which the
// caller distinguishes from a server verdict.
func (r *EngineFuzzRunner) Run(ctx context.Context, req FuzzRequest) (int, error) {
	src, err := renderFuzzRequest(req)
	if err != nil {
		return 0, err
	}

	arena := ast.AcquireArena()
	arena.Reset([]byte(src))
	doc, err := ast.Parse(arena)
	if err != nil {
		arena.Release()
		// A payload the AST cannot express is a bug in the RENDERER, not in the
		// target. Naming it as such stops a defect in this file from being
		// reported as a defect in the user's API.
		return 0, fmt.Errorf("fuzz: rendered probe is not a valid .http document: %w", err)
	}

	eng := r.engine(doc)
	// Close before Release, and Release only after the run has unwound: the
	// arena holds every byte the in-flight request reads.
	defer func() {
		_ = eng.Close()
		arena.Release()
	}()

	var requests []*ast.Request
	for rq := range doc.Requests() {
		requests = append(requests, rq)
	}
	if len(requests) != 1 {
		return 0, fmt.Errorf("fuzz: rendered %d requests, want exactly 1", len(requests))
	}

	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultFuzzTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sink := &statusSink{done: make(chan struct{})}
	handle := eng.Run(runCtx, requests[0], sink)
	// The deadline gives the run a reason to stop; the teardown budget bounds how
	// long we wait for it to actually do so. Without the second, a handler that
	// ignores cancellation hangs the entire fuzz run.
	_ = handle.WaitTimeout(engineTeardownBudget)

	select {
	case <-sink.done:
	case <-runCtx.Done():
		return 0, runCtx.Err()
	}
	if sink.err != nil {
		return 0, sink.err
	}
	return sink.status, nil
}

// statusSink captures the status of a run and discards the body.
//
// Discarding is deliberate: a fuzz verdict is decided by the STATUS, and a 1 MiB
// probe that provokes a 1 GiB response must not be buffered by the fuzzer to
// reach that verdict. The engine's bounded reader still exercises the streaming
// path, which is half of what the probe was for.
type statusSink struct {
	done   chan struct{}
	status int
	err    error
}

func (s *statusSink) Chunk(*engine.Chunk) {}

func (s *statusSink) Done(res *engine.RunResult) {
	s.status = res.Status
	close(s.done)
}

func (s *statusSink) Failed(err error) {
	s.err = err
	close(s.done)
}

// skipHeaders are parameters a fuzz probe must not set.
//
// Go's http.Client either computes or rejects these itself, so a payload that
// varied them would surface a client-side rejection as though it were the
// server's verdict — a false finding that describes the probe, not the target.
var skipHeaders = map[string]bool{
	"host":              true,
	"content-length":    true,
	"transfer-encoding": true,
	"connection":        true,
}

// renderFuzzRequest builds the `.http` document for one payload.
//
// Every declared parameter is given a value: the one under test gets the
// payload, and every other gets a schema-valid default. A request that omits a
// declared parameter tests the wrong thing — the server would reject it for the
// omission, identically for every probe, and the boundary under test would never
// be reached.
func renderFuzzRequest(req FuzzRequest) (string, error) {
	op := req.Op
	if op == nil {
		return "", fmt.Errorf("fuzz: payload %d names operation %q, which is not in the document",
			req.Payload.Index, req.Payload.OpID)
	}
	target := req.Payload
	base := strings.TrimRight(req.BaseURL, "/")

	path, err := expandFuzzPath(op, target)
	if err != nil {
		return "", err
	}
	query := fuzzQuery(op, target)

	method := op.Method
	if method == "" {
		method = "GET"
	}
	targetURL := base + path
	if query != "" {
		targetURL += "?" + query
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# @name fuzz-%d\n", target.Index)
	fmt.Fprintf(&b, "%s %s\n", method, targetURL)
	b.WriteString("Content-Type: application/json\n")
	headers, err := fuzzHeaders(op, target)
	if err != nil {
		return "", err
	}
	for _, h := range headers {
		fmt.Fprintf(&b, "%s: %s\n", h.name, h.value)
	}

	body, err := fuzzBody(op, target)
	if err != nil {
		return "", err
	}
	if body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	b.WriteString("\n###\n")
	return b.String(), nil
}

// expandFuzzPath substitutes every `{name}` in the operation's path template.
func expandFuzzPath(op *fuzz.Op, target fuzz.Payload) (string, error) {
	out := op.Path
	// A `{name}` scan rather than a regexp: it runs once per payload, and a
	// regexp here would be a compile per call for no gain.
	for i := 0; i < len(out); {
		open := strings.IndexByte(out[i:], '{')
		if open < 0 {
			break
		}
		open += i
		end := strings.IndexByte(out[open:], '}')
		if end < 0 {
			return "", fmt.Errorf("fuzz: path %q has an unterminated {", op.Path)
		}
		end += open
		name := out[open+1 : end]

		val, found := fuzz.ValidValue(nil), false
		for _, p := range op.Params {
			if p.In != "path" || p.Name != name {
				continue
			}
			if target.Location == "path" && target.Field == name {
				val, found = target.Value, true
			} else {
				val, found = fuzz.ValidValue(p.Schema), true
			}
			break
		}
		if !found {
			// A template variable with no declared parameter is a document bug.
			// Substituting a placeholder would send a request that could not
			// succeed and then blame the server for rejecting it.
			return "", fmt.Errorf("fuzz: path variable %q in %q has no declared parameter",
				name, op.Path)
		}
		out = out[:open] + fuzzEscapePath(fuzzScalar(val)) + out[end+1:]
		i = open + 1
	}
	return out, nil
}

// fuzzEscapePath percent-encodes the bytes a URL path cannot carry literally.
//
// It deliberately does NOT use url.PathEscape, which is the obvious choice and
// the wrong one. PathEscape encodes `/`, so a path-traversal probe
// (`../../etc/passwd`) would arrive as one segment `..%2F..%2Fetc%2Fpasswd` and
// the server's router would never see a traversal at all. The traversal strategy
// would report itself as run while testing nothing.
//
// So `/` stays literal — that is the probe — and only the bytes that cannot
// appear in a URL are encoded: C0 controls, space, DEL, and the delimiters `#`,
// `?` and `%` (the last because an unencoded one would be read as the start of
// an escape). After the server URL-decodes, it receives the original byte, which
// is what the probe meant.
func fuzzEscapePath(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x21 && c <= 0x7E && c != '#' && c != '?' && c != '%' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// fuzzQuery builds the query string.
//
// Parameters are emitted in sorted order so the rendered request is byte-stable,
// which is what lets two runs of the same seed be compared byte for byte.
func fuzzQuery(op *fuzz.Op, target fuzz.Payload) string {
	var parts []string
	for _, p := range op.Params {
		if p.In != "query" {
			continue
		}
		val := fuzz.ValidValue(p.Schema)
		if target.Location == "query" && target.Field == p.Name {
			val = target.Value
		}
		if val == nil {
			// A null query parameter is dropped rather than sent as the string
			// "null": `?a=null` is a VALUE, and turning an intended null into a
			// literal would test the wrong thing while looking deliberate.
			continue
		}
		parts = append(parts, url.QueryEscape(p.Name)+"="+url.QueryEscape(fuzzScalar(val)))
	}
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

// fuzzHeader is one rendered header line.
type fuzzHeader struct{ name, value string }

// fuzzHeaders renders header parameters, skipping the ones the client owns.
//
// A rendered value that the transport cannot encode makes the whole probe
// undeliverable, which is reported rather than silently dropped or — worse —
// shipped anyway and rejected by the client with an error that reads like a
// server verdict.
func fuzzHeaders(op *fuzz.Op, target fuzz.Payload) ([]fuzzHeader, error) {
	var out []fuzzHeader
	for _, p := range op.Params {
		if p.In != "header" || skipHeaders[strings.ToLower(p.Name)] {
			continue
		}
		val := fuzz.ValidValue(p.Schema)
		if target.Location == "header" && target.Field == p.Name {
			val = target.Value
		}
		if val == nil {
			continue
		}
		raw := fuzzScalar(val)
		if !validHeaderFieldValue(raw) {
			return nil, fmt.Errorf("%w: header %q carries a byte RFC 7230 forbids "+
				"in a field value (%s)", errProbeUndeliverable, p.Name,
				describeUnencodable(raw))
		}
		out = append(out, fuzzHeader{name: p.Name, value: raw})
	}
	return out, nil
}

// validHeaderFieldValue reports whether s is a legal HTTP field value.
//
// RFC 7230 §3.2 defines field-value as field-vchar / SP / HTAB, where field-vchar
// is VCHAR / obs-text. Go's client enforces the same rule, so this check is
// stating a fact the transport would otherwise state as a late failure — and
// failing here lets the probe be SKIPPED with a reason instead of reported as a
// finding.
func validHeaderFieldValue(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\t':
		case c == ' ':
		case c >= 0x21 && c <= 0x7E:
		case c >= 0x80: // obs-text: permitted but not interpreted
		default:
			return false
		}
	}
	return true
}

// describeUnencodable names the offending bytes, so a skipped probe says which
// byte blocked it rather than merely that something did.
func describeUnencodable(s string) string {
	var bad []string
	seen := map[string]bool{}
	for i := 0; i < len(s) && len(bad) < 4; i++ {
		c := s[i]
		if c == '\t' || c == ' ' || (c >= 0x21 && c <= 0x7E) || c >= 0x80 {
			continue
		}
		token := fmt.Sprintf("%#02x", c)
		if !seen[token] {
			seen[token] = true
			bad = append(bad, token)
		}
	}
	if len(bad) == 0 {
		return "none"
	}
	return strings.Join(bad, ", ")
}

// fuzzBody renders the JSON request body.
//
// A probe that varies one property leaves every other property at a valid value,
// so the request differs from a working one by exactly the thing under test. A
// probe with no field — the whole-body strategies — replaces the body entirely.
func fuzzBody(op *fuzz.Op, target fuzz.Payload) (string, error) {
	s := op.RequestBody
	if s == nil || target.Location != "body" {
		return "", nil
	}

	var val any
	if target.Field == "" {
		val = target.Value
	} else {
		if s.Properties == nil || s.Properties[target.Field] == nil {
			return "", fmt.Errorf("fuzz: body property %q is not declared on %s",
				target.Field, op.ID)
		}
		obj := map[string]any{}
		for _, name := range fuzz.PropertyNames(s) {
			obj[name] = fuzz.ValidValue(s.Properties[name])
		}
		obj[target.Field] = target.Value
		val = obj
	}

	raw, err := json.Marshal(val)
	if err != nil {
		return "", fmt.Errorf("fuzz: payload %d does not marshal to JSON: %w", target.Index, err)
	}
	return string(raw), nil
}

// fuzzScalar renders a value for a position where only a string fits: a path
// segment, a query value, or a header value.
//
// It is total — every payload type has a rendering — because a probe that could
// not be rendered would silently shrink the corpus, and a shrunken corpus
// reports itself as complete.
func fuzzScalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		// Integral floats render without a decimal point so a path segment reads
		// as `/users/1`, not `/users/1.0`. A server matching on the string form
		// would 404 on the second and the probe would read as a finding that is
		// really a rendering artefact.
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case int64:
		return strconv.FormatInt(t, 10)
	case int:
		return strconv.Itoa(t)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}
