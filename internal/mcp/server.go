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
)

// ServerName and ServerVersion identify Pulse to the MCP client. They appear in
// the initialize handshake, so an agent can report which tool server answered.
const (
	ServerName    = "pulse"
	ServerVersion = "0.0.0-dev"
)

// Server wraps the MCP server with the pieces the bridge owns: the executor it
// dispatches to and the codec's private stdout handle.
type Server struct {
	mcp *mcpserver.MCPServer
}

// NewServer builds an MCP server exposing the Pulse tools.
//
// exec is the only thing it knows about the domain: the bridge validates
// arguments, dispatches, and formats, and never reaches past Executor into the
// AST or the engine.
func NewServer(exec Executor) (*Server, error) {
	srv := mcpserver.NewMCPServer(ServerName, ServerVersion,
		// Advertise no sampling and no roots: Pulse executes requests, it does
		// not call back into the agent. Claiming capabilities the bridge does
		// not implement invites an agent to depend on them.
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithRecovery(), // an agent must not be able to crash the server
	)
	if err := RegisterTools(srv, exec); err != nil {
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

// DefaultExecutor builds the production executor: the real AST parser and the
// real engine, dispatched by target scheme.
//
// Redactor is nil here and that is an OPEN GAP, not a settled decision: the
// interface exists (Goal 1, ADL-0017) and the chokepoint IS applied in
// collectingSink.outcome, but the concrete secrets.Redactor has no composition
// root supplying it yet because internal/app has no caller. Until it does, MCP
// mode redacts by HEADER NAME only — a resolved secret in a response body would
// reach the model unredacted. Tracked as an open TASKS.md item rather than left
// to be discovered by an agent that already has the value.
func DefaultExecutor() Executor {
	return &EngineExecutor{
		NewEngine: func(doc *ast.Document) engine.ProtocolEngine {
			return engine.NewProtocolRouter(doc, engine.RouterConfig{
				GRPC: &engine.GRPCConfig{Resolver: nil},
			})
		},
	}
}
