package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"pulse/internal/engine"
	"pulse/internal/wasm"
)

// WasmSigner is the live pre-dispatch adapter (ROADMAP.md §3.4.7): it loads
// the @wasm-sign script, instantiates the sandbox with the outbound request
// attached to the context (so header_get works), runs the guest's exported
// sign_request ABI over a canonical envelope, and applies the resulting
// headers to the outbound request.
//
// It lives HERE — the composition root — because it names concrete
// dependencies (os.ReadFile, wasm.New, the secret chain) on behalf of the
// engine, which only knows the Signer interface (AGENTS.md §3.1).
type WasmSigner struct {
	// Resolver answers hmac_sha256 key ids. Production binds the App's secret
	// chain so Vault keys resolve dynamically; tests bind a stub.
	Resolver wasm.SecretResolver
	// Load reads the script blob. Nil selects os.ReadFile. A missing script
	// fails closed — there is no unsigned fallback.
	Load func(path string) ([]byte, error)
	// Timeout bounds one guest call. Zero selects the sandbox default.
	Timeout time.Duration
	// Fuel bounds one guest call's invocations. Zero selects the default.
	Fuel uint64
}

// chainResolver adapts the secret chain to the sandbox contract. Errors (and
// only errors) cross: key MATERIAL stays behind Lookup and only the HMAC
// digest the host computes ever reaches the guest.
type chainResolver struct{ lookup func(string) (string, error) }

func (r chainResolver) Resolve(key string) (string, error) {
	if r.lookup == nil {
		return "", fmt.Errorf("app: no secret resolver bound for wasm key %q", key)
	}
	return r.lookup(key)
}

// Sign implements engine.Signer.
func (s *WasmSigner) Sign(ctx context.Context, req *http.Request) error {
	if ctx == nil || req == nil {
		return fmt.Errorf("app: wasm signer requires a non-nil context and request")
	}
	path, ok := engine.SignerPath(ctx)
	if !ok || path == "" {
		return fmt.Errorf("app: wasm signer invoked with no script path in context")
	}
	load := s.Load
	if load == nil {
		load = os.ReadFile
	}
	blob, err := load(path)
	if err != nil {
		return fmt.Errorf("app: load wasm signer %q: %w", path, err)
	}
	if len(blob) == 0 {
		return fmt.Errorf("app: wasm signer %q is empty", path)
	}
	if s.Resolver == nil {
		return fmt.Errorf("app: wasm signer %q has no secret resolver", path)
	}

	envelope, err := buildSignEnvelope(req)
	if err != nil {
		return err
	}
	if len(envelope) > wasm.MaxEnvelopeBytes {
		return fmt.Errorf("app: sign envelope is %d bytes, over the %d-byte ceiling",
			len(envelope), wasm.MaxEnvelopeBytes)
	}

	mod, err := wasm.New(ctx, wasm.Config{
		Wasm:     blob,
		Resolver: s.Resolver,
		Timeout:  s.Timeout,
		Fuel:     s.Fuel,
	})
	if err != nil {
		return fmt.Errorf("app: instantiate wasm signer %q: %w", path, err)
	}
	defer mod.Close(context.WithoutCancel(ctx))

	signed, err := mod.Sign(wasm.WithRequest(ctx, req), envelope)
	if err != nil {
		return fmt.Errorf("app: wasm signer %q failed: %w", path, err)
	}
	if err := applySignedHeaders(req, envelope, signed); err != nil {
		return fmt.Errorf("app: wasm signer %q: %w", path, err)
	}
	return nil
}

// buildSignEnvelope renders the canonical pre-flight bytes the guest signs.
//
// Layout (LF-separated, deliberately NOT HTTP wire format — this is a signing
// canonicalisation, and giving it its own grammar keeps the guest parser
// total over a tiny language instead of partial over HTTP):
//
//	METHOD path?query LF
//	lowercase-name: value LF   (sorted by name, one line per value)
//	LF
//	body
//
// Header names are lowercased so the guest's scan is case-exact; the host
// re-applies by name through http.Header.Set, which canonicalises. The body
// is consumed and RESTORED, so dispatch still sends it.
func buildSignEnvelope(req *http.Request) ([]byte, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("app: read request body for signing: %w", err)
		}
		body = b
		req.Body = io.NopCloser(bytes.NewReader(b))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(b)), nil
		}
	}
	var buf bytes.Buffer
	uri := req.URL.RequestURI()
	if uri == "" {
		uri = "/"
	}
	buf.WriteString(req.Method)
	buf.WriteByte(' ')
	buf.WriteString(uri)
	buf.WriteByte('\n')
	names := make([]string, 0, len(req.Header))
	for n := range req.Header {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		for _, v := range req.Header[n] {
			buf.WriteString(strings.ToLower(n))
			buf.WriteString(": ")
			buf.WriteString(v)
			buf.WriteByte('\n')
		}
	}
	buf.WriteByte('\n')
	buf.Write(body)
	return buf.Bytes(), nil
}

// applySignedHeaders extracts the guest's headers and mutates the outbound
// request. The guest MUST be append-only: the output must start with the exact
// envelope it was given, and only the trailing lines are parsed as
// `Name: value`. Anything else — a rewritten request line, a malformed header,
// an empty delta — fails closed.
func applySignedHeaders(req *http.Request, envelope, signed []byte) error {
	if len(signed) < len(envelope) || !bytes.Equal(signed[:len(envelope)], envelope) {
		return fmt.Errorf("the guest rewrote the signed envelope prefix: refusing to dispatch")
	}
	delta := signed[len(envelope):]
	if len(bytes.TrimSpace(delta)) == 0 {
		return fmt.Errorf("the guest returned no signature headers: refusing to dispatch unsigned")
	}
	for _, line := range bytes.Split(delta, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		name, value, ok := strings.Cut(string(line), ":")
		if !ok {
			return fmt.Errorf("malformed signature header %q", line)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" || value == "" {
			return fmt.Errorf("malformed signature header %q", line)
		}
		req.Header.Set(name, value)
	}
	return nil
}
