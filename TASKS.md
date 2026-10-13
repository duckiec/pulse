# TASKS.md — Pulse Implementation Plan

| Field | Value |
|---|---|
| Document | Chronological Execution Plan |
| Product | Pulse |
| Version | 1.0.0-draft |
| Status | Authoritative |
| Governed by | `AGENTS.md` §4.6 (every completed item MUST be checked off here) |
| Last updated | 2026-10-11 |

---

## How To Use This File

1. Tasks are **chronological** — do not start Phase N+1 until Phase N exit criteria
   are met and checked.
2. Before marking any task complete, satisfy the AGENTS.md §4.7 Definition of Done.
3. Check boxes with `[x]` only when 100% complete, tests pass, and MEMORY.md holds
   the corresponding ADL entry.
4. Every completed task must carry honest test disposition (PASSED / FAILED /
   BLOCKED / NOT RUN with reason) per AGENTS.md §4.5.
5. Adding, removing, or reordering tasks requires a MEMORY.md entry.
6. `ROADMAP.md` is the architectural blueprint and scope authority; execution tasks
   trace to its micro-goals (X.X.X). A task that implements, alters, or invalidates
   a roadmap contract requires a MEMORY.md ADL entry naming the affected anchor, and
   a ROADMAP.md update if an interface or exit gate moves.

---

## Phase 1 — Core Engine: Go 1.27 Module, Zero-CGO SQLite, .http AST

**Goal:** Pure-Go module skeleton that parses `.http` files into a zero-allocation
AST and persists structured history to SQLite — with no UI and no network.

### 1.1 Module & Toolchain
- [x] P1.1.1 Initialize `go.mod` with `go 1.27`; `CGO_ENABLED=0` build verified on linux/amd64, linux/arm64, darwin/arm64, windows/amd64
- [x] P1.1.2 Establish `cmd/pulse` skeleton with flag parsing, semantic exit codes (0/1/2/3), and stub mode selection
- [ ] P1.1.3 Add `modernc.org/sqlite` to the dependency allowlist; CI audit rejects any dependency not sanctioned in ARCHITECTURE.md §7
  - REMAINDER: the audit mechanism is complete and green (P1.1.4); `modernc.org/sqlite` is admitted with `internal/store` in P1.2.
- [x] P1.1.4 Configure `go vet`, `gofmt`, and CI banned-import lint (`mattn/go-sqlite3`, `gorilla/mux`)
- [x] P1.1.5 Enforcement scripts: `scripts/ci/{build-matrix,tidy-check,doc-sync-gate,banned-import-audit,test-gates}.sh` + `scripts/all-gates.sh` runner
- [x] P1.1.6 GitHub Actions CI workflow (`build-matrix`, `audit`, `doc-sync`, `test`, `race` jobs; race detector documented as a test-time-only cgo exception, MEMORY.md ADL-0010)
- [x] P1.1.7 Toolchain bootstrap: `scripts/bootstrap-toolchain.sh` installs pinned go1.27.2 to `~/sdk` with SHA-256 verification (box shipped Go 1.26.8, violating AGENTS.md §2.1)
- [x] P1.1.8 `config/allowed-modules.txt` allowlist (`golang.org/x/term`, `golang.org/x/sys`) + `.gitignore`
- [x] P1.1.9 Harness proofs: mutation red→green transcript (lexer boundary checks) and planted banned-import lint red→green transcript, both green after removal
- [x] P1.1.10 MCP stdio isolation guard: `redirectStdout` swap + `/dev/null` default + `PULSE_STDOUT_AUDIT=1` tripwire; regression test `TestMCPStdoutIsolation`

### 1.2 SQLite Store
- [ ] P1.2.1 Implement `internal/store` schema per ARCHITECTURE.md §8 (`collections`, `requests`, `history`) with embedded migrations
- [ ] P1.2.2 Verify zero credential columns exist (CI grep over migrations)
- [ ] P1.2.3 Store APIs: upsert collection/request, record history run, query history with redaction at read time

### 1.3 .http AST Parser
- [x] P1.3.1 Lexer over `[]byte` with offset-based tokens (no per-token allocs)
- [x] P1.3.2 Recursive-descent parser producing `Document`/`Request`/`Header`/`Body` nodes in a bump arena — `internal/ast/arena.go` (8-byte-aligned bump allocator, retained blocks, `sync.Pool` recycling via `Document.Release`) and `internal/ast/parser.go` (two-pass: counting pre-pass over the same token stream, then LL(1) fill with one token of lookahead and one-token `unget`)
- [x] P1.3.3 `func (d *Document) Requests() iter.Seq[*Request]` lazy traversal (Go 1.27)
- [x] P1.3.4 Grammar coverage: `###` separators, `@var = value`, all methods, JSON/XML/raw bodies, `{{var}}` interpolation markers (unresolved at parse time) — golden-proven per fixture in `testdata/http/golden/ast/*.ast`
- [x] P1.3.5 Fail-closed diagnostics: `SyntaxError{Path,Kind,Offset,Line,Col,Expected,Found}` rendered as `file:line:col`, `Parse` returns `(nil, err)` on any error and never a partial AST, `ParseDiag` accumulates multi-error diagnostics with resync at `###` (capped at 32); exit-code wiring still lands with `pulse test` (Goal 4)
- [x] P1.3.6 Benchmark gate: `BenchmarkParseCached` at `0 allocs/op` on a 10,000-request AST in a warmed arena (23.5 ms/op), enforced by `scripts/ci/test-gates.sh` in CI alongside `BenchmarkLexer` (G-alloc); the gate decides on `allocs/op` and allows ≤ 8 B/op of amortization, because `sync.Pool` is drained by the GC by design and one refill averages to a fraction of a byte
- [x] P1.3.7 `internal/ast` lexer: 32-byte `Token` (size-asserted), `iter.Seq[Token]` stream, `lexState` transition table `stLineStart → stRequestLine → stHeaders → stBody{JSON,XML,Raw} → stDone` (ROADMAP.md §1.1.1–1.1.2)
- [x] P1.3.8 Zero-allocation gate: `BenchmarkLexer` 0 B/op / 0 allocs/op on the generated 10k-line corpus (~296 MB/s), enforced by `scripts/ci/test-gates.sh` in CI (G-alloc)
- [x] P1.3.9 Termination + non-degenerate-span proof: `FuzzLexer` (22 seeds) asserts ≤ len(src)+1 tokens and no zero-width `TokIllegal`; `TestTokenSize` locks the 32-byte contract; `TestInvariants` checks bounds/monotonicity over the corpus
- [x] P1.3.10 `testdata/http/` corpus: 5 valid + 4 invalid fixtures with golden token files; regeneration via `go test ./internal/ast -run TestGoldenCorpus -update`
- [x] P1.3.11 Corpus-driven defect fixed: stale `wantHTTPVersion` state leaked across requests (fixture 05); fixed by consuming the flag exactly once, covered by goldens
- [x] P1.3.12 AST golden snapshots: `cmp.Diff` comparison of a rendered AST per valid fixture (`testdata/http/golden/ast/*.ast`), regenerated via `go test ./internal/ast -run TestGoldenAST -update-ast`
- [x] P1.3.13 Memory-ceiling gate: `TestMemoryCeiling` (TotalAlloc 1.50× source) and `TestMemoryCeilingRSS` (Linux `/proc/self/statm`, warmed arena: −0.05× source) hold a 10,000-request AST inside the 2× source budget
- [x] P1.3.14 Mutation evidence: `scripts/mutation-proof.sh` breaks two parsing boundaries (the mandatory `'='` in a variable definition; the `'# @assert'` directive) and shows red → restore → green on the `internal/ast` suite
- [x] P1.3.15 Parser fuzz target `FuzzParse` (10 seeds) asserts no panic and the fail-closed invariant (nil document exactly when the error is non-nil) on arbitrary input
- [x] P1.3.16 Corpus-driven defects fixed by the parser (all covered by goldens): (a) the lexer's central newline check leaked a spurious `TokNewline` inside JSON/XML bodies, corrupting every body-bearing fixture (fixed by restricting the check to line-oriented states); (b) a bodyless request swallowed the following `###`, so request-scoped `@var`/`# @assert` ranges anchored to the wrong request (fixed by one-token `unget`); (c) variable/assert scoping was anchored at request creation instead of at the block separator

### 1.4 Engine Core
- [x] P1.4.1 `internal/engine`: request execution over `net/http` with in-memory AST types — `engine.go` (AST→`*http.Request`, `Engine.Run(ctx, req, sink) RunHandle`, redacted `RunResult`)
- [x] P1.4.2 Request construction from AST (method, URL template, headers, body) with interpolation hooks left unbound (Phase 2 provides the resolver) — `Resolver` interface + `ErrUnresolved`; an unbound `{{var}}` **fails closed before any byte reaches the network** (`TestUnresolvedInterpolationFailsClosed`)
- [x] P1.4.3 Structured run results: status, timing, sizes, response SHA-256 (fed by the tee mirror) — `RunResult{Method,URL,Status,Proto,Header,Bytes,Digest,Elapsed}`, digest verified against an independent reference (`TestDigestMatchesReference`)
- [x] P1.4.4 Streaming pipeline: `io.TeeReader` → SHA-256 mirror + bounded `chan *Chunk` (cap 64) → dispatch goroutine → `Sink`; 32 KiB pooled chunks with one-way ownership (`stream.go`)
- [x] P1.4.5 Memory ceiling gate: `TestMemoryCeilingInvariant` proves the in-flight chunk count saturates at exactly 66 (2 162 688 B = 64×32 KiB + reader + dispatcher) for 1 MB/10 MB/100 MB payloads, and is invariant across a 10× payload change
- [x] P1.4.6 Zero-allocation gate: `BenchmarkStreamSteadyState` at `0 allocs/op` streaming 16 MiB (512 chunks/op) through the pooled pipeline (~1.4 GB/s), enforced by `scripts/ci/test-gates.sh`
- [x] P1.4.7 Leak gate: `TestCancellationNoLeak` cancels 8 runs mid-stream; asserts teardown ≤ 50 ms, goroutines 6→3, fds 11→9, and exactly one terminal callback per run
- [x] P1.4.8 Teardown uses `context.WithoutCancel` so a user interrupt cannot abort cleanup; the budget bounds the **abort** unwind only, never a successful drain (dropping delivered chunks would violate "no frame dropped")
- [x] P1.4.9 Redaction: `Authorization`/`Proxy-Authorization`/`Cookie`/`Set-Cookie`/`X-Api-Key` response headers and URL userinfo never leave the engine (`TestRedaction`, `TestRedactURLStripsUserinfo`)
- [x] P1.4.10 Mutation evidence: `scripts/mutation-proof-engine.sh` breaks the queue cap, the chunk size, and the pool recycle, showing red → restore → green

**Phase 1 exit criteria:** `go build ./...` clean with `CGO_ENABLED=0`; AST benchmarks at zero allocs; parse-failure tests prove fail-closed diagnostics with red→green evidence (AGENTS.md §4.2); store schema audited secret-free; Phase 1 completion logged in MEMORY.md with an ADL entry.

---

## Phase 2 — Secrets: OS Keychain, Resolution Chain, Redaction

**Goal:** Zero-trust secret management that touches no disk and never reaches the DB.

### 2.1 Keychain Backends (CGO-free)
- [x] P2.1.1 Linux: Secret Service over D-Bus (`github.com/godbus/dbus/v5`), service=`pulse` — `backend_linux.go` speaks `org.freedesktop.Secret.Service` (OpenSession/SearchItems/Item.GetSecret); no libsecret, no cgo
- [x] P2.1.2 macOS: shell-out to `/usr/bin/security find-generic-password` (no cgo) — `backend_darwin.go`; refuses a `security` binary outside `/usr/bin` (PATH-hijack guard)
- [x] P2.1.3 Windows: advapi32 `CredRead` via `golang.org/x/sys/windows` — `backend_windows.go`; blob decoded as UTF-16LE, odd-length blobs rejected rather than guessed
- [x] P2.1.4 Backend selection at runtime; unavailable backend ⇒ `BLOCKED (reason: ...)` per AGENTS.md §4.5 — `newPlatformBackend()` per build tag; `backend_other.go` reports BLOCKED on platforms with no binding
- [x] P2.1.5 All three backends compile CGO-free (verified by the 4-target build matrix); a locked keyring item reports BLOCKED rather than prompting or substituting a value

### 2.2 Resolver Chain & Ephemeral .env
- [x] P2.2.1 `Resolver` interface: keychain → ephemeral in-memory `.env` → hard error (exit 3) — `Chain.Resolve`, order proven by `TestChainKeychainFirst` / `TestChainFallsBackWhen*`
- [x] P2.2.2 `.env` parsing loads into memory only; no disk writes; cleared on teardown — `LoadEnv`/`LoadEnvFile` + `Chain.Clear()`
- [x] P2.2.3 Interpolation service: resolve `{{var}}` in URL/headers/body at execution time with source-located errors — engine `Resolver` hook bound to `Chain` (`Chain.Lookup`), errors carry `line N`
- [x] P2.2.4 `ErrUnresolved` maps to exit code 3 (`ExitCode`); every resolved value is registered with the redactor BEFORE it is returned, so no secret exists un-scrubbed

### 2.3 Redaction
- [x] P2.3.1 `secrets.Redact` chokepoint covering sensitive headers + all vault-sourced values — `Redactor` with two layers: name-based (10 header names) and value-based (every registered secret, longest-first)
- [x] P2.3.2 Wire redaction into every sink: stderr logging, history store rows, and (Phase 3) UI rendering — engine pipes response headers, response BODY chunks (in place, in the dispatch goroutine), and errors through it; store refuses rows carrying `{{...}}`
- [x] P2.3.3 Memory-bound tracking list (256 entries, longest-first); overflow DROPS rather than evicts (evicting could unprotect a secret still in flight) and counts the drop
- [x] P2.3.4 Outbound requests are NOT scrubbed — the real secret must reach the server; only diagnostic copies are (`TestOutboundSecretStillReachesTheServer`)

### 2.4 Honest Testing
- [x] P2.4.1 Unit tests with a fake backend for the resolver chain (order, fail-closed behavior) — 12 chain/redaction tests in `internal/secrets/redact_test.go`
- [x] P2.4.2 Integration tests per-platform keychain; environments lacking bindings report `BLOCKED (reason: ...)` — `TestPlatformBackendAvailability` SKIPs with `BLOCKED: secret-service-dbus backend unavailable...` on this WSL host (no Secret Service daemon)
- [x] P2.4.3 Schema audit reads live `sqlite_master` and rejects any credential-capable column, with a positive control proving the audit can fail; `secure_delete` verified behaviourally by dumping the raw file after a delete
- [x] P2.4.4 Mutation evidence: `scripts/mutation-proof-secrets.sh` breaks redaction (both layers), fail-closed resolution, the column rule, and `secure_delete` — red → restore → green

**Phase 2 exit criteria:** resolution chain proven fail-closed with red→green evidence; secret-scanner CI gate passes; redaction unit tests cover every sensitive header; platform keychain tests dispositioned honestly (PASSED or BLOCKED with reason).

**Phase 2 status: COMPLETE.** The secret-scanner gate is met by the runtime `sqlite_master` audit (`store.AuditSchema`, executed on every `Open`) plus the planted-secret dump test (`TestSecureDeleteZeroesDeletedRows`); no migration text contains a credential column name. Keychain dispositions: linux/amd64 **BLOCKED** — WSL host with a session bus but no Secret Service daemon, reason emitted by the test itself; darwin **NOT RUN** (no macOS host; backend cross-compiles CGO-free); windows **NOT RUN** (no Windows host; backend cross-compiles CGO-free).

**GOAL 1 STATUS: COMPLETE.** Phases 1 and 2 are done. Of the six Goal 1 exit gates (ROADMAP.md §1.4.1): G-alloc **met** (lexer, parser, and streaming steady state all 0 allocs/op); G-mut **met** (three mutation scripts, eleven broken boundaries, all red→green); G-secret **met** (live schema audit + planted-secret dump); G-honest **met** (keychain dispositions stated with reasons, none faked); G-doc **met** (ARCHITECTURE.md §4–§8 updated in the same commits; ADL-0012/0016/0017). **G-frame is NOT MET** — it belongs to the Bubble Tea UI (Phase 3) and cannot be measured before a UI exists; it is carried forward, not silently passed.

---

## Phase 3 — TUI: Bubble Tea Elm Loop, Design System, Zero-Blocking I/O

**Goal:** A 60-FPS terminal UI that renders a 50MB+ streaming response without
blocking the UI thread.

### 3.0 Streaming Transports & Telemetry (Move 5 — landed ahead of the UI)
- [x] P3.0.1 Unified `Stream` interface (`Read`/`Write`/`Close`/`SetDeadline`/`Kind`) with `StreamKind` HTTP|SSE|WS|GRPC; read-only transports return `ErrWriteUnsupported` rather than pretending to accept writes
- [x] P3.0.2 `StreamManager` lifecycle: `Idle → Connecting → Streaming ⇄ Connecting (reconnect) → Closed`, with `Attempts()` and `LastEventID()` observable and `Close()` idempotent
- [x] P3.0.3 SSE parser: resumable state machine over a bounded line buffer (1 MiB cap, fails closed with `ErrOversizeLine`); `Feed` returns `iter.Seq[Event]` from a pre-bound closure, comments counted as heartbeats, id/name interned so a steady flood allocates nothing
- [x] P3.0.4 Reconnect + resume: `Last-Event-ID` retained on the **manager** (not the per-connection parser) and replayed on every attempt; full-jitter backoff 500 ms → 30 s; idle expiry maps to the documented retryable `ErrIdle`
- [x] P3.0.5 Frame-aligned dispatch: events are coalesced and pushed to the Sink at most once per 16 ms frame through the 32 KiB pooled-chunk channel, so an event flood cannot lock the UI
- [x] P3.0.6 Per-read deadline renewal on the live connection (captured via `httptrace.GotConn`), renewed on every read and on every heartbeat; a silent server surfaces as `ErrIdle` instead of wedging
- [x] P3.0.7 `TelemetryReport`: typed httptrace phases (DNS/connect/TLS/wrote/first-byte/got-conn) with monotonic stamps, bounded 512-point ring, lock-guarded `Snapshot()` for concurrent readers
- [x] P3.0.8 eBPF architecture behind strict build tags; absent privileges or a default build report `BLOCKED`/`not-built` **with a reason** and the request proceeds on httptrace timings alone — never a silent fallback
- [x] P3.0.9 Reconnect gate (`TestReconnectReplaysLastEventID`): connection dropped mid-stream, client reconnects and the server observes `Last-Event-ID: evt-2`
- [x] P3.0.10 Mutation evidence: `scripts/mutation-proof-stream.sh` breaks resume-token retention, idle retryability, frame coalescing, and telemetry recording — red → restore → green

### 3.0b Binary Protocols: gRPC & Dynamic Protobuf Reflection (Move 6 — Goal 2 close)

- [x] P3.0b.1 `ProtocolEngine` interface (`Protocol`/`Run`/`Close`) satisfied by both executors, with compile-time assertions; `ProtocolRouter` dispatches by TARGET SCHEME (`grpc://`, `grpcs://`), so no UI or MCP layer can learn the wire
- [x] P3.0b.2 gRPC executor accepting the SAME `*ast.Request` and delivering to the SAME `Sink` as REST — one run pipeline, two wires
- [x] P3.0b.3 Dynamic schema resolution over `grpc.reflection.v1.ServerReflection` with NO generated protobuf types anywhere in the binary: the reflection client's own descriptor is hand-constructed, field-for-field, against the v1 schema
- [x] P3.0b.4 `DescriptorCache` keyed by (target, symbol) with strict LRU eviction and cross-target namespacing; missing imports are reported by name instead of surfacing as an unrelated "unknown type" error later
- [x] P3.0b.5 Offline fallback: descriptors supplied locally resolve a method whose server refuses reflection, with no network fetch (Pulse is offline-first)
- [x] P3.0b.6 JSON→binary mapping with a structured `*FieldTypeError` carrying the field path (`filter.pageSize`, `tags[2].pageSize`) and the expected kind; strict unknown-field rejection by default
- [x] P3.0b.7 protojson's 64-bit-as-string rule honoured exactly: a quoted number is legal for int64/uint64/fixed64/sint64 and rejected for 32-bit kinds, with an error that explains why
- [x] P3.0b.8 STREAMING binary→JSON encoder: working set stays at one 32 KiB chunk regardless of message size, so the 2 MiB in-flight ceiling from ADL-0016 actually binds; well-known types delegate to protojson so their encodings stay exact
- [x] P3.0b.9 Method-shape guard: client-streaming and bidi return `ErrUnsupportedMethod` BEFORE dialing — no silent degradation; non-2xx/unimplemented surfaces as `*RPCStatusError` carrying both the gRPC code and the mapped numeric status
- [x] P3.0b.10 Reflection gate (`TestReflectionGateResolvesAndRendersJSON`) against a local `grpc.NewServer` with `reflection.Register` whose schema is built entirely at runtime — JSON in, correctly-rendered JSON out
- [x] P3.0b.11 Reflection-descriptor gate (`TestReflectionDescriptorMatchesTheWireProtocol`) pinning every field number, written after v1alpha's numbering shipped by mistake under the v1 package name
- [x] P3.0b.12 Mutation evidence: `scripts/mutation-proof-grpc.sh` breaks type coercion, the reflection oneof, encoder streaming, cache eviction, and the method-shape guard — red → restore → green

**GOAL 2 STATUS: COMPLETE.** ROADMAP.md §2.1 (SSE + stream lifecycle; WebSocket §2.1.4 remains deferred), §2.2 (gRPC with dynamic reflection) and §2.3 (telemetry with explicit eBPF degradation) are implemented and gated. Every gate has red→green evidence. **G-frame is still NOT MET** — carried forward from Goal 1 to Phase 3.1, because the frame budget is a property of the Bubble Tea render loop, which does not exist yet; it is stated as unmet rather than quietly counted as passed.

### 3.1 UI Skeleton & Design System
- [ ] P3.1.1 `internal/ui`: Elm skeleton (Model/Msg/Update/View) with central keymap struct (DESIGN_SYSTEM.md §5)
- [ ] P3.1.2 Layout engine: header/tree/main/status/footer at all breakpoints (≥120, 100–119, 80–99, <80 refuse)
- [ ] P3.1.3 Theme tokens + degradation ladder (truecolor → 256 → 16 → mono) with golden snapshot tests per profile
- [ ] P3.1.4 Glyph modes: Nerd Fonts default, ASCII fallback; golden tests for both
- [ ] P3.1.5 Virtualized tree and response viewports (10k rows at O(visible) memory)

### 3.2 Interaction
- [ ] P3.2.1 Vim motions: `j/k/g/G/Ctrl+d/u/f/b`, `/` search with `n/N`, `Ctrl+P` fuzzy finder
- [ ] P3.2.2 Huh forms: new request, environment switch, confirmations
- [ ] P3.2.3 Help overlay (`?`) rendered from the central keymap
- [ ] P3.2.4 Request editor: `Ctrl+S` send, `Ctrl+X` cancel, insert mode with key passthrough

### 3.3 Zero-Blocking I/O Multiplexing
- [ ] P3.3.1 Streaming pipeline: reader goroutine → `io.TeeReader` mirror (SHA-256) → `chunks chan *Chunk` (cap 64) → UI (`ARCHITECTURE.md` §5.1)
- [ ] P3.3.2 `sync.Pool` chunk buffers (≤32KiB), recycle path via `RecycleMsg`
- [ ] P3.3.3 Frame budget enforcement: one `View` per 16ms tick, stale frame drop
- [ ] P3.3.4 Cancellation: `Ctrl+C`/`Ctrl+X` unwind within 50ms via context cancellation + `context.WithoutCancel` teardown
- [ ] P3.3.5 NDJSON/JSON-lines incremental rendering; pretty-print tail-first for monolithic bodies
- [ ] P3.3.6 Streaming benchmark: 0 allocs/op steady state, p99 frame < 16ms under a 50MB payload; `testing/synctest` audit proving no I/O on the UI thread

**Phase 3 exit criteria:** golden snapshots pass at every breakpoint/profile/glyph mode; streaming benchmarks meet NFR-2/NFR-5/NFR-9; cancellation test green; DESIGN_SYSTEM.md unchanged-or-updated with ADL entry.

---

## Phase 4 — Dual-Mode Boot, Transport Segregation, MCP Server

**Goal:** Immutable dual-mode runtime: `isatty` routing to the TUI and `pulse mcp`
routing to a spec-conformant JSON-RPC stdio server via `mark3labs/mcp-go`.

### 4.1 Mode Routing
- [x] P4.1.1 Mode precedence per ARCHITECTURE.md §2.1: `pulse mcp`/`--mcp` → MCP; bare + isatty(stdout) → TUI; bare + non-TTY → stderr message, exit 2 — gated by `TestRunModeMatrix` (flag × isatty matrix, asserting mode *and* exit code) and `TestMCPBootLeavesStdoutEmpty`
- [x] P4.1.2 isatty detection before any stdout-capable initialization — `main()` evaluates `term.IsTerminal(os.Stdout.Fd())` and passes it in; `run()` decides mode from the passed-in value and never probes the terminal itself
- [ ] P4.1.3 Mode immutability: no runtime mode switching; architecture test asserts `ui` and `mcp` are never both wired — **BLOCKED on Phase 3**, not on effort: `internal/ui` does not exist, so there is nothing to assert against. Mode is already immutable by construction (`run` returns once; there is no dispatch loop), but the *architecture test* cannot be written until the UI package lands. Left unchecked on purpose.

### 4.2 stdio Transport Segregation
- [x] P4.2.1 stdout sentinel: stdout reserved for JSON-RPC in MCP mode; all diagnostics to stderr, prefixed `pulse:` — two layers (MEMORY.md ADL-0020). Production swaps `os.Stdout` to `/dev/null`; `PULSE_STDOUT_AUDIT=1` swaps it to a frame-judging `Tripwire` instead. `mcp.Lock()` is the first call in the MCP path, before the package is used; `ServeLocked(ctx, lock.Real, os.Stdin)` is the only holder of the real channel. Clean stdin EOF and `context.Canceled` both exit 0.
- [x] P4.2.2 Stdout-hygiene CI analyzer: no `fmt.Print*`/`os.Stdout` references under `internal/mcp` and `internal/app` MCP paths — `importaudit` check 3 passes, with two narrow exemptions recorded in ADL-0020: the guard file itself (`internal/mcp/stdio.go` — it *is* the boundary) and `_test.go` files (a test proving isolation must be able to swap `os.Stdout` for a pipe and observe it; test files are not linked into the shipped binary). Neither exemption covers any file a stray print could escape from in a release build.
- [x] P4.2.3 Wire-protocol corruption regression test: stray stdout byte ⇒ build/test failure — `TestMCPDiagnosticsNeverReachStdout` runs a live session against `os.Stdout` itself via an `os.Pipe` swap and asserts every byte on the channel is a valid JSON-RPC frame; `TestStdioLoopEmitsOnlyJSONRPC` does the same over a real pipe-held stdio session; `TestStdoutGuardTripsOnRoguePrint` plants a rogue print and asserts the tripwire rejects it. Mutation M19 proves the gate is real: bypassing the `Logf` sink turns it red.

### 4.3 MCP Server (mark3labs/mcp-go v1.2.1)
- [x] P4.3.1 Server over stdio; spec lifecycle: `initialize` → `initialized` → ops → `shutdown` → exit 0 — gated by `TestProtocolGateHonoursTheHandshake` (initialize/initialized/tools-list round trip) and `TestProtocolGateParsesExecutesAndReturns` (raw frame in, structured frame out).
- [ ] P4.3.2 Tools: `list-collections`, `list-requests`, `get-request`, `run-request`, `get-run`, `get-history` (per ARCHITECTURE.md §9) — **1 of 6 built.** `execute_request` is complete and gated (raw `payload` XOR `path`, plus `index`/`env`/`timeout_ms`/`body_mode`/`include_body`; wired to `internal/ast` parse + `internal/engine.Run`). The five collection/history tools need `internal/store` queries plus an `internal/app` composition root that does not exist yet. The name is `execute_request`, not `run-request`: snake_case per MCP convention — deviation recorded in MEMORY.md ADL-0020.
- [ ] P4.3.3 Every tool result passes through `secrets.Redact` before transmission — **chokepoint built, composition root missing.** The single no-bypass chokepoint is `collectingSink.outcome`, immediately before an outcome becomes a tool result, and it is gated by `TestRedactionChokepointRunsOnResults`. **But `DefaultExecutor` is constructed with a `nil` `secrets.Redactor`** because `internal/app` does not exist yet, so in MCP mode today redaction is by header name only: a vault-sourced value echoed back inside a response *body* is not yet scrubbed. Deliberately NOT ticked. Next action: build `internal/app` as the composition root that wires a concrete `secrets.Redactor` into `DefaultExecutor`, and add a gate asserting a vault value does not survive the round trip in the body.
- [ ] P4.3.4 `run-request` streams via the Phase 3 engine; MCP client receives chunked summaries with redaction applied, never raw secrets — **partially built, deliberately not ticked.** `execute_request` does drive `engine.Run` through the streaming pipeline and collects the result, but it returns a *bounded summary* (`MaxResponseBytes` = 256 KiB, with explicit `truncated: true` and true `body_bytes`), not a chunked progress stream to the client. Chunked delivery to an MCP client is a spec-level feature (progress notifications / partial results) that Phase 3's `sync.Pool` recycle path must exist first, because a client-driven stream outlives a single tool call. Redaction is subject to P4.3.3's open gap, so "never raw secrets" is not yet provable for body content.
- [x] P4.3.5 Error transport: JSON-RPC error objects for parse failures (exit 2 semantics), secret failures (exit 3 semantics), timeouts — standard codes `-32700/-32600/-32601/-32602/-32603` plus a reserved domain block `-32002` parse, `-32003` secret, `-32004` timeout, `-32005` transport, `-32006` unsupported, each with a stable `category` string. `payload()` includes the cause, so a parse failure surfaces the parser's `:1:1:`. An unresolved secret fails closed to `-32003` naming the variable — never a placeholder, never an empty string. Gates: `TestParseFailureCarriesItsOwnCode`, `TestUnresolvedSecretFailsClosed`, `TestInvalidArgumentsAreStructuredErrors`; mutations M20/M24.
- [x] P4.3.6 Mutation evidence for the MCP bridge: `scripts/mutation-proof-mcp.sh` (M19–M24) — all six boundaries red→restore→green. Bumped out of P4.4.3 into its own item here because the bridge is where the protocol lives and the evidence belongs next to it rather than in a release-time audit.

### 4.4 Hardening & Release
- [ ] P4.4.1 Cross-platform build matrix green: `CGO_ENABLED=0` on linux/amd64+arm64, darwin/arm64, windows/amd64
- [ ] P4.4.2 Offline audit: strace/network smoke test proves no egress at runtime
- [ ] P4.4.3 Mutation-evidence audit across all bug-fix commits (AGENTS.md §4.2)
- [ ] P4.4.4 Documentation-sync gate green: every code commit carries TASKS.md/MEMORY.md changes (AGENTS.md §4.6)
- [ ] P4.4.5 v1.0.0 release criteria per PRD.md §9 fully satisfied and evidenced in MEMORY.md

**Phase 4 exit criteria:** both modes run from one binary; stdout hygiene provably clean; MCP lifecycle conforms to the spec (verified against a conformant client); release checklist complete with honest test dispositions throughout.

---

## Progress Log (append-only; newest at bottom)

| Date | Phase/Task | Disposition | Evidence |
|---|---|---|---|
| 2026-10-11 | Scaffolding: 6 Essential Vibe Coding Files created | DONE | Initial commit (this file) |
| 2026-10-11 | ROADMAP.md authored (4 goals, X.X.X); doc surface expanded to 7 files (ADL-0008/0009) | DONE | Companion commit to roadmap |
| 2026-10-11 | Move 0: bootstrap + enforcement harness (ADL-0010) | DONE | 4/4 CGO-free matrix; tidy no-op; audit; mutation + planted-import red→green proofs |
| 2026-10-11 | Move 1: .http lexer (ADL-0011) | DONE | 32-byte Token asserted; 0 B/op bench gate; fuzz termination; 9-fixture golden corpus; race detector green |
| 2026-10-12 | Move 7: the MCP bridge — stdio isolation + `execute_request` (ADL-0020) | PARTIAL — see open items | 24 gates green in `internal/mcp`+`cmd/pulse`; `-race` clean; M19–M24 all red→restore→green; `importaudit` G-hyg PASS; 4/4 CGO-free matrix. **Not claimed done:** 5 of 6 tools (P4.3.2), the `secrets.Redactor` composition root (P4.3.3), chunked client delivery (P4.3.4), and the mode-immutability architecture test (P4.1.3, blocked on Phase 3). |
| 2026-10-12 | Move 7 collateral: fixed a pre-existing flake in `TestReconnectReplaysLastEventID` (Move 5, SSE) | DONE | The test closed the stream *before* waiting for the resumed payload; `Close()` drops a buffered frame rather than dispatching it. 8/8 clean runs after the reorder. Found by a full-suite run, not by inspection. |
