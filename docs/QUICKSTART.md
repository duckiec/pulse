# QUICKSTART — Pulse in five minutes

You need: a terminal. No account, no network, no Go toolchain.

## 0. Install it

Linux and macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/duckiec/pulse/main/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/duckiec/pulse/main/install.ps1 | iex
```

The script drops the `pulse` binary into your user bin directory,
verifies its checksum, and proves it with `pulse --version` plus
`pulse doctor` before handing control back. If `pulse` is not found
afterward, add the printed directory to your `PATH` and open a fresh
terminal.

## 1. Run it

```sh
pulse api.http
```

With no file argument Pulse boots a blank-state scratchpad instead.
Browse files with `j/k`, open a request, press `Ctrl+S` to send.
Bodies stream in chunks; the UI stays responsive under a 50 MB response.

## 2. Scaffold a workspace

```sh
pulse init
```

This writes a richly commented `quickstart.http` (a GET, a POST with a
JSON body, an authenticated request using `{{DEMO_TOKEN}}`, and a
`@wasm-sign` example). It refuses to overwrite an existing file unless
you pass `--force`.

Seal the demo token in one step — no prior keygen or init needed:

```sh
pulse vault set DEMO_TOKEN <value>
```

With no VALUE argument the command prompts securely (no echo) on a TTY
or reads the value from piped stdin. The first `set` on a fresh machine
automatically mints the device identity and seals a new `.pulse.vault`;
`pulse vault list` prints sorted key names only (never values) and
`pulse vault unset <KEY>` removes one.

Check the workspace end to end:

```sh
pulse doctor
```

Doctor verifies keyring reachability, `.pulse.vault` integrity, and
SQLite health; reports `file:line` syntax errors across every `.http`
file; warns when a `{{SECRET}}` is set nowhere; and prints a
ready-to-copy `mcpServers` JSON block for Claude Desktop, Cursor,
Windsurf, and OpenCode.

## 3. Write your first request

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

## 4. Use a variable instead of pasting a secret

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

## 5. Test from the command line

```sh
pulse test . --format tap
```

Assertions (`# @assert status == 200`) run headless with TAP 13 output and
semantic exit codes: `0` pass, `1` failure, `2` usage/parse error,
`3` secret-resolution failure.

## 6. Let an agent drive it

```sh
pulse --mcp
```

The same engine is exposed over JSON-RPC on stdio for local AI clients.
Stdout carries protocol bytes only; diagnostics go to stderr.
See [MCP_COOKBOOK.md](MCP_COOKBOOK.md) for the tool catalog.

## Next steps

- [TUI_GUIDE.md](TUI_GUIDE.md) — keybindings, panes, streaming views
- [HTTP_SYNTAX.md](HTTP_SYNTAX.md) — variables, assertions, `@wasm-sign`
- [MCP_COOKBOOK.md](MCP_COOKBOOK.md) — `execute_request`, history, fuzz tools
