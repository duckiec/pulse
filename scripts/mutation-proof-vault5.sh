#!/usr/bin/env bash
# Mutation proof for Issue 5 (vault resolution + recipient management).
#
# Claims under test:
#   M77: the vault tier is load-bearing in the resolution chain. Bypassing the
#        .env.vault fallback must make the seamless gate fail with the strict
#        terminal error (secrets.ErrUnresolved), not with a wrong value.
#   M78: rotation seals for the WHOLE recipient list. Silently dropping the
#        last recipient must make the rotate gate fail naming Carol.
#
# Restore is from a cmp-verified backup — never `git checkout`, because a
# failed restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
export PATH=/home/duckie/sdk/go1.27.2/bin:$PATH
export GOTOOLCHAIN=local
export CGO_ENABLED=0

BACKUP="${TMPDIR:-/tmp}/pulse-mut-issue5"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/secrets/resolver.go internal/app/test.go"
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

echo "############################################################"
echo "# BASELINE (unmutated): both gates green"
echo "############################################################"
go test -count=1 -run 'TestSeamlessVaultResolutionIsTransparentToTheRunner|TestVaultRotateResealsForTheWholeList' ./internal/app/ 2>&1 | tail -2
echo

echo "############################################################"
echo "# M77: bypass the .env.vault fallback in Chain.Resolve"
echo "############################################################"
python3 - <<'EOF'
import io
p = "internal/secrets/resolver.go"
s = open(p).read()
old = "\t// 3. Decrypted .env.vault payload (memory only, opened at boot).\n\tif v, ok := vault[key]; ok {"
assert old in s, "M77 marker missing"
s = s.replace(old, "\t// 3. Decrypted .env.vault payload (memory only, opened at boot).\n\tif v, ok := vault[key]; ok && false { // M77 MUTANT: vault tier bypassed")
open(p, "w").write(s)
print("M77 applied")
EOF
echo "--- red run (expect FAIL + secrets.ErrUnresolved) ---"
set +e
go test -count=1 -run 'TestSeamlessVaultResolutionIsTransparentToTheRunner' ./internal/app/ 2>&1 | grep -E -- '--- FAIL|^FAIL|^ok |vault_test.go|unresolved' | head -6
red=$?
set -e
echo "--- restoring ---"
restore && echo "restore: cmp clean"
echo "--- green run ---"
go test -count=1 -run 'TestSeamlessVaultResolutionIsTransparentToTheRunner' ./internal/app/ 2>&1 | tail -1
echo

echo "############################################################"
echo "# M78: rotate drops the last recipient in the re-seal loop"
echo "############################################################"
python3 - <<'EOF'
p = "internal/app/test.go"
s = open(p).read()
old = "\tres, err := secrets.Seal2(payload, recipients, signer)"
assert old in s, "M78 marker missing"
s = s.replace(old, "\tres, err := secrets.Seal2(payload, recipients[:len(recipients)-1], signer) // M78 MUTANT: last recipient dropped")
open(p, "w").write(s)
print("M78 applied")
EOF
echo "--- red run (expect FAIL naming carol) ---"
set +e
go test -count=1 -run 'TestVaultRotateResealsForTheWholeList' ./internal/app/ 2>&1 | grep -E -- '--- FAIL|^FAIL|^ok |vault_test.go|carol' | head -6
set -e
echo "--- restoring ---"
restore && echo "restore: cmp clean"
echo "--- green run ---"
go test -count=1 -run 'TestVaultRotateResealsForTheWholeList' ./internal/app/ 2>&1 | tail -1
