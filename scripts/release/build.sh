#!/usr/bin/env bash
# Hand-rolled release build (no GoReleaser, no new tools): cross-compile the
# six supported targets with the release tag stamped in, then write
# checksums.txt beside the binaries. The release workflow uploads dist/*
# as GitHub Release assets; install.sh/install.ps1 download them by name.
#
# Usage: bash scripts/release/build.sh <tag> [dist-dir]
#   tag must look like v1.2.3 (checked, not assumed).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=../ci/env.sh
source "$(dirname "$0")/../ci/env.sh"

tag="${1:-}"
out="${2:-dist}"
if [[ ! "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
    echo "build.sh: tag is required and must look like v1.2.3 (got '$tag')" >&2
    exit 2
fi
rm -rf "$out"
mkdir -p "$out"

# Asset names the installers resolve: pulse-<goos>-<goarch>, plus .exe on
# Windows. Six targets, same matrix as scripts/ci/build-matrix.sh.
targets=("linux/amd64" "linux/arm64" "darwin/amd64" "darwin/arm64" "windows/amd64" "windows/arm64")
for t in "${targets[@]}"; do
    goos="${t%%/*}"
    goarch="${t##*/}"
    asset="pulse-${goos}-${goarch}"
    if [[ "$goos" == "windows" ]]; then
        asset="${asset}.exe"
    fi
    echo "== CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch -> $out/$asset"
    CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -trimpath \
        -ldflags "-X main.version=$tag" \
        -o "$out/$asset" ./cmd/pulse
done

(cd "$out" && sha256sum pulse-* > checksums.txt)
echo "build.sh: PASS ($tag, $(ls "$out"/pulse-* | wc -l) binaries + checksums.txt)"
