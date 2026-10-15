# Pulse

![Pulse banner](assets/banner.svg)

Pulse is an offline, Git-native API workbench. You design HTTP requests in plain `.http` files, run them from a terminal UI, and let local agents run the same requests through an MCP server. One engine drives both clients. No cloud. No accounts. No telemetry.

## Use it

Write a request in `api.http`:

```http
### Get user
GET https://localhost:8080/users/1
Authorization: Bearer {{token}}
```

Run the TUI:

```sh
go run ./cmd/pulse --tui
```

You browse the file tree with `j/k`, open a request, press `Ctrl+S` to send. Bodies stream in chunks. The UI stays responsive under a 50 MB response.

Run the headless MCP server for agents:

```sh
go run ./cmd/pulse --mcp
```

Your agent calls `list-requests`, `get-request`, `run-request` over JSON-RPC on stdio. Stdout carries protocol bytes only. Diagnostics go to stderr.

Run requests without the UI:

```sh
go run ./cmd/pulse test --format tap
```

## Guarantees

| Pillar | What you get |
|---|---|
| ![offline](assets/icons/offline.svg) Offline | You work on an airplane. Pulse opens no socket to Pulse infrastructure, because none exists. |
| ![git-native](assets/icons/git-native.svg) Git-native | Collections are text. You review them in PRs and merge them like code. |
| ![dual-mode](assets/icons/dual-mode.svg) Dual-mode | TUI and MCP share the request engine. You define a request once and drive it from either client. |
| ![single binary](assets/icons/single-binary.svg) Single binary | You ship one CGO-free executable on Linux, macOS, Windows. |

Secrets resolve at runtime: OS keychain, then ephemeral in-memory `.env`, then a hard error. Pulse never writes a secret to disk, the store, or logs. Redaction covers `Authorization`, `Cookie`, `Set-Cookie`, `X-Api-Key` in all sinks.

## Build

Requirements: Go 1.27+.

```sh
CGO_ENABLED=0 go build ./...
go vet ./...
bash scripts/all-gates.sh
```

Live keyring tests need a session bus:

```sh
bash scripts/ci/run-with-keyring.sh bash scripts/all-gates.sh
```

CI adds a race-detector pass (`CGO_ENABLED=1 go test -race ./...`). The detector slows the clock, so the 50k retrieval-latency gate reports its p99 there instead of enforcing the 50 ms budget. You still get the budget enforced in the native suite. Stream `Close` joins the run goroutine, so per-attempt telemetry writes settle before you release the report.

## Docs

- [QUICKSTART.md](docs/QUICKSTART.md) — five minutes to your first request
- [HTTP_SYNTAX.md](docs/HTTP_SYNTAX.md) — the `.http` file format
- [TUI_GUIDE.md](docs/TUI_GUIDE.md) — keybindings, panes, streaming views
- [MCP_COOKBOOK.md](docs/MCP_COOKBOOK.md) — driving Pulse from an AI agent

## Layout

```text
cmd/pulse/        mode selection, exit codes
internal/ast/     .http lexer and parser, no I/O
internal/engine/  transport, streaming, redaction
internal/store/   SQLite history (modernc.org/sqlite, CGO-free)
internal/secrets/ keychain bindings, vault, redaction
internal/rag/     local codebase index
internal/fuzz/    OpenAPI subset reader, boundary matrix, TAP 13
internal/mcp/     JSON-RPC server over stdio
internal/ui/      Bubble Tea models, Lip Gloss styling
internal/app/     composition root, lifecycle
```

## License

MIT. See `LICENSE`.
