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
| `internal/ui` | Bubble Tea models, Lip Gloss styling, Huh forms | `app` | `mcp`, `store`, `engine` directly |
| `internal/mcp` | JSON-RPC lifecycle, tool registry, stdio transport | `app` (not built); today `ast`, `engine`, `secrets` via `EngineExecutor` | `ui` |
| `internal/rag` | Codebase indexing: bounded walker, per-language chunking, structural embedder | stdlib only (`go/parser`) | everything internal; **`store` and `mcp` both depend on this layer's OUTPUT, not on it** |
| `internal/app` | Shared services: run request, list collections, history queries, variable resolution | `ast`, `engine`, `store`, `secrets` | `ui`, `mcp` |
| `internal/engine` | `ProtocolEngine` executors (REST, SSE, gRPC), streaming, checksums, telemetry | `ast` (types) | `ui`, `mcp`, `store` |
| `internal/ast` | Lexer, parser, zero-allocation AST, `iter.Seq` traversal | stdlib only | everything internal |
| `internal/store` | SQLite schema, migrations, history persistence | stdlib + `modernc.org/sqlite` | everything internal |
| `internal/secrets` | Keychain backends, ephemeral env, resolver chain, redaction | stdlib + sanctioned backends (§7) | everything internal |
| `internal/devtools/importaudit` | Static, offline dependency + stdout-hygiene audit (dev tooling) | stdlib only | everything internal; never imported by shipped code |

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
3. HARD ERROR → request fails → exit 3 (headless/MCP) / error banner (TUI)
   (no dummy tokens, no empty-string fallback — AGENTS.md §4.4)
```

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
| `github.com/tetratelabs/wazero` | WebAssembly runtime (interpreter mode) | No stdlib Wasm | G3 §3.4 |
| `github.com/owulveryck/onnx-go` | Pure-Go ONNX inference for local embeddings | No stdlib ML runtime | G3 §3.2 |
| `github.com/fsnotify/fsnotify` | Filesystem watching for the RAG indexer | No stdlib recursive watcher | G3 §3.2 |
| `github.com/grandcat/zeroconf` | mDNS/Bonjour peer discovery | No stdlib mDNS | G4 §4.2 |
| `github.com/getkin/kin-openapi` | OpenAPI 3.0/3.1 parsing for auto-fuzzing | No stdlib OpenAPI | G3 §3.3 |
| `github.com/santhosh-tekuri/jsonschema/v6` | MCP payload validation | No stdlib JSON Schema | G3 §3.1 |

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

**Implementation status: `execute_request` is complete and gated. The remaining
tools below are not built** — see the status column and TASKS.md §4.3.

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
| `list_collections` | — | collection roots and file counts | not built |
| `list_requests` | `collection?`, `filter?` | request refs (file, name, method, url template) | not built |
| `get_request` | `ref` | full AST view of the request (secrets redacted) | not built |
| `get_run` | `run_id` | status of an in-flight run (from the streaming engine) | not built |
| `get_history` | `limit?` | recent executions with redaction applied | not built |
| `explain_failure` | `run_id` \| `ref` | top-k indexed code chunks correlated to a 5xx (ROADMAP.md §3.2.4) | not built |
| `fuzz_start` | `openapi_ref`, `strategy`, `budget` | fuzz session handle (ROADMAP.md §3.3.3) | not built |
| `fuzz_next` | `session` | next generated payload (deterministic seed) | not built |
| `fuzz_report` | `session` | deduplicated findings, redacted | not built |

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
