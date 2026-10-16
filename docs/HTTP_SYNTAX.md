# HTTP syntax reference — the `.http` file format

> **Role in these docs:** the reference. It lists every construct, its
> exact form, and what the parser does with it. For the guided path use
> the [quickstart](QUICKSTART.md); for TUI editing use the
> [TUI guide](TUI_GUIDE.md).

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
  resolves at runtime: OS keychain, then ephemeral in-memory `.env`,
  then the decrypted `.pulse.vault`, then a hard error (exit 3).

## Directives

Directives are `#`-comment lines with a fixed prefix. Any other `#`
line is a plain comment and is ignored by the parser.

| Directive | Form | Meaning |
|---|---|---|
| Name | `# @name my-request` | Attaches a name to the next request. |
| Assert | `# @assert <expr>` | Attaches a check to the next request; evaluated by `pulse test`. |

Assert expressions (see `internal/engine/assert.go`):

| Form | Example | Meaning |
|---|---|---|
| Status equality | `status == 200` | Exact status match. |
| Status comparison | `status == 201`, `status != 404` | Integer comparison. |
| Status range | `status in 200..299` | Inclusive range; reversed ranges (`300..200`) are rejected. |
| Body contains | `body contains "ok"` | Substring match against the response body. |
| Header contains | `header Content-Type contains "json"` | Substring match against a response header. |
| Header equality | `header X-Version == "2"` | Exact header match. |

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

## Canonical form

The serializer emits every request in a fixed order: in-block `@var`
definitions, `# @name`, `# @assert` lines, `@wasm-sign`, the
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
