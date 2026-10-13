package mcp

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// This file implements the stdout isolation boundary required by AGENTS.md §3.3
// and ROADMAP.md §3.1.1.
//
// In MCP mode stdout is not a stream — it is a WIRE. Every byte on it is one
// JSON-RPC 2.0 frame, and a single stray `fmt.Println` in a dependency or in a
// transitive package corrupts the protocol and crashes the connected agent.
//
// The defence has two layers, because one layer is not enough:
//
//  1. Lock() CAPTURES the real stdout before anything else initializes, then
//     replaces os.Stdout. Stray writes are discarded (/dev/null), so they cannot
//     corrupt the wire. This is the production path.
//  2. The TRIPWIRE validates every byte that crosses os.Stdout against
//     JSON-RPC 2.0 framing and reports any violation as fatal. This is the
//     audit path (PULSE_STDOUT_AUDIT=1), and it exists because layer 1 hides
//     the very bug it is meant to catch: with writes silently discarded, a
//     hygiene violation is invisible until a client reports malformed JSON.
//
// Layer 2 is what turns "the output was empty" into "something wrote to stdout
// and it was not JSON-RPC".

// StdoutLock is the handle returned by [Lock]. Real is the only writer the
// JSON-RPC codec is permitted to use; everything else must go to stderr.
//
// The name is spelled out because a type and a function cannot share an
// identifier in Go, and `Lock`/`Lock` would have been the pair otherwise.
type StdoutLock struct {
	// Real is the captured stdout, handed exclusively to the JSON-RPC codec.
	Real *os.File
	// Tripwire is non-nil in audit mode and records violations.
	Tripwire *Tripwire

	prev    *os.File
	devnull *os.File
	once    sync.Once
}

// Restore puts os.Stdout back. It is idempotent and safe to defer: a deferred
// Restore is the correct shape, because the process must give stdout back
// before it exits even if the server returned an error.
func (l *StdoutLock) Restore() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.prev != nil {
			os.Stdout = l.prev
		}
		if l.devnull != nil {
			_ = l.devnull.Close()
		}
	})
}

// Lock captures the real stdout and replaces os.Stdout.
//
// It MUST be the first thing MCP mode does, before any subsystem initializes:
// a dependency that captures os.Stdout at package init has already missed the
// swap by the time we get here, and the audit tripwire is what catches that case
// (ROADMAP.md §3.1.1).
//
// audit selects the tripwire. Leave it off in production: a stray write there is
// discarded rather than fatal, because the wire is what matters, not the noise.
func Lock(audit bool) (*StdoutLock, error) {
	real := os.Stdout
	l := &StdoutLock{Real: real, prev: real}

	if audit {
		l.Tripwire = NewTripwire()
		os.Stdout = tripwireFile(l.Tripwire)
		return l, nil
	}

	// Discard rather than fail: the production path guarantees the wire stays
	// clean even if something writes to stdout, and the audit path is how we
	// learn that something did.
	// os.Stdout is an *os.File, so the discard target must be one too. If
	// /dev/null cannot be opened there is no safe substitute: any real file
	// would put Pulse's discard bytes on a disk an agent could later read, and
	// leaving stdout alone would hand the wire to every stray writer. Failing
	// loudly is the only outcome that keeps the invariant intact.
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("pulse: cannot open %s to isolate stdout: %w", os.DevNull, err)
	}
	l.devnull = f
	os.Stdout = f
	return l, nil
}

// tripwireFile wraps the tripwriter in an *os.File so it can be assigned to
// os.Stdout (whose type is fixed). os.Pipe is the only portable way to get a
// writable *os.File around an arbitrary io.Writer.
func tripwireFile(t *Tripwire) *os.File {
	pr, pw, err := os.Pipe()
	if err != nil {
		// If a pipe cannot be created we cannot represent the tripwire as an
		// *os.File. Returning /dev/null would silently disable the audit, which
		// is the failure mode this whole file exists to prevent — so make the
		// condition loud instead of quiet.
		fmt.Fprintln(os.Stderr, "pulse: FATAL: cannot create the stdout audit pipe; refusing to run MCP mode unaudited")
		panic("pulse: stdout audit pipe unavailable")
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				t.Write(buf[:n])
			}
			if err != nil {
				_ = pr.Close()
				return
			}
		}
	}()
	// Keep pw referenced so it is never garbage collected out from under
	// os.Stdout; it is closed only when the process exits.
	_ = pw
	return pw
}

// Violation is one detected breach of the stdout contract.
type Violation struct {
	// At is when the violation was observed.
	At time.Time
	// Payload is the offending bytes, truncated for safety.
	Payload string
	// Reason explains what was wrong with them.
	Reason string
}

// String renders a violation for stderr.
func (v Violation) String() string {
	return fmt.Sprintf("%s (payload: %s)", v.Reason, v.Payload)
}

// Tripwire is the audit-mode replacement for os.Stdout. It validates every byte
// that crosses the boundary and reports anything that is not a JSON-RPC 2.0
// frame.
//
// Framing note: the MCP stdio transport is newline-delimited JSON, not
// LSP-style Content-Length headers, so a complete frame is a complete LINE. A
// writer may split a frame across several Write calls (and a valid frame may be
// large), so bytes are buffered until a newline arrives before being judged.
type Tripwire struct {
	mu     sync.Mutex
	buf    []byte
	seen   []Violation
	maxLen int

	// OnViolation runs for each violation. The default writes a diagnostic to
	// STDERR and panics — protocol corruption is fatal by design, because a
	// client that has already seen a corrupt frame has lost the session and
	// silently continuing would hand it a second, worse failure.
	OnViolation func(Violation)
}

// DefaultMaxViolationLen bounds how much of a rogue payload is retained.
const DefaultMaxViolationLen = 512

// NewTripwire returns a tripwire with the default fatal handler.
func NewTripwire() *Tripwire {
	t := &Tripwire{maxLen: DefaultMaxViolationLen}
	t.OnViolation = FatalViolation
	return t
}

// FatalViolation reports a violation to stderr and panics.
//
// The panic value is the violation itself so a crash log carries the offending
// payload; nothing about it is ever written to stdout, because stdout is
// precisely the channel that is already broken.
func FatalViolation(v Violation) {
	fmt.Fprintf(os.Stderr, "pulse: FATAL: stdout hygiene violation: %s\n", v)
	panic(v)
}

// Write implements io.Writer.
func (t *Tripwire) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	for {
		i := indexNewline(t.buf)
		if i < 0 {
			break
		}
		line := t.buf[:i]
		t.buf = t.buf[i+1:]
		t.judge(line)
	}
	// An unterminated remainder is only a violation once it is implausibly
	// long: a partially-written valid frame must not be judged.
	if len(t.buf) > t.maxLen*8 {
		t.judge(t.buf)
		t.buf = t.buf[:0]
	}
	return len(p), nil
}

// judge validates one complete line. The caller holds the lock.
func (t *Tripwire) judge(line []byte) {
	if v, bad := validateFrame(line); bad {
		v.At = time.Now()
		t.seen = append(t.seen, v)
		if t.OnViolation != nil {
			t.OnViolation(v)
		}
	}
}

// Recorder is a thread-safe collector for violations, for tests and for the
// audit gate.
//
// It exists because the tripwire calls its handler from the pipe-draining
// goroutine while a test reads from another. Sharing a bare []Violation between
// those two is a data race — which is exactly what -race found — and the fix is
// to give the storage the synchronization the concurrency requires rather than
// to hope the reads are far enough apart.
type Recorder struct {
	mu  sync.Mutex
	got []Violation
}

// Handle satisfies the OnViolation signature and appends safely.
func (r *Recorder) Handle(v Violation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, v)
}

// Violations returns a copy of everything recorded.
func (r *Recorder) Violations() []Violation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Violation(nil), r.got...)
}

// Count reports how many violations were recorded.
func (r *Recorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.got)
}

// Violations returns a copy of everything detected so far.
func (t *Tripwire) Violations() []Violation {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Violation(nil), t.seen...)
}

// Clean reports whether the boundary has held.
func (t *Tripwire) Clean() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.seen) == 0
}

// validateFrame reports whether line is a well-formed JSON-RPC 2.0 frame.
//
// It checks the envelope, not the semantics: `jsonrpc` must be exactly "2.0",
// `id` must be a string, a number, or null (never a bool or an object), and the
// frame must be a JSON OBJECT. That is enough to separate "the JSON-RPC codec
// wrote this" from "someone printed a debug line", which is the distinction the
// audit exists to make.
func validateFrame(line []byte) (Violation, bool) {
	trimmed := strings.TrimSpace(string(line))
	shown := trimmed
	if len(shown) > DefaultMaxViolationLen {
		shown = shown[:DefaultMaxViolationLen] + "…"
	}
	if trimmed == "" {
		return Violation{Payload: shown, Reason: "an empty line crossed stdout; the JSON-RPC transport is newline-delimited JSON"}, true
	}

	var envelope struct {
		JSONRPC json.RawMessage `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  *string         `json:"method"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return Violation{
			Payload: shown,
			Reason:  "stdout carried bytes that are not JSON: " + err.Error() + " (a stray fmt.Print here corrupts every connected agent)",
		}, true
	}

	if len(envelope.JSONRPC) == 0 {
		return Violation{Payload: shown, Reason: `stdout carried a JSON object with no "jsonrpc" field; it is not a JSON-RPC frame`}, true
	}
	var version string
	if err := json.Unmarshal(envelope.JSONRPC, &version); err != nil || version != "2.0" {
		return Violation{
			Payload: shown,
			Reason:  fmt.Sprintf(`stdout carried "jsonrpc": %s, want "2.0"`, strings.TrimSpace(string(envelope.JSONRPC))),
		}, true
	}
	if len(envelope.ID) > 0 {
		var probe any
		if err := json.Unmarshal(envelope.ID, &probe); err != nil {
			return Violation{Payload: shown, Reason: `stdout carried a frame with an unreadable "id"`}, true
		}
		switch probe.(type) {
		case string, float64, nil:
		default:
			return Violation{
				Payload: shown,
				Reason:  fmt.Sprintf(`stdout carried a frame whose "id" is a %T; JSON-RPC ids are strings, numbers, or null`, probe),
			}, true
		}
	}
	if envelope.Method == nil && len(envelope.Result) == 0 && len(envelope.Error) == 0 {
		return Violation{
			Payload: shown,
			Reason:  `stdout carried a JSON-RPC frame with neither "method", "result", nor "error"`,
		}, true
	}
	return Violation{}, false
}

func indexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

// Stderr is where everything that is not the JSON-RPC wire must go. It is
// declared here so every MCP-mode diagnostic has one obvious home.
var Stderr io.Writer = os.Stderr

// Logf writes a diagnostic to stderr, never to stdout.
func Logf(format string, args ...any) {
	fmt.Fprintf(Stderr, "pulse: "+format+"\n", args...)
}
