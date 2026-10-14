#!/usr/bin/env bash
# Mutation proof for the PVLT2 hybrid recipient model (Issue 4, ADL-0031).
#
# The claims under test are authentication claims:
#
#   * M75: the wrap key is bound to the per-slot ephemeral ECDH share. A
#     static secret seals every slot under one key, so any device running
#     the same code unwraps any slot — Eve decrypts the team payload and
#     the swarm gate names the leaked bytes.
#   * M76: a failed slot unwrap is a silent skip, never data. Returning the
#     slot bytes as a "decryption" hands Eve 48 bytes with a nil error and
#     hands Bob garbage instead of his payload — both gates name it.
#
# Restore is from a cmp-verified backup — never `git checkout`, because a failed
# restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

BACKUP="${TMPDIR:-/tmp}/pulse-mut-pvlt2"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/secrets/pvlt2.go"
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
run_gate 'Swarm|TamperMatrix|VerifyBefore|Rotation|SealFails|IdentityRound|KeygenFails|VaultKeygen' ./internal/secrets/ ./internal/app/
echo

echo "############################################################"
echo "# MUTATION 75: the wrap key ignores the ECDH share"
echo "############################################################"
echo "# One static secret for every slot. Eve trials Alice's slot with it,"
echo "# derives the identical wrap key the seal used, and reads the team"
echo "# payload — the gate prints the leaked bytes it caught her holding."
python3 - <<'EOF'
p = 'internal/secrets/pvlt2.go'
s = open(p).read()
old = """\treturn hkdfSHA256(share, ephPub, []byte(wrapInfo), 32), nil"""
assert old in s, "anchor not found"
new = """\t_ = share // M75
\treturn hkdfSHA256([]byte("pulse-static-wrap-secret-00000000"), ephPub, []byte(wrapInfo), 32), nil"""
open(p, 'w').write(s.replace(old, new, 1))
EOF
grep -q 'M75' internal/secrets/pvlt2.go || { echo "MUTATION NOT APPLIED"; exit 1; }
run_gate 'Swarm' ./internal/secrets/
restore
echo "# restored: $(cmp -s "${BACKUP}/internal_secrets_pvlt2.go" internal/secrets/pvlt2.go && echo cmp-clean)"
echo

echo "############################################################"
echo "# MUTATION 76: a failed unwrap returns the slot bytes as data"
echo "############################################################"
echo "# Bob stops at the first slot (not his), receives 48 bytes of wrapped"
echo "# key as his 'payload'; Eve receives the same with a nil error instead"
echo "# of ErrNoRecipient. The tamper matrix's unknown-device case names it"
echo "# independently."
python3 - <<'EOF'
p = 'internal/secrets/pvlt2.go'
s = open(p).read()
old = """\t\tdek, err := wrapAEAD.Open(nil, zeroNonce[:], s.wrapped, nil)
\t\tif err != nil {
\t\t\tcontinue // not our slot: identical refusal shape, no oracle
\t\t}"""
assert old in s, "anchor not found"
new = """\t\tdek, err := wrapAEAD.Open(nil, zeroNonce[:], s.wrapped, nil)
\t\tif err != nil {
\t\t\treturn s.wrapped, nil // M76
\t\t}"""
open(p, 'w').write(s.replace(old, new, 1))
EOF
grep -q 'M76' internal/secrets/pvlt2.go || { echo "MUTATION NOT APPLIED"; exit 1; }
run_gate 'Swarm|TamperMatrix' ./internal/secrets/
restore
echo "# restored: $(cmp -s "${BACKUP}/internal_secrets_pvlt2.go" internal/secrets/pvlt2.go && echo cmp-clean)"
echo

echo "############################################################"
echo "# FINAL (restored, green)"
echo "############################################################"
run_gate 'Swarm|TamperMatrix|VerifyBefore|Rotation|SealFails|IdentityRound|KeygenFails|VaultKeygen' ./internal/secrets/ ./internal/app/
