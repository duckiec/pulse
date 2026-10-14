#!/usr/bin/env bash
# Mutation proof for Wave 7 (P2P sync: CRDT + HLC + mTLS TOFU + zeroconf).
#
# The claims under test are convergence and authentication claims:
#
#   * M91: the OR-Set tombstone union is load-bearing. A merge that
#     ignores tombstones resurrects deleted items on some peers while
#     others hold the delete, so the 3-node gate must diverge (red).
#   * M92: the TOFU fingerprint check is load-bearing. A
#     VerifyPeerCertificate that waves everyone through must let the
#     adversarial unpinned peer connect, so the refusal gate must fail
#     (red) — the connection succeeding IS the failure.
#
# Unlike earlier census scripts this one MACHINE-CHECKS the red: a mutant
# the gate does not catch exits nonzero ("survived") instead of printing
# past it. Restore is from a cmp-verified backup — never `git checkout`,
# because a failed restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
export PATH=/home/duckie/sdk/go1.27.2/bin:$PATH
export GOTOOLCHAIN=local
export CGO_ENABLED=0

BACKUP="${TMPDIR:-/tmp}/pulse-mut-sync"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/sync/crdt.go internal/sync/tls.go"
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

# expect_red <label> <go-test-args...>: the gate MUST fail. A passing gate
# under mutation means the mutant survived and the proof is void.
expect_red() {
    local label="$1"; shift
    echo "--- ${label} (expecting RED) ---"
    if CGO_ENABLED=0 go test -count=1 -timeout 120s "$@" 2>&1 | tee "${BACKUP}/out.txt" | grep -E -- '--- FAIL|^ok |^FAIL' | head -6; then
        true
    fi
    if grep -q -- '--- FAIL' "${BACKUP}/out.txt"; then
        echo "MUTANT CAUGHT: ${label} went red as required"
    else
        echo "MUTANT SURVIVED: ${label} stayed green — the gate proves nothing"
        restore
        exit 1
    fi
}

echo "############################################################"
echo "# BASELINE (unmutated): the full sync suite is green"
echo "############################################################"
CGO_ENABLED=0 go test -count=1 -timeout 120s ./internal/sync/ 2>&1 | tail -2
echo

echo "############################################################"
echo "# MUTATION 91: merge ignores OR-Set tombstones"
echo "############################################################"
echo "# Deleted items resurrect on peers that never see the tombstone while"
echo "# the remover holds the delete: digests diverge forever and the"
echo "# convergence gate must fail on byte-inequality (split brain)."
python3 - <<'PY'
p = 'internal/sync/crdt.go'
s = open(p).read()
before = s
s = s.replace('''	if it.Removed == nil {
		it.Removed = map[uint64]bool{}
	}
	for tag := range o.Removed {
		it.Removed[tag] = true
	}''', '''	// MUTANT-91: tombstones are dropped on the floor. Removed sets never
	// grow, so a delete known to one peer is unknown everywhere else and
	// the same item is simultaneously dead and alive: split brain.
	_ = o.Removed''', 1)
if s == before:
    raise SystemExit('M91 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
grep -q 'MUTANT-91' internal/sync/crdt.go || { echo "MUTATION NOT APPLIED"; restore; exit 1; }
expect_red "M91 tombstones ignored" -run 'TestThreeNodeConvergence|TestDoubleDeliveryIsANoOp' ./internal/sync/
restore
echo

echo "############################################################"
echo "# MUTATION 92: VerifyPeerCertificate waves everyone through"
echo "############################################################"
echo "# Skipping the peers.txt fingerprint check turns TOFU into open"
echo "# enrollment: the adversarial unpinned peer's handshake must then"
echo "# SUCCEED, which is exactly what the refusal gate asserts against."
python3 - <<'PY'
p = 'internal/sync/tls.go'
s = open(p).read()
before = s
s = s.replace('''	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {''', '''	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		// MUTANT-92: the ledger is never consulted. Every presenter —
		// pinned, unknown, or mismatched — is trusted on first use AND
		// every later use, which is not trust-on-first-use but trust-always.
		_ = pins
		return nil
		if len(rawCerts) == 0 {''', 1)
if s == before:
    raise SystemExit('M92 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
grep -q 'MUTANT-92' internal/sync/tls.go || { echo "MUTATION NOT APPLIED"; restore; exit 1; }
expect_red "M92 TOFU skipped" -run 'TestAdversarialUnpinnedPeerRefused|TestMismatchedPinRefused|TestPeersTxtPinningFlow' ./internal/sync/
restore
echo

echo "############################################################"
echo "# RESTORED (cmp-verified) + GREEN re-run"
echo "############################################################"
for f in ${FILES}; do
    if cmp -s "${BACKUP}/$(echo "${f}" | tr / _)" "${f}"; then
        echo "  ${f}: identical to the pre-mutation copy"
    else
        echo "  ${f}: DIFFERS FROM BACKUP"
        exit 1
    fi
done
CGO_ENABLED=0 go test -count=1 -timeout 120s ./internal/sync/ 2>&1 | tail -2
