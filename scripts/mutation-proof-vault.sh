#!/usr/bin/env bash
# Mutation proof for Move 12 (vault cryptography + the CI/CD execution loop).
#
# The claims under test are security and honesty claims:
#
#   * the AES-GCM nonce is unique per seal, which is the one property that
#     makes GCM's authentication meaningful at all;
#   * the exit code is a function of the RESULTS, not of the runner's
#     convenience, because a green facade here is indistinguishable from a
#     passing build;
#   * the container is authenticated, so a flipped byte cannot decrypt to
#     garbage.
#
# Restore is from a cmp-verified backup — never `git checkout`, because a failed
# restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

BACKUP="${TMPDIR:-/tmp}/pulse-mut-move12"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/secrets/vault.go internal/engine/test_runner.go internal/app/test.go internal/fuzz/tap.go"
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

mutate() { # mutate <file> <marker>
    if ! grep -qF "$2" "$1"; then
        echo "MUTATION NOT APPLIED: missing marker $2 in $1"
        restore
        exit 1
    fi
}

run_gate() { # run_gate <regex> <package>
    CGO_ENABLED=0 go test -count=1 -timeout 300s -run "$1" "$2" 2>&1 |
        grep -E -- '--- FAIL|^ok |^FAIL|_test\.go' | head -8
}

VAULT='Vault|Container|Sealing|Nonce|EnvFile|Open|MemoryStore|Store|PlatformStore|Seal'
RUNNER='ExitCode|Assertion|FilesAre|Parallel|Cancelled|DefaultParallel|ParseFailure|Malformed|Bounded|BodyAssertions|Header|Stable|AParseFailure'
APTAP='TAP|ExitCode|StrictBlocked|VaultPaths|WriteTestReport'

echo "############################################################"
echo "# BASELINE (unmutated)"
echo "############################################################"
run_gate "${VAULT}" ./internal/secrets/
run_gate "${RUNNER}" ./internal/engine/
run_gate "${APTAP}" ./internal/app/
echo

echo "############################################################"
echo "# MUTATION 61: the nonce is a FIXED 12-byte slice, not crypto/rand.Read"
echo "############################################################"
echo "# This is the directive's headline mutation and the most important one in"
echo "# this file. A fixed nonce under one key is not a weakness but a break:"
echo "# GCM derives its keystream from (key, nonce), so two seals of different"
echo "# plaintexts XOR to their difference, and the AUTHENTICATION KEY itself is"
echo "# recoverable — an attacker can then forge a third container that verifies."
echo "# The gate must catch it on the SECOND seal, which is the whole defect."
python3 - <<'PY'
p = 'internal/secrets/vault.go'
s = open(p).read()
before = s
s = s.replace('''func generateNonce() ([]byte, error) {
	nonce := make([]byte, VaultNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: generate a vault nonce: %w", err)
	}
	return nonce, nil
}''', '''func generateNonce() ([]byte, error) {
	// MUTANT-61: a hardcoded nonce, exactly the historic AES-CTR/CBC class of bug.
	// The container still ENCRYPTS and DECRYPTS perfectly, so only a gate that
	// seals twice and compares catches it.
	return []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0a, 0x0b, 0x0c}, nil
}''', 1)
if s == before:
    raise SystemExit('M61 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/secrets/vault.go '// MUTANT-61'
run_gate "${VAULT}" ./internal/secrets/
restore
echo

echo "############################################################"
echo "# MUTATION 62: the ciphertext is NOT authenticated — the tag is dropped"
echo "############################################################"
echo "# A tampered container must fail to decrypt. If Open skips the tag, a"
echo "# single flipped byte decrypts to garbage that a caller may accept as a"
echo "# plaintext, and the vault's integrity guarantee is theatre."
python3 - <<'PY'
p = 'internal/secrets/vault.go'
s = open(p).read()
before = s
s = s.replace('''	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// crypto/cipher describes a tag mismatch as "message authentication
		// failed", which would leak that authentication was attempted at all.
		return nil, fmt.Errorf("%w (the key is wrong, the container was tampered, "+
			"or it was truncated)", ErrAuthFailed)
	}
	return plaintext, nil''', '''	// MUTANT-62: the authentication tag is not checked. The ciphertext is
	// returned with its tag stripped, so a flipped byte decrypts to garbage the
	// caller cannot tell from a plaintext. `aead` and `nonce` are kept
	// referenced, because an unused local is a COMPILE error, and a compile
	// error proves nothing about the boundary it was going to test.
	_ = nonce
	raw := ciphertext[:len(ciphertext)-aead.Overhead()]
	return raw, nil''', 1)
if s == before:
    raise SystemExit('M62 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/secrets/vault.go '// MUTANT-62'
run_gate "${VAULT}" ./internal/secrets/
restore
echo

echo "############################################################"
echo "# MUTATION 63: a failing assertion yields exit 0"
echo "############################################################"
echo "# The green facade. A CI pipeline driven by this exit code would report"
echo "# every red build as green, which is exactly the failure mode"
echo "# AGENTS.md 4.5 names: 'NEVER fake or greenwash a skipped test as a pass.'"
python3 - <<'PY'
p = 'internal/engine/test_runner.go'
s = open(p).read()
before = s
s = s.replace('''	for _, e := range r.Entries {
		if !e.Skip && !e.OK {
			return ExitFailed
		}
	}''', '''	// MUTANT-63: failures are ignored
	_ = r.Failed''', 1)
if s == before:
    raise SystemExit('M63 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/engine/test_runner.go '// MUTANT-63'
run_gate "${RUNNER}" ./internal/engine/
run_gate "${APTAP}" ./internal/app/
restore
echo

echo "############################################################"
echo "# MUTATION 64: a secret-resolution failure yields exit 1, not 3"
echo "############################################################"
echo "# The wrong diagnosis. A user with an unconfigured keychain is told a test"
echo "# failed, and goes looking for a failing assertion instead of a missing"
echo "# credential."
python3 - <<'PY'
p = 'internal/engine/test_runner.go'
s = open(p).read()
before = s
s = s.replace('''	for _, e := range r.Entries {
		if !e.Skip && !e.OK && e.secret {
			return ExitSecret
		}
	}''', '''	// MUTANT-64: secret failures are treated as ordinary failures
	_ = ExitSecret''', 1)
if s == before:
    raise SystemExit('M64 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/engine/test_runner.go '// MUTANT-64'
run_gate "${RUNNER}" ./internal/engine/
restore
echo

echo "############################################################"
echo "# MUTATION 65: a parse failure yields exit 1, not 2"
echo "############################################################"
echo "# A malformed .http file is a defect in the COLLECTION, not a failing test."
echo "# Collapsing the two makes a typo look like a broken server."
python3 - <<'PY'
p = 'internal/engine/test_runner.go'
s = open(p).read()
before = s
s = s.replace('''	if r.parseFailed {
		return ExitUsage
	}''', '''	// MUTANT-65: a parse failure is an ordinary failure''', 1)
if s == before:
    raise SystemExit('M65 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/engine/test_runner.go '// MUTANT-65'
run_gate "${RUNNER}" ./internal/engine/
restore
echo

echo "############################################################"
echo "# MUTATION 66: an empty report (no test points) yields exit 0"
echo "############################################################"
echo "# A run that found nothing must not read as success. An empty plan is the"
echo "# quietest possible green."
python3 - <<'PY'
p = 'internal/engine/test_runner.go'
s = open(p).read()
before = s
s = s.replace('''	if len(r.Entries) == 0 {
		return ExitUsage
	}''', '''	// MUTANT-66: nothing happened, and nothing happened is a pass''', 1)
if s == before:
    raise SystemExit('M66 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/engine/test_runner.go '// MUTANT-66'
run_gate "${RUNNER}" ./internal/engine/
restore
echo

echo "############################################################"
echo "# MUTATION 67: the TAP plan is written from a counter, not the results"
echo "############################################################"
echo "# A plan that promises 40 tests and delivers 37 HANGS the consumer. This is"
echo "# the same boundary Move 9's M35 broke, on the other side of the codebase."
python3 - <<'PY'
p = 'internal/fuzz/tap.go'
s = open(p).read()
before = s
s = s.replace('''	if len(ordered) > 0 {
		b.WriteString("1.." + strconv.Itoa(len(ordered)) + "\\n")
	}''', '''	if len(ordered) > 0 {
		b.WriteString("1.." + strconv.Itoa(len(ordered)+7) + "\\n") // MUTANT-67
	}''', 1)
if s == before:
    raise SystemExit('M67 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/fuzz/tap.go '// MUTANT-67'
run_gate "${APTAP}" ./internal/app/
restore
echo

echo "############################################################"
echo "# MUTATION 68: assertions are never evaluated"
echo "############################################################"
echo "# The runner emits points but checks nothing, so every point passes."
python3 - <<'PY'
p = 'internal/engine/test_runner.go'
s = open(p).read()
before = s
s = s.replace('''		if verr := assertion.EvalBody(res, body); verr != nil {
			e.OK = false
			e.Detail = redact(cfg.Redactor, verr.Error())
		}''', '''		// MUTANT-68: the assertion is still evaluated, but its VERDICT is
		// discarded, so every point reports ok. Calling rather than deleting
		// the call is what makes this a behaviour change: dropping the call
		// leaves `assertion` unused, which is a compile error, and a compile
		// error proves nothing about a runner that stopped checking.
		_ = assertion.EvalBody(res, body)''', 1)
if s == before:
    raise SystemExit('M68 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/engine/test_runner.go '// MUTANT-68'
run_gate "${RUNNER}" ./internal/engine/
run_gate "${APTAP}" ./internal/app/
restore
echo

echo "############################################################"
echo "# MUTATION 65b: a parse failure is recorded as a PASSING test point"
echo "############################################################"
echo "# The other half of the parse-failure boundary: Run must not emit an ok"
echo "# entry for a file it could not read, and ExitCodeFor must not be able to"
echo "# report 0 for it."
python3 - <<'PY'
p = 'internal/engine/test_runner.go'
s = open(p).read()
before = s
s = s.replace("""			if errors.Is(res.err, ErrParseFailure) {
				report.parseFailed = true""",
              """			if errors.Is(res.err, ErrParseFailure) {
				// MUTANT-65b: recorded as a passing point instead
				report.Entries = append(report.Entries, Entry{File: "broken", OK: true})""", 1)
if s == before:
    raise SystemExit('M65b PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/engine/test_runner.go '// MUTANT-65b'
run_gate "${RUNNER}" ./internal/engine/
restore
echo

echo "############################################################"
echo "# RESTORED (code back to the pre-mutation implementation)"
echo "############################################################"
for f in ${FILES}; do
    if cmp -s "${BACKUP}/$(echo "${f}" | tr / _)" "${f}"; then
        echo "  ${f}: identical to the pre-mutation copy"
    else
        echo "  ${f}: DIFFERS FROM BACKUP"
    fi
done
run_gate "${VAULT}" ./internal/secrets/
run_gate "${RUNNER}" ./internal/engine/
run_gate "${APTAP}" ./internal/app/
