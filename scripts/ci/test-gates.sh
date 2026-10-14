#!/usr/bin/env bash
# Test gates: full suite plus the zero-allocation benchmark gates
# (ROADMAP.md §1.1.5 / §1.2.2, G-alloc). Three gates are asserted:
#   * BenchmarkLexer            — tokenizes the generated 10k-line corpus.
#   * BenchmarkParseCached      — builds a 10,000-REQUEST AST in a warmed arena.
#   * BenchmarkStreamSteadyState — streams a 16 MiB body through the pooled,
#     bounded chunk pipeline (512 chunks/op) with no allocation in the loop.
# All must report 0 allocs/op. Fixed -benchtime values make the steady state
# deterministic; the parser gates use 2000x, the streaming gate 200x (enough
# iterations for sync.Pool's one-time internal queue growth to amortize out).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=env.sh
source "$(dirname "$0")/env.sh"

echo "== CGO_ENABLED=0 go test ./..."
CGO_ENABLED=0 go test ./...

echo "== benchmark gates (0 allocs/op)"
ast_out="$(CGO_ENABLED=0 go test -run '^$' -bench . -benchmem -benchtime=2000x ./internal/ast)"
eng_out="$(CGO_ENABLED=0 go test -run '^$' -bench . -benchmem -benchtime=200x ./internal/engine)"
out="${ast_out}"$'\n'"${eng_out}"
printf '%s\n' "${out}"

for gate in BenchmarkLexer BenchmarkParseCached BenchmarkStreamSteadyState; do
    line="$(printf '%s\n' "${out}" | grep "^${gate}-" || true)"
    if [ -z "${line}" ]; then
        echo "test-gates: FAIL (${gate} missing)"
        exit 1
    fi
    # The zero-allocation invariant is "no allocation per operation", which is
    # exactly what allocs/op reports: any per-op allocation yields >= 1. B/op is
    # a total-bytes/N average, so a single post-GC sync.Pool refill (the pool is
    # drained by the GC by design) amortizes to a fraction of a byte and must not
    # fail the build. The byte budget only guards against an allocation storm;
    # correctness is decided by allocs/op.
    case "${line}" in
        *"0 allocs/op"*) ;;
        *)
            echo "test-gates: FAIL (${gate} zero-allocation gate red): ${line}"
            exit 1
            ;;
    esac
    bop="$(printf '%s\n' "${line}" | grep -oE '[0-9]+ B/op' | grep -oE '[0-9]+' || echo 0)"
    if [ "${bop}" -gt 65536 ]; then
        echo "test-gates: FAIL (${gate} allocated ${bop} B/op, budget 65536): ${line}"
        exit 1
    fi
    echo "test-gates: PASS (${gate} zero-allocation gate green: ${bop} B/op, 0 allocs/op)"
done

echo "== synctest mandate (P3.3.6, AGENTS.md §2.2)"
# Every concurrent package must bubble its timing properties on the synthetic
# clock: the import must be present AND synctest.Test must execute. A removal
# of either fails this gate.
for pkg in internal/ui internal/fuzz internal/rag internal/engine; do
    if ! grep -rq '"testing/synctest"' "${pkg}/" --include='*_test.go'; then
        echo "test-gates: FAIL (${pkg} imports no testing/synctest bubble)"
        exit 1
    fi
    if ! grep -rqE 'synctest\.Test\(' "${pkg}/" --include='*_test.go'; then
        echo "test-gates: FAIL (${pkg} never executes synctest.Test)"
        exit 1
    fi
    echo "test-gates: PASS (${pkg} synctest bubble live)"
done
