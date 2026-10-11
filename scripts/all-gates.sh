#!/usr/bin/env bash
# Run every enforcement gate. Local pre-push equivalent of CI.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
s="$(dirname "$0")/ci"

bash "$s/build-matrix.sh"
bash "$s/tidy-check.sh"
bash "$s/banned-import-audit.sh"
bash "$s/doc-sync-gate.sh"
bash "$s/test-gates.sh"
bash "$(git rev-parse --show-toplevel)/scripts/smoke-check.sh" >/dev/null

echo "ALL GATES: PASS"
