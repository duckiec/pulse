package engine

import (
	"context"
	"net/http"
)

// RequestHook rewrites a request after interpolation and before the policy
// firewall and dispatch.
//
// It exists for scripting a request cannot express in `.http` syntax: SigV4
// with a body digest, an HMAC over a canonical form, a token exchange that
// needs the final URL. The engine deliberately knows nothing about WebAssembly:
// the concrete implementation lives in internal/app, which owns the sandbox
// (AGENTS.md §3.1 — the engine never imports a sibling).
//
// # Where it runs, and why that order is the contract
//
//	buildRequest (interpolation) -> Hook -> Signer -> PolicyGate -> network
//
// A hook that ran earlier would see `{{base}}/users` instead of the real host,
// and one that ran later would be rewriting a request the firewall already
// approved. The gate is the LAST step for exactly that reason: a hook is
// user-supplied code, and the blast-radius firewall must judge what it produced,
// not what the `.http` file said before the hook ran.
type RequestHook interface {
	// Apply mutates req in place. The `# @wasm` script path travels in ctx via
	// ContextWithHookPath; a nil return means the request may continue.
	Apply(ctx context.Context, req *http.Request) error
}

// WithHook binds the pre-dispatch request hook. Until one is bound, a request
// carrying `# @wasm` fails closed.
func WithHook(h RequestHook) Option { return func(e *Engine) { e.hook = h } }

// hookPathKey carries the `# @wasm` script path from the dispatch site to the
// RequestHook. Unexported so only this package can set it.
type hookPathKey struct{}

// ContextWithHookPath attaches the hook script path to ctx for Apply.
func ContextWithHookPath(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, hookPathKey{}, path)
}

// HookPath returns the script path attached by ContextWithHookPath.
func HookPath(ctx context.Context) (string, bool) {
	p, _ := ctx.Value(hookPathKey{}).(string)
	return p, p != ""
}
