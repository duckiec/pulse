package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"

	"pulse/internal/ast"

	"pulse/internal/engine"
	"pulse/internal/fuzz"
)

// ServerName and ServerVersion identify Pulse to the MCP client. They appear in
// the initialize handshake, so an agent can report which tool server answered.
// ServerVersion is stamped at startup from the build's main.version.
const ServerName = "pulse"

var ServerVersion = "0.0.0-dev"

// Server wraps the MCP server with the pieces the bridge owns: the executor it
// dispatches to and the codec's private stdout handle.
type Server struct {
	mcp *mcpserver.MCPServer
}

// Config is the bridge's dependency set.
//
// Every field is INJECTED. The bridge constructs no store, no redactor, no
// index and no transport, because a protocol layer that builds its own
// dependencies cannot be exercised without building all of them — and a test
// that has to construct the whole application to send one JSON-RPC message
// tests the application, not the protocol.
//
// A nil optional field is a documented degradation, never a silent absence:
// no FuzzRunner omits the fuzz tool from tools/list entirely (an agent that
// cannot see a tool will not call it, which is honest), and no Redactor means
// value scrubbing is off while header-name redaction still applies.
type Config struct {
	// Exec executes `execute_request`. Required.
	Exec Executor
	// FuzzRunner executes `execute_fuzz_run`. nil omits the tool.
	FuzzRunner FuzzRunner
	// FuzzFindings receives every failing fuzz payload (Audit 1B). Nil
	// disables persistence; production passes the store.
	FuzzFindings fuzz.FindingStore
	// Redactor scrubs agent-visible text. Optional; see the type's contract.
	Redactor Redactor
	// History backs list_collections, list_requests, get_request, get_run,
	// and get_history. Nil omits all five.
	History HistoryStore
	// Sessions backs fuzz_start, fuzz_next, and fuzz_report. Nil omits all
	// three.
	Sessions *FuzzSessions
	// Retriever binds RAG context to 5xx probe failures in fuzz_next. Nil
	// means steps carry no explanation — a stated degradation, not silence.
	Retriever Retriever
	// Options are shared register options, e.g. WithFailureBinder.
	Options []Option
	// Authoring backs the six .http authoring tools
	// (get_http_syntax_spec, create_collection, upsert_request,
	// delete_request, import_curl, validate_collection). Nil omits all six.
	Authoring *Authoring
}

// NewServer builds an MCP server exposing the Pulse tools.
//
// exec is the only thing it knows about the domain: the bridge validates
// arguments, dispatches, and formats, and never reaches past Executor into the
// AST or the engine.
func NewServer(exec Executor) (*Server, error) {
	return NewServerWithConfig(Config{Exec: exec})
}

// NewServerWithConfig builds a server from an injected dependency set.
//
// This is the constructor the composition root uses. [NewServer] is the
// single-tool convenience kept for tests and for the degraded boot path, where
// there is no application to wire.
func NewServerWithConfig(cfg Config) (*Server, error) {
	if cfg.Exec == nil {
		return nil, errors.New("mcp: Config.Exec is required")
	}
	srv := mcpserver.NewMCPServer(ServerName, ServerVersion,
		// Advertise no sampling and no roots: Pulse executes requests, it does
		// not call back into the agent. Claiming capabilities the bridge does
		// not implement invites an agent to depend on them.
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithRecovery(), // an agent must not be able to crash the server
	)
	if err := RegisterTools(srv, cfg.Exec, cfg.Options...); err != nil {
		return nil, err
	}
	if cfg.FuzzRunner != nil {
		opts := append(append([]Option{}, cfg.Options...), WithFuzzFindings(cfg.FuzzFindings))
		if err := RegisterFuzzTool(srv, cfg.FuzzRunner, cfg.Redactor, opts...); err != nil {
			return nil, err
		}
	}
	if err := RegisterHistoryTools(srv, cfg.History, cfg.Redactor); err != nil {
		return nil, err
	}
	if err := RegisterIterativeFuzzTools(srv, cfg.Sessions, cfg.FuzzRunner, cfg.Redactor, cfg.Retriever, cfg.FuzzFindings); err != nil {
		return nil, err
	}
	if err := RegisterAuthoringTools(srv, cfg.Authoring); err != nil {
		return nil, err
	}
	return &Server{mcp: srv}, nil
}

// MCPServer exposes the underlying server, for tests and for callers that need
// HandleMessage directly rather than over stdio.
func (s *Server) MCPServer() *mcpserver.MCPServer { return s.mcp }

// HandleMessage processes one JSON-RPC message. It is the seam the protocol gate
// drives, and it is exactly what the stdio loop drives too — so a test exercises
// the production path rather than a parallel implementation of it.
func (s *Server) HandleMessage(ctx context.Context, raw json.RawMessage) mcp.JSONRPCMessage {
	return s.mcp.HandleMessage(ctx, raw)
}

// Serve runs the stdio loop until the input ends or the context is cancelled.
//
// The stdout handle is a parameter, never os.Stdout: the caller captured the
// real channel before Lock() replaced it, and this is the only component
// permitted to write to it (ROADMAP.md §3.1.1).
//
// The error logger is bound to stderr explicitly. mcp-go's default logger writes
// to stderr, but relying on that default would make a protocol requirement
// contingent on a library's current behaviour.
func (s *Server) Serve(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	stdio := mcpserver.NewStdioServer(s.mcp)
	stdio.SetErrorLogger(log.New(Stderr, "pulse-mcp: ", 0))
	return stdio.Listen(ctx, stdin, stdout)
}

// ServeLocked runs the stdio session against an ALREADY-captured stdout.
//
// Splitting the boot in two — Lock() in main, ServeLocked() here — is
// deliberate. Lock must run before the mcp package is used so that no package
// has a chance to capture the unlocked os.Stdout; keeping the two halves in
// different files makes the ordering visible at the call site instead of hiding
// it behind a convenience wrapper that looks safe to call late.
//
// Returns the process exit code (0 on a clean end of session) and any error.
func ServeLocked(ctx context.Context, stdout io.Writer, stdin io.Reader) (int, error) {
	srv, err := NewServer(DefaultExecutor())
	if err != nil {
		Logf("cannot build the MCP server: %v", err)
		return 1, err
	}

	err = srv.Serve(ctx, stdin, stdout)
	switch {
	case err == nil, errors.Is(err, io.EOF):
		// A clean stdin EOF is how an agent exits: success, not failure.
		return 0, nil
	case errors.Is(err, context.Canceled):
		return 0, nil
	default:
		Logf("mcp session ended: %v", err)
		return 1, err
	}
}

// DefaultExecutor builds a bare executor: the real AST parser and the real
// engine, dispatched by target scheme, with NO redactor.
//
// This is the degraded/test path, not production: the composition root
// (App.MCPServer) supplies the concrete secrets.Redactor, and serving
// through this default redacts by HEADER NAME only — a resolved secret in
// a response body would reach the model unredacted. The gap is kept
// visible here so no caller mistakes the convenience for the wired path.
func DefaultExecutor() Executor {
	// Even the degraded path carries the default guard. This executor has no
	// redactor and no resolver, but it does have a network: leaving the
	// blast-radius policy out here would make the convenience path the one an
	// agent could use to reach a production host.
	guard := NewGuard(DefaultGuardPolicy())
	return &EngineExecutor{
		NewEngine: func(doc *ast.Document) engine.ProtocolEngine {
			return engine.NewProtocolRouter(doc, engine.RouterConfig{
				REST: []engine.Option{engine.WithPolicyGate(guard)},
				GRPC: &engine.GRPCConfig{Resolver: nil},
			})
		},
	}
}
