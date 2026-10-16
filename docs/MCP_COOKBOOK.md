# MCP_COOKBOOK — driving Pulse from an AI agent

Run the headless server:

```sh
go run ./cmd/pulse --mcp
```

It speaks JSON-RPC 2.0 over stdio: `initialize` → `initialized` →
operation loop → `shutdown` → exit. **Stdout is the protocol channel.**
Nothing but JSON-RPC may be written there; diagnostics go to stderr.

## Tool catalog (16 tools)

| Tool | What it does |
|---|---|
| `execute_request` | Run one request (inline payload or repo path). Returns status, headers, chunked body. |
| `execute_fuzz_run` | Dispatch one fuzzed request against a target. |
| `list_collections` | List `.http` files in the workspace. |
| `list_requests` | List named requests across collections. |
| `get_request` | Fetch one request definition by ref. |
| `get_run` | Fetch a past run by `run_id`. |
| `get_history` | Recent runs, newest first. |
| `fuzz_start` | Open an iterative fuzz session over an OpenAPI subset. |
| `fuzz_next` | Pull the next fuzz case from the session. |
| `fuzz_report` | Close the session with a TAP 13 report. |
| `get_http_syntax_spec` | Authoritative `.http` grammar + `{{VAR}}` rules. Read this before writing. |
| `create_collection` | Create a new workspace `.http` file (jailed, never overwrites). |
| `upsert_request` | Create or replace one named request block (lossless splice). |
| `delete_request` | Delete one named request block. |
| `import_curl` | Import a raw curl command sanitized (secrets → placeholders). |
| `validate_collection` | Syntax diagnostics + present-vs-missing `{{SECRET}}` check (names only). |

## Recipe 1: run a checked-in request

```json
{"jsonrpc":"2.0","id":1,"method":"tools/call",
 "params":{"name":"execute_request","arguments":{"path":"api.http","index":0}}}
```

`index` selects the request (0-based). There is no `name` argument —
look the index up with `list_requests` first, or address the block by
name with `upsert_request` and then run it.

## Recipe 2: iterate on a failure, then verify

1. `get_request` with the failing ref to see the definition.
2. Ask the human (or the vault flow) for any missing `{{variable}}` —
   never invent a secret, never log one.
3. `execute_request` with the corrected arguments.
4. `get_run` with the returned `run_id` to confirm status and assertions.

## Recipe 3: fuzz a new endpoint

```json
{"jsonrpc":"2.0","id":2,"method":"tools/call",
 "params":{"name":"fuzz_start","arguments":{"spec":"openapi.json","target":"/users"}}}
```

Then loop `fuzz_next` until the session is exhausted and close with
`fuzz_report`. Boundary findings persist to the store; secret-bearing
fields are refused before merge, never echoed back.

## Recipe 4: author a collection, then verify

1. `get_http_syntax_spec` for the grammar — never hallucinate syntax.
2. `create_collection` with placeholder references (`{{API_TOKEN}}`),
   never literal secrets.
3. `upsert_request` per endpoint; `import_curl` for copied commands
   (credentials arrive back as placeholder names + `pulse vault set`
   instructions, never values).
4. Ask the user to seal each placeholder (`pulse vault set NAME`).
5. `validate_collection` to confirm syntax and that nothing is missing.
6. `execute_request` to run it.

## The zero-trust AI write chokepoint

Every disk-writing authoring tool (`create_collection`,
`upsert_request`, `import_curl`) scans URLs, headers, and bodies BEFORE
a byte lands. A literal API key, Bearer token, `.env` payload, URL
userinfo, or already-sealed value aborts the whole write — nothing
partial is left behind — and the call returns:

```text
Refused to write literal credential in header/body. Replace the literal
value with a {{SECRET_NAME}} placeholder and ask the user to run
pulse vault set SECRET_NAME.
```

Write `Authorization: Bearer {{API_TOKEN}}`, not the token. The
chokepoint cannot be bypassed by wording: placeholder-shaped values
pass, everything else in a credential position is refused.

## Zero-trust rules for agents

- Raw secrets never appear in tool responses. Where a value would leak,
  you see `[REDACTED]`.
- If a `{{variable}}` cannot be resolved, the call fails closed (exit 3
  semantics) — ask for the value through the vault flow instead of
  guessing `test` / `changeme` / empty string.
- Keep payloads small and bounded; response bodies arrive in chunks and
  the engine enforces a 2 MiB streaming ceiling.
