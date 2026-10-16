# HTTP_SYNTAX — the `.http` file format

Pulse collections are plain-text `.http` files. One file holds variables,
requests, and assertions. Git is the database of record.

## File skeleton

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

## Rules, in order

1. **Separator.** A line starting with `###` ends a request block.
2. **Variables.** `@name = value` defines a variable. Position sets scope:
   definitions before the first `###` are file-scoped; definitions after a
   separator are local to the requests that follow. A `@var` line without
   `=` is a fail-closed syntax error — never a silent empty value.
3. **Names.** `# @name my-request` attaches a name to the next request.
4. **Assertions.** `# @assert <expr>` attaches a check to the next request.
   Supported forms include `status == 200`, `status in 200..299`, and
   `body contains "ok"`. They run headless via `pulse test`.
5. **Request line.** `METHOD URL` with an optional `HTTP/1.1` version token,
   e.g. `GET https://api.example.com/health HTTP/1.1`. Methods: `GET`,
   `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS` — anything else is
a syntax error, never a guess.
6. **Headers.** `Name: value` lines after the request line. A `#`-commented
   header line is a disabled header — it stays in the file but is not sent.
7. **Body.** After exactly one blank line, the raw body runs to the next
   `###` separator or end of file.
8. **Interpolation.** `{{var}}` in URLs, headers, and bodies resolves at
   runtime from the keychain, then ephemeral `.env`, then a hard error.
9. **Signing.** A bare `@wasm-sign ./sign/auth.wasm` line before a request
   attaches a sandboxed signer; at most one per request, `<path>` with no
   `=`. `@wasm-sign = x` is rejected as a misspelling, not accepted as a
   variable. Requests without the directive never touch the signer.

## Comments

Any `#` line that is not a `# @name` / `# @assert` directive is a plain
comment and is ignored by the parser. Comments, disabled headers, and
variable definitions are preserved round-trip by the Phase 2 serializer
(see ROADMAP Phase 2.1 in the local governance docs).

## Canonical form

The serializer emits every request in a fixed order: in-block `@var`
definitions, `# @name`, `# @assert` lines, `@wasm-sign`, the `METHOD URL`
line, headers, one blank line, then the body. Collections join requests
with `###` separators. A full-document format keeps file-scoped `@var`
definitions but drops free comments and `#`-disabled headers (they have no
AST representation); surgical single-request edits via create/upsert/delete
splice source spans instead, so neighbors, comments, and disabled headers
survive byte-identical.

## Workspace edits

Collection files are created, updated, and deleted inside the workspace
root: `..` traversal, absolute paths outside the root, and symlink escapes
are rejected; creation never truncates an existing file; an upsert block
must hold exactly one request (a replacement swaps the named request's
span, an unknown name appends a new trailing `###` section); deleting a
request that owns its section alone removes the separator with it. Every
result is re-parsed before it reaches disk, and writes land via temp-file
plus atomic rename, so a crash never leaves a truncated collection.

## cURL import

A copied `curl` command imports to one request: method, URL, headers, and
body map across; credentials never land in the file — `Authorization` and
cookie values, `--user` passwords, and secret query keys become
`{{PLACEHOLDER}}` references, with the raw values returned separately for
the vault-seal flow. Unknown flags, multiple URLs, and `@file` bodies fail
closed with the offending input named. URLs without a scheme default to
`http://` for loopback targets (`localhost`, `127.0.0.1`, `[::1]`) and
`https://` elsewhere; `{{templates}}` and path-only references pass
through untouched.

## Secrets discipline

`.http` files are committed to Git. Secrets are never committed: reference
them as `{{PLACEHOLDER}}` variables and seal the values into the OS keychain
or `.pulse.vault`. Response logging redacts `Authorization`, `Cookie`,
`Set-Cookie`, and `X-Api-Key` as `[REDACTED]` in every sink.
