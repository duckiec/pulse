# AGENTS.md — Pulse

**Repository:** `pulse`
**Status:** Open source, actively enforced by AI agents and human maintainers alike.

This file is the persistent, uncompromising instruction manual for every AI coding agent
(and every human contributor) operating on this codebase. When this document conflicts
with any default behavior, habit, or "best practice" you were trained on, **this document
wins.** If a directive here seems impossible to satisfy, stop and report the conflict —
do not silently deviate, and do not weaken the directive.

---

## 1. Project Purpose

Pulse is a strictly offline, Git-native, dual-mode API workbench and headless Model
Context Protocol (MCP) server.

It replaces cloud-locked API clients with two first-class interfaces over one engine:

1. **Interactive Mode** — a high-framerate terminal UI (TUI) for designing, executing,
   and inspecting HTTP requests against local `.http` files.
2. **MCP Mode** — a headless JSON-RPC-over-stdio server that lets local LLMs securely
   execute the same API tests through the Model Context Protocol standard.

**Product invariants:**

- **Offline-first.** Pulse never phones home. No telemetry, no update checks, no cloud
  sync. Everything works on an airplane.
- **Single binary.** One CGO-free executable must build and run on Linux, macOS, and
  Windows from the same source tree.
- **Git-native.** Collections are plain-text `.http` files under standard version
  control. Git is the database of record for request definitions.

- **Canonical upstream: `https://github.com/duckiec/pulse.git` on branch `main`.**
  This remote is the canonical database of record for Pulse's history. The seven
  Essential Vibe Coding Files at the repository root are governed by this
  repository's history, not by any local copy, working tree, or mirror. Agents
  MUST treat `origin/main` as the authority when the two disagree, and MUST NOT
  force-push it (`git push --force` / `--force-with-lease`) — rewriting shared
  history destroys the provenance the ADL ledger depends on. Recorded in
  MEMORY.md ADL-0015.
- **Dual-mode, strict separation.** The TUI and the MCP server share domain logic but
  never share transport channels. Mode is selected at startup and is immutable for the
  process lifetime.

---

## 2. Technical Stack (Late-2026 Baseline)

### 2.1 Language & Toolchain

- **Go 1.27+, strictly.** No compatibility shims for older versions.
- **Build constraint: `CGO_ENABLED=0` is mandatory** for all platforms and all CI jobs.
  Any change that reintroduces a CGO dependency is an automatic revert.
- The `go` toolchain is the only sanctioned build system. No Makefiles-as-source-of-truth
  beyond thin wrappers; no external build daemons.

### 2.2 Standard Library First (Go 1.27 primitives)

You MUST exhaust the Go 1.27 standard library before importing any dependency:

| Concern | Required API | Do NOT import |
|---|---|---|
| Routing | `net/http.ServeMux` (Go 1.22+ pattern syntax, incl. method + wildcard matching) | `gorilla/mux`, `chi`, `echo`, `gin` |
| Context | `context.WithoutCancel`, `context.WithTimeoutCause`, root cancellation | any context reimplementation |
| Iteration | `iter.Seq` / `iter.Seq2` for zero-allocation stream iteration | hand-rolled callback chains |
| HTTP | `net/http` `Request`, `ResponseWriter`, `httptest` | wrapper clients like `resty` |
| JSON | `encoding/json/v2` where stable, else `encoding/json` | reflection-free tags that need third-party codecs |
| Concurrency | `sync.Pool`, channels, `sync.WaitGroup`, `errgroup` | worker frameworks |
| Testing | `testing` (incl. `T.Context()`, `testing/synctest` for goroutine invariants) | bespoke test harnesses |

### 2.3 UI Engine — Charmbracelet (latest stable)

- **Bubble Tea** — strictly Elm architecture (`Model`, `Msg`, `Update`, `View`).
- **Lip Gloss** — all styling and truecolor rendering. No raw ANSI escape sequences.
- **Huh** — all interactive forms and input flows.
- **Bubbles** — viewport, list, textinput, spinner, and progress components as needed.

The render loop MUST remain 100% decoupled from I/O (see §3.2).

### 2.4 Storage — Zero-CGO SQLite

- **`modernc.org/sqlite` exclusively.** Pure-Go, CGO-free SQLite engine. This is the
  only acceptable SQLite driver.
- **`mattn/go-sqlite3` is permanently banned.**
- `database/sql` is the only sanctioned DB abstraction layer. No heavyweight ORMs;
  write explicit SQL.

### 2.5 Protocol — MCP

- Implement the **latest Model Context Protocol specification**: headless JSON-RPC 2.0
  over stdio.
- Follow the spec's lifecycle exactly: `initialize` → `initialized` → operation loop →
  `shutdown` → exit. No custom framing, no extensions that break spec-conformant clients.

### 2.6 State & Secrets

- **`.http` parsing** via a purpose-built, zero-allocation AST. The AST is the single
  source of truth for request definitions — never parse HTTP semantics ad hoc at
  execution time.
- **Secret management:** OS-native keychain bindings (Windows Credential Manager,
  macOS Keychain, Linux Secret Service via D-Bus) plus ephemeral in-memory `.env`
  loads. Keychain access must degrade to a reported failure, never to a fallback value.

### 2.7 I/O & Concurrency

- **`io.TeeReader` + Go channels + `sync.Pool`** is the mandated pattern for streaming
  heavy API response bodies: zero-blocking multiplexing, zero-allocation hot paths,
  chunked delivery to consumers.
- Response bodies MUST be streamed in bounded-size chunks. Loading an entire multi-
  gigabyte response into memory is a defect.

---

## 3. Architecture Rules

### 3.1 Layering

```
cmd/pulse/           main entry, mode selection (--tui | --mcp), process wiring only
internal/ast/        .http lexer, parser, zero-allocation AST (no I/O, no UI)
internal/engine/     request execution: transport, streaming, TeeReader multiplexing
internal/store/      modernc.org/sqlite persistence, migrations, history
internal/secrets/    keychain bindings, ephemeral env resolution, zero-trust vault
internal/rag/        codebase indexing: bounded walker, per-language chunking, embedder
internal/fuzz/       OpenAPI subset reader, deterministic boundary matrix, GCRA limiter, TAP 13
internal/mcp/        JSON-RPC server, spec lifecycle, stdio transport
internal/ui/         Bubble Tea models, Lip Gloss styling, Huh forms
internal/app/        COMPOSITION ROOT — the one package that names a concrete dependency
                     on another package's behalf; owns lifecycle and the app context
```

- Dependency flow is strictly one-directional: `ui` and `mcp` depend on `app`, which
  depends on `engine`, `ast`, `fuzz`, `rag`, `store`, `secrets`. No sibling package may
  import another sibling's internals.
- Domain logic (AST, engine, secrets, store, rag, fuzz) MUST be transport-agnostic and
  UI-agnostic. It is driven by both modes.
- **`internal/app` is the ONLY package permitted to name a concrete dependency on another
  package's behalf.** Every other package declares what it needs as an interface and is
  handed it. A package that constructs its own collaborators cannot be exercised without
  constructing all of them — that is a testability defect before it is an architectural
  one. Where a sibling's concrete type was previously named (the `secrets.Redactor` in
  `internal/mcp`), declare a local interface instead and let `app` prove the concrete type
  satisfies it.
- `cmd/pulse` wires modes and maps outcomes to exit codes. It MUST NOT open a store, read
  a keychain or build a domain service: that is `internal/app`'s work, and duplicating it
  in `main` is how the two drift.

### 3.2 Concurrency Invariants (UI Thread)

- **No network I/O, no disk reads, and no blocking syscalls on the Bubble Tea UI
  thread. Ever.**
- All heavy I/O executes in background goroutines launched from `tea.Cmd`.
- Streaming results are delivered to the UI as `tea.Msg` values — chunked, bounded,
  and rate-limited to the frame budget.
- Use `sync.Pool` for reusable buffers and chunk objects to prevent GC spikes under
  load.
- Long-running operations MUST respect context cancellation and
  `context.WithoutCancel` for teardown/cleanup work that must outlive the request
  context but not the process.

### 3.3 Transport Segregation (MCP Mode)

When Pulse runs in MCP mode:

- **stdout is the reserved JSON-RPC transport channel. Nothing else may write to it.**
- `fmt.Println`, `fmt.Printf`, any direct `os.Stdout` write, and any unstructured
  logging is **forbidden** in every code path reachable in MCP mode — including
  dependency-upgrade and initialization paths.
- All diagnostics go to **stderr**, structured and prefixed (e.g., `pulse: <message>`).
  Even stderr output should be minimal and non-blocking.
- Enforce this with a package-level guard: the logger in `internal/mcp` writes only to
  stderr, and `go vet`/lint rules flag any `os.Stdout` reference under `internal/mcp`
  or `internal/app` when compiled with the `mcp` build tag. Agents must run this check
  before declaring work complete.
- A stray stdout write in MCP mode is a protocol-corruption bug that crashes the
  connected AI client. Treat it as a release blocker.

### 3.4 Data & Secrets Handling

- `.http` files are plaintext and Git-committed by design — but secrets are NEVER
  committed. Requests reference variables; variables resolve at runtime from the
  keychain or ephemeral `.env` (loaded in memory only, never persisted).
- The local SQLite store caches request history and metadata only. It MUST NOT store
  credentials, tokens, or raw `.env` contents. If the store is deleted, the user loses
  convenience, never security.
- Redaction is mandatory in all log output: headers named `Authorization`, `Proxy-
  Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, and any value sourced from the
  secret vault are replaced with `[REDACTED]` before touching stderr, the UI, or the
  store.

---

## 4. Agent Coding Directives

These directives are binding. Each one is phrased so a reviewer (human or CI) can
mechanically verify compliance.

### 4.1 Modern Go Purity

1. If the Go 1.27+ standard library solves it, you MUST use the standard library. Do
   not import a third-party package for routing, context handling, iterators, HTTP,
   JSON, or concurrency primitives. See the table in §2.2.
2. Code MUST compile and pass `go build ./...` with `CGO_ENABLED=0`. If a proposed
   change requires CGO, it is rejected at design time.
3. No "legacy libraries." If you find yourself reaching for a dependency that predates
   a stdlib equivalent, the dependency is the bug.
4. Internal packages live under `internal/`. Exported API surface is minimal by
   default; widen it only when a second consumer justifies it.

### 4.2 Test Sensitivity — The Mutation Rule

**A code fix is invalid unless you can mathematically prove the regression test fails
without the fix.**

Operationalized:

1. For every bug fix, the fix MUST be accompanied by a test that fails on the pre-fix
   code and passes on the post-fix code.
2. Before declaring completion, you MUST demonstrate this by:
   a. Running the test against the original code and capturing the failure output, OR
   b. Identifying the precise assertion that the pre-fix code violates (exact output,
      error, or state), and showing the test's red output via `git stash` of the fix
      followed by the test run, OR
   c. Reasoning from a mutation: applying a single-line mutation to the fix
      (e.g., flipping a boundary condition, removing a bounds check) demonstrably
      turns the test red.
3. Attach the captured red-output evidence to your final report (log snippet or
   transcript). "Tests pass" alone is NOT evidence of fix validity.
4. Characterize tests for pre-existing behavior are equally mandatory: no PR may
   introduce untested logic, and every branch must be reachable by a test.

### 4.3 Zero-Trust Secrets

1. NEVER write credentials, tokens, or `.env` contents to disk in plaintext — not to
   the repo, not to `/tmp`, not to the SQLite store, not to crash dumps.
2. Variable resolution order: **OS keychain → ephemeral in-memory `.env` → error**.
   There is no fourth option.
3. If a secret cannot be resolved, fail closed (§4.5). Dummy tokens, empty strings, and
   `TODO` placeholders are hard failures, never fallbacks.
4. Never log, print, or echo a secret. Apply the redaction rules in §3.4.

### 4.4 Fail-Closed Error Handling

1. If `.http` AST parsing fails, halt the failing operation, surface a structured error
   (file, line, offset, expected token), and exit non-zero in batch/MCP contexts.
2. If a keychain lookup fails, halt and exit non-zero. Never fall back to dummy values.
3. Never silently swallow errors. No `_ = someFunc()` on any fallible call; no empty
   `catch` equivalents; no `if err != nil { /* continue */ }`.
4. Errors wrap with `%w` and preserve the original cause for callers to unwrap. Error
   messages state what failed and why — no `"something went wrong"` vagueness.
5. Exit codes are semantic: `0` success, `1` generic failure, `2` usage/parse error,
   `3` keychain/secret resolution failure. Document new codes where they are added.

### 4.5 Honest CI Reporting

1. A test that cannot run locally MUST be reported as **BLOCKED** or **NOT RUN** —
   never as a pass. Example: keychain integration tests in environments lacking OS
   keychain bindings (e.g., headless CI, minimal WSL without Secret Service).
2. Skipped tests MUST include a machine-readable reason: `SKIP (reason: ...)`,
   `BLOCKED (reason: ...)`, or `NOT RUN (reason: ...)`. Blanket `t.Skip()` without a
   reason is prohibited.
3. NEVER fake or greenwash a skipped test as a pass. Do not delete tests to make CI
   green; do not stub the keychain to make an integration test "pass" without real
   verification. If a test requires real OS services, it runs against them or reports
   BLOCKED.
4. Final reports MUST distinguish: **PASSED** (ran, green), **FAILED** (ran, red),
   **BLOCKED** (could not run — environment), **NOT RUN** (deliberately skipped —
   upstream/ordering). Any other disposition is a lie.

### 4.6 Documentation State Synchronization — The Essential Vibe Coding Files

Pulse is governed by seven living documents at the repository root, collectively the
**Essential Vibe Coding Files**:

| # | File | Role |
|---|---|---|
| 1 | `AGENTS.md` | Non-negotiable engineering directives for all agents |
| 2 | `PRD.md` | Product vision, audience, and scope boundaries |
| 3 | `ROADMAP.md` | Implementation blueprint: 4 goals exploded to X.X.X, exit-gated |
| 4 | `ARCHITECTURE.md` | Technical ecosystem, package map, data flow |
| 5 | `DESIGN_SYSTEM.md` | TUI aesthetics, keybindings, layout laws |
| 6 | `TASKS.md` | The chronological execution checklist |
| 7 | `MEMORY.md` | Append-only ledger of decisions and state changes |

No `README.md` or other root-level document is authorized. The seven files above are
the complete documentation surface.

**The synchronization mandate** — before any task is marked complete, the agent MUST
perform all of the following. Failure to update the documentation state invalidates
the code commit; a commit may be rejected by CI or a reviewer on documentation grounds
alone.

1. **`TASKS.md`** — Check off (change `[ ]` to `[x]`) every item this task completed.
   If a task was partially completed, split it or record the exact remainder; never
   check a box that is not 100% done.
2. **`MEMORY.md`** — Log every technical decision made during the task AND every
   state change (new package, schema change, dependency added/removed, directive
   amendment) using the file's mandated entry format, with a fresh ADL-nnnn ID.
3. **`ARCHITECTURE.md`** — Update immediately if the system topology shifted: new or
   removed packages, data-flow changes, storage schema changes, dependency list
   changes, concurrency-model changes.
4. **`DESIGN_SYSTEM.md`** — Update immediately if the UI changed: new components,
   layout rules, keybindings, palette tokens, or motion behavior.
5. **`PRD.md`** — Update if scope changed (feature added or explicitly out-of-scope
   item accepted). Scope changes require a MEMORY.md entry referencing the decision.
6. **`ROADMAP.md`** — The scope authority. Update if a goal's contracts, exit gates,
   or interface signatures move; every update requires a MEMORY.md ADL entry.
   `TASKS.md` items trace to roadmap micro-goals (X.X.X).
7. **`AGENTS.md`** — Never edited casually. If execution surfaces a necessary new
   directive, the agent MUST record the proposal in `MEMORY.md` and amend
   `AGENTS.md` in a dedicated documentation commit — never bundled mid-feature.

**Atomicity rule:** documentation updates ship in the SAME commit as the code they
describe. "Docs to follow" commits are prohibited.

### 4.7 Definition of Done (Agent Self-Check)

Before marking any task complete, an agent must be able to answer YES to all of the
following:

- [ ] `CGO_ENABLED=0 go build ./...` succeeds.
- [ ] `go vet ./...` clean; no unused dependencies (`go mod tidy` produces no diff).
- [ ] Stdlib-first check performed (§2.2 table honored).
- [ ] All new logic has tests; all fixes have red→green evidence per §4.2.
- [ ] No I/O on the UI thread; streaming paths use channels + `sync.Pool` (§3.2).
- [ ] In MCP mode, zero writes to stdout outside the JSON-RPC layer (§3.3).
- [ ] No plaintext secrets anywhere, on disk or in logs (§3.4, §4.3).
- [ ] All errors wrapped and surfaced; fail-closed on parse/secret failures (§4.4).
- [ ] Test report honestly categorizes every test PASSED/FAILED/BLOCKED/NOT RUN (§4.5).
- [ ] TASKS.md checkboxes updated for all completed items (§4.6).
- [ ] All decisions and state changes logged in MEMORY.md with ADL entries (§4.6).
- [ ] ARCHITECTURE.md / DESIGN_SYSTEM.md updated if topology or UI shifted (§4.6).
- [ ] ROADMAP.md contracts/exit gates updated if a goal's interfaces moved (§4.6).
- [ ] Documentation changes included in the same commit as the code (§4.6).

If any box cannot be checked, the task is NOT done — report the specific blocker rather
than shipping a green facade.

### 4.8 Always-On Ruleset: Ponytail Integration

**Ponytail** (`DietrichGebert/ponytail`, MIT) is installed at the project level
(`.opencode/plugins/` + `.opencode/command/`) and injects its ruleset into every
turn at the active level (default `full`; switch with `/ponytail [lite|full|ultra|off]`).
Its premise — the smallest change that fully solves the task, reuse before
writing, the standard library before dependencies — is *aligned* with this
document. It is permitted as the default implementation lens under one
non-negotiable precedence rule.

**Precedence.** Where Ponytail and this document conflict, **this document
wins** (preamble, restated here because the ruleset arrives as injected
context). Ponytail's own rule — "lazy, not negligent: trust-boundary
validation, data-loss handling, security, and accessibility are never on the
chopping block" — is extended by name with Pulse's gates, which are **never
on the chopping block**:

| Never-cut item | Section |
|---|---|
| `CGO_ENABLED=0` builds on every target | §2.1 |
| Zero-allocation invariants and their benchmark gates (G-alloc) | §2.7, §4.7 |
| The Mutation Rule and red→green evidence for every fix | §4.2 |
| Fail-closed error handling and semantic exit codes | §4.4 |
| Zero-trust secrets, redaction, no plaintext on disk | §3.4, §4.3 |
| Transport segregation (MCP stdout hygiene) | §3.3 |
| Documentation state synchronization (TASKS.md / MEMORY.md / ARCHITECTURE.md updates) | §4.6 |
| Honest test dispositions (PASSED / FAILED / BLOCKED / NOT RUN) | §4.5 |

**Rules of engagement:**

1. **Cut implementations, never gates.** A Ponytail finding of the form
   "delete this / simplify this" is actionable only when the target is not in
   the never-cut table. `/ponytail-review` and `/ponytail-audit` findings that
   touch a gate item are rejected at review.
2. **`shortcut:` comments defer implementation choices only.** A `shortcut:`
   marker must never defer a gate, a test, a redaction path, or a
   documentation update; such a marker is itself a §4.6/§4.7 violation.
3. **The gates are the floor, not the target.** Meeting every AGENTS.md
   obligation with the smallest sufficient code is the intended outcome —
   Ponytail's discipline applies *inside* the evidence, not instead of it.
4. **Level discipline.** Run `/ponytail off` when a task is gate-dominated
   (arena/allocator work, doc-sync batches); the choice is per-session and
   needs no record.
5. **Provenance.** The full ponytail checkout is vendored at
   `.opencode/ponytail/` (so the plugin's `../../hooks`, `../../skills`, and
   `../command` relative paths resolve inside the tree), and `opencode.json`
   registers it by **absolute path** (relative entries hit a doubled-UNC
   resolution bug on this WSL workspace; see MEMORY.md ADL-0014). Both are
   git-ignored — the repository stays the seven Essential Vibe Coding Files
   plus Pulse source, never vendored third-party instruction packs. Pinned
   upstream source and the install/verify scripts live at
   `~/.local/share/opencode-skill-src/`; restore procedure: MEMORY.md
   ADL-0013/ADL-0014. A plugin that loads syntax-clean can still fail at
   runtime: verify the require graph, not just `node --check`.

---

## 5. Enforcement

- CI runs the standard library-purity audit (banned-import list incl.
  `mattn/go-sqlite3`, `gorilla/mux`, and any stdout-writing analyzer in MCP paths),
  the CGO-free build matrix, the mutation-evidence gate on fixes, and the honest-test-
  disposition audit.
- CI runs the **documentation-sync gate**: a commit is rejected if code changes touch
  `internal/` or `cmd/` without a corresponding `TASKS.md` checkbox transition or a
  `MEMORY.md` entry (§4.6); a commit that moves a `ROADMAP.md` interface or exit gate
  without updating that document in the same commit is rejected identically.
- Reviewers (human or AI) may reject any change that violates this document without
  further justification required.
- Proposals to amend this document go through a dedicated discussion; they are not
  bundled with feature PRs.

---

## 6. Agent Skills (configuration, not directive)

**Sections 1–5 are the contract. Section 6 is configuration.** Nothing here is a gate.
A file under `.agents/` that contradicts §1–§5 is wrong, and §1–§5 wins; the correct
response to a conflict is to fix the file, not to weigh the two. Added by MEMORY.md
ADL-0026, which records why the sanctioned documentation surface in §4.6 was widened.

The engineering skills (`tdd`, `triage`, `to-spec`, `domain-modeling`, `code-review`,
`handoff`, and the rest) read their configuration from `.agents/`, not from a
tool-specific directory. `.opencode/` and `.freebuff/` are harnesses; `.agents/` is the
portable copy, and configuration written to a harness directory would not survive a
change of harness.

### Issue tracker

GitHub Issues on `duckiec/pulse`, via the `gh` CLI. Issues may *propose* work; they may
never substitute for the commit that carries it, because `origin/main` is the database of
record (ADL-0015). See `.agents/issue-tracker.md`.

### Triage labels

The five canonical roles, label string equal to role name: `needs-triage`, `needs-info`,
`ready-for-agent`, `ready-for-human`, `wontfix`. `ready-for-agent` is narrower here than
in most repos — it additionally implies the issue already names its gate, its exit
condition, and its mutation per §4.2. See `.agents/triage-labels.md`.

### Domain docs

Single-context. Vocabulary in `GLOSSARY.md`; decisions in the `ADL-nnnn` entries of
`MEMORY.md`, which is the append-only ledger and the single source of truth. There is
deliberately **no `docs/adr/` directory** — a second decision ledger in a repository that
already has one is a fork with no precedence rule. `MEMORY.md` is amended by appending,
never by editing a prior entry. See `.agents/domain.md`.
