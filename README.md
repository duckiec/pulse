<p align="center">
  <img src="assets/banner.svg" alt="Pulse — offline, Git-native API workbench" width="100%" />
</p>

<p align="center">

**pulse**

The offline, Git-native API workbench.

```sh
curl -fsSL https://raw.githubusercontent.com/duckiec/pulse/main/install.sh | sh
```

```powershell
irm https://raw.githubusercontent.com/duckiec/pulse/main/install.ps1 | iex
```

`Go 1.27+` · `MIT` · `CGO-free` · `offline-first`

</p>

---

Cloud API clients store your collections on someone else's server and your tokens in their sync. Pulse keeps requests as plain `.http` files in your repo and resolves secrets at runtime. One engine drives a terminal UI for humans and a 16-tool MCP server for local agents. No accounts. No telemetry. No network to Pulse infrastructure, because none exists.

Requests stream in bounded 32 KiB chunks under a 2 MiB in-flight ceiling, so a 50 MB response renders without wedging the UI. Secrets live in the OS keychain, an ephemeral in-memory `.env`, or a sealed `.pulse.vault` — never in the file, the store, or the logs.

<p align="center">
  <img src="assets/terminal-preview.svg" alt="Pulse terminal session: init, vault seal, doctor, test" width="100%" />
</p>

## Quickstart

Install (above), then:

```sh
pulse init                  # scaffold quickstart.http; refuses to overwrite without --force
pulse api.http              # TUI: j/k scroll, Ctrl+S send, ? help; empty dir boots a scratchpad
pulse --mcp                 # MCP server over stdio (alias of `pulse mcp`); stdout is protocol only
pulse test . --format tap   # headless asserts; exit 0 pass, 1 fail, 2 usage/parse, 3 secret
```

A request in `api.http`:

```http
# @name get-user
# @assert status == 200
GET https://localhost:8080/users/1
Accept: application/json

###
```

Blocks end at `###`. Variables are `@base = https://…`, interpolation is `{{base}}`, signing is a bare `@wasm-sign ./sign/auth.wasm` line (one per request). Seal a token without leaving the terminal: `pulse vault set DEMO_TOKEN` (prompts with no echo, or reads piped stdin). Check the workspace: `pulse doctor`.

## Guarantees

| Pillar | What holds |
|---|---|
| ![offline](assets/icons/offline.svg) Offline | Works on an airplane. No socket to Pulse infrastructure exists to open. |
| ![git-native](assets/icons/git-native.svg) Git-native | Collections are text. Review and merge them like code. |
| ![dual-mode](assets/icons/dual-mode.svg) Dual-mode | TUI and MCP share the engine. Define a request once, drive it from either. |
| ![single binary](assets/icons/single-binary.svg) Single binary | One CGO-free executable on Linux, macOS, Windows (`CGO_ENABLED=0 go build ./...`, Go 1.27+). |

What Pulse does not claim: no startup-time figure and no rival memory number are published here, because none is measured in this repo. The measured budgets are the 2 MiB in-flight streaming ceiling (`internal/engine/stream.go`: 64×32 KiB pooled chunks), the 1 MiB UI tail, and the offline/no-telemetry invariant. Cloud alternatives require accounts and store collections server-side; Pulse stores history metadata in local SQLite and refuses secret-capable columns at every open.

Secrets resolve `keychain → ephemeral in-memory .env → sealed .pulse.vault → hard error (exit 3)`. Files carry `{{PLACEHOLDER}}` references only. Every sink — stderr, store rows, UI, MCP output — scrubs `Authorization`, `Proxy-Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key`, plus every sealed value, to `[REDACTED]`. Outbound requests carry the real secret; diagnostic copies never do.

The MCP server exposes 16 tools: `execute_request`, `execute_fuzz_run`, `list_collections`, `list_requests`, `get_request`, `get_run`, `get_history`, `fuzz_start`, `fuzz_next`, `fuzz_report`, `get_http_syntax_spec`, `create_collection`, `upsert_request`, `delete_request`, `import_curl`, `validate_collection`. Writes refuse literal credentials and name the `pulse vault set` remedy.

## Build

```sh
CGO_ENABLED=0 go build ./...
go vet ./...
bash scripts/all-gates.sh
```

Live keyring tests need a session bus:

```sh
bash scripts/ci/run-with-keyring.sh bash scripts/all-gates.sh
```

## Docs

- [QUICKSTART.md](docs/QUICKSTART.md) — five minutes to your first request
- [HTTP_SYNTAX.md](docs/HTTP_SYNTAX.md) — the `.http` file format
- [TUI_GUIDE.md](docs/TUI_GUIDE.md) — keybindings, panes, streaming views
- [MCP_COOKBOOK.md](docs/MCP_COOKBOOK.md) — driving Pulse from an AI agent

<details>
<summary>Codebase topology</summary>

| Package | Purpose |
|---|---|
| `cmd/pulse/` | Mode selection (`mcp`, `test`, `vault`, `init`, `doctor`, `sync`), exit codes |
| `internal/ast/` | `.http` lexer and parser, no I/O |
| `internal/engine/` | Transport, streaming, redaction |
| `internal/store/` | SQLite history (`modernc.org/sqlite`, CGO-free) |
| `internal/secrets/` | Keychain bindings, vault, redaction |
| `internal/rag/` | Local codebase index |
| `internal/fuzz/` | OpenAPI subset reader, boundary matrix, TAP 13 |
| `internal/mcp/` | JSON-RPC server over stdio |
| `internal/sync/` | P2P collection sync (HLC + CRDT, mTLS TOFU, zeroconf) |
| `internal/wasm/` | Sandboxed WASM signers (wazero, no ambient authority) |
| `internal/ui/` | Bubble Tea models, Lip Gloss styling |
| `internal/app/` | Composition root, lifecycle |

</details>

## License

MIT. See `LICENSE`.
