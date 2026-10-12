package engine

import (
	"context"

	"pulse/internal/ast"
)

// ProtocolKind names the wire protocol an executor speaks.
type ProtocolKind uint8

const (
	ProtoREST ProtocolKind = iota
	ProtoSSE
	ProtoGRPC
)

// String implements fmt.Stringer.
func (p ProtocolKind) String() string {
	switch p {
	case ProtoSSE:
		return "SSE"
	case ProtoGRPC:
		return "GRPC"
	default:
		return "REST"
	}
}

// ProtocolEngine is the single execution contract every transport satisfies.
//
// Both executors take the SAME AST node and deliver to the SAME Sink established
// in Goal 1, so a .http line targeting a REST endpoint and one targeting a gRPC
// method are driven by identical UI and MCP code: the difference is resolved
// entirely below this interface. That is what "protocol-agnostic" means here —
// not that the protocols are similar, but that no layer above this interface
// can tell them apart (ROADMAP.md §2.2.1).
type ProtocolEngine interface {
	// Protocol reports which wire this executor speaks. A composite router
	// reports its default; the per-target executor is what actually runs.
	Protocol() ProtocolKind
	// Run starts req and streams its response to sink, returning a handle that
	// cancels the run and signals completion.
	Run(ctx context.Context, req *ast.Request, sink Sink) RunHandle
	// Close releases engine-owned resources (connections, file descriptors).
	Close() error
}

// Compile-time proof that both executors satisfy the contract. This is the
// mechanism that keeps the dual-mode layers protocol-agnostic: if a future
// executor drifts from the interface, the build stops here rather than in the UI.
var (
	_ ProtocolEngine = (*Engine)(nil)
	_ ProtocolEngine = (*GRPCEngine)(nil)
)

// ProtocolForTarget infers the protocol from a request target scheme. A
// grpc:// or grpcs:// target routes to the gRPC executor; everything else is
// REST. Inference is a property of the target, never of the UI.
func ProtocolForTarget(rawURL string) ProtocolKind {
	if IsGRPCTarget(rawURL) {
		return ProtoGRPC
	}
	return ProtoREST
}
