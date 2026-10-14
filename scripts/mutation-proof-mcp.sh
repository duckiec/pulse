#!/usr/bin/env bash
# Mutation-rule proof for Move 7 (AGENTS.md §4.2): the MCP bridge.
#
# Six boundaries are deliberately broken, one at a time:
#   M19 stderr redirection — the MCP diagnostic logger is pointed at STDOUT,
#        so a boot-time log line corrupts the JSON-RPC wire.
#   M20 schema validation — the schema stops rejecting unknown properties, so
#        an agent's typo is silently accepted.
#   M21 type coercion — coercion stops recording, so the agent cannot tell
#        what Pulse converted on its behalf.
#   M22 the redaction chokepoint — collection stops passing through the
#        redactor, so a secret reaches the model's context window.
#   M23 the response ceiling — collection stops bounding what accumulates,
#        so an unbounded body is pushed into one tool result.
#   M24 the error chain — the structured payload drops the cause, so every
#        schema failure becomes an unactionable summary.
# Code is restored after each (verified with cmp) and the suite must be GREEN.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=ci/env.sh
source "$(dirname "$0")/ci/env.sh"

STDIO=internal/mcp/stdio.go
SERVER=internal/mcp/server.go
TOOLS=internal/mcp/tools.go
VALIDATE=internal/mcp/validate.go
ERRORS=internal/mcp/errors.go
HISTORY=internal/mcp/history.go
ITERATIVE=internal/mcp/iterative.go

BACKUP="${TMPDIR:-/tmp}/pulse-move7-backup"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"
cp "${STDIO}" "${SERVER}" "${TOOLS}" "${VALIDATE}" "${ERRORS}" "${HISTORY}" "${ITERATIVE}" "${BACKUP}/"

restore() {
    cp "${BACKUP}/stdio.go" "${STDIO}"
    cp "${BACKUP}/server.go" "${SERVER}"
    cp "${BACKUP}/tools.go" "${TOOLS}"
    cp "${BACKUP}/validate.go" "${VALIDATE}"
    cp "${BACKUP}/errors.go" "${ERRORS}"
    cp "${BACKUP}/history.go" "${HISTORY}"
    cp "${BACKUP}/iterative.go" "${ITERATIVE}"
    local pair src dst
    for pair in "stdio.go:${STDIO}" "server.go:${SERVER}" "tools.go:${TOOLS}" \
                "validate.go:${VALIDATE}" "errors.go:${ERRORS}" \
                "history.go:${HISTORY}" "iterative.go:${ITERATIVE}"; do
        src="${pair%%:*}"; dst="${pair##*:}"
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

GATES='TestProtocolGate|TestInvalidArguments|TestParseFailure|TestCoercionIsRecorded|TestUnresolvedSecret|TestRedactionChokepoint|TestResponseTruncation|TestStdioLoop|TestIndexOutOfRange|TestNoTestReaches'

echo "############################################################"
echo "# BASELINE (unmutated code)"
echo "############################################################"
CGO_ENABLED=0 go test -count=1 -timeout 300s -run "${GATES}" ./internal/mcp/ 2>&1 | tail -3
echo

echo "############################################################"
echo "# MUTATION 19: MCP diagnostics are written to STDOUT, so a"
echo "# boot-time log line corrupts the JSON-RPC wire"
echo "############################################################"
# The mutation must disable Logf's redirection ENTIRELY — pointing the sink at
# os.Stdout is not enough, because a test that installs its own sink would
# overwrite the defect. Making Logf write straight to os.Stdout models the
# harder version of the bug: a log statement that bypasses the sink.
python3 - <<'PY'
p = 'internal/mcp/stdio.go'
s = open(p).read()
s = s.replace('''func Logf(format string, args ...any) {
	fmt.Fprintf(Stderr, "pulse: "+format+"\\n", args...)
}''', '''func Logf(format string, args ...any) { // MUTANT-19
	_, _ = fmt.Fprintf(os.Stdout, "pulse: "+format+"\\n", args...)
	return
	fmt.Fprintf(Stderr, "pulse: "+format+"\\n", args...)
}''')
open(p, 'w').write(s)
PY
if ! grep -qF '// MUTANT-19' "${STDIO}"; then
    echo "MUTATION NOT APPLIED: missing marker // MUTANT-19"; restore; exit 1
fi
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestMCPDiagnosticsNeverReachStdout' ./internal/mcp/ 2>&1 | grep -E 'corrupted the JSON-RPC wire|not JSON|could not have detected|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# MUTATION 20: the schema stops rejecting unknown"
echo "# properties, so an agent's typo is silently accepted"
echo "############################################################"
mutate "${VALIDATE}" 's|^  "additionalProperties": false$|  "additionalProperties": true // MUTANT-20|' '// MUTANT-20'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestInvalidArgumentsAreStructuredErrors/unknown_field' ./internal/mcp/ 2>&1 | grep -E 'sneaky|accepted|--- FAIL|^FAIL|^ok' | head -6
restore
echo

echo "############################################################"
echo "# MUTATION 21: coercion stops being recorded, so the agent"
echo "# cannot tell what Pulse converted on its behalf"
echo "############################################################"
# The whole append-on-coercion helper is what must go, not a filter in front of it.
python3 - <<'PY'
p = 'internal/mcp/validate.go'
s = open(p).read()
s = s.replace('''		*out = append(*out, coercion{
			Field: field, From: fmt.Sprintf("string %q", t), To: fmt.Sprintf("integer %d", n),
			Reason: "the client serialized an integer as a string",
		})''', '''		_ = n // MUTANT-21: the conversion happened but nothing recorded it''')
open(p, 'w').write(s)
PY
if ! grep -qF '// MUTANT-21' "${VALIDATE}"; then
    echo "MUTATION NOT APPLIED: missing marker // MUTANT-21"; restore; exit 1
fi
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestCoercionIsRecordedNotSilent' ./internal/mcp/ 2>&1 | grep -E 'coerced argument|warning|--- FAIL|^FAIL|^ok' | head -6
restore
echo

echo "############################################################"
echo "# MUTATION 22: the collection path stops passing everybyte"
echo "# through the redaction chokepoint"
echo "############################################################"
python3 - <<'PY'
p = 'internal/mcp/tools.go'
s = open(p).read()
s = s.replace('''	if r != nil {
		out.Body = r.String(out.Body)
		out.URL = r.String(out.URL)
		if out.Headers != nil {
			out.Headers = r.Headers(out.Headers)
		}
		if s.err != nil {
			s.err = r.Error(s.err)
		}
	}
	return out''', '''	if r != nil { // MUTANT-22: headers only, body and url untouched
		if out.Headers != nil {
			out.Headers = r.Headers(out.Headers)
		}
	}
	return out''')
open(p, 'w').write(s)
PY
if ! grep -qF '// MUTANT-22' "${TOOLS}"; then
    echo "MUTATION NOT APPLIED: missing marker // MUTANT-22"; restore; exit 1
fi
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestRedactionChokepointRunsOnResults' ./internal/mcp/ 2>&1 | grep -E 'secret survived|REDACTED|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# MUTATION 23: collection stops bounding what accumulates,"
echo "# so an unbounded body is pushed into one tool result"
echo "############################################################"
python3 - <<'PY'
p = 'internal/mcp/tools.go'
s = open(p).read()
s = s.replace('''	if len(s.buf) >= s.max {
		s.truncated = true
	} else {
		room := s.max - len(s.buf)
		if room < c.N {
			s.buf = append(s.buf, (*c.Buf)[:room]...)
			s.truncated = true
		} else {
			s.buf = append(s.buf, (*c.Buf)[:c.N]...)
		}
	}''', '''	// MUTANT-23: every byte is kept and the ceiling is never reported.
	s.buf = append(s.buf, (*c.Buf)[:c.N]...)''')
open(p, 'w').write(s)
PY
if ! grep -qF '// MUTANT-23' "${TOOLS}"; then
    echo "MUTATION NOT APPLIED: missing marker // MUTANT-23"; restore; exit 1
fi
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestResponseTruncationIsExplicit' ./internal/mcp/ 2>&1 | grep -E 'not marked truncated|not stated|over the|--- FAIL|^FAIL|^ok' | head -6
restore
echo

echo "############################################################"
echo "# MUTATION 24: the structured error payload drops the cause,"
echo "# so every failure becomes an unactionable summary"
echo "############################################################"
python3 - <<'PY'
p = 'internal/mcp/errors.go'
s = open(p).read()
s = s.replace('''	if e.cause != nil {
		if cause := e.cause.Error(); cause != "" && !strings.Contains(detail, cause) {
			detail = detail + ": " + cause
		}
	}''', '''	// MUTANT-24: the summary alone, with the cause discarded.
	_ = strings.TrimSpace''')
open(p, 'w').write(s)
PY
if ! grep -qF '// MUTANT-24' "${ERRORS}"; then
    echo "MUTATION NOT APPLIED: missing marker // MUTANT-24"; restore; exit 1
fi
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestParseFailureCarriesItsOwnCode' ./internal/mcp/ 2>&1 | grep -E 'line:col|mention|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# MUTATION 87: the get_run handler bypasses the history"
echo "# Redactor chokepoint, so a vault value in the error column"
echo "# reaches the MCP client unredacted"
echo "############################################################"
python3 - <<'PY'
p = 'internal/mcp/history.go'
s = open(p).read()
s = s.replace('''		rec = scrubRunRecord(red, rec)''', '''		_ = scrubRunRecord // MUTANT-87: history served raw''')
open(p, 'w').write(s)
PY
if ! grep -qF '// MUTANT-87' "${HISTORY}"; then
    echo "MUTATION NOT APPLIED: missing marker // MUTANT-87"; restore; exit 1
fi
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestSecretConfinementGateIsTheHistoryChokepoint' ./internal/mcp/ 2>&1 | grep -E 'leaked the raw|REDACTED|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# MUTATION 88: the fuzz_next cursor is hardcoded to 0, so"
echo "# every step dispatches probe 0 while the counter advances"
echo "############################################################"
# NOTE: the first M88 form (stepIndex frozen alongside the payload) was GREEN
# for a structural reason — the gate then asserted counter uniqueness, which
# the mutant preserved. The gate was strengthened with an index spy on what
# the TARGET receives; this form freezes the payload while the counter still
# advances, and the spy names the duplicate.
python3 - <<'PY'
p = 'internal/mcp/iterative.go'
s = open(p).read()
s = s.replace('''	p := s.payloads[s.cursor]
	stepIndex := s.cursor''', '''	p := s.payloads[0] // MUTANT-88: the cursor never advances
	stepIndex := s.cursor''')
open(p, 'w').write(s)
PY
if ! grep -qF '// MUTANT-88' "${ITERATIVE}"; then
    echo "MUTATION NOT APPLIED: missing marker // MUTANT-88"; restore; exit 1
fi
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestIterativeFuzzAdvancesWithoutDuplicatesOrStalls' ./internal/mcp/ 2>&1 | grep -E 'M88:|distinct payloads|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# RESTORED (code back to the pre-mutation implementation)"
echo "############################################################"
restore
echo "internal/mcp restored: identical to the pre-mutation copies"
CGO_ENABLED=0 go test -count=1 -timeout 400s ./internal/mcp/ 2>&1 | tail -3