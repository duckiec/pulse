# MCP guide — the agent loop and the blast-radius firewall

> **Role in these docs:** the reference for running Pulse as an MCP server
> safely. It answers "what may an agent send, and what happens when it may
> not?" For the tool catalog and recipes read the
> [MCP cookbook](MCP_COOKBOOK.md); for branch environments and snapshot
> assertions read the [feature guide](FEATURES.md).

```sh
pulse mcp
pulse --mcp      # the same server
```

Stdout is the JSON-RPC channel. Diagnostics go to stderr. The server speaks
the standard lifecycle: `initialize` → `initialized` → operation loop →
`shutdown` → exit.

## The blast-radius firewall

`pulse mcp` reads `.pulse-guard` from the directory it is launched in and
checks every request after variable interpolation, before anything is sent.
The policy is loaded once, when the server is constructed.

```ini
[mcp]
allow_methods = GET, POST
deny_methods = DELETE, PUT, PATCH
allow_hosts = localhost, *.internal.net
deny_hosts = api.production.com
rate_limit = 10/minute
```

| Key | Meaning |
|---|---|
| `allow_methods` | Complete set of permitted methods, upper-cased. Empty or absent means every method. |
| `deny_methods` | Methods that are always refused. Checked before `allow_methods`, so a method in both lists is denied. |
| `allow_hosts` | Complete set of permitted hosts. Empty or absent means every host not denied. |
| `deny_hosts` | Hosts that are always refused. Checked before `allow_hosts`. |
| `rate_limit` | `<count>/<second\|minute\|hour>`. Absent means no limit. |

`[mcp]` is the only section. Unknown sections, unknown keys, duplicate keys
and malformed values are errors: a typo in `allow_host` must not read as "no
restriction".

### Host patterns

- `localhost` and `127.0.0.1` match exactly. Case is ignored.
- `*.internal.net` matches `staging.internal.net` and
  `deep.staging.internal.net`, but not the apex `internal.net`.
- `*` alone permits every host — an explicit opt-in, suitable only for a
  dedicated sandbox.
- A port is ignored in both the pattern and the request: `localhost:8080` in
  the file still matches a request to port 3000.

### The default policy

A missing file is not "no policy". It is:

- every method allowed,
- `localhost`, `127.0.0.1` and `::1` allowed, everything else denied,
- no rate limit.

An agent that needs a remote host must be given one in a committed file — if a
workflow previously pointed at a remote API with no policy, add an `allow_hosts`
entry when upgrading. The refusal names the host and the key to add:

```text
BLOCKED BY POLICY: Host api.example.com is not permitted: no .pulse-guard
exists, so the default policy allows loopback hosts only. Add allow_hosts =
api.example.com to .pulse-guard to permit it.
```

### Rate limiting

`rate_limit` is a sliding window per server process, counted in memory. Only
requests the method and host rules already allowed consume the budget, so a
denied `DELETE` does not shorten the agent's remaining allowance. The window
is not persisted: a new `pulse mcp` process starts with a full budget.

## What a blocked request looks like

The request is refused before dispatch, so no socket is opened. The tool
result is an error with readable text, not a JSON-RPC transport error, so a
model can read the rule and choose a safe alternative:

```text
BLOCKED BY POLICY: Method DELETE is explicitly denied by .pulse-guard.
```

The structured payload carries the same information for a client that wants
to branch on it:

```json
{"blocked": true, "category": "policy", "reason": "Method DELETE is explicitly denied by .pulse-guard."}
```

Policy refusals have the reserved code `-32007` (`category: "policy"`) where a
coded error is returned rather than a tool result.

## Failure modes

| Situation | Behavior |
|---|---|
| `.pulse-guard` missing | Default policy above; the server starts. |
| `.pulse-guard` malformed | `pulse mcp` exits 1 before serving, naming `file:line` and the problem. It does not fall back to the defaults. |
| Policy file elsewhere | Set `PULSE_GUARD_FILE=/path/to/.pulse-guard`. |
| `pulse` TUI or `pulse test` | Not gated. The guard exists for the agent, which is the untrusted caller. |
| gRPC target | Not covered by this version of the policy. The firewall governs HTTP requests; keep gRPC tools out of an agent's workspace until the policy grows a gRPC rule. |

A malformed guard stops the MCP server only. The TUI, `pulse test` and
`pulse drift` still run, because the policy is loaded when the server is
constructed.

## Verifying a policy

1. Write `.pulse-guard` in the workspace.
2. Start `pulse mcp` and send the request through the agent.
3. A denied method or host returns `BLOCKED BY POLICY: …` and the target
   server receives nothing.
4. An allowed request behaves exactly as before.

The unit tests behind these rules are in `internal/mcp/guard_test.go`;
`internal/mcp/guard_tool_test.go` proves the text reaches the agent and that
the target server is never contacted.
