#!/usr/bin/env bash
# Mutation-rule proof for Move 3 (AGENTS.md §4.2).
#
# Three streaming boundaries are deliberately broken, one at a time. Each must
# turn the suite RED with a specific, legible failure:
#   M3  the queue cap — removing it must blow the in-flight ceiling;
#   M4  chunk size   — a smaller chunk must break the 32 KiB geometry;
#   M5  recycle      — not returning buffers to the pool must break G-alloc.
# The code is restored after each (verified with cmp, so the proof works before
# and after the commit) and the suite must return to GREEN.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=ci/env.sh
source "$(dirname "$0")/ci/env.sh"

PKG=internal/engine
BACKUP="${TMPDIR:-/tmp}/pulse-engine-mutation-backup"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"
cp "${PKG}"/*.go "${BACKUP}/"
trap 'cp "${BACKUP}"/*.go "${PKG}/"' EXIT

mutate() { # mutate <file> <sed-expression> <marker>
    sed -i "$2" "${PKG}/$1"
    if ! grep -qF "$3" "${PKG}/$1"; then
        echo "MUTATION NOT APPLIED: missing marker $3"
        restore
        exit 1
    fi
}

restore() {
    cp "${BACKUP}"/*.go "${PKG}/"
    for f in "${BACKUP}"/*.go; do
        if ! cmp -s "${f}" "${PKG}/$(basename "${f}")"; then
            echo "RESTORE FAILED for $(basename "${f}")"
            exit 1
        fi
    done
}

echo "############################################################"
echo "# BASELINE (unmutated code)"
echo "############################################################"
CGO_ENABLED=0 go test -count=1 -timeout 300s ./internal/engine/ 2>&1 | tail -3
echo

echo "############################################################"
echo "# MUTATION 3: widen the queue cap from 64 to 100000 — the"
echo "# ceiling is gone, so a stalled consumer parks the whole"
echo "# response in memory (the spike this gate exists to catch)"
echo "############################################################"
mutate engine.go 's|chunks := make(chan \*Chunk, chunkChannelCap)|chunks := make(chan *Chunk, 100_000) // MUTANT-3|' '// MUTANT-3'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestMemoryCeilingInvariant' ./internal/engine/ 2>&1 | tail -18
restore
echo

echo "############################################################"
echo "# MUTATION 4: shrink the chunk to 4 KiB, breaking the 32 KiB"
echo "# geometry the 2 MiB ceiling is computed from"
echo "############################################################"
mutate stream.go 's|chunkSize = 32 << 10 // 32 KiB|chunkSize = 4 << 10 // MUTANT-4|' '// MUTANT-4'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestChunkGeometry|TestMemoryCeilingInvariant' ./internal/engine/ 2>&1 | tail -14
restore
echo

echo "############################################################"
echo "# MUTATION 5: stop recycling buffers (the one-way ownership"
echo "# rule is broken) — the pool must then allocate per chunk"
echo "############################################################"
mutate stream.go 's|\tchunkPool.Put(buf)|\t_ = buf // MUTANT-5|' '// MUTANT-5'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run '^$' -bench 'BenchmarkStreamSteadyState' -benchmem -benchtime=50x ./internal/engine/ 2>&1 | grep -E 'Benchmark|FAIL|allocs'
restore
echo

echo "############################################################"
echo "# RESTORED (code back to the pre-mutation implementation)"
echo "############################################################"
restore
echo "${PKG}/*.go restored: identical to the pre-mutation copies"
CGO_ENABLED=0 go test -count=1 -timeout 300s ./internal/engine/ 2>&1 | tail -3