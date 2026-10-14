#!/usr/bin/env bash
# Mutation proof for Move 11 (the Wasm sandbox).
#
# The claim being tested is a security claim, so the mutations are the ways a
# sandbox silently stops being one: a bound that is configured but not enforced,
# a capability that is granted by omission, a bounds check that can be talked
# past. Each of those is invisible in a review of the config struct, which is
# exactly why they need gates.
#
# Restore is from a cmp-verified backup — never `git checkout`, because a failed
# restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

BACKUP="${TMPDIR:-/tmp}/pulse-mut-move11"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/wasm/runtime.go internal/wasm/memory.go internal/wasm/host.go internal/engine/engine.go"
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

run_xpkg() { # run_xpkg <regex> <pkg>: cross-package gates (M85 lives in engine/app, not wasm)
    CGO_ENABLED=0 go test -count=1 -timeout 300s -run "$1" "$2" 2>&1 |
        grep -E -- '--- FAIL|^FAIL|^ok' | head -10
}

run_gate() { # run_gate <regex>
    CGO_ENABLED=0 go test -count=1 -timeout 300s -run "$1" ./internal/wasm/ 2>&1 |
        grep -E -- '--- FAIL|^FAIL|^ok|^\s+runtime_test\.go|stack overflow|fatal error' | head -10
}

BOUND='TestInfiniteLoop|TestTimeBudget|TestASpentTimeout'
FUELG='TestCallBudget|TestAGuestUnderBudget|TestDeepRecursionTrap|TestFuelBudget'
MEMG='TestSignRoundTrip|TestTheHostCanSee|TestABIViolations|TestBoundsCheck|TestMemorySizeIsReadLive|TestMemoryLimitIsEnforced|TestEnvelopeCeiling|TestSpentModule'
ISOG='TestAGuestCannotReachTheHost|TestTheABIIsRequired|TestUnlimitedIsNotConfigurable'
HOSTG='TestNowNS|TestHeaderGet|TestLogGoes|TestLogTruncates|TestHostBounds|TestZeroTrustHostGate|TestHostFunctionIsRegistered'
XPKG='TestWasmSignInterceptionIsLive|TestSignerRunsPreDispatch'
ALL="TestSignRoundTrip|TestTheHostCanSee|TestInfiniteLoop|TestTimeBudget|TestCallBudget|TestAGuestUnderBudget|TestDeepRecursionTrap|TestFuelBudget|TestAGuestCannotReachTheHost|TestTheABIIsRequired|TestABIViolations|TestBoundsCheck|TestMemorySizeIsReadLive|TestMemoryLimitIsEnforced|TestUnlimitedIsNotConfigurable|TestEnvelopeCeiling|TestSpentModule|TestCloseIsIdempotent|TestASpentTimeout|TestDefaultLimits"

echo "############################################################"
echo "# BASELINE (unmutated sandbox)"
echo "############################################################"
run_gate "${ALL}"
echo

echo "############################################################"
echo "# MUTATION 50: the time budget is configured but NEVER APPLIED"
echo "# -- WithCloseOnContextDone is off, so a guest in (loop br 0)"
echo "# runs until the process is killed"
echo "############################################################"
echo "# EXPECTED: the gate does not report a clean PASS. It HANGS, because"
echo "# that is what an unbounded guest does, and the gate's own 5s backstop"
echo "# turns the hang into a named failure. That difference matters: a"
echo "# mutation that merely fails is weaker evidence than one that"
echo "# reproduces the actual symptom."
python3 - <<'PY'
import re
p = 'internal/wasm/runtime.go'
s = open(p).read()
before = s
# The replacement is a lambda, not a template: a \1 backreference written
# through a shell heredoc reaches Python still escaped, is written into the file
# as a raw 0x01 byte, and turns the mutation into a compile error that masks
# whatever the gate would have reported.
s = re.sub(r'WithCloseOnContextDone\(true\)\.(\s*)// tier 1: preemption',
           lambda mm: 'WithCloseOnContextDone(false).' + mm.group(1) + '// MUTANT-50',
           s, count=1)
if s == before:
    raise SystemExit('M50 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/runtime.go '// MUTANT-50'
run_gate "${BOUND}"
restore
echo

echo "############################################################"
echo "# MUTATION 51: the fuel listener is never attached, so the call"
echo "# budget is configured and never counted"
echo "############################################################"
python3 - <<'PY'
p = 'internal/wasm/runtime.go'
s = open(p).read()
before = s
s = s.replace('	base = experimental.WithFunctionListenerFactory(base, newFuelListener(spent, fuel, cancelBase))',
              '''	// MUTANT-51: the listener is never attached, so `fuel` is a number in a
	// struct and nothing counts anything.
	_ = experimental.WithFunctionListenerFactory''', 1)
if s == before:
    raise SystemExit('M51 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/runtime.go '// MUTANT-51'
run_gate "${FUELG}"
restore
echo

echo "############################################################"
echo "# MUTATION 52: the fuel budget is UNBOUNDED -- a caller asking"
echo "# for more than the ceiling gets it, so 'limited' means nothing"
echo "############################################################"
python3 - <<'PY'
p = 'internal/wasm/runtime.go'
s = open(p).read()
before = s
s = s.replace('''	if f > MaxFuel {
		return 0, fmt.Errorf("wasm: fuel %d exceeds the %d ceiling", f, MaxFuel)
	}''', '''	// MUTANT-52: no ceiling at all''', 1)
if s == before:
    raise SystemExit('M52 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/runtime.go '// MUTANT-52'
run_gate "${ISOG}"
restore
echo

echo "############################################################"
echo "# MUTATION 53: the resolved memory limit is DISCARDED, so a guest"
echo "# may allocate without bound inside the sandbox"
echo "#"
echo "# EXPECTED RESULT: red. This was GREEN on its first run, and the reason"
echo "# is a missing gate rather than a redundant one:"
echo "# TestDefaultLimitsAreTheDocumentedOnes checks the CONSTANT, and nothing"
echo "# checked that the constant is ever handed to the runtime. A ceiling that"
echo "# is documented and never enforced is the quietest failure a sandbox has"
echo "# -- the config looks right, the doc looks right, and a guest allocates"
echo "# until the process dies. The fix was the missing gate"
echo "# (TestMemoryLimitIsEnforced), not a different mutation."
echo "############################################################"
python3 - <<'PY'
import re
p = 'internal/wasm/runtime.go'
s = open(p).read()
before = s
# Whitespace-insensitive: gofmt aligns the trailing comments in this chain, so a
# pattern that pins them stops matching the next time anyone reformats the file.
# A mutation that silently fails to apply is worse than no mutation, because the
# log then claims a boundary was proven and nothing was.
s = re.sub(r'WithMemoryLimitPages\(pages\)\.(\s*)// linear memory ceiling',
           lambda mm: 'WithMemoryLimitPages(MaxMemoryPages).' + mm.group(1) + '// MUTANT-53',
           s, count=1)
# `pages` is now unused, and an unused local is a compile error that masks the
# boundary being tested.
s = s.replace('	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)',
              '	_ = pages // MUTANT-53: the resolved page count is discarded\n\trt := wazero.NewRuntimeWithConfig(ctx, rtCfg)', 1)
if s == before:
    raise SystemExit('M53 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/runtime.go '// MUTANT-53'
run_gate "${ALL}"
restore
echo

echo "############################################################"
echo "# MUTATION 54: WASI is linked, so the guest regains the clock,"
echo "# the filesystem and the filesystem's environment"
echo "############################################################"
echo "# NOTE: this is expressed as a direct wasi_snapshot_preview1"
echo "# instantiation, which is how a future refactor would 'helpfully'"
echo "# make TinyGo and Rust targets work. Pulse is offline-first"
echo "# (invariant I-3): a guest that can read the clock is already a"
echo "# determinism failure, and one that can read the environment can"
echo "# read a token out of it."
python3 - <<'PY'
p = 'internal/wasm/runtime.go'
s = open(p).read()
before = s
s = s.replace('''	mod, err := rt.InstantiateWithConfig(base, cfg.Wasm, modCfg)''',
'''	// MUTANT-54: WASI is linked into the sandbox
	if _, wErr := wasi_snapshot_preview1.Instantiate(ctx, rt); wErr != nil {
		_ = rt.Close(ctx)
		return nil, wErr
	}

	mod, err := rt.InstantiateWithConfig(base, cfg.Wasm, modCfg)''', 1)
s = s.replace('"github.com/tetratelabs/wazero/experimental"',
              '"github.com/tetratelabs/wazero/experimental"\n\t"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"', 1)
if s == before:
    raise SystemExit('M54 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/runtime.go '// MUTANT-54'
run_gate "${ISOG}"
restore
echo

echo "############################################################"
echo "# MUTATION 55: the bounds check is turned into the form integer"
echo "# overflow defeats -- (ptr+size) <= memSize, which WRAPS at"
echo "# ptr = 2^32-1 and is trivially true there"
echo "############################################################"
echo "# EXPECTED RESULT: GREEN -- a structural no-op, and the reason is"
echo "# defence in depth rather than a weak gate. checkRange has TWO"
echo "# independent guards: `ptr > avail` and `size > avail-ptr`. The"
echo "# first catches ptr = 2^32-1 before the addition is evaluated at all,"
echo "# so the wrapping form is UNREACHABLE while it is present. M55b below"
echo "# removes both, which is the mutation that makes the gate bite."
python3 - <<'PY'
p = 'internal/wasm/memory.go'
s = open(p).read()
before = s
s = s.replace("""	if size > avail-ptr {
		return fmt.Errorf("%w: %d bytes at %d exceeds the %d-byte memory",
			ErrBounds, size, ptr, avail)
	}""", """	if ptr+size > avail { // MUTANT-55: wraps at ptr = 2^32-1
		return fmt.Errorf("%w: %d bytes at %d exceeds the %d-byte memory",
			ErrBounds, size, ptr, avail)
	}""", 1)
if s == before:
    raise SystemExit('M55 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/memory.go '// MUTANT-55'
run_gate "${MEMG}"
restore
echo

echo "############################################################"
echo "# MUTATION 55b: BOTH bounds guards removed -- the host performs no"
echo "# range checking of its own at all"
echo "############################################################"
python3 - <<'PY'
p = 'internal/wasm/memory.go'
s = open(p).read()
before = s
s = s.replace("""	if ptr > avail {
		return fmt.Errorf("%w: offset %d is past the %d-byte memory", ErrBounds, ptr, avail)
	}""", "	_ = avail // MUTANT-55b: the pointer guard is gone", 1)
s = s.replace("""	if size > avail-ptr {
		return fmt.Errorf("%w: %d bytes at %d exceeds the %d-byte memory",
			ErrBounds, size, ptr, avail)
	}""", "	// MUTANT-55b: the length guard is gone too", 1)
if s == before or s.count('MUTANT-55b') < 2:
    raise SystemExit('M55b PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/memory.go '// MUTANT-55b'
run_gate "${MEMG}"
restore
echo

echo "# MUTATION 56: the guest's reported output length is TRUSTED,"
echo "# so a malicious guest names a range the host then reads"
echo "############################################################"
python3 - <<'PY'
p = 'internal/wasm/memory.go'
s = open(p).read()
before = s
s = s.replace('''	if uint64(n) > capacity {
		return nil, fmt.Errorf("%w: sign_request claims %d output bytes for a %d-byte buffer",
			ErrABIViolation, n, capacity)
	}''', '''	// MUTANT-56: the reported length is trusted''', 1)
if s == before:
    raise SystemExit('M56 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/memory.go '// MUTANT-56'
run_gate "${MEMG}"
restore
echo

echo "############################################################"
echo "# MUTATION 57: allocate()'s answer is trusted"
echo "############################################################"
echo "# EXPECTED RESULT: GREEN -- another structural no-op, for the same"
echo "# reason as M55. The memory boundary has THREE independent layers:"
echo "# the check in allocate(), the check in write(), and wazero's own"
echo "# bounds check inside api.Memory.Write. Removing one leaves two, and a"
echo "# reviewer shown only 'M57: red' would conclude the allocate() check is"
echo "# load-bearing when it is the OUTERMOST of three."
python3 - <<'PY'
p = 'internal/wasm/memory.go'
s = open(p).read()
before = s
s = s.replace("""	if err := m.checkRange(ptr, size); err != nil {
		return 0, err
	}
	return ptr, nil""", """	// MUTANT-57: the allocator's answer is trusted
	return ptr, nil""", 1)
if s == before:
    raise SystemExit('M57 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/memory.go '// MUTANT-57'
run_gate "${MEMG}"
restore
echo

echo "############################################################"
echo "# MUTATION 57b: the allocate() check, the write() check AND wazero's"
echo "# own Write refusal are all discarded -- the host assumes, and the"
echo "# transport's answer is thrown away"
echo "############################################################"
python3 - <<'PY'
p = 'internal/wasm/memory.go'
s = open(p).read()
before = s
s = s.replace("""	if err := m.checkRange(ptr, size); err != nil {
		return 0, err
	}
	return ptr, nil""", "	return ptr, nil // MUTANT-57b", 1)
s = s.replace("""	if err := m.checkRange(ptr, uint32(len(b))); err != nil {
		return err
	}""", "	// MUTANT-57b: write() no longer checks either", 1)
# The THIRD layer is wazero's own api.Memory.Write, which bounds-checks
# internally and returns false. Discarding that result is what turns "the host
# checked" into "the host assumed" — and without this third replacement M57b
# came back GREEN while the script claimed the host-side stack was gone.
s = s.replace("""	if !m.mem.Write(ptr, b) {
		// Unreachable given checkRange, and checked anyway: a false here means
		// the guest shrank its memory between the check and the write, and
		// proceeding would either drop bytes or panic.
		return fmt.Errorf("%w: write of %d bytes at %d was refused", ErrBounds, len(b), ptr)
	}""", "	m.mem.Write(ptr, b) // MUTANT-57b: the transport's own refusal is discarded", 1)
if s == before or s.count('MUTANT-57b') < 3:
    raise SystemExit('M57b PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/memory.go '// MUTANT-57b'
run_gate "${MEMG}"
restore
echo

echo "# MUTATION 58: a spent module stays OPEN, so the budget is"
echo "# advisory: the first call is detected and every later call runs"
echo "############################################################"
python3 - <<'PY'
p = 'internal/wasm/runtime.go'
s = open(p).read()
before = s
s = s.replace('''	if errors.As(err, &exit) && ctx.Err() != nil {
		m.closed.Store(true)''', '''	if errors.As(err, &exit) && ctx.Err() != nil {
		// MUTANT-58: the module is never poisoned''', 1)
if s == before:
    raise SystemExit('M58 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/runtime.go '// MUTANT-58'
run_gate "${FUELG}"
restore
echo

echo "############################################################"
echo "# MUTATION 59: the memory ABI check is skipped, so a module that"
echo "# exports no memory is accepted and every later access is on nil"
echo "############################################################"
python3 - <<'PY'
p = 'internal/wasm/runtime.go'
s = open(p).read()
before = s
s = s.replace('''	mem := mod.ExportedMemory(ABIMemory)
	if mem == nil {''', '''	var mem = mod.ExportedMemory(ABIMemory)
	if false && mem == nil { // MUTANT-59''', 1)
if s == before:
    raise SystemExit('M59 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/runtime.go '// MUTANT-59'
run_gate "${ISOG}"
restore
echo

echo "############################################################"
echo "# MUTATION 85: the pre-dispatch signer is bypassed in the engine, so a"
echo "# request carrying @wasm-sign dispatches UNSIGNED"
echo "############################################################"
python3 - <<'PY'
p = 'internal/engine/engine.go'
s = open(p).read()
before = s
s = s.replace('if path := e.doc.WasmSign(req); path != "" {',
              'if path := e.doc.WasmSign(req); path != "" && false { // MUTANT-85', 1)
if s == before:
    raise SystemExit('M85 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/engine/engine.go '// MUTANT-85'
run_xpkg "${XPKG}" ./internal/engine/
run_xpkg "TestWasmSignInterceptionIsLive" ./internal/app/
restore
echo

echo "############################################################"
echo "# MUTATION 86: the host bounds check is additive-only, so ptr+size"
echo "# wraps past 2^32 and an out-of-bounds guest range reads as in-bounds"
echo "############################################################"
python3 - <<'PY'
p = 'internal/wasm/host.go'
s = open(p).read()
before = s
s = s.replace('''func inBounds(mem api.Memory, ptr, size uint32) bool {
	avail := mem.Size()
	if ptr > avail {
		return false
	}
	if size > avail-ptr {
		return false
	}
	return true
}''', '''func inBounds(mem api.Memory, ptr, size uint32) bool {
	avail := mem.Size()
	if ptr+size > avail { // MUTANT-86: wraps
		return false
	}
	return true
}''', 1)
if s == before:
    raise SystemExit('M86 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/host.go '// MUTANT-86'
run_gate "${HOSTG}"
restore
echo

echo "############################################################"
echo "# MUTATION 86b: only the SECOND guard goes additive; the first"
echo "# (ptr > avail) still stands. EXPECTED GREEN, recorded as a"
echo "# structural no-op like M55: the wrap case never reaches the broken"
echo "# line, which is the defense-in-depth argument for keeping both."
echo "############################################################"
python3 - <<'PY'
p = 'internal/wasm/host.go'
s = open(p).read()
before = s
s = s.replace('''	if size > avail-ptr {
		return false
	}''', '''	if ptr+size > avail { // MUTANT-86b: wraps, but unreachable while ptr > avail stands
		return false
	}''', 1)
if s == before:
    raise SystemExit('M86b PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/wasm/host.go '// MUTANT-86b'
run_gate "${HOSTG}"
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
run_gate "${ALL}"
