#!/usr/bin/env bash
# CGO-free cross-build matrix (AGENTS.md §2.1). Builds every supported target
# with CGO_ENABLED=0; a single failure exits non-zero and fails the build.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=env.sh
source "$(dirname "$0")/env.sh"

# Six targets (Issue #11 release bar): Intel + ARM Macs and ARM Windows are
# real machines Pulse must run on, and a CGO-free tree builds them for free.
targets=("linux/amd64" "linux/arm64" "darwin/amd64" "darwin/arm64" "windows/amd64" "windows/arm64")
for t in "${targets[@]}"; do
    echo "== CGO_ENABLED=0 GOOS=${t%%/*} GOARCH=${t##*/} go build ./..."
    CGO_ENABLED=0 GOOS="${t%%/*}" GOARCH="${t##*/}" go build -trimpath ./...
done
echo "build-matrix: PASS (6/6 targets, CGO-free)"
