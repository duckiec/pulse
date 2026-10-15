# TUI_GUIDE — the terminal workbench

Launch:

```sh
go run ./cmd/pulse --tui
```

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
| `Enter` | Open / confirm |
| `Ctrl+S` | Send the selected request |
| `x` | Cancel a running stream |
| `1`–`5` | Switch pane / view |
| `?` | Full help overlay |
| `q` | Quit (confirm if a stream is live) |

## Sending a request

Select a request, press `Ctrl+S`. Status and headers render first; the body
follows in chunks. `# @assert` outcomes appear inline — a failing assert
shows the expression, the expected value, and the actual value, with the
source line for the failure.

## Secrets in the TUI

`{{variables}}` resolve from the OS keyring or ephemeral `.env` at send
time. Unresolvable secrets fail the send with a named error, never a dummy
value. History shows `[REDACTED]` wherever a secret value would appear.

## Coming in Phase 2

Blank-state scratchpad boot for empty folders, an in-TUI composer (`[e]`
edit, `[i]` import cURL, `[v]` vault seal), and one-key JSON formatting —
all keyboard-driven, all stdlib plus the already-sanctioned Charm stack.
