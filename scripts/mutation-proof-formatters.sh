#!/usr/bin/env bash
# Mutation proof for the `pulse test` JSON/silent formatters (Issue 2).
#
# The claims under test are honesty claims:
#
#   * the JSON aggregates are COPIED from the engine's report, not recomputed,
#     so the two documents cannot disagree about what the run meant;
#   * --silent changes what is WRITTEN, never what is concluded, so a silent
#     failure still exits 1 with zero bytes on stdout.
#
# Restore is from a cmp-verified backup — never `git checkout`, because a failed
# restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

BACKUP="${TMPDIR:-/tmp}/pulse-mut-formatters"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/app/test.go"
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
        grep -E -- '--- FAIL|^ok |^FAIL|_test\.go' | head -8
}

FMT='JSON|Silent|UnknownFormat'

echo "############################################################"
echo "# BASELINE (unmutated)"
echo "############################################################"
run_gate "${FMT}" ./internal/app/
echo

echo "############################################################"
echo "# MUTATION 69: the JSON formatter maps total into passed"
echo "############################################################"
echo "# The aggregates must be copied from the report. Mapping total into"
echo "# passed makes a one-pass one-fail run report passed=2, which the counts"
echo "# gate names and the array cross-check confirms independently."
sed -i 's/Passed:  report\.Passed,/Passed:  report.Total, \/\/ M69/' internal/app/test.go
grep -q 'M69' internal/app/test.go || { echo "MUTATION NOT APPLIED"; exit 1; }
run_gate 'TestJSONCountsMatchTheReport' ./internal/app/
restore
echo "# restored: $(cmp -s "${BACKUP}/internal_app_test.go" internal/app/test.go && echo cmp-clean)"
echo

echo "############################################################"
echo "# MUTATION 70: --silent falls through and prints anyway"
echo "############################################################"
echo "# Silence is a property of what is written, not of what concluded. A"
echo "# silent flag that still emits TAP breaks the contract on every run,"
echo "# pass or fail, in both formats."
python3 - <<'EOF'
p = 'internal/app/test.go'
s = open(p).read()
old = "\tif silent {\n\t\treturn res.Exit, nil\n\t}"
assert old in s, "anchor not found"
open(p, 'w').write(s.replace(old, "\tif false && silent { // M70\n\t\treturn res.Exit, nil\n\t}", 1))
EOF
grep -q 'M70' internal/app/test.go || { echo "MUTATION NOT APPLIED"; exit 1; }
run_gate 'TestSilentWritesNothingToStdoutButStillExitsCorrectly' ./internal/app/
restore
echo "# restored: $(cmp -s "${BACKUP}/internal_app_test.go" internal/app/test.go && echo cmp-clean)"
echo

echo "############################################################"
echo "# FINAL (restored, green)"
echo "############################################################"
run_gate "${FMT}" ./internal/app/
