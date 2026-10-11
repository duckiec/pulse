#!/usr/bin/env bash
# Final smoke verification: mode matrix behavior on the real binary.
set -euo pipefail
cd /home/duckie/pulse
bin=/tmp/pulse-bin
trap 'rm -f "$bin"' EXIT
go build -o "$bin" ./cmd/pulse

echo "== --version =="
out=$("$bin" --version 2>/dev/null) || true
printf 'stdout=%q\n' "$out"

echo "== mcp mode: stdout must be empty =="
stdout_file=$(mktemp)
"$bin" mcp 2>/dev/null >"$stdout_file" || true
n=$(wc -c <"$stdout_file")
echo "stdout_bytes=$n"
head -2 < <("$bin" mcp 2>&1 >/dev/null || true)
rm -f "$stdout_file"

echo "== bare invocation on a pipe: exit 2 =="
"$bin" </dev/null >/dev/null 2>/tmp/pulse-err.txt && code=0 || code=$?
echo "exit=$code"
head -2 /tmp/pulse-err.txt
rm -f /tmp/pulse-err.txt

echo "== mcp with stray arg: exit 2 =="
"$bin" mcp extra >/dev/null 2>/tmp/pulse-err.txt && code=0 || code=$?
echo "exit=$code"
head -1 /tmp/pulse-err.txt
rm -f /tmp/pulse-err.txt
