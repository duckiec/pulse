# v0.1.0 Beta — 5-Minute Sanity Check

Pulse v0.1.0 is the first tagged release: a dual-mode HTTP workbench with a
terminal UI, a headless `pulse test` runner, an encrypted local vault, and an
MCP server. It ships as six CGO-free binaries with `checksums.txt`.

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
pulse --version    # expect: pulse v0.1.0
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

Known issue in v0.1.0: on a default install, `pulse test` does **not** read
`./.pulse.vault` unless you export `PULSE_VAULT_FILE=.pulse.vault`. If your run
fails with exit `3` and `not found in keychain` even
though the key is listed, that is this bug — not your setup. Workaround:
export `PULSE_VAULT_FILE=.pulse.vault` for the run.

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

## What is not in scope for v0.1.0

P2P sync, gRPC, `@wasm-sign`, and the MCP server are implemented but not part of
this five-minute pass. Hold those reports for the next beta unless they block
install, `doctor`, or `pulse test`.
