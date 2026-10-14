#!/usr/bin/env bash
# Mutation proof for Move 14 (60-FPS TUI: frame governor + sink consumption).
#
# Claims under test:
#   M79: the update loop holds the frame budget. A 100 ms sleep in Update is
#        not a slowdown but a correctness defect (10k msgs/s behind a 100 ms
#        handler is an unbounded backlog), and the p99 gate must catch it.
#   M80: the sink consumes every chunk. Dropping every second chunk must turn
#        the exactness gate's line count from 4000 to 2000 — there is no fuzz
#        in the assertion to hide in.
#
# Restore is from a cmp-verified backup — never `git checkout`, because a
# failed restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
export PATH=/home/duckie/sdk/go1.27.2/bin:$PATH
export GOTOOLCHAIN=local
export CGO_ENABLED=0

BACKUP="${TMPDIR:-/tmp}/pulse-mut-move14"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/ui/app.go"
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

echo "############################################################"
echo "# BASELINE (unmutated): all UI gates green"
echo "############################################################"
go test -count=1 ./internal/ui/ 2>&1 | tail -1
echo

echo "############################################################"
echo "# M79: 100 ms sleep in Update (blocking render loop)"
echo "############################################################"
python3 - <<'EOF'
p = "internal/ui/app.go"
s = open(p).read()
old = "func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {"
assert old in s, "M79 marker missing"
s = s.replace(old, old + "\n\tif true { // M79 MUTANT: blocking render loop\n\t\ttime.Sleep(100 * time.Millisecond)\n\t}", 1)
open(p, "w").write(s)
print("M79 applied")
EOF
echo "--- red run (expect FAIL: p95 ~= 100ms >> 16ms budget) ---"
set +e
go test -count=1 -run 'TestGFrameUpdateAndViewHoldTheFrameBudget' ./internal/ui/ 2>&1 | grep -E -- '--- FAIL|^FAIL|^ok |app_test.go:' | head -6
set -e
echo "--- restoring ---"
restore && echo "restore: cmp clean"
echo "--- green run ---"
go test -count=1 -run 'TestGFrameUpdateAndViewHoldTheFrameBudget' ./internal/ui/ 2>&1 | tail -1
echo

echo "############################################################"
echo "# M80: sink drops every second chunk"
echo "############################################################"
python3 - <<'EOF'
p = "internal/ui/app.go"
s = open(p).read()
old = """func (s *uiSink) Chunk(c *engine.Chunk) {
	s.received++"""
assert old in s, "M80 marker missing"
s = s.replace(old, old + "\n\tif s.received%2 == 0 { // M80 MUTANT: drop every second chunk\n\t\tif c != nil {\n\t\t\tc.Recycle()\n\t\t\ts.recycled++\n\t\t}\n\t\treturn\n\t}", 1)
open(p, "w").write(s)
print("M80 applied")
EOF
echo "--- red run (expect FAIL: lines = 2000, want 4000) ---"
set +e
go test -count=1 -run 'TestGFrameStreamIsExactUnderFlood' ./internal/ui/ 2>&1 | grep -E -- '--- FAIL|^FAIL|^ok |app_test.go:' | head -6
set -e
echo "--- restoring ---"
restore && echo "restore: cmp clean"
echo "--- green run ---"
go test -count=1 ./internal/ui/ 2>&1 | tail -1
