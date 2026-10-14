# ARCHITECTURE.md — Pulse Technical Architecture

| Field | Value |
|---|---|
| Document | System Architecture |
| Product | Pulse |
| Version | 1.0.0-draft |
| Status | Authoritative |
| Canonical upstream | `https://github.com/duckiec/pulse.git` (branch `main`) — the database of record for Pulse's history |
| License | AGPL-3.0 (root `LICENSE`, from the upstream initial commit) |
| Governed by | `AGENTS.md` |
| Extended by | `ROADMAP.md` (Goals 2–4 introduce SSE/WebSocket/gRPC transports, eBPF telemetry, local RAG, fuzzing, wazero sandboxing, CRDT sync, and vault PKI; contracts herein reflect those goals) |
| Last updated | 2026-10-12 |

---

## 1. Architectural Overview

Pulse is a single CGO-free Go binary with two immutable execution modes sharing one
domain engine:

```
                       ┌──────────────────────────────────────────────┐
                       │                 cmd/pulse                    │
                       │  flag parsing, mode selection, process wiring │
                       └───────────────┬───────────────┬──────────────┘
                                       │               │
                        isatty(stdout) │               │ `pulse mcp` / `--mcp`
                                       ▼               ▼
                        ┌────────────────────┐  ┌────────────────────┐
                        │   MODE A: TUI      │  │  MODE B: MCP       │
                        │   Bubble Tea       │  │  JSON-RPC/stdio    │
                        │   Elm loop, 60 FPS │  │  mark3labs/mcp-go  │
                        └─────────┬──────────┘  └─────────┬──────────┘
                                  │      stdin/stdout    │
                                  │      reserved        │
                                  ▼                       ▼
                        ┌──────────────────────────────────────────────┐
                        │              internal/app                    │
                        │   shared services: request execution,       │
                        │   history, secret resolution, collections    │
                        └───────────────┬───────────────┬──────────────┘
                                        │               │
                    ┌───────────────────┼───────────────┼───────────────────┐
                    ▼                   ▼               ▼                   ▼
            ┌──────────────┐   ┌──────────────┐  ┌─────────────┐   ┌─────────────┐
            │ internal/ast │   │internal/engine│ │internal/store│  │internal/    │
            │ .http lexer, │   │ transport,   │  │ modernc.org/ │  │secrets      │
            │ parser, AST  │   │ TeeReader    │  │ sqlite schema│  │ keychain,   │
            │ (iter.Seq)   │   │ streaming    │  │ zero secrets │  │ redaction   │
            └──────────────┘   └──────────────┘  └─────────────┘   └─────────────┘
```

## 2. Dual-Mode Routing Logic

> **Move 10 status.** `pulse mcp` builds the full composition root
> (`internal/app`) and serves `execute_request` + `execute_fuzz_run` over
> stdio. `pulse` with no arguments still routes to the TUI, which **does not
> exist** and exits 1 with a diagnostic; that half of the matrix is unchanged.

Mode is selected once at process start and is **immutable for the process lifetime**.
The `internal/ui` and `internal/mcp` packages are never linked into a simultaneous
runtime; they are the only two consumers of `internal/app`.

### 2.1 Precedence (highest first)

| Order | Condition | Result |
|---|---|---|
| 1 | Subcommand `pulse mcp` (or `pulse --mcp`) | **MCP mode** — forced, no isatty heuristic |
| 2 | Bare `pulse` with `isatty(stdout) == true` | **TUI mode** |
| 3 | Bare `pulse` with `isatty(stdout) == false` | **Fail closed** — stderr message, exit code 2. Never attempt TUI against a pipe; never treat a non-TTY as an implicit MCP invitation |

### 2.2 isatty detection

- Use `golang.org/x/term.IsTerminal(int(os.Stdout.Fd()))` (pure Go, syscall-level).
- Detection runs before any subsystem initialization that could write to stdout.
- MCP mode registers a **stdout sentinel**: the process installs a guard that fails
  the build (and panics in debug builds) if any code path writes to `os.Stdout`
  outside the JSON-RPC layer. All diagnostics go to stderr, prefixed `pulse:`.

### 2.3 Mode isolation contract

| Concern | TUI mode | MCP mode |
|---|---|---|
| stdout | Rendered frames | Reserved JSON-RPC bytes — nothing else |
| stdin | Key events | JSON-RPC frames |
| Diagnostics | Status bar (never stderr noise) | stderr, prefixed, minimal |
| Signal handling | `q`/Ctrl+C graceful shutdown | JSON-RPC `shutdown` → exit 0 |
| Secret resolution | Keychain/ephemeral .env | Keychain/ephemeral .env (identical chain) |

## 3. Package Map & Boundaries

| Package | Responsibility | Depends on | Never imports |
|---|---|---|---|
| `cmd/pulse` | Flags, mode selection, wiring, exit codes | `app`, `ui`, `mcp`, `golang.org/x/term` | — (leaf wiring only) |
| `internal/ui` | Bubble Tea models, Lip Gloss styling, central keymap, telemetry strip (Move 14). Holds executors only as `engine.ProtocolEngine`; no Huh forms yet | `app`, `ast`, `engine` + Charm stack (§7) | `mcp`, `store` |
| `internal/mcp` | JSON-RPC lifecycle, tool registry, stdio transport. **Constructs no dependencies** (Move 10) — everything is injected | `ast`, `engine`, `fuzz` | `ui`, `secrets`, `store` |
| `internal/wasm` | Untrusted-code sandbox: zero-capability wazero runtime, memory ABI, time + call budgets | `github.com/tetratelabs/wazero` (interpreter, no cgo) | everything internal; no host modules, so no sibling can be imported by a guest either |
| `internal/rag` | Codebase indexing: bounded walker, per-language chunking, structural embedder | stdlib only (`go/parser`) | everything internal; **`store` and `mcp` both depend on this layer's OUTPUT, not on it** |
| `internal/fuzz` | OpenAPI subset reader (JSON + hand-rolled YAML), deterministic boundary matrix, GCRA rate limiter, TAP 13 emitter | stdlib only (`encoding/json`, `regexp`, `math/rand` seed) | everything internal; it builds payloads and never sends one |
| `internal/app` | **Composition root** (Move 10): the only package that names a concrete dependency for another package. Builds store, secrets chain + redactor, embedder, retriever, binder, executor, fuzz runner; owns lifecycle and context | `ast`, `engine`, `fuzz`, `mcp`, `rag`, `secrets`, `store` | `ui` |
| `internal/engine` | `ProtocolEngine` executors (REST, SSE, gRPC), streaming, checksums, telemetry | `ast` (types) | `ui`, `mcp`, `store` |
| `internal/ast` | Lexer, parser, zero-allocation AST, `iter.Seq` traversal | stdlib only | everything internal |
| `internal/store` | SQLite schema, migrations, history persistence | stdlib + `modernc.org/sqlite` | everything internal |
| `internal/secrets` | Keychain backends, ephemeral env, resolver chain, redaction | stdlib + sanctioned backends (§7) | everything internal |
| `internal/devtools/importaudit` | Static, offline dependency + stdout-hygiene audit (dev tooling) | stdlib only | everything internal; never imported by shipped code |

**Move 11 added one dependency: `github.com/tetratelabs/wazero v1.12.0`.** It is the
WebAssembly runtime for the auth/signing sandbox (§15), pure Go, and compiled in
interpreter mode — no JIT, no cgo, so it cross-compiles CGO-free to every target in the
build matrix. There is no stdlib WebAssembly runtime, and the feature is a hard
requirement of Goal 3: an untrusted signing script must be confined, and confinement
means running it where it cannot reach the host. It adds no indirect dependency.
**wazero v1.12 exposes no fuel API and its `FunctionListener` is bound at
CompileModule rather than Call time — a measurement recorded in
`config/allowed-modules.txt` and MEMORY.md ADL-0024, because the sandbox's security
posture depends on what the runtime actually enforces.**

**Move 14 added three dependencies: `github.com/charmbracelet/bubbletea
v1.3.10`, `github.com/charmbracelet/lipgloss v1.1.0`,
`github.com/charmbracelet/bubbles v1.0.0`.** They are the AGENTS.md §2.3 UI
engine (Elm loop, styling, viewport) plus 17 transitive pure-Go modules
(terminal I/O, unicode widths, color profiles), each named in
`config/allowed-modules.txt`. `huh` is deliberately absent: forms belong to
a future move. All three are CGO-free on every build-matrix target (verified
by source scan for `import "C"` and by the 4-target matrix itself).

**The composition root is the only place a concrete dependency is chosen.**
Before Move 10, `internal/mcp` constructed its own executor inside
`DefaultExecutor`, which is why its `secrets.Redactor` was nil and why three
separate moves recorded the same blocker without being able to fix it. The
dependency arrows now point the other way: every package declares what it needs
as an interface, and `internal/app` supplies it.

**Move 10 added no dependency either.** The cost of the composition root was one
sibling import REMOVED: `internal/mcp` declared `mcp.Redactor` (a one-method
interface) and stopped importing `internal/secrets`.

**Move 9 also added no dependency.** ROADMAP.md §3.3.1 names
`github.com/getkin/kin-openapi` and §3.3 needs YAML, but both `gopkg.in/yaml.v2`
and `gopkg.in/yaml.v3` are on the permanently-banned list, so the schema reader
is purpose-built over `encoding/json` plus a subset YAML parser (§13). The
`kin-openapi` row in §7 is a **forward contract that was not exercised**: the
row remains as the admission point should a future move need full OpenAPI
coverage, and CI does not permit the import today.

**Move 8 added no dependency at all.** Local RAG (`internal/rag`) is stdlib-only:
`go/parser` for Go chunking, `embed`-free hashing for the structural vector, and
FTS5 + brute-force cosine in `internal/store` on the existing `modernc.org/sqlite`
driver. There is no vector extension (all are C libraries, barred by §2.4) and no
ONNX runtime is vendored (ROADMAP.md §3.2.2's model artifact cannot be fetched at
runtime, invariant I-3). The Embedder interface is the seam a real model drops into.

Supporting directories (not Go packages): `config/allowed-modules.txt` (sanctioned
dependency allowlist), `scripts/` (`bootstrap-toolchain.sh`, `ci/*` gate scripts,
`all-gates.sh`), `testdata/http/` (lexer/parser corpus + golden files),
`.github/workflows/ci.yml` (GitHub Actions).

Dependency flow is one-directional; cycles are a build-time error and a review-time
rejection.

## 4. .http AST Parser (`internal/ast`)

### 4.1 Pipeline

```
[]byte (.http file)
   │  lexer: tokens over the raw buffer (no per-token allocation; offsets, not copies)
   ▼
Token stream  ◄── IMPLEMENTED (Move 1): 32-byte Token, iter.Seq[Token], lexState table
   │  parser: recursive descent, arena-allocated nodes
   ▼
Document AST  ──iter.Seq──▶  consumers (engine tee-off, UI tree, MCP tool listing)
```

**Implementation status:** lexer **and** parser are complete and gated. Lexer:
0 allocs/op on a generated 10k-line corpus, termination proven by fuzz seeds,
golden corpus at `testdata/http/golden/`. Parser: `BenchmarkParseCached`
(10,000-request AST, warmed arena) at **0 B/op / 0 allocs/op**, memory ceiling
**1.50× source**, golden AST snapshots at `testdata/http/golden/ast/`. The token
contract and state machine are recorded in MEMORY.md ADL-0011; the arena
allocation strategy and the fail-closed parse contract in ADL-0012.

**Files:** `lexer.go` (tokenizer, `Tokens()`/`NextToken()`/`Push()`),
`parser.go` (AST nodes, `Parse`/`ParseDiag`/`ParseFile`), `arena.go`
(`Arena`, `AcquireArena`, `allocSlice`).

**Parser shape — two passes over one token stream.** A counting pre-pass
(`countNodes`) sizes every arena array exactly, then an LL(1) fill pass builds
them. The pre-pass deliberately drives `Lexer.NextToken()` rather than
`Tokens()`: it must see the same tokens the fill pass sees, including those
after an illegal token, or the counts would disagree and the fill pass would
write past its arrays. Lookahead is exactly one token in frame; a request that
ends on a token belonging to the document level (the following `###`) hands that
token back with a one-token `unget` (`Lexer.Push`) instead of swallowing it.

**Scoping.** Variables and asserts are scoped by **block separator**, not by
request creation: everything declared before the first `###` (or, failing that,
before the first request) is file-scoped (`Document.Vars`); everything after a
separator belongs to the next request in that block (`Document.VarsOf` /
`Document.AssertsOf`).

### 4.2 Grammar (v1)

```
document  := request* 
request   := separator? headerline* body
separator := '###' line
headerline:= ('#' comment | '@' name '=' value | method URL [HTTP/1.1]) line
method    := GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS
body      := ('{' ... '}' | '<' ... '>' | raw-lines) until EOF|'###'
variable  := '{{' name '}}' interpolation, resolved at execution time (never parse time)
```

### 4.3 Zero-allocation guarantees

- Lexer/parser operate on the original `[]byte`; string names are views into the
  buffer (no `string()` conversions in hot paths).
- The AST is **offset-based**: every string is an `int32` span into an
  arena-owned copy of the source, materialized on demand by `Document.Str`
  (`unsafe.String`). A `string`-per-field AST does not fit the 2× ceiling.
- Node allocation uses a bump arena owned by the `Document`, freed in one
  release. Allocations are 8-byte aligned; a single large node array gets an
  exact-fit block. `Arena.Release` clears the parse state but **retains** the
  source buffer and node blocks, so a warm cycle allocates nothing.
- **The root `Document` is pooled on the Go heap, never arena-allocated.** The GC
  does not trace pointers stored inside arena blocks, so the one node holding Go
  pointers must stay outside the arena — otherwise the arena's own lifetime is
  invisible to the collector.
- Benchmark gates (CI, `scripts/ci/test-gates.sh`): `BenchmarkLexer` and
  `BenchmarkParseCached` each report `0 allocs/op`. The gate asserts on
  `allocs/op`, not on an averaged `B/op`: the GC drains `sync.Pool` by design,
  so an occasional pool refill averages to a fraction of a byte and must not be
  read as per-operation allocation.
- Traversal is lazy: `func (d *Document) Requests() iter.Seq[*Request]` — consumers
  (UI virtualized list, MCP listing, streaming execution) pull one request at a
  time; a 10,000-request collection costs O(1) memory.

### 4.4 Fail-closed parsing

Any lex/parse error halts immediately with a structured diagnostic:
`file:line:col: expected <token>, found <lexeme>` — exit code 2 in headless/MCP
contexts, surfaced in the TUI status bar in interactive mode. Partial ASTs are never
returned on error.

The contract is split in two: `Parse` returns `(nil, err)` on the **first**
error and never a partially filled AST; `ParseDiag` keeps going, resyncing at
`###` and accumulating up to 32 `*SyntaxError`s for the editor/TUI path — but it
still returns no document, because a file with any diagnostic has no valid AST.
A violated *parser* invariant (a count/fill disagreement) is reported as an
internal error with no file position, never as a user-facing diagnostic.

**Implementation status:** implemented and gated (Move 3). Streaming gate:
`BenchmarkStreamSteadyState` at 0 allocs/op; memory ceiling proven invariant at
66 in-flight chunks across 1 MB/10 MB/100 MB payloads; leak gate shows
goroutines and fds returning to baseline after 8 cancelled runs. See MEMORY.md
ADL-0016.

**Persistent transports (Move 5).** `Stream` unifies HTTP/SSE/WS/gRPC behind
`Read`/`Write`/`Close`/`SetDeadline`/`Kind`; read-only transports return
`ErrWriteUnsupported` rather than faking a write. `StreamManager` owns the
lifecycle — `Idle → Connecting → Streaming ⇄ Connecting (reconnect) → Closed` —
and exposes `Attempts()` and `LastEventID()`.

The **resume token is owned by the manager, not the parser**: the parser is
rebuilt per connection, the token must not be. Reconnect replays
`Last-Event-ID` with full-jitter backoff (500 ms → 30 s). An idle expiry maps to
the documented retryable `ErrIdle`; oversize framing and a non-2xx status are
terminal. Events are coalesced and dispatched at most once per 16 ms frame
through the same 32 KiB pooled channel, so the 2 MiB ceiling is unchanged and an
event flood cannot lock the UI. See MEMORY.md ADL-0018.

**Binary protocols (Move 6).** `ProtocolEngine` is the execution contract every
executor satisfies: `Protocol()`, `Run(ctx, *ast.Request, Sink) RunHandle`,
`Close()`. Both the REST and gRPC executors take the **same AST node** and
deliver to the **same Sink**, so a `.http` line targeting a REST endpoint and one
targeting a gRPC method are driven by identical UI and MCP code. Compile-time
assertions pin both executors to the interface; `ProtocolRouter` selects by
**target scheme** (`grpc://`, `grpcs://`), never by anything a user chose, which
is what makes the dual-mode layers genuinely protocol-agnostic. A REST-only
build is valid and reports `ErrUnsupportedMethod` rather than handing a `grpc://`
string to an HTTP parser.

**Schemas are resolved at runtime and nothing about them is compiled in.** The
gRPC executor fetches a method descriptor over
`grpc.reflection.v1.ServerReflection` and builds every message with `dynamicpb`
from the returned descriptor. Pulse's own reflection client has **no generated
protobuf type either**: its descriptor is constructed field by field in
`reflection_wire.go`, which is the only way to call the reflection service without
importing grpc's generated client. `DescriptorCache` is keyed by
(target, symbol) with strict LRU — the target is part of the key because the
same symbol means different things on different servers, and a cross-server cache
hit would be a silent correctness bug. A server that refuses reflection falls
back to locally supplied descriptors; with neither source, resolution fails
closed as `ErrNoDescriptors`. No schema is ever fetched from anywhere but the
server the user addressed.

**The binary→JSON direction streams.** Marshalling a whole message and chopping
it into chunks would measure nothing: the 2 MiB ceiling would bind on the channel
while the real allocation happened a moment earlier and unmeasured. The encoder
writes through a fixed 32 KiB scratch buffer, so working memory is independent of
message size. Byte-equality with `protojson` is explicitly not a contract —
protojson's whitespace is unstable by design — so comparisons are on decoded
values; well-known types (`Timestamp`, `Duration`, `Struct`, wrappers) delegate
to protojson so their bespoke encodings stay exact. See MEMORY.md ADL-0019.

**Exact geometry (MEMORY.md ADL-0016).** `chunkSize = 32 KiB`,
`chunkChannelCap = 64`, so the queue holds **2 097 152 B (2 MiB)**. A chunk is
live from checkout until the consumer recycles it, so the true worst case is
64 queued + 1 being filled + 1 held by the dispatcher = **66 chunks =
2 162 688 B = 2.0625 MiB** (`inFlightBytes`). That figure — not a flat "2 MiB" —
is what the tests assert, and it is independent of payload size.

**Sink contract.** `Run(ctx, req, sink) RunHandle` returns no error: success,
transport failure, and cancellation all arrive through the sink, so there is one
place to observe results. `Sink.Chunk` is called from the dispatch goroutine and
**transfers ownership**: a sink that keeps the bytes must copy them, or call
`Chunk.Recycle()`. `Recycle` is idempotent (a double recycle is counted and
ignored rather than corrupting the pool). Exactly one terminal callback per run
(`Done` **or** `Failed`), and never a chunk after it.

**Teardown budget bounds aborts, not success.** Cleanup runs on
`context.WithoutCancel(ctx)` so a user interrupt cannot abort it. On abort the
dispatcher recycles instead of delivering, and the 50 ms budget applies. On
success the engine waits for the drain with no budget — capping it would mean
discarding chunks already promised to the sink, violating "no frame dropped".

**Interpolation fails closed.** With no `Resolver` bound, a `{{var}}` aborts the
run before the request is issued (`ErrUnresolved`); a resolver error is fatal.
A literal `{{var}}` is never sent to a real endpoint.

## 5. Zero-Blocking I/O Model (`internal/engine`)

The required behavior: stream a **50MB+ JSON response** without dropping a single
frame, with bounded memory.

### 5.1 The mandated pattern — `io.TeeReader` + channels + `sync.Pool`

```
http.Response.Body (net.Conn-backed reader)
   │
   ├─▶ io.TeeReader mirror ──▶ checksum hasher (sha256) │ grows per chunk, O(1) mem
   │
   ▼
reader goroutine (owns the socket read loop — NOT the UI thread)
   │   buffered channel: chunks chan *Chunk, cap = 64
   ▼
◄── *Chunk { Buf *[]byte (pooled, ≤32KiB), N int, Final bool } ──►
UI goroutine                       MCP handler (streams summary + cites offsets)
   │
   └─▶ returns *Chunk.Buf to sync.Pool after render (RecycleMsg)
```

### 5.2 Invariants

| # | Invariant | Enforcement |
|---|---|---|
| 1 | The Bubble Tea thread never performs network I/O, disk reads, or blocking syscalls | `testing/synctest` audit + CI lint; AGENTS.md §3.2 |
| 2 | Reader goroutines are the only socket owners; they block on channel send under backpressure, never the UI | Design by construction (channel direction); proven by the gated-consumer ceiling gate |
| 3 | Buffers come from a global `sync.Pool` (fixed 32 KiB, reset-on-recycle); zero allocs in steady state | `BenchmarkStreamSteadyState` at 0 allocs/op, gated in CI |
| 4 | Memory ceiling = 66 × 32 KiB (2 162 688 B) — independent of response size | `TestMemoryCeilingInvariant` across 1 MB/10 MB/100 MB |
| 5 | Context cancellation unwinds in ≤50ms via `context.WithoutCancel` teardown | `TestCancellationNoLeak` (8 cancels, goroutine + fd accounting) |
| 6 | The checksum/tee mirror never blocks consumption — it is a passive sink on the read path | `sha256` cannot fail or block; digest verified against a reference run |
| 7 | A persistent stream reconnects and replays `Last-Event-ID` from the manager-owned token | `TestReconnectReplaysLastEventID` (connection dropped mid-stream) |
| 8 | Stream events are repainted per frame, never per event | Frame-coalescing gate: 500 events dispatch in far fewer than 500 chunks |
| 9 | Telemetry is never the reason a request fails | eBPF probes return a disposition (`available`/`blocked`/`not-built`) with a reason; httptrace timings stand alone |
| 10 | No protobuf schema is compiled into the binary | The reflection client's descriptor is hand-built and pinned field-by-field; the gate server's schema is built at runtime |
| 11 | UI and MCP layers cannot learn the wire | `ProtocolEngine` is the only executor surface they hold; `ProtocolRouter` dispatches on target scheme |
| 12 | The response direction never buffers a whole message | Encoder working-set high-water mark stays at one 32 KiB chunk for a 200 KB document (`TestJSONStreamDoesNotMaterializeTheMessage`) |
| 13 | A type mismatch in a `.http` body fails with a field path, never a coercion | `*FieldTypeError` carries `filter.pageSize` / `tags[2].pageSize` and the expected kind |

### 5.3 Backpressure & frame budget

- The UI consumes at most one batch of chunks per frame (16.6ms budget); surplus
  chunks remain buffered, applying backpressure to the socket reader.
- NDJSON/JSON-lines responses render incrementally (parse rows as they arrive);
  monolithic JSON renders as pretty-printed tail-first with a streaming indicator.

## 6. Zero-Trust Secret Architecture (`internal/secrets`)

### 6.1 Resolution chain (fail-closed)

```
{{var}} encountered in request
   │
   ▼
1. OS keychain lookup (service="pulse", account=<var name>)
   │ hit ──▶ value (in-memory, never logged at full length)
   │ miss
   ▼
2. Ephemeral .env (loaded into memory at boot; NEVER written to disk)
   │ hit ──▶ value
   │ miss
   ▼
3. Decrypted .env.vault (Issue 5: the PVLT2 container named by
   PULSE_VAULT_FILE, unwrapped ONCE at boot with the device identity,
   memory-only, zeroised on Close; an explicit .env wins over it)
   │ hit ──▶ value (registered with the redactor at boot, like every tier)
   │ miss
   ▼
4. HARD ERROR → request fails → exit 3 (headless/MCP) / error banner (TUI)
   (no dummy tokens, no empty-string fallback — AGENTS.md §4.4)
```

Executors never hold the raw chain: `App.resolver()` (`engineResolver`, in
`internal/app` — the composition root) translates `secrets.ErrUnresolved`
onto `engine.ErrUnresolved` so the runner's exit-3 mapping fires in every
mode. The translation cannot live anywhere else: engine may not import
secrets and secrets may not import engine (no sibling imports, §3.1), so it
sits in the one package where both sides are visible.

### 6.2 Platform keychain backends (CGO-free)

| OS | Backend | Mechanism | Status |
|---|---|---|---|
| Linux | Secret Service via D-Bus | `github.com/godbus/dbus/v5` (pure Go), `org.freedesktop.Secret.Service` | Implemented |
| macOS | Keychain | Shell-out to `/usr/bin/security find-generic-password` (pure Go; no Security.framework cgo) | Implemented, NOT RUN (no macOS host) |
| Windows | Credential Manager | `golang.org/x/sys/windows` advapi32 `CredRead` (pure Go syscall) | Implemented, NOT RUN (no Windows host) |
| other | none | reports BLOCKED; the chain falls through to `.env`, then to a hard error | Implemented |

If a backend is unavailable (headless CI, minimal WSL), the resolver reports
`BLOCKED (reason: no keychain binding)` per AGENTS.md §4.5 — it never fakes success.
A keyring item that exists but is **locked** is reported as BLOCKED too: unlocking
needs an interactive prompt, which a headless process cannot supply, and a
placeholder is not an option (AGENTS.md §4.3).

### 6.3 Storage exclusion guarantee

**Secrets never touch `modernc.org/sqlite`.** The schema (§8) contains no credential
columns by construction; a CI check greps migrations for forbidden column names
(`token`, `secret`, `password`, `authorization`, `apikey`, `credential`).

`store.AuditSchema` enforces the same rule **at runtime on every `Open`**: it reads
the live `sqlite_master` DDL (not a copy of the constant) and refuses to start if
any column name could hold a credential. Matching is case-insensitive and
normalizes `_`, `-`, and `.`, so `api_key` cannot slip past a rule that only knows
`apikey`.

### 6.4 Redaction pipeline

Sensitive headers (`Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`,
`X-Api-Key`) and all vault-sourced values are replaced with `[REDACTED]` before any
output reaches stderr, the UI, or the SQLite store. Redaction is applied at the
single output chokepoint (`internal/secrets.Redact`), not per-call-site.

**Two layers, both required.** Name-based rules cover the header names above plus
`X-Auth-Token`, `X-Session-Token`, `X-Csrf-Token`, `Proxy-Authorization-Basic`, and
`WWW-Authenticate`. Value-based rules scrub every secret **the moment it is
resolved, before it is returned**, wherever it later appears — a server that echoes
a token back into a 200 response defeats layer one entirely.

The engine implements this at its own boundary: response headers are copied and
redacted before they reach a `RunResult`, response **body chunks** are scrubbed in
place inside the dispatch goroutine before the `Sink` sees them, and URL userinfo is
stripped from both the recorded URL and any `net/http` transport error.

**Outbound requests are deliberately not scrubbed.** The real secret must reach the
server; only diagnostic *copies* (RunResult, errors, store rows) are redacted.
`internal/engine` declares a `Redactor` interface rather than importing
`internal/secrets`, keeping the domain packages siblings per §3.1; `internal/app`
supplies the concrete implementation.

## 7. Sanctioned Third-Party Dependencies (Stdlib-First Exceptions)

Every non-stdlib dependency requires an explicit entry here and a MEMORY.md ADL.

| Package | Purpose | Why stdlib is insufficient | Anchor |
|---|---|---|---|
| `modernc.org/sqlite` | Pure-Go SQLite engine | No stdlib SQL database; CGO is banned | G1 |
| `github.com/charmbracelet/bubbletea` | Elm-architecture TUI loop | Terminal UIs are not a stdlib concern | G1 |
| `github.com/charmbracelet/lipgloss` | Styling, truecolor, layout | ANSI styling engine | G1 |
| `github.com/charmbracelet/bubbles` | viewport/list/textinput/spinner | Component library | G1 |
| `github.com/charmbracelet/huh` | Interactive forms | Form orchestration | G1 |
| `github.com/mark3labs/mcp-go` | MCP JSON-RPC server/client over stdio | MCP protocol implementation | G3 |
| `github.com/godbus/dbus/v5` | Linux Secret Service transport (pure Go D-Bus) | OS keychain access without libsecret/cgo | G1 |
| `modernc.org/sqlite` | SQLite driver — **the only sanctioned driver** | `mattn/go-sqlite3` is cgo and banned | G1 |
| `golang.org/x/term`, `golang.org/x/sys` | isatty / advapi32 credential access | OS-level syscalls | G1 |
| `github.com/google/go-cmp` (test-only) | Structural diffs for golden snapshots | No stdlib diff | G1 |
| `github.com/BurntSushi/toml` | `pulse.toml` config parsing | No stdlib TOML | G1 |
| `github.com/coder/websocket` | WebSocket client (RFC 6455) | No stdlib WebSocket | G2 §2.1 |
| `google.golang.org/protobuf` | Protobuf runtime: `protoreflect`, `dynamicpb`, `protodesc`, `protojson` | Move 6 mandates fully dynamic schemas, so messages must be constructible from descriptors fetched at runtime; this is Google's own runtime, pure Go | G2 §2.2 |
| `google.golang.org/grpc` | gRPC transport: HTTP/2 framing, metadata, status codes | No stdlib gRPC; the cgo resolver is unused and `CGO_ENABLED=0` builds the pure-Go path | G2 §2.2 |
| `google.golang.org/grpc` + `google.golang.org/protobuf` | gRPC transport + dynamic messages | No stdlib gRPC/protobuf | G2 §2.2 |
| `github.com/cilium/ebpf` | eBPF program loading — **build-tagged `ebpf`, not in default builds** | No stdlib eBPF | G2 §2.3 |
| ~~`github.com/tetratelabs/wazero`~~ | WebAssembly runtime — **now vendored (Move 11, §15)** | No stdlib Wasm | G3 §3.4 |
| `github.com/owulveryck/onnx-go` | Pure-Go ONNX inference for local embeddings | No stdlib ML runtime | G3 §3.2 |
| `github.com/fsnotify/fsnotify` | Filesystem watching for the RAG indexer | No stdlib recursive watcher | G3 §3.2 |
| `github.com/grandcat/zeroconf` | mDNS/Bonjour peer discovery | No stdlib mDNS | G4 §4.2 |
| `github.com/getkin/kin-openapi` | OpenAPI 3.0/3.1 parsing for auto-fuzzing — **NOT VENDORED**; Move 9 reads the needed subset directly (§13) and no YAML module is permitted | No stdlib OpenAPI | G3 §3.3 |
| `github.com/santhosh-tekuri/jsonschema/v6` | MCP payload validation | No stdlib JSON Schema | G3 §3.1 |
| ~~`github.com/tetratelabs/wazero`~~ | (Move 11, §15) | — | — |

**Move 12 added no dependency.** The vault is stdlib crypto (`crypto/aes`,
`crypto/cipher`, `crypto/rand`) precisely because AGENTS.md §2.2 requires the
stdlib first and because a cryptography dependency is the class of dependency
most worth not having: the audit surface is the standard library's, which is
reviewed by more eyes than any vendored copy.

Anything not on this list fails `go mod tidy` audit in CI. `ROADMAP.md` §0.4 is the
admission point for forward contracts; rows introduced by roadmap goals carry their
goal anchor above.

Build-tag and artifact constraints:

- `github.com/cilium/ebpf` compiles in only under `//go:build ebpf`; its eBPF
  object files are produced out-of-band (clang/bpftool) and pinned by SHA-256 —
  the default build never requires a kernel capability or privilege.
- The ONNX embedding model is an out-of-band artifact pinned by SHA-256 in
  `pulse.toml`, loaded from `$PULSE_HOME/models/`, and **never downloaded at
  runtime** (offline-first invariant).
- Wasm guest scripts are repo-committed, SHA-pinned artifacts (Rust/TS targets);
  no runtime fetch.
- Vault PKI, CRDT, and telemetry clocks are stdlib-only (`crypto/ecdh`,
  `crypto/aes`, `crypto/cipher`, `crypto/ed25519`, `encoding/binary`) — no exceptions.

## 8. Storage Schema (`internal/store`)

**Driver:** `modernc.org/sqlite` (pure Go, CGO-free). `mattn/go-sqlite3` is permanently
banned (AGENTS.md §2.4).

**Pragmas, set at open and verified there:**

- `secure_delete=ON` — deleted rows are overwritten with zeros instead of surviving as
  free cells recoverable from the raw file. Verified behaviourally: the test writes a
  marker, deletes it, vacuums, and greps the raw database bytes for the marker.
- `journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=5000`.

```sql
CREATE TABLE collections(id TEXT PK, root_path TEXT, created_at INTEGER)
CREATE TABLE requests(id TEXT PK, collection_id TEXT FK, file_path TEXT, name TEXT,
         method TEXT, url_template TEXT, ordinal INTEGER, updated_at INTEGER)
CREATE TABLE history(id INTEGER PK AUTOINCREMENT, request_id TEXT FK, started_at INTEGER,
        duration_ms INTEGER, status_code INTEGER, request_bytes INTEGER,
        response_bytes INTEGER, sha256 TEXT, error TEXT, run_source TEXT)
```

- `run_source`: `tui` | `mcp:<client-name>` — provenance for audit.
- Bodies (request/response) are **not persisted by default**; history stores metadata
  and the response SHA-256 only. Body persistence is deferred pending a privacy ADL.
- `response_bytes`/`duration_ms` power the timing UI and `get-history` MCP tool.
- `url_template` stores the **raw** template including `{{var}}` anchors: the store
  records what the file said, never what was sent.
- `InsertHistory` refuses any row whose `error` text still contains a `{{...}}`
  anchor — a surviving anchor means an unresolved secret is one bug away from a
  secret.

## 9. MCP Server (`internal/mcp`)

**Implementation status: `execute_request`, the five collection/history tools,
and the three iterative-fuzz tools are complete and gated (#7). `explain_failure`
remains unbuilt** — see the status column and TASKS.md §4.3.

- Library: `github.com/mark3labs/mcp-go` v1.2.1 (sanctioned in §7).
- Transport: stdio, spec-compliant lifecycle (`initialize` → `initialized` →
  operations → `shutdown` → exit 0). No custom framing. MCP stdio is
  **newline-delimited JSON**, not Content-Length framing — anything that parses
  the wire must buffer to the next newline and tolerate split frames.
- Tools (**`snake_case`, per MCP convention** — see MEMORY.md ADL-0020 for the
  deviation from ROADMAP.md §3.1.3's illustrative kebab-case):

| Tool | Params | Returns | Status |
|---|---|---|---|
| `execute_request` | exactly one of `payload` \| `path`, plus `index?`, `env?`, `timeout_ms?`, `body_mode?`, `include_body?` | status, timing, size, checksum, bounded body (redacted) | **BUILT** |
| `list_collections` | — | collection roots and file counts | **BUILT (#7)** |
| `list_requests` | `collection?` | request refs (file, name, method, url template) | **BUILT (#7)** |
| `get_request` | `ref` | stored request definition (raw template; secrets never stored) | **BUILT (#7)** |
| `get_run` | `run_id` | execution details of one run by id, redacted via scrubRunRecord | **BUILT (#7)** |
| `get_history` | `limit?`, `offset?` | recent executions newest-first, redacted via scrubRunRecord | **BUILT (#7)** |
| `explain_failure` | `run_id` \| `ref` | top-k indexed code chunks correlated to a 5xx (ROADMAP.md §3.2.4) | not built |
| `fuzz_start` | `schema_path`, `op?`, `target`, `seed?`, `budget?`, `rate?`, `concurrency?` | fuzz session handle (ROADMAP.md §3.3.3) | **BUILT (#7)** |
| `fuzz_next` | `session` | next probe verdict + RAG-bound 5xx context + finding id | **BUILT (#7)** |
| `fuzz_report` | `session` | TAP 13 + deduplicated findings, redacted; discards the session | **BUILT (#7)** |

### 9.1 Boot ordering (the isolation boundary)

```
cmd/pulse/main.go   runMCP()
  │
  ├─ 1. mcp.Lock(audit)          ← FIRST. Captures os.Stdout as lock.Real,
  │                               then points os.Stdout at /dev/null, or at a
  │                               Tripwire when PULSE_STDOUT_AUDIT=1.
  │                               Must precede any *use* of the mcp package:
  │                               a dependency that captured os.Stdout in init()
  │                               has already missed the swap, and the tripwire
  │                               is the backstop for that case.
  │
  ├─ 2. mcp.ServeLocked(ctx, lock.Real, os.Stdin)
  │                               The JSON-RPC codec is the ONLY holder of
  │                               lock.Real. ServeLocked returns
  │                               (exitCode, error); clean stdin EOF and
  │                               context.Canceled both map to exit 0.
  │
  └─ 3. lock.Restore()           (deferred)
```

`Lock` is a function and `StdoutLock` is its result type — Go forbids a
package-level type and function from sharing an identifier.

### 9.2 Error taxonomy

Protocol failures keep their JSON-RPC meanings. Domain failures occupy a
reserved `-320xx` block so an agent can branch on the **code** rather than on
English prose; each carries a stable `category` string in `data`, and
`ProtocolError.payload()` includes the cause (e.g. the parser's `:1:1:`).

| Code | Category | Meaning |
|---|---|---|
| `-32700` | `internal` | JSON parse error |
| `-32600` | `internal` | invalid request object |
| `-32601` | `internal` | unknown method |
| `-32602` | `arguments` | schema or semantic validation failure — handler never invoked |
| `-32603` | `internal` | internal error |
| `-32002` | `parse` | `.http` AST parse failure |
| `-32003` | `secret` | unresolved variable — fail-closed, never a placeholder |
| `-32004` | `timeout` | run exceeded its budget |
| `-32005` | `transport` | connection/dial/read failure |
| `-32006` | `unsupported` | request shape the engine cannot serve |

A failure with **no** HTTP status is reported as the error itself, never as a
synthesised `status: 0` outcome; any partial body is attached to `data`.

### 9.3 Invariants

- All logging goes to stderr only, prefixed `pulse:`. A single byte on stdout
  outside the JSON-RPC layer is a release blocker (AGENTS.md §3.3), and the
  tripwire turns that claim from unfalsifiable into a failing test.
- Call arguments are schema-validated **before** dispatch (jsonschema); unknown
  keys are rejected (`additionalProperties: false`), and coercion is per-field,
  opt-in, and **recorded** in a `warnings` list the caller can read
  (ROADMAP.md §3.1.3).
- Response bodies accumulate to at most `MaxResponseBytes` (256 KiB) — a model's
  context window, not a server buffer. Truncation is explicit: `truncated: true`,
  `body_bytes` reports the **true** total, plus warning text.
- Redaction is applied in `collectingSink.outcome`, immediately before the
  outcome becomes a tool result — the single chokepoint no path can bypass.
  **Open:** no composition root supplies a concrete `secrets.Redactor` yet
  (`internal/app` does not exist), so in MCP mode today redaction is by header
  name only. Tracked in TASKS.md §4.3; not claimed as complete.

## 10. Error Handling & Exit Codes

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Generic failure |
| 2 | Usage / parse error (incl. bad flags, `.http` AST errors) |
| 3 | Keychain / secret resolution failure |
| 130 | SIGINT graceful abort (TAP plan tail emitted, per ROADMAP.md §4.1.4) |

All errors wrap with `%w`; structured diagnostics include file:line:col where
applicable. Never swallow, never dummy-fallback.

## 11. Build & Platform Matrix

| Target | GOOS/GOARCH | CGO | Notes |
|---|---|---|---|
| Linux | linux/amd64, linux/arm64 | `CGO_ENABLED=0` | DBus backend |
| macOS | darwin/arm64 | `CGO_ENABLED=0` | `security` CLI backend |
| Windows | windows/amd64 | `CGO_ENABLED=0` | advapi32 backend |

Built with the `go` toolchain only (`CGO_ENABLED=0 go build -trimpath ./cmd/pulse`).

---

*Topology changes to this document are mandatory and same-commit per AGENTS.md §4.6.*

## 12. Local RAG (`internal/rag` + `internal/store/rag.go` + `internal/mcp/error_binder.go`)

**Status (MEMORY.md ADL-0021):** indexing, storage, retrieval and 5xx binding are
built and gated. Incremental reindex, the 50k-chunk latency benchmark, and the
`internal/app` composition root that would make the explanation reachable from a
running server are **not** built.

### 12.1 Why there is no vector extension

`sqlite-vec`, `vss`, and every other SQLite vector extension are C libraries.
`CGO_ENABLED=0` is mandatory on every platform and every CI job (§2.1), so there
is no vector extension, no ANN index and no HNSW — not as a performance
trade-off, but as a constraint of the build. FTS5 **was verified compiled into
`modernc.org/sqlite` v1.60.1 with a callable `bm25()`** by probe, not assumed.

### 12.2 Hybrid retrieval

| Signal | Implementation | Finds |
|---|---|---|
| Lexical | FTS5 + `bm25()`, top-200 | exact tokens — what a stack trace supplies |
| Structural | brute-force cosine over int8, d=256 | shape, identifier morphology |

Fused by **Reciprocal Rank Fusion** (`1/(60+rank)`, summed across whichever
signals placed a chunk). RRF merges two **rankings**, not two scores, so it needs
no calibration between an unbounded bm25 and a cosine in [-1,1] — which is the
whole reason it is used over a weighted sum. Each hit reports `lexical_rank`,
`vector_rank` and `sources`, so a fused result is auditable.

**The FTS5 query is built, never passed through.** Error bodies are full of `"`,
`*`, `:`, `(`, `AND`, `OR`, `NOT`, `NEAR`; handing one to `MATCH` verbatim turns
every response containing a quote into a query syntax error — the explanation
feature failing precisely when it is needed. Terms are extracted as FTS5's own
bareword alphabet and re-emitted as quoted phrases joined by **OR** (AND would
return zero rows exactly when the identifier matters most).

### 12.3 Chunking (functional boundaries, windowing only as overflow)

| Language | Cut at | Notes |
|---|---|---|
| Go | `go/parser` top-level decls | compiler's boundaries, not a guess; `decl.Doc` included; symbols receiver-qualified (`(*Router).handle`) |
| Python | indentation-defined blocks | indentation IS the structure; a block ends at a dedent **or** a same-indent block start; nested `def`s get their own named chunks |
| Astro | frontmatter / `<script>` / `<style>` / template | HTML with islands, so the author's own units |

Declarations over **64 lines** are windowed with **16-line overlap**. The overlap
is not decoration: without it, a defect on a boundary line lands in neither
window when the split point moves.

### 12.4 Secret guarantees

- **Deny-list at the WALKER**: a file matching `.env*`, `*.pem`, `*.key`,
  `id_rsa`, `.git-credentials`, `secrets.*` is never *opened*. A secret that is
  read can already have been indexed; redaction afterwards is too late.
- **Redaction before storage AND before embedding.** An embedding of a secret is
  a secret in a form nothing downstream can redact — it is already hashed into
  numbers.
- **The RAG schema passes the same `AuditSchema` no-credential-columns gate** as
  the history schema.
- Error text and retrieved source are both redacted before the error text becomes
  a search **query**: the query reaches an embedding and then a store.

### 12.5 Streaming and memory

`Index(root, opts…) iter.Seq2[Chunk, error]` is **lazy**, and that is the memory
contract rather than an API convenience: a generator that accumulated its output
would hold every chunk of the repository live at once. A consumer that writes and
drops keeps the live set at one file's chunks plus one worker.

Bounded by construction: fixed worker pool (never a goroutine per file), a
512 KiB per-file cap enforced from the directory entry, one descriptor open per
worker, pooled read buffers. **Termination cancels a derived context** rather than
using a side channel — a side channel must be selected on at every point the
producer can block, and any forgotten point is a deadlock reported as a suite
timeout. The gate asserts the walker's own `PeakBuf` high-water mark does not grow
while the chunk count does (3,138 bytes at both 125 and 1,000 files).

### 12.6 5xx binding

`FailureBinder` runs on every outcome and attaches the top-3 retrieved chunks
when, and only when, `500 <= status <= 599`. Both directions matter: a 5xx that
gets no context is a silent feature failure, and a 200 that does is noise that
costs context window and teaches the agent to ignore the attachment. A retrieval
failure is a **DEGRADATION recorded in `warnings`**, never a failure of the
request — the agent asked to run a request and got a 500, and that is the answer.

`internal/mcp` does **not** import `internal/store` (§3.1); the seam is the
`Retriever` interface, satisfied by `StoreRetriever`, which takes function
parameters rather than the store type. That keeps the database driver out of the
MCP-reachable package tree (AGENTS.md §3.3).

### 12.7 Storage

```
rag_chunks(id PK, path, lang, start_ln, end_ln, symbol, text, vec BLOB, scale, updated_at)
rag_fts   VIRTUAL TABLE fts5(text, symbol, path, content='rag_chunks')
rag_meta(key PK, value)
```

Created by `InitRAG`, **not** by `Open`: the main schema applies unconditionally
when the history database opens, so putting FTS5 there would make Pulse
unopenable on a driver build without FTS5 — for an optional feature. Writes are
batched at **256 chunks per transaction** so a concurrent reader waits
milliseconds; each chunk's row and its FTS row share the transaction, because
splitting them leaves an index that disagrees with itself depending on which leg
you ask.

---

## 13. Auto-Fuzzing (`internal/fuzz` + `internal/mcp/fuzz_tool.go` + `internal/mcp/fuzz_engine.go`)

### 13.1 Shape

```
OpenAPI document (JSON or YAML)
   │  LoadOpenAPI — refusing subset parser, fatal $ref resolution
   ▼
fuzz.IR  ──►  Generate(op, seed, budget)  ──►  []Payload   (deterministic, sorted)
                                                   │
             ValidateFuzzOptions (loopback, clamped bounds)
                                                   │
             limiter.Wait(ctx)  ──►  semaphore.Acquire(ctx)   ── rate before send
                                                   │
             EngineFuzzRunner.Run ──►  render .http ──► ast.Parse ──► engine.Run
                                                   │
                                             status + Sink
                                                   ▼
                     fuzz.Render([]Result)  ──►  TAP 13  ──►  MCP tool result
```

`internal/fuzz` builds payloads and never sends one. `internal/mcp` owns the
dispatch and the reporting. The seam is `FuzzRunner`, mirroring the injected
`EngineExecutor` pattern — and `EngineFuzzRunner` is the engine-backed
implementation.

### 13.2 Three limits, because they bound different things

| Bound | What it limits | Default | Ceiling |
|---|---|---|---|
| `budget` | total payloads, so a miscounted schema cannot run for ever | 200 | 5,000 |
| `rate_per_second` | load **on the target** | 20/s | concurrency × 100 |
| `concurrency` | load **on Pulse** — open sockets, goroutines | 4 | 32 |

Collapsing them into one knob is the mistake: a high rate with unbounded
concurrency opens hundreds of sockets, and a low rate with unbounded concurrency
holds them open for the whole run.

### 13.3 The rate guarantee

`internal/fuzz.Limiter` is GCRA. With rate `R` requests/second, burst `C`, and
`interval = 1/R`, `burstDelta = (C-1)/R`:

```
tat   := max(now, previous_tat)      // theoretical arrival time
allow := tat - burstDelta             // when this request may proceed
if allow < now { allow = now }
previous_tat = tat + interval         // RESERVE at decide time
```

Reserving at DECIDE time rather than WAKE time is the whole point. The textbook
token bucket sleeps when empty, so N waiters compute the same sleep and wake
together — admitting a burst of N under exactly the load the limiter exists to
prevent. The reservation gives:

```
admission_time(i) >= t0 + i/R - (C-1)/R
  ⟹  for any window of length Δ:  count <= C + R·Δ
```

`MaxPeakAdmissions(Δ)` returns that bound, and the gate asserts against it
rather than recomputing the formula — a test that re-derives the arithmetic can
agree with a broken implementation by both being wrong the same way.

### 13.4 The transport is part of the rate guarantee

`fuzzTransport()` is not a tuning choice. `DisableKeepAlives` removes the
pooled-connection race that makes `net/http` transparently retry an idempotent
request; `Proxy: nil` stops `HTTP_PROXY` from carrying a `127.0.0.1` target
through a proxy host. Without the first, the server sees more requests than the
limiter scheduled; without the second, "loopback" describes a URL rather than
the traffic. Against loopback the extra handshake costs microseconds, which is
irrelevant next to the rate limit every probe already waits for.

### 13.5 Verdicts

| Outcome | TAP line | Meaning |
|---|---|---|
| 2xx or 4xx | `ok` | the server handled the boundary value; a 4xx is correct behaviour |
| 5xx | `not ok` | a boundary value crashed the handler — the finding this tool exists for |
| transport error | `not ok` | no response at all |
| undeliverable probe | `ok` + `# SKIP` | the transport cannot carry it (RFC 7230 §3.2); it never left Pulse |
| cancelled / budget exhausted | `ok` + `# SKIP` | not run; the plan still covers it |

The fourth row is the one that keeps the tool honest: a header payload carrying
a NUL is refused by the client before it is sent, so the target said nothing
about it. Reporting that as a failure makes a correct server look broken;
dropping it silently shrinks a corpus the plan still promises, which hangs the
consumer.

### 13.6 Determinism

`Generate` is a pure function of `(op, seed, budget)`. No clock, no global RNG,
and every map is walked through `sortedKeys` — Go randomises map iteration, and a
corpus emitted in map order differs on every run, which would make "it stopped
happening" indistinguishable from "this run generated different inputs". The
seed chooses only among values that are equally valid for the declared schema,
so coverage of declared constraints never varies with it.

---

## 14. Composition Root (`internal/app`)

### 14.1 Why it exists

Every subsystem built in Moves 0–9 was gated and none of them could run. Three
open items across three moves shared one root cause — nothing constructed
anything:

| Item | Symptom | Root cause |
|---|---|---|
| P4.3.3 (Move 7) | MCP mode redacted by header name only; a vault value in a response **body** reached the model | `DefaultExecutor` built the executor with a nil `secrets.Redactor` |
| P3.2.10 (Move 8) | The 5xx explanation was unreachable from `pulse --mcp` | Nothing held a `*store.Store` |
| P3.3a.9 (Move 9) | `execute_fuzz_run` existed and could not be called | Nothing held an `EngineFuzzRunner` |

Recording a blocker three times is a signal that the blocker is structural.
`internal/app` is the fix: the one package allowed to name a concrete dependency
on another package's behalf.

### 14.2 The wiring

```
                 ┌────────────────── internal/app ──────────────────┐
  store.Open ──► │ Store  ──InitRAG──► rag_chunks / rag_fts       │
                 │   │                                            │
  env file ────► │ Chain ──┐                                       │
                 │   │     ├──► ONE *secrets.Redactor instance     │
                 │   └───► │      (resolve registers, scrub reads) │
                 │          │                                      │
  rag.NewHashingEmbedder ─► Embedder                             │
                 │          │                                      │
                 │ Retriever{SearchFn, EmbedFn} ──► FailureBinder│
                 │          │                          │           │
                 │ Executor{EngineExecutor, Redactor, Resolver}    │
                 │ FuzzRunner{EngineFuzzRunner, Redactor}         │
                 └──────────┬───────────────────────────────────────┘
                            │ mcp.NewServerWithConfig
                            ▼
              execute_request        execute_fuzz_run
```

The single most important line in that diagram is the shared `Redactor`. A value
resolved through `Chain` is **registered** with it; everything that leaves the
process is **scrubbed** by it. Two redactors would leave a window where a value
is known to the resolver and invisible to the scrubber — and every individual
field would still be non-nil, so no structural check would catch it. The gate
asserts the round trip for exactly that reason.

### 14.3 Lifecycle

```
New(ctx, cfg) ──► (*App, error)      partial Apps never escape
Close() ──► cancel ctx ──► store.Close()      idempotent
```

The order is load-bearing. Cancelling first lets in-flight work observe the
shutdown and unwind through its own teardown path (Goal 1 §1.2.6) rather than
having its SQLite handle pulled out from under it. Idempotence is not polish: a
deferred `Close` and an explicit one from a signal handler both run in any
realistic program.

### 14.4 Configuration

| Variable | Effect | Default |
|---|---|---|
| `PULSE_HOME` | data directory | `<user config dir>/pulse` |
| `PULSE_ENV_FILE` | ephemeral `.env`, loaded in memory | none |
| `PULSE_RAG_ROOT` | corpus to index at startup | none (no indexing) |
| `PULSE_RAG_CONCURRENCY` | walker workers | package default |
| `PULSE_RUN_TIMEOUT` | one request | 30s |
| `PULSE_STDOUT_AUDIT` | `1` swaps stdout for the tripwire | off |

Environment rather than flags: every one is a deployment detail an MCP client
sets once, and flags would invite an agent to vary them per call.

### 14.5 Fail-closed construction

`MCPServer` refuses to serve without an executor, a fuzz runner or a redactor.
This is not defensiveness for its own sake — a server with no redactor still
answers every request, correctly formatted, just with the user's tokens in the
body. Refusing at construction is the only state in which the mistake cannot
ship unnoticed.

### 14.6 Degradation, reported

| Condition | Behaviour | Why not fatal |
|---|---|---|
| No OS keychain | variables resolve via `.env`, then fail closed | A headless developer machine has no keychain; failing to start there would make Pulse unusable for a large fraction of its users |
| Driver without FTS5 | lexical leg degrades; vector leg answers | Local RAG is optional and already reports DEGRADED |
| Unreadable `.env` | degradation recorded, env is empty | The order is keychain → `.env` → error, and a keychain-only user must still get a tool |
| Corpus will not index | degradation recorded, empty index | An empty index is a correct answer ("no context"), not a broken feature |

What is forbidden is booting and behaving as though the subsystem had loaded.

---

## 15. Wasm Sandbox (`internal/wasm`)

### 15.1 Threat model

A signing script is user-supplied code running inside Pulse's process, on the
user's machine, handling bytes that authenticate the user's requests. It must not be
able to read the filesystem, the environment, or the keychain; open or inherit a
socket; read the clock or the randomness source; allocate without bound; run without
bound; or hand the host a pointer it can trust.

### 15.2 Every denial is an omission

```
wazero.NewRuntimeConfigInterpreter()
    WithCloseOnContextDone(true)     tier 1: preemption
    WithMemoryLimitPages(64)          4 MiB

no host modules instantiated   -> any import fails to resolve
no WithEnv / WithFS            -> no environment, no filesystem
no WithSys* / WithRandSource   -> no clock, no randomness, no sleep
no WithOsyield / WithArgs      -> no scheduling, no argv
stdout/stderr = io.Discard     stdin = always-EOF
WithStartFunctions()           -> clears the default _start; WASI is not linked
```

There is no capability to revoke later because none is ever granted, which is a
stronger property than a whitelist: a guest that names a host module is not refused
by a policy, it fails to find anything to import.

### 15.3 The memory boundary

```
host:  ptr := allocate(len(envelope) + 4096)     0 is failure, and refused
host:  memory[ptr : ptr+len] = envelope
guest: n    := sign_request(ptr, len)            len is the ENVELOPE, not the capacity
host:  result = copy(memory[ptr : ptr+n])        0 <= n <= len + 4096
```

**A guest-supplied LENGTH, never a pointer.** ROADMAP §3.4.2 sketched the guest
returning `(sig_ptr, sig_len)`; a guest-returned pointer is an address the host
must trust, and a bounds check that is wrong is a memory-safety bug in the host.
Pinning the output to the host's own buffer leaves only a length, checked against a
buffer the host owns.

`checkRange` subtracts rather than adds: `ptr + size` wraps on uint32, so
`ptr+size <= avail` is trivially true at `2^32-1`. `ErrBounds` (a host bug) and
`ErrABIViolation` (a guest bug) are distinct. The result is copied out — `Read`
returns a VIEW, and a later guest write must not mutate a signature that has
already been verified.

### 15.4 What wazero v1.12 actually enforces

| Bound | Mechanism | Guarantees | Measured |
|---|---|---|---|
| Time | `WithCloseOnContextDone` + deadline | **termination** | 50.4ms against a 50ms deadline, `(loop br 0)` |
| Fuel | instantiation-time call budget, cancels the module context | **detection + poisoning**; deterministic | 20,000-call guest, budget 50 — detected, module poisoned |
| Depth | wazero's own interpreter stack bound | a clean trap for deep recursion | recursion traps, host survives |
| Memory | 4 MiB cap | the ceiling holds | `memory.grow` REFUSES; it does not trap |

Fuel is a detector, not a preemptor — the 20,000-call guest above ran to completion
before the cancellation landed, because a straight-line body has no safe point. Its
value is that it counts calls rather than milliseconds (so it decides identically on
a loaded CI runner) and that a module detected over budget is never reused: a guest
observed doing too much cannot sign later on a call where the clock happened to be
quiet. wazero v1.12 exposes no fuel API, and a panicking listener does not abort a
call, so neither of those was available.

### 15.5 Not built

`internal/wasm` has no caller. §3.4.3's host-import whitelist and the
HMAC-by-key-id chain do not exist, so a guest cannot compute a signature. There is
no instance pooling. Goal 3 is recorded as PARTIAL (§3.5) rather than COMPLETE, and
these are three of the four reasons.

---

## 16. CI/CD Loop & Vault (`internal/engine` + `internal/app` + `internal/secrets`)

### 16.1 Two halves, one layering rule

```
internal/engine/test_runner.go   parse, execute, evaluate -> []Entry
                                          |
                                          | imports internal/ast ONLY
                                          v
internal/app/test.go             tapReporter -> fuzz.Render (Move 9)
                                          |
                                          v
cmd/pulse/main.go                pulse test <dir> -> exit code
```

`engine` produces and does not format; `app` formats. That is not tidiness —
Move 9's TAP emitter lives in `internal/fuzz`, and AGENTS.md §3.1 forbids a
sibling importing a sibling. `app` is the only package allowed to name both, so
it is the only place the wiring can live, and the result is that a JSON or
silent reporter costs no change in the engine.

`App.newRouter` is shared by the test runner and the MCP path, so a test run and
an MCP call redact identically. Two constructions would be two places for the
redactor, resolver and timeout to drift apart.

### 16.2 The exit code

| Code | Meaning |
|---|---|
| 0 | every test point passed, or was skipped with a stated reason |
| 1 | any assertion failed, or a request errored |
| 2 | usage error, parse error, **or a report with no test points** |
| 3 | secret-resolution failure |

`ExitCodeFor` is a pure function of the report, so `pulse test` and the runner
cannot disagree. Two properties are deliberate and are gated rather than
argued:

- **an empty report is 2, not 0** — a run that produced no test points has not
  passed anything, and success for "nothing happened" is the greenwash §4.5
  exists to prevent;
- **a secret failure is scanned for before any plain failure**, so exit 3 does
  not depend on file ordering. Exit 3 means "configure your keychain"; exit 1
  means "a test failed"; telling the user the second while the first is true
  sends them in the wrong direction.

### 16.3 The vault

```
"PVLT1" | flags u8 | nonce[12] | gcm.Seal(...) = ciphertext || tag[16]
```

AES-256-GCM from the stdlib. No KDF: the key is 32 bytes of `crypto/rand` held in
the keychain, and stretching a uniformly random key is theatre that implies the
key was a passphrase.

**A nonce is generated per seal and never reused.** This is the one property the
whole design rests on: nonce reuse under one key recovers GCM's authentication
key, so an attacker can forge a container that verifies. A counter, a timestamp,
a hash of the plaintext and a fixed constant each eventually repeat; only
`crypto/rand` cannot be made to repeat by a workload change, a clock
adjustment, or a rotation done twice. The gate seals 200 times and asserts no
nonce repeats, no container repeats, and all 200 decrypt to the same plaintext.

Every decryption failure reports identically. The GCM tag is the only mechanism
that distinguishes "wrong key" from "one flipped byte", so surfacing that to the
CLI would hand an oracle to anyone who can present a container and observe the
exit status.

`Seal` does not zeroise its caller's buffer. It did originally and the round-trip
gate caught it: a function that wipes what it was handed cannot be composed.
Zeroisation belongs to the caller, and the file path applies it to the buffer it
allocated itself — which is also why `vault encrypt` REMOVES the plaintext file.

### 16.3b The PVLT2 recipient model (ADL-0031)

```
"PVLT2" | flags u8 | count u16 | per slot: ephPub[32] | wrappedDEK[48] |
signerPub[32] | pNonce[12] | gcm.Seal(DEK, ...) = ciphertext || tag[16] | sig[64]
```

Hybrid: a fresh 32B DEK per seal encrypts the payload; each slot wraps it
under HKDF-SHA256(ECDH(eph, recipient), salt=ephPub, info=`pulse-vault-v1`)
with a zero wrap nonce (single-use key — the age rule). Ed25519 signs every
prior byte and is verified BEFORE any slot is attempted. Slot matching is
authentication-based (trial-unwrap; first slot that verifies wins) with one
identical `ErrNoRecipient` for all membership failures — no oracle. Device
identity is X25519 (decrypt) + Ed25519 (sign) halves in namespaced keychain
accounts; `pulse vault keygen` prints `pulse-pub-…` / `pulse-sig-…`.
Rotation/revocation is re-sealing over a new list (no separate path to
drift); PVLT1 still opens (magic mismatch keeps the versions apart).

### 16.4 Not built

The `vault rotate` CLI, the `pulse.recipients` file flow, and transparent
vault resolution all shipped (Issue 5, ADL-0032); `pulse test --format json`
/ `--silent` (ADL-0027) and the SIGINT plan tail with exit 130 (ADL-0028)
are built. What remains: the G-secret repo scan gate over diffs (§4.3.2 — a
commit holding keychain-shaped material must fail CI), and the contract-form
`*.vault`-reference detection in collections (§4.3.3 — superseded by
boot-time resolution, recorded as a deliberate deviation in ADL-0032). The
three real keychain write backends (darwin `security`, windows `CredWriteW`,
linux Secret Service) compile on every cross-compile target and their
last-mile syscalls remain BLOCKED in this environment: no darwin or windows
host, and no Secret Service daemon in WSL. What CHANGED (Issue #9, MEMORY.md
ADL-0041): everything AROUND the syscalls is now hermetically covered — the
argv construction (darwin, `internal/secrets/cmdbuilder.go`, untagged), the
D-Bus framing (linux, `internal/secrets/dbusframe.go`, linux-tagged — godbus's
own darwin/windows files do not compile under Go 1.27, so the seam stays
where the backend is), and the credential encoding (windows,
`internal/secrets/credencode.go`, untagged) — each at 100% statement coverage
with no daemon (`internal/secrets/boundary_test.go`, `dbusframe_test.go`).
The audit also closed two real defects: the darwin write passed the secret as
an argv element (now stdin via trailing bare `-w`, plus the absolute
`/usr/bin/security` pin the read path already had), and the linux write
stored under `{ServiceName: key}` while the read searched
`{service, account}` (items no read could find — now one shared
constructor). Wave 6.5 (MEMORY.md ADL-0042) then verified the Linux
boundary against BARE METAL: `scripts/ci/run-with-keyring.sh` builds an
isolated session (fresh bus, unlocked login collection, hermetic keyring
dir) and `internal/secrets/linux_live_test.go` (`integration` tag, zero
mocks) round-trips a random device identity through the production write
and read paths, proves daemon commitment via the independent `secret-tool`
client, deletes, and asserts `ErrNotFound`. Live probing fixed four real
defects the hermetic pins could not see: no `DefaultCollection` property
(reads resolve the `default` alias), `OpenSession` outputs belong in
`Store` (not `Call` args), `CreateItem` lives on the collection with
`a{sv}` props, and content_type is `s` (`(oayays)`). The Linux Secret
Service boundary is now mathematically AND empirically verified.
Darwin/windows last-mile syscalls stay NOT RUN (no hosts; cross-compile
green), reported with reason, never papered over (AGENTS.md §4.5).

---

