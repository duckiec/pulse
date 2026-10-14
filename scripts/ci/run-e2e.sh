#!/usr/bin/env bash
# Bare-metal end-to-end crucible (Issue #11 release bar, Phase 3).
# Builds ./cmd/pulse and drives the binary as a real OS process: live
# keyring, loopback sockets, stdio pipes, on-disk SQLite, real Wasm
# guests, mTLS sync, and a pty TUI smoke.
#
# Stage 1 needs a live Secret Service daemon; run under
# scripts/ci/run-with-keyring.sh for the isolated session. Without one
# the suite reports SKIP (reason: no live keyring), never a false pass
# (AGENTS.md §4.5).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=env.sh
source "$(dirname "$0")/env.sh"

echo "== e2e crucible (compiled binary, 6 stages)"
CGO_ENABLED=0 go test -tags=e2e -count=1 -timeout 20m -v ./e2e/
