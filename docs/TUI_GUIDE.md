# TUI guide — the terminal workbench

> **Role in these docs:** the how-to guide for humans. It answers "how
> do I do X in the terminal UI?" For the guided first run use the
> [quickstart](QUICKSTART.md); for exact file grammar use the
> [HTTP syntax reference](HTTP_SYNTAX.md).

Launch the workbench over a file, or with no argument for a scratchpad:

```sh
pulse [file.http]
pulse quickstart.http
```

(From source: `go run ./cmd/pulse [file.http]`.) With no file argument
Pulse boots a blank-state scratchpad instead of erroring — see
[Starting empty](#starting-empty).

The UI is a Bubble Tea Elm loop (`Model` / `Update` / `View`) styled
with Lip Gloss. The render thread never does I/O: network and disk run
in background commands and stream back as messages, so keystrokes stay
instant while a 50 MB body lands in bounded 32 KiB chunks under a 2 MiB
in-flight ceiling.

## Layout

The workbench is a fixed spatial grid, not a stack of panels. Every frame is
laid out from the terminal size on each resize, so borders never clip and the
same panes sit in the same places at 80×24 and at 200×60 — there is no
"welcome screen" to leave behind, because the workbench is the first screen.

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

- **Top omnibox** — the live target: a method badge, the interpolated URL, and
  the `@name`, with the workspace file and run phase inlaid in the border. It
  is the one strip that never hides, including behind a modal or the help
  overlay.
- **Left sidebar** — every request in the document, grouped by file, with the
  cursor as a solid selection bar. `j`/`k` move it and `Enter` makes it the
  run target. The sidebar narrows with the terminal but never disappears; at
  80 columns it shows the method and path (`GET /health`) rather than the
  whole URL.
- **Request pane** — the flight line, then `@assert` directives, headers,
  variables, and a body preview, in that order: it is ordered by what matters
  if you only have four rows. Overflow is counted (`… +N more`), never
  silently dropped.
- **Response pane** — a telemetry header (status badge, protocol, size, wall
  time, then the transport phases) above a syntax-coloured viewport. JSON
  keys, strings, numbers and literals are tinted; anything that is not JSON
  passes through byte-identical. Long bodies page; the stream can be cancelled
  mid-flight with `Ctrl+X`.
- **Status bar** — one line: the mode (`NORMAL`, `EDIT`, `SEARCH`, …) as a
  solid chip, the keycaps that apply in that mode, then the run accounting
  (lines, bytes, drops, backlog) and the last toast. A narrow terminal drops
  keycaps from the right, never the mode.

Panes carry the accent border only while they hold the focus, so "where do my
keys go" is answerable at a glance. Every badge also carries its word, so the
layout survives a monochrome terminal; `--ascii` additionally degrades the box
glyphs and every chrome decoration.

Below 80×24 the workbench refuses to draw a broken frame and says so in
words; pipe to `pulse test` for that case.

## Keybindings

The footer and `?` overlay render from the central key registry
(`internal/ui/keys.go`), so what you see on screen is authoritative.
The table below mirrors it.

| Key | Action |
|---|---|
| `q` or `Ctrl+C` | Quit (cancels any in-flight run) |
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

Run `pulse` with no file in an empty directory and it boots the workbench over
an in-memory Untitled Request — never an error, and never a welcome wall. The
request is already in the target slot, ready to send, and the response hint row
carries the authoring verbs: `e` edit, `i` import curl, `g` starter, `n` new
file. Saving writes `requests.http` beside you (atomically, never truncated);
nothing is created until you save.

## How to compose a request

Press `e`. The composer edits name, method, URL, headers, and a
multi-line body.

- `Space` cycles the method picker (`GET`, `POST`, `PUT`, `PATCH`,
  `DELETE`, `HEAD`, `OPTIONS`).
- The URL gains a missing scheme when the block is built (`localhost`
  → `http`, everything else → `https`).
- Headers are a list: `a` adds, `e` edits, `d` deletes, `Space`
  toggles a row off (stays in the file, out of the flight).
- `f` (or `Ctrl+F` in the body) pretty-prints JSON; non-JSON is
  refused with the body untouched.
- `Enter` applies and sends. `Ctrl+S` (or `w` on the headers list)
  saves to disk without sending. `Esc` cancels.

Composer limits: the composer models what the AST represents.
In-block `@var`, `# @assert`, and `@wasm-sign` are captured at open
and survive a save; free comments and `#`-disabled headers inside the
edited span do not. Hand-maintained files keep every byte through the
span splicer — the composer is the fast path, not the lossless one.

## How to import curl, seal secrets, and manage files

- **Import (`i`).** Paste a curl command: method, URL, headers, and
  body fill the composer, and credentials become `{{PLACEHOLDERS}}` —
  raw values never enter the file. The toast counts the extraction.
- **Vault (`v`).** Seal one token: name plus a masked value field. The
  keychain takes it when a write path exists, otherwise the
  memory-only tier (the same chain the send resolves through). Success
  names `{{NAME}}` only — values are never echoed.
- **New file (`n`).** Creates a collection file (default
  `requests.http`) with one untitled request and makes it current.
- **Starter (`g`).** Writes the two-request starter (health check plus
  `{{API_TOKEN}}` create) — never over an existing file.
- **Yank (`y`).** Copies the request as `curl` (placeholders intact)
  to the clipboard, with a byte-count toast when no clipboard exists.

## How to fix a missing secret

A send that fails on an unresolved `{{SECRET}}` gains a remediation
line at the top of the response pane:

```text
Secret {{SECRET}} is missing. Press [v] to set it.
```

The vault modal prefills the name, so the fix is one paste. Sealing
the named secret clears the hint; resend to retry. Unresolvable
secrets fail the send with a named error (exit 3 semantics), never a
dummy value. History shows `[REDACTED]` wherever a secret value would
appear.

## How to read a response

Select a request and press `Ctrl+S`. The response pane fills top-down: a status
badge with protocol, size and wall time; the transport phases (`dns`, `tcp`,
`tls`, `wrote`, `first-byte`, `total`, plus `reused-conn` when the connection
was kept alive); a rule; then the body in the viewport. A phase that never ran
reads `—`, never `0µs` — a reused connection has no DNS lookup, and the pane
does not invent one.

While a body streams, the first line becomes a live `streaming` badge with
bytes and lines received, the pane border turns accent, and the status bar
shows `backlog` if the frame governor had to drop chunks. `p` freezes the
frame without dropping the stream; `Ctrl+X` cancels.

JSON bodies are syntax-coloured (keys, strings, numbers, literals). The
highlighter never reformats or rewrites: a line it does not recognise passes
through byte-identical, so a body is always exactly what the server sent.

`# @assert` outcomes ride at the top of the response body, so the assertion
and the response are visible at the same time — a failing assert names the
expression, the expected value, and the actual value with the source line for
the failure.

A `# @assert snapshot` request reports its own line. On a mismatch the pane
shows the structural diff (JSON paths) or a line diff, plus the
`[u] accept the new snapshot` hint.

## How to accept a snapshot

When a snapshot assertion fails, read the diff and press `u`. The live
response becomes the new baseline, `api.http.snap` is rewritten, and the
toast confirms the update. Commit the `.snap` file with the change — it
is the reviewed record of the new response shape.

`u` does nothing when the snapshot already matches, and it refuses to
overwrite a malformed snapshot file: that file needs a human, not a new
baseline. The first run of a snapshot request writes the file and passes.
See the [feature guide](FEATURES.md) for the file format and the size
limit.

## How branch environments apply

Pulse reads `.env.pulse` from the directory you launched it in. The
checked-out branch selects a `[branch:...]` block, so `{{BASE_URL}}`
changes when you switch branches; `{{GIT_BRANCH}}` is available inside
URLs, headers, bodies, and the file itself. The omnibox and the request
pane show the unresolved template, not the secret or the branch value —
values resolve at send time. See
[Git-aware environments](FEATURES.md#git-aware-environments).
