# MEMORY.md — Pulse Decision & State Ledger

| Field | Value |
|---|---|
| Document | Append-Only Architectural Decision Log |
| Product | Pulse |
| Version | 1.0.0 |
| Status | Authoritative |
| Governed by | `AGENTS.md` §4.6 |
| Last updated | 2026-10-11 |

---

## Purpose

This file is the **running ledger of architectural decisions and state changes** for
Pulse. It exists so that any agent or human can reconstruct *why* the system looks
the way it does without archaeology through commits. It is append-only: entries are
never edited after acceptance (superseded entries are marked `SUPERSEDED by ADL-nnnn`).

## Mandatory Entry Format

Every decision or state change MUST be logged in exactly this format. IDs are
sequential and never reused. Agents MUST append an entry before marking a task
complete (AGENTS.md §4.7).

```markdown
### ADL-nnnn — <short imperative title>

- **Date:** YYYY-MM-DD
- **Status:** PROPOSED | ACCEPTED | SUPERSEDED by ADL-xxxx | DEFERRED
- **Context:** What forces exist? What problem/pressure triggered this?
- **Decision:** What was chosen — stated unambiguously.
- **Consequences:** What becomes easier, what becomes forbidden, what follow-ups exist.
- **Refs:** Files/PRs/issues affected (e.g., ARCHITECTURE.md §5, TASKS.md P3.3)
```

## State Change Log (append-only, newest at bottom)

| Date | ADL | Change | Affected files |
|---|---|---|---|
| 2026-10-11 | ADL-0006 | Repository initialized with the 6 Essential Vibe Coding Files | all root docs |
| 2026-10-11 | ADL-0008 | ROADMAP.md adopted; PRD §7 rows 5/6/8 superseded — SSE/WS, gRPC, Wasm, fuzzing now in scope | ROADMAP.md, PRD.md |
| 2026-10-11 | ADL-0009 | Documentation surface expanded from 6 to 7 Essential Vibe Coding Files (ROADMAP.md added) | AGENTS.md |
| 2026-10-11 | ADL-0010 | Move 0 bootstrap: `pulse` module path, GitHub Actions CI, toolchain bootstrap script, race-detector cgo exception | go.mod, .github/, scripts/ |
| 2026-10-11 | ADL-0011 | Move 1 lexer land: 32-byte Token, iter.Seq stream, lexState table, golden corpus discipline | internal/ast, testdata/ |
| 2026-10-11 | ADL-0013 | Ponytail always-on at project level; agent skills moved global→project; AGENTS.md §4.8 amendment | AGENTS.md, .opencode/ (ignored) |
| 2026-10-12 | ADL-0014 | Option A: ponytail plugin vendor fix — full checkout at `.opencode/ponytail/`, absolute plugins path, package.json shim | AGENTS.md, .gitignore |
| 2026-10-12 | ADL-0012 | Move 2 parser land: bump-arena memory strategy (8-byte alignment, retained blocks, GC-visible root) and the fail-closed `(nil, err)` parse contract | internal/ast/arena.go, internal/ast/parser.go, testdata/, scripts/ |
| 2026-10-12 | ADL-0015 | Canonical upstream established: `origin` = https://github.com/duckiec/pulse.git, branch `main`; local history rebased onto the upstream AGPL-3.0 initial commit (no force-push) | AGENTS.md §1, ARCHITECTURE.md header |
| 2026-10-12 | ADL-0016 | Move 3 engine land: 32 KiB × 64-slot streaming geometry, 2.0625 MiB in-flight ceiling, Sink ownership contract, abort-only teardown budget | internal/engine/, ROADMAP.md §1.2.1/§1.2.3 |
| 2026-10-12 | ADL-0017 | Move 4: modernc.org/sqlite as the sole driver, `PRAGMA secure_delete=ON` verified behaviourally, runtime `sqlite_master` schema audit, and the two-layer redaction chokepoint | internal/secrets/, internal/store/, internal/engine/stream.go |
| 2026-10-12 | ADL-0018 | Move 5: StreamManager lifecycle states, resume-token ownership, frame-aligned dispatch, and the httptrace→eBPF fallback degradation policy | internal/engine/sse.go, sse_parser.go, telemetry*.go |
| 2026-10-12 | ADL-0019 | Move 6: the ProtocolEngine abstraction, fully dynamic protobuf reflection (no generated types in the binary), and the streaming binary→JSON encoder | internal/engine/protocol.go, grpc.go, grpc_engine.go, reflect.go, reflection_wire.go, jsonstream.go |

---

## ADL-0001 — AGENTS.md established as the governing document

- **Date:** 2026-10-11
- **Status:** ACCEPTED
- **Context:** Pulse will be developed substantially by AI agents with heterogeneous
  defaults. Without a single uncompromising instruction manual, agent output drifts
  toward generic conventions (third-party helpers, swallowed errors, greenwashed
  tests). The project needs a persistent authority that overrides agent priors.
- **Decision:** `AGENTS.md` at the repository root is the highest-authority document
  in the project. It defines the agent coding directives, the Definition of Done, and
  the enforcement model. Where it conflicts with convention, AGENTS.md wins. It is
  amended only via dedicated documentation commits proposed through a MEMORY.md entry.
- **Consequences:** Every PR is reviewable against a mechanical checklist; agents have
  no latitude for "reasonable" deviation; amendments require explicit proposals.
- **Refs:** AGENTS.md; TASKS.md (progress log discipline)

## ADL-0002 — CGO banned; modernc.org/sqlite is the only SQLite engine

- **Date:** 2026-10-11
- **Status:** ACCEPTED
- **Context:** A single static binary that cross-compiles trivially (Linux/macOS/
  Windows from one checkout) is a product requirement (PRD NFR-1). The conventional
  `mattn/go-sqlite3` driver requires CGO and a host C toolchain, breaking
  cross-compilation and complicating distribution.
- **Decision:** `CGO_ENABLED=0` is mandatory for all builds and CI jobs.
  `modernc.org/sqlite` (pure-Go SQLite translator) is the sole sanctioned database
  engine. `mattn/go-sqlite3` is permanently banned; any dependency requiring CGO is
  rejected at review time.
- **Consequences:** Pure-Go discipline extends to all other OS integrations (keychain
  backends must use D-Bus, the macOS `security` CLI, or x/sys syscalls — see
  ADL-0007). SQLite performance is translated-C rather than native C — accepted for
  the local-history workload.
- **Refs:** AGENTS.md §2.1/§2.4; ARCHITECTURE.md §7/§8; TASKS.md P1.1.3/P1.1.4

## ADL-0003 — Go 1.27 is the hard language baseline

- **Date:** 2026-10-11
- **Status:** ACCEPTED
- **Context:** Modern Go provides primitives that eliminate whole categories of
  third-party dependencies and allocation bugs (`net/http.ServeMux` pattern routing,
  `context.WithoutCancel`, `iter.Seq`/`iter.Seq2`, `testing/synctest`). Supporting
  older versions forces either polyfills or legacy dependency sprawl.
- **Decision:** `go 1.27` is the minimum and target toolchain. No compatibility
  shims for older versions. Stdlib-first is mandatory: if the Go 1.27 standard
  library solves a concern, importing a third-party package for it is a defect.
- **Consequences:** The dependency surface stays tiny (see ARCHITECTURE.md §7
  allowlist); zero-allocation designs are enforceable via `iter.Seq` and pool
  patterns; CI tests on 1.27 only.
- **Refs:** AGENTS.md §2.1/§2.2/§4.1; ARCHITECTURE.md §4.3

## ADL-0004 — Dual-mode execution (TUI + MCP) is the locked architectural paradigm

- **Date:** 2026-10-11
- **Status:** ACCEPTED
- **Context:** Two consumers exist for the same domain engine: humans in a terminal
  and local LLM agents (Cursor, Claude) via the Model Context Protocol. Cloud API
  clients cannot serve the second audience because they require egress and cannot
  guarantee wire hygiene on stdio.
- **Decision:** Pulse ships exactly two immutable modes selected at process start:
  (A) TUI — Bubble Tea, 60 FPS, Elm architecture; (B) MCP — headless JSON-RPC 2.0
  over stdio, `pulse mcp` subcommand. Mode is decided before any stdout-capable
  initialization: explicit subcommand/flag wins; bare invocation routes by
  `isatty(stdout)`; non-TTY bare invocation fails closed (exit 2). stdout in MCP mode
  is a reserved channel; anything else writing to it is a release blocker. A third
  mode (batch/CLI, GUI) is out of scope for v1.
- **Consequences:** `internal/ui` and `internal/mcp` never coexist in a runtime; both
  consume `internal/app` only; stdout-hygiene analysis is a CI gate; agent clients
  (Cursor/Claude) integrate without bespoke adapters.
- **Refs:** AGENTS.md §1/§3.3; ARCHITECTURE.md §2/§9; TASKS.md P4.1/P4.2/P4.3

## ADL-0005 — The 6 Essential Vibe Coding Files are the complete documentation surface

- **Date:** 2026-10-11
- **Status:** ACCEPTED — superseded in part by ADL-0009 (surface count and file lists)
- **Context:** Long-running agent development degrades when documentation rots. A
  fixed, minimal set of living documents with mandated synchronization prevents drift
  without a heavyweight process. No README is authorized — the six files carry all
  onboarding and operational knowledge.
- **Decision:** The complete documentation surface is exactly: `AGENTS.md`,
  `PRD.md`, `ARCHITECTURE.md`, `DESIGN_SYSTEM.md`, `TASKS.md`, `MEMORY.md`. Agents
  must keep them synchronized: check off completed TASKS.md items, log every decision
  and state change here in MEMORY.md, update ARCHITECTURE.md on topology shifts and
  DESIGN_SYSTEM.md on UI shifts — all in the same commit as the code. A code commit
  without its documentation state update is invalid.
- **Consequences:** CI enforces a documentation-sync gate; "docs later" commits are
  prohibited; new root-level documents require an ADL entry; the agent's Definition
  of Done includes four documentation checkboxes.
- **Refs:** AGENTS.md §4.6/§4.7/§5; all six root docs

## ADL-0006 — Git-native `.http` format adopted; proprietary collection formats banned

- **Date:** 2026-10-11
- **Status:** ACCEPTED
- **Context:** The value loop is "test APIs without leaving the terminal and without
  uploading anything to the cloud." Proprietary JSON collection formats (Postman
  v2.x et al.) pull users toward vendor ecosystems, complicate diffs/review, and
  reintroduce cloud-shaped data models. The `.http` standard (JetBrains REST Client
  format) is plaintext, human-diffable, and Git-native.
- **Decision:** `.http` files are the sole collection format — the "database of
  record" for request definitions, parsed by a purpose-built zero-allocation AST
  (`internal/ast`) using Go 1.27 `iter.Seq` traversal. Importers/exporters for
  proprietary formats are explicitly out of scope (PRD.md §7). Variables resolve at
  runtime from the OS keychain or ephemeral in-memory `.env` — never from committed
  files.
- **Consequences:** Collections diff like code; the parser is the correctness
  bottleneck and carries the strictest tests (0 allocs/op, fail-closed diagnostics);
  secrets stay out of Git by construction.
- **Refs:** AGENTS.md §2.6/§3.4; ARCHITECTURE.md §4; PRD.md §7 item 4; TASKS.md P1.3

## ADL-0007 — Zero-CGO keychain strategy: D-Bus / `security` CLI / advapi32

- **Date:** 2026-10-11
- **Status:** ACCEPTED
- **Context:** ADL-0002 bans CGO, but secret management requires OS-native secure
  storage. The common Go keychain libraries reach for cgo bindings (Security.framework
  on macOS), which would violate the ban.
- **Decision:** Platform backends are implemented without CGO: Linux — Secret Service
  over D-Bus (`github.com/godbus/dbus`); macOS — shell-out to
  `/usr/bin/security find-generic-password`; Windows — Credential Manager via
  `golang.org/x/sys/windows` advapi32. Secrets never touch the SQLite schema (no
  credential columns exist, enforced by CI grep). Resolution is fail-closed; missing
  bindings are reported `BLOCKED (reason: ...)`, never faked (AGENTS.md §4.5).
- **Consequences:** Keychain integration tests require per-platform environments;
  environments lacking bindings report BLOCKED, not PASS; the resolution chain
  (keychain → ephemeral .env → hard error, exit 3) is uniform across platforms.
- **Refs:** AGENTS.md §2.6/§3.4/§4.3; ARCHITECTURE.md §6; TASKS.md P2.1/P2.4.2

## ADL-0008 — ROADMAP.md adopted; protocol, sandbox, and fuzzing scope reinstated

- **Date:** 2026-10-11
- **Status:** ACCEPTED
- **Context:** The initial PRD.md §7 excluded SSE/WebSocket transports, gRPC,
  Wasm-based extensibility, and auto-fuzzing to protect early execution focus. The
  architectural review for the "Protocol Supremacy" and "AI Orchestration" goals
  showed these capabilities are required for the product's two audiences (terminal
  purists hitting streaming endpoints; local agents needing payload generation), and
  that deferring them would force interface-breaking retrofits of the engine's
  `Stream` contract and the MCP tool surface. The roadmap needed a single,
  exit-gated blueprint instead of a calendar.
- **Decision:** `ROADMAP.md` is created as the implementation blueprint and the
  **scope authority** for Pulse. It contains four exit-gated goals (foundation,
  protocol supremacy, AI orchestration, zero-cloud collaboration), exploded to a
  maximum depth of X.X.X, with no time-based estimates. PRD.md §7 rows 5, 6, and 8
  are superseded: SSE/WebSockets, gRPC (dynamic reflection), sandboxed Wasm auth
  scripting (wazero), OpenAPI-driven auto-fuzzing, CRDT P2P sync, and the vault PKI
  are IN SCOPE. GraphQL remains excluded. The roadmap's §0.4 dependency registry is
  the single admission point for new packages, each requiring pure-Go status and a
  CGO-free build.
- **Consequences:** PRD.md §7 carries an amendment banner; FR-13..FR-21 added to the
  requirements table; v1.0.0 release criteria now include exit-gate evidence;
  `Stream`, `Engine`, and `Sink` interfaces are designed for extension (no retrofit).
  wazero's lack of fuel metering is an accepted, documented boundary (§3.4.4).
- **Refs:** ROADMAP.md (all); PRD.md §5/§7/§9; ARCHITECTURE.md §7 (dependency registry)

## ADL-0009 — Documentation surface expanded to seven Essential Vibe Coding Files

- **Date:** 2026-10-11
- **Status:** ACCEPTED
- **Context:** ADL-0005 fixed the documentation surface at six files and forbade
  other root-level documents. Adding ROADMAP.md (ADL-0008) without amending that rule
  would place the repo in violation of its own governance.
- **Decision:** The Essential Vibe Coding Files are now seven: `AGENTS.md`, `PRD.md`,
  `ROADMAP.md`, `ARCHITECTURE.md`, `DESIGN_SYSTEM.md`, `TASKS.md`, `MEMORY.md`.
  ROADMAP.md is a first-class sync target: interface/exit-gate changes require a
  same-commit doc update plus an ADL entry, and the Definition of Done gains a
  roadmap-sync checkbox. No README.md is authorized; the seven files remain the
  complete documentation surface.
- **Consequences:** AGENTS.md §4.6 table, mandate list, and §4.7 DoD updated; CI's
  documentation-sync gate extended to ROADMAP.md touching; new root documents still
  require an ADL.
- **Refs:** AGENTS.md §4.6/§4.7/§5; all seven root docs

## ADL-0010 — Move 0 bootstrap: `pulse` module path, GitHub Actions CI, toolchain provisioning

- **Date:** 2026-10-11
- **Status:** ACCEPTED
- **Context:** Execution began. The dev box shipped Go 1.26.8, violating the hard
  1.27 baseline (AGENTS.md §2.1); the module needed an import path; CI needed a
  home; and the race detector (`go test -race`) requires cgo instrumentation at
  test time, which sits in tension with the CGO-free build mandate.
- **Decision:**
  1. Module path is **`pulse`** (no host prefix). Adopting a hosted path
     (`github.com/<org>/pulse`) later is a mechanical rename recorded as an ADL;
     nothing in the architecture depends on the host prefix.
  2. CI runs on **GitHub Actions** (`.github/workflows/ci.yml`): `build-matrix`
     (4 targets, CGO_ENABLED=0), `audit` (tidy + banned-import), `doc-sync`
     (PRs only), `test`, and `race`. Every job uses go-version "1.27".
  3. `scripts/bootstrap-toolchain.sh` provisions the pinned **go1.27.2** into
     `~/sdk`, SHA-256-verified against go.dev. `scripts/ci/env.sh` prefers the
     pinned toolchain locally; CI uses setup-go.
  4. The **race detector runs in a separate job with CGO_ENABLED=1**. This is a
     test-time instrumentation exception only: the shipped binary remains
     CGO-free and `build-matrix` proves it on every target. No other cgo usage
     is permitted anywhere in the build.
- **Consequences:** Local `git` identity already configured (duckiec); the
  enforcement harness (`scripts/all-gates.sh`) runs build-matrix, tidy-check,
  banned-import-audit, doc-sync-gate, and the zero-allocation test gate as one
  command; the audit tool (`internal/devtools/importaudit`) is static and
  offline (parses go.mod + sources with go/parser, no network).
- **Refs:** go.mod; .github/workflows/ci.yml; scripts/ci/*; config/allowed-modules.txt; TASKS.md P1.1.5–P1.1.10

## ADL-0011 — Move 1 lexer: token contract, state machine, golden-corpus discipline

- **Date:** 2026-10-11
- **Status:** ACCEPTED
- **Context:** The lexer is the foundation every later goal parses through, and the
  zero-allocation claim is architectural, not aspirational (ROADMAP.md §1.1).
  The implementation had to make the token stream allocation-free, terminating,
  and byte-exact, with the grammar's edge cases pinned by tests rather than
  convention.
- **Decision:**
  1. `ast.Token` is exactly **32 bytes**: eight 4-byte fields (`Kind`,
     `Start`, `End`, `Line`, `Col`, `EndLine`, `EndCol`, `Depth`), no padding,
     asserted by `TestTokenSize`. Positions are offsets into the caller-owned
     source buffer; `Token.Text(src)` is the allocation-free accessor.
  2. The stream API is `Lexer.Tokens() iter.Seq[Token]` (Go 1.27), zero heap
     traffic per token — proven by `BenchmarkLexer` at 0 B/op / 0 allocs/op on
     a generated 10k-line corpus (~296 MB/s), enforced in CI.
  3. Grammar navigation is the explicit `lexState` table
     (`stLineStart → stRequestLine → stHeaders → stBody{JSON,XML,Raw} → stDone`),
     with quote/escape sub-states for JSON brace counting and tag/quoted/
     comment sub-states for XML depth tracking.
  4. Token granularity decisions: line terminators emit `TokNewline` after
     content and `TokBlankLine` for empty lines (the latter is the header/body
     delimiter); raw bodies emit a single `TokBodyOpen` carrying the full span;
     a self-closing XML root emits `TokBodyOpen` spanning `<tag` plus
     `TokBodyClose` spanning `/>` so spans tile without overlap.
  5. Failure is fail-closed by construction: the stream stops at the first
     `TokIllegal`; `Lexer.Diag()` returns a 16-byte value-type diagnostic
     (kind + span); no resync-continue, no partial AST. Every illegal token
     spans ≥1 byte (the only zero-width token is `TokEOF`).
  6. Interpolation anchors (`{{name}}`) are lexed as `TokInterp` inside
     single-line values only (targets, header values); body anchors are the
     parser's job from body spans — keeps the lexer's position arithmetic
     exact. This is a documented boundary, aligned with ROADMAP.md §1.1.7.
- **Consequences:** The corpus caught a real defect: `wantHTTPVersion` state
  leaked across requests (fixture 05 crashed the second request's
  interpolation); fixed by consuming the flag exactly once. Golden files
  (`testdata/http/golden/*.tokens`) are the grammar's executable spec,
  regenerated only via `-update` with an ADL when the grammar intentionally
  moves. Exit codes 2 and 3 are defined but not yet emitted (they bind at the
  parser/`pulse test` layers). `TestLexBehavior` pins 24 construct-level token
  sequences; `FuzzLexer` (22 seeds) proves termination and span sanity.
- **Refs:** internal/ast/{token.go,lexer.go}; internal/ast/*_test.go;
  testdata/http/**; TASKS.md P1.3.7–P1.3.11; ARCHITECTURE.md §3/§4

## ADL-0013 — Ponytail always-on at project level; agent skills relocated global→project

- **Date:** 2026-10-11
- **Status:** ACCEPTED
- **Context:** The maintainer requested Ponytail run always-on, and the previously
  installed agent skills were not visible because they had been placed in the
  global `~/.config/opencode/skills/` directory, which this OpenCode deployment
  does not auto-discover. Ponytail's injected ruleset ("the best code is the
  code you never wrote", `shortcut:` markers, aggressive cutting) can conflict
  with Pulse's evidence-heavy gates if left unreconciled.
- **Decision:**
  1. Skills live at **project level** `pulse/.opencode/skills/` (auto-discovered
     per OpenCode docs); the global skills directory was removed entirely.
     Installed set: 51 skills — ponytail's 6, mattpocock's 27 (engineering +
     productivity catalog; deprecated/in-progress/misc excluded), firecrawl's 18
     (core + build, **skills only, no MCP wiring** — no `FIRECRAWL_API_KEY`, no
     cloud MCP server registered; invoking them remains an explicit act).
  2. Ponytail runs **always-on** at project level via `.opencode/plugins/`
     (auto-loaded; pure Node built-ins, zero npm dependencies) plus its six
     slash commands in `.opencode/command/`. Level control: `/ponytail
     [lite|full|ultra|off]`, default `full`.
  3. **AGENTS.md §4.8** fixes precedence: where the injected ruleset and
     AGENTS.md conflict, AGENTS.md wins; the never-cut table names the CGO-free
     matrix, zero-alloc gates, Mutation Rule, fail-closed handling, zero-trust
     secrets, stdout hygiene, doc-sync, and honest dispositions as beyond the
     chopping block; `shortcut:` markers may defer implementation choices only,
     never a gate, test, or doc update.
  4. `.opencode/` is **git-ignored**: the Pulse repository carries source and the
     seven Essential Vibe Coding Files only, never vendored third-party
     instruction packs. This is reversible with one .gitignore line if the
     maintainer prefers committing the vendored set.
- **Consequences:** Agents in this repo now receive both AGENTS.md and the
  ponytail ruleset every turn; skills are visible at project level after a
  session restart. Update path: `~/.local/share/opencode-skill-src/`
  (`install-skills.sh`, `install-firecrawl.sh`, `move-to-project.sh`, pinned
  clones of all three repos). Firecrawl skills present but inert — running them
  is a cloud act and requires the user to supply credentials out-of-band.
- **Refs:** AGENTS.md §4.8; .gitignore; ~/.local/share/opencode-skill-src/*;
  TASKS.md (no execution-task change — environment tooling only)

## ADL-0014 — Option A: ponytail plugin vendor fix (silent load failure corrected)

- **Date:** 2026-10-12
- **Status:** ACCEPTED (supersedes the plugin-install half of ADL-0013)
- **Context:** The "always-on" plugin installed under ADL-0013 was **silently
  failing to load**. Two independent defects, both invisible to the syntax-only
  verification originally performed (`node --check`):
  1. **`require()` path resolution.** `ponytail.mjs` resolves its shared
     instruction/config builders via `require('../../hooks/ponytail-instructions')`
     and `require('../../hooks/ponytail-config')` — paths that only exist inside
     a *full* ponytail checkout. The partial copy at `.opencode/plugins/` had
     no `hooks/` sibling, so the plugin threw
     `MODULE_NOT_FOUND: Cannot find module '../../hooks/ponytail-instructions'`
     at load (log: `level=WARN message="failed to load plugin"` at
     2026-10-11T22:28:54 and 2026-10-12T00:27:13). OpenCode degrades silently —
     the session runs normally with no ruleset.
  2. **Doubled-UNC path resolution.** Moving to the vendored full checkout with
     a *relative* plugins entry (`".opencode/ponytail/.opencode/plugins"`)
     exposed a second failure: OpenCode's directory-plugin loader runs an
     `npm install` step and built the malformed path
     `\\wsl.localhost\FedoraLinux-44\FedoraLinux-44\home\duckie\pulse\...`
     (distro name doubled) → `ENOENT package.json`.
- **Decision (Option A):**
  1. The **full ponytail checkout** is vendored at `pulse/.opencode/ponytail/`
     (git-ignored), so every relative path inside the plugin — `../../hooks`,
     `../../skills`, `../command` — resolves inside the vendored tree.
  2. `pulse/opencode.json` (git-ignored) registers the plugin with an
     **absolute path** (`/home/duckie/pulse/.opencode/ponytail/.opencode/plugins`),
     which sidesteps the doubled-UNC relative-resolution bug.
  3. A minimal **`package.json` shim** (`main: index.js`, `type: module`,
     no dependencies) was added inside the vendored plugins dir because the
     directory-plugin loader performs an npm-install step requiring one; upstream
     omits it because npm consumers load the package root instead.
  4. The broken partial `.opencode/plugins/` copy was **deleted**, and the six
     project-level ponytail skill/command duplicates were **removed** — the
     plugin now registers its own skills and commands from the vendored tree
     (45 non-ponytail skills remain at `.opencode/skills/`).
- **Consequences:** Verified end-to-end, not by syntax check:
  (a) smoke test — module loads, context hook registers, 6 skills + 6 commands
  register, 2859-byte ruleset injected at default mode; (b) server log —
  `msg="loading plugin" id=/home/duckie/pulse/.opencode/ponytail/.opencode/plugins`
  with watchers on `index.js` and `ponytail.mjs`, and **zero** plugin-load
  failures after the fix; (c) functional probe — the live model reports
  "Ponytail is injected at **full** level". Lesson recorded: verify *runtime*
  require graphs, never just syntax. `opencode.json` is git-ignored alongside
  `.opencode/` because a committed config pointing into ignored vendored
  tooling would dangle for fresh clones.
- **Refs:** .opencode/ponytail/**; opencode.json; AGENTS.md §4.8; ADL-0013;
  ~/.local/share/opencode-skill-src/{vendor-option-a,finalize-option-a,run-smoke,log-evidence}.sh

---

## ADL-0012 — Arena memory-alignment strategy and the fail-closed parse contract

- **Date:** 2026-10-12
- **Status:** ACCEPTED
- **Context:** Move 2 had to build the AST (`Document`/`Request`/`Header`/`Body`/
  `Variable`/`Assert`) with zero heap traffic per parse and hold a 10,000-request
  document inside 2× its source size. The parser is fed an `iter.Seq[Token]`
  stream, must do LL(1) descent with exactly one token of lookahead in frame,
  and must resolve file-scoped `@var=value` declarations in a pre-pass. Three
  constraints collide: (a) node allocation must be free, (b) node strings must
  not duplicate the source, (c) the GC must never be allowed to reclaim memory
  the AST still points at.
- **Decision — arena memory strategy:**
  1. **Offset-based AST.** Every string is a `[start,end)` `int32` span into an
     arena-owned copy of the source (`Arena.Reset` copies once); accessors
     materialize zero-copy views via `unsafe.String` (`Document.Str`). String
     fields would have cost ~16 B each and blown the 2× ceiling; spans keep
     `Request` at 88 B and the whole 10k document at **1.50× source**
     (`TestMemoryCeiling`) with **−0.05× RSS growth** once warm
     (`TestMemoryCeilingRSS`).
  2. **8-byte alignment, exact-fit blocks.** `Arena.alloc` rounds every
     allocation up to 8 bytes — the alignment of every AST node type (all
     `int32`/`uint8` fields plus 8- or 16-byte slice headers). The single large
     array (`[]Request`, `[]Header`) gets its own **exact-fit** block instead of
     a geometric successor, so no allocation is split across blocks and no
     block is over-reserved (`TestArenaLargeAllocationExactFit`).
  3. **Blocks survive the pool round-trip.** `Arena.Release` clears only the
     parse state (current block, offset, cursor) and keeps `src` + retained
     blocks. Clearing them too was measurably wrong: `BenchmarkParseCached`
     reported **11 allocs/op, 4.88 MB/op** (source copy + ~1.7 MB of blocks
     rebuilt every cycle) and is now **0 allocs/op**. This is the mechanism
     behind the G-alloc gate.
  4. **The gate measures `allocs/op`, not an averaged byte count.** `B/op` is
     total-bytes/N, so a single post-GC `sync.Pool` refill — the pool is drained
     by the collector by design — averages to ~1 B/op over 2000 iterations
     while the real invariant (no allocation per operation) still holds.
     `scripts/ci/test-gates.sh` therefore requires `0 allocs/op` and allows
     ≤ 8 B/op; gating on `0 B/op` was observed to fail intermittently for a
     reason that has nothing to do with the parser.
  4. **The root `Document` is pooled on the Go heap, never arena-allocated.**
     It is the one node holding Go pointers (`Path`, the node slices, and the
     arena itself), and **the GC does not trace pointers stored inside arena
     blocks**. Arena-allocating it made the arena's own lifetime invisible to
     the collector: it could be reclaimed while a live `Document` still pointed
     at it, producing GC-timing-dependent memory corruption (observed as a
     segfault in `Arena.grow` only in the full suite, only when a prior test had
     called `runtime.GC()`). Pooling keeps zero allocations *and* keeps the
     arena graph reachable.
  5. **The pre-pass counts over the identical stream the fill pass consumes.**
     `countNodes` drives `Lexer.NextToken`, not `Tokens()`: `Tokens` stops at
     the first illegal token, so on a recoverable file it under-counted every
     request after the damage and the fill pass then wrote past its arrays.
- **Decision — fail-closed parse contract:**
  1. `Parse(arena) (*Document, error)` returns `(nil, err)` on **any** error.
     A partially filled AST is never handed out; callers that need rich
     diagnostics use `ParseDiag`, which resyncs at `###` and accumulates up to
     32 `SyntaxError`s (`Path`, `Kind`, `Offset`, `Line`, `Col`, `Expected`,
     `Found`) — it still returns no document, because a file with any
     diagnostic has no valid AST.
  2. **Internal invariant violations are not source diagnostics.** A
     count/fill disagreement (`internalErr`) names no file position because the
     source is not at fault; it halts the parse and is never recorded into the
     user-visible diagnostic list (`SyntaxError.internal`). Capacity checks on
     every node array fail closed rather than silently truncating the AST.
  3. **Errors are recorded exactly once**, at `parseDocument`. Recording inside
     `errorAt` as well duplicated every diagnostic — caught by the mutation
     proof and fixed.
- **Consequences:** `internal/ast` parses a 10,000-request AST at 0 allocs/op
  (23.5 ms/op, two lex passes over a 3.26 MB corpus — profiled, no parser
  hotspot). Golden AST snapshots (`testdata/http/golden/ast/*.ast`, compared
  with `cmp.Diff`) lock the node shapes. Writing the parser surfaced three
  latent defects the lexer-only tests could not: a spurious `TokNewline` emitted
  inside JSON/XML bodies (now baked out of the token goldens), a bodyless
  request swallowing its following `###` (which mis-anchored request-scoped
  `@var`/`# @assert` ranges), and var/assert scoping anchored at request
  creation rather than at the block separator.
- **Dependency note:** `github.com/google/go-cmp` is admitted to
  `config/allowed-modules.txt` as a **test-only** exception for `cmp.Diff`
  (ROADMAP.md §1.3.6 golden snapshots); it has no stdlib equivalent for
  structural diffs and never reaches the shipped binary.
- **Refs:** internal/ast/arena.go; internal/ast/parser.go;
  internal/ast/{arena,parser,parser_bench,memory_rss_linux}_test.go;
  testdata/http/golden/ast/; scripts/mutation-proof.sh;
  scripts/ci/test-gates.sh; TASKS.md P1.3.2–P1.3.16; AGENTS.md §4.2

---

## ADL-0015 — Canonical upstream: `https://github.com/duckiec/pulse.git`

- **Date:** 2026-10-12
- **Status:** ACCEPTED
- **Context:** Through Move 2, Pulse's history lived in a local WSL working tree
  only. AGENTS.md declared Git "the database of record" while no remote existed
  to back that claim, so the ADL ledger, the essential documentation surface, and
  the executable gates were single-machine and unrecoverable.
- **Decision:**
  1. **`origin` is `https://github.com/duckiec/pulse.git`, branch `main`.** It is
     the canonical database of record for Pulse's history; `origin/main` is the
     authority whenever it disagrees with a local working tree.
  2. **Force-push is prohibited** on this remote. The ADL ledger is an
     append-only provenance record; rewriting shared history silently destroys
     the evidence the Mutation Rule (§4.2) and this ledger depend on.
  3. **Non-destructive first integration.** The upstream already held one commit
     (`dfc9da3` "Initial commit", 661 lines) carrying a **GNU AGPL-3.0
     `LICENSE`**, authored by the repository owner, with **no common ancestor**
     with local history. A plain `git push` was correctly rejected as
     non-fast-forward. Force-pushing would have satisfied the directive in
     letter while destroying the owner's deliberate licensing decision, so the
     five local commits were **rebased onto `origin/main`** instead
     (`dfc9da3..3ee94a3`), preserving both the LICENSE and the full local
     history in a linear order. No `--force` was used.
  4. Recorded in **AGENTS.md §1** (product invariants) and the
     **ARCHITECTURE.md** header table (canonical upstream + license rows), so
     both the governance document and the technical document name the same
     authority.
- **Consequences:** Move 0–2 are on the canonical upstream and are
  `origin/main`-tracked. Post-rebase verification: `scripts/all-gates.sh` re-run
  green on the rebased tree before pushing (build-matrix 4/4, tidy no-op,
  importaudit, doc-sync, both benchmark gates) — rebase changed commit hashes
  only, never content. The local branch `backup-pre-rebase` was kept during the
  operation and deleted once `main` was verified identical in content.
- **Note (flagged, not decided):** the upstream license is **AGPL-3.0**, a
  strong network copyleft. That is a deliberate owner choice and is recorded
  here unchanged; it is recorded because AGPL's network-use clause has
  distribution consequences worth revisiting deliberately at release time — this
  entry does not change it.
- **Refs:** `git remote -v` (origin); AGENTS.md §1; ARCHITECTURE.md header;
  TASKS.md §0 bootstrap state

---

## ADL-0016 — The streaming geometry, the 2 MiB ceiling, and the Sink contract

- **Date:** 2026-10-12
- **Status:** ACCEPTED
- **Context:** Move 3 had to stream a 50 MB+ response without dropping a frame,
  without unbounded memory, and without ever blocking a caller or a UI thread.
  Three numbers decide all of it, and every other design choice follows from
  them.
- **Decision — the geometry (ROADMAP.md §1.2.3–1.2.4):**
  1. **`chunkSize = 32 << 10` (32 KiB).** Every pooled buffer is exactly one
     chunk; a read is `io.ReadFull` into that buffer, so *buffer count bounds
     bytes exactly* — no fractional tail, no size-class rounding.
  2. **`chunkChannelCap = 64`, giving a 2 MiB resident queue.** 64 × 32 KiB =
     2 097 152 B.
  3. **The true in-flight ceiling is `maxInFlightChunks = 66`, not 64.** A
     chunk is live from the moment the reader checks it out until the consumer
     recycles it, so the worst case is *64 queued + 1 being filled + 1 held by
     the dispatcher* = **2 162 688 B = 2.0625 MiB**. The honest number is
     recorded in the code as `inFlightBytes` and asserted by the gate; claiming
     a flat "2 MiB" would have been off by 64 KiB. 64 slots is the queue; 66 is
     the memory.
  4. **This is independent of payload size, and that is the claim the gate
     proves.** `TestMemoryCeilingInvariant` stalls the consumer completely (a
     `gatedSink` blocked on a channel, modelling a frozen UI) across 1 MB,
     10 MB, and 100 MB bodies, and asserts the in-flight count pins at exactly
     66 for the two payloads large enough to saturate — a **10× payload
     increase moves the peak by zero chunks**. The 1 MB case pins at 33 instead,
     because a 32-chunk body physically cannot fill a 64-slot queue; the test
     asserts that exact arithmetic rather than pretending it saturated.
- **Decision — the Sink contract:**
  1. **Chunk ownership is one-way:** engine → `Sink.Chunk` → `Chunk.Recycle()`
     → pool. The engine never reads a buffer after handing it off. A sink that
     wants the bytes past the call must copy them.
  2. **`Recycle` is idempotent.** A double recycle is *counted* and ignored
     rather than pushing the same buffer into the pool twice — which would hand
     one buffer to two concurrent readers, the silent corruption ROADMAP.md
     §1.2.3 warns about. Counting keeps it observable in tests without making a
     consumer bug crash the UI. A white-box test poisons recycled buffers to
     catch any use-after-recycle (`TestSinkOwnsEveryChunk`).
  3. **Exactly one terminal callback per run**, `Done` or `Failed`, never both,
     and never a chunk after it — both asserted.
  4. **Interface revised (AGENTS.md §4.6, ROADMAP.md is the scope authority).**
     `Run` returns `RunHandle`, **not** `(*RunHandle, error)`: every outcome
     arrives through the sink, so there is exactly one place to observe results
     and a caller cannot silently ignore a return value. ROADMAP.md §1.2.1 was
     updated in the same commit to match; §1.2.3's unused 8 KiB SSE constant was
     annotated as deferred to the SSE transport (Goal 2), its only consumer.
- **Decision — teardown budget bounds aborts only:**
  - Teardown runs on `context.WithoutCancel(ctx)` with its own 50 ms deadline,
    so a user interrupt mid-stream cannot abort cleanup.
  - **The 50 ms budget applies to the abort path only.** On success the engine
    waits for the dispatcher with *no* budget: every chunk already promised to
    the sink must be delivered, and how long a consumer takes is the consumer's
    business. Capping a successful drain would mean discarding delivered data —
    a direct violation of "stream without dropping a single frame". On abort the
    dispatcher recycles instead of delivering, so the unwind is the engine's own
    work and is genuinely bounded. (Measured: the original always-bounded version
    failed its own gate at 33–50 ms of pure sink drain time.)
- **Decision — interpolation fails closed:** with no `Resolver` bound, a
  `{{var}}` anywhere in target, header, or body aborts the run *before the
  request is issued* (`ErrUnresolved`). Sending a literal `{{var}}` to a real
  endpoint would silently corrupt the request. A resolver error is likewise
  fatal; there is no empty-string fallback (AGENTS.md §4.3).
- **Consequences:** `BenchmarkStreamSteadyState` reports **0 allocs/op** for a
  16 MiB body (512 chunks/op) at ~1.4 GB/s, gated in CI. The residual non-zero
  `B/op` is `sync.Pool`'s own internal queue growth (`poolChain.pushHead`),
  bounded and one-time — which is exactly why the gate decides on `allocs/op`
  rather than on an averaged byte count. The leak gate shows goroutines 6→3 and
  fds 11→9 across 8 cancelled runs. Mutation proof
  (`scripts/mutation-proof-engine.sh`): widening the queue to 100 000 spikes
  in-flight to **104 chunks (3 407 872 B)** at 10 MB and **201 chunks
  (6 586 368 B)** at 100 MB; shrinking the chunk to 4 KiB breaks the geometry
  assertion; dropping `chunkPool.Put` yields **1105 allocs/op**.
- **Refs:** internal/engine/engine.go; internal/engine/stream.go;
  internal/engine/engine_test.go; scripts/mutation-proof-engine.sh;
  scripts/ci/test-gates.sh; TASKS.md P1.4.1–P1.4.10; ROADMAP.md §1.2.1/§1.2.3

---

## ADL-0017 — Zero-trust storage, `secure_delete`, and the redaction chokepoint

- **Date:** 2026-10-12
- **Status:** ACCEPTED
- **Context:** Move 4 closed Phase 2. Two guarantees had to become mechanical
  rather than aspirational: *no secret ever reaches the database file*, and *no
  resolved secret ever reaches a Sink, a log, or a UI*. Both are easy to state
  and easy to erode, so each is enforced by code that fails loudly.
- **Decision — the SQLite driver:**
  - **`modernc.org/sqlite` is the only driver.** It is the sole implementation
    sanctioned by AGENTS.md §2.4 and ARCHITECTURE.md §7; `mattn/go-sqlite3` stays
    permanently banned because it requires cgo and the CGO_ENABLED=0 mandate is
    absolute. It brings `modernc.org/libc`, `mathutil`, and `memory` as
    transitive pure-Go dependencies, allowlisted explicitly so the import audit
    stays meaningful rather than being widened to a prefix that would admit
    anything.
- **Decision — `PRAGMA secure_delete=ON`:**
  - Set in the DSN **and verified at open** (`verifySecureDelete` reads the
    pragma back and fails if it is not 1). Trusting that a DSN was honoured is
    how a security control silently stops existing.
  - Verified **behaviourally**, not just by flag:
    `TestSecureDeleteZeroesDeletedRows` writes a marker, deletes the row,
    vacuums, then reads the raw database file and asserts the marker is gone.
    Without the pragma SQLite leaves deleted content in free cells, recoverable
    with a hex editor — so a flag assertion alone would have proved nothing.
- **Decision — the schema is the enforcement mechanism:**
  - The schema has **no column that could hold a header value or an auth body**:
    no header/auth/token/cookie column, and no request/response body column at
    all. History stores metadata and the response SHA-256 only.
  - `store.AuditSchema` runs on **every `Open`** (not only in tests), parsing the
    live `sqlite_master` DDL rather than a copy of the constant, so a future
    migration cannot quietly widen the schema without the tool refusing to
    start. Matching is **normalized** — lowercased with `_`, `-`, `.` removed —
    because `api_key` would otherwise slip past a rule that only knows `apikey`
    (this was a real gap, caught by the test's positive control).
  - `InsertHistory` refuses any row whose error text still contains a `{{...}}`
    anchor: a surviving anchor means an unresolved secret is one bug away from a
    secret.
- **Decision — the redaction chokepoint is two layers:**
  1. **Name-based:** the values of `Authorization`, `Proxy-Authorization`,
     `Cookie`, `Set-Cookie`, `X-Api-Key`, `X-Auth-Token`, `X-Session-Token`,
     `X-Csrf-Token`, `Proxy-Authorization-Basic`, `WWW-Authenticate` are always
     replaced.
  2. **Value-based:** every secret is registered with the redactor **the instant
     it is resolved, before it is returned**, and is then scrubbed wherever it
     turns up — a JSON error body, a URL, an error string. Name-based rules alone
     do not help when a server echoes the token back into a 200 response.
  - **Injection, not import.** `internal/engine` defines a `Redactor` interface
    and `internal/app` supplies `secrets.Redactor`. ARCHITECTURE.md §3.1 keeps
    domain packages as siblings; importing secrets into the engine would have
    created a sibling dependency. The engine uses it at three points: response
    headers, response **body chunks** (in place, inside the dispatch goroutine,
    before the Sink sees them), and errors.
  - **Outbound requests are deliberately NOT scrubbed.** The real secret must
    reach the server; redacting it would send `[REDACTED]` as the credential and
    break authentication while looking correct in every other test.
    `TestOutboundSecretStillReachesTheServer` guards exactly this.
- **Decision — the tracked-secret list is memory-bound by dropping, not evicting:**
  Capped at 256 entries, longest-first so overlapping secrets scrub whole. On
  overflow new registrations are **dropped and counted**, never evicted: evicting
  could remove the secret currently in use and let it leak. Values under 4 bytes
  are never tracked (scrubbing "ab" would corrupt ordinary text). Values shorter
  than the 10-byte placeholder cannot be replaced in the fixed-size streaming
  buffer, so the in-place path **skips and counts** them rather than running off
  the end; the allocating paths still cover them.
- **Consequences:** 26 secrets/store tests plus 4 engine redaction tests. The
  keychain dispositions are stated honestly: **linux/amd64 BLOCKED** (this WSL
  host has a session bus but no Secret Service daemon — the test emits
  `BLOCKED: secret-service-dbus backend unavailable...` and skips); **darwin and
  windows NOT RUN** (no host; both backends cross-compile CGO-free, which the
  4-target build matrix proves). No test fakes a pass. Mutation proof
  (`scripts/mutation-proof-secrets.sh`): stopping the allocating redactor leaks
  `AKIA-EXAMPLE-SECRET-9f3a2b` to the sink; stopping the in-place streaming scrub
  leaks `sk-live-EXAMPLE-4f7b2c9d-secret` in a response body; making the chain
  return no error breaks fail-closed; breaking the column rule breaks the audit;
  dropping `secure_delete` fails at `Open` with "deleted rows would survive in
  the raw file".
- **Goal 1 status:** closed out, with **G-frame explicitly carried forward** —
  it is a Bubble Tea UI property and cannot be measured before Phase 3 exists.
  Claiming it would be exactly the kind of dishonest green this repository
  forbids.
- **Refs:** internal/secrets/{resolver,redact,backend_linux,backend_darwin,
  backend_windows,backend_other}.go; internal/store/sqlite.go;
  internal/engine/{engine,stream}.go; scripts/mutation-proof-secrets.sh;
  TASKS.md P2.1.1–P2.4.4 and the Goal 1 status note; ROADMAP.md §1.4.1

---

## ADL-0018 — Stream lifecycle states, resume-token ownership, and telemetry degradation

- **Date:** 2026-10-12
- **Status:** ACCEPTED
- **Context:** Move 5 opened Goal 2 with two capabilities that must not damage
  the contracts Goal 1 already proved: a **persistent** transport that
  reconnects forever, and microsecond telemetry. Both add failure modes that a
  one-shot request/response never had — a stream that outlives its connection,
  and instrumentation that must never be the reason a request fails.
- **Decision — the connection lifecycle (ROADMAP.md §2.1.2/§2.1.6):**
  - **States:** `Idle → Connecting → Streaming ⇄ Connecting (reconnect) →
    Closed`. Exposed on the `StreamManager` interface, with `Attempts()` and
    `LastEventID()` observable and `Close()` idempotent. The manager owns the
    state machine; the UI observes it and never drives it ("backoff state lives
    in the run handle, not in the model").
  - **The resume token lives on the MANAGER, not on the parser.** The parser is
    rebuilt for every connection attempt; the token must not be. Ownership is the
    whole design: `m.setID(ev.ID)` writes the manager's token, and reconnect
    reads it. Wiping it on disconnect is the mutation the reconnect gate exists
    to catch, and it is caught (`Last-Event-ID on reconnect = "", want "evt-2"`).
  - **Idle expiry maps to the documented `ErrIdle`.** An `os.ErrDeadlineExceeded`
    from the read deadline is translated to `ErrIdle` before it leaves the pump.
    Without that mapping a silent server surfaced as an opaque transport error
    and was retried for the *wrong reason* — the retry worked by accident rather
    than by contract. `ErrIdle` is retryable; oversize framing and a non-2xx
    status are terminal (retrying a refused endpoint only hammers it).
  - **Full-jitter backoff** 500 ms → 30 s, doubling per attempt.
- **Decision — frame-aligned dispatch (ROADMAP.md §2.1.5):**
  - Events are coalesced into the existing 32 KiB pooled-chunk channel and
    dispatched at most once per 16 ms frame. The 2 MiB in-flight ceiling from
    ADL-0016 is unchanged — SSE reuses the same pool and the same channel
    discipline rather than introducing a second buffering regime.
  - Frames are **re-serialized** from parsed events (`id:`/`event:`/`data:`), so
    a coalesced frame is faithful in content while being canonical in spelling.
- **Decision — telemetry degradation (ROADMAP.md §2.3):**
  - `httptrace` phases are recorded on Go's monotonic clock into a pre-allocated
    512-point ring; overflow drops the oldest point rather than growing, because
    telemetry must never become the allocation that breaks a request.
  - **The eBPF path is an architecture, not a dependency.** `cilium/ebpf` is NOT
    added: the default build must not require clang/bpftool, a kernel
    capability, or a privilege the user may not have (ARCHITECTURE.md §7 keeps
    it out of the default build). The default build reports a disposition:
    `available` / `blocked` / `not-built`, **always with a reason**. On Linux the
    reason is derived from `/proc/sys/kernel/unprivileged_bpf_disabled` and
    `CapEff` (CAP_BPF bit 39, CAP_SYS_ADMIN bit 21); elsewhere it is "eBPF is a
    Linux facility".
  - **Degradation is explicit and the request never fails because of it.**
    `ProbeEBPF()` is a cached, side-effect-free probe; `Load()` returns a
    disposition rather than a fatal error. A BLOCKED run falls back to the
    httptrace timings *and says so* — never a silent fallback masquerading as
    precision (AGENTS.md §4.5).
  - **Reports are read through `Snapshot()`.** Trace hooks fire on
    runtime-internal goroutines, so a consumer reading `Total` while the
    transport finalizes the report is a data race (found by `-race`, fixed by
    making the report copy-out, not by avoiding concurrent reads).
- **Consequences:** reconnect gate green — the client reconnects after a
  mid-stream drop and the server observes `Last-Event-ID: evt-2`; the resume
  token also survives a parser rebuild; frame coalescing proven by dispatching
  far fewer chunks than events (mutation shows 1849 chunks for 500 events when
  the gate is bypassed). Defects the gates caught: the parser sliced
  `p.buf[:i]` instead of `p.buf[start:i]`, so every line was parsed from the
  start of the buffer and each field value swallowed the lines before it (which
  produced a malformed `Last-Event-ID` and an unparseable header); the parser
  dispatched on every line instead of on a blank line; and `ErrIdle` was
  documented but never produced.
- **Refs:** internal/engine/sse.go; internal/engine/sse_parser.go;
  internal/engine/telemetry.go; internal/engine/telemetry_ebpf{,_linux,_other}.go;
  internal/engine/stream_test.go; internal/engine/telemetry_test.go;
  scripts/mutation-proof-stream.sh; TASKS.md P3.0.1–P3.0.10; ROADMAP.md §2.1/§2.3

---

## ADL-0019 — ProtocolEngine abstraction, fully dynamic reflection, and a streaming binary→JSON encoder

- **Date:** 2026-10-12
- **Status:** ACCEPTED
- **Context:** Goal 2 required two things that pull against each other.
  ROADMAP.md §2.2 asked for gRPC support while §2.1 had just unified every wire
  behind one `Stream` interface — a tempting place to grow a second, gRPC-shaped
  pipeline. And the move directive required that Pulse be **completely
  dynamic**: no statically compiled `.pb.go` stubs. A reflection client is
  itself a gRPC method, so the obvious implementation would import grpc's
  generated reflection client — quietly reintroducing exactly what was banned.
- **Decision — the abstraction (`protocol.go`, `grpc.go`):**
  - **`ProtocolEngine` is `Protocol`/`Run`/`Close`, and nothing more.** Both
    executors take the same `*ast.Request` and deliver to the same `Sink`.
    "Protocol-agnostic" is enforced mechanically: compile-time assertions pin
    both executors to the interface, and `ProtocolRouter` dispatches on the
    TARGET SCHEME (`grpc://`, `grpcs://`) rather than on anything a user
    selected. A REST-only build is a legitimate configuration — the router then
    reports `ErrUnsupportedMethod` for a gRPC target instead of handing a
    `grpc://` string to an HTTP parser.
  - **`GRPCConfig.JSON` is a `*JSONOptions`, not a value.** `JSONOptions{}` with
    `StrictUnknown=false` is a real configuration; collapsing it into the zero
    value silently converted a user's request for leniency into strictness. A
    test now pins both directions.
- **Decision — dynamic reflection (`reflection_wire.go`, `reflect.go`):**
  - **The reflection client's descriptor is constructed by hand, field for
    field.** This is the only way to call the reflection service without a
    generated stub, and it is also the sharpest edge in the codebase: a
    hand-built descriptor has no compiler to catch a mistake.
  - **v1 and v1alpha use DIFFERENT field numbers.** The descriptor was first
    written with v1alpha's numbering (`file_containing_symbol = 2`) under the v1
    package name. The server decoded field 2 as `host`, found no oneof case set,
    and answered `invalid MessageRequest: <nil>` — an error naming neither the
    field nor the cause, traceable to nothing. Fixed to the v1 numbering
    (`host = 1`, oneof fields 3–7) and pinned field-by-field by
    `TestReflectionDescriptorMatchesTheWireProtocol`.
  - **Descriptors are requested by SERVICE and the transitive closure is
    assembled locally**, because protojson cannot resolve a message whose file
    imports others and a single-file answer fails later as an unrelated
    "unknown type" error. A missing import is reported **by name**, up front.
  - **`DescriptorCache` is keyed by (target, symbol) and strictly LRU.** The
    key includes the target because the same symbol means different things on
    different servers, and a cross-server cache hit would be a silent
    correctness bug rather than a visible one.
  - **Reflection is attempted first, local descriptors second, and failure is
    explicit.** Pulse never fetches a schema from anywhere but the server the
    user asked about.
- **Decision — JSON ↔ protobuf mapping (`reflect.go`):**
  - **Strict by default, with a structured error.** `*FieldTypeError` carries the
    dotted field path (`filter.pageSize`, `tags[2].pageSize`), the expected
    kind, and the JSON type that arrived. protojson rejects the same payloads;
    what it cannot supply is a machine-readable path for a UI to point at.
  - **protojson's 64-bit-as-string rule is honoured exactly.** A quoted number is
    legal for int64/uint64/sint64/fixed64/sfixed64 and rejected for 32-bit
    kinds, with an error that says WHY — otherwise `"limit": "3"` looks like an
    arbitrary rule rather than a consequence of protobuf's number model.
- **Decision — the binary→JSON direction streams (`jsonstream.go`):**
  - **Marshalling the whole message and then chopping it into chunks was
    rejected.** That measures nothing: the 2 MiB ceiling would bind on the
    channel while the real allocation happened a moment earlier and unmeasured,
    so a 100 MB message would drive the heap by 100 MB no matter how carefully
    the transport is bounded. The encoder writes through a fixed 32 KiB scratch
    buffer, and `TestJSONStreamDoesNotMaterializeTheMessage` asserts the
    high-water mark directly.
  - **Byte-equality with protojson is explicitly NOT a contract.** protojson
    emits unstable whitespace by design; the streaming encoder emits compact
    JSON with identical values, field names, and ordering. Tests compare decoded
    values, never bytes.
  - **Well-known types delegate to protojson.** Timestamp, Duration, Struct and
    the wrapper types have bespoke JSON forms; reimplementing them would buy
    nothing and risk being approximately rather than exactly right.
  - **String quoting is hand-written because `strconv.AppendQuote` emits Go
    escapes (`\x00`) that are not valid JSON.** Invalid UTF-8 becomes U+FFFD so
    the document stays decodable.
- **Consequences:** 25 gRPC/reflection gates green, `-race` clean, five mutation
  boundaries red→green. The reflection gate runs against a `grpc.NewServer`
  whose schema is built at runtime — if a generated stub were load-bearing, that
  server would not be callable at all. Client-streaming and bidi are refused
  before dialing (`ErrUnsupportedMethod`): half-supporting them is the failure
  mode ROADMAP.md §2.2.1 named.
- **Refs:** internal/engine/protocol.go; internal/engine/grpc.go (router);
  internal/engine/grpc_engine.go; internal/engine/reflect.go;
  internal/engine/reflection_wire.go; internal/engine/jsonstream.go;
  internal/engine/dynamic.go; internal/engine/grpc_test.go;
  internal/engine/jsonstream_test.go; scripts/mutation-proof-grpc.sh;
  TASKS.md P3.0b.1–P3.0b.12; ROADMAP.md §2.2

### ADL-0020 — The MCP bridge: two-layer stdout isolation, schema-first tool dispatch, and the `execute_request` naming deviation

- **Date:** 2026-10-12
- **Status:** ACCEPTED
- **Context:** Phase 4 (§4.2, §4.3) of TASKS.md and ROADMAP.md §3.1. The MCP
  server had to exist before anything else could talk to an agent, and its
  defining property is negative: **it must be impossible for Pulse itself to
  corrupt the JSON-RPC wire.** A stray `fmt.Println` from anywhere in the
  process inserts a non-JSON line into a stream the client parses as JSON-RPC
  2.0, and the client's parser — not ours — decides what to do about it. Most
  clients die. So the question was never "where do we avoid printing"; it is
  "what makes a print impossible rather than merely discouraged".
- **Decision — stdout isolation is two layers, not one:**
  - **Layer 1, production: swap `os.Stdout` to `/dev/null`.** `mcp.Lock(audit)`
    captures the real channel as `StdoutLock.Real`, then points `os.Stdout` at
    the null device. Every `fmt.Print*`, every `log.Printf`, every dependency
    that captures `os.Stdout` into a package-level var at init lands in a
    device that costs one `write(2)` and discards. The wire is unreachable from
    the rest of the program *by construction*, not by review discipline.
  - **Layer 2, audit: swap `os.Stdout` to a `Tripwire`.** Under
    `PULSE_STDOUT_AUDIT=1` the same swap installs a newline-buffered frame judge
    instead of `/dev/null`. It forwards every write into the wire **and** checks
    it: a complete line that is not a valid JSON-RPC 2.0 frame (`jsonrpc:"2.0"`,
    an id of type string/number/null, and a `method`, `result`, or `error` key)
    is protocol corruption and panics to stderr. Because MCP stdio is
    newline-delimited JSON and *not* Content-Length framing, the judge buffers
    to the next newline and therefore tolerates a frame split across writes.
  - **The audit layer is not a debug convenience; it is what makes layer 1
    auditable.** A swap to `/dev/null` is silent by construction, so "we wrote
    nothing to the wire" is unfalsifiable by inspection. The tripwire turns the
    same claim into a test that fails loudly, which is what
    `TestMCPStdoutAuditTripwire` and `TestStdoutGuardTripsOnRoguePrint` use.
- **Decision — the guard runs first, and the boot is split in two:**
  - `mcp.Lock()` is the *first* call in `cmd/pulse/main.go`'s MCP path, before the
    `mcp` package or anything it imports is *used*. Ordering matters: a package
    that runs `var out = os.Stdout` in its `init` has already captured the real
    channel by the time any Pulse code runs, and swapping the variable afterwards
    cannot reach it. Running the guard first is the best that can be done from
    inside the process; the tripwire is the backstop for the residual case.
  - Consequently the API is `Lock() (StdoutLock, error)` then
    `ServeLocked(ctx, lock.Real, os.Stdin)`, and `ServeLocked` returns
    `(exitCode int, err error)`. It does not capture `os.Stdout` itself — that
    would be the very indirection the guard removes. The type is `StdoutLock`
    rather than `Lock` because Go forbids a package-level type and function from
    sharing an identifier.
  - Clean stdin EOF **and** `context.Canceled` both map to exit 0. A client that
    closes the pipe is a normal shutdown, not a failure, and treating it as one
    would make every harness wrapper look like a crash.
- **Decision — error taxonomy: standard codes plus a reserved domain block.**
  Protocol-level failures keep the JSON-RPC meanings (`-32700` parse, `-32600`
  invalid request, `-32601` unknown method, `-32602` invalid params, `-32603`
  internal). Domain failures map into `-32002…-32006` — parse, unresolved secret,
  timeout, transport, unsupported — so an agent can branch on the code without
  pattern-matching English prose. Each carries a stable `category` string in
  `data` (`arguments`/`parse`/`secret`/`timeout`/`transport`/`unsupported`/
  `internal`); the category is part of the contract and the message text is not.
  `ProtocolError.payload()` **includes the cause**, so a schema failure carries
  the parser's `:1:1:` line:col and is actionable rather than merely
  categorical.
- **Decision — schema-first validation with RECORDED, opt-in coercion:**
  - The tool schema is compiled once at registration. Unknown keys are rejected
    (`additionalProperties: false`) — an agent's typo must be an error, not a
    silently ignored field, because a silently ignored field is how an agent
    concludes its request was honoured.
  - MCP clients send loosely-typed JSON. Coercion (`index` and `timeout_ms` as
    integer-or-string, `include_body` as boolean-or-string) is therefore
    **per-field, opt-in, and recorded**: every conversion is appended to a
    warning list that travels back in the tool result. Coercion that is not
    reported is coercion the caller cannot see, which is worse than a rejection.
  - "Exactly one of `payload`/`path`" is enforced **semantically**, not in the
    schema, because a JSON Schema cannot express "one of two mutually exclusive
    keys" without leaving one of the three cases unstated to the client.
- **Decision — `MaxResponseBytes = 256 KiB`, and truncation is explicit.** The
  accumulated body is bounded because it is destined for a model's context
  window; an unbounded body is a denial-of-service aimed at the agent, not the
  server. Truncation sets `truncated: true`, reports the **true** `body_bytes`
  total (not the truncated count, which would be a lie), and carries warning
  text. `TestResponseTruncationIsExplicit` uses a 400 000-byte fixture — an earlier 200 KB
  fixture sat *under* the ceiling and proved nothing, which is how a gate can
  be green and meaningless.
- **Decision — a failure with no HTTP status is reported as the error, not as a
  zero-status result.** Transport death, timeout, and secret failure happened
  before any status existed. Synthesising `status: 0` would present "no response"
  as "a response, whose status is zero", which is exactly the kind of plausible-
  looking wrong answer an agent will then reason about. When a partial body
  exists it is attached to the error's `data`, so nothing is lost.
- **Decision — `Recorder`, not `RecoverInto`.** The panic-recovery helper
  originally appended to a shared slice with no synchronization; `-race` flagged
  it as a genuine data race once two concurrent tool calls were in flight.
  Replacing it with a mutex-guarded `Recorder` is the fix, and the race
  detector — not inspection — is what found it.
- **Decision — the tool is named `execute_request`, not `run-request`.** This is
  a deliberate deviation from ROADMAP.md §3.1.3's illustrative kebab-case name.
  MCP convention across the ecosystem is `snake_case`, clients' generated SDK
  bindings assume it, and a one-off hyphenated name in a registry that will hold
  `list_collections`, `get_history`, `fuzz_start`, … is a permanent wart. The
  roadmap's name was illustrative; the naming *convention* it illustrated is not
  authoritative. All other planned tools follow snake_case.
- **Decision — the G-hyg analyzer exempts exactly two things, and both are
  principled:**
  1. `internal/mcp/stdio.go` — the guard file. It is the boundary; flagging it
     would be flagging the thing that enforces the rule. Every other file in
     `internal/mcp` remains fully subject to the rule.
  2. `_test.go` files under the MCP paths. A test that proves the isolation
     holds *must* be able to swap `os.Stdout` for a pipe and observe what
     crossed it; holding tests to the production rule would forbid writing the
     gate at all. Test files are also not linked into the shipped binary, so a
     print in one cannot reach a client's wire.
- **Consequences:** 24 gates green in `internal/mcp` and `cmd/pulse`; `-race`
  clean; six mutation boundaries red→green (`scripts/mutation-proof-mcp.sh`,
  M19–M24); the `importaudit` G-hyg analyzer passes with the two exemptions.
  The dependency allowlist gains `mark3labs/mcp-go v1.2.1` and
  `santhosh-tekuri/jsonschema/v6 v6.0.3` (both pure Go, CGO-free) plus their
  transitives. **Open gap, deliberately not papered over:** `DefaultExecutor` is
  constructed with a `nil` `secrets.Redactor`, because no composition root
  (`internal/app`) exists yet. The redaction *chokepoint* is implemented and
  tested in `collectingSink.outcome`, but in MCP mode today redaction is by
  header name only — vault-sourced values are not yet scrubbed from bodies. This
  is tracked as an open TASKS.md item and is **not** claimed as done.
  A pre-existing flake in `TestReconnectReplaysLastEventID` (engine, Move 5) was
  found and fixed here: it closed the stream *before* waiting for the resumed
  payload, and `Close()` drops a buffered frame rather than dispatching it. 8/8
  clean after the fix.
- **Refs:** internal/mcp/stdio.go; internal/mcp/errors.go; internal/mcp/validate.go;
  internal/mcp/tools.go; internal/mcp/server.go; internal/mcp/server_test.go;
  cmd/pulse/main.go; cmd/pulse/main_test.go;
  internal/devtools/importaudit/main.go; internal/engine/stream_test.go;
  scripts/mutation-proof-mcp.sh; config/allowed-modules.txt;
  TASKS.md P4.2.1–P4.2.3, P4.3.1, P4.3.5; ROADMAP.md §3.1

---

### ADL-0021 — Local RAG: FTS5 + brute-force cosine fused by RRF, and the chunking heuristics

- **Date:** 2026-10-13
- **Status:** ACCEPTED
- **Context:** ROADMAP.md §3.2. The goal is narrow and concrete: when an API
  answers 5xx, hand the local LLM the backend code most likely responsible. The
  difficulty is that the obvious implementation is forbidden. `sqlite-vec`,
  `vss`, and every other SQLite vector extension are **C libraries**, and
  AGENTS.md §2.1 makes `CGO_ENABLED=0` mandatory on every platform and every CI
  job. There is therefore no vector extension, no ANN index, no HNSW — not as a
  performance trade-off, but as a constraint of the build. The same constraint
  rules out `onnx-go` fetching a MiniLM artifact at runtime (invariant I-3,
  offline-first), so there is no neural embedding either.
- **Decision — two independent signals, fused by Reciprocal Rank Fusion:**
  - **Lexical: FTS5** (verified compiled into `modernc.org/sqlite` v1.60.1, with
    `bm25()` callable — a probe at implementation time, not an assumption).
    FTS5 finds exact tokens, which is what a stack trace supplies.
  - **Structural: brute-force cosine** over int8 vectors, computed in Go. FTS5
    finds tokens; cosine finds shape, and neither subsumes the other.
  - **Fusion: RRF with k=60**, `1/(k+rank)` summed across whichever signals
    placed a chunk. RRF merges two **rankings**, not two scores, so it needs no
    calibration between an unbounded bm25 and a cosine in [-1,1] — which is the
    whole reason it is used rather than a weighted sum.
  - The cost is O(N·d) for the cosine leg at d=256 int8, ~256 multiply-adds per
    chunk, ~12.8M ops at the ROADMAP's 50k ceiling. Bounded by the FTS leg
    taking candidates first.
- **Decision — the embedding is structural, and says so.** `HashingEmbedder` is a
  signed feature-hashing projection over identifier tokens, sub-word splits, and
  character trigrams at d=256. It is a real vector and genuinely informative for
  code — it captures `camelCase` morphology, so a trace saying "parse http
  response" finds `parseHTTPResponse` — but it is NOT semantic, and
  `Embedder.Name()` returns `structural-hash-v1` so every result is labelled.
  `UnavailableEmbedder` is a **first-class type**, not a nil interface: "the
  model is not installed" must be reported to the user, not silently treated as
  "no results". Its `Embed` returns `ErrModelUnavailable`, never a zero vector,
  because a zero vector ranks as "matches nothing" and the user is told their
  code has no similar chunks rather than that the feature is not installed.
  This is ROADMAP.md §3.2.2's DEGRADED contract honoured with the model path
  stubbed at the interface boundary rather than faked behind it.
- **Decision — chunking cuts at FUNCTIONAL boundaries, per language, with
  windowing only as overflow:**
  - **Go: `go/parser`.** The boundaries are the compiler's own
    (FuncDecl/GenDecl/TypeSpec), and the doc comment is included via `decl.Doc`.
    Scanning for the word `func` would start bogus chunks inside string literals
    and comments; a chunk beginning mid-expression is worse than no chunk
    because it retrieves confidently and wrongly. Method symbols are
    receiver-qualified (`(*Router).handleUsers`) because two `Serve` methods on
    different types are different bugs.
  - **Python: indentation IS the structure.** No Python parser exists in the Go
    stdlib and adding one would violate stdlib-first for a heuristic that does not
    need exactness. `def`/`async def`/`class`/`@` start a block at **any**
    indentation, and a block ends at the next dedent **or** at the same indent
    that starts a new block — the second condition is the one that is easy to
    miss, and without it a top-level `class` (indent 0) runs to end of file and
    yields one chunk for the whole module. Nested defs become their own named
    chunks: a stack frame names `inner`, and a chunk labelled `handle` that
    merely contains `inner` is a weaker match.
  - **Astro: structural fences** — frontmatter, `<script>`, `<style>`, template.
  - **Windowing is overflow, not the default**: declarations over 64 lines are
    split with 16-line overlap. The overlap is not decoration — without it a
    defect on a boundary line lands in neither window when the split moves.
- **Decision — the secret guarantee is at the WALKER, not the scrubber.** A file
  matching `.env*`, `*.pem`, `*.key`, `id_rsa`, `.git-credentials`, `secrets.*`
  and the like is never OPENED. A secret that is read can already have been
  indexed; redaction afterwards is too late. `.gitignore` semantics are a
  documented subset, and a leading `/` is preserved by normalisation because
  stripping it (the obvious cleanup) turns an anchored rule into one that matches
  at any depth — silent over-exclusion.
- **Decision — the FTS5 query is BUILT, never passed through.** Real error text
  contains `"`, `*`, `:`, `(`, `AND`, `OR`, `NOT`, `NEAR` — all FTS5 syntax.
  Handing a 500 body to `MATCH` verbatim turns every response whose body contains
  a quote into a query syntax error, i.e. the explanation feature fails precisely
  when it is needed. Terms are extracted as `[A-Za-z0-9_]` runs (FTS5's own
  bareword alphabet) and re-emitted as quoted phrases joined by **OR** — OR
  because a stack trace's tokens are mostly absent from any single file and AND
  would return zero rows exactly when the identifier matters most.
- **Decision — RAG tables are created by `InitRAG`, not by `Open`.** The main
  schema applies unconditionally when the history database opens, so putting
  FTS5 there would make the whole application unopenable on a driver build
  without FTS5 — for a feature that is optional and reports DEGRADED honestly. A
  user who never indexes a codebase should not be unable to run Pulse at all.
  `InitRAG` probes with a real CREATE and a real `bm25()` call, because a build
  can advertise the flag and still fail to instantiate the module, and finding
  that out at first query time means the user's first 500 explanation fails with
  a syntax error instead of at index time with a clear message.
- **Decision — writes are batched at 256 chunks per transaction.** A single
  transaction for 50k chunks holds the write lock for minutes; batching means a
  concurrent reader (an MCP session mid-explanation) waits milliseconds. Each
  chunk's row and its FTS5 row are written in the SAME transaction, because
  splitting them leaves a window where a chunk is findable by vector and
  invisible to lexical search — an index that disagrees with itself depending on
  which leg you ask.
- **Decision — internal/mcp does not import internal/store, and internal/store
  does not import internal/rag.** The dependency flow in ARCHITECTURE.md §3 is
  one-directional, so the store's `CosineInt8` and the RAG package's are written
  twice and pinned to agree by `TestCosineMatchesTheRAGPackage`; the MCP
  `Retriever` interface is satisfied by `StoreRetriever`, which takes function
  parameters rather than the store type, so no database driver is reachable from
  inside the MCP-reachable package tree (AGENTS.md §3.3).
- **Consequences — what the gates proved, including about themselves.**
  Four separate gates had to be rewritten because they measured the wrong thing,
  and the reasons are the most useful part of this entry:
  - **Memory ceiling.** Whole-process RSS is dominated by the Go runtime's
    baseline; `HeapAlloc` with the collector DISABLED reports total allocation
    (278 MB for a 1,000-file corpus — churn, not retention); and a *sampled*
    `HeapAlloc` peak carries the collector's own policy, swinging between 1.15x
    and 2.44x on identical source across five runs. The gate now asserts against
    the walker's OWN `PeakBuf` high-water mark — the largest single-file read
    buffer, tracked by the code that allocates it — and requires that it not
    grow while the chunk count does. That is machine-independent and cannot drift
    with GC timing. Measured: 3,138 bytes for both 125 and 1,000 files, 1.00x.
  - **File-descriptor leak.** `/proc/self/fd` sampled before and after 12 rounds
    of cancelled walks plus 25 differently-timed aborts; delta exactly 0.
  - **Churn.** TotalAlloc is bounded at 256x source. The gate SKIPS under `-race`
    with a stated reason: the detector's shadow allocations inflate TotalAlloc
    by 8-10%, and the measurement is 237x in a normal build versus 257x under
    `-race` on identical source — a 256x threshold sitting inside the
    instrumentation's noise would pass or fail depending on whether the race
    detector was switched on. Loosening the bound instead would leave a bound
    that no longer says what it was chosen to say.
  - **Mutation proof M25–M31**, all red → restore → green. Two of the seven
    boundaries took seven attempts to make meaningful, and that is recorded in
    `scripts/mutation-proof-rag.sh` rather than tidied away:
    - *M30 (vector-leg ordering).* The obvious mutation — changing cosine's
      divisor — is a **no-op**: `quantize` rescales every vector to L2 length
      exactly 127, so `|a|·|b|` already equals 127². Both a straight divisor
      change and a clamp to 127² left the gate green. The reason is a real design
      property: **RRF consumes rankings, not scores**, so narrowing the numeric
      band changes the returned `Score` but not the order fusion sees. Only an
      ordering change is a behavioural break.
    - *M31 (consumer's right to stop).* Six earlier mutations left the gate
      green, each for a correct reason: cancellation is enforced at four
      independent points, so disabling one is masked by the other three; and the
      consumer's exit is independently visible through `done`. The seventh
      attempt deadlocked for 600 seconds — and that hang was a **real defect**,
      so the fix went into the code rather than the gate: termination now cancels
      a derived context, and the producer already selects on `ctx.Done()` for the
      caller's own cancellation. One mechanism, no site where it can be
      forgotten, no reachable deadlock. The shipped mutation removes the
      consumer's right to stop while keeping liveness (workers drain rather than
      exit, producer stops watching `done`).
- **Consequences — honest gaps, NOT claimed done:**
  - **No `internal/app` composition root.** `StoreRetriever` exists and is
    gated, but nothing wires a `*store.Store` into an MCP server, so the binding
    is not reachable from `pulse --mcp` yet. This is the same gap P4.3.3 records
    from Move 7 and it now blocks the explanation feature end to end.
  - **No incremental reindex (ROADMAP §3.2.5).** `DeleteRAGChunksByFile` and the
    content-addressed id are the primitives it needs and are built; the fsnotify
    watcher is not. Branch-switch simulation is NOT RUN.
  - **No retrieval-latency benchmark at 50k chunks.** The recall gates are
    fixtures of 2–4 chunks; the O(N·d) claim at the ROADMAP's ceiling is argued
    from the operation count, not measured.
  - **`onnx-go` is not vendored.** ROADMAP §3.2.2 names it; shipping a stub
    behind the same interface was chosen because a model artifact cannot be
    fetched at runtime and none is vendored. The interface is the seam.
- **Refs:** internal/rag/rag.go, walker.go, chunk.go, embed.go, indexer.go,
  indexer_test.go, race_on_test.go, race_off_test.go; internal/store/rag.go,
  rag_test.go; internal/mcp/error_binder.go, error_binder_test.go, tools.go;
  scripts/mutation-proof-rag.sh; TASKS.md P3.2.1–P3.2.4; ROADMAP.md §3.2

---

## Entry Template (copy for new decisions)

```markdown
### ADL-XXXX — <title>

- **Date:** YYYY-MM-DD
- **Status:** PROPOSED
- **Context:** 
- **Decision:** 
- **Consequences:** 
- **Refs:** 
```
