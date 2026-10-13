#!/usr/bin/env bash
# Mutation proof for the `pulse test` SIGINT graceful halt (Issue 3).
#
# The claims under test are exit-honesty claims:
#
#   * a run halted by SIGINT exits 130, not 1: conflating "the user stopped
#     the run" with "a test failed" sends the operator in the wrong direction;
#   * the queue actually halts on cancellation instead of running the whole
#     collection anyway while merely reporting differently.
#
# Restore is from a cmp-verified backup — never `git checkout`, because a failed
# restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

BACKUP="${TMPDIR:-/tmp}/pulse-mut-sigint"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/engine/test_runner.go"
for f in ${FILES}; do
    cp "${f}" "${BACKUP}/$(echo "${f}" | tr / _)"
done

restore() {
    local bad=0
    for f in ${FILES}; do
        cp "${BACKUP}/$(echo "${f}" | tr / _)" "${f}"
        if ! cmp -s "${BACKUP}/$(echo "${f}" | tr / _)" "${f}"; then
            echo "  RESTORE FAILED for ${f}"
            bad=1
        fi
    done
    return ${bad}
}
trap restore EXIT

run_gate() { # run_gate <regex> <package>
    CGO_ENABLED=0 go test -count=1 -timeout 300s -run "$1" "$2" 2>&1 |
        grep -E -- '--- FAIL|^ok |^FAIL|_test\.go' | head -10
}

SIGINT='SIGINT|BailOut|Cancelled|Interrupted'

echo "############################################################"
echo "# BASELINE (unmutated)"
echo "############################################################"
run_gate "${SIGINT}" ./internal/engine/
run_gate 'Interrupted' ./internal/app/
echo

echo "############################################################"
echo "# MUTATION 71: a SIGINT cancellation falls through to exit 1"
echo "############################################################"
echo "# The 130 mapping is the whole feature at the exit-code layer: without"
echo "# it an interrupted run is indistinguishable from a test failure, and"
echo "# the operator debugs assertions when they should just re-run."
sed -i 's/report\.ExitCode = ExitInterrupted/report.ExitCode = ExitFailed \/\/ M71/' internal/engine/test_runner.go
grep -q 'M71' internal/engine/test_runner.go || { echo "MUTATION NOT APPLIED"; exit 1; }
run_gate 'SIGINT|Cancelled|Interrupted' ./internal/engine/ ./internal/app/
restore
echo "# restored: $(cmp -s "${BACKUP}/internal_engine_test_runner.go" internal/engine/test_runner.go && echo cmp-clean)"
echo

echo "############################################################"
echo "# MUTATION 72: the queue ignores the cancelled context"
echo "############################################################"
echo "# Without the ctx.Done branch the semaphore still bounds concurrency,"
echo "# so every file eventually dispatches against a dead context and fails"
echo "# fast — the run takes the same time while testing nothing halted. The"
echo "# server hit-count and the Interrupted flag both name it."
python3 - <<'EOF'
p = 'internal/engine/test_runner.go'
s = open(p).read()
old = """\t\t\tselect {\n\t\t\tcase sem <- struct{}{}:\n\t\t\tcase <-ctx.Done():\n\t\t\t\tresults[i] = fileResult{err: ctx.Err()}\n\t\t\t\treturn\n\t\t\t}"""
assert old in s, "anchor not found"
new = """\t\t\tsem <- struct{}{} // M72"""
open(p, 'w').write(s.replace(old, new, 1))
EOF
grep -q 'M72' internal/engine/test_runner.go || { echo "MUTATION NOT APPLIED"; exit 1; }
run_gate 'SIGINT|Cancelled|BailOut' ./internal/engine/
restore
echo "# restored: $(cmp -s "${BACKUP}/internal_engine_test_runner.go" internal/engine/test_runner.go && echo cmp-clean)"
echo

echo "############################################################"
echo "# FINAL (restored, green)"
echo "############################################################"
run_gate "${SIGINT}" ./internal/engine/
run_gate 'Interrupted' ./internal/app/
