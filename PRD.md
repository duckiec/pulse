# PRD.md — Pulse Product Requirements Document

| Field | Value |
|---|---|
| Document | Product Requirements Document |
| Product | Pulse |
| Version | 1.0.0-draft |
| Status | Authoritative |
| Governed by | `AGENTS.md` (all directives herein are subordinate to AGENTS.md) |
| Last updated | 2026-10-11 |

---

## 1. Problem Statement

The incumbent API clients have degraded into cloud-locked productivity suites.
Accounts are mandatory, collections sync to vendor servers, request payloads transit
third-party infrastructure, and the runtime cost has ballooned into an Electron
application measured in gigabytes of RAM. For two audiences this model is broken:

1. **Terminal-native engineers** who want a 60-FPS keyboard-driven workbench, not a
   browser tab wearing a native costume.
2. **Local AI coding agents** (Cursor, Claude Code, etc.) which need to execute API
   tests from inside the agent loop but cannot send request bodies, headers, or
   environment files to a cloud service — and whose clients crash the moment a
   subprocess corrupts stdio.

Pulse exists because API testing must be **offline, Git-native, and scriptable by
local intelligence** — none of which cloud clients can be by construction.

## 2. Product Vision

Pulse is a strictly offline, Git-native, dual-mode API workbench written in pure Go.

- **For humans:** a high-framerate terminal UI that feels like `vim`, `lazygit`, and
  `htop` had a child — instant keystroke response, zero electron, zero accounts.
- **For machines:** a headless Model Context Protocol (MCP) server that exposes the
  exact same request engine to local LLMs over JSON-RPC on stdio, with strict
  transport segregation so the wire protocol is never corrupted.

One engine. Two clients. No cloud.

## 3. Target Audience

### 3.1 Terminal Purists

| Attribute | Detail |
|---|---|
| Environment | Lives in the terminal; tmux + vim/neovim workflows |
| Tooling expectations | Keyboard-first, Vim motions, Nerd Fonts, truecolor terminal |
| Pain points today | Electron clients break keyboard flow, require login, sync code to the cloud |
| Success signal | Reaches for `pulse` before opening a GUI client or curl |

### 3.2 Local AI Coding Agents

| Attribute | Detail |
|---|---|
| Environment | Cursor, Claude Code/Desktop, and any MCP-conformant client |
| Discovery | MCP stdio handshake; tools enumerated per spec |
| Pain points today | Cloud-only APIs; agents cannot invoke private/localhost APIs; stdio corruption from sloppy subprocesses |
| Success signal | Agent reliably runs `.http` requests and inspects responses without any human step and without any network call leaving the machine |

## 4. Core Value Loop

### 4.1 Human Loop (Interactive Mode)

```
git pull            → collection updated (.http files, plaintext, reviewable)
   ↓
launch `pulse`      → TUI in <100ms, no login, no splash
   ↓
navigate (j/k, Ctrl+P fuzzy find, / search)
   ↓
edit request in terminal → variables resolve from OS keychain or ephemeral .env
   ↓
Ctrl+S sends request → body streams in byte-chunks, UI never blocks
   ↓
inspect (headers, pretty/NDJSON body, timing, size) → iterate
   ↓
commit to git      → the collection IS the source of truth
```

Every step is local. Nothing above ever opens a socket to a Pulse-operated service,
because no such service exists.

### 4.2 Agent Loop (MCP Mode)

```
client spawns `pulse mcp`  → JSON-RPC initialize/initialized over stdio
   ↓
client calls tools (list-requests, get-request, run-request, get-history)
   ↓
Pulse resolves secrets from the user's OS keychain (never the cloud)
   ↓
request executes locally; response is summarized and returned over stdio
   ↓
client inspects results; agent adjusts code or request
```

The agent loop MUST NOT require: accounts, network egress to Pulse infrastructure,
unstructured stdout noise, or credential material exposed in tool results.

## 5. Functional Requirements

| ID | Requirement | Priority |
|---|---|---|
| FR-1 | Parse `.http` standard files (JetBrains/REST Client format) into a zero-allocation AST; support `###` separators, `@var = value` definitions, `{{var}}` interpolation | P0 |
| FR-2 | Execute HTTP requests (GET/POST/PUT/PATCH/DELETE/HEAD/OPTIONS) with streaming response bodies | P0 |
| FR-3 | Interactive TUI: collection tree, request editor, response viewer with tabs (Body / Headers / Timing / Stream) | P0 |
| FR-4 | MCP server mode with tools: `list-collections`, `list-requests`, `get-request`, `run-request`, `get-run`, `get-history` | P0 |
| FR-5 | OS keychain secret resolution + ephemeral in-memory `.env` loads; strict fail-closed on resolution failure | P0 |
| FR-6 | Persistent local history (SQLite, zero-CGO) of executed requests and response summaries | P1 |
| FR-7 | Variable resolution chain: `{{var}}` → keychain → `.env` → hard error with source location | P0 |
| FR-8 | Log/output redaction for sensitive headers and vault-sourced values in all sinks (stderr, UI, store) | P0 |
| FR-9 | Response body streaming with byte-chunk delivery to the UI at 60 FPS frame budget | P0 |
| FR-10 | Request timing, size, status, and header inspection for every execution | P1 |
| FR-11 | Graceful terminal degradation: Truecolor → 256-color → ANSI 16 → monochrome; Nerd Fonts → ASCII glyph fallback | P1 |
| FR-12 | Configuration via a local `pulse.toml` (theme, keybindings, glyph mode, motion); no account or cloud config | P2 |
| FR-13 | Native SSE and WebSocket streaming through the same zero-blocking engine pipeline (ROADMAP.md §2.1) | P1 |
| FR-14 | gRPC unary + server-streaming via dynamic protobuf reflection (ROADMAP.md §2.2) | P1 |
| FR-15 | Microsecond-precise TCP/TLS telemetry with eBPF opt-in, CGO-free (ROADMAP.md §2.3) | P2 |
| FR-16 | Local RAG over the user's codebase with 5xx-handler correlation (ROADMAP.md §3.2) | P2 |
| FR-17 | OpenAPI-driven auto-fuzzing through the MCP tool surface (ROADMAP.md §3.3) | P2 |
| FR-18 | wazero sandbox for auth-signing scripts with a closed host-import whitelist (ROADMAP.md §3.4) | P2 |
| FR-19 | Headless `pulse test` with TAP output and honest exit codes (ROADMAP.md §4.1) | P1 |
| FR-20 | CRDT P2P collection sync over mDNS/Tailscale, no server (ROADMAP.md §4.2) | P2 |
| FR-21 | Vault: commit-encrypted `.env` via X25519 recipients, private keys in keychain only (ROADMAP.md §4.3) | P2 |

## 6. Non-Functional Requirements

| ID | Requirement | Verification |
|---|---|---|
| NFR-1 | Single static binary; `CGO_ENABLED=0` for Linux/macOS/Windows | CI build matrix |
| NFR-2 | 60 FPS UI: `View` returns within the frame budget under a streaming 50MB+ response | Frame-time benchmark |
| NFR-3 | Cold start < 100 ms to first frame (TUI mode) | Benchmark in CI |
| NFR-4 | Zero network I/O on the Bubble Tea UI thread | `testing/synctest` + code audit; CI lint |
| NFR-5 | Zero allocations in the `.http` parse hot path | `-benchmem` gate: 0 allocs/op |
| NFR-6 | Secrets never persisted to disk (repo, store, temp, crash dumps) | Secret-scanner gate in CI |
| NFR-7 | MCP mode: zero bytes on stdout outside the JSON-RPC layer | CI stdout-hygiene analyzer |
| NFR-8 | Fully offline: no telemetry, no update checks, no phone-home (verifiable by strace/`-tags netaudit` smoke test) | CI offline audit |
| NFR-9 | Memory ceiling under streaming load: bounded by channel capacity + in-flight chunks | Stress test with 100MB payload |

## 7. Explicitly OUT OF SCOPE

This section is a hard boundary enforced by review. Proposing any item below is an
automatic PR rejection without a PRD amendment (which requires a MEMORY.md ADL entry).

> **Amendment (ADL-0008, 2026-10-11):** `ROADMAP.md` is the scope authority for
> Pulse. Goals 2–4 of the roadmap supersede rows 5, 6, and 8 below — SSE/WebSocket
> transport, gRPC, sandboxed extensibility, and auto-fuzzing are now IN SCOPE per
> `ROADMAP.md` and `MEMORY.md` ADL-0008. The remaining rows stay in force.

| # | Out of scope | Rationale |
|---|---|---|
| 1 | Cloud synchronization, accounts, login, or any Pulse-operated server | The product's raison d'être (roadmap P2P sync is peer-to-peer, no server) |
| 2 | Electron, web, or any GUI variant; browser views of any kind | Terminal-first is the mandate |
| 3 | CGO dependencies of any kind (`mattn/go-sqlite3` et al.) | Single-binary, cross-platform builds |
| 4 | Proprietary collection formats (Postman v2.x JSON, Insomnia, Bruno, etc.) — no importers, no exporters | Git-native `.http` is the only format |
| 5 | ~~Plugin systems, scripted pre/post-request hooks~~ **SUPERSEDED by ROADMAP.md Goal 3.4** | Wasm sandbox with a closed import whitelist is the sanctioned extensibility surface |
| 6 | ~~gRPC, GraphQL, WebSocket, or SSE transports~~ **SUPERSEDED by ROADMAP.md Goals 2.1–2.2** | Now in scope; GraphQL remains excluded (REST + gRPC suffice) |
| 7 | Team collaboration features, shared environments, comments | Git handles collaboration (roadmap CRDT sync covers collections only) |
| 8 | ~~Built-in mock servers, contract testing, fuzzing~~ **SUPERSEDED by ROADMAP.md Goal 3.3** | Auto-fuzzing is in scope via OpenAPI-driven local generation |
| 9 | Mobile or desktop packaging (app bundles, installers) | A static binary is the distribution |
| 10 | OAuth flows / token auto-refresh wizards | Secrets stay in the user's keychain; Pulse never brokers auth |
| 11 | Fuel-metered Wasm execution, JIT runtimes | wazero interpreter + caps + deadlines is the enforced boundary (ROADMAP.md §3.4.4) |

## 8. Success Metrics

| Metric | Target |
|---|---|
| Time from `git pull` to first executed request | < 60 seconds |
| UI frame time during 50MB response streaming | p99 < 16 ms |
| MCP client crash incidents (from stdout corruption) | 0 |
| Binary size | < 30 MB, zero CGO |
| Offline operation | 100% of features |
| CI honesty | 0 greenwashed/skipped-as-pass test results |

## 9. Release Criteria (v1.0.0)

1. All P0 requirements met and independently verified.
2. `CGO_ENABLED=0` builds pass on Linux (amd64/arm64), macOS (arm64), Windows (amd64).
3. TASKS.md Phases 1–4 fully checked with documentary evidence per AGENTS.md §4.2/§4.5.
4. MEMORY.md reflects every ADL entry with closed status or explicit deferral.
5. Zero open BLOCKED items on P0 features.
6. Every reached ROADMAP.md goal has its exit gate (X.4.1) green or an honest
   BLOCKED/NOT RUN disposition with a stated reason — never greenwashed.

---

*Amendments to this document require a MEMORY.md ADL entry and a dedicated commit
per AGENTS.md §4.6.*
