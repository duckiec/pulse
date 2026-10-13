#!/usr/bin/env bash
# Mutation proof for Move 10 (the composition root).
#
# The claim being tested is DI itself: that what the App hands the bridge is
# what actually reaches an agent's tool result. A wiring bug is invisible from
# inside any one package — every field is non-nil, every interface is satisfied
# and every subsystem is individually green. These mutations break the wiring
# and require the end-to-end gates to go red.
#
# Restore is from a cmp-verified file backup — never `git checkout`, because a
# failed restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

BACKUP="${TMPDIR:-/tmp}/pulse-mut-move10"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/app/app.go internal/app/mcp.go internal/mcp/server.go internal/mcp/redactor.go"
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
    CGO_ENABLED=0 go test -count=1 -timeout 900s -run "$1" "$2" 2>&1 |
        grep -E -- '--- FAIL|^FAIL|^ok|^\s+app_test\.go' | head -10
}

APP='TestCompositionRoot|TestApp|TestSecretExitCode'

echo "############################################################"
echo "# BASELINE (unmutated composition root)"
echo "############################################################"
run_gate "${APP}" ./internal/app/
echo

echo "############################################################"
echo "# MUTATION 42: the redaction chokepoint is not wired at all — neither"
echo "# the engine's nor the executor's — the exact defect Move 7 recorded as"
echo "# unfixable (TASKS P4.3.3)"
echo "#"
echo "# This took THREE attempts to state honestly, and the attempts are the"
echo "# finding. Redaction is enforced at TWO independent layers:"
echo "#"
echo "#   1. engine.WithRedactor  — scrubs in flight, before a byte reaches a"
echo "#      Sink, so nothing untrusted ever holds the value;"
echo "#   2. EngineExecutor.Redactor — scrubs the collected Outcome again, in"
echo "#      case anything reached the Sink by another route."
echo "#"
echo "# Removing EITHER ONE ALONE LEAVES THE GATE GREEN. That is not a weak"
echo "# gate; it is a real property of a real defence-in-depth design, and a"
echo "# reviewer who sees only 'M42: red' and not the two green attempts would"
echo "# conclude the outer layer is load-bearing when it is not. Disabling both"
echo "# is what makes the defect real, so that is what this mutation does."
python3 - <<'PY'
import re
p = 'internal/app/app.go'
s = open(p).read()
before = s
s = re.sub(r'engine\.WithRedactor\(a\.Redactor\),',
           'engine.WithRedactor(nil), // MUTANT-42a', s, count=1)
s = s.replace("""	a.Executor = &mcp.EngineExecutor{
		Redactor: a.Redactor,""",
              """	a.Executor = &mcp.EngineExecutor{
		Redactor: nil, // MUTANT-42b""", 1)
if s == before or 'MUTANT-42a' not in s or 'MUTANT-42b' not in s:
    raise SystemExit('M42 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/app/app.go '// MUTANT-42a'
mutate internal/app/app.go '// MUTANT-42b'
run_gate "${APP}" ./internal/app/
restore
echo

echo "############################################################"
echo "# MUTATION 43: the fuzz runner is never handed to the bridge, so"
echo "# execute_fuzz_run is advertised by nobody and cannot be called"
echo "############################################################"
python3 - <<'PY'
p = 'internal/app/mcp.go'
s = open(p).read()
before = s
s = s.replace('''		FuzzRunner: a.FuzzRunner,''', '''		FuzzRunner: nil, // MUTANT-43''', 1)
if s == before:
    raise SystemExit('M43 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/app/mcp.go '// MUTANT-43'
run_gate "${APP}" ./internal/app/
restore
echo

echo "############################################################"
echo "# MUTATION 44: the failure binder is not registered, so a 500"
echo "# returns a bare status with no source context attached"
echo "############################################################"
python3 - <<'PY'
p = 'internal/app/mcp.go'
s = open(p).read()
before = s
s = s.replace('''		Options:    []mcp.Option{mcp.WithFailureBinder(a.Binder)},''',
              '''		Options:    nil, // MUTANT-44''', 1)
if s == before:
    raise SystemExit('M44 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/app/mcp.go '// MUTANT-44'
run_gate "${APP}" ./internal/app/
restore
echo

echo "############################################################"
echo "# MUTATION 45: the RAG tables are never created, so retrieval"
echo "# errors and the 5xx explanation silently degrades to nothing"
echo "############################################################"
python3 - <<'PY'
p = 'internal/app/app.go'
s = open(p).read()
before = s
s = s.replace('''	if err := st.InitRAG(a.ctx); err != nil {
		return fmt.Errorf("app: initialise the RAG tables: %w", err)
	}''', '''	// MUTANT-45: tables never created''', 1)
if s == before:
    raise SystemExit('M45 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/app/app.go '// MUTANT-45'
run_gate "${APP}" ./internal/app/
restore
echo

echo "############################################################"
echo "# MUTATION 46: the retriever is wired with a nil SearchFn, which is"
echo "# the exact Move 8 state — the struct exists, the gates on it pass, and"
echo "# nothing is connected to a store, so every 5xx degrades silently"
echo "############################################################"
echo "#"
echo "# The obvious mutation here was to give the BINDER a different Redactor,"
echo "# and it came back GREEN — for a reason worth recording. The error text"
echo "# arrives inside Outcome.Body, which layers 1 and 2 of the chokepoint have"
echo "# already scrubbed by the time the binder sees it, so the binder's own"
echo "# scrubber has nothing left to do. That is defence in depth, not a"
echo "# missing test. This mutation targets the retriever instead, which is"
echo "# genuinely load-bearing: with no SearchFn, retrieval returns nothing and"
echo "# the 5xx explanation silently becomes an empty attachment."
python3 - <<'PY'
p = 'internal/app/app.go'
s = open(p).read()
before = s
s = s.replace("""		SearchFn: func(ctx context.Context, query string, vec []int8, limit int) ([]mcp.RagHit, error) {
			hits, err := a.Store.Search(ctx, query, vec, limit)""",
              """		SearchFn: func(ctx context.Context, query string, vec []int8, limit int) ([]mcp.RagHit, error) {
			if true { // MUTANT-46: not connected to a store
				return nil, nil
			}
			hits, err := a.Store.Search(ctx, query, vec, limit)""", 1)
if s == before:
    raise SystemExit('M46 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/app/app.go '// MUTANT-46'
run_gate "${APP}" ./internal/app/
restore
echo

echo "############################################################"
echo "# MUTATION 47: the App's context is never cancelled, so in-flight"
echo "# work is torn down under the resource release rather than"
echo "# unwinding through its own path"
echo "############################################################"
python3 - <<'PY'
p = 'internal/app/app.go'
s = open(p).read()
before = s
s = s.replace('''		if a.cancel != nil {
			a.cancel()
		}''', '''		// MUTANT-47: the context outlives Close''', 1)
if s == before:
    raise SystemExit('M47 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/app/app.go '// MUTANT-47'
run_gate "${APP}" ./internal/app/
restore
echo

echo "############################################################"
echo "# MUTATION 48: indexing runs with no redaction, so the user's"
echo "# own source text — including any committed token — is written"
echo "# to the database and embedded as a vector"
echo "############################################################"
python3 - <<'PY'
p = 'internal/app/mcp.go'
s = open(p).read()
before = s
s = s.replace('''		Redact:      a.Redactor.String,''',
              '''		Redact:      func(s string) string { return s }, // MUTANT-48: identity = no redaction''', 1)
if s == before:
    raise SystemExit('M48 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/app/mcp.go '// MUTANT-48'
run_gate "${APP}" ./internal/app/
restore
echo

echo "############################################################"
echo "# MUTATION 49: Close does not close the store, leaking the SQLite"
echo "# handle (and the WAL/shm files) for the life of the process"
echo "############################################################"
python3 - <<'PY'
p = 'internal/app/app.go'
s = open(p).read()
before = s
s = s.replace('''		if a.Store != nil {
			if err := a.Store.Close(); err != nil {
				a.closeErr = fmt.Errorf("app: close the store: %w", err)
			}
		}''', '''		// MUTANT-49: the handle is leaked''', 1)
if s == before:
    raise SystemExit('M49 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/app/app.go '// MUTANT-49'
run_gate "${APP}" ./internal/app/
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
run_gate "${APP}" ./internal/app/
