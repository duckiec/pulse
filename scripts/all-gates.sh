#!/usr/bin/env bash
# Run every enforcement gate. Local pre-push equivalent of CI.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
s="$(dirname "$0")/ci"

bash "$s/build-matrix.sh"
bash "$s/tidy-check.sh"
bash "$s/banned-import-audit.sh"
bash "$s/doc-sync-gate.sh"
bash "$s/test-gates.sh"
# Wave 6.5: optional live-keyring gate. When the isolated Secret Service
# session can be constructed (dbus-run-session + gnome-keyring-daemon in
# PATH), the bare-metal boundary runs too — so the OS security boundary
# never regresses silently. Without those binaries the gate reports SKIP
# (reason: no keyring session), never a false pass (AGENTS.md §4.5).
if command -v dbus-run-session >/dev/null 2>&1 && command -v gnome-keyring-daemon >/dev/null 2>&1; then
  echo "== live Linux keyring gate (Wave 6.5)"
  bash "$s/run-with-keyring.sh" go test -tags=integration ./internal/secrets/...
else
  echo "== live Linux keyring gate: SKIP (reason: no dbus-run-session/gnome-keyring-daemon in PATH)"
fi
bash "$(git rev-parse --show-toplevel)/scripts/smoke-check.sh" >/dev/null

echo "ALL GATES: PASS"
