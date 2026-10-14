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
# Wave 8 adds two wiring mutants beside the domain pair:
#
#   * M93: the discovery channel-ownership fix is load-bearing. Closing
#     out from the resolver goroutine while the pump still sends is the
#     Stage-4 send-on-closed-channel panic; the fast-cancel gate must die
#     on it (red). A panic counts as red here, not just --- FAIL.
#   * M94: the readLoop TOFU-observability branch is load-bearing. A
#     dialed peer dying pre-first-frame must be LOUD (the TLS 1.3 alert
#     lands post-Dial-success); silencing it blinds the e2e C leg, and the
#     white-box gate must fail (red).
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

FILES="internal/sync/crdt.go internal/sync/tls.go internal/sync/discovery.go internal/app/sync.go"
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
    if grep -q -- '--- FAIL' "${BACKUP}/out.txt" || grep -q -- '^panic: ' "${BACKUP}/out.txt"; then
        echo "MUTANT CAUGHT: ${label} went red as required"
    else
        echo "MUTANT SURVIVED: ${label} stayed green — the gate proves nothing"
        restore
        exit 1
    fi
}

echo "############################################################"
echo "# BASELINE (unmutated): the sync + app suites are green"
echo "############################################################"
CGO_ENABLED=0 go test -count=1 -timeout 120s ./internal/sync/ ./internal/app/ 2>&1 | tail -3
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
echo "# MUTATION 93: resolver goroutine closes out under a live pump"
echo "############################################################"
echo "# close(out) belongs strictly to the pump (sole producer). Closing it"
echo "# from the resolver side reintroduces the Stage-4 race: under fast"
echo "# cancel the pump sends on a closed channel and the runtime panics."
python3 - <<'PY'
p = 'internal/sync/discovery.go'
s = open(p).read()
before = s
s = s.replace('''	go func() {
		if err := resolver.Browse(bctx, ServiceName, ServiceDomain, entries); err != nil {''', '''	go func() {
		// MUTANT-93: close from the wrong side. The pump still owns its
		// sends, so fast cancel turns this into send-on-closed-channel.
		defer close(out)
		if err := resolver.Browse(bctx, ServiceName, ServiceDomain, entries); err != nil {''', 1)
if s == before:
    raise SystemExit('M93 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
grep -q 'MUTANT-93' internal/sync/discovery.go || { echo "MUTATION NOT APPLIED"; restore; exit 1; }
expect_red "M93 close(out) on resolver side" -run 'TestBrowseFastCancelNeverPanics' ./internal/sync/
restore
echo

echo "############################################################"
echo "# MUTATION 94: pre-first-frame death goes silent"
echo "############################################################"
echo "# Dropping the readLoop observability Logf blinds TOFU rejection: the"
echo "# dial succeeded, the alert arrived on first Read, and nobody says so."
python3 - <<'PY'
p = 'internal/app/sync.go'
s = open(p).read()
before = s
s = s.replace('''mcp.Logf("sync: dial %s: connection failed before first frame: %v", owner, err)''', '''/* MUTANT-94: silent rejection */ _ = err''', 1)
if s == before:
    raise SystemExit('M94 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
grep -q 'MUTANT-94' internal/app/sync.go || { echo "MUTATION NOT APPLIED"; restore; exit 1; }
expect_red "M94 silent pre-first-frame death" -run 'TestReadLoop' ./internal/app/
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
CGO_ENABLED=0 go test -count=1 -timeout 120s ./internal/sync/ ./internal/app/ 2>&1 | tail -3
