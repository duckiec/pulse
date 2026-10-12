#!/usr/bin/env bash
# Mutation-rule proof for Move 5 (AGENTS.md §4.2): SSE + telemetry.
#
# Four boundaries are deliberately broken, one at a time:
#   M10 resume token — the manager wipes its Last-Event-ID on disconnect, so a
#       reconnect silently starts from scratch and the server cannot replay.
#   M11 idle policy — deadline renewal is removed, so a silent server wedges the
#       stream instead of surfacing ErrIdle and reconnecting.
#   M12 frame gate  — the coalescing ticker is bypassed, so the repaint budget
#       becomes one dispatch per event (the lockup this gate exists to prevent).
#   M13 telemetry   — phases stop being recorded, so the report reports zeros
#       dressed up as measurements.
# Code is restored after each (verified with cmp) and the suite must be GREEN.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=ci/env.sh
source "$(dirname "$0")/ci/env.sh"

SSE=internal/engine/sse.go
PARSER=internal/engine/sse_parser.go
TELE=internal/engine/telemetry.go
BACKUP="${TMPDIR:-/tmp}/pulse-move5-backup"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"
cp "${SSE}" "${PARSER}" "${TELE}" "${BACKUP}/"

restore() {
    cp "${BACKUP}/sse.go" "${SSE}"
    cp "${BACKUP}/sse_parser.go" "${PARSER}"
    cp "${BACKUP}/telemetry.go" "${TELE}"
    local pair src dst
    for pair in "sse.go:${SSE}" "sse_parser.go:${PARSER}" "telemetry.go:${TELE}"; do
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
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestReconnect|TestLastEventID|TestIdleTimeout|TestFrameAligned|TestTelemetryReport' ./internal/engine/ 2>&1 | tail -3
echo

echo "############################################################"
echo "# MUTATION 10: the resume token is wiped on disconnect, so a"
echo "# reconnect starts from scratch and cannot replay"
echo "############################################################"
mutate "${SSE}" 's|^\t\tif !sleepCtx(ctx, backoff(m.cfg, attempt)) {|\t\tm.setID("") // MUTANT-10\n\t\tif !sleepCtx(ctx, backoff(m.cfg, attempt)) {|' '// MUTANT-10'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestReconnectReplaysLastEventID|TestLastEventIDSurvivesParserRebuild' ./internal/engine/ 2>&1 | grep -E 'Last-Event-ID|resume token|never reconnected|--- FAIL|^FAIL|^ok' | head -8
restore
echo

echo "############################################################"
echo "# MUTATION 11: an idle timeout stops being retryable, so a"
echo "# silent server ends the stream instead of reconnecting"
echo "############################################################"
mutate "${SSE}" 's|^\tcase errors.Is(err, ErrIdle):$|\tcase errors.Is(err, ErrIdle): // MUTANT-11|' '// MUTANT-11'
mutate "${SSE}" 's|^\t\treturn true // silence is retryable: reconnect and replay$|\t\treturn false // MUTANT-11|' '// MUTANT-11'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestIdleTimeoutIsRetryable' ./internal/engine/ 2>&1 | grep -E 'did not trigger a reconnect|must not wedge|--- FAIL|^FAIL|^ok' | head -6
restore
echo

echo "############################################################"
echo "# MUTATION 12: an emit is forced after every read, so the"
echo "# 16ms frame budget is gone and each event dispatches alone"
echo "############################################################"
mutate "${SSE}" 's|^\t\t\tif parser.Heartbeats() > 0 {|\t\t\tif err := emit(false); err != nil { // MUTANT-12\n\t\t\t\treturn err\n\t\t\t}\n\t\t\tif parser.Heartbeats() > 0 {|' '// MUTANT-12'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestFrameAlignedDispatch' ./internal/engine/ 2>&1 | grep -E 'not frame-coalesced|no frames|--- FAIL|^FAIL|^ok' | head -6
restore
echo

echo "############################################################"
echo "# MUTATION 13: the request-write phase stops being recorded,"
echo "# so the report serves zeros dressed up as measurements"
echo "############################################################"
mutate "${TELE}" 's|^\ttr.WroteRequest = func(httptrace.WroteRequestInfo) {|\ttr.WroteRequest = func(httptrace.WroteRequestInfo) { // MUTANT-13\n\t\treturn|' '// MUTANT-13'
CGO_ENABLED=0 go test -count=1 -timeout 300s -run 'TestTelemetryReportHasRealTimings' ./internal/engine/ 2>&1 | grep -E 'want > 0|no trace points|--- FAIL|^FAIL|^ok' | head -6
restore
echo

echo "############################################################"
echo "# RESTORED (code back to the pre-mutation implementation)"
echo "############################################################"
restore
echo "internal/engine restored: identical to the pre-mutation copies"
CGO_ENABLED=0 go test -count=1 -timeout 400s ./internal/engine/ 2>&1 | tail -3