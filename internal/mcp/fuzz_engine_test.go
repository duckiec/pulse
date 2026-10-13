package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pulse/internal/fuzz"
)

// capture records what a fuzz probe actually put on the wire.
//
// The gate reads the SERVER'S view, not the renderer's output. A renderer that
// produces a plausible document while dropping or mangling the probe would pass
// a test that only compares strings against itself.
type capture struct {
	mu     sync.Mutex
	hits   []capturedReq
	server *httptest.Server
}

type capturedReq struct {
	Method string
	Path   string
	Query  string
	Body   string
	Header http.Header
}

func newCapture(t *testing.T, status func(capturedReq) int) *capture {
	t.Helper()
	c := &capture{}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		got := capturedReq{
			Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Body: string(body), Header: r.Header.Clone(),
		}
		c.mu.Lock()
		c.hits = append(c.hits, got)
		c.mu.Unlock()
		st := 200
		if status != nil {
			st = status(got)
		}
		w.WriteHeader(st)
	}))
	t.Cleanup(c.server.Close)
	return c
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.hits)
}

func (c *capture) all() []capturedReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]capturedReq, len(c.hits))
	copy(out, c.hits)
	return out
}

func writeSpec(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "api.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write schema: %v", err)
	}
	return p
}

const pathParamSpec = `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /users/{id}:
    get:
      parameters:
        - name: id
          in: path
          required: true
          schema: {type: integer, minimum: 1, maximum: 1000}
        - name: verbose
          in: query
          schema: {type: boolean}
        - name: X-Trace
          in: header
          schema: {type: string, maxLength: 8}
      responses:
        '200': {description: ok}
`

// TestEngineRunnerSendsExactlyOneRequestPerPayload is the rate-guarantee gate.
//
// net/http transparently RETRIES an idempotent request that reuses a connection
// the server has already closed. The retry is invisible to the caller, so the
// target receives more requests than the limiter ever scheduled — and a ceiling
// proven against the limiter then describes traffic that is not happening. This
// gate is where that shows up: the server counts requests, not payloads.
func TestEngineRunnerSendsExactlyOneRequestPerPayload(t *testing.T) {
	cap := newCapture(t, nil)
	ir, err := fuzz.LoadOpenAPI(writeSpec(t, fuzzSchema))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	op := ir.Op("POST /users")
	payloads := fuzz.Generate(op, 9, 0)

	runner := newEngineRunner()
	for _, p := range payloads {
		if _, err := runner.Run(context.Background(),
			FuzzRequest{BaseURL: cap.server.URL, Op: op, Payload: p}); err != nil {
			t.Fatalf("payload %d (%s): %v", p.Index, p.Note, err)
		}
	}

	if got, want := cap.count(), len(payloads); got != want {
		t.Errorf("the server received %d requests for %d payloads; a transparent "+
			"transport retry inflates the traffic above the rate the limiter "+
			"enforced", got, want)
	}
	t.Logf("%d payloads -> %d requests at the server", len(payloads), cap.count())
}

// TestFuzzTransportIgnoresTheProxyEnvironment proves the loopback guarantee is
// about TRAFFIC and not just about a URL string.
//
// http.DefaultTransport honours HTTP_PROXY, so a target spelled
// `http://127.0.0.1:8080` would be sent to a proxy host that is not loopback —
// carrying every payload, injection probes included, off the machine. A proxy
// consulted here would also be an outbound request, which invariant I-3 forbids.
func TestFuzzTransportIgnoresTheProxyEnvironment(t *testing.T) {
	tr := fuzzTransport()
	if tr.Proxy != nil {
		t.Error("the fuzz transport installs a Proxy function; an HTTP_PROXY in " +
			"the environment would carry every payload off the machine")
	}

	// Behavioural half: with a proxy environment set, a probe must still land on
	// the loopback sink and nowhere else.
	proxyHit := make(chan struct{}, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case proxyHit <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)

	cap := newCapture(t, nil)
	ir, err := fuzz.LoadOpenAPI(writeSpec(t, fuzzSchema))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	op := ir.Op("POST /users")
	if _, err := newEngineRunner().Run(context.Background(), FuzzRequest{
		BaseURL: cap.server.URL, Op: op,
		Payload: fuzz.Payload{Index: 0, OpID: op.ID, Location: "body",
			Field: "name", Note: "probe", Value: "secret-payload"},
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cap.count() != 1 {
		t.Errorf("the loopback sink saw %d requests, want 1", cap.count())
	}
	select {
	case <-proxyHit:
		t.Error("a probe was routed through HTTP_PROXY; the payload left the machine")
	default:
	}
	t.Log("the probe reached the loopback sink and never touched the proxy")
}

// TestEngineRunnerDeliversThePayloadVerbatim is the delivery gate.
//
// It is the answer to "does the fuzzer actually send what it generated": every
// payload must arrive at the target with its value intact, in the position its
// Location names, and with every OTHER declared parameter still valid — because
// a probe that trips the server's own validation on an unrelated parameter never
// reaches the boundary under test, and reports the rejection as a finding.
func TestEngineRunnerDeliversThePayloadVerbatim(t *testing.T) {
	cap := newCapture(t, nil)
	ir, err := fuzz.LoadOpenAPI(writeSpec(t, pathParamSpec))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	op := ir.Op("GET /users/{id}")
	if op == nil {
		t.Fatal("GET /users/{id} missing")
	}

	runner := newEngineRunner()
	for _, p := range fuzz.Generate(op, 3, 0) {
		status, err := runner.Run(context.Background(),
			FuzzRequest{BaseURL: cap.server.URL, Op: op, Payload: p})
		if err != nil {
			if !errors.Is(err, errProbeUndeliverable) {
				t.Errorf("payload %d (%s): %v", p.Index, p.Note, err)
			}
			continue
		}
		if status != 200 {
			t.Errorf("payload %d (%s): status %d", p.Index, p.Note, status)
		}
	}

	hits := cap.all()
	t.Logf("%d probes delivered, %d generated", len(hits), len(fuzz.Generate(op, 3, 0)))
	if len(hits) == 0 {
		t.Fatal("no probe reached the server")
	}

	// Only probes the transport CAN carry are dispatched. The ones it cannot
	// (a header value carrying a NUL) are rejected before the request is built,
	// so the delivered sequence is the generated one with those removed. Building
	// the expected sequence explicitly keeps the index alignment honest instead
	// of drifting by a silent offset.
	deliverable := make([]fuzz.Payload, 0, len(hits))
	undeliverable := 0
	for _, p := range fuzz.Generate(op, 3, 0) {
		if p.Location != "header" {
			deliverable = append(deliverable, p)
			continue
		}
		if !validHeaderFieldValue(fuzzScalar(p.Value)) {
			undeliverable++
			continue
		}
		deliverable = append(deliverable, p)
	}
	if len(deliverable) != len(hits) {
		t.Fatalf("%d probes were deliverable but %d reached the server "+
			"(%d skipped as unencodable)", len(deliverable), len(hits), undeliverable)
	}
	t.Logf("%d probes delivered, %d skipped as unencodable", len(hits), undeliverable)

	sawPath, sawQuery, sawHeader := 0, 0, 0
	for i, p := range deliverable {
		if i >= len(hits) {
			break
		}
		got := hits[i]
		if got.Method != op.Method {
			t.Errorf("probe %d used %s, want %s", p.Index, got.Method, op.Method)
		}
		// Every probe must carry the sibling parameters, or it would be rejected
		// for the omission before the boundary under test is ever evaluated.
		if got.Query == "" {
			t.Errorf("probe %d (%s) dropped the `verbose` query parameter: %q",
				p.Index, p.Note, got.Path)
		}
		// A nil sibling is deliberately not sent at all: there is no wire
		// representation of a null header. Everything else must be present, or
		// the probe would be rejected for the omission before the boundary under
		// test is ever reached.
		// The header under test must be present UNLESS this very probe made it
		// absent by asking for a null value, which has no wire representation.
		// Every other case is a sibling the renderer is obliged to fill in, or
		// the server rejects the request for the omission before the boundary
		// under test is reached.
		probeIsNullHeader := p.Location == "header" && p.Field == "X-Trace" && p.Value == nil
		if !probeIsNullHeader {
			if _, present := got.Header["X-Trace"]; !present {
				t.Errorf("probe %d (%s) dropped the `X-Trace` header", p.Index, p.Note)
			}
		}

		switch p.Location {
		case "path":
			sawPath++
			// The path segment must be exactly the scalar rendering of the value.
			if !strings.HasSuffix(got.Path, "/users/"+fuzzScalar(p.Value)) {
				t.Errorf("probe %d: path %q does not end with the payload %q",
					p.Index, got.Path, fuzzScalar(p.Value))
			}
		case "query":
			sawQuery++
			if !strings.Contains(got.Query, "verbose="+fuzzScalar(p.Value)) {
				t.Errorf("probe %d: query %q does not carry the payload %q",
					p.Index, got.Query, fuzzScalar(p.Value))
			}
		case "header":
			sawHeader++
			// Compared through the map, not Get: a probe whose value is the
			// empty string is still PRESENT, and Get reports both as "".
			// A nil value has no wire representation: there is no such thing as
			// a null HTTP header, so "absent" is the faithful outcome and the
			// header must NOT appear. Sending `X-Trace:` with nothing after it
			// would claim a value the probe never had.
			values, present := got.Header["X-Trace"]
			if p.Value == nil {
				if present {
					t.Errorf("probe %d: a null header value was sent as %q; a null "+
						"header has no wire representation and must be absent",
						p.Index, strings.Join(values, ","))
				}
			} else if !present {
				t.Errorf("probe %d: the `X-Trace` header is absent", p.Index)
			} else if len(values) != 1 || values[0] != onTheWire(fuzzScalar(p.Value)) {
				t.Errorf("probe %d: header %q, want %q",
					p.Index, strings.Join(values, ","), onTheWire(fuzzScalar(p.Value)))
			}
		}
	}
	t.Logf("delivered: %d path, %d query, %d header probes", sawPath, sawQuery, sawHeader)
	if sawPath == 0 || sawQuery == 0 || sawHeader == 0 {
		t.Errorf("only some parameter locations were exercised: path=%d query=%d header=%d",
			sawPath, sawQuery, sawHeader)
	}
}

// TestEngineRunnerDeliversTemplateInjectionIntact is the gate for the reason the
// runner owns its engine construction.
//
// The corpus contains `{{7*7}}` on purpose. The engine expands `{{name}}` in
// every request, so without the round-trip resolver the probe is consumed by
// Pulse's own expander, never reaches the server, and the injection strategy is
// reported as tested while having tested nothing. Binding the resolver to the
// wrong engine, or not at all, fails here and nowhere else.
func TestEngineRunnerDeliversTemplateInjectionIntact(t *testing.T) {
	cap := newCapture(t, nil)
	ir, err := fuzz.LoadOpenAPI(writeSpec(t, `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /render:
    post:
      requestBody:
        content:
          application/json:
            schema:
              type: object
              properties:
                tpl: {type: string}
      responses:
        '200': {description: ok}
`))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	op := ir.Op("POST /render")

	const probe = "{{7*7}}"
	payload := fuzz.Payload{
		Index: 1, OpID: op.ID, Location: "body", Field: "tpl",
		Strategy: fuzz.Injection, Note: "template", Value: probe,
	}
	status, err := newEngineRunner().Run(context.Background(),
		FuzzRequest{BaseURL: cap.server.URL, Op: op, Payload: payload})
	if err != nil {
		t.Fatalf("Run: %v — the probe was consumed before it left Pulse", err)
	}
	if status != 200 {
		t.Fatalf("status %d", status)
	}

	hits := cap.all()
	if len(hits) != 1 {
		t.Fatalf("the server saw %d requests, want 1", len(hits))
	}
	var body struct {
		Tpl string `json:"tpl"`
	}
	if err := json.Unmarshal([]byte(hits[0].Body), &body); err != nil {
		t.Fatalf("the server received %q, which is not JSON: %v", hits[0].Body, err)
	}
	if body.Tpl != probe {
		t.Errorf("the server received %q, want %q; the expander consumed the "+
			"probe before it was sent", body.Tpl, probe)
	}
	t.Logf("the server received the probe verbatim: %q", body.Tpl)
}

// TestRoundTripResolverIsAnIdentity pins the resolver's contract directly, so a
// regression is attributed to the resolver rather than to whatever probe happens
// to exercise it next.
func TestRoundTripResolverIsAnIdentity(t *testing.T) {
	r := roundTripResolver{}
	for _, name := range []string{"7*7", "token", "", "a b", "}}", "{{"} {
		got, err := r.Lookup(name)
		if err != nil {
			t.Errorf("Lookup(%q): %v", name, err)
			continue
		}
		if want := "{{" + name + "}}"; got != want {
			t.Errorf("Lookup(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestEngineRunnerReportsAServerCrash proves the engine path surfaces a 5xx as a
// finding with the payload, rather than swallowing it in the sink.
func TestEngineRunnerReportsAServerCrash(t *testing.T) {
	cap := newCapture(t, func(capturedReq) int { return http.StatusInternalServerError })
	ir, err := fuzz.LoadOpenAPI(writeSpec(t, fuzzSchema))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	op := ir.Op("POST /users")
	if op == nil {
		t.Fatal("POST /users missing")
	}

	out, err := ExecuteFuzz(context.Background(), FuzzOptions{
		SchemaPath:  writeSpec(t, fuzzSchema),
		Target:      cap.server.URL,
		OpID:        "POST /users",
		Seed:        5,
		Budget:      20,
		Rate:        2000,
		Concurrency: 4,
	}, newEngineRunner(), nil)
	if err != nil {
		t.Fatalf("ExecuteFuzz: %v", err)
	}
	if out.Failed != out.Total {
		t.Errorf("%d of %d probes failed, want all %d: a 500 must be a finding",
			out.Failed, out.Total, out.Total)
	}
	if !strings.Contains(out.TAP, "unexpected status 500") {
		t.Errorf("the TAP output does not report the 500:\n%s", firstLinesOf(out.TAP, 14))
	}
	if !strings.Contains(out.TAP, "  ---") {
		t.Errorf("the TAP output carries no diagnostic block:\n%s", firstLinesOf(out.TAP, 14))
	}
	if cap.count() == 0 {
		t.Error("the crash was reported but no probe reached the server")
	}
	t.Logf("%s; %d probes dispatched", fuzz.Summarize(out.Findings), cap.count())
}

// TestEngineRunnerBoundsAServerHangingForever proves a probe that hangs a
// handler costs the run ONE timeout rather than the whole run.
//
// A boundary fuzzer that meets a hanging handler and then waits for it is
// indistinguishable from a hung process, and the difference is only visible
// after the corpus has been running for minutes.
func TestEngineRunnerBoundsAServerHangingForever(t *testing.T) {
	release := make(chan struct{})
	blocked := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blocked <- struct{}{}
		<-release // never answers
	}))
	defer srv.Close()
	defer close(release)

	ir, err := fuzz.LoadOpenAPI(writeSpec(t, fuzzSchema))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	runner := &EngineFuzzRunner{Timeout: 150 * time.Millisecond}

	start := time.Now()
	_, err = runner.Run(context.Background(), FuzzRequest{
		BaseURL: srv.URL, Op: ir.Op("POST /users"),
		Payload: fuzz.Payload{Index: 0, OpID: "POST /users", Location: "body",
			Field: "name", Note: "probe", Value: "x"},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Error("a hanging server produced no error")
	}
	if elapsed > 3*time.Second {
		t.Errorf("one probe against a hanging server took %v; the timeout is not "+
			"bounding the run", elapsed)
	}
	t.Logf("a hanging handler cost %v, bounded by the %v timeout", elapsed, runner.Timeout)
}

// TestEngineRunnerFailsClosedOnAnUnknownOperation proves a payload that names an
// operation outside the document is an error, not a silently empty request.
func TestEngineRunnerFailsClosedOnAnUnknownOperation(t *testing.T) {
	ir, err := fuzz.LoadOpenAPI(writeSpec(t, fuzzSchema))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	_, err = newEngineRunner().Run(context.Background(), FuzzRequest{
		BaseURL: "http://127.0.0.1:1", Op: nil,
		Payload: fuzz.Payload{Index: 0, OpID: "DELETE /nope"},
	})
	if err == nil {
		t.Fatal("a payload with no operation was dispatched")
	}
	if !strings.Contains(err.Error(), "DELETE /nope") {
		t.Errorf("the error does not name the operation: %v", err)
	}
	_ = ir
}

// TestRenderedRequestIsByteStable proves two renders of the same payload agree.
//
// Determinism is the package's central claim; the renderer is the last place map
// iteration could break it, and a request whose bytes vary run to run cannot be
// diffed between runs.
func TestRenderedRequestIsByteStable(t *testing.T) {
	ir, err := fuzz.LoadOpenAPI(writeSpec(t, fuzzSchema))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	op := ir.Op("POST /users")
	for _, p := range fuzz.Generate(op, 11, 0) {
		first, err := renderFuzzRequest(FuzzRequest{BaseURL: "http://127.0.0.1:9", Op: op, Payload: p})
		if err != nil {
			t.Fatalf("render %d: %v", p.Index, err)
		}
		for i := 0; i < 20; i++ {
			got, err := renderFuzzRequest(FuzzRequest{
				BaseURL: "http://127.0.0.1:9", Op: op, Payload: p})
			if err != nil {
				t.Fatalf("render %d: %v", p.Index, err)
			}
			if got != first {
				t.Fatalf("payload %d rendered differently on attempt %d:\n%s\n---\n%s",
					p.Index, i, first, got)
			}
		}
	}
	t.Logf("%d payloads each rendered 21 times, byte-identical",
		len(fuzz.Generate(op, 11, 0)))
}

// TestEngineRunnerRejectsAPathVariableWithNoParameter proves the renderer fails
// on a malformed document instead of sending a request that cannot succeed.
//
// Substituting a placeholder would produce a 404 that reads as a server finding
// when it is a defect in the schema Pulse was handed.
func TestEngineRunnerRejectsAPathVariableWithNoParameter(t *testing.T) {
	ir, err := fuzz.LoadOpenAPI(writeSpec(t, `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /a/{undeclared}:
    get:
      responses:
        '200': {description: ok}
`))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	_, err = renderFuzzRequest(FuzzRequest{
		BaseURL: "http://127.0.0.1:9", Op: ir.Op("GET /a/{undeclared}"),
		Payload: fuzz.Payload{Index: 0, OpID: "GET /a/{undeclared}"},
	})
	if err == nil {
		t.Fatal("a path variable with no declared parameter was rendered")
	}
	if !strings.Contains(err.Error(), "undeclared") {
		t.Errorf("the error does not name the variable: %v", err)
	}
}

// TestUndeliverableProbeIsSkippedNotFailed is the honesty gate.
//
// A NUL byte cannot appear in an HTTP header value, so the client refuses the
// request and it never leaves Pulse. Counting that as a FAILURE would report a
// correct server as broken — and a user who learns the tool cries wolf stops
// reading it. Silently dropping it would shrink the corpus while the TAP plan
// still promised the case, which is the hang this file's plan checks exist to
// prevent. The only disposition that is both honest and complete is SKIP with a
// stated reason.
func TestUndeliverableProbeIsSkippedNotFailed(t *testing.T) {
	cap := newCapture(t, nil) // every reachable probe is a pass
	out, err := ExecuteFuzz(context.Background(), FuzzOptions{
		SchemaPath:  writeSpec(t, pathParamSpec),
		Target:      cap.server.URL,
		OpID:        "GET /users/{id}",
		Seed:        3,
		Budget:      200,
		Rate:        5000,
		Concurrency: 4,
	}, newEngineRunner(), nil)
	if err != nil {
		t.Fatalf("ExecuteFuzz: %v", err)
	}
	if out.Skipped == 0 {
		t.Fatalf("no probe was skipped; the header NUL payloads should be "+
			"undeliverable [%d passed, %d failed]", out.Passed, out.Failed)
	}
	if out.Failed != 0 {
		t.Errorf("%d undeliverable probes were reported as server findings:\n%s",
			out.Failed, firstLinesOf(out.TAP, 20))
	}
	if !strings.Contains(out.TAP, "# SKIP") {
		t.Errorf("a skipped probe carries no SKIP marker:\n%s", firstLinesOf(out.TAP, 20))
	}
	if !strings.Contains(out.TAP, "not run:") {
		t.Errorf("a skip gives no reason (AGENTS.md \u00a74.5 requires one):\n%s",
			firstLinesOf(out.TAP, 20))
	}
	// The plan must still cover every payload, skipped ones included.
	if !strings.Contains(out.TAP, "1.."+itoa(out.Total)) {
		t.Errorf("the plan does not cover the skipped probes:\n%s", firstLinesOf(out.TAP, 6))
	}
	var resultLines int
	for _, l := range strings.Split(out.TAP, "\n") {
		if strings.HasPrefix(l, "ok ") || strings.HasPrefix(l, "not ok ") {
			resultLines++
		}
	}
	if resultLines != out.Total {
		t.Errorf("%d result lines for a plan of %d; the document is incomplete",
			resultLines, out.Total)
	}
	t.Logf("%d passed, %d failed, %d skipped, %d dispatched",
		out.Passed, out.Failed, out.Skipped, cap.count())
}

// TestFuzzEscapePathKeepsTraversalLiteral is the gate for the decision NOT to
// use url.PathEscape.
//
// PathEscape encodes `/`, which would deliver a traversal probe as a single
// segment. The server's router would never see a traversal, and the injection
// strategy would report itself as run while testing nothing.
func TestFuzzEscapePathKeepsTraversalLiteral(t *testing.T) {
	cases := []struct{ in, want string }{
		{"../../etc/passwd", "../../etc/passwd"}, // the probe: slashes stay
		{"a/b/c", "a/b/c"},
		// A raw space cannot appear in a URL, so it is encoded; the server URL-
		// decodes it back to the space the probe meant. What must survive LITERAL
		// is the injection's structure, not its whitespace.
		{"' OR '1'='1", "'%20OR%20'1'='1"},
		{"'OR'1'='1", "'OR'1'='1"},
		{"{{7*7}}", "{{7*7}}"},
		{"a b", "a%20b"},    // a raw space cannot appear in a URL
		{"a\x00b", "a%00b"}, // control bytes are encoded
		{"a#b", "a%23b"},    // # would start a fragment
		{"a?b", "a%3Fb"},    // ? would start a query
		{"100%", "100%25"},  // % would start an escape
	}
	for _, c := range cases {
		if got := fuzzEscapePath(c.in); got != c.want {
			t.Errorf("fuzzEscapePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestFuzzEscapePathProducesAUsableURL proves the escaped form survives a real
// request, which is the only thing the escaping is for.
func TestFuzzEscapePathProducesAUsableURL(t *testing.T) {
	cap := newCapture(t, nil)
	ir, err := fuzz.LoadOpenAPI(writeSpec(t, `openapi: 3.0.0
info: {title: T, version: "1"}
paths:
  /files/{name}:
    get:
      parameters:
        - name: name
          in: path
          required: true
          schema: {type: string}
      responses:
        '200': {description: ok}
`))
	if err != nil {
		t.Fatalf("LoadOpenAPI: %v", err)
	}
	op := ir.Op("GET /files/{name}")

	// A traversal probe and a control-byte probe both have to reach the server.
	for _, probe := range []string{"../../etc/passwd", "a b", "a\x00b"} {
		if _, err := newEngineRunner().Run(context.Background(), FuzzRequest{
			BaseURL: cap.server.URL, Op: op,
			Payload: fuzz.Payload{Index: 0, OpID: op.ID, Location: "path",
				Field: "name", Note: "probe", Value: probe},
		}); err != nil {
			t.Errorf("probe %q was not deliverable: %v", probe, err)
		}
	}
	hits := cap.all()
	if len(hits) != 3 {
		t.Fatalf("the server saw %d requests, want 3", len(hits))
	}
	// The traversal arrives DECODED as a traversal, which is what the probe is.
	if !strings.HasSuffix(hits[0].Path, "/etc/passwd") {
		t.Errorf("the traversal probe arrived as %q; url decoding did not restore "+
			"the traversal", hits[0].Path)
	}
	t.Logf("traversal arrived as %q", hits[0].Path)
}

// TestDescribeUnencodableNamesTheByte proves a skip says WHICH byte blocked it.
func TestDescribeUnencodableNamesTheByte(t *testing.T) {
	got := describeUnencodable("a\x00b\x1fc\x00")
	for _, want := range []string{"0x00", "0x1f"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeUnencodable = %q, missing %s", got, want)
		}
	}
	if strings.Contains(got, "0x00,") && strings.Count(got, "0x00") != 1 {
		t.Errorf("describeUnencodable = %q; a repeated byte is listed once", got)
	}
}

// onTheWire is what Go's client actually puts in a header value.
//
// RFC 7230 section 3.2.4 makes leading and trailing whitespace around a field
// value optional, and Go strips it. A probe of " " therefore arrives as "". That
// is not the transport being wrong; it is the definition of a field value, and
// the probe still reached the target — which is what the gate is checking.
func onTheWire(v string) string {
	return strings.Trim(v, " \t")
}
