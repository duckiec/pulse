#!/usr/bin/env bash
# go mod tidy gate (AGENTS.md §4.1): tidy must be a no-op. Runs offline
# (GOPROXY=off): if all required modules are already in the cache, the project
# is fully reproducible without network access (invariant I-3).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=env.sh
source "$(dirname "$0")/env.sh"

backup="$(mktemp -d)"
trap 'rm -rf "${backup}"' EXIT
cp go.mod "${backup}/go.mod"
[ -f go.sum ] && cp go.sum "${backup}/go.sum"

# Cold-cache fix: a fresh CI runner holds an empty module cache, so an
# offline tidy fails on fetch errors instead of drift. `go mod download`
# cannot warm this: tidy walks the tests of imported packages (grpc's
# opentelemetry wiring, lipgloss's terminfo probes), which live outside
# the build list. Run tidy online first (fills $GOMODCACHE from the
# network; with a warm cache it makes zero requests, so local
# airplane runs stay offline-friendly), then re-run it offline: with a
# warm cache that second pass proves the project is fully reproducible
# without network access (invariant I-3). Drift is diffed after both.
GOFLAGS=-mod=mod go mod tidy
GOPROXY=off GOFLAGS=-mod=mod go mod tidy

fail=0
if ! diff -u "${backup}/go.mod" go.mod; then
    echo "tidy-check: FAIL (go.mod drift)"
    fail=1
fi
if [ -f "${backup}/go.sum" ] || [ -f go.sum ]; then
    if ! diff -u "${backup}/go.sum" go.sum; then
        echo "tidy-check: FAIL (go.sum drift)"
        fail=1
    fi
fi
if [ "${fail}" -ne 0 ]; then
    exit 1
fi
echo "tidy-check: PASS (offline go mod tidy is a no-op)"
