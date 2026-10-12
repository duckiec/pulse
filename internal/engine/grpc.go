package engine

import (
	"context"
	"fmt"
	"sync"

	"pulse/internal/ast"
)

// ProtocolRouter is the ProtocolEngine the UI and MCP layers actually hold.
//
// It holds one executor per protocol and picks by the request TARGET, never by
// anything the user typed into a mode selector or a file extension. That is the
// mechanism behind "the UI and MCP modes remain completely protocol-agnostic":
// neither layer holds a reference to a concrete executor, so neither can grow a
// protocol-specific branch. Adding WebSocket later means adding an executor and
// one line here — no UI or MCP change (ROADMAP.md §2.1.1/§2.2.1).
type ProtocolRouter struct {
	doc  *ast.Document
	rest ProtocolEngine
	grpc ProtocolEngine

	mu     sync.RWMutex
	closed bool
}

// RouterConfig configures a ProtocolRouter.
type RouterConfig struct {
	// REST holds options for the REST executor.
	REST []Option
	// GRPC holds options for the gRPC executor. Nil means "no gRPC executor",
	// which is a legitimate configuration: a REST-only build still satisfies
	// the ProtocolEngine contract.
	GRPC *GRPCConfig
}

// NewProtocolRouter binds both executors to one parsed document.
func NewProtocolRouter(doc *ast.Document, cfg RouterConfig) *ProtocolRouter {
	r := &ProtocolRouter{doc: doc, rest: New(doc, cfg.REST...)}
	if cfg.GRPC != nil {
		r.grpc = NewGRPCEngine(doc, *cfg.GRPC)
	}
	return r
}

// EngineFor returns the executor that serves a raw request target.
func (r *ProtocolRouter) EngineFor(rawTarget string) (ProtocolEngine, error) {
	if !IsGRPCTarget(rawTarget) {
		return r.rest, nil
	}
	if r.grpc == nil {
		return nil, fmt.Errorf("engine: %w: target %q needs a gRPC executor, and this engine was built without one",
			ErrUnsupportedMethod, rawTarget)
	}
	return r.grpc, nil
}

// Run implements ProtocolEngine: it resolves the executor from the target and
// delegates. The returned handle cancels the delegated run.
func (r *ProtocolRouter) Run(ctx context.Context, ar *ast.Request, sink Sink) RunHandle {
	r.mu.RLock()
	closed := r.closed
	r.mu.RUnlock()
	if closed {
		return r.failedRun(ar, sink, fmt.Errorf("engine: router is closed"))
	}

	raw := r.doc.Str(ar.URLStart, ar.URLEnd)
	eng, err := r.EngineFor(raw)
	if err != nil {
		return r.failedRun(ar, sink, err)
	}
	return eng.Run(ctx, ar, sink)
}

// Protocol implements ProtocolEngine. A router is a composite, so it reports
// ProtoREST as its default; the per-target executor is what actually runs.
func (r *ProtocolRouter) Protocol() ProtocolKind { return ProtoREST }

// Close tears down every executor it owns, reporting the first failure.
func (r *ProtocolRouter) Close() error {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()

	var firstErr error
	if r.grpc != nil {
		if err := r.grpc.Close(); err != nil {
			firstErr = err
		}
	}
	if err := r.rest.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// failedRun delivers a setup error through the Sink and returns an already-
// finished handle, so a caller that dispatches and waits never deadlocks on a
// run that never started.
func (r *ProtocolRouter) failedRun(_ *ast.Request, sink Sink, err error) RunHandle {
	done := make(chan struct{})
	go func() {
		defer close(done)
		sink.Failed(err)
	}()
	return RunHandle{done: done}
}

var _ ProtocolEngine = (*ProtocolRouter)(nil)
