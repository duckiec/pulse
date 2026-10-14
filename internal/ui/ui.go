// Package ui is Pulse's interactive terminal interface: a Bubble Tea Elm
// program over the same engine the headless modes drive.
//
// # Why this package exists
//
// Moves 0–13 built every subsystem and gated each one, but nothing a human
// could watch: `pulse test` prints TAP and exits, `pulse mcp` serves an
// agent. This package renders a running request — streaming body, status,
// and microsecond transport timings — at 60 FPS without ever doing I/O on
// the render thread.
//
// # The three laws
//
//  1. The Bubble Tea thread never performs network I/O, disk reads, or
//     blocking syscalls (AGENTS.md §3.2). The engine owns the socket; the
//     UI owns pixels. Chunks cross the boundary as tea.Msg values carrying
//     UI-owned copies, and the pooled engine buffer is recycled before the
//     message is even sent.
//  2. Rendering is frame-governed, never event-driven. Chunk arrival (tens
//     of thousands per second on localhost) is coalesced by a 16 ms tick;
//     View sees at most one state per frame, and backlog is dropped with a
//     visible count rather than buffered unboundedly.
//  3. Memory is bounded by construction. The viewport keeps a 1 MiB / 10k-line
//     tail plus counters; total UI retention never exceeds the 2 MiB
//     in-flight ceiling the engine is gated on (ROADMAP.md §1.2.4).
//
// # Layering
//
// This package depends on internal/app (the composition root), internal/ast
// and internal/engine — the direction AGENTS.md §3.1 sanctions for ui. It
// holds executors only as engine.ProtocolEngine, so REST and gRPC targets
// are driven by identical UI code (ARCHITECTURE.md invariant 11). It never
// imports internal/mcp or internal/store.
package ui
