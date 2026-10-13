# Pulse

Pulse is a strictly offline, Git-native API workbench: a terminal UI for designing and
executing HTTP requests against `.http` files, and a headless MCP server that runs the
same collections through the Model Context Protocol.

This glossary records the vocabulary Pulse coins. General programming concepts —
timeouts, contexts, HTTP clients, error wrapping — are deliberately absent even though
they are pervasive here; they mean the same thing they mean everywhere.

## Collections

**Collection**:
A set of `.http` files tracked in version control. The unit of work, and the reason Git
is the database of record.
_Avoid_: suite, project, repo of requests

**Request**:
One HTTP request block within a `.http` file, delimited from its neighbours.
_Avoid_: call, operation, endpoint (an endpoint is the server-side thing)

**Directive**:
A `#`-prefixed comment carrying metadata — `@name`, `@assert`. A directive binds to the
request that **follows** it, never the one above. Getting this backwards makes every
assertion vanish silently, which is indistinguishable from a working runner.
_Avoid_: annotation, comment, tag

**Assertion**:
A parsed `# @assert` directive: six operators over five subjects (`status`, `code`,
`body`, `header <name>`, `duration`) and nothing else. Every input either parses into
exactly one assertion or fails closed.
_Avoid_: check, expectation, predicate

**Test point**:
One evaluation of an assertion — or one request that carries no assertions, which still
produces exactly one point so the TAP plan is never empty. The unit a plan counts.
_Avoid_: test, case (a "test" in this repo is a *file*; see Gate)

## Evidence

**Gate**:
A test that is expected to fail when the code is wrong. A test that passes under every
mutation is decoration, not a gate.
_Avoid_: test (a test may be neither a gate nor load-bearing), spec

**Mutation**:
The deliberate single-line defect that proves a gate bites. Required evidence for every
fix, not an occasional extra.
_Avoid_: break, sabotage, fault injection

**Exit code**:
The semantic outcome of `pulse test`, a pure function of the report: `0` all pass, `1`
any failure, `2` usage/parse **or a report with no test points**, `3` secret-resolution.
A run that produced no points has not passed anything, so it is not `0`.
_Avoid_: status, result code, return value

**BLOCKED**:
An honest test disposition: the test could not run here — no keychain, no host, no
daemon. Distinct from a pass and from a failure, and never reported as either.
_Avoid_: skipped (a skip is a choice; a BLOCKED is a limit), N/A, todo

**Tape**:
A TAP 13 document: a plan (`1..N`) followed by exactly N results. The plan and the
results must agree, or a CI consumer that trusts the plan hangs.
_Avoid_: report, output, log

## Runtime

**Mode**:
Interactive (TUI) or MCP. Selected at startup, immutable for the process lifetime, and
never mixed — in MCP mode stdout is the JSON-RPC transport and nothing else may touch it.
_Avoid_: frontend, backend, interface

**Composition root**:
The one package permitted to name a concrete dependency on another package's behalf.
Every other package declares an interface and is handed it.
_Avoid_: wiring, bootstrap, DI container

**Arena**:
The bump allocator backing the zero-allocation `.http` AST. Strings, headers and bodies
are slices into it; releasing the document releases all of them at once.
_Avoid_: pool, buffer, slab

**Chunk**:
A bounded, pooled unit of a streamed response body. It owns its buffer and returns it to
the pool; a body is never materialised whole.
_Avoid_: packet, buffer, fragment

**Sink**:
The consumer a stream is multiplexed into. The sink owns every chunk it receives.
_Avoid_: consumer, listener, handler

**Redactor**:
The mandatory scrubber applied before any value reaches stderr, the UI, or the store.
Headers named `Authorization`, `Cookie`, `X-Api-Key` and anything sourced from the vault
become `[REDACTED]`.
_Avoid_: masker, sanitizer, filter

## Secrets

**Keychain**:
The OS-native credential store — Windows Credential Manager, macOS Keychain, Linux Secret
Service. The only durable home for a secret. Resolution order is keychain, then an
ephemeral in-memory `.env`, then a hard failure; there is no fourth option.
_Avoid_: credential store, keystore, vault (a vault is what keychain holds the key *for*)

**Vault**:
An AES-256-GCM encrypted `.env.vault` file committed alongside the collection it serves.
The container is `PVLT1` plus a `crypto/rand` nonce plus ciphertext and tag.
_Avoid_: encrypted file, secret file, keystore

**Nonce**:
The 96-bit value, unique per seal, that makes AES-GCM's keystream and authentication key
unique per message. Reuse under one key does not weaken encryption — it breaks it, and
forges are then possible.
_Avoid_: IV, salt, seed

**Zeroisation**:
Overwriting a buffer's contents once it is no longer needed. It is the caller's to
invoke; a function that wipes what it was handed cannot be composed or called twice.
_Avoid_: wiping, scrubbing, clearing (clearing is a language primitive; zeroisation is
the security obligation)

## See also

Decisions are not in this file. They are `ADL-nnnn` entries in `MEMORY.md`; see
`.agents/domain.md` for how to read them.