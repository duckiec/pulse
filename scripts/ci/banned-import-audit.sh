#!/usr/bin/env bash
# Banned-import audit: stdlib-first mandate (AGENTS.md §2.2/§4.1) plus MCP
# stdout hygiene (AGENTS.md §3.3). Static and offline via
# internal/devtools/importaudit.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
# shellcheck source=env.sh
source "$(dirname "$0")/env.sh"
go run ./internal/devtools/importaudit config/allowed-modules.txt
