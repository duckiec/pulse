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
   e.g. `GET https://api.example.com/health HTTP/1.1`.
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

## Secrets discipline

`.http` files are committed to Git. Secrets are never committed: reference
them as `{{PLACEHOLDER}}` variables and seal the values into the OS keychain
or `.pulse.vault`. Response logging redacts `Authorization`, `Cookie`,
`Set-Cookie`, and `X-Api-Key` as `[REDACTED]` in every sink.
