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
