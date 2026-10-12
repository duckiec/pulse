#!/usr/bin/env bash
# Mutation-rule proof for Move 2 (AGENTS.md §4.2).
#
# Two parsing boundaries are deliberately broken, one at a time, and the test
# suite must go RED for each. The code is restored afterwards (verified with
# cmp, not with git, so the proof works before and after the commit) and the
# suite must return to GREEN. The transcript this script prints is the evidence.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=ci/env.sh
source "$(dirname "$0")/ci/env.sh"

PARSER=internal/ast/parser.go
BACKUP="$(mktemp -d)/parser.go.orig"
cp "${PARSER}" "${BACKUP}"
trap 'cp "${BACKUP}" "${PARSER}"' EXIT

mutate() { # mutate <sed-expression> <marker>
    sed -i "$1" "${PARSER}"
    if ! grep -qF "$2" "${PARSER}"; then
        echo "MUTATION NOT APPLIED: missing marker $2"
        cp "${BACKUP}" "${PARSER}"
        exit 1
    fi
}

restore() {
    cp "${BACKUP}" "${PARSER}"
    if ! cmp -s "${BACKUP}" "${PARSER}"; then
        echo "RESTORE FAILED"
        exit 1
    fi
}

echo "############################################################"
echo "# BASELINE (unmutated code)"
echo "############################################################"
CGO_ENABLED=0 go test -count=1 ./internal/ast/ 2>&1 | tail -3
echo

echo "############################################################"
echo "# MUTATION 1: drop the mandatory '=' boundary in a variable"
echo "# definition (@bad-var is accepted as if the line had parsed)"
echo "############################################################"
mutate 's|if eq < 0 {|if false { // MUTANT-1|' '// MUTANT-1'
CGO_ENABLED=0 go test -count=1 ./internal/ast/ 2>&1 | tail -12
restore
echo

echo "############################################################"
echo "# MUTATION 2: ignore the required '# @assert' directive"
echo "# boundary (directive renamed so no assert is ever recognised),"
echo "# yielding an AST that silently drops every assert node"
echo "############################################################"
mutate 's|assertDirective = "# @assert"|assertDirective = "# @asserT" // MUTANT-2|' '// MUTANT-2'
CGO_ENABLED=0 go test -count=1 -run 'TestGoldenAST' ./internal/ast/ 2>&1 | tail -24
restore
echo

echo "############################################################"
echo "# RESTORED (code back to the pre-mutation implementation)"
echo "############################################################"
if ! cmp -s "${BACKUP}" "${PARSER}"; then
    echo "RESTORE FAILED: ${PARSER} differs from the backup"
    exit 1
fi
echo "${PARSER} restored: identical to the pre-mutation copy"
CGO_ENABLED=0 go test -count=1 ./internal/ast/ 2>&1 | tail -3