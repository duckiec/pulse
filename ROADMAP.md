# ROADMAP.md — Pulse Implementation Blueprint

| Field | Value |
|---|---|
| Document | Implementation Blueprint (architectural, not calendrical) |
| Product | Pulse |
| Version | 1.0.0 |
| Status | Authoritative |
| Governed by | `AGENTS.md` (all directives herein subordinate to AGENTS.md) |
| Supersedes | Scope exclusions listed in `PRD.md` §7 (see MEMORY.md ADL-0008) |
| Companion docs | `PRD.md`, `ARCHITECTURE.md`, `DESIGN_SYSTEM.md`, `TASKS.md`, `MEMORY.md` |
| Last updated | 2026-10-11 |

---

# Section 0 — Operating Doctrine

## 0.1 Purpose & Authority

This document defines the **sequence of implementation goals** for Pulse: four
massive, hyper-compressed engineering objectives exploded to their finest
architectural grain. It contains no schedules, no estimates, and no milestones keyed
to elapsed time. Progress is measured exclusively by **exit gates**: each goal closes
only when its gate conditions pass under the verification model in `AGENTS.md`.

This document is the scope authority for Pulse. Where `PRD.md` §7 conflicts with a
goal herein, **this document wins** (supersession recorded in `MEMORY.md` ADL-0008).
Where any content conflicts with `AGENTS.md` discipline, `AGENTS.md` wins.

## 0.2 Numbering Discipline (The X.X.X Limit)

- Every goal is decomposed into **at most three levels**: `X` (goal), `X.X`
  (initiative), `X.X.X` (micro-architectural bullet).
- `X.X.X.X` numbering is **forbidden**. Depth is expressed by finer `X.X.X`
  bullets, never by deeper numbering.
- Every `X.X.X` bullet MUST be a **verifiable artifact**: a technical decision, an
  interface contract, a memory-management rule, or a schema definition — never prose
  aspiration.
- Every `X.X.X` bullet carries the same four annotations: **Prereq** (hard
  prerequisites), **Contract** (the Go signature or wire format), **Challenge**
  (the architectural hazard), **Verify** (the mechanical proof required).

## 0.3 Invariant Stack (Non-Negotiable Across ALL Goals)

| # | Invariant | Source |
|---|---|---|
| I-1 | Go 1.27 language baseline; stdlib-first (`iter.Seq`, `context.WithoutCancel`, `net/http` ServeMux patterns) | AGENTS.md §2.2 |
| I-2 | `CGO_ENABLED=0` for every target and every CI job | AGENTS.md §2.1 |
| I-3 | Local-first: no telemetry, no phone-home, no Pulse-operated server, works fully offline | AGENTS.md §1 |
| I-4 | Dual-mode TUI + MCP, mode immutable per process, stdout reserved in MCP mode | AGENTS.md §3.3 |
| I-5 | Zero network I/O / disk reads on the Bubble Tea UI thread | AGENTS.md §3.2 |
| I-6 | `io.TeeReader` + channels + `sync.Pool` for all heavy streaming | AGENTS.md §2.7 |
| I-7 | Fail-closed secrets: keychain → ephemeral `.env` → hard error (exit 3); no dummy tokens | AGENTS.md §4.3 |
| I-8 | Secrets never persisted to disk or the SQLite store | AGENTS.md §3.4 |
| I-9 | Mutation rule: every fix proven red→green before acceptance | AGENTS.md §4.2 |
| I-10 | Honest dispositions: PASSED / FAILED / BLOCKED / NOT RUN with reasons | AGENTS.md §4.5 |
| I-11 | Documentation sync: code and doc state land in the same commit | AGENTS.md §4.6 |

## 0.4 Dependency Governance

New dependencies are admitted ONLY by appearing in this registry (`ARCHITECTURE.md`
§7 is updated in the same commit, and a `MEMORY.md` ADL is appended). The rule of
thumb: the standard library handles it, or the package is pure Go and no sanctioned
counterpart exists.

| Package | Purpose | Purity |
|---|---|---|
| `modernc.org/sqlite` | SQLite engine, zero CGO | pure Go |
| `github.com/charmbracelet/{bubbletea,lipgloss,bubbles,huh}` | TUI stack | pure Go |
| `github.com/mark3labs/mcp-go` | MCP JSON-RPC stdio server | pure Go |
| `github.com/coder/websocket` | WebSocket client (RFC 6455) | pure Go |
| `google.golang.org/grpc`, `google.golang.org/protobuf` | gRPC + dynamic messages | pure Go |
| `github.com/cilium/ebpf` | eBPF program loading (opt-in build tag) | pure Go |
| `github.com/tetratelabs/wazero` | WebAssembly runtime | pure Go |
| `github.com/owulveryck/onnx-go` | Local embedding inference | pure Go |
| `github.com/fsnotify/fsnotify` | Filesystem watching for indexer | pure Go |
| `github.com/grandcat/zeroconf` | mDNS/Bonjour discovery | pure Go |
| `github.com/getkin/kin-openapi` | OpenAPI document parsing | pure Go |
| `github.com/santhosh-tekuri/jsonschema/v6` | MCP payload validation | pure Go |
| `golang.org/x/{term,sys,net}` | isatty, advapi32, websocket deps | pure Go |
| stdlib only where possible (`crypto/ecdh`, `crypto/aes`, `crypto/cipher`, `crypto/ed25519`, `crypto/tls`, `encoding/binary`, `iter`, `sync`, `context`) | vault PKI, CRDT, everything else | — |

## 0.5 Verification Model

Every goal declares its own gates. The gates draw from a fixed menu:

| Gate | Mechanism |
|---|---|
| G-alloc | `go test -bench . -benchmem` with an 0-alloc/op assertion |
| G-frame | p99 `View` duration < 16ms under synthetic 50MB stream load |
| G-mut | Mutation proof: the regression test provably fails without the fix |
| G-hyg | CI analyzer: zero stdout writes outside the JSON-RPC layer in MCP paths |
| G-secret | CI scanner: forbidden columns/strings absent from store and repo |
| G-honest | Every test dispositioned PASSED / FAILED / BLOCKED / NOT RUN |
| G-doc | Same-commit documentation sync per AGENTS.md §4.6 |

---

# Goal 1 — The Core Foundation & Zero-Trust State

**Intent.** Build the substrate every later goal stands on: a zero-allocation `.http`
AST that streams through `iter.Seq`; a render loop that never touches I/O while a
50MB+ response pours through `io.TeeReader` into pooled channel chunks; and a secret
substrate that makes spillage structurally impossible, not merely discouraged.

## 1.1 The `.http` AST Engine (Zero-Allocation, `iter.Seq`)

### 1.1.1 Lexer state machine and token set

- **Prereq:** Go 1.27; `internal/ast` package importing nothing but stdlib; source
  buffer ownership documented (caller-owned `[]byte`, never copied).
- **Contract:**
  ```go
  package ast

  type TokenKind uint8
  const (
      TokEOF TokenKind = iota
      TokSeparator // ###
      TokComment   // # ...
      TokVariable  // @name = value
      TokMethod; TokURL; TokHeaderName; TokHeaderValue
      TokHTTPVersion
      TokBodyOpen; TokBodyClose // { } or < >
      TokInterp  // {{name}}
      TokAssert  // # @assert ...
      TokNewline
      TokIllegal
  )

  type Token struct {
      Kind TokenKind
      Start, End int32 // byte offsets into the source buffer
      Line, Col  int32 // 1-based, for diagnostics
  }

  type Lexer struct { src []byte; off int; line, col int32 }
  func NewLexer(src []byte) *Lexer
  func (l *Lexer) Next() (Token, error) // TokIllegal + *ErrSyntax on invalid byte
  func (l *Lexer) Reset(src []byte)     // reuse without allocation
  ```
- **Challenge:** The two body dialects (`{...}` JSON, `<...>` XML) and the raw
  fallback make the line-based grammar context-sensitive; a naive scanner allocates a
  token struct per byte. The lexer therefore advances by states and emits only
  boundary tokens, carrying offsets into the source instead of slices where the
  consumer needs text lazily.
- **Verify:** G-alloc on `BenchmarkLexer` (0 allocs/op over a 10k-line corpus);
  fuzz corpus (`testing.F` in Go 1.27) proving `TokIllegal` is never emitted with a
  zero-length span and `Next` always terminates.

### 1.1.2 Lexer states (explicit transition table)

- **Prereq:** 1.1.1 token set; line-ending normalization policy decided (accept
  `\n`, `\r\n`, lone `\r`; record original offsets, expose normalized views).
- **Contract:**
  ```go
  type lexState uint8
  const (
      stLineStart   lexState = iota // #, @, ###, method, or blank
      stRequestLine                 // method SP URL SP HTTP/x.y
      stHeaders                      // name: value
      stBodyJSON                     // brace-aware depth counting
      stBodyXML                      // tag-aware
      stBodyRaw                      // terminators: blank line + ### or EOF
      stDone
  )
  func (l *Lexer) step() lexState // pure function of (state, byte)
  ```
- **Challenge:** Brace counting inside JSON bodies must ignore braces inside JSON
  strings and escapes; XML bodies must ignore `<` inside attribute values and
  comments. The state machine keeps a small quote/escape sub-state rather than
  re-scanning, preserving the single-pass, offset-only guarantee.
- **Verify:** Table-driven unit tests per state transition including adversarial
  inputs (unterminated string, `{"a":"}"}`), each asserting the exact emitted token
  sequence; mutation gate G-mut applied to every brace-counting fix.

### 1.1.3 Parser: recursive descent over an arena

- **Prereq:** 1.1.2 token stream; arena allocator (1.1.5); AST node definitions.
- **Contract:**
  ```go
  type Node interface{ Span() (int32, int32) }

  type Document struct {
      Path string
      Reqs []Request          // slice allocated from arena
      Vars []Variable         // @name = value, file-scope
  }
  type Request struct {
      Name     string    // from optional '# @name' or method+line
      Method   string
      URL      string    // may contain {{var}} anchors
      Headers  []Header
      Body     Body      // Kind: JSON|XML|Raw|None + offsets
      Asserts  []Assert  // # @assert ... lines
  }
  type Header struct{ Name, Value string } // Value may contain {{var}}
  type Body struct{ Kind BodyKind; Start, End int32 }
  type Variable struct{ Name, Value string }
  type Assert struct{ Expr string; Line int32 }

  func Parse(path string, src []byte) (*Document, error)
  func (d *Document) Release() // frees the arena; Document unusable after
  ```
- **Challenge:** Two-level lookahead is required to distinguish a request header
  from a file variable and to detect the implicit request terminator. The parser
  holds exactly one token of lookahead in the stack frame (no token buffer slice),
  and recursive descent never re-scans bytes: every `Name`/`Value` string is
  materialized once, only for tokens the consumer will read, via a single
  arena-backed `string(unsafe)`-free conversion (safe `string(src[a:b])` but only
  once per syntactic element).
- **Verify:** Golden-file corpus: every `.http` fixture parses to a `cmp.Diff`-clean
  expected AST snapshot; G-alloc on `BenchmarkParseCached`; `Parse` returns
  `(*Document)(nil), err` on every malformed input (never a partial AST — AGENTS.md §4.4).

### 1.1.4 `iter.Seq` traversal and lazy consumption

- **Prereq:** 1.1.3 AST; consumers registered: `internal/engine` (execution),
  `internal/ui` (virtualized tree), `internal/mcp` (tool listing).
- **Contract:**
  ```go
  func (d *Document) Requests() iter.Seq[*Request]
  func (d *Document) Variables() iter.Seq2[string, string]
  // Streaming parse for very large files / MCP listing without full materialization:
  func ParseSeq(path string, src []byte) iter.Seq2[*Request, error]
  ```
- **Challenge:** `ParseSeq` must yield each request as soon as its terminating
  blank line + `###` separator is consumed, while the document header variables must
  still apply retroactively to already-yielded requests. Resolution: file variables
  are scanned in a cheap pre-pass (offsets only, zero allocation) because they can
  only appear before the first request; requests then stream.
- **Verify:** Property test: for the corpus, `ParseSeq` yields a sequence identical
  to `Parse(...).Requests()`; memory test: parsing a 10,000-request fixture holds
  peak RSS within 2× the source size (no per-request heap growth).

### 1.1.5 Zero-allocation memory boundaries and the arena

- **Prereq:** 1.1.3 node layout; benchmark harness in `internal/ast`.
- **Contract:**
  ```go
  type Arena struct{}
  func (a *Arena) alloc(n int) []byte   // 8-byte aligned bump; grows by doubling
  func (a *Arena) bytes() int          // observability for tests
  // Package-level pool for the source buffer copies that must not escape:
  var srcPool = sync.Pool{New: func() any { b := make([]byte, 0, 64<<10); return &b }}
  ```
- **Challenge:** The arena must never hold the file contents itself (that would
  double memory for large collections) — it holds only `Request`/`Header` structs
  and their materialized strings. Strings referencing the source buffer are
  `unsafe.String`-free views only when the source is immutable for the Document
  lifetime; file-backed documents copy the source once into an arena slab.
- **Verify:** G-alloc: `benchmark` assertion of 0 allocs/op for lex+tokens, and a
  bounded, documented allocation budget (arena growth log) for parse; `testing.B`
  report `allocs/op` and `B/op` tracked in CI as regressions-fail.

### 1.1.6 Error diagnostics and fail-closed recovery

- **Prereq:** 1.1.1 `ErrSyntax`; resync points defined (`###` at line start, EOF).
- **Contract:**
  ```go
  type ErrSyntax struct {
      Path     string
      Line, Col int32
      Expected string // lexeme class
      Found    string // actual lexeme
  }
  func (e *ErrSyntax) Error() string // "path:line:col: expected X, found Y"
  // Resync scans forward to the next separator to collect further diagnostics,
  // but the Document is always discarded on error.
  func ParseDiag(path string, src []byte) ([]ErrSyntax, error)
  ```
- **Challenge:** Error recovery must never masquerade as success (AGENTS.md §4.4),
  yet editors and the TUI tree need *all* errors, not just the first. `ParseDiag`
  resyncs to accumulate, but `Parse` still returns `nil, err`; the TUI displays the
  full diagnostic list in the status/error pane and the request stays unexecutable.
- **Verify:** Corpus of malformed fixtures each asserting the exact diagnostic tuple;
  mutation gate on every resync fix; exit-code mapping test (parse failure → 2 in
  `pulse test` and MCP `run-request` error objects).

### 1.1.7 Variable anchor model (deferred interpolation)

- **Prereq:** 1.1.3 AST; `internal/secrets` resolver contract (1.3.2).
- **Contract:** `{{name}}` occurrences are preserved as `TokInterp` anchors with
  offsets; the AST stores header/URL values as raw strings; interpolation happens in
  `internal/engine` at execution time via `Resolver.Resolve(name)`; unresolved names
  produce `*ErrUnresolved{Name, Line}` and halt the run (exit 3 semantics).
- **Challenge:** The AST must remain pure (parse-time purity is what makes the
  zero-alloc and caching guarantees provable). Interpolation is therefore modeled as
  a separate, allocation-charged pass that only materializes a `*http.Request` — its
  cost is benchmarked separately from the parse.
- **Verify:** Round-trip test: AST → interpolated request → recorded run matches the
  same fixture executed by an independent hand-written request; unresolved-variable
  test asserts fail-closed (no empty-string substitution).

## 1.2 The Decoupled Render Loop & Zero-Blocking Streaming

### 1.2.1 Elm synchronization primitives

- **Prereq:** `bubbletea` program; `internal/engine` sink interface; `DESIGN_SYSTEM.md` §2.
- **Contract:**
  ```go
  type Engine interface {
      Run(ctx context.Context, req *ast.Request, sink Sink) RunHandle
  }
  type Sink interface {
      Chunk(*Chunk)      // called from the dispatch goroutine only
      Done(*RunResult)   // terminal, exactly once
      Failed(error)      // terminal, exactly once, mutually exclusive with Done
  }
  type RunHandle struct{ cancel context.CancelFunc; done chan struct{} }
  ```
  `Run` returns no error: every outcome — success, transport failure, or user
  cancellation — is delivered through the sink, so a caller has exactly one place
  to observe results and cannot accidentally ignore one. `RunHandle` exposes
  `Cancel`, `Done() <-chan struct{}`, `Wait`, and `WaitTimeout`.
  The Bubble Tea side: `func fetch(r *ast.Request) tea.Cmd` spawns the goroutine and
  returns `tea.Msg` values (`chunkMsg`, `runDoneMsg`, `runFailedMsg`); `Update`
  mutates the model only; `View` performs no I/O and no time reads.
  *(Signature revised in Move 3: the contract originally read `(*RunHandle, error)`.
  See MEMORY.md ADL-0016 §interface.)*
- **Challenge:** The engine's `Sink` callbacks run on the engine goroutine while the
  model is owned by the UI goroutine. Hand-off is exclusively via channel-sent
  messages (bubbletea's `Msg` channel already provides the memory barrier); the
  sink therefore never touches model memory directly — `Chunk` pushes to an
  internal `chan *Chunk` consumed by the `tea.Cmd` goroutine that emits msgs.
- **Verify:** `testing/synctest` test asserting the UI goroutine performs zero
  syscalls/net calls during a simulated stream (a fault-injecting `net.Conn` records
  any read from the UI goroutine); race detector clean across the full suite.

### 1.2.2 `io.TeeReader` multiplexing

- **Prereq:** 1.2.1 sink; `net/http` response body; SHA-256 via `crypto/sha256`.
- **Contract:**
  ```go
  h := sha256.New()
  body := io.TeeReader(resp.Body, h)   // passive mirror: O(1) memory, never blocks
  bufp := chunkPool.Get().(*[]byte)
  n, err := body.Read((*bufp)[:cap(*bufp)])
  ```
- **Challenge:** The tee mirror must never apply backpressure or error the read
  path: `sha256` `Write` cannot fail and cannot block, so the mirror is safe. The
  mirror's total state is the 32-byte digest — the 50MB response never exists in
  memory. The reader loop maps `io.EOF` to a final `Chunk{Final: true}` and calls
  `Done` with status/timing/digest.
- **Verify:** G-alloc on the streaming benchmark (0 allocs/op steady state after
  warmup); digest correctness test against a same-payload reference run.

### 1.2.3 Chunk sizing and `sync.Pool` lifecycle

- **Prereq:** 1.2.2 read loop; content-type awareness.
- **Contract:**
  ```go
  var chunkPool = sync.Pool{New: func() any { b := make([]byte, 0, 32<<10); return &b }}
  type Chunk struct { Buf *[]byte; N int; Final bool }

  const (
      chunkHTTP = 32 << 10 // 32 KiB for JSON/XML/binary bodies
      chunkSSE  = 8  << 10 // 8 KiB, flushed per SSE event for latency
  )
  ```
  Recycle rule: the consumer returns `*Chunk.Buf` to the pool after the bytes have
  been rendered/copied (`chunkPool.Put(c.Buf)`); the UI emits a recycle message per
  rendered batch; the engine never reuses a buffer it has handed off.
  *(Implemented in Move 3 as `chunkSize = 32 << 10`; the 8 KiB SSE constant is
  deferred to the SSE transport in Goal 2, which is its only consumer. `Chunk`
  additionally carries `Recycle()`/`Released()`/`Bytes()`, and a double `Recycle`
  is counted and ignored rather than corrupting the pool — see MEMORY.md
  ADL-0016 §ownership.)*
- **Challenge:** Pool discipline is the GC-spike defense: any double-Put or
  use-after-Put corrupts data *silently*. The design makes this structurally safe:
  ownership transfer is one-way (engine → UI), `Chunk` is heap-allocated once per
  batch from its own small pool, and the pool `New` resets `len` to 0 while tests
  run with a `sync.Pool` wrapper that poisons recycled buffers to detect misuse.
- **Verify:** G-alloc on stream steady state; stress test streaming 100MB through
  the UI measuring peak heap (bounded by channel cap + in-flight); race detector;
  poisoned-pool test fails on any double-recycle.

### 1.2.4 Channel buffer limits and backpressure

- **Prereq:** 1.2.1 sink; frame governor (1.2.5).
- **Contract:**
  ```go
  chunks := make(chan *Chunk, 64) // 64 * 32KiB = 2 MiB hard ceiling, HTTP
  // SSE: make(chan *Chunk, 256)  // 256 * 8KiB = 2 MiB ceiling, latency-tuned
  ```
  Backpressure law: a full channel blocks the **reader goroutine** (via TCP window),
  never the UI; the UI drains at most one batch per frame.
- **Challenge:** The 2 MiB ceiling must hold *independently of response size*, and
  the UI must never buffer unbounded pending chunks between frames. The pager keeps
  only the tail (configurable, default 10,000 rows / 1 MiB of decoded text) plus
  counters; overflow is dropped-with-count, visible in the status bar.
- **Verify:** G-frame with 50MB synthetic stream; memory-ceiling test asserting RSS
  invariance across 1MB/10MB/100MB payloads; backpressure test with a stalled UI
  consumer proving the UI thread stays responsive (frame times unaffected).

### 1.2.5 Frame governor (60 FPS budget)

- **Prereq:** `DESIGN_SYSTEM.md` §2.2 message taxonomy; `bubbletea` `tea.Tick`.
- **Contract:**
  ```go
  const frameInterval = 16 * time.Millisecond // 60 FPS
  // Update: accumulate dirty state from msgs; tickMsg triggers at most one View.
  ```
- **Challenge:** Chunk arrival rate (thousands/sec on localhost) far exceeds frame
  rate; rendering per chunk would melt the frame budget. The model batches pending
  chunks and the governor drops stale ticks — `View` sees at most one coalesced
  state per frame.
- **Verify:** G-frame test: `p99 View < 16ms` with a synthetic producer firing 10k
  msgs/second; golden-frame count assertion (frames rendered == ticks fired).
- **SHIPPED (Move 14, ADL-0033).** `TestGFrameUpdateAndViewHoldTheFrameBudget`
  times 25 Update+View passes over full-size chunks (p95 360µs vs the 16 ms
  budget); the E2E floods assert `Frames() > 0` under load, which is the
  ticks-fired half of the golden-frame count. Mutation M79 (100 ms Update
  sleep) drives p95 to ≈102 ms and goes red.

### 1.2.6 Cancellation and `context.WithoutCancel` teardown

- **Prereq:** 1.2.1 run handle; HTTP body close semantics.
- **Contract:**
  ```go
  runCtx, cancel := context.WithTimeout(parent, d.timeout)
  defer cancel()
  // teardown must survive runCtx cancellation to drain/close deterministically:
  teardownCtx := context.WithoutCancel(context.WithValue(parent, runKey, id))
  go func() { defer resp.Body.Close(); <-teardownCtx.Done() /* bounded 50ms */ }()
  ```
- **Challenge:** Cancelling a run while the reader is blocked on a socket read can
  leak the connection if `Body.Close` is not called from a context that outlives the
  cancellation. `context.WithoutCancel` preserves values (log correlation) without
  inheriting cancellation, so teardown completes but is bounded by its own deadline.
- **Verify:** Cancellation test: interrupt at random offsets mid-50MB-stream; assert
  teardown completes ≤50ms, goroutine count returns to baseline (runtime.NumGoroutine
  snapshot), no file-descriptor leak.

## 1.3 The Zero-Trust Security Substrate

### 1.3.1 Cross-platform keychain backends (CGO-free)

- **Prereq:** AGENTS.md §4.3; ARCHITECTURE.md §6.2; per-OS test environments.
- **Contract:**
  ```go
  package secrets

  type Backend interface {
      Available() error // nil, or *Unavailable{Reason}
      Get(ctx context.Context, service, account string) ([]byte, error)
      Set(ctx context.Context, service, account string, secret []byte) error
      Delete(ctx context.Context, service, account string) error
  }

  // linux: Secret Service over D-Bus (github.com/godbus/dbus)
  // darwin: exec `/usr/bin/security find-generic-password -s <svc> -a <acct> -w`
  // windows: advapi32 CredReadW/CredWriteW (golang.org/x/sys/windows), CRED_TYPE_GENERIC
  ```
- **Challenge:** The macOS backend shells out; the private key material transits a
  subprocess pipe, which must be excluded from core dumps (no honors; documented
  risk) and never logged. D-Bus Secret Service requires an unlocked collection and
  a promptable session — headless environments report `Unavailable`, never a
  fallback.
- **Verify:** Per-backend integration tests on real OS services; environments
  lacking bindings report `BLOCKED (reason: ...)` per AGENTS.md §4.5 (never
  stubbed-green); G-hyg/redaction test proves keychain output never reaches logs.

### 1.3.2 Ephemeral `.env` resolution and the resolver chain

- **Prereq:** 1.3.1; AST variable anchors (1.1.8).
- **Contract:**
  ```go
  type Resolver interface {
      Resolve(ctx context.Context, name string) (string, Source, error)
  }
  type Source uint8 // SourceKeychain, SourceEnv
  func Chain(primary Resolver, fallback Resolver, rest ...Resolver) Resolver
  // order is FIXED: keychain → ephemeral env → ErrUnresolved (exit 3)

  type envStore struct{ vals map[string]string } // in-memory only; zeroized on Close
  func LoadEnv(path string) (*envStore, error)  // reads, never writes; no disk persistence
  ```
- **Challenge:** `.env` values are as sensitive as keychain entries once loaded, so
  the in-memory map is zeroized (`clear`) on teardown and never printed; interning of
  resolved secrets is avoided to prevent retention in long-lived strings.
- **Verify:** Chain-order test proving precedence; fail-closed test asserting exit
  code 3 (never substitution of empty string); redaction test asserting env-sourced
  values appear as `[REDACTED]` in every sink.

### 1.3.3 Redaction chokepoint

- **Prereq:** `DESIGN_SYSTEM.md` sinks list; `internal/store` write path.
- **Contract:**
  ```go
  var SensitiveHeaders = map[string]struct{}{ /* Authorization, Proxy-Authorization,
     Cookie, Set-Cookie, X-Api-Key, ... */ }
  func RedactString(s string) string   // replaces known-secret values with [REDACTED]
  func RedactHeaders(h textproto.MIMEHeader)
  // All sinks (stderr writer, UI strings, store rows) are constructed through this chokepoint.
  ```
- **Challenge:** Redaction by header name alone misses secrets in bodies and URLs;
  the redactor therefore also tracks a process-local set of vault-sourced *values*
  (matched by exact match and by `sha256`-indexed prefix) applied to every string
  emitted from the app layer.
- **Verify:** Corpus test: a request with every sensitive header, a token in the
  body, and a token in a query param is asserted fully redacted across stderr, UI
  render output, and the SQLite history row.

### 1.3.4 SQLite history schema — the zero-spillage guarantee

- **Prereq:** `modernc.org/sqlite`; `ARCHITECTURE.md` §8; migration framework.
- **Contract:**
  ```sql
  PRAGMA journal_mode=WAL;
  PRAGMA foreign_keys=ON;
  PRAGMA secure_delete=ON;  -- zero-fill deleted pages

  CREATE TABLE IF NOT EXISTS collections(
      id TEXT PRIMARY KEY, root_path TEXT NOT NULL, created_at INTEGER NOT NULL);
  CREATE TABLE IF NOT EXISTS requests(
      id TEXT PRIMARY KEY, collection_id TEXT NOT NULL REFERENCES collections(id),
      file_path TEXT NOT NULL, name TEXT NOT NULL, method TEXT NOT NULL,
      url_template TEXT NOT NULL, ordinal INTEGER NOT NULL, updated_at INTEGER NOT NULL);
  CREATE TABLE IF NOT EXISTS history(
      id INTEGER PRIMARY KEY AUTOINCREMENT, request_id TEXT NOT NULL REFERENCES requests(id),
      started_at INTEGER NOT NULL, duration_ms INTEGER NOT NULL, status_code INTEGER,
      request_bytes INTEGER, response_bytes INTEGER, sha256 TEXT, error TEXT,
      run_source TEXT NOT NULL);
  CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL);
  ```
  Structural guarantee: the schema is a **whitelist** — no column name matching the
  forbidden set (`secret`, `token`, `password`, `authorization`, `apikey`,
  `credential`, `cookie`, `bearer`, `env`) may exist, enforced by CI static analysis
  of migrations plus a runtime integrity test that dumps the schema and asserts the
  whitelist.
- **Challenge:** History rows contain URLs and headers — URL query params and
  `Authorization`-like values can carry secrets into `requests.url_template` or
  `history.error`. The store layer routes every column value through
  `RedactString` (1.3.3) at write time and strips query strings from stored URL
  templates at AST-parse time (templates keep `?`, drop rendered values).
- **Verify:** G-secret gate (CI grep of migrations + schema-dump assertion); planted-
  secret test: inserting a row containing a token via the public store API must yield
  a dump with zero occurrences of the token; `PRAGMA secure_delete` behavior asserted
  after `DELETE`.

### 1.3.5 Resolution failure semantics and exit codes

- **Prereq:** 1.3.2; `cmd/pulse` exit-code policy (AGENTS.md §4.4).
- **Contract:** unresolved variable → `*ErrUnresolved` → MCP `run-request` returns a
  JSON-RPC error object `code=-32003`; `pulse test` exits 3; TUI shows an error pane
  and refuses to execute. No dummy token, no empty substitution, no prompt loop.
- **Challenge:** The MCP path must translate domain errors to JSON-RPC *without*
  leaking the variable's partial value in the error `data` (redaction applies to
  error payloads too).
- **Verify:** Table-driven test over every error class asserting (exit code,
  JSON-RPC error shape, redacted data); mutation gate on each mapping.

## 1.4 Goal 1 Exit Gates

### 1.4.1 Gate checklist

- **G-alloc:** lexer 0 allocs/op; parse-of-cached-document 0 allocs/op; streaming
  steady state 0 allocs/op.
- **G-frame:** p99 `View` < 16ms under the 50MB synthetic stream — **MET (Move 14, ADL-0033).** p95 Update+View 360µs over 25 full-size chunks (measured per-sample, unit-level, no socket); 50 MiB unpaced flood byte- and line-exact with retention ≤ 2 MiB (`TestGFrameFiftyMegabyteStreamStaysBounded`). The p99 is asserted over 25 samples rather than a literal 50 MB render loop because retention is size-independent by construction (bounded tail) — rendering 50 MB of tail would measure a path the code cannot take.
- **G-mut:** every parser/streaming fix carries red→green evidence.
- **G-secret:** schema whitelist, planted-secret dump test, migration grep all pass.
- **G-honest:** keychain backend tests dispositioned PASSED per platform or BLOCKED
  with a stated reason.
- **G-doc:** `ARCHITECTURE.md` §4/§5/§6 and `DESIGN_SYSTEM.md` §2 updated in the same
  commit if contracts moved; ADL entries appended to `MEMORY.md`.

---

# Goal 2 — Protocol Supremacy & Advanced Networking

**Intent.** Extend the engine beyond request/response HTTP without diluting its
contracts: SSE and WebSockets stream through the *same* `io.TeeReader` +
`sync.Pool` + channel pipeline; gRPC rides a generalized transport interface with
dynamic protobuf reflection; and microsecond-precise telemetry measures TCP/TLS
handshakes without CGO, degrading honestly when the OS refuses.

## 2.1 Native SSE and WebSocket Streaming

### 2.1.1 Transport generalization contract

- **Prereq:** Goal 1 engine sink; `net/http` client; `coder/websocket` (pure Go,
  admitted in §0.4).
- **Contract:**
  ```go
  package engine

  // Every transport — HTTP, SSE, WebSocket, gRPC — terminates in this interface,
  // so the UI/MCP layers never learn which wire is underneath.
  type Stream interface {
      Read() (*Chunk, error)        // blocking; pool-backed buffer; Final set at EOF
      Write(p []byte) error         // nil for read-only transports
      Close() error
      SetDeadline(t time.Time) error
      Kind() StreamKind             // HTTP | SSE | WS | GRPC
  }

  type StreamKind uint8
  const (KindHTTP StreamKind = iota; KindSSE; KindWS; KindGRPC)

  var _ Stream = (*httpStream{})(*sseStream{})(*wsStream{})(*grpcStream{})
  ```
- **Challenge:** Unifying four wire protocols behind one `Stream` risks a
  lowest-common-denominator interface (e.g., write support leaked into read-only
  streams). The contract therefore splits the direction: read-only transports return
  a sentinel error `ErrWriteUnsupported` rather than lying; the UI never offers a
  write affordance for them.
- **Verify:** Interface conformance tests per transport with a recording `net.Conn`;
  contract test that `Kind()` matches the wired transport under the dual-mode
  router.

### 2.1.2 SSE connection lifecycle and deadlines

- **Prereq:** `net/http` with streaming body; SSE parser (2.1.5).
- **Contract:**
  ```go
  func OpenSSE(ctx context.Context, urlStr string, hdr Header, idle time.Duration) (Stream, error)
  // per-read deadline renewal: conn.SetReadDeadline(now.Add(idle)) before each Read
  // heartbeat ("": comment lines) reset the deadline; idle expiry → ErrIdle, retryable
  // reconnection honors Last-Event-ID; backoff: 500ms → 30s with full jitter
  ```
- **Challenge:** A silent SSE server (no heartbeats) must not wedge the UI nor leak
  the goroutine. The reader renews the deadline per event; on `ErrIdle` the stream
  closes cleanly, the UI shows a reconnecting state with attempt count, and
  reconnection re-issues `Last-Event-ID`. Backoff state lives in the run handle, not
  the model.
- **Verify:** Adversarial server test: heartbeats only (no data), instant `close`,
  mid-event truncation — each asserting clean transitions, no goroutine/fd leak, and
  correct `Last-Event-ID` replay; deadline renewal proven under a frozen clock
  (`testing/synctest`).

### 2.1.3 SSE parser state machine

- **Prereq:** 2.1.2 byte stream; chunk pool (Goal 1 §1.2.3).
- **Contract:**
  ```go
  type Event struct { ID, Name string; Data []byte; Retry time.Duration }

  type sseParser struct{ /* line buffer, field collector, BOM-stripped flag */ }
  func (p *sseParser) Feed(chunk []byte) iter.Seq[Event] // event per blank line
  func (p *sseParser) Flush() iter.Seq[Event]            // stream-EOF: yield partial
  ```
  Field rules: `field: value` (single optional space), `data` appended with `\n`,
  dispatch on blank line, `id`/`retry` remembered, comments (`:`) ignored.
- **Challenge:** Events arrive split across arbitrary chunk boundaries (field split
  mid-name, `data` split mid-line). The parser is a resumable state machine over a
  bounded line buffer; growth beyond the cap (default 1 MiB) fails closed with a
  structured error rather than allocating without bound.
- **Verify:** Differential test against a reference SSE parser over a randomized
  chunk-splitting fuzzer; 0-alloc assertion for `Feed` on pre-buffered events;
  oversize-line test asserts fail-closed error.

### 2.1.4 WebSocket client (RFC 6455) connection lifecycle

- **Prereq:** `coder/websocket`; `net/http` upgrade path; `crypto/sha1`,
  `encoding/base64` for the handshake key.
- **Contract:**
  ```go
  func DialWS(ctx context.Context, urlStr string, hdr Header) (Stream, error)
  // frame rules: client MUST mask; fragmentation via continuation (opcode 0x0);
  // control frames (ping 0x9/pong 0xA/close 0x8) ≤125 bytes, may interleave;
  // close handshake: send close(1000), await peer close, deadline-bounded.
  // payload policy (configurable): JSON/text → reassemble into chunk;
  // binary → stream chunked, oversize frames buffer up to cap then ErrOversize.
  ```
- **Challenge:** Fragment reassembly must coexist with the chunk model: a WS message
  is not a `Chunk`. The `wsStream` buffers fragments into a pooled message buffer and
  emits one `Chunk` per *complete* message, with `Final` on the close frame; a
  watchdog bounds reassembly memory.
- **Verify:** Interop test against a Python `websockets` echo server: fragmented
  text, interleaved pings, 16-bit and 64-bit length frames, and abrupt close;
  0-alloc steady-state assertion per message; race detector.

### 2.1.5 UI repaint triggers for append-only streams

- **Prereq:** `DESIGN_SYSTEM.md` §3 layout; frame governor (Goal 1 §1.2.5).
- **Contract:** SSE/WS `Chunk`s append to the same virtualized pager as HTTP bodies;
  repaint trigger = frame tick, never per-event; auto-tail follows the append cursor
  (`Stream` tab shows live tail); pause/resume (`Space`) freezes the cursor while
  events continue to buffer (bounded, counted).
- **Challenge:** Endless streams make "bottom" a moving target; tailing must not
  re-layout the whole viewport per frame. The pager renders a sliding window over
  an in-memory ring buffer of the last N decoded events and repaints only visible
  rows.
- **Verify:** G-frame with a synthetic SSE producer at 10k events/sec: p99 `View`
  < 16ms, ring-buffer memory constant; tail-follow test asserting cursor tracks the
  last row within one frame of a pause.
- **SHIPPED IN PART (Move 14, ADL-0033).** The p99 and memory-constant halves
  are gated (`TestGFrameUpdateAndViewHoldTheFrameBudget`, 50 MiB flood with
  retention ≤ 2 MiB); tail-follow is the viewport's `GotoBottom`-on-coalesce
  with no dedicated cursor-tracking test. Pause/resume (`Space`), the Stream
  tab, and WS reconnection continuity are NOT built — no Stream pane exists
  yet — and are not claimed.

### 2.1.6 Reconnect, resume, and run identity

- **Prereq:** 2.1.2 backoff; run handle (Goal 1 §1.2.1).
- **Contract:** Reconnects keep the same `RunHandle` (UI state continuity); each
  attempt increments a counter surfaced in the status bar; `Last-Event-ID` and WS
  `Sec-WebSocket-Protocol` resume tokens are replayed; a hard cap on attempts is
  configured (default: unbounded with user-visible attempt counter — the user owns
  cancellation).
- **Challenge:** Reconnection must not double-count history: each reconnect produces
  one `history` row per completed *segment*, correlated by a shared `run_group` id.
- **Verify:** Kill-and-restart server test: assert segment rows correlate, no lost
  events across the boundary (server replays from `Last-Event-ID`), attempt counter
  visible.

## 2.2 gRPC with Dynamic Protobuf Reflection

### 2.2.1 Transport interface instantiation for gRPC

- **Prereq:** 2.1.1 `Stream` contract; `google.golang.org/grpc` (pure Go, §0.4);
  plaintext credentials for localhost (`insecure.NewCredentials()`), TLS otherwise.
- **Contract:**
  ```go
  func OpenGRPC(ctx context.Context, target string) (*grpcConn, error)
  // unary            → single Chunk (protojson body) + Done(trailer)
  // server-streaming → Chunk per message; Final on stream EOF
  // client-streaming / bidi → requires engine Write path (Goal 2 extension point)
  ```
- **Challenge:** gRPC frames are length-prefixed (1-byte compression flag + 4-byte
  big-endian length) and trailers-only responses are legal; the stream layer must
  decode frames, surface `grpc-status`/`grpc-message` as HTTP-equivalent status, and
  map the trailers into `RunResult` metadata.
- **Verify:** Golden test against a local in-process gRPC server covering unary,
  server-streaming, and error-status responses; frame-decode unit tests including
  compressed-flag rejection (client refuses compressed payloads).

### 2.2.2 Dynamic message resolution via server reflection

- **Prereq:** 2.2.1 connection; `google.golang.org/protobuf/reflect/protoreflect` +
  `dynamicpb`.
- **Contract:**
  ```go
  // symbols resolved through the reflection service (no compile-time protos):
  //   /grpc.reflection.v1.ServerReflection/ServerReflectionInfo (bidi)
  //   request: {file_containing_symbol: "pkg.Service/Method"}
  //   → FileDescriptorProto → protoregistry → protoreflect.MethodDescriptor
  desc, err := resolveMethod(ctx, conn, "grpc.health.v1.Health/Check")
  req := dynamicpb.NewMessage(desc.Input())
  json.Unmarshal(body, protojson.UnmarshalOptions{}.Unmarshal, req) // protojson
  ```
- **Challenge:** Reflection may be disabled on the server; resolution must fail with
  a precise, actionable error (and offer a local `.proto`/descriptor-file fallback
  path loaded from the collection dir — no network fetch, offline-first honored).
  Descriptor sets are cached keyed by (target, symbol) with an LRU bound.
- **Verify:** Test server with reflection disabled asserts the structured error and
  the local-descriptor fallback success; cache-hit benchmark; mutation gate on the
  symbol-resolution fix.

### 2.2.3 JSON ↔ protobuf mapping rules

- **Prereq:** 2.2.2 messages; `protojson` with `DiscardUnknown: false` for strictness.
- **Contract:** Request bodies authored in `.http` blocks are protojson; unknown
  fields are rejected by default (config `grpc.strict_json = true`), enum values
  accept names and numbers, `int64`/`uint64` render as strings (protojson rule),
  `EmitUnpopulated` configurable.
- **Challenge:** protojson's strictness surprises users (int64-as-string, enum names
  only). The UI must render validation errors with field-path context so the editor
  can underline the offending location.
- **Verify:** Table-driven cases: enum by name/number, int64 strings, unknown field
  rejection, well-known types (`Timestamp` RFC 3339); each error case asserts a
  field-path-bearing message.

### 2.2.4 Streaming RPC → chunk channel mapping

- **Prereq:** 2.1.1 `Stream`; 2.2.3 json mapping.
- **Contract:** each server message → `Chunk` (pooled protojson bytes); stream
  termination → `Final`; `grpc-status` non-OK → `Failed` with a structured
  `*GRPCStatus{Code, Message}`; client-streaming/bidi methods are detected from
  the method descriptor (`IsStreamingClient`) and either rejected with a clear
  error or routed through the engine's write path when interactive input is enabled.
- **Challenge:** The engine's sink contract assumes request-complete bodies; bidi
  RPCs break that assumption. Decision: v1 of gRPC supports unary + server-streaming
  only; client-streaming methods return `ErrUnsupportedMethod` with a clear status —
  no silent degradation.
- **Verify:** Method-descriptor matrix test (unary/server/client/bidi × expected
  behavior); streaming timing test asserting chunk flow control under reflection.

## 2.3 Low-Level Telemetry (CGO-Free, Microsecond Precision)

### 2.3.1 CGO-free constraint map

- **Prereq:** AGENTS.md §2.1 (`CGO_ENABLED=0`); per-OS syscall boundaries.
- **Contract:** telemetry uses `net/http/httptrace` (stdlib), `crypto/tls` hooks, and
  OS syscalls directly — `unix.ClockGettime`-style calls via `syscall.Syscall` on
  Linux, `QueryPerformanceCounter` via `golang.org/x/sys/windows` on Windows — no
  cgo bridges anywhere. The `unsafe` package is confined to the telemetry package
  and fenced by GOOS build tags with a `go vet` clean guarantee.
- **Challenge:** Monotonic raw clocks differ per OS: Linux offers
  `CLOCK_MONOTONIC`/`CLOCK_MONOTONIC_RAW`; macOS offers `mach_absolute_time`
  (numerator/denominator conversion); Windows offers QPC. The abstraction exposes
  `nanos()` with documented per-OS semantics — `time.Now()`'s monotonic reading is
  the default; RAW variants are opt-in because of migration across cores.
- **Verify:** Clock-skew test comparing the telemetry clock against `time.Now`
  monotonic within tolerance; per-OS build matrix compiles; `unsafe` usage audited
  by a CI lint rule scoped to the telemetry package.

### 2.3.2 Trace points and collection contract

- **Prereq:** `net/http/httptrace`; 2.3.1 clocks.
- **Contract:**
  ```go
  type TracePoint struct {
      Op          string // "dns","connect","tls","wrote","firstbyte","done"
      Start, End  time.Time // monotonic
      Meta        string   // redacted remote addr, TLS version/cipher
  }
  type Collector struct{ /* bounded ring, e.g. 512 points */ }
  func WithTrace(ctx context.Context, c *Collector) context.Context
  // httptrace hooks: ConnectStart/Done, TLSHandshakeStart/Done,
  // WroteRequest, GotFirstResponseByte, PutIdleConn, GotConn(reused)
  ```
- **Challenge:** Trace callbacks fire on runtime-internal goroutines; the collector
  must be lock-cheap and never allocate on the hot path (pre-allocated ring, atomic
  index). `Meta` passes through `RedactString` — TLS versions and cipher names are
  safe; addresses are host:port only (no query strings).
- **Verify:** p99 trace overhead < 2% throughput against an untraced baseline;
  ring-overflow test asserts bounded memory; redaction test on trace output.

### 2.3.3 eBPF strategy (optional, build-tagged)

- **Prereq:** `github.com/cilium/ebpf` (pure Go, §0.4); kernel ≥5.8 with BTF;
  compiled objects produced by clang/bpftool outside CI (pinned SHAs).
- **Contract:**
  ```go
  //go:build ebpf
  package ebpftrace
  // kprobes: tcp_v4_connect / tcp_connect (v6), tcp_set_state; tracepoint: sock send/recv
  // ring buffer: perf RB → ebpf.NewReaderFromMap → decode 4-tuple + timestamps
  func Load(spec []byte) (*Tracer, error)
  ```
  Correlation: events keyed by (src,dst 4-tuple, pid) joined to in-flight runs;
  output feeds the same `Collector` with `Op` values `"ebpf.connect"`,
  `"ebpf.retransmit"`.
- **Challenge:** eBPF requires privileges (CAP_BPF/CAP_SYS_ADMIN) and a compatible
  kernel; absence must produce `BLOCKED (reason: no CAP_BPF)` per AGENTS.md §4.5 —
  never a silent fallback masquerading as precision. The toolchain dependency
  (clang) is deliberately excluded from the default build.
- **Verify:** On capable hosts: handshake phases match httptrace within a
  jitter envelope; on incapable hosts: BLOCKED disposition test asserting the
  reason string; 0-alloc on the event-decode path.

### 2.3.4 Precision reporting and degradation ladder

- **Prereq:** 2.3.2 collector; `DESIGN_SYSTEM.md` timing tab.
- **Contract:** Waterfall render (DNS → TCP → TLS → TTFB → transfer) with
  microsecond readouts; precision source is reported explicitly (`raw` /
  `monotonic` / `BLOCKED`); histogram summary (p50/p95/p99) per phase across runs.
- **Challenge:** Mixing clock sources across phases breaks arithmetic; all phase
  arithmetic is done in the collector's clock domain and converted once at render.
- **Verify:** Golden waterfall snapshot test; precision-source label test including
  the BLOCKED path; cross-platform CI dispositions recorded honestly.

## 2.4 Goal 2 Exit Gates

### 2.4.1 Gate checklist

- SSE/WS/gRPC all terminate in the `Stream` contract with conformance tests.
- Deadline, reconnect, and IOError-matrix tests green (G-mut on each fix).
- 0-alloc assertions hold for SSE `Feed`, WS message path, trace collector.
- eBPF path either PASSED on capable hosts or BLOCKED with stated reason — never faked.
- `ARCHITECTURE.md` transport section and dependency registry updated; ADL appended.

### 2.4.2 Goal 2 status — 2026-10-12

**Goal 2 is complete, with two items explicitly NOT met.** Stating them is the
point; counting them as passed would be the failure mode §2.4.1 exists to catch.

| Gate | Status | Evidence |
|---|---|---|
| §2.1 SSE: unified `Stream` contract | **MET** | `TestStreamIsReadOnly`, `TestStateMachineTransitions` (Move 5, ADL-0018) |
| §2.1.2–2.1.3 SSE lifecycle + parser | **MET** | `TestReconnectReplaysLastEventID`, `TestIdleTimeoutIsRetryable`, `TestSSEParserSplitAcrossChunks` |
| §2.1.5 frame-aligned repaints | **MET** | `TestFrameAlignedDispatch` (mutation: 1849 chunks for 500 events) |
| §2.1.6 reconnect + `Last-Event-ID` | **MET** | `TestReconnectReplaysLastEventID` — server observes `Last-Event-ID: evt-2` |
| §2.1.4 WebSocket client | **DEFERRED** | Not started. `Stream` carries the `KindWS` slot and the read-only write refusal, but no RFC 6455 client exists. |
| §2.2.1 gRPC transport | **MET** | `TestReflectionGateResolvesAndRendersJSON`, `TestServerStreamingDeliversEveryMessageThroughTheSink`, `TestClientStreamingIsRejectedBeforeDialing` |
| §2.2.2 dynamic reflection | **MET** | `TestReflectionDescriptorMatchesTheWireProtocol`, `TestReflectionUnavailableFallsBackToLocalDescriptors`, `TestReflectionUnavailableWithNoFallbackFailsClosed` |
| §2.2.3 JSON ↔ protobuf mapping | **MET** | `TestTypeMismatchFailsCleanly` (+ nested and repeated paths), `TestValidQuotedNumberForInt64IsAccepted`, `TestUnknownFieldIsRejectedByDefault` |
| §2.3 telemetry + eBPF degradation | **MET** | `TestEBPFDegradesToBlocked` (disposition always carries a reason), `TestTelemetryDegradesWithoutBreakingTheRequest` |
| G-frame (p99 `View` < 16 ms) | **MET (Move 14)** | `TestGFrameUpdateAndViewHoldTheFrameBudget` (p95 360µs), 50 MiB flood bound, M79/M80 red→green (`scripts/mutation-proof-ui.sh`) |
| G-mut on every fix | **MET** | `scripts/mutation-proof.sh` (M1–M2), `mutation-proof-secrets.sh` (M6a–M9), `mutation-proof-stream.sh` (M10–M13), `mutation-proof-grpc.sh` (M14–M18) |

**Contract changes made during implementation** (all recorded in MEMORY.md):

- §2.2.1's `OpenGRPC(ctx, target) (*grpcConn, error)` was superseded by the
  `ProtocolEngine` interface plus `GRPCEngine.Run(ctx, *ast.Request, Sink)`
  (ADL-0019). The original signature implied a separate pipeline returning its
  own connection type, which would have defeated the unification §2.1.1 asked
  for. gRPC output goes through the **same** `Sink` as REST.
- §2.2.1's "client-streaming / bidi → requires engine Write path" is resolved by
  **refusal**: they return `ErrUnsupportedMethod` before dialing. §2.2.4's
  rationale ("no silent degradation") takes precedence over a partial
  implementation.
- §2.2.1's "frame-decode unit tests including compressed-flag rejection" are
  **superseded**: `google.golang.org/grpc` owns framing, so Pulse never decodes
  a length prefix itself. Compression negotiation is grpc-go's contract, not a
  Pulse gate; Pulse's own binary→JSON encoder is gated instead
  (`TestJSONStreamDoesNotMaterializeTheMessage`).
- §2.2.2's `{file_containing_symbol: "pkg.Service/Method"}` is carried literally
  in the descriptor but the **symbol sent is the service full name**
  (`pkg.Service`), matching the reflection protocol: asking for a symbol that is
  not a descriptor name returns an error rather than a file.

---

# Goal 3 — AI Orchestration & Sandboxed Extensibility

**Intent.** Make Pulse the canonical bridge between local LLM agents and local APIs:
a bulletproof MCP stdio transport, a pure-Go RAG index over the user's own codebase,
an auto-fuzzing loop driven by OpenAPI schemas, and a wazero sandbox for user auth
scripts — all without CGO, without cloud, and without secrets crossing trust
boundaries.

## 3.1 The MCP Bridge (JSON-RPC over stdio)

### 3.1.1 stdio isolation rules

- **Prereq:** AGENTS.md §3.3; `mark3labs/mcp-go` (pure Go, §0.4); the dual-mode
  router (ARCHITECTURE.md §2 precedence; the stdout guard itself is implemented here
  first because it is security-critical at boot).
- **Contract (shipped, MEMORY.md ADL-0020):**
  ```go
  // In cmd/pulse, MCP mode -- the guard runs FIRST, before the mcp package is used:
  lock, err := mcp.Lock(audit bool) // audit == (os.Getenv("PULSE_STDOUT_AUDIT") == "1")
  defer lock.Restore()
  // lock.Real is os.Stdout captured before the swap; os.Stdout now points at
  // os.DevNull (production) or at a frame-judging Tripwire (audit). In audit mode
  // any complete line that is not a valid JSON-RPC 2.0 frame panics to stderr,
  // converting a hygiene violation into a loud, test-detectable failure.
  code, err := mcp.ServeLocked(ctx, lock.Real, os.Stdin)
  ```
  The API is split rather than a single `NewStdioServer(...).Listen(...)` because a
  helper that captures `os.Stdout` internally is itself the indirection the guard
  exists to remove; the codec is handed the captured channel and nothing else.
  `ServeLocked` returns `(exitCode, err)`, and both clean stdin EOF and
  `context.Canceled` map to exit 0.
- **Challenge:** Dependencies and transitive packages may capture `os.Stdout` before
  the swap; the guard therefore runs **first** in `main`, and the audit-mode writer
  catches late captures. The JSON-RPC codec is the only component holding
  `realStdout`.
- **Verify:** G-hyg CI analyzer: zero `os.Stdout`/`fmt.Print*` references under
  `internal/mcp` and `internal/app` MCP paths; a planted-stray-write regression test
  fails the build; audit-mode test panics as designed. **Shipped**, with two narrow
  analyzer exemptions recorded in ADL-0020: the guard file itself
  (`internal/mcp/stdio.go` -- it *is* the boundary) and `_test.go` files (a test
  proving isolation must be able to swap `os.Stdout` for a pipe and observe it; test
  files are not linked into the shipped binary). Gates:
  `TestMCPStdoutIsolation`, `TestMCPStdoutAuditTripwire`,
  `TestMCPDiagnosticsNeverReachStdout`, `TestStdioLoopEmitsOnlyJSONRPC`,
  `TestStdoutGuardTripsOnRoguePrint`.

### 3.1.2 Routing precedence and mode immutability

- **Prereq:** 3.1.1; `golang.org/x/term` isatty.
- **Contract:** precedence is fixed and tested: `pulse mcp` subcommand or `--mcp`
  flag → MCP; bare `pulse` + `isatty(stdout)` → TUI; bare `pulse` + non-TTY → stderr
  hint + exit 2. Mode is a build-time-injected value into the app context, immutable
  for the process lifetime; an architecture test asserts `internal/ui` and
  `internal/mcp` are never both wired.
- **Challenge:** A pipe consumer (CI) or an agent spawning `pulse` without the
  subcommand must never get TUI garbage on stdout; the non-TTY fail-closed branch
  is the guard. Agents are documented to invoke `pulse mcp`.
- **Verify:** Matrix test over (flag × isatty) asserting mode and exit codes;
  stdout byte-content assertion in MCP-mode boot (empty before handshake).
  **Partially shipped.** Routing precedence, the non-TTY fail-closed branch, and the
  MCP-boot stdout assertion are implemented and gated
  (`TestRunModeMatrix`, `TestMCPBootLeavesStdoutEmpty`). The architecture test that
  asserts `internal/ui` and `internal/mcp` are never both wired is **NOT built** --
  `internal/ui` does not exist yet, so there is nothing to assert against. That item
  stays unchecked in TASKS.md; it cannot honestly be ticked until Phase 3 lands.

### 3.1.3 Tool registry and payload validation

- **Prereq:** 3.1.1; `github.com/santhosh-tekuri/jsonschema/v6` (pure Go, §0.4);
  app services.
- **Contract:**
  ```go
  type Tool struct {
      Name    string          // kebab-case: run-request, get-request, ...
      Handler ToolHandler
  }
  type ToolHandler func(ctx context.Context, req CallToolRequest) (*CallToolResult, error)

  // Shipped as "execute_request", NOT the illustrative "run-request" below.
  // Naming is snake_case per MCP ecosystem convention; see MEMORY.md ADL-0020.
  srv.AddTool(mcp.NewTool("execute_request",
      mcp.WithDescription("Execute a .http request from a raw payload or a file path"),
      mcp.WithString("payload"),  // raw .http source, XOR
      mcp.WithString("path"),     // path to a .http file,  XOR
      mcp.WithNumber("index"),    // which request in the document
      mcp.WithString("env"),
      mcp.WithNumber("timeout_ms")),
      executeRequestHandler)

  // every CallToolRequest args validated against the tool's JSON Schema BEFORE dispatch;
  // validation failure → JSON-RPC error -32602 (invalid params); no partial dispatch.
  ```
- **Challenge:** MCP clients send loosely-typed JSON (numbers as strings, extra
  fields). Validation is schema-first, and coercion is explicitly opt-in per tool
  (`mcp.With...` option) — never implicit. Tool results always pass
  `RedactString`/`RedactHeaders` (Goal 1 §1.3.3) before serialization.
- **Verify:** Table-driven invalid-payload tests per tool asserting -32602 and zero
  handler invocation; redaction test on each tool result; G-hyg on the result path.
  **Shipped for `execute_request`.** Concretely: the schema is compiled once at
  registration; unknown keys are rejected (`additionalProperties: false`) because a
  silently-ignored field is how an agent concludes its request was honoured;
  coercion (`index`/`timeout_ms` integer-or-string, `include_body`
  boolean-or-string) is per-field, opt-in, and **recorded** in a `warnings` list the
  caller reads, because coercion that is not reported is worse than a rejection;
  "exactly one of `payload`/`path`" is enforced semantically, not in the schema,
  because JSON Schema cannot express two mutually-exclusive optional keys without
  leaving one of the three cases unstated to the client.
  Gates: `TestInvalidArgumentsAreStructuredErrors` (6 cases),
  `TestCoercionIsRecordedNotSilent`, `TestProtocolGateParsesExecutesAndReturns`, `TestRedactionChokepointRunsOnResults`.
  **Open:** the redaction chokepoint is implemented and tested in
  `collectingSink.outcome`, but `DefaultExecutor` is built with a `nil`
  `secrets.Redactor` because no composition root (`internal/app`) exists yet -- so in
  MCP mode today redaction is by header name only and vault-sourced values are not
  yet scrubbed from bodies. Recorded as an open TASKS.md item; **not** counted as
  done.

### 3.1.4 Lifecycle, cancellation, and error mapping

- **Prereq:** 3.1.3; engine run handle.
- **Contract:** spec lifecycle `initialize → initialized → ops → shutdown → exit 0`;
  client cancellation (`notifications/cancelled`) maps to the run's `context.Cancel`;
  domain errors map to JSON-RPC codes: parse `-32002`, unresolved secret `-32003`,
  timeout `-32004`, transport `-32005`, unsupported `-32006`; error `data` payloads
  are redacted and never contain secret material.
- **Challenge:** Cancellation must unwind engines within the MCP call budget
  (Goal 1 §1.2.6 teardown ≤50ms) or the client hangs. The tool handler propagates
  the per-call context into `Engine.Run` and awaits `RunHandle.done`.
- **Verify:** Lifecycle conformance test against a scripted client (initialize,
  call, cancel mid-stream, shutdown, exit 0); error-code table test; cancellation
  latency assertion. **Shipped.** Gates: `TestProtocolGateHonoursTheHandshake`,
  `TestCancellationStopsTheToolCall`, `TestStdioLoopEmitsOnlyJSONRPC`, and the
  `-32002`/`-32003`/`-32602` structured-error cases. Domain errors occupy the
  reserved `-320xx` block exactly as specified (parse `-32002`, unresolved secret
  `-32003`, timeout `-32004`, transport `-32005`, unsupported `-32006`), and each
  carries a stable `category` string in `data` so an agent can branch on the code
  rather than on English prose; `ProtocolError.payload()` **includes the cause**, so
  a schema failure surfaces the parser's `:1:1:` line:col and is actionable rather
  than merely categorical.
  **One judgement recorded explicitly:** a failure with no HTTP status (transport
  death, timeout, unresolved secret) is reported as the error itself, never as a
  synthesised `status: 0` outcome, with any partial body attached to `data`.
  Presenting "no response" as "a response whose status is zero" produces a
  plausible-looking wrong answer that an agent will reason about.
  **Test-harness constraint (documented in the test, not a product limitation):**
  mcp-go's stdio loop cancels the request context on stdin EOF, so a scripted test
  agent must hold the input pipe open until the response is observed.

## 3.2 Local RAG over the Application Codebase (Pure Go)

### 3.2.1 Indexing pipeline and file chunking

- **Prereq:** `go/parser` + `go/ast` (stdlib) for Go files; `github.com/fsnotify/
  fsnotify` (pure Go, §0.4) for incremental reindex; deny-list = `.gitignore`
  semantics + hardcoded secret paths (`.env*`, `*.pem`, `*.key`, keychain dirs).
- **Contract:**
  ```go
  type Chunk struct {
      ID       string // sha256(path + ":" + span)
      Path     string // repo-relative
      Lang     string
      StartLn  int
      Symbol   string // enclosing FuncDecl/type name when available
      Text     string // redacted source span
      Vec      []int8 // quantized embedding (per-vector scale stored alongside)
      Scale    float32
  }
  func Index(root string) iter.Seq[Chunk] // lazy chunk generator
  // Go files: cut at top-level decl boundaries (FuncDecl/GenDecl/TypeSpec);
  // oversize decls: windowed by 64 lines with 16-line overlap;
  // non-Go files: fixed windows with the same overlap.
  ```
- **Challenge:** The index must never contain secret material: every chunk passes
  `RedactString` before storage, and the deny-list is enforced at the walker (a file
  matching `.env`/`*.pem` is never opened). The walker honors `.gitignore` so build
  artifacts and vendored deps stay out.
- **Verify:** Fixture-repo test: expected chunk count/spans per language; planted
  secret in a tracked file never appears in any stored chunk; incremental reindex
  (watcher event) yields exactly the changed chunks.
  **Partially shipped (MEMORY.md ADL-0021).** The pipeline, the per-language
  chunkers and the deny-list are built and gated:
  `TestGoChunksCutAtDeclarationBoundaries`, `TestPythonChunksCutAtIndentationBlocks`,
  `TestAstroChunksSplitAtStructuralFences`, `TestOversizeDeclarationIsWindowedWithOverlap`,
  `TestDenyListRefusesSecretFiles`, `TestGitignoreSemantics`, `TestWalkStatsAreHonest`,
  `TestRedactionRunsBeforeStorageAndEmbedding`, `TestTwoIndexRunsAgree`.
  **Contract refinements, both forced by something observed:**
  - **The Go cut uses `decl.Doc`, not `decl.Pos()`.** `decl.Pos()` is the `func`
    keyword, several lines below the comment, so the naive cut drops the doc
    comment from every chunk — and the comment is half of what makes a retrieved
    chunk explain anything.
  - **A Python block ends at a same-indent block start as well as at a dedent.**
    A top-level `class` has indent 0, so the dedent rule alone can never fire and
    the class runs to end of file as one chunk — the module, retrieved whenever
    any of its methods is named.
  - **`.gitignore` normalisation preserves a leading `/`.** Stripping it, which
    looks like tidy-up, converts an anchored rule into one matching at any depth.
    Over-exclusion is silent and its symptom — an explanation that cannot find
    the file you are staring at — looks exactly like a retrieval miss.
  **NOT BUILT:** the fsnotify watcher and the stale-chunk reaper (§3.2.5).

### 3.2.2 Embedding strategy without CGO vector DBs

- **Prereq:** `github.com/owulveryck/onnx-go` (pure Go, §0.4); a pinned embedding
  model artifact (e.g., MiniLM-class, int8) fetched out-of-band and verified by
  SHA-256 — never downloaded at runtime (offline-first, I-3).
- **Contract:**
  ```go
  // model lives at $PULSE_HOME/models/embd-int8.onnx (pinned sha in pulse.toml)
  type Embedder interface{ Embed(ctx context.Context, text string) ([]int8, float32, error) }
  // availability: MissingModel → Embedder reports Unavailable; the system falls back
  // to FTS5 lexical search only and reports DEGRADED honestly (never fake vectors).
  ```
- **Challenge:** Pure-Go ONNX execution covers the op subset used by small
  transformer encoders; unsupported ops must fail with a precise
  `UnsupportedOp` error feeding the DEGRADED path rather than panicking or
  returning zero vectors silently.
- **Verify:** Embedding determinism test (identical text → identical vector);
  unsupported-op test asserts DEGRADED; vector quality smoke test on a fixture corpus
  (nearest-neighbor sanity: a function's own docstring ranks its body first).
  **Shipped as a structural embedder behind the same interface (ADL-0021).**
  `onnx-go` is **not vendored**, because the contract it exists to satisfy —
  a pinned model at `$PULSE_HOME/models/embd-int8.onnx`, fetched out of band and
  verified by SHA-256 — cannot be satisfied without shipping the artifact, and
  Pulse never downloads one at runtime (invariant I-3). Shipping a stub behind
  the identical interface keeps the seam real and the DEGRADED path reachable.
  What ships is `HashingEmbedder`: signed feature-hashing over identifier tokens,
  sub-word splits and character trigrams at d=256. It is a real vector and it is
  informative for code — `TestCamelCaseIsSplit` proves a trace saying "parse http
  response" finds `parseHTTPResponse` — but it is not semantic, and
  `Embedder.Name()` returns `structural-hash-v1` so no result can be mistaken for
  one.
  **The DEGRADED path is a TYPE, not a nil interface.** `UnavailableEmbedder`
  returns `ErrModelUnavailable` and never a zero vector, because a zero vector
  ranks as "matches nothing" and the user would be told their code has no similar
  chunks rather than that the feature is not installed. Those are different
  claims and only one of them is true.
  Gates: `TestEmbeddingIsDeterministic`, `TestEmbeddingSeparatesSimilarFromUnrelated`,
  `TestCamelCaseIsSplit`, `TestUnavailableEmbedderFailsClosed`, `TestEmptyTextYieldsNoVector`.

### 3.2.3 Storage and hybrid retrieval in SQLite

- **Prereq:** Goal 1 §1.3.4 schema discipline (whitelist columns); FTS5 availability
  in `modernc.org/sqlite` (enabled in the engine build).
- **Contract:**
  ```sql
  CREATE TABLE IF NOT EXISTS rag_chunks(
      id TEXT PRIMARY KEY, path TEXT NOT NULL, lang TEXT NOT NULL,
      start_ln INTEGER NOT NULL, symbol TEXT, text TEXT NOT NULL,
      vec BLOB NOT NULL, scale REAL NOT NULL,
      updated_at INTEGER NOT NULL);
  CREATE VIRTUAL TABLE IF NOT EXISTS rag_fts USING fts5(
      text, content='rag_chunks', content_rowid='rowid');
  -- retrieval: FTS5 bm25 rank ∥ brute-force cosine over vec (int8 dot with scale),
  -- fused by Reciprocal Rank Fusion (k=60); no secret columns, whitelist audited.
  ```
- **Challenge:** Brute-force cosine over a large local repo is O(N·d); with int8
  vectors and a bounded candidate set (FTS top-200 OR full scan at ≤50k chunks),
  latency stays interactive; the scan runs on a background goroutine (I-5) with
  results streamed as `tea.Msg`/MCP chunks.
- **Verify:** Retrieval benchmark (p99 latency on a 50k-chunk fixture); recall@10
  fixture test (known query→expected chunk); whitelist/secret grep on the new
  schema; background-goroutine assertion.
  **Partially shipped (ADL-0021).** Schema, hybrid retrieval and the recall gates
  are built: `TestRAGSchemaSurvivesTheSecretAudit`,
  `TestHybridRetrievalFindsTheKnownChunk`, `TestVectorLegOrdersByCosine`,
  `TestReciprocalRankFusionPrefersAgreement`, `TestUpsertIsBoundedPerTransaction`,
  `TestStaleFTSRowsAreReplaced`, `TestReindexIsIdempotent`,
  `TestDeleteByFileRemovesBothLegs`, `TestCosineMatchesTheRAGPackage`.
  **FTS5 availability was PROBED, not assumed** — a build can advertise the
  compile option and still fail to instantiate the module, and discovering that at
  first query time means the user's first 500 explanation fails with a syntax
  error instead of at index time with a clear message.
  **Three decisions worth recording because each is the opposite of the obvious
  one:**
  - **The tables are created by `InitRAG`, not by `Open`.** The main schema
    applies unconditionally at open, so FTS5 there would make Pulse unopenable on
    a driver without it — for an optional feature that reports DEGRADED.
  - **The FTS5 query is BUILT, never passed through.** Error bodies are full of
    `"`, `*`, `:`, `(`, `AND`, `OR`, `NOT`, `NEAR`. Handing one to `MATCH`
    verbatim turns every response containing a quote into a query syntax error:
    the explanation feature failing precisely when it is needed.
    `TestFTSQueryCannotBeInjected` sends eight realistic 500 bodies.
  - **A chunk's row and its FTS row are written in the SAME transaction.**
    Splitting them leaves a window where a chunk is findable by vector and
    invisible to lexical search — an index that disagrees with itself depending
    on which leg you ask.
  **NOT RUN:** the 50k-chunk p99 latency benchmark. The recall fixtures are 2–4
  chunks; the O(N·d) cost at the ceiling is argued from the operation count
  (~12.8M int8 multiply-adds at d=256, N=50k), not measured.

### 3.2.4 Attaching backend handler code to a 500

- **Prereq:** 3.2.3 retrieval; engine run results; MCP tool surface.
- **Contract:**
  ```go
  // MCP tool: explain-failure { run_id | ref } →
  //   1. load history row (status 5xx) + redacted error text
  //   2. embed(error text + request symbol) → top-k chunks ranked by RRF
  //   3. return [{path, start_ln, symbol, text}] + correlation id
  // TUI: Ctrl+G on a 5xx response opens the RAG drawer with the same ranking.
  ```
- **Challenge:** The explanation must not leak source beyond the user's machine
  (it never leaves stdio/UI) and must not leak secrets embedded in stack traces —
  error text passes `RedactString` before embedding *and* after retrieval.
- **Verify:** Failure-to-chunk correlation test on a fixture repo (seeded bug →
  expected handler ranks top-3); redaction test with a token in the error body.
  **Shipped (ADL-0021).** Gates:
  `TestFiveHundredBindingIsNotSuppressedOnSuccess` (11 status cases, asserting
  **both** directions), `TestErrorTextExtraction` (9 body shapes),
  `TestExtractionIsBounded`, `TestBindingRedactsErrorText`,
  `TestBindingRedactsRetrievedSource`, `TestRetrievalFailureIsDegradedNotFatal`,
  `TestDegradedRetrievalIsLabelled`, `TestBindingIsBounded`, `TestNilBinderIsSilent`,
  `TestStoreRetrieverMapsEveryField`, `TestRenderBindingProducesReadableOutput`.
  **The 5xx gate is `500 <= status <= 599`, exactly, and both directions are
  pinned** because they fail differently. A 5xx that gets no context is a silent
  feature failure — the worst kind, because everything looks healthy. A 200 that
  does is noise that costs the agent's context window and teaches it to ignore
  the attachment. The range is closed at both ends on purpose: a synthetic or
  proxied status above 599 is not a server error this binder can explain, and
  attaching source to it would be a confident wrong answer.
  **Redaction runs on the error text BEFORE it becomes a search query**, not after
  — the query reaches an embedding and then a store, so a secret in it is a secret
  on disk and in a model's context window.
  **A retrieval failure is a degradation, never a failure of the request.** The
  agent asked to run a request and got a 500; that is the answer, and the context
  attachment is recorded in `warnings` where the agent will see it.
  **NOT BUILT:** no `internal/app` composition root exists, so nothing hands a
  `*store.Store` to the binder and this is not yet reachable from `pulse --mcp`.
  `StoreRetriever` exists and is gated; the wiring does not.

### 3.2.5 Incremental maintenance and index health

- **Prereq:** 3.2.1 watcher; HLC stamps (Goal 4 §4.2.1).
- **Contract:** watcher events (create/modify/delete/rename) map to chunk
  upsert/delete keyed by content hash; stale chunks (deleted files) are reaped on
  each sync; a `rag_meta` table records index version, model sha, and chunk count;
  rebuild is a single CLI invocation with progress streamed to stderr (never stdout
  in MCP mode).
- **Challenge:** Bulk operations (branch switch) must not wedge the UI: reindexing
  is chunk-batched with cooperative yields, and the UI shows a progress statement
  fed by batched messages.
- **Verify:** Branch-switch simulation test (rename storm) completes with consistent
  index; progress batching test; determinism: two rebuilds produce identical chunk
  sets.

## 3.3 Auto-Fuzzing via Local LLM + MCP

### 3.3.1 Schema ingestion (OpenAPI → internal IR)

- **Prereq:** ~~`github.com/getkin/kin-openapi` (pure Go, §0.4)~~ — **NOT
  VENDORED.** Move 9 needs YAML to read a `.yaml` schema, and both
  `gopkg.in/yaml.v2` and `gopkg.in/yaml.v3` are permanently banned, so no
  OpenAPI library (every one of which takes a YAML dependency) can be admitted.
  The reader is purpose-built over `encoding/json` plus a hand-rolled YAML
  *subset* parser. Goal 1 AST for request templates: used, and load-bearing —
  the engine runner renders each probe to `.http` and parses it with the real
  parser (§3.3.3).
- **Contract:** as written, with two additions the implementation forced:
  ```go
  type IR struct {
      Version string
      Ops []Op // {ID, Method, Path, Params, RequestBody *Schema, Responses}
  }
  func LoadOpenAPI(path string) (*IR, error) // 3.0/3.1; $ref resolution bounded;
                                            // remote refs refused (offline, I-3)
  type ErrRemoteRef struct{ Ref string }
  type ErrRefCycle struct{ Ref string }
  type ErrUnsupportedDocument struct{ Why string }
  ```
  The typed errors are the addition. A schema reader's failure modes need to be
  *distinguishable by a caller*, because "this document is bad" and "this
  machine cannot reach the file" call for different responses.
- **Challenge:** `$ref` cycles and remote refs must fail closed; the loader resolves
  local refs with a visited-set and rejects any URI with a scheme (offline-first).
  **Resolved, and the implementation generalised the rule:** an unresolvable
  `$ref` is fatal for the WHOLE load, at eight independent sites. The first
  draft skipped one of them, which `TestLoadOpenAPIRejectsRemoteRefs` caught: a
  document whose only constraint lived behind a remote ref produced a corpus
  that silently omitted it. A partial IR is indistinguishable from a complete
  one, and a boundary tester that reports the wrong coverage is worse than one
  that refuses.
- **Additional challenge, not in the original contract:** a hand-rolled YAML
  parser is a *refusing* parser. Anchors, aliases, tags, block scalars, complex
  keys and tab indentation each produce a typed error naming the line. A lenient
  parser that mis-nests a schema produces payloads for constraints nobody
  declared and reports them as coverage — confident and wrong rather than loud.
- **Verify:** Fixture suite: 3.0 and 3.1 documents, cyclic refs, remote refs — each
  asserting exact IR or the structured error; G-mut on loader fixes.
  **SHIPPED** (MEMORY.md ADL-0022; TASKS.md P3.3a.1). Gates:
  `TestLoadOpenAPIFromYAML`, `TestLoadOpenAPIFromJSON`,
  `TestLoadOpenAPIRejectsRemoteRefs`, `TestLoadOpenAPIRejectsRefCycles`,
  `TestLoadOpenAPIRejectsNonOpenAPIInput` (8 bad documents). Mutations M37
  (green — a structural no-op, eight independent enforcement sites, recorded in
  the script) and M38 (red — the one site with no twin).

### 3.3.2 Deterministic payload generation

- **Prereq:** 3.3.1 IR; `math/rand/v2` PCG seeded per run id (reproducible).
- **Contract:**
  ```go
  type Strategy uint8
  const (Boundary Strategy = iota; TypeConfusion; Encoding; Oversize; Injection)
  func Gen(s *Schema, seed uint64, budget int) iter.Seq[map[string]any]
  // boundary: 0, -1, ±max, empty string, length-1/limit+1 strings
  // type confusion: string↔number↔bool↔null↔array↔object swaps
  // encoding: unicode, surrogates, overlong UTF-8 (as raw bytes), base64 blobs
  // oversize: field at 1<<20 bytes
  // injection: SQLi/NoSQLi/SSRF-shaped probes targeted at localhost ONLY
  ```
- **Challenge:** Generated payloads can themselves carry secrets (if the schema
  example values do) — every generated body passes `RedactString` before storage,
  and SSRF-shaped probes are refused against non-loopback targets unless explicitly
  allowed (target allowlist in `pulse.toml`).
- **Implementation note — the allowlist was NOT built.** The loopback restriction
  is unconditional and enforced structurally at three points: the target is parsed
  and its host checked *numerically* (`net.IP.IsLoopback`, so a decimal-encoded
  `2130706433` or an IPv4-mapped `::ffff:8.8.8.8` cannot pass a prefix
  comparison); the transport sets `Proxy: nil`, so `HTTP_PROXY` cannot carry a
  `127.0.0.1` target through a proxy host; and the fuzz transport is the only one
  used. An allowlist that can re-admit non-loopback targets is a downgrade from an
  unconditional guarantee, so the contract is tightened rather than loosened — a
  deliberate departure from the original wording, recorded in ADL-0022.
- **Verify:** Determinism test (same seed → byte-identical corpus); strategy
  coverage test (each strategy emits ≥1 distinct failure mode on a seeded buggy
  server); redaction test on stored findings.
  **SHIPPED** (ADL-0022; TASKS.md P3.3a.2, P3.3a.5). Determinism is proven over
  26 generations: `TestGenerateIsDeterministic`. Coverage of *declared
  constraints* — not of strategies — is the stronger property, because a
  generator that skips `maxLength` because of a typo reports a clean run having
  tested nothing: `TestGenerateCoversDeclaredConstraints`,
  `TestGenerateCoversEverySchemaType`, `TestGenerateProbesDeclaredPatterns`,
  `TestGenerateEmitsEveryStrategy`. The seed cannot remove a declared-constraint
  case: `TestGenerateIsSeedStable` over five seeds. A budget truncates by
  even-stride rather than by prefix, because a prefix would only ever exercise
  the first parameter alphabetically: `TestGenerateHonoursTheBudget`. Redaction
  of the emitted diagnostic: `TestFuzzToolRedactsDiagnostics`. Loopback:
  `TestFuzzToolRejectsNonLoopbackTargets`, `TestRequireLoopbackRejectsNumericSneaks`,
  `TestFuzzTransportIgnoresTheProxyEnvironment`.

### 3.3.3 Execution loop and findings

- **Prereq:** 3.3.2 generator; engine run path; MCP `run-request` handler.
- **Contract:** the loop is MCP-native: the local LLM client drives
  `fuzz-start {openapi_ref, strategy, budget}` → `fuzz-next` → `run-request` per
  payload → `fuzz-report`; findings dedup by signature `sha256(status + canonical
  response prefix)`; findings persist to `history` with `run_source='fuzz'` plus a
  `findings` table (payload, signature, status, first_seen, last_seen).
- **Challenge:** Unbounded fuzz loops must be interruptible and budgeted: the loop
  honors the per-run context (cancellation via Ctrl+X / MCP cancel) and a
  configured max-payload count; in-flight runs always drain through the existing
  teardown path (Goal 1 §1.2.6).
- **Verify:** Cancellation mid-fuzz test (no orphaned runs); dedup test (identical
  responses collapse to one finding); budget test asserts hard stop.
- **Implementation note — SHIPPED AS A SINGLE-SHOT TOOL, streaming NOT built.**
  `execute_fuzz_run` runs the whole budget under one limiter and returns a TAP 13
  document; the client does not drive `fuzz-start`/`fuzz-next` per payload. The
  bound, the interruptibility and the teardown path are all present — a cancelled
  run emits the remaining cases as SKIP so the plan stays complete, and a hanging
  handler costs one timeout rather than the run — so the streaming contract is a
  reshaping of the loop rather than a new mechanism. It is **not built** and is
  not claimed (TASKS.md P3.3a.11).
- **Implementation note — the rate limiter is the contract's load-bearing part and
  it is NOT a token bucket.** GCRA reserves the slot at decide time, giving
  `admission(i) >= t0 + i/R - (C-1)/R` and therefore **no window of length Δ
  holds more than `C + R·Δ` admissions**. The textbook construction fails under
  concurrency: N waiters sleep the same duration and wake together, admitting a
  burst of N exactly when the limiter matters. Measured — 1,000 concurrent
  requests at a real `httptest` sink, 200.6/s against 200/s configured, worst
  1-second window 203 against a bound of 204. The gate measures the *world*:
  under mutation M33 the limiter's own observations stay correct and only
  timestamps taken at the sink detect the defect.
- **Implementation note — the transport is part of the rate guarantee.**
  `http.Transport` transparently retries an idempotent request whose pooled
  connection the server closed, so the target received more requests than the
  limiter scheduled; `DisableKeepAlives` removes the race. Found by
  `TestEngineRunnerSendsExactlyOneRequestPerPayload`, which counts requests at
  the server rather than at the limiter.
- **Implementation note — probes travel the real engine path.** Each payload is
  rendered to `.http`, parsed by the real AST and executed by `engine.Run`, so a
  probe meets the same variable expansion, redaction, deadlines and bounded
  reads as a hand-written request. Three consequences followed and are fixed: a
  round-trip `Resolver` makes `{{...}}` probes byte-exact instead of being
  consumed by Pulse's own expander; an **undeliverable** probe (one carrying a
  byte RFC 7230 forbids in a header) is SKIPPED with a reason rather than
  reported as a server finding; and 2xx and 4xx both PASS, because a boundary
  value the server correctly rejects is the behaviour under test.
- **Move 10 closed the reachability half:** `internal/app` is the composition
  root, and `execute_fuzz_run` is reachable end to end from `pulse --mcp`.
  `TestCompositionRootFuzzEndToEnd` drives the real stdout lock, the real stdio
  loop over in-memory pipes, the real tool, the real fuzzer and the real engine
  against a real `httptest` target, and asserts the requests actually arrived —
  counted **at the server**, not at the runner, because a tool that reported
  success without sending anything would satisfy every other assertion.
- **NOT BUILT:** finding persistence and dedup (`sha256(status + canonical
  response prefix)`, the `findings` table, `run_source='fuzz'`). No tool
  handler performs the write and no `findings` table exists. A composition root
  removes the *wiring* blocker; it does not implement a schema. Recorded as
  TASKS.md P3.3a.10.
- **SHIPPED in part** (ADL-0022; TASKS.md P3.3a.3–P3.3a.8). Gates:
  `TestRateLimiterNeverExceedsTheConfiguredRate`,
  `TestRateLimiterEnforcesTheBoundUnderContention`,
  `TestRateLimiterObservationsRespectTheInvariant`,
  `TestLimiterCancellationDoesNotWidenTheRate`, `TestSemaphoreBoundsConcurrency`,
  `TestFuzzToolCompletesThePlanOnCancellation`,
  `TestTAPDocumentIsWellFormed`, `TestTAPFailureEmbedsTheTriggeringPayload`,
  `TestTAPSkippedTestsAreMarked`, `TestUndeliverableProbeIsSkippedNotFailed`,
  `TestEngineRunnerSendsExactlyOneRequestPerPayload`,
  `TestEngineRunnerDeliversTemplateInjectionIntact`. Mutations M32–M41.

### 3.3.4 Goal 3 bridge status (Move 10)

Goal 3's **MCP** half is now bridged end to end: a local agent launches
`pulse mcp`, gets `execute_request` and `execute_fuzz_run`, and both run
against real subsystems through the composition root. Goal 1's **interactive**
half is not — the TUI does not exist and `pulse` with no arguments still exits
1 — and Goal 3 §3.4 (Wasm sandboxing) is untouched.

The move that made this possible was the composition root, and the signal that
it was needed was structural rather than functional: three open items across
three separate moves recorded the same root cause and none of them could fix it,
because the bridge constructed its own dependencies. The general lesson recorded
in ADL-0023 is that **a package which constructs its own collaborators cannot be
exercised without constructing all of them**, and that is a testability defect
before it is an architectural one.

## 3.4 Wasm Sandboxing (wazero) for Auth Signing Scripts

### 3.4.1 Runtime embedding and instantiation

- **Prereq:** `github.com/tetratelabs/wazero` (pure Go, §0.4); interpreter-only
  runtime config (deterministic, no JIT codegen surfaces).
- **Contract:**
  ```go
  ctx := context.Background()
  r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
  defer r.Close(ctx)
  // guest modules instantiated from a pinned, sha-verified .wasm artifact
  // (Rust/TS compile targets, committed in-repo; never fetched at runtime).
  memCfg := wazero.NewMemoryConfig().WithMaxPages(64) // 4 MiB cap per instance
  mod, err := r.InstantiateWithConfig(ctx, code, moduleName,
      wazero.NewModuleConfig().WithName(instName).WithMemoryConfig(memCfg).
          WithStartFunctions()) // start functions run; no WASI imported
  ```
- **Challenge:** Instantiation cost per request is real; instances are pooled
  (`sync.Pool` of instantiated modules keyed by script sha) and reused across runs;
  a poisoned instance (trap) is discarded, never returned to the pool.
- **Verify:** Instantiation/pool benchmark; trap-recovery test (guest traps →
  structured error, instance discarded, next run unaffected); race detector across
  concurrent pooled guests.
- **SHIPPED (Move 11, MEMORY.md ADL-0024).** `internal/wasm` builds the
  interpreter runtime with the memory cap at 4 MiB, `CloseOnContextDone(true)`,
  and **no host modules instantiated at all** — which is what makes every import
  fail to resolve and the guest refuse to instantiate. `WithStartFunctions()` is
  cleared explicitly, stdin is always-EOF, stdout and stderr are `io.Discard`,
  and `WithEnv`/`WithFS`/`WithSysWalltime`/`WithSysNanotime`/`WithSysNanosleep`/
  `WithRandSource`/`WithOsyield`/`WithArgs` are all unset. There is no capability
  to revoke later, because none is ever granted.
  Gates: `TestAGuestCannotReachTheHost`, `TestTheABIIsRequired`,
  `TestDefaultLimitsAreTheDocumentedOnes`, `TestCloseIsIdempotent`.
  Mutations M54 (WASI linked in) and M59 (the memory-ABI check skipped) red.
  NOT met: the instantiation-cost benchmark and the pooling of instances keyed by
  script sha. Each `Module` owns its own runtime, so there is nothing to pool yet;
  the interpreter's cost is paid per guest, which is correct for a tool running a
  few hundred requests a second and wrong for a hot path.
- **SHIPPED (Move 11, MEMORY.md ADL-0024).** `internal/wasm` builds the
  interpreter runtime with the memory cap at 4 MiB, `CloseOnContextDone(true)`,
  and **no host modules instantiated at all** — which is what makes every import
  fail to resolve and the guest refuse to instantiate. `WithStartFunctions()` is
  cleared explicitly, stdin is always-EOF, stdout and stderr are `io.Discard`,
  and `WithEnv`/`WithFS`/`WithSysWalltime`/`WithSysNanotime`/`WithSysNanosleep`/
  `WithRandSource`/`WithOsyield`/`WithArgs` are all unset. There is no capability
  to revoke later, because none is ever granted.
  Gates: `TestAGuestCannotReachTheHost`, `TestTheABIIsRequired`,
  `TestDefaultLimitsAreTheDocumentedOnes`, `TestCloseIsIdempotent`.
  Mutations M54 (WASI linked in) and M59 (the memory-ABI check skipped) red.
  NOT met: the instantiation-cost benchmark and the pooling of instances keyed by
  script sha. Each `Module` owns its own runtime, so there is nothing to pool yet;
  the interpreter's cost is paid per guest, which is correct for a tool running a
  few hundred requests a second and wrong for a hot path.

### 3.4.2 Host/guest memory passing conventions

- **Prereq:** 3.4.1 runtime; host module functions.
- **Contract:**
  ```go
  // guest exports: alloc(size)->ptr ; dealloc(ptr) ; sign(payload_ptr,len)->(sig_ptr,sig_len)
  // host writes into guest linear memory; host reads results back, always bounds-checked.
  fn := mod.ExportedFunction("sign")
  _, err := fn.Call(ctx, uint64(payloadPtr), uint64(len(payload)))
  sig, ok := mod.Memory().Read(uint32(sigPtr), uint32(sigLen)) // ok=false → trap path
  // chunk pool buffers are copied into guest memory via Memory().Write; host never
  // passes Go pointers into wasm and never retains slices into guest memory.
  ```
- **Challenge:** The pointer/len protocol is a classic memory-safety footgun: a
  malicious or buggy guest can return out-of-bounds pointers. Every `Memory().Read`
  is bounds-checked (`ok` flag), sizes are capped (payload ≤1 MiB, signature
  ≤512 B), and returned slices are copied into pooled Go buffers before the next
  guest call can clobber them.
- **Verify:** Malformed-guest fuzz: bogus pointers/huge lengths → structured
  `ErrGuestBounds`, never a panic; ASAN-adjacent stress under the race detector.
- **SHIPPED (Move 11), WITH A DELIBERATE DEVIATION (ADL-0024).** The guest
  exports `allocate(size) -> ptr`, `sign_request(ptr, size) -> i32` and `memory`.
  The host allocates `len + 4 KiB`, writes the envelope, calls the signer, and
  reads back.

  **The `(sig_ptr, sig_len)` sketch above is not what was built, and the reason
  is the safety argument rather than convenience.** A guest-returned pointer is
  an address the host must trust, and a bounds check that is wrong is a
  memory-safety bug in the host. Pinning the output to the buffer the host
  already allocated leaves only a LENGTH as guest-supplied, and a length is
  checked against a buffer the host owns — there is no address for a malicious
  guest to choose. The host passes the ENVELOPE length rather than the capacity,
  so a guest cannot mistake the reserved tail for content and sign the wrong
  bytes.

  Ranges are checked as `ptr > avail` and `size > avail - ptr`. The subtraction
  is not stylistic: `ptr + size` wraps on uint32, so the obvious form is
  trivially true at `ptr = 2^32-1`, which is the first value a hostile guest
  would return. `ErrBounds` (a host bug) and `ErrABIViolation` (a guest bug) are
  distinct types for the same reason.
  Gates: `TestSignRoundTripIsTheExecutionGate`,
  `TestTheHostCanSeeTheEnvelopeBeforeSigning`, `TestABIViolationsAreRefused`,
  `TestBoundsCheckSurvivesTheWrapCase`, `TestMemorySizeIsReadLive`,
  `TestEnvelopeCeilingIsEnforced`.
  Mutations M55b, M56, M57b red.
- **SHIPPED (Move 11), WITH A DELIBERATE DEVIATION (ADL-0024).** The guest
  exports `allocate(size) -> ptr`, `sign_request(ptr, size) -> i32` and `memory`.
  The host allocates `len + 4 KiB`, writes the envelope, calls the signer, and
  reads back.

  **The `(sig_ptr, sig_len)` sketch above is not what was built, and the reason
  is the safety argument rather than convenience.** A guest-returned pointer is
  an address the host must trust, and a bounds check that is wrong is a
  memory-safety bug in the host. Pinning the output to the buffer the host
  already allocated leaves only a LENGTH as guest-supplied, and a length is
  checked against a buffer the host owns — there is no address for a malicious
  guest to choose. The host passes the ENVELOPE length rather than the capacity,
  so a guest cannot mistake the reserved tail for content and sign the wrong
  bytes.

  Ranges are checked as `ptr > avail` and `size > avail - ptr`. The subtraction
  is not stylistic: `ptr + size` wraps on uint32, so the obvious form is
  trivially true at `ptr = 2^32-1`, which is the first value a hostile guest
  would return. `ErrBounds` (a host bug) and `ErrABIViolation` (a guest bug) are
  distinct types for the same reason.
  Gates: `TestSignRoundTripIsTheExecutionGate`,
  `TestTheHostCanSeeTheEnvelopeBeforeSigning`, `TestABIViolationsAreRefused`,
  `TestBoundsCheckSurvivesTheWrapCase`, `TestMemorySizeIsReadLive`,
  `TestEnvelopeCeilingIsEnforced`.
  Mutations M55b, M56, M57b red.

### 3.4.3 Zero-trust host functions (keys never enter the sandbox)

- **Prereq:** 3.4.2; keychain backend (Goal 1 §1.3.1); stdlib `crypto/hmac`.
- **Contract:**
  ```go
  // host module "pulse/env" v1 — the ONLY imports a guest may declare:
  //   now_ns() -> u64                          (monotonic clock)
  //   header_get(name_ptr,name_len)->i64       (request headers, redacted output)
  //   hmac_sha256(key_id_ptr,key_id_len, msg_ptr,msg_len)->i64
  //       → host fetches the key BY ID from the keychain and computes HMAC in Go;
  //         key material never crosses into guest memory.
  //   log(level, ptr, len)                     → stderr only; stdout forbidden
  // guest computes canonical request bytes; host performs the MAC; guest assembles
  // the Authorization value and returns it via the memory protocol.
  ```
- **Challenge:** A guest that could read keys or write to stdout would defeat both
  the vault (I-8) and transport segregation (I-4). The import surface is therefore a
  closed whitelist, `log` is stderr-only by construction, and HMAC/AWS-v4-style
  signing chains are host-side with key ids as the only handle the guest sees.
- **Verify:** Import-whitelist test (guest declaring anything else fails to
  instantiate); stdout-in-guest test asserts zero bytes on the wire; end-to-end
  HMAC and AWS-v4 canonical-request tests against Go reference implementations.
- **SHIPPED (#6, ADL-0038): all four host functions implemented.** `pulse/env`
  exposes `hmac_sha256(key_id_ptr,key_id_len,msg_ptr,msg_len,out_ptr,out_cap)->i32`,
  `now_ns()->i64`, `header_get(name_ptr,name_len,out_ptr,out_cap)->i32`, and
  `log(ptr,len)` (stderr sink, never stdout). Two deliberate deviations from
  the contract sketch above, both in the direction of §3.4.2's in-place
  philosophy: (1) hmac and header_get take explicit `(out_ptr,out_cap)` ranges
  rather than returning an `i64` handle, so no guest-chosen pointer is ever
  trusted — only lengths against host-checked buffers; (2) header_get answers
  sensitive headers (Authorization, Cookie, …) as `[REDACTED]`, presence
  without content. Every range uses subtraction-form bounds checks, proven by
  `TestHostBoundsChecksAreSubtractionForm` (mutation M86 red→green).
  Tracked as TASKS.md P3.4.6.

### 3.4.4 Sandbox limits, traps, and metering

- **Prereq:** 3.4.1; context deadlines.
- **Contract:** per-call deadline propagated via `CloseOnContextDone`;
  memory capped at 4 MiB (3.4.1); **fuel metering is unavailable in wazero** — the
  compensating controls are the memory cap, the context deadline, the interpreter
  (bounded instruction cost), and the closed import whitelist; all limits are
  documented as the security boundary and asserted by tests.
- **Challenge:** Without fuel, a tight infinite loop in the guest relies on the
  deadline; `CloseOnContextDone` unwinds the call deterministically, but the pooled
  instance must be discarded afterward (its memory may be inconsistent).
- **Verify:** Infinite-loop guest test asserts deadline-bounded unwind and instance
  discard; memory-cap test (guest exceeding 4 MiB traps); limit-surface documentation
  test (a golden doc snippet matching the enforced config).
- **SHIPPED (Move 11), WITH THREE MEASUREMENTS THAT CHANGED THE DESIGN (ADL-0024).**
  The contract above is right that fuel is unavailable; three findings beyond it
  are worth recording, because each produced code that would otherwise have been
  confidently wrong.

  1. **A panicking `FunctionListener` does not abort a guest call.** The panic
     escapes into the host and the call does not return, so "abort from the
     listener" is not a fuel mechanism either.
  2. **A listener attached to the context passed to `Call` is silently ignored.**
     wazero binds it at `CompileModule` time. The first draft attached it per
     call and counted nothing — indistinguishable, from the logs, from a budget
     that was merely generous.
  3. **A call budget is a DETECTOR, not a preemptor.** With the listener bound
     correctly, a 20,000-call guest ran against a budget of 50 and completed:
     the listener cancels the context on call 51 and the guest runs to the end
     of its straight-line body, because there is no safe point until there is.

  So the two tiers do different jobs, and the "compensating controls" list above
  was incomplete. **TIMEOUT is what guarantees termination** (measured 50.4ms
  against a 50ms deadline for `(loop br 0)`). **FUEL guarantees that "it
  survived" is not mistaken for "it behaved"**: it is deterministic — it counts
  calls, not milliseconds, so it decides the same way on a loaded CI runner as on
  an idle laptop — and it poisons the module, so a guest detected over budget is
  never reused. A timeout-only sandbox stops a hanging guest and then reuses it;
  a fuel-only sandbox would notice a runaway and do nothing.

  wazero's interpreter additionally enforces its own call-stack bound, which
  traps deep recursion cleanly before any large budget could fire. That is a
  third, runtime-owned bound, and `TestDeepRecursionTrap` records it rather than
  letting fuel appear to cover depth.

  The memory-cap "traps" wording above is also imprecise as built:
  WebAssembly's `memory.grow` REFUSES an over-limit growth (returns -1) rather
  than trapping, so the security property is that the ceiling HOLDS, not that the
  call fails.
  Gates: `TestInfiniteLoopIsStoppedByTheTimeBudget`,
  `TestTimeBudgetIsBoundedNotMerelyConfigured`,
  `TestCallBudgetIsDetectedAndTheModuleIsPoisoned`,
  `TestAGuestUnderBudgetIsNotPoisoned`, `TestDeepRecursionTrap`,
  `TestFuelBudgetIsEnforcedAcrossCalls`, `TestSpentModuleIsNotReused`,
  `TestMemoryLimitIsEnforced`, `TestUnlimitedIsNotConfigurable`,
  `TestASpentTimeoutDoesNotLeaveTheHostRunning`.
  Mutations M50, M51, M52, M53, M58 red.

  NOT met: the golden limit-surface documentation test. The limits are asserted
  against the code (`TestDefaultLimitsAreTheDocumentedOnes`) but not diffed
  against this document, so the two can drift without failing anything.
- **SHIPPED (Move 11), WITH THREE MEASUREMENTS THAT CHANGED THE DESIGN (ADL-0024).**
  The contract above is right that fuel is unavailable; three findings beyond it
  are worth recording, because each produced code that would otherwise have been
  confidently wrong.

  1. **A panicking `FunctionListener` does not abort a guest call.** The panic
     escapes into the host and the call does not return, so "abort from the
     listener" is not a fuel mechanism either.
  2. **A listener attached to the context passed to `Call` is silently ignored.**
     wazero binds it at `CompileModule` time. The first draft attached it per
     call and counted nothing — indistinguishable, from the logs, from a budget
     that was merely generous.
  3. **A call budget is a DETECTOR, not a preemptor.** With the listener bound
     correctly, a 20,000-call guest ran against a budget of 50 and completed:
     the listener cancels the context on call 51 and the guest runs to the end
     of its straight-line body, because there is no safe point until there is.

  So the two tiers do different jobs, and the "compensating controls" list above
  was incomplete. **TIMEOUT is what guarantees termination** (measured 50.4ms
  against a 50ms deadline for `(loop br 0)`). **FUEL guarantees that "it
  survived" is not mistaken for "it behaved"**: it is deterministic — it counts
  calls, not milliseconds, so it decides the same way on a loaded CI runner as on
  an idle laptop — and it poisons the module, so a guest detected over budget is
  never reused. A timeout-only sandbox stops a hanging guest and then reuses it;
  a fuel-only sandbox would notice a runaway and do nothing.

  wazero's interpreter additionally enforces its own call-stack bound, which
  traps deep recursion cleanly before any large budget could fire. That is a
  third, runtime-owned bound, and `TestDeepRecursionTrap` records it rather than
  letting fuel appear to cover depth.

  The memory-cap "traps" wording above is also imprecise as built:
  WebAssembly's `memory.grow` REFUSES an over-limit growth (returns -1) rather
  than trapping, so the security property is that the ceiling HOLDS, not that the
  call fails.
  Gates: `TestInfiniteLoopIsStoppedByTheTimeBudget`,
  `TestTimeBudgetIsBoundedNotMerelyConfigured`,
  `TestCallBudgetIsDetectedAndTheModuleIsPoisoned`,
  `TestAGuestUnderBudgetIsNotPoisoned`, `TestDeepRecursionTrap`,
  `TestFuelBudgetIsEnforcedAcrossCalls`, `TestSpentModuleIsNotReused`,
  `TestMemoryLimitIsEnforced`, `TestUnlimitedIsNotConfigurable`,
  `TestASpentTimeoutDoesNotLeaveTheHostRunning`.
  Mutations M50, M51, M52, M53, M58 red.

  NOT met: the golden limit-surface documentation test. The limits are asserted
  against the code (`TestDefaultLimitsAreTheDocumentedOnes`) but not diffed
  against this document, so the two can drift without failing anything.

## 3.5 Goal 3 Exit Gates

> **Goal 3 STATUS: COMPLETE (Move 15) — zero-trust host functions, incremental
> RAG reindex, and fuzz finding persistence/dedup are shipped and gated. The
> 50k-chunk p99 latency benchmark remains an argued, not measured, bound.**
>
> Goal 3 asked for three capabilities: a UI-driven workbench (Goal 3.1–3.3), a
> fully online MCP bridge (Goal 3.3), and an untrusted-code sandbox (Goal 3.4).
> As of Move 11:
>
> **CLOSED.** The MCP bridge: `pulse mcp` builds the composition root
> (`internal/app`, Move 10) and serves `execute_request` and `execute_fuzz_run`
> end to end over stdio against real subsystems, with the redaction chokepoint
> wired at both layers and the 5xx explanation reachable from a running server.
> The sandbox: `internal/wasm` runs an untrusted guest with zero ambient
> authority under a proven time budget and a detected call budget.
>
> **OPEN, and NOT claimed:**
>   1. **§3.4.3 zero-trust host functions and the HMAC-by-key-id chain.** No
>      host functions are registered, so a guest can neither read a key nor
>      compute a signature.
>   2. **RAG: the 50k-chunk p99 latency benchmark (§3.2.9)** — the O(N·d) cost
>      is argued from operation count, not measured.
>   3. **RAG: incremental reindex and the branch-switch simulation (§3.2.5)** —
>      the primitives exist and are gated; the watcher does not.
>   4. **Fuzzing: finding persistence and dedup (§3.3.3)** — no `findings`
>      table, no signature, and the streaming `fuzz-start`/`fuzz-next` loop is
>      unbuilt.
>
> The directive accompanying Move 11 asked for Goal 3 to be marked COMPLETE.
> **It is recorded as PARTIAL on purpose.** AGENTS.md §4.5 requires every test and
> every claim to be honestly disposed, and §4.6 requires ROADMAP.md to be the
> scope authority; a goal whose own exit-gate checklist lists four unmet items
> cannot be truthfully closed, and closing it here would launder those four items
> out of the document that exists to track them. They are listed above so the
> next move inherits them rather than inherit a green facade.

### 3.5.1 Gate checklist

- G-hyg: zero stdout writes in MCP paths; stray-write regression test fails build.
  **MET.** Two layers (MEMORY.md ADL-0020), three dynamic regression gates,
  importaudit check 3, and now held through composition-root construction AND
  serving (Move 10: `TestAppNeverWritesToStdout`,
  `TestAppServesWithoutASingleStrayByte`). Mutation M19.
- Lifecycle conformance against a scripted MCP client (3.1.4).
  **MET.** Not only against a scripted client but end to end against a real one:
  `TestCompositionRootFuzzEndToEnd` drives `mcp.Lock`, the stdio loop over
  in-memory pipes, the tool registry, the fuzzer, the engine, and a real
  `httptest` target, asserting that the requests actually arrived — counted at
  the SERVER, not at the runner.
- RAG: determinism, recall fixture, secret-free chunks, DEGRADED path when no model.
  **Partially met (ADL-0021, updated by ADL-0023).** Determinism, recall,
  secret-free chunks and the DEGRADED path are green, and **the composition root
  that blocked reachability from a running server shipped in Move 10** — a 5xx
  from `pulse --mcp` now carries source context, gated by
  `TestCompositionRootWiresTheFailureBinder`. STILL NOT MET: the 50k-chunk p99
  latency benchmark (§3.2.9) and the incremental-reindex branch-switch
  simulation (§3.2.5).
- Fuzzing: determinism, dedup, cancellation, budget.
  **Partially met.** Determinism, cancellation and budget are green; the rate
  gate was rewritten onto the quantity it actually constrains (TASKS.md P3.3a.3)
  and two assertions were added to it. **Dedup is NOT met** — the
  `sha256(status + canonical response prefix)` signature, the `findings` table
  and `run_source='fuzz'` do not exist.
- Wasm: bounds tests, import whitelist, key-never-enters-guest proof, trap recovery.
  **Partially met.** Bounds tests, import whitelist and trap recovery are green
  (gates and M50–M59 listed under §3.4.2 and §3.4.4). **The
  key-never-enters-guest proof is NOT met**: §3.4.3 registers no host functions,
  so a guest cannot read a key — but also cannot sign at all, which is not the
  capability the section specifies.
- Dependency registry updated; ADLs appended; `ARCHITECTURE.md` MCP/RAG/wasm
  sections synced same-commit. **MET.** `wazero v1.12.0` added to
  `allowed-modules.txt` with the no-fuel measurement recorded inline;
  ARCHITECTURE.md §15 and §7 updated in the same commit.

---

# Goal 4 — Zero-Cloud Collaboration & CI/CD Supremacy

**Intent.** Close the loop: a headless `pulse test` pipeline that CI systems can
drive, peer-to-peer collection sync that never touches a Pulse-operated server, and
a vault that lets teams commit production configuration to Git while private keys
never leave their OS keychains.

## 4.1 Headless Pipeline: `pulse test` — COMPLETE (Issue 5, ADL-0032)

**COMPLETE.** 4.1.1 shipped in Move 12; 4.1.2 shipped in Move 12; the 4.1.3
formats and the 4.1.4 exit/signal surface shipped across P4.1c.10 (JSON/silent),
P4.1c.11 (SIGINT-130) and Issue 5 (vault-transparent resolution: a vault-only
run resolves through the boot-loaded tier and exits 0, an unresolvable run
exits 3 via `App.resolver()`). The stale per-section "NOT met" lines below
are corrected in place; nothing under 4.1 remains open.

### 4.1.1 Assertion model

- **Prereq:** Goal 1 AST extended with `# @assert` lines (`TokAssert`, 1.1.1);
  empty `.http` files parse to zero requests without error.
- **Contract:**
  ```go
  // grammar:  # @assert status == 200
  //           # @assert status in 200..299
  //           # @assert duration < 500ms
  //           # @assert body contains "ok"
  //           # @assert header Content-Type contains "json"
  type Assertion interface{ Eval(res *RunResult) error }
  func ParseAssert(line string) (Assertion, error)
  ```
- **Challenge:** Assertion expressions invite a full language (creep). The grammar
  is deliberately tiny and total: every expression parses or fails closed with a
  `file:line:col` diagnostic; there is no evaluation scripting surface.
- **Verify:** Golden tests per operator (including boundary equality and
  `in range` semantics); malformed-assert corpus each asserting the structured
  parse error and exit 2.
- **SHIPPED (Move 12), WITH ONE DELIBERATE DEVIATION (ADL-0025).** The grammar is
  six operators over five subjects and nothing else, in `internal/engine/assert.go`.
  `in lo..hi` is inclusive at BOTH ends and `<` is strict; both are pinned by
  boundary tests rather than by prose. A subject that cannot be read FAILS rather
  than passing vacuously, so a typo'd subject cannot report green for a file.
  `EvalBody(res, body)` takes the body SEPARATELY from `*RunResult`: `RunResult`
  is a bounded summary carrying Bytes and a Digest but not the body, so
  `body contains` cannot be evaluated against it, and asserting on the Digest
  would be a different and much weaker statement. Reading `header <name>`
  canonicalises the name, because Go's `http.Header` is keyed by the canonical
  MIME form and a `.http` file written `content-type` would never match.
  Gates: `TestAssertionGrammarIsTotal` (24 expressions), `TestAssertionBoundarySemantics`,
  `TestBodyAssertionsFailWithoutABody`, `TestHeaderAssertionCanonicalises`.
  Mutation M68 (the assertion's verdict discarded) red.

### 4.1.2 Execution flow and concurrency

- **Prereq:** 4.1.1; engine run path; errgroup-style bounded parallelism.
- **Contract:** collection (or the file argument) is parsed once, variables resolved
  before each request, then requests run with bounded parallelism
  (`--parallel N`, default derived from a conservative cap, not core count — CI
  machines punish aggressive fan-out); per-request context timeout
  (`--timeout`); collection-level teardown via `context.WithoutCancel` (Goal 1
  §1.2.6).
- **Challenge:** Fail-closed semantics under parallel runs: a secret-resolution
  failure in one request must abort the whole run (exit 3) without cancelling
  unrelated in-flight requests' output lines — output is serialized through a single
  writer (4.1.3) regardless of parallelism.
- **Verify:** Parallel run test with mixed pass/fail/secret-failure asserting exact
  output ordering (by completion, serialized) and exit code; cancellation test.

### 4.1.3 Output formats: TAP, JSON, silent

- **Prereq:** 4.1.2 results; TAP (Test Anything Protocol) 13.
- **Contract:**
  ```go
  // TAP:
  //   1..N
  //   ok 1 - GET /users returns 200
  //   not ok 2 - POST /users contains "ok"
  //     ---
  //     message: body did not contain "ok"
  //     severity: fail
  //     ...
  //   ok 3 # SKIP (reason: requires keychain, BLOCKED in this environment)
  // lines are flushed (unbuffered, one write per line) for CI liveness.
  // JSON: newline-delimited run records (redacted). silent: exit code only.
  ```
- **Challenge:** TAP consumers parse incrementally; buffered writers that flush at
  exit break streaming dashboards. The writer is unbuffered-per-line with explicit
  flush semantics, and SIGINT emits the plan tail before exiting 130.
- **Verify:** TAP grammar conformance test against a reference TAP consumer;
  SIGINT test asserting plan-tail emission; JSON redaction test; G-hyg in MCP
  contexts (no TAP on stdout when serving).
- **SHIPPED IN PART (Move 12).** TAP 13 only, emitted line-per-write (unbuffered),
  because a TAP consumer parses incrementally and a buffered writer breaks a
  streaming dashboard. The engine produces structured entries and `internal/app`
  formats them onto Move 9's emitter, which satisfies AGENTS.md §3.1's no-sibling-
  import rule AND reuses the emitter — a JSON or silent reporter costs no change
  in the engine.
  Gates: `TestTAPOutputConformsToThirteen`, `TestTAPPlanAndResultsAgreeForAnAllPassingRun`,
  `TestWriteTestReportIsOneWritePerLine`, `TestExitCodeMatrixFromTheCLIPath`.
  Mutation M67 (the plan written from a counter, the same boundary as Move 9's
  M35) red.
  UPDATE (Issue 5): the JSON and silent formats shipped (TASKS.md P4.1c.10:
  `RenderJSONReport`, `--silent` zero-byte stdout, unknown formats exit 2)
  and the SIGINT plan tail with exit 130 shipped (TASKS.md P4.1c.11).
  REMAINS: a conformance test against a REFERENCE TAP consumer rather than
  internal assertions.

### 4.1.4 Exit codes and signal safety

- **Prereq:** AGENTS.md §4.4; 4.1.2.
- **Contract:** 0 = all pass (or BLOCKED-with-reason on non-secret environment
  limits — reported as skip lines, never as pass); 1 = any failure; 2 = usage or
  parse error; 3 = secret-resolution failure; 130 = SIGINT. No exit code 0 unless
  every test point is `ok`.
- **Challenge:** "Greenwash" pressure (AGENTS.md §4.5) peaks here: a blocked test
  must be a visible `# SKIP (reason: ...)` line, and the plan header counts it. CI
  policy files may require `--strict-blocked` to fail the run instead.
- **Verify:** Exit-code matrix test including the `--strict-blocked` mode; audit
  test asserting skip lines carry reasons; mutation gate on exit-code logic.
- **SHIPPED IN PART (Move 12, MEMORY.md ADL-0025).** The exit-code mapping is a
  pure function of the report (`engine.ExitCodeFor`), mirrored from the runner so
  the CLI cannot disagree: 0 all pass, 1 any failure, 2 usage-or-parse, 3
  secret-resolution, and a BLOCKED-with-reason test is a `# SKIP` line that does
  NOT fail the run. **A report with NO test points is exit 2, not 0** — reporting
  success for "nothing happened" is the greenwash AGENTS.md §4.5 exists to
  prevent, and it is gated. The mapping scans for a secret failure BEFORE any
  plain failure so exit 3 does not depend on file ordering; the single-pass
  version made it do so, which mutation M64 caught.
  **An EMPTY report is exit 2** — a run with no test points has not passed
  anything, and M66 proves the gate catches the "nothing happened is a pass"
  mutation.
  Gates: `TestExitCodeMirrorsTAPResults`, `TestExitCodeForIsATotalFunction`,
  `TestExitCodeMatrixFromTheCLIPath` (drives the CLI seam).
  Mutations M63, M64, M65, M65b, M66 all red.
  UPDATE (Issue 5): SIGINT 130 plan-tail emission shipped (TASKS.md P4.1c.11:
  `TestSIGINTFlushesTheCompletedPointsAndExitsOneThirty`,
  `TestInterruptedRunExitsOneThirtyAndRendersPartialDocuments`) and the
  `--strict-blocked` mode has its gate (`TestStrictBlockedEscalatesTheExitCode`,
  TASKS.md P4.1c.3). Nothing further remains under 4.1.4.

## 4.2 Peer-to-Peer Sync (CRDT over mDNS/Tailscale)

### 4.2.1 CRDT data model and Hybrid Logical Clocks

- **Prereq:** Goal 1 AST; `encoding/binary`; device identity in keychain (Ed25519
  key pair, generated on first run).
- **Contract:**
  ```go
  type HLC struct{ WallMS int64; Counter uint32; Node [8]byte }
  func (h HLC) Compare(o HLC) int        // (WallMS, Counter, Node) lexicographic
  func (h *HLC) Observe(o HLC, now time.Time) // merge rule
  type Item struct {                     // one request per file+name
      ID      string
      Fields  map[string]FieldValue      // LWW per field, stamped with HLC
      Body    BodyValue                  // opaque: {Sha256, Bytes} LWW by HLC
      Removed map[uint64]bool           // OR-Set tombstones (add tags)
      Adds    map[uint64]HLC
  }
  ```
- **Challenge:** Concurrent edits to the *same* line of a `.http` file must merge
  deterministically on every peer. Modeling requests as Items with per-field LWW +
  opaque-body LWW sidesteps three-way text merging entirely; HLC ordering needs no
  wall-clock trust beyond monotonicity, and `Observe` keeps counters monotonic under
  clock drift.
- **Verify:** Randomized merge simulation (Go 1.27 `testing/synctest` + deterministic
  seeds): N peers × M random ops → identical state on all peers; HLC property tests
  (monotonicity under drift, tie-breaking by Node id).

### 4.2.2 Merge, digest, and anti-entropy protocol

- **Prereq:** 4.2.1; Merkle digest over sorted `(id, HLC)` pairs.
- **Contract:**
  ```go
  type Digest struct{ Root [32]byte; Count uint32; MaxHLC HLC }
  func Digest(items []*Item) Digest
  // protocol: exchange digests; on divergence, exchange the item-id sets, then
  // states for differing ids only; merge is deterministic and idempotent.
  ```
- **Challenge:** Anti-entropy must converge without unbounded bandwidth: digest
  comparison prunes; ids-with-HLC sets are compact; states transfer lazily. Merge
  determinism means the wire encoding is canonical (fixed field order, sorted keys).
- **Verify:** Convergence test under partition-and-heal simulation (no message loss
  beyond in-flight); idempotence test (applying the same state twice is a no-op);
  canonical-encoding byte-equality test.

### 4.2.3 Peer discovery and transport

- **Prereq:** `github.com/grandcat/zeroconf` (pure Go, §0.4); Tailscale tailnet IPs
  as a manual backend (no tsnet dependency — decision recorded, size rationale).
- **Contract:**
  ```go
  // LAN: advertise/browse "_pulse._tcp." with TXT {node=<id>, hlc=<packed>, proto=1}
  // Tailscale: peers listed in pulse.toml [sync.peers] (100.x addresses), dialed directly
  // wire: length-prefixed frames; TLS 1.3 mutual auth with device Ed25519 certs;
  // TOFU: peer fingerprints pinned in git-tracked peers.txt, mismatch → refuse.
  ```
- **Challenge:** Discovery on hostile LANs must not become an attack surface: the
  mTLS + TOFU pinning means a discovered-but-unpinned peer is *discovered only for
  display*, never synced with until the user pins it explicitly.
- **Verify:** Two-process sync test on loopback (zeroconf on localhost); unpinned-
  peer refusal test; pinning flow test (pin → sync → unpin → refuse again).

## 4.3 Vault Cryptography (Commit-Encrypted Production Config)

### 4.3.1 Key model and container format

- **Prereq:** stdlib only: `crypto/ecdh` (X25519), `crypto/sha256`+
  `crypto/hmac`-based HKDF, `crypto/aes`+`crypto/cipher` (AES-256-GCM),
  `crypto/ed25519`, `clear()` builtin.
- **Contract:**
  ```go
  // Recipient = X25519 public key (32B), base64, listed in git-tracked pulse.recipients
  // Private keys live ONLY in the OS keychain (service "pulse", account "vault:x25519")
  // Container "PVLT1" (binary, committed):
  //   magic "PVLT1" | flags u8 | recipientCount u16
  //   per recipient: ephPub[32] | wrappedDEK[32+16]      // HKDF(ECDH) → AES-GCM wrap
  //   signerPub[32] | nonce[12] | ciphertext | tag[16]    // payload = DEK-encrypted .env
  ```
- **Challenge:** Committing ciphertext to Git is forever — key rotation is mandatory
  and re-encryption is the rotation primitive (4.3.4). The format carries per-event
  nonces and an Ed25519 signature over the ciphertext so tampering is detectable
  before decryption.
- **Verify:** Container round-trip test (encrypt→decrypt byte-identical); tamper
  tests per field (flip a byte in each region → verification or decryption failure,
  never silent); format golden-bytes test.
- **SHIPPED IN PART, WITH A DEVIATION FROM THIS CONTRACT (ADL-0025).** The
  container is `PVLT1`: magic, a reserved flags byte, a 12-byte `crypto/rand`
  nonce, and `gcm.Seal`'s output with the tag appended. AES-256-GCM from the
  stdlib, with NO KDF — the key is 32 bytes of `crypto/rand` held in the
  keychain, and stretching a uniformly random key is theatre.
  **The contract's recipient model is NOT what was built.** There is no
  per-recipient `ephPub | wrappedDEK`, no X25519, no HKDF and no Ed25519
  signature: one key, one container, no recipient list. Key rotation and
  revocation therefore have to mean "change the key", which does not help a
  revoked contributor who already holds the old one — the threat model this
  section explicitly calls out as the reason for the design.
  Gates: `TestContainerFormatRoundTrips`, `TestEnvFileRoundTripThroughDisk`,
  `TestOpenRefusesTamperedContainers` (7 cases: every field flipped, truncated,
  wrong key).
  Mutations M61 (a hardcoded nonce) and M62 (the authentication tag dropped) red.
- **CRYPTO CORE SHIPPED (ADL-0031).** `internal/secrets/pvlt2.go` implements
  the hybrid recipient model: fresh DEK + payload nonce + per-slot ephemeral
  per seal; ECDH → HKDF-SHA256(salt=ephPub, info=`pulse-vault-v1`) wrap with
  zero nonce; `PVLT2` layout with per-slot `ephPub | wrappedDEK`, signer key,
  nonce, ciphertext+tag, and a 64B Ed25519 signature verified BEFORE any
  unwrap. Slot matching is authentication-based (trial-unwrap). Device
  identity (`identity.go`: X25519 + Ed25519 halves, namespaced keychain
  accounts, `pulse-pub-…`/`pulse-sig-…`) and `pulse vault keygen` are wired.
  Gates: 3-key swarm, 12-region tamper matrix, verify-before-decrypt
  ordering, rotation/revocation via re-seal. Mutations M75/M76 red.
  COMPLETE (Issue 5, ADL-0032): the `vault rotate` CLI +
  `pulse.recipients` flow and resolution (§4.3.3) shipped — see below.
  NOT met: multi-recipient round-trip, the per-field tamper MATRIX as specified
  (each region and each byte class), and a golden-bytes test for the container
  format.

### 4.3.2 Commit-time encryption flow

- **Prereq:** 4.3.1; keychain `Get` for the committer's private key; AST/env loader
  (Goal 1 §1.3.2).
- **Contract:** `pulse vault encrypt <plaintext.env>` → read plaintext → DEK =
  `crypto/rand` 32B → AES-GCM encrypt → for each recipient in `pulse.recipients`:
  ephemeral X25519 → ECDH with recipient public → HKDF-SHA256(salt=ephPub,
  info="pulse-vault-v1") → wrap DEK → sign ciphertext with the committer's keychain
  Ed25519 key → write `PVLT1` container → **the plaintext is never written to disk**
  and zeroized in memory after the write.
- **Challenge:** Accidental plaintext leakage (editor swap files, shell history,
  crash dumps). The command only ever *reads* a plaintext path given explicitly,
  writes the container, and zeroizes; CI has a secret-scanner gate over diffs (G-secret)
  that fails commits containing keychain-shaped material.
- **Verify:** End-to-end fixture (3 recipients); G-secret gate on the repo; plaintext-
  zeroization test (post-call scan of the buffer, poisoned buffers).
- **SHIPPED IN PART (ADL-0025).** `pulse vault encrypt <path.env>` reads the
  plaintext, seals it under the keychain-derived key, writes the container at
  0600, and REMOVES the plaintext — a vault next to the `.env` it came from
  protects nothing. The in-memory plaintext is zeroised after the write, and that
  zeroisation is legitimate where `Seal`'s was not: `Seal` was originally
  destroying its CALLER's buffer, which the round-trip gate caught immediately
  (a function that wipes what it was handed cannot be called twice on the same
  bytes), so zeroisation moved to the file path that owns the buffer it read.
  NOT met: the 3-recipient fixture, the G-secret repo scan gate, and rotation.

### 4.3.3 Resolution-time decryption (via authorized keychains)

- **Prereq:** 4.3.1; keychain backend; resolver chain.
- **Contract:** on startup, the resolver detects `*.vault` references in the
  collection env → for the recipient slot matching the local keychain public key:
  ECDH(localPriv, ephPub) → HKDF → unwrap DEK → AES-GCM decrypt → values loaded
  into the ephemeral env store (Goal 1 §1.3.2, memory-only, zeroized on close); no
  recipient match → structured `ErrNoRecipient` → exit 3.
- **Challenge:** A revoked user's machine still holds old containers; that is why
  rotation (4.3.4) must re-wrap; decryption must verify the signature *before*
  attempting DEK unwrap (fail fast, no oracle feedback).
- **Verify:** Multi-recipient fixture (each authorized member decrypts; an
  unauthorized key yields `ErrNoRecipient`); verify-before-decrypt ordering test;
  no-plaintext-on-disk assertion during resolution.
- **SHIPPED (Issue 5, ADL-0032), WITH A DELIBERATE DEVIATION FROM THIS
  CONTRACT.** The contract specifies `*.vault`-reference detection in
  collections; what shipped is transparent boot-time resolution, strictly
  stronger for the stated threat model: `PULSE_VAULT_FILE` names the PVLT2
  container, App init unwraps it once with the device identity and holds it
  memory-only (`Chain.vault`, zeroised by `Chain.Clear` on Close), and custody
  is keychain → explicit .env → vault → `ErrUnresolved` → exit 3 (via
  `App.resolver()`, which translates the secrets sentinel onto
  `engine.ErrUnresolved`). No per-request vault I/O exists to leak timing or
  file handles; a revoked device holds only containers no future re-seal will
  include it in. `pulse vault decrypt` remains the one explicit
  write-plaintext-to-disk path, and it is the user's requested command.
  Gates: seamless vault-only run, precedence, Close-scrub, degrade-to-exit-3.
  Mutations M77/M78 red (`scripts/mutation-proof-vault5.sh`).

### 4.3.4 Rotation and revocation

- **Prereq:** 4.3.2/4.3.3.
- **Contract:** `pulse vault rotate` → generate new DEK → re-encrypt payload →
  re-wrap for the current recipient list (removing revoked keys) → re-sign →
  commit. Revocation is immediate for *new* commits; previously committed containers
  remain readable by the revoked key until team members force-rotate the payload —
  documented explicitly as the threat model.
- **Verify:** Rotation fixture: post-rotation, revoked recipient fails
  (`ErrNoRecipient`), authorized recipients succeed; rotation is idempotent on
  byte-identical inputs except nonce/signature fields.
- **PRIMITIVE SHIPPED (ADL-0031).** Re-`Seal2` over a new recipient list with
  a fresh DEK is rotation and revocation; `TestRotationAndRevocation` proves
  it (revoked key absent from the new container, old containers still open).
  SHIPPED (Issue 5, ADL-0032): `pulse vault rotate` re-seals from the .env —
  or, when the .env is long gone, from the current container decrypted with
  the device key — for every key in the Git-tracked `pulse.recipients` file
  (one `pulse-pub-…` per line, `#` comments; `pulse vault add-recipient`
  appends, duplicates rejected), overwrites the container at 0600, and
  removes the .env it read. Gates: whole-list rotation (alice/bob/carol
  open), vault-only rotation. Mutation M78 (last recipient dropped → carol
  locked out) red.

## 4.4 Goal 4 Exit Gates

### 4.4.1 Gate checklist

- TAP conformance, exit-code matrix, SIGINT tail emission, strict-blocked mode.
- CRDT convergence under randomized partitions (deterministic seeds), canonical
  wire encoding, TOFU pinning tests.
- Vault round-trip, tamper matrix, rotation/revocation fixtures, plaintext-
  never-on-disk assertions.
- G-hyg: no TAP/JSON output on stdout in MCP-serving contexts.
- ADLs appended; `ARCHITECTURE.md` sync/vault sections updated same-commit.

---

# Appendix A — Cross-Reference Index

| Engineering surface | Roadmap anchor | Governing document |
|---|---|---|
| `.http` lexer/parser/AST | 1.1.x | ARCHITECTURE.md §4 |
| Render loop, streaming, pooling | 1.2.x | ARCHITECTURE.md §5; DESIGN_SYSTEM.md §2–3 |
| Keychain, redaction, store schema | 1.3.x | ARCHITECTURE.md §6, §8 |
| SSE / WebSocket | 2.1.x | ARCHITECTURE.md §2 (transport) |
| gRPC + protobuf reflection | 2.2.x | ARCHITECTURE.md §7 (deps) |
| eBPF / syscall telemetry | 2.3.x | ARCHITECTURE.md §7 (deps) |
| MCP stdio bridge | 3.1.x | ARCHITECTURE.md §9 |
| Local RAG | 3.2.x | ARCHITECTURE.md §7 (deps) |
| Auto-fuzzing | 3.3.x | ARCHITECTURE.md §7 (deps) |
| wazero sandbox | 3.4.x | ARCHITECTURE.md §7 (deps) |
| `pulse test` pipeline | 4.1.x | AGENTS.md §4.4–4.5 |
| CRDT P2P sync | 4.2.x | ARCHITECTURE.md §7 (deps) |
| Vault PKI | 4.3.x | ARCHITECTURE.md §6 (keychain) |

**Ordering law.** Goals 1→2→3→4 are dependency-ordered, not schedule-ordered: Goal
2 extends Goal 1 interfaces; Goal 3 builds on Goals 1–2 execution paths; Goal 4
consumes the assertion grammar introduced in Goal 1 §1.1.1 and the engine of Goals
1–3. A goal may not be merged while any of its exit gates (X.4.1) is red, blocked,
or hand-waved.
