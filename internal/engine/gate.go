package engine

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// Policy gate: the pre-dispatch seam an embedding server uses to refuse a
// request before it reaches the network.
//
// # Why the seam is here and not in the caller
//
// A gate that vets the RAW `.http` source cannot know the host: the target is
// still `{{base}}/users`, and only the engine's resolver turns that into
// `https://api.example.com/users`. A gate that vets the AST header map cannot
// know a header value that came from the keychain. By the time buildRequest has
// expanded the URL and every header, the request is exactly what the transport
// would send — so that is where the check runs, and a refusal is a failure
// before `client.Do` rather than a filter on its way back.
//
// The engine deliberately owns no policy: it defines the interface and the
// typed error, and the composition root injects a concrete gate into the router
// it builds for the MCP server. The TUI and `pulse test` routers are built
// without one, because an interactive human is not the untrusted party a guard
// exists for.

// PolicyGate vets one fully expanded request before dispatch. A nil return
// allows the request; a non-nil return MUST be a *PolicyBlockedError so the
// calling tool can distinguish a policy refusal from a transport failure.
type PolicyGate interface {
	Check(GateRequest) error
}

// GateRequest is the request as it would go on the wire.
//
// URL is the parsed target, never the raw template: its Host is what a host
// policy matches and its Query is what a required-parameter policy inspects.
// Header carries resolved values, so a gate must not copy them into an error.
type GateRequest struct {
	// Method is the HTTP method, upper case (e.g. "GET").
	Method string
	// URL is the expanded target.
	URL *url.URL
	// Header is the expanded request header set.
	Header http.Header
	// Line is the request's source line, for diagnostics.
	Line int32
}

// ErrPolicyBlocked marks a refusal by a PolicyGate. It is a sentinel so callers
// classify with errors.Is, whatever the concrete gate wraps.
var ErrPolicyBlocked = errors.New("engine: blocked by policy")

// PolicyBlockedError is a typed refusal. Reason is a complete sentence written
// for the agent that authored the request — it is the text a tool returns, not
// an internal diagnostic — and MUST NOT contain a header value, a query value,
// or any other resolved secret.
type PolicyBlockedError struct {
	// Reason explains what the policy refused and why, in one sentence.
	Reason string
}

// Error implements error. The prefix is stable because agents and tests match
// it: a blocked request is not a "request failed" error.
func (e *PolicyBlockedError) Error() string { return "BLOCKED BY POLICY: " + e.Reason }

// Unwrap exposes the sentinel for errors.Is.
func (e *PolicyBlockedError) Unwrap() error { return ErrPolicyBlocked }

// NewPolicyBlockedError builds a refusal with the given sentence.
func NewPolicyBlockedError(reason string) *PolicyBlockedError {
	return &PolicyBlockedError{Reason: reason}
}

// WithPolicyGate installs a pre-dispatch gate on this engine. Nil disables the
// gate, which is the default for every router that serves a human.
func WithPolicyGate(g PolicyGate) Option {
	return func(e *Engine) { e.gate = g }
}

// checkGate runs the gate, if any, and guarantees the typed refusal shape.
// A gate that returns an untyped error is a programming defect; it is wrapped
// rather than allowed to masquerade as a transport failure.
func (e *Engine) checkGate(req GateRequest) error {
	if e.gate == nil {
		return nil
	}
	if err := e.gate.Check(req); err != nil {
		var blocked *PolicyBlockedError
		if errors.As(err, &blocked) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrPolicyBlocked, err)
	}
	return nil
}
