#!/usr/bin/env bash
set -uo pipefail
cd /home/duckie/pulse
rm -f zz-final.sh
cp /mnt/c/Users/steven/AppData/Local/Temp/opencode/commit-move6.txt /tmp/cmsg6.txt
echo "=== status before commit ==="
git status --short
echo "=== commit ==="
git add -A && git commit -F /tmp/cmsg6.txt 2>&1 | tail -5
echo "=== push ==="
GIT_TERMINAL_PROMPT=0 git push origin main 2>&1 | tail -3
echo "=== log ==="
git log --oneline -3
echo "=== tree clean? ==="
git status --short
echo "(clean)"