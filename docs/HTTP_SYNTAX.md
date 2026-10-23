# HTTP syntax reference — the `.http` file format

> **Role in these docs:** the reference. It lists every construct, its
> exact form, and what the parser does with it. For the guided path use
> the [quickstart](QUICKSTART.md); for TUI editing use the
> [TUI guide](TUI_GUIDE.md); for branch environments and snapshots use
> the [feature guide](FEATURES.md).

Pulse collections are plain-text `.http` files. One file holds
variables, requests, and assertions. Git is the database of record.
Secrets are never stored here — reference them as `{{PLACEHOLDER}}`
(see [Secrets](#secrets)).

## Example

```http
@base = https://api.example.com
@token = fixture-value

# @name create-user
# @assert status == 201
# @assert body contains "ok"
POST {{base}}/users HTTP/1.1
Content-Type: application/json
Authorization: Bearer {{token}}

{"name": "ada"}

###
```

## Block separator

A line starting with `###` ends a request block. Everything after the
separator belongs to the next block. The final block does not require a
trailing separator.

## Variables

Form: `@name = value` (whitespace around `=` is allowed).

- Definitions before the first `###` are file-scoped: visible to every
  request in the file.
- Definitions after a separator are block-local: visible to the
  requests that follow them.
- A `@var` line without `=` is a syntax error (fail closed). Pulse
  never treats it as an empty value.
- Interpolation `{{var}}` is allowed in URLs, headers, and bodies and
  resolves at runtime: OS keychain, then the Git-aware `.env.pulse`
  (branch block, then `[default]`), then the ephemeral in-memory `.env`,
  then the decrypted `.pulse.vault`, then a hard error (exit 3).
- `{{GIT_BRANCH}}` is reserved and auto-populated from
  `git rev-parse --abbrev-ref HEAD`; it is unresolved outside a branch.
  See [Git-aware environments](#git-aware-environments).

## Directives

Directives are `#`-comment lines with a fixed prefix. Any other `#`
line is a plain comment and is ignored by the parser.

| Directive | Form | Meaning |
|---|---|---|
| Name | `# @name my-request` | Attaches a name to the next request. |
| Assert | `# @assert <expr>` | Attaches a check to the next request; evaluated by `pulse test`. |
| Snapshot | `# @assert snapshot` | Compares the response body with `api.http.snap`; requires `# @name`. |
| Hook | `# @wasm ./hooks/auth.wasm` | Runs a sandboxed WebAssembly request rewriter before signing and dispatch. Attaches to the next request. |

Assert expressions (see `internal/engine/assert.go`):

| Form | Example | Meaning |
|---|---|---|
| Status equality | `status == 200` | Exact status match. |
| Status comparison | `status == 201`, `status != 404` | Integer comparison. |
| Status range | `status in 200..299` | Inclusive range; reversed ranges (`300..200`) are rejected. |
| Body contains | `body contains "ok"` | Substring match against the response body. |
| Header contains | `header Content-Type contains "json"` | Substring match against a response header. |
| Header equality | `header X-Version == "2"` | Exact header match. |
| Snapshot | `snapshot` | Structural comparison with the stored baseline (see below). |

### Snapshot assertions

`# @assert snapshot` writes the response body to an adjacent
`<file>.http.snap` on the first run, then compares structurally on later
runs. JSON key order and whitespace are ignored; a changed value is
reported at its path. `pulse test -u` accepts the live body as the new
baseline.

- The request must carry `# @name`; the name is the file's namespace.
- The compared body is capped at 256 KiB. A larger response fails the
  assertion rather than storing a prefix.
- Non-JSON bodies use a line diff.

See [Snapshot assertions](FEATURES.md#snapshot-assertions)
for the file format, the TUI `u` action, and CI output.

## Git-aware environments

A `.env.pulse` file in the workspace maps branch selectors to variables:

```ini
[default]
BASE_URL = http://localhost:8080

[branch:staging]
BASE_URL = https://staging.internal.net

[branch:feat/*]
BASE_URL = https://preview-{{GIT_BRANCH}}.internal.net
```

- `[default]` applies first; a matching `[branch:...]` block overrides it
  key by key.
- `*` matches any run of characters (including `/`); `?` matches one.
- An exact selector beats a glob regardless of file order.
- Outside a Git repository, or on a detached HEAD, only `[default]`
  applies and `{{GIT_BRANCH}}` is unresolved.

Set `PULSE_ENV_PULSE_FILE` to read a file outside the invocation
directory. See [Git-aware environments](FEATURES.md#git-aware-environments)
for precedence against the keychain and `.env`.

## Request line

Form: `METHOD URL` with an optional `HTTP/1.1` version token.

```http
GET https://api.example.com/health HTTP/1.1
```

Accepted methods: `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`,
`OPTIONS`. Any other method token is a syntax error.

## Headers

`Name: value` lines after the request line, before the blank line.

- A `#`-commented header line is a disabled header: it stays in the
  file but is not sent.
- Header values may contain `{{variables}}`.

## Body

After exactly one blank line following the headers, the raw body runs
to the next `###` separator or end of file. There is no escaping: bytes
are sent as written. Omit the blank line and body for requests without
one.

## Request signing

A bare `@wasm-sign ./sign/auth.wasm` line before a request attaches a
sandboxed WASM signer to that request.

- At most one `@wasm-sign` per request.
- The path form takes no `=`: `@wasm-sign = x` is rejected as a
  misspelling, not accepted as a variable.
- Requests without the directive never touch the signer. The module
  runs with no filesystem, no network, and no clock.

## Request hooks

A `# @wasm ./hooks/auth.wasm` comment before a request attaches a sandboxed
request hook to it.

- The path is required; `# @wasm` with no path is a parse error. At most one
  hook per request. `# @wasm-sign` is a different directive and is never read
  as a hook.
- The hook runs after `{{variable}}` interpolation and before signing and the
  `.pulse-guard` firewall. It may change the method, URL, headers, and body,
  which is what makes SigV4 and HMAC-over-body scripts possible.
- It receives the pending request as JSON in guest memory and returns the
  rewritten JSON; the sandbox has no filesystem, environment, network, clock,
  or WASI. The full contract is in
  [Request hooks](FEATURES.md#wasm-request-hooks).
- A request without the directive never loads the sandbox. If a directive is
  present and no sandbox can run the script, the run fails closed instead of
  sending the unmodified request.

## Canonical form

The serializer emits every request in a fixed order: in-block `@var`
definitions, `# @name`, `# @assert` lines, `# @wasm`, `@wasm-sign`, the
`METHOD URL` line, headers, one blank line, then the body. Collections
join requests with `###` separators.

A full-document format keeps file-scoped `@var` definitions but drops
free comments and `#`-disabled headers (they have no AST
representation). Surgical single-request edits via `create_collection`,
`upsert_request`, and `delete_request` splice source spans instead, so
neighbors, comments, and disabled headers survive byte-identical.

## Workspace edit rules

Collection files are created, updated, and deleted inside the workspace
root:

- `..` traversal, absolute paths outside the root, and symlink escapes
  are rejected.
- Creation never truncates an existing file.
- An upsert block must hold exactly one request. A replacement swaps
  the named request's span; an unknown name appends a new trailing
  `###` section.
- Deleting a request that owns its section alone removes the separator
  with it.
- Every result is re-parsed before it reaches disk. Writes land via
  temp-file plus atomic rename, so a crash never leaves a truncated
  collection.

## cURL import

A copied `curl` command imports to one request: method, URL, headers,
and body map across.

- Credentials never land in the file. `Authorization` and cookie
  values, `--user` passwords, and secret query keys become
  `{{PLACEHOLDER}}` references; the raw values are returned separately
  for the `pulse vault set` flow.
- Unknown flags, multiple URLs, and `@file` bodies fail closed with the
  offending input named.
- URLs without a scheme default to `http://` for loopback targets
  (`localhost`, `127.0.0.1`, `[::1]`) and `https://` elsewhere.
- `{{templates}}` and path-only references pass through untouched.

## Secrets

`.http` files are committed to Git. Seal values outside them:

```sh
pulse vault set API_TOKEN <value>
```

Response logging redacts `Authorization`, `Proxy-Authorization`,
`Cookie`, `Set-Cookie`, and `X-Api-Key` — plus every sealed value — as
`[REDACTED]` in every sink: stderr, store rows, UI, and MCP output.
Outbound requests carry the real secret; diagnostic copies never do.
