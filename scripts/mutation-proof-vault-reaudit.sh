#!/usr/bin/env bash
# Mutation re-audit for the vault AES-256-GCM layer (Issue 4, ADL-0029).
#
# Move 12 built the layer and proved it with M61-M68. This script re-proves
# the two load-bearing boundaries against the code as it stands, under fresh
# IDs, because a gate that has not been run since is a gate that is assumed:
#
#   * M73: the nonce comes from crypto/rand per seal, not from a constant —
#     reuse under one key breaks confidentiality AND authenticity;
#   * M74: a failed GCM Open is a refusal, never a partial plaintext — an
#     ignored error boundary hands tampered bytes to the caller as data.
#
# Restore is from a cmp-verified backup — never `git checkout`, because a failed
# restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

BACKUP="${TMPDIR:-/tmp}/pulse-mut-vault-reaudit"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/secrets/vault.go"
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

echo "############################################################"
echo "# BASELINE (unmutated)"
echo "############################################################"
run_gate 'Sealing|Tampered|EnvFile|ContainerFormat' ./internal/secrets/
echo

echo "############################################################"
echo "# MUTATION 73: the nonce is a FIXED 12-byte slice"
echo "############################################################"
echo "# A constant nonce passes a single seal and fails on the second: the"
echo "# loop is the test, and trial 1 names the reused bytes."
python3 - <<'EOF'
p = 'internal/secrets/vault.go'
s = open(p).read()
old = """\tnonce, err := generateNonce()
\tif err != nil {
\t\treturn nil, err
\t}"""
assert old in s, "anchor not found"
new = """\tnonce := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c} // M73"""
open(p, 'w').write(s.replace(old, new, 1))
EOF
grep -q 'M73' internal/secrets/vault.go || { echo "MUTATION NOT APPLIED"; exit 1; }
run_gate 'TestSealingTheSamePlaintextTwiceDiffers' ./internal/secrets/
restore
echo "# restored: $(cmp -s "${BACKUP}/internal_secrets_vault.go" internal/secrets/vault.go && echo cmp-clean)"
echo

echo "############################################################"
echo "# MUTATION 74: a failed GCM Open still returns its bytes"
echo "############################################################"
echo "# Distinct from M62 (which dropped the tag at SEAL time): this ignores"
echo "# the error at OPEN time, turning every tamper case into accepted data."
echo "# The nonce/ciphertext/tag/truncation/wrong-key subtests all name it;"
echo "# the pre-auth format checks (magic, flags, short) still pass, which is"
echo "# correct — they fail before authentication is reached."
python3 - <<'EOF'
p = 'internal/secrets/vault.go'
s = open(p).read()
old = """\tplaintext, err := aead.Open(nil, nonce, ciphertext, nil)
\tif err != nil {"""
assert old in s, "anchor not found"
new = """\tplaintext, err := aead.Open(nil, nonce, ciphertext, nil)
\tif err != nil {
\t\treturn plaintext, nil // M74"""
open(p, 'w').write(s.replace(old, new, 1))
EOF
grep -q 'M74' internal/secrets/vault.go || { echo "MUTATION NOT APPLIED"; exit 1; }
run_gate 'TestOpenRefusesTamperedContainers' ./internal/secrets/
restore
echo "# restored: $(cmp -s "${BACKUP}/internal_secrets_vault.go" internal/secrets/vault.go && echo cmp-clean)"
echo

echo "############################################################"
echo "# FINAL (restored, green)"
echo "############################################################"
run_gate 'Sealing|Tampered|EnvFile|ContainerFormat' ./internal/secrets/
