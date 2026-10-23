# Quickstart

**Zero to a tested request, a snapshot baseline, and a guarded agent.**

<sub>[Features](FEATURES.md) · [HTTP syntax](HTTP_SYNTAX.md) · [TUI](TUI_GUIDE.md) · [MCP cookbook](MCP_COOKBOOK.md) · [MCP guide](MCP_GUIDE.md)</sub>

You need a terminal. You do not need an account, a network connection, or a Go toolchain — every command below runs against the installed `pulse` binary.

| Step | You end up with |
|---|---|
| [1 · Install](#1-install) | `pulse` on your `PATH` |
| [2 · Scaffold](#2-scaffold-a-workspace) | `quickstart.http` with four request shapes |
| [3 · Seal a secret](#3-seal-a-secret) | A vault and a device identity |
| [4 · Send](#4-send-a-request) | The terminal workbench |
| [5 · Test](#5-test-headless) | TAP output and semantic exit codes |
| [6 · Agent](#6-let-an-agent-drive-it) | A 16-tool MCP server |
| [7 · Branch + snapshot](#7-make-the-environment-follow-the-branch) | `.env.pulse` and `api.http.snap` |
| [8 · Contract, guard, merge](#8-check-the-collection-against-the-contract) | `pulse drift`, `.pulse-guard`, `pulse merge` |
| [9 · Capture + replay](#9-capture-live-traffic) | `generated.http`, an offline mock |

## 1. Install

**Linux / macOS**

```sh
curl -fsSL https://raw.githubusercontent.com/duckiec/pulse/main/install.sh | sh
```

**Windows PowerShell**

```powershell
irm https://raw.githubusercontent.com/duckiec/pulse/main/install.ps1 | iex
```

The script downloads the matching release asset plus `checksums.txt`, verifies the SHA-256 checksum fail-closed, installs into your user bin directory, and self-checks with `pulse --version` and `pulse doctor` inside an isolated temp `HOME`. It never touches your workspace. If your shell cannot find `pulse` afterward, add the printed directory to your `PATH` and open a fresh terminal.

**From source** (local directory only, no install):

```sh
CGO_ENABLED=0 go build -trimpath -o ./pulse ./cmd/pulse
./pulse --version
```

## 2. Scaffold a workspace

```sh
mkdir my-api && cd my-api
pulse init
```

This writes a commented `quickstart.http` with four shapes: a `GET` with no secrets, a `POST` with a JSON body, an authenticated request that references `{{DEMO_TOKEN}}`, and a `@wasm-sign` example. It refuses to overwrite an existing file unless you pass `--force`.

## 3. Seal a secret

```sh
pulse vault set DEMO_TOKEN <value>
```

With no `VALUE` argument the command prompts securely (no echo) on a TTY or reads the value from piped stdin. The first `set` on a fresh machine mints the device identity and creates `.pulse.vault` automatically — no prior `keygen` or `init` step.

| Command | Effect |
|---|---|
| `pulse vault list` | Sorted key names only, never values |
| `pulse vault unset KEY` | Remove one entry |
| `pulse vault keygen` | Mint or rotate the device identity used for P2P sync |

Secrets resolve at runtime in this order: OS keychain → Git-aware `.env.pulse` → ephemeral in-memory `.env` → decrypted `.pulse.vault` → hard error (exit `3`). Pulse never writes a raw value to disk, the SQLite store, or logs — diagnostics show `[REDACTED]`.

## 4. Send a request

```sh
pulse quickstart.http
```

With no file argument Pulse boots a blank-state scratchpad instead of erroring. Browse with `j`/`k`, open a request, press `Ctrl+S` to send. Bodies stream in bounded 32 KiB chunks under a 2 MiB in-flight ceiling, so the UI stays responsive under a 50 MB response. See the [TUI guide](TUI_GUIDE.md) for the full keymap.

### Write your first request

Create `api.http`:

```http
# @name get-user
# @assert status == 200
GET https://localhost:8080/users/1
Accept: application/json

###
```

Blocks end at a `###` separator line: a method line, headers, one blank line, then an optional body. Reference secrets instead of pasting them:

```http
@base = https://localhost:8080

# @name get-user
GET {{base}}/users/1
Authorization: Bearer {{token}}

###
```

`{{token}}` resolves through the chain in [§3](#3-seal-a-secret). Never commit a raw token: `.http` files are Git-native plaintext by design. The full grammar is in the [HTTP syntax reference](HTTP_SYNTAX.md).

## 5. Test headless

```sh
pulse test . --format tap
```

Assertions (`# @assert status == 200`) run headless with TAP 13 output.

| Exit | Meaning |
|---|---|
| `0` | Every assertion passed |
| `1` | At least one assertion or snapshot failed |
| `2` | Usage or parse error |
| `3` | Secret-resolution failure |

## 6. Let an agent drive it

```sh
pulse --mcp
pulse doctor        # prints a ready-to-paste mcpServers block
```

The same engine is exposed as a 16-tool MCP server over JSON-RPC on stdio (`pulse mcp` is the same server). Stdout carries protocol bytes only; diagnostics go to stderr. `pulse doctor` prints the `mcpServers` JSON for Claude Desktop, Cursor, Windsurf, and OpenCode, and reports keyring reachability, `.pulse.vault` integrity, SQLite health, `file:line` syntax errors across every `.http` file, and any `{{SECRET}}` that is set nowhere.

The tool catalog and recipes are in the [MCP cookbook](MCP_COOKBOOK.md); the policy is in the [MCP guide](MCP_GUIDE.md).

## 7. Make the environment follow the branch

Add `.env.pulse` beside the collection:

```ini
[default]
BASE_URL = http://localhost:8080

[branch:staging]
BASE_URL = https://staging.internal.net
```

`{{BASE_URL}}` now changes with the checkout. To lock a response shape into review, add `# @assert snapshot` to a named request: the first `pulse test` writes `api.http.snap`, later runs diff against it, and `pulse test -u` accepts a new baseline. In the TUI, `u` does the same thing.

## 8. Check the collection against the contract

```sh
pulse drift openapi.json .
```

Every request is matched by path and method. The command reports a path the spec does not declare, an operation marked `deprecated: true`, and a required query parameter or header the request omits — each with `file:line`. No request is sent.

| Exit | Meaning |
|---|---|
| `0` | The collection matches the spec |
| `1` | Drift |
| `2` | The spec or collection cannot be read |

### Bound what an agent may do

Before letting an agent drive the MCP server, add `.pulse-guard` to the workspace:

```ini
[mcp]
allow_methods = GET, POST
deny_methods = DELETE, PUT, PATCH
allow_hosts = localhost, *.internal.net
rate_limit = 10/minute
```

Without the file, `pulse mcp` allows every method against loopback hosts only. A refused call returns `BLOCKED BY POLICY: …` to the model and sends nothing. The [MCP guide](MCP_GUIDE.md) documents every key and the failure modes.

### Merge collections like records

Bind the semantic merge driver once per clone, and tell Git to use it for `.http` files:

```sh
git config merge.pulse.driver "pulse merge %O %A %B"
echo '*.http merge=pulse' >> .gitattributes
```

Two branches adding different things to the same request — a header on one, an assertion on the other — now merge with both changes kept. Only the same field changing to different values leaves standard conflict markers. Requests may also run a sandboxed rewriter with `# @wasm ./hooks/auth.wasm` before signing and the policy gate; see [Request hooks](FEATURES.md#wasm-request-hooks).

## 9. Capture live traffic

Stop hand-writing requests you can capture. Put the recorder in front of the server your app already calls:

```sh
pulse record :9000 --target http://localhost:8080 --out generated.http
```

Point the frontend at `http://localhost:9000`, click through the flows you care about, then Ctrl-C. Each distinct request is appended to `generated.http` with an `@name`, the credentials replaced by `{{AUTH_TOKEN}}`-style variables, and an `# @assert status == <code>` line. Review it, replace the variables with the names in your vault, and commit — `pulse test .` runs it unchanged.

The recorded URL is the upstream's, not the proxy's, so the file keeps working after the recorder exits. Larger-than-64 KiB, binary, and `###`-bearing request bodies are omitted with a comment in the block instead of being truncated.

### Work offline from the cache

Every 2xx response seen by `pulse test` or the MCP executor is kept in the local SQLite replay cache — latest success per method+path. Run the collection once, stop the backend, and serve the recorded responses instead:

```sh
pulse test .
# stop the backend
pulse mock . --from-cache --port 8080
curl http://127.0.0.1:8080/users/1
```

The server matches method and path (dynamic ids included), ignores query strings, and replays the cached status, headers, and body. An endpoint that has never succeeded is a 404 with the command that populates it — never an invented response. `--host 0.0.0.0` exposes the cache to your LAN for a phone or a second machine.

## Next

| Document | Read it for |
|---|---|
| [FEATURES.md](FEATURES.md) | Branch environments, snapshots, drift, capture, offline replay, merges, hooks, firewall |
| [TUI_GUIDE.md](TUI_GUIDE.md) | Panes, composer, vault, fuzz dashboard |
| [HTTP_SYNTAX.md](HTTP_SYNTAX.md) | Variables, assertions, signing, cURL import |
| [MCP_COOKBOOK.md](MCP_COOKBOOK.md) | `execute_request`, history, fuzz, authoring tools |
| [MCP_GUIDE.md](MCP_GUIDE.md) | `.pulse-guard` and the blast-radius firewall |
