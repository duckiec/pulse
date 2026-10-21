# Quickstart — Pulse in five minutes

> **Role in these docs:** the tutorial. Follow it top to bottom the first
> time; it produces a working workspace. For task lookup use the
> [TUI guide](TUI_GUIDE.md) and [MCP cookbook](MCP_COOKBOOK.md); for exact
> grammar use the [HTTP syntax reference](HTTP_SYNTAX.md); for the agent
> policy use the [MCP guide](MCP_GUIDE.md).

You need a terminal. You do not need an account, a network connection,
or a Go toolchain. Every command below runs against the installed
`pulse` binary.

## 1. Install the binary

Linux and macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/duckiec/pulse/main/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/duckiec/pulse/main/install.ps1 | iex
```

The script downloads the matching release asset plus `checksums.txt`,
verifies the SHA-256 checksum fail-closed, installs into your user bin
directory, and self-checks with `pulse --version` and `pulse doctor`
inside an isolated temp `HOME`. It never touches your workspace. If your
shell cannot find `pulse` afterward, add the printed directory to your
`PATH` and open a fresh terminal.

To build from source instead (local directory only, no install):

```sh
export PATH="$HOME/sdk/go1.27.2/bin:$PATH"
CGO_ENABLED=0 go build -trimpath -o ./pulse ./cmd/pulse
./pulse --version
```

## 2. Scaffold a workspace

```sh
mkdir my-api && cd my-api
pulse init
```

This writes a commented `quickstart.http` with four shapes: a `GET`
with no secrets, a `POST` with a JSON body, an authenticated request
that references `{{DEMO_TOKEN}}`, and a `@wasm-sign` example. It refuses
to overwrite an existing file unless you pass `--force`.

## 3. Seal the demo secret

```sh
pulse vault set DEMO_TOKEN <value>
```

With no `VALUE` argument the command prompts securely (no echo) on a
TTY or reads the value from piped stdin. The first `set` on a fresh
machine mints the device identity and creates `.pulse.vault`
automatically — no prior `keygen` or `init` step. `pulse vault list`
prints sorted key names only, never values; `pulse vault unset <KEY>`
removes one; `pulse vault keygen` mints or rotates the device identity
used for P2P sync.

Secrets resolve in this order at runtime: OS keychain, then ephemeral
in-memory `.env`, then the decrypted `.pulse.vault`, then a hard error
with exit 3. Pulse never writes a raw value to disk, the SQLite store,
or logs — diagnostics show `[REDACTED]`.

## 4. Check the workspace

```sh
pulse doctor
```

Doctor verifies keyring reachability, `.pulse.vault` integrity, and
SQLite health; reports `file:line` syntax errors across every `.http`
file; warns when a `{{SECRET}}` is set nowhere; and prints a
ready-to-paste `mcpServers` JSON block for Claude Desktop, Cursor,
Windsurf, and OpenCode.

## 5. Send a request

Open the workbench over the scaffold:

```sh
pulse quickstart.http
```

With no file argument Pulse boots a blank-state scratchpad instead of
erroring. Browse with `j`/`k`, open a request, press `Ctrl+S` to send.
Bodies stream in bounded 32 KiB chunks under a 2 MiB in-flight ceiling,
so the UI stays responsive under a 50 MB response. See the
[TUI guide](TUI_GUIDE.md) for the full keymap.

## 6. Write your first request

Create `api.http`:

```http
# @name get-user
# @assert status == 200
GET https://localhost:8080/users/1
Accept: application/json

###
```

Blocks end at a `###` separator line. A method line, headers, one blank
line, then an optional body. Use variables instead of pasting secrets:

```http
@base = https://localhost:8080

# @name get-user
GET {{base}}/users/1
Authorization: Bearer {{token}}

###
```

`{{token}}` resolves through the chain in §3. Never commit a raw token:
`.http` files are Git-native plaintext by design. See the
[HTTP syntax reference](HTTP_SYNTAX.md) for the full grammar.

## 7. Test headless

```sh
pulse test . --format tap
```

Assertions (`# @assert status == 200`) run headless with TAP 13 output.
Exit codes are semantic: `0` pass, `1` failure, `2` usage or parse
error, `3` secret-resolution failure.

## 8. Let an agent drive it

```sh
pulse --mcp
```

The same engine is exposed as a 16-tool MCP server over JSON-RPC on
stdio (`pulse mcp` is the same server). Stdout carries protocol bytes
only; diagnostics go to stderr. See the
[MCP cookbook](MCP_COOKBOOK.md) for the tool catalog and recipes.

## 9. Make the environment follow the branch

Add a `.env.pulse` beside the collection:

```ini
[default]
BASE_URL = http://localhost:8080

[branch:staging]
BASE_URL = https://staging.internal.net
```

`{{BASE_URL}}` now changes with the checkout. To lock a response shape into
review, add `# @assert snapshot` to a named request: the first
`pulse test` writes `api.http.snap`, later runs diff against it, and
`pulse test -u` accepts a new baseline. In the TUI, `u` does the same thing.

## 10. Check the collection against the contract

With an OpenAPI document beside the collection:

```sh
pulse drift openapi.json .
```

Every request is matched by path and method. The command reports a path the
spec does not declare, an operation marked `deprecated: true`, and a required
query parameter or header the request omits, each with `file:line`. It exits
`0` when the collection matches, `1` on drift, and `2` when the spec or
collection cannot be read. No request is sent.

## 11. Bound what an agent may do

Before letting an agent drive the MCP server, add `.pulse-guard` to the
workspace:

```ini
[mcp]
allow_methods = GET, POST
deny_methods = DELETE, PUT, PATCH
allow_hosts = localhost, *.internal.net
rate_limit = 10/minute
```

Without the file, `pulse mcp` allows every method against loopback hosts only.
A refused call returns `BLOCKED BY POLICY: …` to the model and sends nothing.
The [MCP guide](MCP_GUIDE.md) documents every key and the failure modes.

## Next steps

- [Feature guide](FEATURES.md) — branch environments, snapshots, drift, and the firewall
- [TUI guide](TUI_GUIDE.md) — panes, composer, vault, fuzz dashboard
- [HTTP syntax reference](HTTP_SYNTAX.md) — variables, assertions, signing, cURL import
- [MCP cookbook](MCP_COOKBOOK.md) — `execute_request`, history, fuzz, authoring tools
- [MCP guide](MCP_GUIDE.md) — `.pulse-guard` and the blast-radius firewall
