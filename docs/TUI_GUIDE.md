# TUI guide

**The terminal workbench: panes, keybindings, the composer, and the streaming response view.**

<sub>[Quickstart](QUICKSTART.md) · [Features](FEATURES.md) · [HTTP syntax](HTTP_SYNTAX.md) · [MCP cookbook](MCP_COOKBOOK.md) · [MCP guide](MCP_GUIDE.md)</sub>

```sh
pulse [file.http]
pulse quickstart.http
```

With no file argument Pulse boots a blank-state scratchpad instead of erroring — see [Starting empty](#starting-empty). From source: `go run ./cmd/pulse [file.http]`.

The UI is a Bubble Tea Elm loop (`Model` / `Update` / `View`) styled with Lip Gloss. The render thread never does I/O: network and disk run in background commands and stream back as messages, so keystrokes stay instant while a 50 MB body lands in bounded 32 KiB chunks under a 2 MiB in-flight ceiling.

```text
╭─ pulse ────────────────────────────────────────── requests.http · DONE ─╮
│  POST   http://localhost:8080/notes                    @create-note       │
╰─────────────────────────────────────────────────────────────────────────╯
╭─ [1] Requests (2) ─╮╭─ Request ──────────────────────────────────────────╮
│▾ requests.http     ││ POST  http://localhost:8080/notes HTTP/1.1         │
│▸ POST /notes       ││ ✓ @assert status == 201                            │
│  GET /health       ││ Content-Type: application/json                     │
│                    │╰─────────────────────────────────────────────────────╯
│                    │╭─ Response ─────────────────────────────────────────╮
│                    ││ 201 Created   HTTP/1.1  64B  12.3ms                │
│                    ││ dns 412µs  tcp 1.2ms  tls —  wrote —  …            │
│                    ││────────────────────────────────────────────────────│
│                    ││ {                                                  │
│                    ││   "id": 7,                                         │
╰────────────────────╯╰─────────────────────────────────────────────────────╯
 NORMAL   q quit   ^S send   ^X cancel   tab pane   e edit  …  lines 5  64B
```

## Layout

The workbench is a fixed spatial grid, not a stack of panels. Every frame is laid out from the terminal size on each resize, so borders never clip and the same panes sit in the same places at 80×24 and at 200×60 — there is no "welcome screen" to leave behind, because the workbench is the first screen.

| Region | Contents |
|---|---|
| Top omnibox | The live target: method badge, interpolated URL, `@name`, with the workspace file and run phase inlaid in the border. Never hides, including behind a modal or help. |
| Left sidebar | Every request in the document, grouped by file, cursor as a solid selection bar. `j`/`k` move it, `Enter` retargets. Narrows but never disappears; at 80 columns it shows `GET /health`. |
| Request pane | Flight line, then `@assert` directives, headers, variables, body preview — ordered by what matters if you only have four rows. Overflow is counted (`… +N more`), never dropped. |
| Response pane | Telemetry header (status, protocol, size, wall time, transport phases) above a syntax-coloured viewport. Long bodies page; `Ctrl+X` cancels mid-flight. |
| Status bar | Mode chip (`NORMAL`, `EDIT`, `SEARCH`, …), the keycaps that apply in that mode, run accounting (lines, bytes, drops, backlog), last toast. A narrow terminal drops keycaps, never the mode. |

Panes carry the accent border only while they hold the focus, so "where do my keys go" is answerable at a glance. Every badge also carries its word, so the layout survives a monochrome terminal; `--ascii` additionally degrades the box glyphs and every chrome decoration.

Below 80×24 the workbench refuses to draw a broken frame and says so in words; pipe to `pulse test` for that case.

## Keybindings

The footer and `?` overlay render from the central key registry (`internal/ui/keys.go`), so what you see on screen is authoritative. This table mirrors it.

| Key | Action |
|---|---|
| `q` / `Ctrl+C` | Quit (cancels any in-flight run) |
| `Ctrl+S` | Send the selected request (`w` also saves inside the composer) |
| `Ctrl+X` | Cancel a running stream |
| `Tab` | Cycle pane focus |
| `j` / `k` (or arrows) | Move down / up |
| `Home` / `End` | Jump to top / bottom |
| `Ctrl+D` / `Ctrl+U` (or `PgDn` / `PgUp`) | Half-page down / up |
| `Ctrl+F` / `Ctrl+B` | Full page down / up |
| `p` | Pause / resume the stream |
| `/` | Search the stream |
| `Ctrl+N` / `N` | Next / previous search hit |
| `Ctrl+P` | Command palette |
| `e` | Compose the request (method, URL, headers, body) |
| `i` | Import a pasted curl command into the composer |
| `v` | Seal a secret without leaving the TUI |
| `y` | Yank the request as a curl command |
| `u` | Accept the new snapshot when a snapshot assertion fails |
| `n` | New collection file in the workspace |
| `g` | Generate the starter collection |
| `f` | Fuzz dashboard (formats JSON inside the composer) |
| `Enter` | Open / confirm |
| `?` | Full help overlay |

## Starting empty

Run `pulse` with no file in an empty directory and it boots the workbench over an in-memory Untitled Request — never an error, and never a welcome wall. The request is already in the target slot, ready to send, and the response hint row carries the authoring verbs: `e` edit, `i` import curl, `g` starter, `n` new file. Saving writes `requests.http` beside you (atomically, never truncated); nothing is created until you save.

## Composing a request

Press `e`. The composer edits name, method, URL, headers, and a multi-line body.

| Key | In the composer |
|---|---|
| `Space` | Cycle the method picker (`GET`, `POST`, `PUT`, `PATCH`, `DELETE`, `HEAD`, `OPTIONS`) |
| `a` / `e` / `d` | Add / edit / delete a header row |
| `Space` (headers) | Toggle a row off — stays in the file, out of the flight |
| `f` / `Ctrl+F` | Pretty-print JSON in the body; non-JSON is refused with the body untouched |
| `Enter` | Apply and send |
| `Ctrl+S` / `w` | Save to disk without sending |
| `Esc` | Cancel |

The URL gains a missing scheme when the block is built (`localhost` → `http`, everything else → `https`).

**Composer limits.** The composer models what the AST represents. In-block `@var`, `# @assert`, and `@wasm-sign` are captured at open and survive a save; free comments and `#`-disabled headers inside the edited span do not. Hand-maintained files keep every byte through the span splicer — the composer is the fast path, not the lossless one.

## Import, vault, and files

- **Import (`i`).** Paste a curl command: method, URL, headers, and body fill the composer, and credentials become `{{PLACEHOLDERS}}` — raw values never enter the file. The toast counts the extraction.
- **Vault (`v`).** Seal one token: name plus a masked value field. The keychain takes it when a write path exists, otherwise the memory-only tier (the same chain the send resolves through). Success names `{{NAME}}` only — values are never echoed.
- **New file (`n`).** Creates a collection file (default `requests.http`) with one untitled request and makes it current.
- **Starter (`g`).** Writes the two-request starter (health check plus `{{API_TOKEN}}` create) — never over an existing file.
- **Yank (`y`).** Copies the request as `curl` (placeholders intact) to the clipboard, with a byte-count toast when no clipboard exists.

## Missing secrets

A send that fails on an unresolved `{{SECRET}}` gains a remediation line at the top of the response pane:

```text
Secret {{SECRET}} is missing. Press [v] to set it.
```

The vault modal prefills the name, so the fix is one paste. Sealing the named secret clears the hint; resend to retry. Unresolvable secrets fail the send with a named error (exit `3` semantics), never a dummy value. History shows `[REDACTED]` wherever a secret value would appear.

## Reading a response

Select a request and press `Ctrl+S`. The response pane fills top-down: a status badge with protocol, size and wall time; the transport phases (`dns`, `tcp`, `tls`, `wrote`, `first-byte`, `total`, plus `reused-conn` when the connection was kept alive); a rule; then the body in the viewport. A phase that never ran reads `—`, never `0µs` — a reused connection has no DNS lookup, and the pane does not invent one.

While a body streams, the first line becomes a live `streaming` badge with bytes and lines received, the pane border turns accent, and the status bar shows `backlog` if the frame governor had to drop chunks. `p` freezes the frame without dropping the stream; `Ctrl+X` cancels.

JSON bodies are syntax-coloured (keys, strings, numbers, literals). The highlighter never reformats or rewrites: a line it does not recognise passes through byte-identical, so a body is always exactly what the server sent.

`# @assert` outcomes ride at the top of the response body, so the assertion and the response are visible at the same time — a failing assert names the expression, the expected value, and the actual value with the source line for the failure.

A `# @assert snapshot` request reports its own line. On a mismatch the pane shows the structural diff (JSON paths) or a line diff, plus the `[u] accept the new snapshot` hint.

## Accepting a snapshot

When a snapshot assertion fails, read the diff and press `u`. The live response becomes the new baseline, `api.http.snap` is rewritten, and the toast confirms the update. Commit the `.snap` file with the change — it is the reviewed record of the new response shape.

`u` does nothing when the snapshot already matches, and it refuses to overwrite a malformed snapshot file: that file needs a human, not a new baseline. The first run of a snapshot request writes the file and passes. See [Snapshot assertions](FEATURES.md#snapshot-assertions) for the file format and the size limit.

## Branch environments

Pulse reads `.env.pulse` from the directory you launched it in. The checked-out branch selects a `[branch:...]` block, so `{{BASE_URL}}` changes when you switch branches; `{{GIT_BRANCH}}` is available inside URLs, headers, bodies, and the file itself. The omnibox and the request pane show the unresolved template, not the secret or the branch value — values resolve at send time. See [Git-aware environments](FEATURES.md#git-aware-environments).
