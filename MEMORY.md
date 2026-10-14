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

### ADL-0022 — Auto-fuzzing: GCRA rate limiting, a refusing YAML subset, and TAP 13

- **Date:** 2026-10-13
- **Status:** PROPOSED
- **Context:** ROADMAP.md §3.3 asks for an LLM-driven boundary fuzzer over a
  local OpenAPI schema, and names two prerequisites that cannot be met:
  `github.com/getkin/kin-openapi`, and YAML parsing. Both `gopkg.in/yaml.v2` and
  `gopkg.in/yaml.v3` are **permanently banned** (`internal/devtools/importaudit`),
  so the YAML dependency is not a judgement call — it is unavailable. Meanwhile
  the feature sends hundreds of deliberately malformed requests at a machine the
  user is sitting in front of, driven by an agent that does not experience a
  server becoming unresponsive as something a human would. The failure mode that
  matters is therefore not "misses a bug" but "takes down the user's box".
- **Decision:**
  - **The limiter is GCRA, not a textbook token bucket.** The standard
    construction — refill at rate R, spend 1, sleep when empty — has a
    concurrency defect that only appears under load: N waiters all observe
    "empty", all compute the same wait, all sleep the same duration and all wake
    together, admitting a burst of N and exceeding R for a full interval
    afterwards. GCRA reserves the slot at DECIDE time:
    `tat := max(now, prev_tat); allow := tat - (C-1)/R; tat += 1/R`. That yields
    one inequality, `admission(i) >= t0 + i/R - (C-1)/R`, from which **no window
    of length Δ contains more than `C + R·Δ` admissions**. Measured: 1,000
    concurrent requests against a real `httptest` sink at a configured 200/s
    achieved 200.6/s, with the worst 1-second window holding 203 admissions
    against a bound of 204.
  - **The rate gate measures the WORLD, not the limiter's own report.** Under
    mutation M33 (reserve but do not wait) the limiter's recorded observations
    stay perfectly correct and every self-reporting assertion passes; only
    timestamps recorded at the sink detect it. The gate asserts over a SLIDING
    window rather than fixed buckets, because a fixed window hides a burst
    straddling its boundary — which is exactly the shape the defect produces.
  - **Cancellation does not refund a slot.** The reservation is taken under the
    lock and the sleep happens outside it. Refunding on cancellation would let
    the run ahead of schedule, which is how "the limiter cancelled cleanly"
    becomes "the limiter is no longer limiting".
  - **Zero, a negative rate, or concurrency below one is a configuration ERROR,
    never clamped.** A caller who wants no limit has to say so explicitly;
    an accidental zero that silently means unlimited is the single worst defect
    this file could have.
  - **A hand-rolled, REFUSING YAML subset parser.** Anchors, aliases, tags,
    block scalars, complex keys and tab indentation each produce a typed error
    naming the line. The reason is not squeamishness: a lenient parser that
    mis-nests a schema produces payloads for constraints nobody declared and
    reports them as coverage, so the failure mode is confident and wrong. An
    incomplete read is indistinguishable from a clean run, which is the worst
    property a boundary tester can have. Format is detected by first significant
    byte, never by extension.
  - **A `$ref` that cannot be followed is FATAL, at eight independent sites.**
    The first implementation `continue`d past it, and
    `TestLoadOpenAPIRejectsRemoteRefs` caught it: a document whose only
    constraint lived behind a remote ref produced a corpus that silently omitted
    it. Fail the whole load; the caller sees a partial IR otherwise.
  - **A remote `$ref` is refused outright** (invariant I-3): resolving one is a
    network fetch triggered by a user merely pointing Pulse at a file.
  - **The fuzz transport disables keep-alives and ignores proxy environment.**
    Both are corrections to the default transport, and both were found by
    building the thing. `http.Transport` silently retries an idempotent request
    whose pooled connection the server has closed — invisible to the caller, so
    the target receives more requests than the limiter ever scheduled and the
    proven ceiling stops describing actual traffic. And `http.DefaultTransport`
    honours `HTTP_PROXY`, so a target spelled `127.0.0.1` would carry every
    payload off the machine through a proxy host, turning the loopback guarantee
    into a statement about a URL string rather than about traffic.
  - **A round-trip `Resolver` makes interpolation an identity.** The engine
    expands `{{name}}` in URLs, headers and bodies, so a `{{7*7}}` injection
    probe was consumed by Pulse's own expander before leaving the process, and
    the injection strategy reported itself as run while testing nothing.
    Returning `"{{" + name + "}}"` from `Lookup` reproduces the input exactly for
    every input — `{{7*7}}` → `{{7*7}}`, `a{{x}}b{{y}}` → `a{{x}}b{{y}}`. It is
    bound last and unconditionally because a resolver that can be forgotten is a
    resolver that will be. The one input it cannot survive is an odd number of
    `{{` anchors, which the expander reports as an unterminated anchor: a real,
    reported outcome rather than a silent corruption, and unavoidable given the
    engine offers neither an escape nor a literal mode.
  - **Three verdicts, not two: pass, fail, skip.** A probe the transport cannot
    carry at all (a header value containing a NUL, which RFC 7230 §3.2 forbids)
    is SKIPPED with a stated reason. It never left Pulse, so the target said
    nothing about it; reporting it as a failure makes a correct server look
    broken, and silently dropping it shrinks a corpus the TAP plan still
    promises — which hangs the consumer. Same treatment for cancellation.
  - **TAP 13, with the two rules hand-rolled emitters break.** The plan is
    emitted from the same slice the results were rendered from (a consumer that
    plans for 40 and receives 39 hangs), and descriptions are flattened (a
    newline inside one makes the next line parse as a new test, corrupting every
    result after it). Strings are single-quoted with `'` doubled — the one
    escaping rule YAML guarantees unconditionally — rather than choosing a style
    by content, which breaks on a value containing both quote types.
  - **2xx and 4xx both pass; 5xx fails.** A boundary value the server correctly
    rejects is the behaviour under test. Treating a 400 as a finding would make
    every fuzzer report every well-written server, and a user who learns the
    tool cries wolf stops reading it.
  - **The seed chooses among equally-valid values, never the matrix shape**, so
    coverage of declared constraints cannot vary between runs. A budget is a cap
    applied by even-stride, not a prefix — a prefix would only ever exercise the
    first parameter alphabetically.
  - **`internal/fuzz` depends on stdlib only. Move 9 added no dependency.**
- **Consequences — honest gaps, NOT claimed done:**
  - **No `internal/app` composition root.** `RegisterFuzzTool` and
    `EngineFuzzRunner` are implemented and gated, but nothing hands a runner to a
    server, so `execute_fuzz_run` is not reachable from `pulse --mcp`. This is
    the same gap that blocks P3.2.10 (Move 8) and P4.3.3 (Move 7), and it now
    blocks the fuzzer and the error-explanation feature from one missing seam.
    Building it is the highest-value remaining action in the repository.
  - **Finding persistence and dedup are NOT built.** ROADMAP §3.3.3 specifies
    dedup by `sha256(status + canonical response prefix)`, a `findings` table and
    `run_source='fuzz'`. `internal/mcp` never writes to `store`, so none of it
    exists.
  - **The streaming `fuzz-start`/`fuzz-next`/`fuzz-report` loop is NOT built.**
    Move 9 ships the single-shot bounded form. The bound and the interruptibility
    are already in place, so the streaming contract is a reshaping of the loop
    rather than a new mechanism — but it is not built.
  - **`internal/fuzz` is not in the AGENTS.md §3.1 package list.** That list was
    written before this move. The amendment belongs in a dedicated
    documentation commit per §4.6 item 7 and is deliberately NOT bundled here.
  - **The rate gate's arithmetic is proven for the limiter, and one-payload-one-
    request is proven for the transport, but no gate measures the *combined*
    achieved rate at a production-typical budget.** `TestFuzzToolReportsATargetServerRate`
    asserts the achieved rate is within tolerance of the configured one; it does
    not re-derive the sliding-window bound end to end through the tool.
- **Refs:** internal/fuzz/openapi.go, yaml.go, boundary.go, limiter.go, tap.go,
  openapi_test.go, limiter_test.go, tap_test.go; internal/mcp/fuzz_tool.go,
  fuzz_engine.go, fuzz_test.go, fuzz_engine_test.go;
  scripts/mutation-proof-fuzz.sh; TASKS.md P3.3a.1–P3.3a.8 (P3.3a.9–P3.3a.11 open);
  ROADMAP.md §3.3.

---

### ADL-0023 — The composition root: one package that chooses, and a layering inversion it paid for

- **Date:** 2026-10-13
- **Status:** PROPOSED
- **Context:** Moves 0–9 built ten subsystems, each gated, and **none of them
  could run**. Three open items across three separate moves shared one root
  cause and had no fix, because each was not a feature:
  - `P4.3.3` (Move 7): `DefaultExecutor` was constructed inside the bridge with
    a nil `secrets.Redactor`, so MCP mode redacted by header name only and a
    vault-sourced value echoed back inside a response **body** reached the model
    intact.
  - `P3.2.10` (Move 8): `StoreRetriever` existed and was gated, but nothing held
    a `*store.Store`, so the 5xx explanation was unreachable from `pulse --mcp`.
  - `P3.3a.9` (Move 9): the fuzzer had a tool, a runner and a gate, and nothing
    handed the runner to a server.
  Each was recorded honestly as NOT BUILT, which is right, but recording a
  blocker three times is a signal that the blocker is structural. It was: the
  bridge constructed its own dependencies, so the choice of vault was a property
  of the protocol layer rather than of the application.
- **Decision:**
  - **`internal/app` is the one package that knows about every other one.** Every
    other package declares what it needs as an interface and is handed it. The
    design goal is not architectural fashion — it is what makes each subsystem
    testable without booting SQLite, a keychain and an HTTP transport.
  - **The layering inversion was paid for, not worked around.**
    `internal/mcp` declared `mcp.Redactor`, a one-method interface, and stopped
    importing `internal/secrets`. Naming the concrete type there would have made
    the vault a property of the protocol layer; declaring the contract lets the
    composition root decide what is installed and prove it satisfies the
    interface. `(*Redactor).String` gained a **nil-receiver guard** for this —
    a nil `*Redactor` means "no values registered", which is true, and without
    the guard a typed nil stored in an interface field passes a `!= nil` check
    and panics one line later. The compile-time assertion lives in `internal/app`
    because that is the one package allowed to see both sides.
  - **Construction returns `(*App, error)` and never a partial App.** Every
    resource acquired on the way is released on the way out, so a caller cannot
    use — or Close — something half-built. Gated by
    `TestAppConstructionFailureLeaksNothing`: 11 open descriptors before and
    after a failed construction.
  - **`Close` cancels the context BEFORE releasing the store**, and is
    idempotent. The ordering lets in-flight work observe the shutdown and unwind
    through its own teardown path (Goal 1 §1.2.6) rather than having its database
    handle pulled out from under it. Idempotence is not polish: a deferred
    `Close` and an explicit one from a signal handler both run in any realistic
    program, and a second `sql.DB.Close` returns an error the caller cannot
    interpret.
  - **The chain and the redactor are ONE vault.** A value resolved through the
    chain is registered with the redactor, so it cannot then appear in a
    response body, a diagnostic or a retrieved chunk. Two `Redactor`s would leave
    a window where a value is known to the resolver and invisible to the
    scrubber. The gate asserts the ROUND TRIP, because every individual field
    being non-nil proves nothing about whether they are the same instance.
  - **Fail closed at construction.** `MCPServer` refuses to serve without an
    executor, a fuzz runner or a redactor; `IndexCorpus` refuses to index without
    one. A server with no redactor still answers every request — it just answers
    them with the user's tokens in the body. Failing at construction is the only
    disposition where the mistake cannot ship unnoticed.
  - **Degradations are reported, never hidden.** A missing keychain, an
    unreadable `.env` and an unindexable corpus each degrade with a stated
    reason while the app keeps serving. A headless developer machine has no
    keychain, and failing to start there would make Pulse unusable for a large
    fraction of the people who would use a local API workbench. What is
    forbidden is starting and behaving as though the subsystem had loaded.
  - **Secret exit codes come from the error CHAIN.** `app.SecretExitCode`
    returns 3 for `secrets.ErrUnresolved` and 0 for everything else, and the
    caller falls through to its generic path. It cannot delegate to
    `secrets.ExitCode`, which maps EVERY error to a code and would make a disk
    failure indistinguishable from a vault failure at the call site.
  - **The CLI reads `PULSE_*` environment variables, not flags.** Every one is a
    deployment detail an MCP client sets once; putting them on the command line
    would invite an agent to vary them per call. An unparsable value is logged
    and the default is used rather than ignored silently.
  - **`mcp.Lock()` stays the first statement of the MCP path**, with `app.New`
    after it. Folding the lock into a convenience constructor would look safe to
    call late, which is how a boot log ends up on the JSON-RPC channel.
  - **The bridge builds nothing.** `mcp.NewServerWithConfig` takes an
    `Executor`, an optional `FuzzRunner`, an optional `Redactor` and register
    options. A protocol layer that constructs its own dependencies cannot be
    exercised without constructing all of them.
- **Consequences — honest gaps, NOT claimed done:**
  - **Five of six MCP tools remain unbuilt** (`list-collections`,
    `list-requests`, `get-request`, `get-run`, `get-history`). The composition
    root removed the *wiring* blocker; only the handlers remain.
  - **Fuzz finding persistence and dedup are NOT built** (P3.3a.10). A root
    seam does not implement a schema: no `findings` table, no
    `sha256(status + canonical response prefix)` dedup, no `run_source='fuzz'`.
  - **The streaming `fuzz-start`/`fuzz-next` loop is NOT built** (P3.3a.11).
  - **Chunked client delivery is NOT built** (P4.3.4). It needs Phase 3's
    `sync.Pool` recycle path, because a client-driven stream outlives a single
    tool call.
  - **`P4.1.3` mode immutability is still BLOCKED on Phase 3** — `internal/ui`
    does not exist, so there is no second wiring to assert against.
  - **`P3.2.5` incremental reindex and `P3.2.9` the 50k benchmark remain open**
    from Move 8; this move did not touch them.
  - **The TUI is still a stub that exits 1.** Goal 3's *MCP* half is bridged;
    Goal 1's interactive half is not, and the binary's default mode is still the
    unimplemented one.
  - **Two mutations needed several attempts each, and the attempts are the
    finding rather than noise to hide.** Redaction is enforced at TWO independent
    layers (`engine.WithRedactor` in flight, `EngineExecutor.Redactor` on the
    collected outcome) and removing EITHER ONE ALONE leaves every gate green —
    M42 therefore disables both, and the script records why. M46's first form
    gave the binder a different redactor and came back green for a real reason:
    the error text arrives inside an already-scrubbed `Outcome.Body`. Both are
    recorded inline rather than tidied into a clean-looking list.
  - **One gate was measuring the wrong thing and was found by a mutation, not by
    inspection.** `TestAppStoresNoSecretOnDisk` read only `pulse.db`; SQLite runs
    in WAL mode, so a freshly written row has not been checkpointed into the main
    file, and the gate stayed green against M48 with the secret in plain text one
    file away. It now reads `-wal` and `-shm` too. The same gate was ALSO
    indexing a `.js` corpus, which `rag` does not chunk — so it stored nothing
    and proved nothing. Both faults were invisible because every assertion in it
    was technically satisfied.
- **Refs:** internal/app/app.go, mcp.go, app_test.go; internal/mcp/redactor.go,
  server.go; internal/secrets/redact.go; cmd/pulse/main.go;
  scripts/mutation-proof-app.sh (M42–M49); TASKS.md P4.1b.1–P4.1b.9, P3.2.10,
  P3.3a.9, P3.3a.10, P4.3.3; ROADMAP.md §3.1.1.

---

### ADL-0024 — Wasm sandboxing: no fuel in wazero v1.12, and a memory ABI that returns a length

- **Date:** 2026-10-13
- **Status:** PROPOSED
- **Context:** ROADMAP.md §3.4 asks for a sandbox for untrusted auth/signing
  scripts on `tetratelabs/wazero` (pure Go, interpreter-only). §3.4.4 already
  recorded that fuel metering is unavailable, and §3.4.2 sketched an ABI where the
  guest returns `(sig_ptr, sig_len)`. Both records shaped the work, and building
  it produced four findings the roadmap did not anticipate.
- **Decision:**
  - **Every capability is denied by never being offered.** No host modules are
    instantiated, so ANY import fails to resolve and the module refuses to
    instantiate. Interpreter mode only (`NewRuntimeConfigInterpreter`, never the
    JIT: the compiler mmaps executable pages, is non-deterministic in cost, and
    buys throughput a signing script does not need). Memory capped at 4 MiB,
    stdout and stderr to `io.Discard`, stdin always-EOF, `_start` cleared.
    Deliberately unset: `WithEnv`, `WithFS`, `WithSysWalltime`, `WithSysNanotime`,
    `WithSysNanosleep`, `WithRandSource`, `WithOsyield`, `WithArgs`. There is no
    capability to revoke later, because none is granted, which is a stronger
    property than a whitelist and the reason no `pulse/env` host module exists.
  - **The ABI returns a LENGTH, not a pointer.** The guest exports
    `allocate(size) -> ptr`, `sign_request(ptr, size) -> i32` and `memory`. The host
    allocates `len + 4 KiB` of headroom, writes the envelope, calls the signer, and
    reads back.
    The deviation from §3.4.2's `(sig_ptr, sig_len)` is the safety argument, not
    convenience: a guest-returned pointer is an address the host must trust, and a
    bounds check that is wrong is a memory-safety bug in the host. Pinning the
    output to the host's own buffer leaves a LENGTH as the only guest-supplied
    value, and a length is checked against a buffer the host owns. There is no
    address for a malicious guest to choose.
    The host passes the ENVELOPE length, not the capacity, so a guest cannot
    mistake the reserved tail for content and sign the wrong bytes.
  - **Bounds checks subtract instead of adding.** `ptr > avail` and
    `size > avail - ptr`. `ptr + size <= avail` wraps on uint32, so it is trivially
    true at `ptr = 2^32-1` — the first value a hostile guest would return.
    `ErrBounds` (a host bug) and `ErrABIViolation` (a guest bug) are separate types
    so a caller can tell whose defect it is.
  - **wazero v1.12 has no fuel API at all.** A case-insensitive search of the whole
    module finds the word only in an unrelated internal test helper. §3.4.4 already
    knew; this confirms it independently.
  - **A panicking FunctionListener does not abort a guest call.** The panic escapes
    into the host and the call does not return, so "abort from the listener" is not
    a fuel mechanism either.
  - **A listener attached to the `Call` context is silently ignored.** wazero binds
    it at `CompileModule` time. The first draft attached it per call and counted
    nothing, which is indistinguishable from a budget that is merely generous.
  - **A call budget is a DETECTOR, not a preemptor.** With the listener bound
    correctly, a 20,000-call guest ran against a budget of 50 and completed: the
    listener cancels on call 51, and a straight-line body has no safe point until
    the end. So the two tiers do different jobs.
    **TIMEOUT guarantees termination** (`WithCloseOnContextDone(true)`; measured
    50.4ms against a 50ms deadline for `(loop br 0)`).
    **FUEL guarantees that "it survived" is not mistaken for "it behaved"** — it is
    deterministic (counts calls, not milliseconds, so it decides identically on a
    loaded CI runner) and it poisons the module, so a guest detected over budget is
    never reused. A timeout-only sandbox stops a hanging guest and then reuses it;
    a fuel-only sandbox notices a runaway and does nothing.
  - **wazero's interpreter has its own call-stack bound** which traps deep recursion
    cleanly before any large budget could fire. That is a third, runtime-owned
    bound, and it is recorded rather than counted as fuel.
  - **`memory.grow` refuses, it does not trap.** A memory-cap gate asserting an error
    was wrong and failed; the security property is that the ceiling HOLDS.
  - **Test guests are assembled by hand.** The gates need modules no compiler emits
    (an infinite loop, a liar about its output length, a pointer past memory, a
    20,000-call storm). `wasmBuilder` assembles ~40 bytes per module, annotated with
    equivalent WAT, which is cheaper than depending on TinyGo or `wat2wasm` and
    keeps `go test` the only tool required.
  - **`github.com/tetratelabs/wazero v1.12.0`** is sanctioned in
    `config/allowed-modules.txt` with the no-fuel measurement recorded inline, so a
    future reader of the allowlist sees the constraint rather than rediscovering
    it. It is the 11th module; the sandbox adds no indirect dependency.
- **Consequences — honest gaps, NOT claimed done:**
  - **Goal 3 is recorded as PARTIAL, not COMPLETE.** The Move 11 directive asked for
    Goal 3 to be marked COMPLETE; §3.5.1's own checklist lists four unmet
    obligations, and AGENTS.md §4.5 forbids disposing a claim as green when it is
    not. ROADMAP.md §3.5 now carries the remainder explicitly, so the next move
    inherits four open items rather than a green facade. The four are: §3.4.3 host
    functions and HMAC-by-key-id; the 50k-chunk latency benchmark; the
    incremental-reindex branch-switch simulation; and fuzzing dedup plus the
    streaming loop.
  - **§3.4.3 zero-trust host functions are NOT built.** No host functions is
    strictly more isolated and satisfies the whitelist gate, but a guest cannot
    compute a signature at all, so the HMAC-by-key-id chain has no host side.
  - **`internal/wasm` has no caller.** It is a self-contained sandbox. The
    composition root exists as of Move 10, so the seam is available and nothing
    uses it; `pulse --mcp` signs nothing with a guest.
  - **No instance pooling.** §3.4.1 specifies a pool keyed by script sha. Each
    `Module` owns its own runtime, so there is nothing to pool, and the
    interpreter's per-guest cost is paid every time.
  - **The limit-surface documentation test (§3.4.4) is NOT built.** Limits are
    asserted against the code but not diffed against this document, so the two can
    drift.
  - **The .gitignore lockdown is recorded here for the two contradictions it
    surfaced, neither of which `.gitignore` can resolve.** The whitelist it
    implements omitted `DESIGN_SYSTEM.md`, one of the seven Essential Vibe Coding
    Files (AGENTS.md §4.6) — a default-deny list missing a mandated document
    reintroduces through the exception list the exact failure the rule exists to
    prevent, so it was added. The same directive also whitelists `README.md` and
    `CONTRIBUTING.md` while §4.6 says no README is authorized; both negations were
    written (they are permissions, and neither file exists) and the disagreement is
    recorded here rather than resolved unilaterally.
  - **An unanchored `!README.md` matches at every depth.** The first version of the
    rule resurrected `vendor/README.md` and `testdata/fixture/README.md` — the
    exact failure it was meant to prevent — and it did so silently until
    `git check-ignore` was run rather than assumed. Every negation is now anchored
    with a leading slash.
- **Refs:** internal/wasm/runtime.go, memory.go, runtime_test.go, wasmgen_test.go;
  scripts/mutation-proof-wasm.sh (M50–M59); config/allowed-modules.txt;
  ARCHITECTURE.md §15; ROADMAP.md §3.4.1–§3.4.4, §3.5; .gitignore.

---

### ADL-0025 — Vault cryptography and the CI/CD loop: a nonce, an exit code, and a binary that runs

- **Date:** 2026-10-13
- **Status:** PROPOSED
- **Context:** Goal 4 opens with two capabilities: a headless `pulse test` pipeline a CI
  system can drive (ROADMAP.md §4.1), and commit-encrypted production config
  (ROADMAP.md §4.3). The directive scoped both, adding two gates of its own that
  became the most valuable things in the move: a NONCE gate that proves nonce
  uniqueness, and a TAP EXIT gate that proves the exit code mirrors the results.
- **Decision:**
  - **AES-256-GCM, stdlib only.** `crypto/aes`, `crypto/cipher`, `crypto/rand`, and
    nothing else — no third-party crypto, no KDF, no password mode. The container is
    `PVLT1`: magic, a reserved flags byte, a 12-byte nonce, then `gcm.Seal`'s
    output with the tag appended. **A reserved-flags byte is REFUSED rather than
    ignored**: an ignored flag that later means "compressed" would decrypt to
    garbage, and a refused one makes the container's version the thing that has to
    be upgraded.
  - **The key is 32 bytes of `crypto/rand`, with no derivation.** Stretching a
    uniformly random key is theatre that makes a weaker key look stronger, and it
    would imply the key was a passphrase.
  - **A nonce is generated per seal and never reused.** This is the load-bearing
    property of the whole file. Nonce reuse under one key is not a weakness but a
    break: GCM's keystream cancels between the two plaintexts, and the
    AUTHENTICATION KEY is recoverable, so an attacker can forge a third container
    that verifies. A counter, a timestamp, a hash of the plaintext and a fixed
    constant are each schemes that eventually repeat — only `crypto/rand` cannot
    be made to repeat by a change in workload, a clock adjustment, a re-encryption
    of unchanged input, or a rotation done twice. The cost is 12 bytes per seal.
  - **The nonce gate seals 200 times and asserts THREE things**: no nonce repeats,
    no container repeats, and all 200 decrypt to the same plaintext. The loop is
    the test, because a fixed nonce passes a single seal and fails on the second —
    which is the whole defect. Mutation M61 names the actual nonce in its failure.
  - **`Seal` does NOT zeroise its caller's buffer.** It did originally, and
    `TestSealingTheSamePlaintextTwiceDiffers` caught it immediately: a function
    that wipes what it was handed cannot be composed, cannot be called twice on
    the same bytes, and fails the most natural assertion there is. Zeroisation is
    the caller's, and the FILE path applies it to the buffer it allocated itself —
    which is why `vault encrypt` removes the plaintext file as well as the buffer.
  - **Every decryption failure reports the same thing.** `ErrAuthFailed` covers
    wrong key, tampered byte and truncation identically. The GCM tag is the only
    mechanism that distinguishes them, so surfacing the difference to the CLI
    would hand an oracle to anyone who can present a container and observe the
    exit status. The exit CODES differ (auth vs format) but both are 1, and
    `kindForDecrypt` documents why.
  - **The keychain's read and write sides are separate interfaces.**
    `SecretResolver` has been read-only since Move 4 and three moves of
    resolution logic, degradation reporting and fail-closed mapping are built on
    it; making it also write would give every reader a credential-writing path it
    does not need. `SecretStore` is new (Store/Available/Backend), implemented on
    darwin via the `security` CLI, windows via `CredWriteW` through
    `x/sys/windows`, and linux via Secret Service D-Bus `CreateItem`.
  - **The exit code is a pure function of the report**, mirrored from
    `engine.ExitCodeFor` so `pulse test` and the runner cannot disagree:
    0 all pass, 1 any failure, 2 usage-or-parse, 3 secret-resolution, and a
    BLOCKED-with-reason test is a `# SKIP` line that does NOT fail the run (it
    fails only under `--strict-blocked`). **An empty report is exit 2, not 0** — a
    run that produced no test points has not passed anything, and reporting
    success for "nothing happened" is the greenwash AGENTS.md §4.5 exists to
    prevent.
  - **The exit mapping scans for a secret failure BEFORE any plain failure.** The
    single-pass version returned 1 whenever a plain failure happened to precede a
    secret one, which made exit 3 a function of file ordering rather than of
    cause. Exit 3 means "configure your keychain" and exit 1 means "a test
    failed"; telling a user about a failing assertion while their real problem is
    a missing credential sends them in the wrong direction.
  - **The runner produces structured entries and does not format.** Move 9's TAP
    13 emitter lives in `internal/fuzz` and AGENTS.md §3.1 forbids a sibling
    importing a sibling, so `internal/engine` is a producer and `internal/app` is
    the formatter. This is the layering rule AND the reuse: a JSON or silent
    reporter costs no change in the engine.
  - **`App.newRouter` is shared by the test runner and the MCP path.** Two
    constructions would be two places for the redactor, the resolver and the
    timeout to be wired differently, and a test run that redacts differently
    from an MCP call is a redaction gate that passes in one mode and fails in the
    other.
  - **Assertions are validated before any request is dispatched.** A malformed
    assertion is exit 2, and discovering it after six requests have hit the server
    is both slow and misleading.
  - **`assert.go`'s `EvalBody` takes the body separately, deviating from ROADMAP
    §4.1.1's `Eval(res *RunResult)`.** `RunResult` is a bounded SUMMARY
    (Status, Elapsed, Bytes, Digest) that deliberately does not carry the body,
    because a body can be gigabytes and a RunResult outlives the stream that
    produced it. `body contains` therefore cannot be evaluated against a
    RunResult; the honest options were to assert against the Digest — a much
    weaker statement, and one that cannot answer "does the error message name the
    symbol" — or to pass the body in.
  - **`pulse test` is single-shot and never installs the stdout lock**, so its TAP
    goes to ordinary stdout and there is no JSON-RPC stream to corrupt.
- **Consequences — honest gaps, NOT claimed done:**
  - **The three real keychain backends are BLOCKED, not PASSED.** `vault init`
    cannot be verified on this host: WSL has no Secret Service daemon, and there
    is no darwin or windows machine. `TestStoreAvailableNamesTheReason` reports
    the reason rather than passing silently, and the storage gate runs against
    `MemoryStore`. The darwin/windows/linux write paths are COMPILED on all
    cross-compile targets but not executed. The darwin path passes the secret as
    an argv element to a short-lived child, which `ps` can read — a real and
    documented weakness of the CGO-free constraint, not an oversight, mitigated
    only by `vault init` being a once-per-machine operation.
  - **A 200-seal nonce gate is not a statistical proof.** It detects a FIXED
    nonce with certainty and a poorly seeded PRNG with near-certainty, but it
    cannot bound the collision probability of a 96-bit random nonce. The
    guarantee is that the source is `crypto/rand`, which the gate asserts
    indirectly.
  - **No rotation, revocation, or multi-recipient support** (ROADMAP §4.3.4,
    §4.3.1). The container carries no recipient list and no per-recipient
    wrapped DEK, so `pulse vault rotate` would have to re-encrypt with the SAME
    key rather than wrap a new one for a new recipient set. Revocation is
    therefore "change the key", which does not help a revoked contributor who
    already holds the old one.
  - **No resolvable `*.vault` in a collection** (ROADMAP §4.3.3). A request
    referencing `{{VAULTED_TOKEN}}` fails closed with an unresolved variable.
  - **No `pulse test` --format json / --silent** (ROADMAP §4.1.3), and no SIGINT
    plan-tail emission. The exit code works; the alternate output formats and the
    signal path do not exist.
  - **Goal 4 is not effectively OPENED by this move**: the headless loop and the
    single-recipient vault are in, peer-to-peer sync (§4.2) is untouched, and the
    rotation/revocation half of §4.3 is not built.
  - **One `-race` run of `internal/engine` failed on `TestSSEStreamCarriesTelemetry`**
    and did not reproduce in five subsequent runs. It is in Move 5 code this move
    did not touch (git diff shows only `assert.go`, `test_runner.go` and their
    tests added to that package). Reported as a PRE-EXISTING FLAKE, not as a pass.
- **Refs:** internal/secrets/vault.go, store.go and its four platform files,
  vault_test.go; internal/engine/assert.go, test_runner.go, test_runner_test.go;
  internal/app/test.go, test_test.go; cmd/pulse/main.go;
  scripts/mutation-proof-vault.sh (M61–M68, M65b); ROADMAP.md §4.1, §4.3.

---

### ADL-0026 - Agent-skill configuration lives in `.agents/`, and the documentation surface widens by exactly four files

- **Date:** 2026-10-13
- **Status:** PROPOSED
- **Context:** The engineering skills (`tdd`, `triage`, `to-spec`, `domain-modeling`,
  `code-review`, `handoff`) read per-repo configuration: an issue tracker, a triage label
  vocabulary, and a domain-doc convention. The skill that scaffolds this defaults to
  `docs/agents/`, which is neither portable across harnesses nor compatible with the
  markdown lockdown. This repo already vendors its skills into `.agents/` (the portable
  tree, hash-pinned by `skills-lock.json`) and mirrors them into `.opencode/` and
  `.freebuff/`; configuration written to a harness directory would not survive a change
  of harness.
- **Decision:**
  - **Configuration goes in `.agents/`, alongside the skills that read it.** A grep of
    all 40 vendored skills found that **only the setup skill itself** ever mentions
    `docs/agents/`; no consumer hardcodes a path. The location is therefore a convention
    established by whatever points at it, which means the only thing that has to change
    is the pointer - and portability is free.
  - **Issue tracker: GitHub Issues**, via `gh`, which is installed and authenticated.
    `.agents/issue-tracker.md` additionally records that an issue proposes work and never
    substitutes for the commit carrying it, because ADL-0015 makes `origin/main` the
    database of record.
  - **Triage labels: the five canonical defaults**, created in the tracker with
    `gh label create`. `.agents/triage-labels.md` narrows `ready-for-agent` for THIS
    repo: under §4.2 and §4.7, "ready for an agent" means the issue already names its
    gate, its exit condition, and its mutation. An issue that cannot answer those three
    is `needs-info`.
  - **There will be no `docs/adr/` directory.** `MEMORY.md` is already the append-only
    decision ledger (§4.6) with 26 entries and is what CI checks. A parallel ADR
    directory would give this repository two sources of truth for the same decisions with
    no rule for which wins - §4.5's greenwash applied to architecture. `.agents/domain.md`
    says so explicitly, so the next agent does not create one.
  - **§4.6's documentation surface widens by exactly four files**: `.agents/issue-tracker.md`,
    `.agents/triage-labels.md`, `.agents/domain.md`, and `GLOSSARY.md`. No eighth
    *domain* document and no second ledger; these are skill plumbing plus a vocabulary
    index.
  - **The lockdown itself is not loosened.** `.gitignore` gains two anchored negations,
    `!/.agents/*.md` and `!/GLOSSARY.md`. `/.agents/*.md` is anchored AND single-star on
    purpose: git documents that an unanchored `!` matches at every depth, and a single
    `*` does not cross a `/`. The line therefore whitelists exactly the three config files
    directly in `.agents/` and leaves the ~40 vendored `SKILL.md` files underneath
    `.agents/skills/` ignored - consistent with the existing policy for `.opencode/`
    (ADL-0013/0014), where the vendor tree is untracked because it is reproducible from
    `skills-lock.json` plus the pinned upstream.
  - **Verified, not assumed.** `git check-ignore` was run on all four whitelisted files
    (all committable), on five vendored `SKILL.md` files (all still ignored - the leak
    check), and on five decoy paths at four depths (`notes.md`, `docs/README.md`,
    `vendor/README.md`, `testdata/fixture/README.md`, `internal/app/README.md` - all still
    ignored, so the default-deny survives).
- **Consequences - honest gaps:**
  - **The vendored skill tree and `skills-lock.json` are BOTH untracked.** That is the
    pre-existing policy, but it has a consequence worth stating: the lock file that exists
    to make the tree reproducible is not itself committed, so a fresh clone of `main` gets
    **no skills at all** until someone runs the restore procedure in
    `~/.local/share/opencode-skill-src/` (ADL-0013/0014). The new `.agents/*.md` config
    files this commit DOES ship therefore travel without the skills that read them. That
    is a coherent trade - the config is cheap to review, the tree is not - but it is a
    real one, and committing the tree or at minimum `skills-lock.json` is a separate
    decision this entry does not make.
  - **`AGENTS.md` gained §6, and it is explicitly non-normative.** It opens by stating
    that sections 1-5 are the contract and section 6 is configuration, and that a
    conflicting file under `.agents/` is to be fixed rather than weighed. An
    `AGENTS.md` whose later section quietly outranks its earlier one is worse than no
    section at all. Appended as §6 so no existing section renumbers and no cross-reference
    in §4 breaks.
  - **`GLOSSARY.md` was written from vocabulary the repository already had settled**
    (AGENTS.md, PRD.md, ARCHITECTURE.md, ADL-0025) rather than invented, and each term
    names where it is authoritative. It is an index, not a source. A term whose definition
    disagreed with the code would be a bug in the glossary, so it is reviewable against
    the commits it indexes.
  - This entry amends §4.6, so per §4.6.7 it ships in a **dedicated documentation
    commit**, not bundled with feature work.
- **Refs:** `.agents/issue-tracker.md`, `.agents/triage-labels.md`, `.agents/domain.md`,
  GLOSSARY.md, .gitignore (the two anchored negations), AGENTS.md §6.

---

### ADL-0027 — `pulse test` gains JSON and silent formatters with exit-code purity

- **Date:** 2026-10-13
- **Status:** ACCEPTED
- **Context:** Move 12 left `pulse test` TAP-only (ADL-0025 consequences): no
  `--format json` / `--silent`, although the engine/runner split was built
  precisely so a second formatter costs no engine change. HANDOFF.md §4.2
  sequenced this first because it has zero open design questions.
- **Decision:**
  - `internal/app` owns a `TestFormat` (`tap` default, `json`) with
    `ParseTestFormat` failing closed on anything else; `TestDirEx` validates
    the format BEFORE any request dispatches (unknown → exit 2, zero bytes,
    zero requests) and `RunTest` renders BOTH documents from the same
    redacted entries. `--silent` returns the report's exit with zero writes.
  - Exit mapping is UNCHANGED and still `engine.ExitCodeFor`'s pure function
    (0 pass-or-skipped, 1 failure, 2 parse/usage, 3 secret-first): the
    formatter copies `total`/`passed`/`failed`/`skipped`/`exit` from the
    report and never recomputes them, so TAP and JSON cannot disagree.
  - Redaction parity is structural, not assumed: JSON is built from the same
    entry stream TAP consumes, and `TestJSONRedactsSecrets` drives the one
    runner path that echoes response content (a header-equality failure,
    assert.go `%s is %q, want %q`) carrying a vault value.
  - Mutation pool advances to M69 (total mapped into `passed`) and M70
    (`--silent` falls through to TAP), proven in
    `scripts/mutation-proof-formatters.sh` with cmp-verified restore.
    `ARCHITECTURE.md` is untouched: no package, data flow, or schema moved.
- **Consequences:**
  - `pulse test --format json` is available to CI dashboards; `--silent` is
    safe for wrappers that only need the exit status. Neither weakens
    fail-closed parse/secret semantics.
  - Remaining Move 12 gaps are unchanged: SIGINT plan-tail, `--strict-blocked`
    skip-source design call, PVLT2 recipient model (before vault resolution
    per the sequencing constraint), then P4.1c.9 vault resolution.
- **Refs:** internal/app/test.go, test_test.go; cmd/pulse/main.go (`--format`,
  `--silent`); scripts/mutation-proof-formatters.sh (M69–M70); TASKS.md
  P4.1c.10; ROADMAP.md §4.1.3/§4.1.4 (contracts unmoved).

---

### ADL-0028 — `pulse test` halts gracefully on SIGINT with a partial report and exit 130

- **Date:** 2026-10-13
- **Status:** ACCEPTED
- **Context:** Move 12 returned `(nil, context.Canceled)` when a test run was
  cancelled: the completed work was dropped, the CLI could only report a bare
  error, and an interrupted run was indistinguishable from a failure.
  ROADMAP.md §4.1.4 requires the plan tail before exit 130, and HANDOFF.md
  §4.3 forbids testing it with real signals.
- **Decision:**
  - `cmd/pulse` wraps the test path in `signal.NotifyContext(os.Interrupt)`
    — a bare stdlib call with no branching logic, so it carries no gate of
    its own; the cancellation contract is gated injectably in the runner,
    which already takes a context.
  - `engine.TestRunner.Run` treats a dispatch-bailed worker as a halted
    queue slot, not an error: it collects the completed entries, sets
    `TestReport.Interrupted`, and exits `ExitInterrupted` (130). Interruption
    outranks failures and `--strict-blocked` in both the runner and
    `App.RunTest`, because escalating it would conflate "the user stopped
    the run" with "a test failed".
  - The tail declares what ran: TAP plan and JSON counts cover the completed
    entries only, so no consumer hangs or is lied to. Parse failures stay
    fatal with exit 2 ahead of all of this.
  - The old `TestACancelledRunStopsPromptly` assertion (nil report +
    `context.Canceled`) tested the previous contract and was rewritten to
    the partial-report one — promptness and queue-drain assertions kept.
  - Mutation pool advances to M71 (130 degraded to 1) and M72 (queue
    ignores cancellation), proven in `scripts/mutation-proof-sigint.sh` with
    cmp-verified restore. `ARCHITECTURE.md` §16.4 updated (JSON/silent and
    the SIGINT tail are no longer "not built"); no topology moved.
- **Consequences:**
  - Ctrl+C during `pulse test` now yields a valid partial TAP/JSON document
    and exit 130; `--silent` stays silent at 130. A second SIGINT does not
    force-kill (the context is already cancelled) — accepted, not gated.
  - Remaining Move 12 gaps: `--strict-blocked` skip-source design call,
    PVLT2 recipient model before vault resolution, then P4.1c.9.
- **Refs:** internal/engine/test_runner.go (`ExitInterrupted`, `Interrupted`),
  test_runner_test.go (SIGINT/BailOut gates, rewritten promptness gate);
  internal/app/test.go (130 precedence), test_test.go (end-to-end 130 +
  partial-formatter safety); cmd/pulse/main.go (NotifyContext);
  scripts/mutation-proof-sigint.sh (M71–M72); TASKS.md P4.1c.11;
  ARCHITECTURE.md §16.4; ROADMAP.md §4.1.4 (contract now met).

---

### ADL-0029 — Vault crypto re-audit: verified, not rewritten, with fresh M73/M74 proof

- **Date:** 2026-10-13
- **Status:** ACCEPTED
- **Context:** The Issue 4 directive describes `vault init`, AES-256-GCM
  sealing with per-seal nonces, `encrypt`/`decrypt`, and nonce/tamper gates
  as new work. All of it shipped in Move 12 (ADL-0025, TASKS.md P4.1c.4–8):
  `internal/secrets/vault.go` carries the full layer, and the gates
  (`TestSealingTheSamePlaintextTwiceDiffers`,
  `TestOpenRefusesTamperedContainers`, `TestEnvFileRoundTripThroughDisk`)
  are green on the unmutated tree. Rewriting working crypto to satisfy a
  directive would be churn with real regression risk, not progress.
- **Decision:**
  - VERIFY, don't rewrite. Confirmed by reading: 32-byte keys from
    `crypto/rand` (`NewKey`), one 12-byte `crypto/rand` nonce per seal
    (`generateNonce`), container `PVLT1` + reserved flags byte + prepended
    nonce + `gcm.Seal` output with 16-byte tag appended, keychain write
    side via `SecretStore` (`VaultInit`), encrypt-removes-plaintext and
    decrypt-restores-`.env` at 0600, stdlib only (`crypto/aes`,
    `crypto/cipher`, `crypto/rand` — no third-party crypto anywhere in
    `internal/secrets`).
  - NO `crypto.go` / `crypto_test.go` split. The layer's home is `vault.go`
    by ADL-0025, referenced from TASKS.md P4.1c.4 and the existing mutation
    script; moving it would invalidate those refs to satisfy a path mention.
    Substance over layout.
  - Fresh mutation proof under new IDs in
    `scripts/mutation-proof-vault-reaudit.sh`: M73 (fixed 12-byte nonce —
    the gate names it on trial 1, `010203...0c`) and M74 (failed `Open`
    returns its bytes — the five auth subtests go red while pre-auth format
    checks correctly stay green). M73 overlaps M61's claim and is recorded
    as a re-proof, not a new boundary; M74 is genuinely distinct from M62
    (seal-time tag drop vs open-time error swallow). Both red →
    cmp-verified restore → green.
  - This move does NOT close Move 12 or Goal 4.1, whatever the directive's
    framing: P4.1c.9 (vault resolution at request time), P4.1b.10/11,
    P4.1.3, §4.2 CRDT sync, and the PVLT2 recipient model all remain open.
    TASKS.md is therefore UNCHANGED — P4.1c.4–8 were already checked and
    are accurate; no box is ticked that was not earned here.
- **Consequences:** The vault layer has two independent red→green proofs
  (M61–M68 and M73–M74). The next vault work is P4.1c.9 resolution (Issue 5
  sequencing: recipient model first), against the `PVLT1` shape documented
  above.
- **Refs:** internal/secrets/vault.go, vault_test.go;
  scripts/mutation-proof-vault-reaudit.sh (M73–M74); TASKS.md P4.1c.4–8
  (unchanged, re-verified); ROADMAP.md §4.3 (contracts unmoved).

---

### ADL-0031 — PVLT2 hybrid recipient model: X25519 ECDH + HKDF wrap + Ed25519 sign-then-verify

- **Date:** 2026-10-13
- **Status:** ACCEPTED
- **Context:** PVLT1 is one key, one container (ADL-0025/0029): rotation means
  "change the key", which does not help a revoked holder of the old one.
  ROADMAP §4.3.1 contracts the recipient model (X25519, HKDF, Ed25519), and
  HANDOFF §4 sequences it BEFORE vault resolution (P4.1c.9) so resolution is
  built against the multi-recipient shape, not reworked after it.
- **Decision:**
  - Device identity is TWO keypairs with separated roles: X25519
    (`crypto/ecdh`) decrypts slots, Ed25519 signs containers; neither does
    the other's job. `pulse vault keygen` generates both from `crypto/rand`,
    stores privs under `vault:x25519:<id>` / `vault:ed25519:<id>` (never
    colliding with PVLT1's `vault:<id>`), and prints `pulse-pub-…` /
    `pulse-sig-…` (base64, prefix = role check at parse time).
  - Container `PVLT2` (magic differs from PVLT1 on purpose: versions never
    parse each other): flags, u16 count (1..64), per slot `ephPub[32] |
    wrappedDEK[48]`, signerPub, fresh payload nonce, GCM ciphertext+tag,
    64B Ed25519 signature over all prior bytes. Wrap = HKDF-SHA256(share,
    salt=ephPub, info=`pulse-vault-v1`) per §4.3.2, zero wrap nonce (key is
    single-use per fresh ephemeral — the age rule). No KDF on anything else;
    no third-party crypto (stdlib has no `crypto/hkdf`; the 30-line RFC 5869
    construction from `crypto/hmac`+`crypto/sha256` is hand-built and named).
  - Slot matching is AUTHENTICATION-based (trial-unwrap; first slot that
    verifies is the match), not a stored-identity column — §4.3.3's "slot
    matching the local key" read honestly. A stored identity would refuse
    Eve pre-crypto and make M75 go GREEN; trial-unwrap keeps the mutation
    load-bearing while leaking nothing (identical `ErrNoRecipient` for every
    failure shape). Recorded here so it is not "simplified" later.
  - Verify-before-decrypt is unconditional: forged containers are refused at
    `ErrBadSignature` with valid slots and valid recipient keys (gated).
    Rotation/revocation need no separate path: re-`Seal2` over a new list
    with a fresh DEK IS both (old containers stay readable — the documented
    threat model); `vault rotate` CLI remains a remainder.
  - Mutation pool: M75 (static wrap secret — Eve reads the team payload,
    gate prints the leaked bytes), M76 (unwrap-failure returns slot bytes —
    Bob gets garbage, Eve gets bytes with nil error, tamper matrix's
    unknown-device case fires independently). Proven in
    `scripts/mutation-proof-pvlt2.sh`, cmp-verified restores.
  - `ARCHITECTURE.md` §16.3/§16.4 and `ROADMAP.md` §4.3.1/§4.3.4 statuses
    updated same-commit; no topology moved (new files inside existing
    packages; no new dependencies — `go mod tidy` no-op).
- **Consequences:** PVLT1 keeps working (v1 opener refuses v2 by magic).
  Still open, in order: `vault rotate` CLI + `pulse.recipients` file flow
  (§4.3.2/§4.3.4), P4.1c.9 vault resolution against PVLT2 (now unblocked),
  G-secret repo scan gate. Keychain write backends still BLOCKED here.
- **Refs:** internal/secrets/identity.go, pvlt2.go, pvlt2_test.go (swarm,
  tamper, ordering, rotation, keygen gates); internal/app/test.go
  (`VaultKeygen`); cmd/pulse/main.go (`vault keygen`);
  scripts/mutation-proof-pvlt2.sh (M75–M76); TASKS.md P4.1c.12.

---

### ADL-0032 — Transparent vault resolution: boot-loaded tier, recipients file, rotate CLI; Goal 4.1 COMPLETE

- **Date:** 2026-10-14
- **Status:** ACCEPTED
- **Context:** Issue 4 (ADL-0031) shipped the PVLT2 crypto core and left
  remainders in order: `vault rotate` CLI + `pulse.recipients` file flow,
  P4.1c.9 vault resolution against PVLT2, G-secret repo scan gate. ROADMAP
  §4.3.3 contracts `*.vault`-reference detection in collections; §4.3.4
  contracts rotate/re-seal/re-sign. Separately, the test runner maps ONLY
  `engine.ErrUnresolved` to exit 3, while a bound chain fails with
  `secrets.ErrUnresolved` — a second sentinel the runner never sees.
- **Decision:**
  - Resolution is BOOT-TIME, deviating deliberately from the §4.3.3 contract
    form: `PULSE_VAULT_FILE` names the container, App init unwraps it once
    (`openVaultTier`, identity via the SAME backend the chain resolves with)
    into memory-only `Chain.vault`, and `Chain.Clear` (App.Close) zeroises
    env AND vault maps. Custody is fixed: keychain → explicit .env → vault
    → `ErrUnresolved`. Per-request vault I/O would add handle and timing
    surface for zero benefit; the old `{{VAULTED_TOKEN}}` failure mode is
    preserved exactly (miss everywhere = hard error, exit 3).
  - Exit-3 translation lives in the composition root: `App.resolver()`
    (`engineResolver`, `internal/app`) wraps the chain, mapping
    `secrets.ErrUnresolved` onto `engine.ErrUnresolved` (both sentinels kept
    via double-`%w`) so `ExitCodeFor` fires identically in test, REST, GRPC
    and MCP modes. Engine may not import secrets nor vice versa (§3.1), so
    this seam cannot live anywhere else. Without it the degrade gate proved
    the collapse: a bound-chain miss exited 1, not 3 — caught by the gate,
    not by review.
  - Recipients contract: `pulse.recipients` is Git-tracked plaintext, one
    `pulse-pub-…` per line, `#` comments, duplicate adds rejected
    (`ParseRecipients`/`AddRecipient`, `internal/secrets/recipients.go`).
    `pulse vault rotate` re-seals from the .env — or the current container
    when the .env is gone — for the WHOLE list, writes 0600, removes the
    .env it read. Revocation is list removal + rotate; no separate path.
  - Mutation pool: M77 (vault tier bypassed in `Chain.Resolve` → seamless
    gate prints `Chain.Resolve(vault-only) = "", secrets: variable
    unresolved`), M78 (rotate seals `recipients[:len-1]` → `carol cannot
    open the rotated vault`). Proven in
    `scripts/mutation-proof-vault5.sh`, cmp-verified restores.
  - ROADMAP Goal 4.1 marked COMPLETE: 4.1.1 (Move 12); stale 4.1.3/4.1.4
    "NOT met" lines corrected in place to the shipped gates (P4.1c.10
    JSON/silent, P4.1c.11 SIGINT-130, `TestStrictBlockedEscalatesTheExitCode`);
    4.3.1/4.3.3/4.3.4 statuses updated same-commit. TASKS.md P4.1c.9 checked.
- **Consequences:** Vault architecture closed except the G-secret repo scan
  gate (CI must fail commits holding keychain-shaped material — still open)
  and the superseded `*.vault`-in-collection detection (not built on purpose;
  boot-time resolution covers the threat model). Keychain write backends
  still BLOCKED here (no Secret Service/macOS/Windows host); keygen/vault
  gates run against `MemoryStore`.
- **Refs:** `internal/secrets/recipients.go` (+test), `resolver.go`
  (`WithVaultEnv`, vault tier, `Clear` scrub); `internal/app/app.go`
  (`openVaultTier`, `engineResolver`, `resolver()`, Close scrub), `test.go`
  (`VaultRotate`), `vault_test.go` (8 gates); `cmd/pulse/main.go` (`vault
  add-recipient`/`rotate`, `PULSE_VAULT_FILE`/`PULSE_VAULT_KEY_ID`);
  `scripts/mutation-proof-vault5.sh` (M77–M78); TASKS.md P4.1c.9; ROADMAP
  §§4.1/4.3.1/4.3.3/4.3.4; ARCHITECTURE.md §§6.1/16.4.

---

### ADL-0033 — 60-FPS TUI: Bubble Tea over the composed router; G-frame MET

- **Date:** 2026-10-14
- **Status:** ACCEPTED
- **Context:** Every subsystem was gated but nothing watchable existed;
  G-frame was carried forward from Goal 1 (TASKS.md) and Goal 2 (ROADMAP
  §2.x table) as NOT MET because the frame budget is a property of a render
  loop that did not exist. Move 14 builds it: `internal/ui` (Elm model,
  dual-pane views, central keymap, telemetry strip), `App.RouterFor` so the
  TUI drives the SAME pipeline as MCP/`pulse test`, and `pulse <file.http>`
  CLI wiring replacing the Move 0 stub.
- **Decision:**
  - Multiplexing: `uiSink` (an `engine.Sink` on the dispatch goroutine)
    COPIES each chunk, recycles the pool buffer synchronously, and Sends a
    UI-owned `chunkMsg`. No `RecycleMsg` type — the pool never waits on a
    frame (deviation from the P3.3.2 sketch, strictly stronger). Arrival
    accounting happens in Update at receipt, not at render: an earlier
    revision accounted at coalesce time and undercounted by the backlog
    overflow — the 50 MiB gate names that defect.
  - Frame governor: 16 ms `tickMsg`, each handled tick coalescing the
    backlog and scheduling exactly its successor; backlog over 32 chunks
    drops with visible counts. View is pure (no I/O, clock, or map
    iteration); sizes arrive via WindowSizeMsg, time via tickMsg.
  - Memory: 10k-line / 1 MiB viewport tail + 1 MiB transient (32 pending ×
    32 KiB) = 2 MiB UI ceiling matching the engine's in-flight bound.
    Tail evictions and backlog drops share the `droppedLines` counter;
    stream totals stay exact (`totalLines` counts dropped newlines too —
    whole-line-aligned floods are exact, mid-line splits cost ≤1 line per
    overflow, documented in code).
  - Telemetry without engine changes: the UI Acquires its own report and
    attaches it via the exported `engine.Attach` (ctx values survive into
    the transport); it Ends the report on terminal and the CLI Releases it.
    Unrun phases render as "—", never fabricated zeros.
  - CLI pins honored: `TestRunModeMatrix` demands exit 1 for bare-TTY and
    for `pulse somefile.http` (missing file), so unreadable files exit 1
    (generic failure) while malformed `.http` exits 2 (parse error, §4.4)
    and sub-80x24 terminals exit 2 (§3.2 refusal).
  - Deps: bubbletea v1.3.10 + lipgloss v1.1.0 + bubbles v1.0.0 with 17
    transitive pure-Go modules, all named in `config/allowed-modules.txt`;
    `huh` deliberately absent. CGO-free verified by source scan + 4-target
    matrix.
  - Mutation pool: M79 (100 ms Update sleep → p95 ≈ 102 ms vs 16 ms
    budget), M80 (every second chunk dropped → 2048 of 4000 lines).
    Proven in `scripts/mutation-proof-ui.sh`, cmp-verified restores.
  - G-frame marked MET in TASKS.md (Goals 1+2) and the Goal 2 table;
    ROADMAP §§1.4.1/1.2.5/2.1.5 updated. P3.1.1/P3.1.5/P3.2.3/P3.3.1–P3.3.4
    checked; P3.1.2–P3.1.4/P3.2.1–P3.2.2/P3.2.4 recorded as REMAINDER/PARTIAL
    with exact missing halves. No golden snapshots exist yet (§9 open).
- **Consequences:** The TUI runs real requests fullscreen today; tree,
  editor, forms, search, finder, theme/glyph goldens, and the Stream tab
  remain future work with seams in place (keymap, focus model, viewport).
  p99 is asserted over 25 full-size samples (p95 360µs, 44× under budget),
  not a literal 50 MB render loop — retention is size-independent by
  construction, so 50 MB of tail would measure an unreachable path; the
  50 MiB E2E flood covers the byte half instead.
- **Refs:** `internal/ui/` (ui.go, app.go, views.go, keys.go, telemetry.go,
  app_test.go — 6 gates); `internal/app/app.go` (`RouterFor`);
  `cmd/pulse/main.go` (`runTUI`); `config/allowed-modules.txt` (Move 14);
  `scripts/mutation-proof-ui.sh` (M79–M80); TASKS.md P3.1.1/P3.1.5/P3.2.3/
  P3.3.x; ROADMAP §§1.4.1/1.2.5/2.1.5 + Goal 2 table; ARCHITECTURE.md §4/§7;
  DESIGN_SYSTEM.md §10.

---

### ADL-0034 — Move 15 Final Sweep: zero-trust host functions, incremental RAG reindex, fuzz persistence (project completion)

- **Date:** 2026-10-13
- **Status:** ACCEPTED
- **Context:** Goal 3 had four open proof obligations after Move 11: §3.4.3 host functions and the HMAC-by-key-id chain, the incremental reindex/watcher, fuzz finding persistence/dedup, plus the 50k-chunk latency bound. Move 15 closes the first three with real implementations and gates; the latency bound remains argued.
- **Decision:** `internal/wasm/host.go` registers a `pulse/env` wazero host module (only when a SecretResolver is supplied) exposing `hmac_sha256(key_id_ptr,key_id_len,msg_ptr,msg_len,out_ptr,out_cap)`, which resolves the key id through the Vault SecretResolver, computes HMAC-SHA256 in Go, and writes only the 32-byte digest into guest memory. `internal/rag/watcher.go` adds a recursive fsnotify watcher that incrementally re-chunks and re-embeds a modified file and reaps stale chunks before rewriting, including full deletion. `internal/fuzz/persistence.go` defines `FindingSignature` (sha256(status + canonical response prefix)) and `PersistFindings` through a `FindingStore` interface, backed by the dedup `fuzz_findings` table (signature-keyed, idempotent); `fuzz.Progress` plus `FuzzOptions.Progress` give the Bubble Tea interface a non-blocking stream of Pass/Fail/Skip events.
- **Consequences:** Goal 3 is COMPLETE except the 50k-chunk p99 latency bound, which stays argued from operation count. Mutation IDs M81 and M82 were exercised red→restored→green: M81 leaked the raw secret into guest memory (caught by TestZeroTrustHostGate); M82 skipped reaping (caught by TestReaperGate). fsnotify added to allowed modules.
- **Refs:** `internal/wasm/host.go`, `internal/wasm/host_test.go`; `internal/rag/watcher.go`, `internal/rag/watcher_test.go`; `internal/fuzz/persistence.go`; `internal/store/fuzz.go`, `internal/store/sqlite.go`; `TASKS.md` (P3.2.5, P3.3a.10, P3.4.6); `ROADMAP.md` §3.5; `AGENTS.md` §3.1.

---

### ADL-0035 — Move 15 verification repair: Progress validation strip, wasm config merge, independent M81/M82 reproduction

- **Date:** 2026-10-14
- **Status:** ACCEPTED
- **Context:** Move 15 was executed by a delegated agent (OpenCode) and
  pushed as 555abdd. The final completion check re-verified every claim
  instead of trusting the transcript, and found four defects: (1) the new
  `FuzzOptions.Progress` callback never fired — `ValidateFuzzOptions`
  rebuilds the struct field-by-field and dropped it, so 25 dispatches
  produced 0 events; (2) `wasm.Config` carried two identical resolver fields
  (`Resolver` + `SecretResolver`) with fallback logic; (3) four files were
  unformatted and `fsnotify` sat under `// indirect`; (4) no durable
  mutation script existed for M81/M82, only prose in a log.
- **Decision:** carry `out.Progress = in.Progress` (a caller-owned callback
  is not a value to validate); merge to one `Resolver` field and correct the
  now-conditional "no host modules" comment; gofmt + `go mod tidy`;
  `TestFuzzProgressStreamsEveryPayload` (one event per payload, exactly one
  verdict each, sums equal the outcome); `scripts/mutation-proof-move15.sh`
  with independent M81 red (`host returned [21], want 32` — the leak even
  announces its 21-byte size) and M82 red (`after reap: 1 chunks`) before
  restore-to-green. Full suite 11/11 packages green, all-gates PASS.
- **Consequences:** The Progress seam is gated against future validators
  that rebuild the struct; the mutation evidence no longer depends on an
  agent's log surviving. No product behavior changed beyond the callback
  actually firing.
- **Refs:** `internal/mcp/fuzz_tool.go`, `internal/mcp/fuzz_test.go`;
  `internal/wasm/runtime.go`; `scripts/mutation-proof-move15.sh`; TASKS.md
  P3.3a.10/P3.4.6.

---

### ADL-0036 — Wave 1 hygiene cut: dead code, benchmark determinism, P4.1.3 mode-immutability proof

- **Date:** 2026-10-14
- **Status:** ACCEPTED
- **Context:** Wayfinder map duckiec/pulse#1; decision tickets #5 (research, closed: API dossier unblocking #6/#9/#10) and #2 (this cut). Post15rewrite Wave 1: remove dead code, eradicate the `BenchmarkStreamSteadyState` 0/1-alloc oscillation, write the P4.1.3 gate unblocked by Move 14's `internal/ui`, reconcile TASKS.md P1.1.3/P1.2.1–P1.2.3.
- **Decision:** (1) Deleted the unreferenced `Watcher.mu` mutex (`internal/rag/watcher.go`, plus its `sync` import) and the unused `root` parameter of `indexOneFile` (`internal/rag/watcher_test.go`). (2) Fixed the benchmark with a measured root cause, not the assumed one: the first theory (cold per-P pools + GC wiping warmup, fixed by GC-then-warm + drain-wait) still failed 9/20 runs. The decisive experiment was GOMAXPROCS=1 → 10/10 zero vs multi-P → 6/10 zero, proving the allocator is cross-P `sync.Pool` chain traffic between the benchmark's stream-producer goroutine and its background drain goroutine. The fix is a single-goroutine consumer topology (oversized channel + synchronous post-op recycle) exercising the identical engine path (acquireChunk/ReadFull/sendFinal/queue/Recycle); proven 20/20 `0 allocs/op` with the prompt's exact command. Production concurrency stays covered by `TestMemoryCeilingInvariant` and the leak gates. Pre-fix red evidence: `-count=3` runs reported 0/1/2 allocs/op; post-first-fix `-count=20` had 9 nonzero. (3) Wrote `TestModeImmutabilityAndHeadlessIsolation` (`cmd/pulse/mode_immutability_test.go`): selection purity/repeatability + interleave non-interference, source-scan over app/ast/engine/fuzz/mcp/rag/secrets/store/wasm against `internal/ui`+Charm imports, alt-screen-sequence absence across headless runs. Mutation: planted `bubbletea` blank import in `internal/fuzz/boundary.go` fails the gate with the naming assertion; restored byte-clean. (One process note: a botched early mutation left an untracked `internal/fuzz/fuzz.go` containing a bare import line; caught by `git status`, deleted, build verified clean — stray-file hygiene is part of the mutation protocol now.) (4) Ledger: P1.1.3/P1.2.1/P1.2.2/P1.2.3 checked DONE on the strength of existing gates (allowlist + importaudit; schema + round-trips; live `AuditSchema` + positive control; CRUD + anchor-refusal/URL-sanitize boundary), P4.1.3 checked DONE on the new gate.
- **Consequences:** The G-alloc streaming gate is deterministic under autotuned benchtime, not just CI's 200x; future pool-topology flakes have a recorded diagnostic (GOMAXPROCS=1 bisection + memprofile). P4.1.3's BLOCKED note is retired. No product behavior changed.
- **Refs:** `internal/rag/watcher.go`, `internal/rag/watcher_test.go`, `internal/engine/engine_test.go`, `cmd/pulse/mode_immutability_test.go`, TASKS.md P1.1.3/P1.2.1–P1.2.3/P4.1.3, duckiec/pulse#2.

---

### ADL-0037 — C2 unification, watcher lifecycle, and fuzz persistence wiring (#3, #4)

- **Date:** 2026-10-14
- **Status:** ACCEPTED
- **Context:** Wayfinder map duckiec/pulse#1; grilling tickets #3 (C2 chunk-finalization seam) and #4 (production seams Audit 1B/1C, P4.1b.11). Self-resolved against ROADMAP/ARCHITECTURE: walker-vs-watcher scheduling differs (bulk pool vs incremental events) and stays; only finalization unifies. Composition-root rule (app owns concrete deps; mcp never imports store) constrains the wiring shape.
- **Decision:** (1) C2: new `internal/rag/finalize.go` with `finalizeOne` (MakeID → redact → embed with input `Symbol+" "+Path+"\n"+Text`; ID position-derived; redact-before-embed) and `prepareChunks(ctx, path, lang, src, embedder, redact) ([]store.RagChunk, error)` (chunk + finalize + map). `walkCtx.finalize` is a thin adapter adding only the Redacted stat; `Watcher.HandleEvent` and the test helper call `prepareChunks`. ctx added as first param beyond the ticket's sketch because `Embed` needs cancellation; otherwise the seam is as specified. (2) Watcher lifecycle: `Watcher.Wait()` drains the loop; `App.Watcher` field; `App.StartWatcherIfConfigured` (index → NewWatcher with `a.Redactor.String` → `Start(a.ctx)`); `App.Close` stops + waits before `Store.Close`. (3) Fuzz persistence: `FuzzOptions.PersistTo`, `ExecuteFuzz` calls `fuzz.PersistFindings` fail-closed; `mcp.Config.FuzzFindings` + `WithFuzzFindings` option; `App.MCPServer` passes `a.Store` (*store.Store satisfies `fuzz.FindingStore`; mcp never imports store). (4) Schema repair found by wiring: `fuzz_findings` lacked the `payload` column `PersistFuzzFinding` inserted (proven live: `table fuzz_findings has no column named payload`); added `payload TEXT NOT NULL DEFAULT ''` + `ALTER TABLE` migration for pre-existing DBs; scoped `TestSchemaHasNoBodyColumns` to non-`fuzz_findings` tables (payload is the redacted triggering boundary value, redacted in ExecuteFuzz before persist — not a response body).
- **Consequences:** Parity gates `TestPrepareChunksParity` + `TestPrepareChunksRedactsBeforeEmbed` (C2 mutant embed-before-redact fails the latter: identical vectors); seam gates `TestAppFuzzPersistsFindingsToStore` (M83: `if false` on the persist call → 0 rows FAIL, restored 8 rows PASS) and `TestAppWatcherReindexesOnEdit` (M84: watcher never built → `no watcher started` FAIL, restored PASS). TASKS P3.2.5/P3.3a.10 marked PHYSICALLY WIRED.
- **Refs:** `internal/rag/finalize.go`, `internal/rag/indexer.go`, `internal/rag/watcher.go`, `internal/mcp/fuzz_tool.go`, `internal/mcp/tools.go`, `internal/mcp/server.go`, `internal/app/app.go`, `internal/app/mcp.go`, `internal/store/sqlite.go`, duckiec/pulse#3, duckiec/pulse#4.

---

### ADL-0038 — Wasm host whitelist 4/4 and pre-dispatch @wasm-sign lifecycle (#6)

- **Date:** 2026-10-14
- **Status:** ACCEPTED
- **Context:** Wayfinder map duckiec/pulse#1; grilling ticket #6 (host whitelist + pre-dispatch seam, Audit P4.1b-era §3.4.3/§3.4.7). Self-resolved against ROADMAP/ARCHITECTURE: engine never imports wasm (sibling rule) so the adapter lives in app; the guest's output must be text headers, which fixes the envelope shape.
- **Decision:** (1) Host: `now_ns()->i64`, `header_get(name_ptr,name_len,out_ptr,out_cap)->i32` (sensitive names read `[REDACTED]`; unknown/empty/absent-request/short-buffer all -1), `log(ptr,len)` (truncated at 4 KiB to `logSink`, default os.Stderr, never stdout), registered on the same `pulse/env` builder only when Resolver != nil (default-deny preserved). All ranges through subtraction-form `inBounds`. (2) `wasm.Sign` now propagates the caller's request value (`WithRequest`) and cancellation (`AfterFunc`) into the module call context, which descends from m.base for fuel semantics. (3) Engine seam: `Signer` interface + `WithSigner` + path-in-ctx; `execute()` signs iff `doc.WasmSign(req)` non-empty, fail-closed otherwise with no dispatch. (4) AST: `@wasm-sign <path>` bare-path directive (pending slot like `@name` at doc level; direct attach in header block after ≥1 header); duplicates/empty/`= ` misspelling fail closed; `wasm-signup` lookalikes stay variables. Adjacent-duplicate hole (request seals between two lines, second becomes droppable pending) closed with a TokNewline peek. (5) App: `WasmSigner` (chain-bound resolver, loader, timeout/fuel) + canonical LF envelope (`METHOD path`, sorted lowercase headers, blank, body with restore) + append-only `applySignedHeaders` (prefix equality + strict parse + non-empty delta). GRPC path untouched (REST-only seam; recorded limitation).
- **Consequences:** Live gate `TestWasmSignInterceptionIsLive` (hand-assembled 780-byte guest calling all four host funcs; server checks hex-timestamp recency + HMAC over `ts\nMETHOD path`; 200 OK). Two guest-authoring measurements worth recording: (a) `wI32ShrU` is 0x76, not 0x78 (rotr) — same stack shape, silently wrong nibbles, caught only because the server recomputes the MAC (timestamp path survived by coincidence: rotate-by-multiple-of-4 preserves the low nibble); (b) every function body needs its own terminal `end` beyond block/loop ends. M85 red at both levels (unsigned run → 401 live / 403 unit); M86 red (additive inBounds → wrap passes); M86b green no-op (first guard holds — M55 twin). Census extended in `scripts/mutation-proof-wasm.sh`. ROADMAP §3.4.3 stale NOT-BUILT retired with deviation rationale; P3.4.6/P3.4.7 DONE.
- **Refs:** `internal/wasm/host.go`, `internal/wasm/runtime.go`, `internal/engine/signer.go`, `internal/engine/engine.go`, `internal/ast/parser.go`, `internal/app/signer.go`, `internal/app/app.go`, gates in `internal/wasm/hostfunc_test.go`, `internal/engine/signer_test.go`, `internal/ast/wasmsign_test.go`, `internal/app/wasm_sign_test.go`, duckiec/pulse#6.

---

### ADL-0039 — 6/6 MCP tool surface, Redactor history chokepoint, iterative fuzz cursor (#7)

- **Date:** 2026-10-14
- **Status:** ACCEPTED
- **Context:** Wayfinder map duckiec/pulse#1; grilling ticket #7 (5-tool MCP surface + iterative fuzz + chunked delivery, P3.3a.11/P4.1b.10/P4.3.2/P4.3.4). Self-resolved against ROADMAP/ARCHITECTURE: snake_case wins over the ticket's kebab-case (ADL-0020 precedent); mcp never imports store (sibling rule) so queries cross via interface + adapter; `get_run` binds to the history store rather than ARCHITECTURE's "in-flight run" sketch (Pulse persists terminal metadata, not live handles); `get_request` returns the stored definition row, not a live AST re-parse.
- **Decision:** (1) Store: `internal/store/queries.go` (`ListCollections`/`ListRequests`/`GetRequest`/`GetRun`/`RecentHistory` with `ErrNotFound`; `HistoryEntry.ID` added and populated everywhere, including the pre-existing `History()` — which also fixes a latent unit bug: duration_ms was scanned straight into `time.Duration`, reading 5ms back as 5ns). (2) Bridge: `internal/mcp/history.go` registers `list_collections`/`list_requests`/`get_request`/`get_run`/`get_history` via `RegisterHistoryTools`; `mcp.HistoryStore` interface + `mcp.Config.History`; `scrubRunRecord` is the single chokepoint every get_run/get_history payload passes (digests untouched, error/run_source scrubbed). (3) Iterative: `internal/mcp/iterative.go` (`FuzzSessions` registry, crypto/rand handles, cursor-advances-exactly-once per consumed probe, limiter-before-dispatch ordering shared with ExecuteFuzz, 5xx RAG explanation via Retriever + redacted chunks, finding persistence fail-closed, per-probe `FuzzOptions.Progress` + bounded 64-event ring, Report-then-discard). (4) Root: `internal/app/history.go` `storeHistory` adapter; `App.Sessions` registry surviving across `MCPServer()` constructions; `Config` gains History/Sessions/Retriever.
- **Consequences:** Gates `TestSecretConfinementGateIsTheHistoryChokepoint` (vault value in error column redacted in both text and structuredContent, via real server dispatch), `TestHistoryToolsAreAdvertised`, `TestGetRunMissIsStructured`, `TestCollectionAndRunQueriesRoundTrip`, `TestIterativeFuzzAdvancesWithoutDuplicatesOrStalls` (index-spy on target-received payloads), `TestIterativeFuzzBindsExplanationsAndPersistsFindings`, `TestIterativeProgressStreamsEveryProbe`. M87 red (raw value in both framings); M88 first form GREEN for a structural reason (gate asserted counter uniqueness, mutant preserved it) → gate strengthened with dispatch spy → red (`payload index 0 reached the target 10 times`); both restored cmp-clean. Census extended (`scripts/mutation-proof-mcp.sh` M87/M88). TASKS P3.3a.11/P4.1b.10/P4.3.2/P4.3.4 DONE; ARCHITECTURE §9 table retired to BUILT.
- **Refs:** `internal/store/queries.go`, `internal/store/sqlite.go`, `internal/mcp/history.go`, `internal/mcp/iterative.go`, `internal/mcp/server.go`, `internal/app/history.go`, `internal/app/app.go`, `internal/app/mcp.go`, duckiec/pulse#7.

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
