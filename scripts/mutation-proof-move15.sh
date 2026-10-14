#!/usr/bin/env bash
# Mutation proof for Move 15 (zero-trust Wasm host + RAG reaper).
#
# Claims under test:
#   M81: the host function returns a digest, never key material. Writing the
#        raw secret into guest memory must trip the zero-trust memory scan.
#   M82: the reaper deletes stale chunks. Skipping the delete must leave the
#        phantom row the Reaper Gate counts.
#
# Restore is from a cmp-verified backup — never `git checkout`, because a
# failed restore would leak a mutant into the next run.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
export PATH=/home/duckie/sdk/go1.27.2/bin:$PATH
export GOTOOLCHAIN=local
export CGO_ENABLED=0

BACKUP="${TMPDIR:-/tmp}/pulse-mut-move15"
rm -rf "${BACKUP}"
mkdir -p "${BACKUP}"

FILES="internal/wasm/host.go internal/rag/watcher.go"
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
echo "# BASELINE (unmutated): both gates green"
echo "############################################################"
go test -count=1 -run 'TestZeroTrustHostGate' ./internal/wasm/ 2>&1 | tail -1
go test -count=1 -run 'TestReaperGate' ./internal/rag/ 2>&1 | tail -1
echo

echo "############################################################"
echo "# M81: host returns the raw secret instead of the HMAC"
echo "############################################################"
python3 - <<'EOF'
p = "internal/wasm/host.go"
s = open(p).read()
old = "\tsig := h.Sum(nil)\n"
assert old in s, "M81 marker missing"
s = s.replace(old, "\tsig := h.Sum(nil)\n\t_ = sig\n\tsig = []byte(secret) // M81 MUTANT: raw secret to the guest\n", 1)
open(p, "w").write(s)
print("M81 applied")
EOF
echo "--- red run (expect FAIL: signature mismatch + ZERO-TRUST VIOLATION) ---"
set +e
go test -count=1 -run 'TestZeroTrustHostGate' ./internal/wasm/ 2>&1 | grep -E -- '--- FAIL|^FAIL|^ok |host_test.go:' | head -6
set -e
echo "--- restoring ---"
restore && echo "restore: cmp clean"
echo "--- green run ---"
go test -count=1 -run 'TestZeroTrustHostGate' ./internal/wasm/ 2>&1 | tail -1
echo

echo "############################################################"
echo "# M82: reaper skips deleting stale chunks on remove/rename"
echo "############################################################"
python3 - <<'EOF'
p = "internal/rag/watcher.go"
s = open(p).read()
old = """	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		_, err := w.Store.DeleteRAGChunksByFile(ctx, rel)
		return err
	}"""
assert old in s, "M82 marker missing"
s = s.replace(old, """	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		return nil // M82 MUTANT: stale chunks never reaped
	}""", 1)
open(p, "w").write(s)
print("M82 applied")
EOF
echo "--- red run (expect FAIL: 1 chunks remain) ---"
set +e
go test -count=1 -run 'TestReaperGate' ./internal/rag/ 2>&1 | grep -E -- '--- FAIL|^FAIL|^ok |watcher_test.go:' | head -6
set -e
echo "--- restoring ---"
restore && echo "restore: cmp clean"
echo "--- green run ---"
go test -count=1 -run 'TestReaperGate' ./internal/rag/ 2>&1 | tail -1
