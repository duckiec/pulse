package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

// Traffic recording: `pulse record :9000 --target http://localhost:8080`.
//
// # What it is
//
// A reverse proxy that writes every request it forwards into a .http file, so a
// collection can be generated from traffic a frontend, a mobile app or curl
// already produces. The proxy itself is stdlib (httputil.ReverseProxy); the
// value is the translation from a live exchange into Pulse's own format.
//
// # Why the recorded URL is the target's, not the proxy's
//
// The collection must hit the real server when it is replayed, so a request
// captured on :9000 is written as `http://localhost:8080/users`, not
// `http://localhost:9000/users`. Recording the listen address would generate a
// collection that points back at the recorder and can never pass again.
//
// # Why sensitive headers become variables
//
// `Authorization: Bearer <token>` in a .http file is a committed credential.
// The recorder replaces the whole value with `{{AUTH_TOKEN}}`, the same
// placeholder idiom the collection format already uses, so the captured file is
// safe to commit and resolves through the secret chain at run time. Registered
// secrets are scrubbed out of every other value and out of the body; a value
// the redactor changes becomes a variable too, because `[REDACTED]` is not a
// request anyone can replay.
//
// # Why capture is best-effort, never in the response path
//
// Writing a block is a small append, not an fsync, and it happens after the
// upstream response headers arrive. A failure to write is logged and dropped:
// losing one captured request is recoverable, failing the proxied request is
// not — the same rule history recording follows.

// maxRecordBodyBytes bounds what the recorder copies out of a request body.
// Larger bodies are forwarded intact and omitted from the block with a comment,
// because a truncated body in a collection is a request that looks real and
// behaves differently.
const maxRecordBodyBytes = 64 << 10

// recordHeaderVars maps the headers whose values are credentials to the
// placeholder the generated block uses. Headers in sensitiveHeaders that are
// not listed here fall back to an uppercased, underscore-joined name, so a
// future `X-Session-Token` is redacted without a code change.
var recordHeaderVars = map[string]string{
	"authorization":       "AUTH_TOKEN",
	"proxy-authorization": "PROXY_AUTH_TOKEN",
	"x-api-key":           "API_KEY",
	"api-key":             "API_KEY",
	"cookie":              "COOKIE",
}

// RecordConfig configures one recording proxy.
type RecordConfig struct {
	// Target is the upstream base URL, e.g. http://localhost:8080.
	Target string
	// Out is the .http file to append to. It is created if absent.
	Out string
	// Redactor scrubs registered secrets from captured values. Nil records
	// what arrived, still redacting credential headers by name.
	Redactor Redactor
	// Logf receives one line per captured or skipped request. Nil is silent.
	Logf func(format string, args ...any)
}

// Recorder is an http.Handler that proxies to the target and appends a .http
// block for every distinct request it forwards.
type Recorder struct {
	target   *url.URL
	out      string
	proxy    *httputil.ReverseProxy
	redactor Redactor
	logf     func(format string, args ...any)

	mu        sync.Mutex
	seen      map[string]bool
	usedNames map[string]bool
	wrote     bool
	blocks    int
}

type recordCaptureKey struct{}

// recordCapture is the per-request data ModifyResponse cannot otherwise see:
// the inbound method, headers and body. It is attached to the request context
// before proxying, and ReverseProxy carries that context to the outbound
// request whose response reaches ModifyResponse.
type recordCapture struct {
	method       string
	url          string
	headers      http.Header
	body         []byte
	bodyPresent  bool
	bodyComplete bool
}

// NewRecorder validates the target, prepares the output file and builds the
// proxy. It does not listen: the CLI owns the process lifecycle.
func NewRecorder(cfg RecordConfig) (*Recorder, error) {
	target, err := url.Parse(strings.TrimSpace(cfg.Target))
	if err != nil {
		return nil, fmt.Errorf("record: parse target: %w", err)
	}
	if target.Scheme != "http" && target.Scheme != "https" {
		return nil, fmt.Errorf("record: target %q must be an http:// or https:// URL", cfg.Target)
	}
	if target.Host == "" {
		return nil, fmt.Errorf("record: target %q has no host", cfg.Target)
	}
	out := strings.TrimSpace(cfg.Out)
	if out == "" {
		return nil, fmt.Errorf("record: an output .http file is required")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return nil, fmt.Errorf("record: create the output directory: %w", err)
	}

	r := &Recorder{
		target:    target,
		out:       out,
		redactor:  cfg.Redactor,
		logf:      cfg.Logf,
		seen:      map[string]bool{},
		usedNames: readRecordedNames(out),
	}
	if r.logf == nil {
		r.logf = func(string, ...any) {}
	}
	r.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// The inbound Host names the recorder, not the service. Forward
			// the target's Host so virtual-host routing still works.
			pr.Out.Host = target.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			if cap, ok := resp.Request.Context().Value(recordCaptureKey{}).(*recordCapture); ok && cap != nil {
				r.record(cap, resp.StatusCode)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			r.logf("record: %s %s failed: %v", req.Method, req.URL.Path, err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(struct {
				Error string `json:"error"`
			}{Error: fmt.Sprintf("pulse record: the target %s could not be reached", target.Host)})
		},
	}
	return r, nil
}

// ServeHTTP captures the inbound request, then proxies it.
func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	cap := &recordCapture{
		method:  req.Method,
		url:     r.outboundURL(req),
		headers: req.Header.Clone(),
	}
	cap.body, cap.bodyPresent, cap.bodyComplete = readRecordedBody(req)
	ctx := context.WithValue(req.Context(), recordCaptureKey{}, cap)
	r.proxy.ServeHTTP(w, req.WithContext(ctx))
}

// outboundURL is the URL the generated block will send: the target origin plus
// the inbound path and query. It mirrors ReverseProxy's path join so the
// recorded request and the forwarded request are the same address.
func (r *Recorder) outboundURL(req *http.Request) string {
	u := *r.target
	u.Path = joinURLPath(r.target.Path, req.URL.Path)
	u.RawPath = ""
	u.RawQuery = joinURLQuery(r.target.RawQuery, req.URL.RawQuery)
	u.Fragment = ""
	return u.String()
}

// joinURLPath matches httputil.ReverseProxy's singleJoiningSlash rule.
func joinURLPath(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash && b != "":
		return a + "/" + b
	}
	return a + b
}

func joinURLQuery(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "&" + b
}

// readRecordedBody buffers a bounded request body so the block can be rendered
// before the response arrives, then restores the body for the proxy.
//
// A body over the limit is put back together from what was read plus the unread
// remainder: it is forwarded byte-for-byte and omitted from the block.
func readRecordedBody(req *http.Request) (body []byte, present, complete bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, false, true
	}
	buf, err := io.ReadAll(io.LimitReader(req.Body, maxRecordBodyBytes+1))
	complete = err == nil && len(buf) <= maxRecordBodyBytes
	if complete {
		req.Body = io.NopCloser(bytes.NewReader(buf))
	} else {
		req.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), req.Body))
	}
	return buf, true, complete
}

// record renders and appends one block. It is called from the proxy goroutine,
// so the whole decision — dedupe, name allocation, write — is under one lock;
// otherwise two concurrent captures of the same route could both claim a name.
func (r *Recorder) record(cap *recordCapture, status int) {
	key := strings.ToUpper(cap.method) + " " + cap.url
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[key] {
		r.logf("skipped %s %s (already captured)", cap.method, r.scrub(cap.url))
		return
	}
	name := r.claimName(cap.method, cap.url)
	block := r.renderBlock(cap, status, name)
	if err := r.appendBlock(block); err != nil {
		r.logf("record: write %s: %v", r.out, err)
		return
	}
	r.seen[key] = true
	r.blocks++
	r.logf("captured %s %s -> %d  # %s", cap.method, r.scrub(cap.url), status, name)
}

// scrub pipes a value through the redactor, if one is installed.
func (r *Recorder) scrub(s string) string {
	if r.redactor == nil {
		return s
	}
	return r.redactor.String(s)
}

// claimName returns a file-unique `@name` slug for the request. Names are
// namespaced by method and path, with a numeric suffix when the same shape is
// captured with a different query string.
func (r *Recorder) claimName(method, rawURL string) string {
	path := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		path = u.Path
	}
	base := "record-" + slug(method) + "-" + slug(path)
	if base == "record-" {
		base = "record-request"
	}
	name := base
	for n := 2; r.usedNames[name]; n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	r.usedNames[name] = true
	return name
}

// slug lowers and reduces text to [a-z0-9-] runs, the shape a request name may
// take in a file and in a `pulse test -run` selector.
func slug(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// renderBlock produces one complete .http request block, including its
// directives, headers, body and status assertion.
func (r *Recorder) renderBlock(cap *recordCapture, status int, name string) string {
	url := r.scrub(cap.url)
	var b strings.Builder
	fmt.Fprintf(&b, "### %s %s (captured by pulse record)\n", strings.ToUpper(cap.method), url)
	fmt.Fprintf(&b, "# @name %s\n", name)
	fmt.Fprintf(&b, "# @assert status == %d\n", status)
	if cap.bodyPresent && !cap.bodyComplete {
		fmt.Fprintf(&b, "# pulse record: the request body exceeded %d bytes and was omitted\n", maxRecordBodyBytes)
	}
	if cap.bodyPresent && cap.bodyComplete && !utf8Body(cap.body) {
		// A binary body cannot be represented in a text collection without
		// encoding it, and silently base64-ing it would generate a different
		// request. Say so instead.
		fmt.Fprintf(&b, "# pulse record: binary request body omitted\n")
	} else if cap.bodyPresent && cap.bodyComplete && bodyHasSeparator(cap.body) {
		fmt.Fprintf(&b, "# pulse record: request body contains a ### separator and was omitted\n")
	}
	fmt.Fprintf(&b, "%s %s HTTP/1.1\n", strings.ToUpper(cap.method), url)

	for _, headerName := range captureHeaderOrder(cap.headers) {
		values := cap.headers[headerName]
		if len(values) == 0 || skipCapturedHeader(headerName) {
			continue
		}
		value := r.captureHeaderValue(headerName, values)
		if value == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n", textproto.CanonicalMIMEHeaderKey(headerName), value)
	}

	body := []byte(r.scrub(string(cap.body)))
	writable := cap.bodyPresent && cap.bodyComplete && utf8Body(body) &&
		!bodyHasSeparator(body)
	if writable && len(body) > 0 {
		b.WriteByte('\n')
		b.Write(body)
		if body[len(body)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// captureHeaderValue renders one header value, replacing credentials and
// scrubbing registered secrets. A value the redactor changed becomes the
// header's variable: a literal `[REDACTED]` would not replay.
func (r *Recorder) captureHeaderValue(name string, values []string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	variable, secret := recordHeaderVars[lower]
	if !secret {
		secret = sensitiveHeaders[lower]
		variable = strings.ToUpper(strings.ReplaceAll(lower, "-", "_"))
	}
	joined := flattenHeaderValues(values)
	if secret {
		return "{{" + variable + "}}"
	}
	if r.redactor != nil {
		scrubbed := r.redactor.String(joined)
		if scrubbed != joined {
			return "{{" + variable + "}}"
		}
	}
	return joined
}

// flattenHeaderValues joins repeated headers into one line. Newlines are
// replaced with spaces so a header value cannot inject a second header or a
// block separator into the generated file.
func flattenHeaderValues(values []string) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.ReplaceAll(v, "\r", " ")
		v = strings.ReplaceAll(v, "\n", " ")
		if v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, ", ")
}

// captureHeaderOrder returns the header names in deterministic, case-insensitive
// order so two runs that see the same traffic produce the same file.
func captureHeaderOrder(h http.Header) []string {
	out := make([]string, 0, len(h))
	for name := range h {
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := strings.ToLower(out[i]), strings.ToLower(out[j])
		if li == lj {
			return out[i] < out[j]
		}
		return li < lj
	})
	return out
}

// skipCapturedHeader drops headers that describe one connection rather than the
// request: replaying them would either be ignored or actively wrong.
func skipCapturedHeader(name string) bool {
	switch strings.ToLower(name) {
	case "host", "content-length", "accept-encoding", "connection",
		"proxy-connection", "keep-alive", "te", "trailer", "transfer-encoding",
		"upgrade":
		return true
	}
	return isHopByHopHeader(name)
}

// appendBlock appends the block in one write. A block written into a file whose
// last line lacks a newline is prefixed with one, so an interrupted previous
// session cannot glue two requests together.
func (r *Recorder) appendBlock(block string) error {
	prefix := ""
	if !r.wrote {
		if info, err := os.Stat(r.out); err == nil && info.Size() > 0 {
			if last, err := lastByte(r.out); err == nil && last != '\n' {
				prefix = "\n"
			}
		}
	}
	f, err := os.OpenFile(r.out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write([]byte(prefix + block))
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if cerr != nil {
		return cerr
	}
	r.wrote = true
	return nil
}

func lastByte(path string) (byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return 0, err
	}
	var b [1]byte
	if _, err := f.ReadAt(b[:], info.Size()-1); err != nil {
		return 0, err
	}
	return b[0], nil
}

// readRecordedNames collects the @name values already in the output file, so a
// second recording session continues the namespace instead of colliding.
func readRecordedNames(path string) map[string]bool {
	out := map[string]bool{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "# @name "); ok {
			if name := strings.TrimSpace(rest); name != "" {
				out[name] = true
			}
		}
	}
	return out
}

// utf8Body reports whether a body can be written verbatim into a text file.
// NUL bytes and invalid UTF-8 are the two shapes that would corrupt the file or
// silently change what is sent.
func utf8Body(body []byte) bool {
	if bytes.IndexByte(body, 0) >= 0 {
		return false
	}
	return utf8.Valid(body)
}

// bodyHasSeparator reports whether a body line would end the request block.
func bodyHasSeparator(body []byte) bool {
	if bytes.HasPrefix(body, []byte("###")) {
		return true
	}
	return bytes.Contains(body, []byte("\n###"))
}
