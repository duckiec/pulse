package app

// `# @wasm <script.wasm>`: a pre-dispatch request hook run in the same
// zero-capability sandbox as @wasm-sign (internal/wasm).
//
// # Why a second directive instead of widening @wasm-sign
//
// @wasm-sign has an append-only contract: the guest may add headers to a
// canonical envelope, never rewrite the request. That narrowness is what makes
// it safe to run on every authenticated request. A hook is a general rewrite
// (SigV4 over a body digest, token exchange, presigning), so it needs a
// different contract and must be opt-in per request.
//
// # The data contract
//
// The pending request is written into the guest's linear memory as JSON:
//
//	{"method":"POST",
//	 "url":"https://api.example.com/items?x=1",
//	 "headers":[["Accept","application/json"],["X-Trace","1"]],
//	 "body":"{\"a\":1}"}
//
// The guest exports `handle_request(ptr, len) -> i32`, rewrites the document in
// place, and returns the new length. The host validates the returned document
// strictly and REPLACES the outbound request's method, URL, headers and body.
// Headers are an array of [name, value] pairs so order and repeated names
// survive; `body` is a UTF-8 string, and a body that is not valid UTF-8 is
// refused rather than silently corrupted by JSON encoding.
//
// # Capability boundary
//
// The hook sandbox is instantiated with NO Resolver, so no host module exists:
// no filesystem, no environment, no network, no clock, no WASI, and no
// `pulse/env` functions. A guest that imports anything at all fails to
// instantiate (internal/wasm classifies that as ErrIsolationViolation). Reading
// `/etc/passwd` or `PULSE_VAULT_FILE` is not "blocked" by a check that could be
// bypassed; there is no import through which either is reachable.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"pulse/internal/engine"
	"pulse/internal/wasm"
)

// hookRequest is the JSON document handed to a `# @wasm` guest.
type hookRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers [][2]string `json:"headers"`
	Body    string      `json:"body"`
}

// WasmHook is the live engine.RequestHook adapter.
type WasmHook struct {
	// Load reads the script blob. Nil selects os.ReadFile. A missing script
	// fails closed; there is no "run without the hook" fallback.
	Load func(path string) ([]byte, error)
	// Timeout bounds one guest call. Zero selects the sandbox default.
	Timeout time.Duration
	// Fuel bounds one guest call's invocations. Zero selects the default.
	Fuel uint64
}

// Apply implements engine.RequestHook.
func (h *WasmHook) Apply(ctx context.Context, req *http.Request) error {
	if ctx == nil || req == nil {
		return fmt.Errorf("app: wasm hook requires a non-nil context and request")
	}
	path, ok := engine.HookPath(ctx)
	if !ok || path == "" {
		return fmt.Errorf("app: wasm hook invoked with no script path in context")
	}
	load := h.Load
	if load == nil {
		load = os.ReadFile
	}
	blob, err := load(path)
	if err != nil {
		return fmt.Errorf("app: load wasm hook %q: %w", path, err)
	}
	if len(blob) == 0 {
		return fmt.Errorf("app: wasm hook %q is empty", path)
	}

	doc, err := encodeHookRequest(req)
	if err != nil {
		return err
	}

	// No Resolver: zero host modules, so every import a guest declares fails
	// to instantiate. That is the whole capability boundary for this feature.
	mod, err := wasm.New(ctx, wasm.Config{Wasm: blob, Timeout: h.Timeout, Fuel: h.Fuel})
	if err != nil {
		return fmt.Errorf("app: instantiate wasm hook %q: %w", path, err)
	}
	defer mod.Close(context.WithoutCancel(ctx))
	if !mod.CanHook() {
		return fmt.Errorf("app: wasm hook %q does not export %s", path, wasm.ABIHandleRequest)
	}

	out, err := mod.HandleRequest(ctx, doc)
	if err != nil {
		return fmt.Errorf("app: wasm hook %q failed: %w", path, err)
	}
	if err := applyHookResponse(req, out); err != nil {
		return fmt.Errorf("app: wasm hook %q: %w", path, err)
	}
	return nil
}

// encodeHookRequest serialises the pending request. Header names are sorted so
// the same request produces the same bytes on every run, which is what lets a
// guest hash or sign the document deterministically.
func encodeHookRequest(req *http.Request) ([]byte, error) {
	body := ""
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("app: read request body for the wasm hook: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(b))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(b)), nil
		}
		// JSON strings are UTF-8. Replacing invalid bytes with U+FFFD would
		// silently change a body a signature may cover, so refuse instead.
		if !utf8.Valid(b) {
			return nil, fmt.Errorf("app: the request body is not valid UTF-8; a # @wasm hook cannot round-trip it")
		}
		body = string(b)
	}

	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([][2]string, 0, len(req.Header))
	for _, name := range names {
		values := req.Header[name]
		for _, v := range values {
			pairs = append(pairs, [2]string{name, v})
		}
	}
	raw, err := json.Marshal(hookRequest{
		Method:  req.Method,
		URL:     req.URL.String(),
		Headers: pairs,
		Body:    body,
	})
	if err != nil {
		return nil, fmt.Errorf("app: encode the hook request: %w", err)
	}
	return raw, nil
}

// applyHookResponse validates the guest's document and replaces the outbound
// request with it. Every field is checked: this is the boundary where code that
// ran without capabilities hands the host a request, and a header value
// containing CR/LF would be response splitting rather than a rewrite.
func applyHookResponse(req *http.Request, raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("the guest returned an empty document")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var doc hookRequest
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("the guest returned invalid JSON: %w", err)
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("the guest returned more than one JSON document")
	}

	if doc.Method == "" || !validMethod(doc.Method) {
		return fmt.Errorf("the guest returned an invalid method %q", doc.Method)
	}
	parsed, err := url.Parse(doc.URL)
	if err != nil {
		return fmt.Errorf("the guest returned an unparseable url: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("the guest returned a url with the unsupported scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("the guest returned a url with no host")
	}

	header := make(http.Header, len(doc.Headers))
	for _, pair := range doc.Headers {
		name, value := strings.TrimSpace(pair[0]), pair[1]
		if name == "" || !validHeaderName(name) {
			return fmt.Errorf("the guest returned an invalid header name %q", name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("the guest returned a header value containing a line break for %q", name)
		}
		header[http.CanonicalHeaderKey(name)] = append(header[http.CanonicalHeaderKey(name)], value)
	}

	req.Method = strings.ToUpper(doc.Method)
	req.URL = parsed
	// Clearing Host makes the transport use the URL's host. Leaving the old
	// value would send a request whose Host header contradicts its target.
	req.Host = ""
	req.Header = header
	body := []byte(doc.Body)
	if len(body) == 0 {
		// http.NoBody (not a NopCloser over nothing) keeps the transport from
		// treating an empty GET as an unknown-length chunked body.
		req.Body = http.NoBody
		req.GetBody = nil
		req.ContentLength = 0
		return nil
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))
	return nil
}

// validMethod reports whether s is an HTTP token, per RFC 9110's method ABNF.
// A hook may switch verbs (GET to POST for a presigned upload), so the check is
// the grammar rather than Pulse's own seven-keyword list.
func validMethod(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isTokenByte(s[i]) {
			return false
		}
	}
	return true
}

// validHeaderName applies the same token rule to a field name.
func validHeaderName(s string) bool {
	return validMethod(s)
}

func isTokenByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}
