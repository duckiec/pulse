#!/usr/bin/env bash
# Mutation-rule proof for Move 6 (AGENTS.md §4.2): gRPC + dynamic reflection.
#
# Five boundaries are deliberately broken, one at a time:
#   M14 dynamic type resolution — an int32 field given a JSON string is coerced
#        instead of rejected, so a type mismatch is silently accepted.
#   M15 reflection descriptor — the reflection client stops resolving oneof
#        members, so schema lookup silently fails on the server side.
#   M16 streaming encoder — the response direction buffers the whole message
#        before dispatching, breaking the bounded working-set invariant.
#   M17 descriptor cache — the LRU stops evicting, so the cache grows unbounded.
#   M18 method-shape guard — client-streaming methods stop being refused and are
#        attempted anyway.
# Code is restored after each (verified with cmp) and the suite must be GREEN.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=ci/env.sh
source "$(dirname "$0")/ci/env.sh"

REFLECT=internal/engine/reflect.go
WIRE=internal/engine/reflection_wire.go
JSON=internal/engine/jsonstream.go
ENGINE=internal/engine/grpc_engine.go

BACKUP="${TMPDIR:-/tmp}/pulse-move6-backup"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"
cp "${REFLECT}" "${WIRE}" "${JSON}" "${ENGINE}" "${BACKUP}/"

restore() {
    cp "${BACKUP}/reflect.go" "${REFLECT}"
    cp "${BACKUP}/reflection_wire.go" "${WIRE}"
    cp "${BACKUP}/jsonstream.go" "${JSON}"
    cp "${BACKUP}/grpc_engine.go" "${ENGINE}"
    local pair src dst
    for pair in "reflect.go:${REFLECT}" "reflection_wire.go:${WIRE}" \
                "jsonstream.go:${JSON}" "grpc_engine.go:${ENGINE}"; do
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

echo "############################################################"
echo "# BASELINE (unmutated code)"
echo "############################################################"
CGO_ENABLED=0 go test -count=1 -timeout 300s \
  -run 'TestReflectionGate|TestTypeMismatch|TestValidQuoted|TestUnknownField|TestJSONStream|TestReflectionDescriptor|TestClientStreaming|TestDescriptorCache' \
  ./internal/engine/ 2>&1 | tail -3
echo

echo "############################################################"
echo "# MUTATION 14: dynamic type resolution coerces instead of"
echo "# rejecting, so an int32 field accepts a JSON string"
echo "############################################################"
mutate "${REFLECT}" 's|^\t\t\tif isQuotable64(fd.Kind()) {$|\t\t\tif true { // MUTANT-14|' '// MUTANT-14'
CGO_ENABLED=0 go test -count=1 -timeout 300s \
  -run 'TestTypeMismatchFailsCleanly|TestTypeMismatchInNestedFieldReportsThePath|TestTypeMismatchInRepeatedFieldReportsTheIndex' \
  ./internal/engine/ 2>&1 | grep -E 'field .* is int32|want a \*FieldTypeError|was accepted|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# MUTATION 15: the hand-built reflection descriptor drops a"
echo "# oneof member, so the server cannot resolve the schema"
echo "############################################################"
mutate "${WIRE}" 's|^\t\t\t\t\toneofStrField("file_containing_symbol", "fileContainingSymbol", 4, \&reqOneof),$|\t\t\t\t\tstrField("file_containing_symbol", "fileContainingSymbol", 4), // MUTANT-15|' '// MUTANT-15'
CGO_ENABLED=0 go test -count=1 -timeout 300s \
  -run 'TestReflectionDescriptorMatchesTheWireProtocol|TestReflectionGateResolvesAndRendersJSON' \
  ./internal/engine/ 2>&1 | grep -E 'in-oneof|oneof has|run failed|refused reflection|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# MUTATION 16: the encoder stops flushing, so the response"
echo "# direction buffers the whole message instead of streaming it"
echo "############################################################"
mutate "${JSON}" 's|^\tif len(s.scratch) < chunkSize \|\| s.err != nil {$|\tif true \|\| len(s.scratch) < chunkSize \|\| s.err != nil { // MUTANT-16|' '// MUTANT-16'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestJSONStreamDoesNotMaterializeTheMessage' \
  ./internal/engine/ 2>&1 | grep -E 'buffering the message instead of streaming|working set peaked|--- FAIL|^FAIL|^ok' | head -6
restore
echo

echo "############################################################"
echo "# MUTATION 17: the descriptor cache stops evicting"
echo "############################################################"
mutate "${REFLECT}" 's|^\t\tif evicted, dropped := c.order.push(key); dropped {$|\t\tif evicted, dropped := c.order.push(key); dropped \&\& false { // MUTANT-17|' '// MUTANT-17'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestDescriptorCacheIsBoundedAndNamespaced' \
  ./internal/engine/ 2>&1 | grep -E 'cache holds|declared and not used|--- FAIL|^FAIL|^ok' | head -6
restore
echo

echo "############################################################"
echo "# MUTATION 18: client-streaming methods stop being refused"
echo "############################################################"
mutate "${ENGINE}" 's|^\tif md.IsStreamingClient() {$|\tif false { // MUTANT-18|' '// MUTANT-18'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestClientStreamingIsRejectedBeforeDialing' \
  ./internal/engine/ 2>&1 | grep -E 'must be refused|still sent to the server|--- FAIL|^FAIL|^ok' | head -6
restore
echo

echo "############################################################"
echo "# RESTORED (code back to the pre-mutation implementation)"
echo "############################################################"
restore
echo "internal/engine restored: identical to the pre-mutation copies"
CGO_ENABLED=0 go test -count=1 -timeout 400s ./internal/engine/ 2>&1 | tail -3