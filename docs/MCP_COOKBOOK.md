# MCP_COOKBOOK — driving Pulse from an AI agent

Run the headless server:

```sh
go run ./cmd/pulse --mcp
```

It speaks JSON-RPC 2.0 over stdio: `initialize` → `initialized` →
operation loop → `shutdown` → exit. **Stdout is the protocol channel.**
Nothing but JSON-RPC may be written there; diagnostics go to stderr.

## Tool catalog (10 tools)

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

## Recipe 1: run a checked-in request

```json
{"jsonrpc":"2.0","id":1,"method":"tools/call",
 "params":{"name":"execute_request","arguments":{"path":"api.http","name":"get-user"}}}
```

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

## Zero-trust rules for agents

- Raw secrets never appear in tool responses. Where a value would leak,
  you see `[REDACTED]`.
- If a `{{variable}}` cannot be resolved, the call fails closed (exit 3
  semantics) — ask for the value through the vault flow instead of
  guessing `test` / `changeme` / empty string.
- Keep payloads small and bounded; response bodies arrive in chunks and
  the engine enforces a 2 MiB streaming ceiling.
