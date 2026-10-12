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
| `internal/mcp` | JSON-RPC lifecycle, tool registry, stdio transport | `app` | `ui`, `engine` directly |
| `internal/app` | Shared services: run request, list collections, history queries, variable resolution | `ast`, `engine`, `store`, `secrets` | `ui`, `mcp` |
| `internal/engine` | `http.Client` execution, streaming, checksums, timing | `ast` (types) | `ui`, `mcp`, `store` |
| `internal/ast` | Lexer, parser, zero-allocation AST, `iter.Seq` traversal | stdlib only | everything internal |
| `internal/store` | SQLite schema, migrations, history persistence | stdlib + `modernc.org/sqlite` | everything internal |
| `internal/secrets` | Keychain backends, ephemeral env, resolver chain, redaction | stdlib + sanctioned backends (§7) | everything internal |
| `internal/devtools/importaudit` | Static, offline dependency + stdout-hygiene audit (dev tooling) | stdlib only | everything internal; never imported by shipped code |

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
| 2 | Reader goroutines are the only socket owners; they block on channel send under backpressure, never the UI | Design by construction (channel direction) |
| 3 | Buffers come from a global `sync.Pool` (`cap` 32KiB, reset-on-recycle); zero allocs in steady state | `-benchmem` gate on streaming benchmark |
| 4 | Memory ceiling = 64 × 32KiB (channel) + in-flight chunks — independent of response size | Stress test with 100MB payload |
| 5 | Context cancellation (user interrupt `Ctrl+C`, MCP timeout) unwinds in ≤50ms via `context.WithoutCancel` teardown goroutines | Cancellation test |
| 6 | The checksum/tee mirror never blocks consumption — it is a passive sink on the read path | Benchmarks show tee adds <2% throughput |

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

| OS | Backend | Mechanism |
|---|---|---|
| Linux | Secret Service via D-Bus | `github.com/godbus/dbus` (pure Go) |
| macOS | Keychain | Shell-out to `/usr/bin/security find-generic-password` (pure Go; no Security.framework cgo) |
| Windows | Credential Manager | `golang.org/x/sys/windows` advapi32 `CredRead`/`CredWrite` (pure Go syscall) |

If a backend is unavailable (headless CI, minimal WSL), the resolver reports
`BLOCKED (reason: no keychain binding)` per AGENTS.md §4.5 — it never fakes success.

### 6.3 Storage exclusion guarantee

**Secrets never touch `modernc.org/sqlite`.** The schema (§8) contains no credential
columns by construction; a CI check greps migrations for forbidden column names
(`token`, `secret`, `password`, `authorization`, `apikey`, `credential`).

### 6.4 Redaction pipeline

Sensitive headers (`Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`,
`X-Api-Key`) and all vault-sourced values are replaced with `[REDACTED]` before any
output reaches stderr, the UI, or the SQLite store. Redaction is applied at the
single output chokepoint (`internal/secrets.Redact`), not per-call-site.

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
| `github.com/godbus/dbus` | Linux Secret Service transport | OS keychain access | G1 |
| `golang.org/x/term`, `golang.org/x/sys` | isatty / advapi32 credential access | OS-level syscalls | G1 |
| `github.com/BurntSushi/toml` | `pulse.toml` config parsing | No stdlib TOML | G1 |
| `github.com/coder/websocket` | WebSocket client (RFC 6455) | No stdlib WebSocket | G2 §2.1 |
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

```sql
collections(id TEXT PK, root_path TEXT, created_at INTEGER)
requests(id TEXT PK, collection_id TEXT FK, file_path TEXT, name TEXT,
         method TEXT, url_template TEXT, ordinal INTEGER, updated_at INTEGER)
history(id INTEGER PK AUTOINCREMENT, request_id TEXT FK, started_at INTEGER,
        duration_ms INTEGER, status_code INTEGER, request_bytes INTEGER,
        response_bytes INTEGER, sha256 TEXT, error TEXT, run_source TEXT)
```

- `run_source`: `tui` | `mcp:<client-name>` — provenance for audit.
- Bodies (request/response) are **not persisted by default**; history stores metadata
  and the response SHA-256 only. Body persistence is deferred pending a privacy ADL.
- `response_bytes`/`duration_ms` power the timing UI and `get-history` MCP tool.

## 9. MCP Server (`internal/mcp`)

- Library: `github.com/mark3labs/mcp-go` (sanctioned in §7).
- Transport: stdio, spec-compliant lifecycle (`initialize` → `initialized` →
  operations → `shutdown` → exit 0). No custom framing.
- Tools (kebab-case, spec-conformant):

| Tool | Params | Returns |
|---|---|---|
| `list-collections` | — | collection roots and file counts |
| `list-requests` | `collection?`, `filter?` | request refs (file, name, method, url template) |
| `get-request` | `ref` | full AST view of the request (secrets redacted) |
| `run-request` | `ref`, `env?`, `timeout_ms?` | status, timing, size, checksum, streamed body summary (secrets redacted) |
| `get-run` | `run_id` | status of an in-flight run (from the streaming engine) |
| `get-history` | `limit?` | recent executions with redaction applied |
| `explain-failure` | `run_id` \| `ref` | top-k indexed code chunks correlated to a 5xx (ROADMAP.md §3.2.4) |
| `fuzz-start` | `openapi_ref`, `strategy`, `budget` | fuzz session handle (ROADMAP.md §3.3.3) |
| `fuzz-next` | `session` | next generated payload (deterministic seed) |
| `fuzz-report` | `session` | deduplicated findings, redacted |

- All tool results pass through `secrets.Redact`. All logging goes to stderr only.
  A single byte on stdout outside the JSON-RPC layer is a release blocker
  (AGENTS.md §3.3).
- Call arguments are schema-validated before dispatch (jsonschema); validation
  failures return JSON-RPC `-32602` and never reach the handler (ROADMAP.md §3.1.3).

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
