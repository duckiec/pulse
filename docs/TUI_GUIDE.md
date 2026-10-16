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

- **File tree** — workspace `.http` files and their named requests.
- **Request pane** — method, URL, headers, body of the selection.
- **Stream view** — live response: status, headers, chunked body, and
  assertion outcomes. Long bodies page; the stream can be cancelled
  mid-flight with `Ctrl+X`.

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
| `n` | New collection file in the workspace |
| `g` | Generate the starter collection |
| `f` | Fuzz dashboard (formats JSON inside the composer) |
| `Enter` | Open / confirm |
| `?` | Full help overlay |

## Starting empty

Run `pulse` with no file in an empty directory and it boots a welcome
state over an in-memory untitled request — never an error. The footer
offers five verbs: `[e]` edit, `[i]` import curl, `[g]` starter,
`[n]` new file, `[?]` help. Saving writes `requests.http` beside you
(atomically, never truncated); nothing is created until you save.

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

Select a request and press `Ctrl+S`. Status and headers render first;
the body follows in chunks. `# @assert` outcomes appear inline — a
failing assert shows the expression, the expected value, and the actual
value, with the source line for the failure.
