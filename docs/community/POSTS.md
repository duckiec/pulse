# Release posts

Short announcement copy per tagged release, newest first. The heading version,
the `pulse --version` expectation, and the tag must always agree; the long form
lives in [BETA_TESTING.md](BETA_TESTING.md).

## v0.1.2 — the workbench arrives

Pulse **v0.1.2** is out, and the terminal UI is no longer a welcome screen
with a prompt: it is a persistent workbench.

One grid, resolved per size: a three-row omnibox with the method pill, the
live URL, the `@name` and the inlaid workspace and phase; a `[1] Requests`
sidebar beside a `Request` pane over a `Response` pane; and one status bar
carrying the mode, the keys that work in that mode, and the accounting. The
focused pane takes the accent border, the rest stay slate. The welcome wall
is gone — an empty directory boots the workbench over an in-memory Untitled
Request with `e edit · i curl · g starter · n new file` on the hint row, and
writes nothing until you save.

JSON responses are highlighted without being rewritten, selection is a
full-width bar, and the request pane orders what matters into four rows:
asserts, headers, variables, body — then `… +N more`.

This release also closes the flaky `-race` job (issue #14): LAN discovery now
uses `libp2p/zeroconf/v2`, whose `Shutdown` closes its sockets and waits for
the receive goroutines instead of mutating state under them. The old
test-only shutdown shim is gone.

```sh
curl -fsSL https://raw.githubusercontent.com/duckiec/pulse/main/install.sh | sh
pulse --version   # pulse v0.1.2
```

Six CGO-free binaries, checksums verified at install. The v0.1.1
workspace-vault fix carries forward. The five-minute checklist is in
[BETA_TESTING.md](BETA_TESTING.md).

## v0.1.1 — the workspace vault reads itself

Pulse **v0.1.1** is out: the first patch to the beta, fixing the only blocker
testers hit.

In v0.1.0, `pulse vault set DEMO_TOKEN` sealed a token into `./.pulse.vault`,
but `pulse test` only looked there when `PULSE_VAULT_FILE` was exported. The
documented quickstart — seal a secret, run the suite — failed with exit `3`
and `not found in keychain` even though the key was right there. Resolution
now defaults to the workspace vault when one exists, `PULSE_VAULT_FILE` still
overrides it, and a workspace with no vault stays silent. No other behaviour
changed.

```sh
curl -fsSL https://raw.githubusercontent.com/duckiec/pulse/main/install.sh | sh
pulse --version   # pulse v0.1.1
```

Six CGO-free binaries, checksums verified at install. If you were partway
through the beta checklist, start again at the `pulse test` step — no
workaround needed.

**Next up:** the TUI v2 overhaul — shipped in v0.1.2 as the persistent
workbench with an omnibox, a request sidebar, and a telemetry-headed response
viewport, replacing the welcome screen entirely.

## v0.1.0 — first beta

The first tagged release: dual-mode HTTP workbench, headless `pulse test`
runner, encrypted local vault, MCP server. Six CGO-free binaries plus
`checksums.txt`. Superseded by v0.1.1 for the workspace-vault fix; see
[BETA_TESTING.md](BETA_TESTING.md) for the checklist.
