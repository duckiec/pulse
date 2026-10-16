# TUI_GUIDE — the terminal workbench

Launch:

```sh
go run ./cmd/pulse [file.http]
```

With no file argument Pulse boots the blank-state scratchpad (see
"Starting empty" below) instead of erroring.

The UI is a Bubble Tea Elm loop (`Model` / `Update` / `View`) styled with
Lip Gloss. The render thread never does I/O: network and disk run in
background commands and stream back as messages, so keystrokes stay instant
while a 50 MB body lands in bounded chunks from a pooled buffer ring.

## Layout

- **File tree** — workspace `.http` files and their named requests.
- **Request pane** — method, URL, headers, body of the selection.
- **Stream view** — live response: status, headers, chunked body, assertion
  outcomes. Long bodies page; the stream can be cancelled mid-flight.

## Keybindings

| Key | Action |
|---|---|
| `j` / `k` | Move down / up (vim motions; arrows work too) |
| `home` / `end` | Jump to top / bottom |
| `Ctrl+D` / `Ctrl+U` | Half-page down / up (`PgDn` / `PgUp` work too) |
| `Ctrl+F` / `Ctrl+B` | Full page down / up |
| `p` | Pause / resume the stream |
| `Enter` | Open / confirm |
| `Ctrl+S` | Send the selected request (saves inside the composer) |
| `Ctrl+X` | Cancel a running stream |
| `tab` | Cycle pane focus / composer field |
| `e` | Compose the request (method, URL, headers, body) |
| `i` | Import a pasted curl command into the composer |
| `v` | Seal a secret without leaving the TUI |
| `y` | Yank the request as a curl command |
| `n` | New collection file in the workspace |
| `g` | Generate the starter collection |
| `/` then `Ctrl+N` / `N` | Search the stream; next / previous hit |
| `Ctrl+P` | Command palette |
| `f` | Fuzz dashboard (formats JSON inside the composer) |
| `?` | Full help overlay |
| `q` | Quit (cancels any in-flight run) |

## Starting empty

Run `pulse` with no file in an empty directory and it boots a welcome
state over an in-memory Untitled Request — never an error. The footer
names the five verbs: `[e]` edit, `[i]` import curl, `[g]` starter,
`[n]` new file, `[?]` help. Saving writes `requests.http` beside you
(atomic, never truncated); nothing is created until you save.

## The composer (`e`)

The composer edits name, method, URL, headers, and a multi-line body.
`Space` cycles the method picker; the URL gains a missing scheme
(`localhost` → `http`, everything else → `https`) when the block is
built. Headers are a list: `a` adds, `e` edits, `d` deletes, `Space`
toggles a row off (stays in the file, out of the flight). `f` (or
`Ctrl+F` in the body) pretty-prints JSON; non-JSON is refused with the
body untouched. `Enter` applies and sends; `Ctrl+S` (or `w` on the
headers list) saves to disk without sending; `Esc` cancels.

### Composer limits

The composer models what the AST represents. Free comments and `#`-
disabled headers inside the edited span do not survive a save (in-block
`@var`, `# @assert`, and `@wasm-sign` do — they are captured at open).
Hand-maintained files keep every byte through the span splicer; the
composer is the fast path, not the lossless one.

## Import, vault, files

- `[i]` pastes a curl command: method, URL, headers, and body fill the
  composer, and credentials become `{{PLACEHOLDERS}}` — raw values never
  enter the file. The toast counts the extraction.
- `[v]` seals one token: name plus a masked value field. The keychain
  takes it when a write path exists, otherwise the memory-only tier
  (same chain the send resolves through). Success names `{{NAME}}`
  only — values are never echoed.
- `[n]` creates a collection file (default `requests.http`) with one
  untitled request and makes it current. `[g]` writes the two-request
  starter (health check + `{{API_TOKEN}}` create) — never over an
  existing file.
- `y` copies the request as `curl` (placeholders intact) to the
  clipboard, with a byte-count toast when no clipboard exists.

## When a secret is missing

A send that fails on an unresolved `{{SECRET}}` gains a remediation
line at the top of the response pane:
`Secret {{SECRET}} is missing. Press [v] to set it.` The vault modal
prefills the name, so the fix is one paste. Sealing the named secret
clears the hint; resend to retry.

## Sending a request

Select a request, press `Ctrl+S`. Status and headers render first; the body
follows in chunks. `# @assert` outcomes appear inline — a failing assert
shows the expression, the expected value, and the actual value, with the
source line for the failure.

## Secrets in the TUI

`{{variables}}` resolve from the OS keyring or ephemeral `.env` at send
time. Unresolvable secrets fail the send with a named error, never a dummy
value. History shows `[REDACTED]` wherever a secret value would appear.

