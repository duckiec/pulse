# Release posts

Short announcement copy per tagged release, newest first. The heading version,
the `pulse --version` expectation, and the tag must always agree; the long form
lives in [BETA_TESTING.md](BETA_TESTING.md).

## v0.1.1 — the workspace vault reads itself

Pulse **v0.1.1** is out, one day after the first beta, and it fixes the only
blocker testers hit.

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

**Next up:** the TUI v2 overhaul — a persistent workbench with an omnibox,
a request sidebar, and a telemetry-headed response viewport, replacing the
welcome screen entirely.

## v0.1.0 — first beta

The first tagged release: dual-mode HTTP workbench, headless `pulse test`
runner, encrypted local vault, MCP server. Six CGO-free binaries plus
`checksums.txt`. Superseded by v0.1.1 for the workspace-vault fix; see
[BETA_TESTING.md](BETA_TESTING.md) for the checklist.
