# MCP cookbook

**Running Pulse from an AI agent: the tool catalog and the recipes that keep it honest.**

<sub>[Quickstart](QUICKSTART.md) · [Features](FEATURES.md) · [HTTP syntax](HTTP_SYNTAX.md) · [TUI](TUI_GUIDE.md) · [MCP guide](MCP_GUIDE.md)</sub>

```sh
pulse --mcp
pulse mcp
```

Both forms start the same server (from source: `go run ./cmd/pulse --mcp`). It speaks JSON-RPC 2.0 over stdio with the spec lifecycle: `initialize` → `initialized` → operation loop → `shutdown` → exit. **Stdout is the protocol channel.** Nothing but JSON-RPC may be written there; diagnostics go to stderr.

The server is sandboxed by `.pulse-guard` in the workspace: methods, hosts and a request rate. A refused call returns `BLOCKED BY POLICY: …` as text and sends nothing. The [MCP guide](MCP_GUIDE.md) documents the file; the recipes below assume the request is permitted.

## Prerequisites

- A workspace with at least one `.http` file (`pulse init` scaffolds one). Collection paths are jailed to the workspace root: `..` traversal, absolute paths outside the root, and symlink escapes are rejected.
- Secrets sealed before the run (`pulse vault set NAME <value>`). Unresolvable `{{variables}}` fail closed — never guess `test`, `changeme`, or an empty string.

## Tool catalog

| Tool | What it does |
|---|---|
| `execute_request` | Run one request (inline payload or repo path); returns status, headers, chunked body |
| `execute_fuzz_run` | Dispatch one fuzzed request against a target |
| `list_collections` | List `.http` files in the workspace |
| `list_requests` | List named requests across collections |
| `get_request` | Fetch one request definition by ref |
| `get_run` | Fetch a past run by `run_id` |
| `get_history` | Recent runs, newest first |
| `fuzz_start` | Open an iterative fuzz session over an OpenAPI subset |
| `fuzz_next` | Pull the next fuzz case from the session |
| `fuzz_report` | Close the session with a TAP 13 report |
| `get_http_syntax_spec` | Authoritative `.http` grammar and `{{VAR}}` rules. Read this before writing |
| `create_collection` | Create a new workspace `.http` file (jailed, never overwrites) |
| `upsert_request` | Create or replace one named request block (lossless splice) |
| `delete_request` | Delete one named request block |
| `import_curl` | Import a raw curl command sanitized (secrets become placeholders) |
| `validate_collection` | Syntax diagnostics plus present-vs-missing `{{SECRET}}` check (names only) |

## Recipe: run a checked-in request

```json
{"jsonrpc":"2.0","id":1,"method":"tools/call",
 "params":{"name":"execute_request","arguments":{"path":"api.http","index":0}}}
```

`index` selects the request (0-based). There is no `name` argument — look the index up with `list_requests` first, or address the block by name with `upsert_request` and then run it.

## Recipe: iterate on a failure, then verify

1. Call `get_request` with the failing ref to see the definition.
2. Ask the human (or use the vault flow) for any missing `{{variable}}` — never invent a secret, never log one.
3. Call `execute_request` with the corrected arguments.
4. Call `get_run` with the returned `run_id` to confirm status and assertions.

## Recipe: fuzz an endpoint

```json
{"jsonrpc":"2.0","id":2,"method":"tools/call",
 "params":{"name":"fuzz_start","arguments":{"spec":"openapi.json","target":"/users"}}}
```

Then loop `fuzz_next` until the session is exhausted and close with `fuzz_report`. Boundary findings persist to the store; secret-bearing fields are refused before merge, never echoed back.

## Recipe: author a collection, then verify

1. Call `get_http_syntax_spec` for the grammar — never hallucinate syntax.
2. Call `create_collection` with placeholder references (`{{API_TOKEN}}`), never literal secrets.
3. Call `upsert_request` per endpoint; use `import_curl` for copied commands (credentials arrive back as placeholder names plus `pulse vault set` instructions, never values).
4. Ask the user to seal each placeholder (`pulse vault set NAME`).
5. Call `validate_collection` to confirm syntax and that nothing is missing.
6. Call `execute_request` to run it.

## The zero-trust write chokepoint

Every disk-writing authoring tool (`create_collection`, `upsert_request`, `import_curl`) scans URLs, headers, and bodies **before** a byte lands. A literal API key, Bearer token, `.env` payload, URL userinfo, or already-sealed value aborts the whole write — nothing partial is left behind — and the call returns:

```text
Refused to write literal credential in header/body. Replace the literal
value with a {{SECRET_NAME}} placeholder and ask the user to run
pulse vault set SECRET_NAME.
```

Write `Authorization: Bearer {{API_TOKEN}}`, not the token. The chokepoint cannot be bypassed by wording: placeholder-shaped values pass, everything else in a credential position is refused.

## Zero-trust rules for agents

- Raw secrets never appear in tool responses. Where a value would leak, you see `[REDACTED]`.
- If a `{{variable}}` cannot be resolved, the call fails closed (exit `3` semantics) — ask for the value through the vault flow instead of guessing.
- Keep payloads small and bounded; response bodies arrive in chunks and the engine enforces a 2 MiB in-flight ceiling (32 KiB chunks).
