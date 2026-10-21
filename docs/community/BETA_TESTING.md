# v0.1.1 Beta — 5-Minute Sanity Check

Pulse v0.1.1 is the first patched beta: a dual-mode HTTP workbench with a
terminal UI, a headless `pulse test` runner, an encrypted local vault, and an
MCP server. It ships as six CGO-free binaries with `checksums.txt`.

v0.1.1 fixes the one blocker found in the v0.1.0 beta: `pulse test` now reads
the workspace vault (`./.pulse.vault`) by default, so the documented
`pulse vault set` → `pulse test` flow works without exporting
`PULSE_VAULT_FILE`. Nothing else changed.

We need **five minutes**, not a full test plan. Pick the platform you actually
run, install from the release assets, and report what breaks. "Everything
worked" is a useful result too.

## 1. Install (~2 min)

Linux / macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/duckiec/pulse/main/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://raw.githubusercontent.com/duckiec/pulse/main/install.ps1 | iex
```

The installer downloads the matching asset plus `checksums.txt`, verifies the
SHA-256 checksum fail-closed, and self-checks `pulse --version` and
`pulse doctor` inside a temporary `HOME`. It does not touch your workspace.

Please report which asset you used:

- `pulse-linux-amd64` / `pulse-linux-arm64`
- `pulse-darwin-amd64` / `pulse-darwin-arm64`
- `pulse-windows-amd64.exe` / `pulse-windows-arm64.exe`

## 2. Doctor (~1 min)

```sh
mkdir -p ~/pulse-beta && cd ~/pulse-beta
pulse --version    # expect: pulse v0.1.1
pulse doctor
```

`doctor` should exit `0` and report keyring and SQLite health. Warnings about a
missing vault and missing `.http` files are expected in an empty directory. A
**panic** is a bug; please paste the full output.

## 3. Scaffold and seal (~1 min)

```sh
pulse init
printf "hello-beta" | pulse vault set DEMO_TOKEN
pulse vault list        # prints the key name, never the value
```

Fixed in v0.1.1: `pulse test` reads `./.pulse.vault` by default. If a run still
exits `3` with `not found in keychain` while `pulse vault list` shows the key,
that is a **new** bug — please report it with the vault path you used.
`PULSE_VAULT_FILE` still wins when set, so an explicit path is always honored.

## 4. Headless run (~1 min)

```sh
printf 'GET https://example.com\n' > check.http
pulse test check.http --format tap
```

Expected: TAP 13 output and exit `0`. A single file is accepted even though the
help text says "directory". Exit codes are semantic: `0` pass, `1` assertion
failure, `2` usage/parse error, `3` secret resolution failure.

## 5. Report back

Open a comment on this issue with:

```
OS + arch:
Asset used:
pulse --version:
pulse doctor: pass / fail (paste warnings)
init + vault set: pass / fail
pulse test: pass / fail (exit code)
Anything surprising:
```

## Ground rules

- **Never paste a real secret or token.** Use a throwaway value.
- Use a scratch directory; Pulse writes `~/.config/pulse/pulse.db` globally
  (`PULSE_HOME` overrides it).
- Bugs, panics, unclear errors, and confusing help text are all in scope — the
  friction matters as much as the failure.
- Do not test against systems you do not own.

## What is not in scope for v0.1.1

P2P sync, gRPC, `@wasm-sign`, and the MCP server are implemented but not part of
this five-minute pass. Hold those reports for the next beta unless they block
install, `doctor`, or `pulse test`.
