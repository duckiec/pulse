#!/usr/bin/env bash
# Mutation proof for Move 8 (Local RAG & contextual error binding).
# Each mutation breaks a load-bearing behaviour; the gate must go red.
# Restore is from a cmp-verified file backup — never `git checkout`, because the
# RAG sources are untracked during development and a failed restore would leak a
# mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

BACKUP="${TMPDIR:-/tmp}/pulse-mut-move8"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/rag/embed.go internal/rag/chunk.go internal/rag/walker.go internal/rag/indexer.go internal/store/rag.go internal/mcp/error_binder.go"
for f in ${FILES}; do
    cp "${f}" "${BACKUP}/$(echo "${f}" | tr / _)"
done

restore() {
    local ok=0
    for f in ${FILES}; do
        cp "${BACKUP}/$(echo "${f}" | tr / _)" "${f}"
        if ! cmp -s "${BACKUP}/$(echo "${f}" | tr / _)" "${f}"; then
            echo "RESTORE FAILED for ${f}"
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
    CGO_ENABLED=0 go test -count=1 -timeout 600s -run "$1" "$2" 2>&1 | grep -E -- '--- FAIL|^FAIL|^ok|no tests to run' | head -6
}

BINDER='TestFiveHundredBinding|TestErrorTextExtraction|TestRetrievalFailure|TestDegradedRetrieval|TestBindingIsBounded|TestBindingRedacts|TestStoreRetriever|TestRenderBinding'
RAGT='TestGoChunks|TestPythonChunks|TestAstroChunks|TestOversize|TestEmbedding|TestCamelCase|TestUnavailable|TestMemoryCeiling|TestFileDescriptor|TestTwoIndexRuns|TestGitignore|TestDenyList|TestRedactionRuns|TestWalkStats|TestEmptyText|TestCosine|TestIndexIsLazy|TestNonGoFile|TestOversizeFile'
STORET='TestRAG|TestFTS|TestHybrid|TestReciprocal|TestLexicalOnly|TestReindex|TestStaleFTS|TestUpsert|TestCosineMatches|TestEmptyQuery|TestDeleteByFile|TestVectorized'

echo "############################################################"
echo "# BASELINE (unmutated code)"
echo "############################################################"
run_gate "${BINDER}" ./internal/mcp/
run_gate "${RAGT}"  ./internal/rag/
run_gate "${STORET}" ./internal/store/
echo

echo "############################################################"
echo "# MUTATION 25: the 5xx gate is widened to every status >= 500,"
echo "# so a 200 carries backend context the agent did not ask for"
echo "############################################################"
sed -i 's|^func bind5xx(status int) bool { return status >= 500 \&\& status <= 599 }$|func bind5xx(status int) bool { return status >= 500 } // MUTANT-25|' internal/mcp/error_binder.go
mutate internal/mcp/error_binder.go '// MUTANT-25'
run_gate 'TestFiveHundredBinding' ./internal/mcp/
restore
echo

echo "############################################################"
echo "# MUTATION 26: the 5xx gate is narrowed to 500 only, so a 503"
echo "# — the single most common deploy-time failure — gets no context"
echo "############################################################"
sed -i 's|^func bind5xx(status int) bool { return status >= 500 \&\& status <= 599 }$|func bind5xx(status int) bool { return status == 500 } // MUTANT-26|' internal/mcp/error_binder.go
mutate internal/mcp/error_binder.go '// MUTANT-26'
run_gate 'TestFiveHundredBinding' ./internal/mcp/
restore
echo

echo "############################################################"
echo "# MUTATION 27: the query is passed to FTS5 verbatim, so any error"
echo "# body containing FTS5 syntax is a query error, not an answer"
echo "############################################################"
sed -i 's|^\texpr := BuildFTSQuery(terms)$|\texpr := strings.Join(terms, " ") // MUTANT-27|' internal/store/rag.go
mutate internal/store/rag.go '// MUTANT-27'
run_gate 'TestFTSQueryCannotBeInjected|TestHybridRetrieval' ./internal/store/
restore
echo

echo "############################################################"
echo "# MUTATION 28: redaction is dropped from retrieved source, so a"
echo "# hardcoded credential in a fixture reaches the model context"
echo "############################################################"
sed -i 's|^\t\ttext := b.scrub(c.Text)$|\t\ttext := c.Text // MUTANT-28|' internal/mcp/error_binder.go
mutate internal/mcp/error_binder.go '// MUTANT-28'
run_gate 'TestBindingRedacts' ./internal/mcp/
restore
echo

echo "############################################################"
echo "# MUTATION 29: the walker drops the secret deny-list, so a"
echo "# .env file is opened and indexed like any other source"
echo "############################################################"
sed -i 's|^\tif deniedExact\[strings.ToLower(base)\] {$|\tif false \&\& deniedExact[strings.ToLower(base)] { // MUTANT-29|' internal/rag/walker.go
mutate internal/rag/walker.go '// MUTANT-29'
run_gate 'TestDenyList|TestWalkStatsAreHonest' ./internal/rag/
restore
echo

echo "############################################################"
echo "# MUTATION 30: the vector leg sorts by chunk id instead of by cosine, so"
echo "# a structurally unrelated chunk can outrank the exact match and the"
echo "# fused ranking silently degrades to alphabetical"
echo "############################################################"
# NOTE ON WHAT THIS MUTATION IS NOT, since two earlier attempts were no-ops.
#
# The obvious mutation is to change cosine's DIVISOR, and that is a NON-EVENT
# here: quantize rescales every vector to L2 length exactly 127, so |a||b| IS
# 127*127 for every pair and dividing by 127*127 is arithmetically identical.
# Clamping the divisor to 127*127 is a no-op for the same reason. Both attempts
# left the gate green, which is a property of the design rather than a weak gate.
#
# The reason is worth stating because it IS the design: Reciprocal Rank Fusion
# consumes RANKINGS, not scores. Narrowing the numeric band of every cosine
# changes the magnitude of the returned Score but not the ORDER fusion sees, so
# retrieval quality is untouched. Only an ordering change is a real behavioural
# break — which is what this mutation does.
python3 - <<'PY'
p = 'internal/store/rag.go'
s = open(p).read()
before = s
old = ('\tsort.SliceStable(all, func(i, j int) bool {\n'
       '\t\tif all[i].score != all[j].score {\n'
       '\t\t\treturn all[i].score > all[j].score\n'
       '\t\t}\n'
       '\t\treturn all[i].id < all[j].id\n'
       '\t})')
new = ('\tsort.SliceStable(all, func(i, j int) bool { // MUTANT-30\n'
       '\t\treturn all[i].id < all[j].id\n'
       '\t})')
s = s.replace(old, new, 1)
if s == before:
    raise SystemExit('M30 PATTERN DID NOT MATCH')
open(p, 'w').write(s)
PY
mutate internal/store/rag.go '// MUTANT-30'
run_gate 'TestVectorLegOrdersByCosine|TestHybridRetrieval|TestCosineMatches' ./internal/store/
restore
echo

echo "############################################################"
echo "# MUTATION 31: the consumer's right to stop is removed - the workers keep"
echo "# draining and the producer stops watching for the consumer's exit - so"
echo "# abandoning the generator walks the whole tree, while the descriptor gate,"
echo "# the cancel gate and the laziness gate all stay green"
echo "############################################################"
# HOW THIS BOUNDARY WAS FOUND, which is the part worth keeping.
#
# Six earlier mutations of this behaviour were applied and all six left the gate
# GREEN. Each was replaced rather than tuned away:
#
#   1-2. Removing the ENUMERATOR's ctx check, or the POOL LOOP's: each masked by
#        the other three. Defence in depth working.
#   3.   Removing the explicit `stop` signal alone: masked by the `done` case in
#        the send select.
#   4.   Removing `stop` AND the `done` case together: the abandoned walk still
#        saw 2.5% of the tree, because the consumer's exit closed `done`.
#   5.   Removing `stop`, `done`, and the per-file pre-send check: a 600-second
#        TIMEOUT. The walk channel filled with nobody draining it and the
#        enumerator blocked forever on a send. That is the genuine defect, but a
#        hang reported as a suite timeout is a useless signal — so the FIX went
#        into the code, not the gate: termination now cancels a derived context,
#        and the producer already selects on ctx.Done() for the caller's own
#        cancellation. One mechanism, no forgotten site, no reachable deadlock.
#   6.   Cancelling that derived context on abandon: still green, because `done`
#        (closed when the pool exits) independently tells the producer the
#        consumer is gone.
#
# Every earlier attempt was green for a CORRECT reason. This one removes the
# remaining mechanism in the only way that keeps liveness: the workers stop
# EXITING on abandon and keep draining, so the producer never blocks — it simply
# walks a tree nobody is listening to. That is precisely the cost the laziness
# contract exists to avoid, and it is also what a real regression looks like: a
# refactor removes the "return false ends the walk" path because the other
# signals happened to cover it in testing.
python3 - <<'PY'
p = 'internal/rag/indexer.go'
s = open(p).read()
orig = s

# (a) abandon no longer exits the worker pool; the pool keeps draining so the
# producer never blocks on a send.
old_a = ('\t\t\t\tif !yield(Chunk{}, &CorpusError{Path: wk.rel, Err: err}) {\n'
         '\t\t\t\t\treturn\n'
         '\t\t\t\t}\n'
         '\t\t\t\tcontinue')
new_a = ('\t\t\t\tif !yield(Chunk{}, &CorpusError{Path: wk.rel, Err: err}) {\n'
         '\t\t\t\t\tcontinue // MUTANT-31a: draining, not exiting\n'
         '\t\t\t\t}\n'
         '\t\t\t\tcontinue')
s = s.replace(old_a, new_a, 1)

old_a2 = ('\t\t\t\tif !yield(*c, nil) {\n'
          '\t\t\t\t\treturn\n'
          '\t\t\t\t}')
new_a2 = ('\t\t\t\tif !yield(*c, nil) {\n'
          '\t\t\t\t\tcontinue // MUTANT-31b: draining, not exiting\n'
          '\t\t\t\t}')
s = s.replace(old_a2, new_a2, 1)

# (b) the producer stops watching for the consumer's exit.
old_b = '\t\tcase <-done:\n\t\t\treturn fs.SkipAll'
new_b = '\t\tcase <-done: // MUTANT-31c\n\t\t\treturn fs.SkipAll'
s = s.replace(old_b, new_b, 1)

if s == orig:
    raise SystemExit('M31 PATTERN DID NOT MATCH')
for marker in ('// MUTANT-31a', '// MUTANT-31b', '// MUTANT-31c'):
    if marker not in s:
        raise SystemExit('M31 site did not apply: %s' % marker)
open(p, 'w').write(s)
PY
mutate internal/rag/indexer.go '// MUTANT-31a'
run_gate 'TestAbandoningTheGeneratorStopsTheWalk|TestIndexIsLazy|TestFileDescriptor' ./internal/rag/
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
run_gate "${BINDER}" ./internal/mcp/
run_gate "${RAGT}"  ./internal/rag/
run_gate "${STORET}" ./internal/store/