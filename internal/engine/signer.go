package engine

import (
	"context"
	"net/http"
)

// Signer mutates an outbound request immediately before network dispatch.
//
// It is invoked by execute() after buildRequest and before client.Do, and
// ONLY when the AST request carries a @wasm-sign directive: a request that
// does not ask for signing never touches the signer. A nil Signer is legal
// and means \"signing unavailable\" — a request that asks for it then fails
// closed rather than dispatching unsigned.
//
// The concrete implementation lives in internal/app (the composition root),
// which binds the Wasm sandbox, the secret resolver and the script loader.
// Like Redactor and Resolver, this is an interface so the engine never
// imports a sibling (AGENTS.md §3.1).
type Signer interface {
	// Sign mutates req in place (typically by setting signature headers).
	// The wasm script path travels in ctx via ContextWithSignerPath; the
	// *.http request that asked for signing is what selected it.
	Sign(ctx context.Context, req *http.Request) error
}

// WithSigner binds the pre-dispatch signer. Until one is bound, a request
// carrying @wasm-sign fails closed.
func WithSigner(s Signer) Option { return func(e *Engine) { e.signer = s } }

// signerPathKey carries the @wasm-sign script path from the dispatch site to
// the Signer. Unexported so only this package can set it; the Signer reads it
// through SignerPath.
type signerPathKey struct{}

// ContextWithSignerPath attaches the wasm script path to ctx for Sign.
func ContextWithSignerPath(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, signerPathKey{}, path)
}

// SignerPath returns the script path attached by ContextWithSignerPath.
func SignerPath(ctx context.Context) (string, bool) {
	p, _ := ctx.Value(signerPathKey{}).(string)
	return p, p != ""
}
