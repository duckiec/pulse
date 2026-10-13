#!/usr/bin/env bash
# Mutation proof for Move 9 (auto-fuzzing + TAP).
# Each mutation breaks a load-bearing behaviour; the gate must go red.
# Restore is from a cmp-verified file backup — never `git checkout`, because a
# failed restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

BACKUP="${TMPDIR:-/tmp}/pulse-mut-move9"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/fuzz/limiter.go internal/fuzz/tap.go internal/fuzz/openapi.go internal/fuzz/boundary.go internal/mcp/fuzz_tool.go internal/mcp/fuzz_engine.go"
for f in ${FILES}; do
    cp "${f}" "${BACKUP}/$(echo "${f}" | tr / _)"
done

restore() {
    local ok=0
    for f in ${FILES}; do
        cp "${BACKUP}/$(echo "${f}" | tr / _)" "${f}"
        if ! cmp -s "${BACKUP}/$(echo "${f}" | tr / _)" "${f}"; then
            echo "  RESTORE FAILED for ${f}"
            ok=1
        fi
    done
    return ${ok}
}
trap restore EXIT

mutate() { # mutate <file> <marker>
    if ! grep -qF "$2" "$1"; then
        echo "MUTATION NOT APPLIED: missing marker $2 in $1"
        restore
        exit 1
    fi
}

run_gate() { # run_gate <gate-regex> <package>
    CGO_ENABLED=0 go test -count=1 -timeout 600s -run "$1" "$2" 2>&1 | grep -E -- '--- FAIL|^FAIL|^ok' | head -8
}

LIM='TestRateLimiter|TestSemaphore|TestLimiter'
FUZ='TestFuzz|TestLoopback|TestParseFuzzArgs|TestRequireLoopback'
ENG='TestEngineRunner|TestRoundTrip|TestRendered|TestFuzzTransport|TestUndeliverable|TestDescribeUnencodable'
TAPG='TestTAP'
IRG='TestLoadOpenAPI|TestGenerate'

echo "############################################################"
echo "# BASELINE (unmutated code)"
echo "############################################################"
run_gate "${LIM}" ./internal/fuzz/
run_gate "${TAPG}|${IRG}" ./internal/fuzz/
run_gate "${FUZ}|${ENG}" ./internal/mcp/
echo

echo "############################################################"
echo "# MUTATION 32: the token bucket grants INFINITE tokens, so every"
echo "# waiter is admitted immediately and the rate limit stops existing"
echo "############################################################"
# The mutation keeps the code COMPILING and keeps the reservation, so the only
# thing that changes is the burst allowance: a burst of effectively infinite
# tokens means the limit is never reached, which is the exact shape of "the
# limiter was installed but does nothing".
python3 - <<'PY'
p = 'internal/fuzz/limiter.go'
s = open(p).read()
before = s
s = s.replace('\t\tburstDelta: time.Duration(float64(interval) * float64(burst-1)),',
              '\t\tburstDelta: time.Duration(1<<62) / time.Duration(burst), // MUTANT-32: effectively infinite tokens', 1)
if s == before:
    raise SystemExit('M32 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/fuzz/limiter.go '// MUTANT-32'
run_gate "${LIM}" ./internal/fuzz/
restore
echo

echo "############################################################"
echo "# MUTATION 33: the limiter RESERVES but does not wait, so every"
echo "# caller returns at once and the burst is unbounded"
echo "############################################################"
# This is the more interesting variant: the reservation bookkeeping survives, so
# the limiter's own observations still look correct, and only the WAIT is dropped.
# A gate that reads the limiter's self-report passes; only one that measures the
# world catches it.
sed -i 's|^\tif wait <= 0 {$|\tif true { // MUTANT-33\n\t\treturn nil\n\t}\n\tif wait <= 0 {|' internal/fuzz/limiter.go
mutate internal/fuzz/limiter.go '// MUTANT-33'
run_gate "${LIM}" ./internal/fuzz/
restore
echo

echo "############################################################"
echo "# MUTATION 34: TAP renders a 5xx as a pass, so a crash reads as a"
echo "# clean run and a CI job passes on a broken server"
echo "############################################################"
sed -i 's|^\t} else {\n\t\tsb.WriteString("not ok")|X|' internal/fuzz/tap.go
python3 - <<'PY'
p = 'internal/fuzz/tap.go'
s = open(p).read()
before = s
s = s.replace('''		} else {
			sb.WriteString("not ok")
		}''', '''		} else {
			sb.WriteString("ok") // MUTANT-34: a failure reads as a pass
		}''', 1)
if s == before:
    raise SystemExit('M34 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/fuzz/tap.go '// MUTANT-34'
run_gate "${TAPG}" ./internal/fuzz/
restore
echo

echo "############################################################"
echo "# MUTATION 35: the TAP plan count is written from a counter that"
echo "# disagrees with the results, which is what HANGS a TAP consumer"
echo "############################################################"
python3 - <<'PY'
p = 'internal/fuzz/tap.go'
s = open(p).read()
before = s
s = s.replace('''	if len(ordered) > 0 {
		b.WriteString("1.." + strconv.Itoa(len(ordered)) + "\\n")
	}''', '''	if len(ordered) > 0 {
		b.WriteString("1.." + strconv.Itoa(len(ordered)+7) + "\\n") // MUTANT-35
	}''', 1)
if s == before:
    raise SystemExit('M35 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/fuzz/tap.go '// MUTANT-35'
run_gate "${TAPG}" ./internal/fuzz/
restore
echo

echo "############################################################"
echo "# MUTATION 36: the loopback restriction is dropped, so a fuzz run"
echo "# can be aimed at any host on the network"
echo "############################################################"
python3 - <<'PY'
p = 'internal/mcp/fuzz_tool.go'
s = open(p).read()
before = s
s = s.replace('''	if err := RequireLoopback(out.Target); err != nil {
		return out, WrapProtocolError(CodeInvalidParams,
			"the fuzz target must be a loopback address", err)
	}''', '''	if false { // MUTANT-36
		if err := RequireLoopback(out.Target); err != nil {
			return out, WrapProtocolError(CodeInvalidParams,
				"the fuzz target must be a loopback address", err)
		}
	}''', 1)
if s == before:
    raise SystemExit('M36 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/mcp/fuzz_tool.go '// MUTANT-36'
run_gate "${FUZ}" ./internal/mcp/
restore
echo

echo "############################################################"
echo "# MUTATION 37: a ref that cannot be followed is skipped instead of"
echo "# failing the load, so the corpus silently omits that constraint"
echo "############################################################"
echo "# EXPECTED RESULT: GREEN — this mutation is a NO-OP, and the reason is"
echo "# structural rather than accidental."
echo "#"
echo "# There are EIGHT independent sites that turn an unresolvable ref into a"
echo "# fatal error (grep -c 'r.fail(' internal/fuzz/openapi.go). Removing one"
echo "# leaves seven, and the remote-ref fixture happens to route its broken"
echo "# ref through a parameter schema — which is enforced at the r.schema call"
echo "# site, not the params() deref site this mutation touches."
echo "#"
echo "# It is reported here rather than deleted because a mutation that goes"
echo "# green is a claim about the implementation worth making on the record: the"
echo "# refusal is defence in depth, not a single check someone remembered to"
echo "# write. M38 below attacks the ONE site that has no twin — the gate that"
echo "# turns a recorded failure into a returned error — and it is red."
python3 - <<'PY'
p = 'internal/fuzz/openapi.go'
s = open(p).read()
before = s
s = s.replace('''				r.fail(err)
				return
			}''', '''				_ = err // MUTANT-37: a broken ref is silently skipped
				continue
			}''', 1)
if s == before:
    raise SystemExit('M37 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/fuzz/openapi.go '// MUTANT-37'
run_gate "${IRG}" ./internal/fuzz/
restore
echo

echo "############################################################"
echo "# MUTATION 38: the recorded failure is never returned, so every refusal"
echo "# becomes a successful load of a schema Pulse could not fully read"
echo "############################################################"
python3 - <<'PY'
p = 'internal/fuzz/openapi.go'
s = open(p).read()
before = s
s = s.replace('''	if r.fatal != nil {
		return nil, r.fatal
	}''', '''	if false { // MUTANT-38: the refusal is recorded and then ignored
		return nil, r.fatal
	}''', 1)
if s == before:
    raise SystemExit('M38 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/fuzz/openapi.go '// MUTANT-38'
run_gate "${IRG}" ./internal/fuzz/
restore
echo

echo "############################################################"
echo "# MUTATION 39: the round-trip resolver is dropped, so the engine's"
echo "# expander consumes template-injection probes and the injection"
echo "# strategy reports itself as run while testing nothing"
echo "############################################################"
python3 - <<'PY'
p = 'internal/mcp/fuzz_engine.go'
s = open(p).read()
before = s
s = s.replace('''	opts = append(opts, engine.WithResolver(roundTripResolver{}))''',
              '''	// MUTANT-39: no resolver bound''', 1)
if s == before:
    raise SystemExit('M39 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/mcp/fuzz_engine.go '// MUTANT-39'
run_gate "${ENG}" ./internal/mcp/
restore
echo

echo "############################################################"
echo "# MUTATION 40: the transport honours HTTP_PROXY, so a target spelled"
echo "# 127.0.0.1 carries every payload off the machine through a proxy"
echo "############################################################"
python3 - <<'PY'
p = 'internal/mcp/fuzz_engine.go'
s = open(p).read()
before = s
s = s.replace('''		Proxy:                 nil,''',
              '''		Proxy: http.ProxyFromEnvironment, // MUTANT-40''', 1)
if s == before:
    raise SystemExit('M40 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/mcp/fuzz_engine.go '// MUTANT-40'
run_gate "${ENG}" ./internal/mcp/
restore
echo

echo "############################################################"
echo "# MUTATION 41: an undeliverable probe is reported as a server finding,"
echo "# so a probe that never left Pulse makes a correct server look broken"
echo "############################################################"
python3 - <<'PY'
p = 'internal/mcp/fuzz_tool.go'
s = open(p).read()
before = s
s = s.replace('''		if errors.Is(runErr, errProbeUndeliverable) {''',
              '''		if false && errors.Is(runErr, errProbeUndeliverable) { // MUTANT-41''', 1)
if s == before:
    raise SystemExit('M41 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/mcp/fuzz_tool.go '// MUTANT-41'
run_gate "${ENG}" ./internal/mcp/
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
run_gate "${LIM}" ./internal/fuzz/
run_gate "${TAPG}|${IRG}" ./internal/fuzz/
run_gate "${FUZ}|${ENG}" ./internal/mcp/