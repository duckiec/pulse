#!/usr/bin/env bash
# Mutation-rule proof for Move 4 (AGENTS.md §4.2): secrets, redaction, storage.
#
# Five zero-trust boundaries are deliberately broken, one at a time. Each must
# turn the suite RED with a legible failure:
#   M6a redaction    — the allocating path stops replacing (required proof);
#   M6b redaction    — the IN-PLACE streaming scrub stops replacing, so a
#                      secret echoed in a response body reaches the Sink;
#   M7  fail-closed  — the chain returns no error at all;
#   M8  schema audit — the credential-column rule stops matching;
#   M9  secure_delete— deleted rows would survive in the raw file.
# Code is restored after each (verified with cmp, so this works before and after
# the commit) and the suite must return to GREEN.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=ci/env.sh
source "$(dirname "$0")/ci/env.sh"

SECRETS=internal/secrets/redact.go
RESOLVER=internal/secrets/resolver.go
STORE=internal/store/sqlite.go
BACKUP="${TMPDIR:-/tmp}/pulse-move4-backup"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"
cp "${SECRETS}" "${RESOLVER}" "${STORE}" "${BACKUP}/"

restore() {
    cp "${BACKUP}/redact.go" "${SECRETS}"
    cp "${BACKUP}/resolver.go" "${RESOLVER}"
    cp "${BACKUP}/sqlite.go" "${STORE}"
    local pair src dst
    for pair in "redact.go:${SECRETS}" "resolver.go:${RESOLVER}" "sqlite.go:${STORE}"; do
        src="${pair%%:*}"
        dst="${pair##*:}"
        if ! cmp -s "${BACKUP}/${src}" "${dst}"; then
            echo "RESTORE FAILED for ${dst}"
            exit 1
        fi
    done
}
trap restore EXIT

mutate() { # mutate <file> <sed-expr> <marker>
    sed -i "$2" "$1"
    if ! grep -qF "$3" "$1"; then
        echo "MUTATION NOT APPLIED: missing marker $3"
        restore
        exit 1
    fi
}

echo "############################################################"
echo "# BASELINE (unmutated code)"
echo "############################################################"
CGO_ENABLED=0 go test -count=1 -timeout 300s ./internal/secrets/ ./internal/store/ ./internal/engine/ 2>&1 | tail -4
echo

echo "############################################################"
echo "# MUTATION 6a: the allocating redaction path stops replacing"
echo "############################################################"
mutate "${SECRETS}" 's|^\tchanged := false$|\tchanged := false; return s // MUTANT-6a|' '// MUTANT-6a'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'Redact|Scrub' ./internal/secrets/ 2>&1 | grep -E 'SECRET LEAKED|survived|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# MUTATION 6b: the IN-PLACE streaming scrub stops replacing,"
echo "# so a secret echoed in a response body reaches the Sink"
echo "############################################################"
mutate "${SECRETS}" 's|^\teligible := make(\[\]\[\]byte, 0, len(snapshot))$|\teligible := make([][]byte, 0, len(snapshot)); return len(b), 0 // MUTANT-6b|' '// MUTANT-6b'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'ResponseBodyIsRedacted' ./internal/engine/ 2>&1 | grep -E 'SECRET LEAKED|--- FAIL|^FAIL|^ok' | head -5
restore
echo

echo "############################################################"
echo "# MUTATION 7: the chain returns no error on an unresolved"
echo "# variable (AGENTS.md 4.3 forbids a dummy/empty fallback)"
echo "############################################################"
mutate "${RESOLVER}" 's|^\t\treturn "", fmt.Errorf("%w: %q (%v)", ErrUnresolved, key, lastErr)$|\t\treturn "", nil // MUTANT-7|' '// MUTANT-7'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestChainFailClosed|TestResolveAllFailsOnFirstMiss' ./internal/secrets/ 2>&1 | grep -E 'must be an error|expected failure|does not wrap|exit code|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# MUTATION 8: the credential-column rule stops matching"
echo "############################################################"
mutate "${STORE}" 's|^\tlower := strings.ToLower(name)$|\tlower := ""; _ = name // MUTANT-8|' '// MUTANT-8'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'Schema' ./internal/store/ 2>&1 | grep -E 'credential-capable|failed to flag|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# MUTATION 9: secure_delete is dropped from the DSN, so a"
echo "# deleted row would survive as recoverable free cells"
echo "############################################################"
sed -i 's|_pragma=secure_delete(1)||; s|dsn := path + "?|dsn := path + "? // MUTANT-9|' "${STORE}"
if ! grep -qF '// MUTANT-9' "${STORE}"; then
    echo "MUTATION NOT APPLIED: missing marker // MUTANT-9"
    restore
    exit 1
fi
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'SecureDelete' ./internal/store/ 2>&1 | grep -E 'secure_delete|still present|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# RESTORED (code back to the pre-mutation implementation)"
echo "############################################################"
restore
echo "internal/secrets + internal/store restored: identical to the pre-mutation copies"
CGO_ENABLED=0 go test -count=1 -timeout 300s ./internal/secrets/ ./internal/store/ ./internal/engine/ 2>&1 | tail -4