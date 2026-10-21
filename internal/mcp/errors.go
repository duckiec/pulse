package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// JSON-RPC error codes used by the MCP bridge (ROADMAP.md §3.1.4).
//
// The standard codes come from JSON-RPC 2.0 itself; the -320xx block is the
// implementation-defined range the roadmap reserves for domain failures, and it
// is used precisely because an agent can act on the difference between "your
// arguments were wrong" (-32602) and "the endpoint refused to resolve a secret"
// (-32003).
const (
	// CodeParseError is JSON-RPC's malformed-JSON code.
	CodeParseError = -32700
	// CodeInvalidRequest is JSON-RPC's malformed-envelope code.
	CodeInvalidRequest = -32600
	// CodeMethodNotFound is JSON-RPC's unknown-method code.
	CodeMethodNotFound = -32601
	// CodeInvalidParams is JSON-RPC's bad-arguments code.
	CodeInvalidParams = -32602
	// CodeInternalError is JSON-RPC's catch-all.
	CodeInternalError = -32603

	// CodeParseFailed means the .http source did not parse.
	CodeParseFailed = -32002
	// CodeSecretUnresolved means a variable could not be resolved from the
	// keychain or the ephemeral environment. Pulse fails closed rather than
	// sending a placeholder (AGENTS.md §4.3), so this is the code an agent sees
	// when authentication is not configured.
	CodeSecretUnresolved = -32003
	// CodeTimeout means the run exceeded its budget.
	CodeTimeout = -32004
	// CodeTransport means the request never reached a response.
	CodeTransport = -32005
	// CodeUnsupported means the requested shape is refused.
	CodeUnsupported = -32006
	// CodePolicyBlocked means `.pulse-guard` refused the request before it
	// was dispatched. It is separate from transport errors because the agent
	// can fix it: a different method, a different host, or a wait.
	CodePolicyBlocked = -32007
)

// ProtocolError is a domain failure carrying its JSON-RPC code, so a tool
// handler can fail with a code an agent can branch on instead of an opaque
// string.
//
// Data is included in the JSON-RPC `error.data` payload and MUST already be
// redacted: the engine's Redactor runs over every message before it reaches
// here, and this type does not redact on the way out (AGENTS.md §3.4).
type ProtocolError struct {
	Code    int
	Message string
	Data    any
	cause   error
}

// Error implements error.
func (e *ProtocolError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s", e.Message, e.cause)
	}
	return e.Message
}

// Unwrap exposes the cause for errors.Is/As.
func (e *ProtocolError) Unwrap() error { return e.cause }

// NewProtocolError builds a coded error.
func NewProtocolError(code int, message string) *ProtocolError {
	return &ProtocolError{Code: code, Message: message}
}

// WrapProtocolError builds a coded error around a cause.
func WrapProtocolError(code int, message string, cause error) *ProtocolError {
	return &ProtocolError{Code: code, Message: message, cause: cause}
}

// WithData attaches structured detail. The value must be redacted.
func (e *ProtocolError) WithData(data any) *ProtocolError {
	e.Data = data
	return e
}

// AsProtocolError extracts a *ProtocolError from an error chain.
func AsProtocolError(err error) (*ProtocolError, bool) {
	var pe *ProtocolError
	if errors.As(err, &pe) {
		return pe, true
	}
	return nil, false
}

// errorData is the structured payload attached to a JSON-RPC error. It is
// marshalled, not rendered, so a caller can inspect it programmatically.
type errorData struct {
	Code      int    `json:"code"`
	Category  string `json:"category"`
	Detail    string `json:"detail,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// Categories describe what an agent should DO about a failure. They are stable
// strings because an agent branches on them.
const (
	CategoryArguments   = "arguments"
	CategoryParse       = "parse"
	CategorySecret      = "secret"
	CategoryTimeout     = "timeout"
	CategoryTransport   = "transport"
	CategoryUnsupported = "unsupported"
	CategoryPolicy      = "policy"
	CategoryInternal    = "internal"
)

// ToJSONRPC converts a coded error into an mcp-go JSON-RPC error.
//
// mcp-go's tool handlers return a plain error and the server assigns a code, so
// the tool path reports failures through CallToolResult{IsError:true} with the
// code carried in the payload. This conversion exists for handlers that can
// return a bare JSON-RPC error (and for the lifecycle surface).
func (e *ProtocolError) ToJSONRPC(id mcp.RequestId) mcp.JSONRPCError {
	return mcp.NewJSONRPCError(id, e.Code, e.Message, e.payload())
}

func (e *ProtocolError) payload() errorData {
	detail := e.Message
	// The cause carries what the agent needs — the failing field, the missing
	// import, the line number — and a payload that omits it reduces every
	// failure to an unactionable summary. It is already redacted by the time it
	// reaches here: the chokepoint runs in the executor.
	if e.cause != nil {
		if cause := e.cause.Error(); cause != "" && !strings.Contains(detail, cause) {
			detail = detail + ": " + cause
		}
	}
	return errorData{
		Code:     e.Code,
		Category: categoryFor(e.Code),
		Detail:   detail,
	}
}

func categoryFor(code int) string {
	switch code {
	case CodeInvalidParams, CodeInvalidRequest:
		return CategoryArguments
	case CodeParseError, CodeParseFailed:
		return CategoryParse
	case CodeSecretUnresolved:
		return CategorySecret
	case CodeTimeout:
		return CategoryTimeout
	case CodeTransport:
		return CategoryTransport
	case CodeUnsupported:
		return CategoryUnsupported
	case CodePolicyBlocked:
		return CategoryPolicy
	default:
		return CategoryInternal
	}
}

// renderErrorData marshals the structured payload for a CallToolResult. It
// returns a plain string rather than an error: a failure to marshal the error
// description must not mask the original failure.
func renderErrorData(pe *ProtocolError) string {
	b, err := json.Marshal(pe.payload())
	if err != nil {
		return `{"category":"internal","detail":"error detail could not be encoded"}`
	}
	return string(b)
}
