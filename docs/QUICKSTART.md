# QUICKSTART — Pulse in five minutes

You need: a terminal, Go 1.27+, no account, no network.

## 1. Run it

```sh
go run ./cmd/pulse --tui
```

Browse files with `j/k`, open a request, press `Ctrl+S` to send.
Bodies stream in chunks; the UI stays responsive under a 50 MB response.

## 2. Write your first request

Create `api.http`:

```http
# @name get-user
# @assert status == 200
GET https://localhost:8080/users/1
Accept: application/json

###
```

Blocks are separated by `###`. A method line, headers, a blank line, then
an optional body. See [HTTP_SYNTAX.md](HTTP_SYNTAX.md) for the full grammar.

## 3. Use a variable instead of pasting a secret

```http
@base = https://localhost:8080

# @name get-user
GET {{base}}/users/1
Authorization: Bearer {{token}}

###
```

`{{token}}` resolves at runtime: OS keychain first, then an ephemeral
in-memory `.env`, then a hard error. Pulse never writes the secret to disk,
the store, or logs — it prints as `[REDACTED]`. Never commit a raw token
into an `.http` file; files are Git-native plaintext by design.

## 4. Test from the command line

```sh
go run ./cmd/pulse test --format tap
```

Assertions (`# @assert status == 200`) run headless with TAP 13 output and
semantic exit codes: `0` pass, `1` failure, `2` usage/parse error,
`3` secret-resolution failure.

## 5. Let an agent drive it

```sh
go run ./cmd/pulse --mcp
```

The same engine is exposed over JSON-RPC on stdio for local AI clients.
Stdout carries protocol bytes only; diagnostics go to stderr.
See [MCP_COOKBOOK.md](MCP_COOKBOOK.md) for the tool catalog.

## Next steps

- [TUI_GUIDE.md](TUI_GUIDE.md) — keybindings, panes, streaming views
- [HTTP_SYNTAX.md](HTTP_SYNTAX.md) — variables, assertions, `@wasm-sign`
- [MCP_COOKBOOK.md](MCP_COOKBOOK.md) — `execute_request`, history, fuzz tools
