#!/usr/bin/env bash
# Test gates: full suite plus the zero-allocation benchmark gate
# (ROADMAP.md §1.1.5). The benchmark gate asserts BenchmarkLexer reports
# exactly 0 B/op and 0 allocs/op on the generated 10k-line corpus.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=env.sh
source "$(dirname "$0")/env.sh"

echo "== CGO_ENABLED=0 go test ./..."
CGO_ENABLED=0 go test ./...

echo "== benchmark gates (0 allocs/op)"
out="$(CGO_ENABLED=0 go test -run '^$' -bench . -benchmem ./internal/ast)"
printf '%s\n' "${out}"

line="$(printf '%s\n' "${out}" | grep '^BenchmarkLexer-' || true)"
if [ -z "${line}" ]; then
    echo "test-gates: FAIL (BenchmarkLexer missing)"
    exit 1
fi
case "${line}" in
    *"0 B/op"*"0 allocs/op"*)
        echo "test-gates: PASS (zero-allocation gate green)"
        ;;
    *)
        echo "test-gates: FAIL (zero-allocation gate red): ${line}"
        exit 1
        ;;
esac
