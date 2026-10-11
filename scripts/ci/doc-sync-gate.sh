#!/usr/bin/env bash
# Documentation-sync gate (AGENTS.md §4.6): code changes under cmd/ or
# internal/ must land in the same commit as documentation state changes
# (any of the seven Essential Vibe Coding Files). Exits 1 on violation.
#
# Scope selection:
#   GITHUB_BASE_REF set (CI pull_request): origin/$GITHUB_BASE_REF...HEAD
#   otherwise: staged + unstaged changes (pre-commit / local runs)
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

if [ -n "${GITHUB_BASE_REF:-}" ]; then
    changed="$(git diff --name-only "origin/${GITHUB_BASE_REF}...HEAD" || true)"
elif git rev-parse --verify HEAD >/dev/null 2>&1; then
    changed="$(git diff --cached --name-only; git diff --name-only)"
else
    changed="$(git status --porcelain | awk '{print $2}')"
fi

code="$(printf '%s\n' "${changed}" | grep -E '^(cmd|internal)/' || true)"
docs="$(printf '%s\n' "${changed}" | grep -E '^(AGENTS|PRD|ROADMAP|ARCHITECTURE|DESIGN_SYSTEM|TASKS|MEMORY)\.md$' || true)"

if [ -n "${code}" ] && [ -z "${docs}" ]; then
    echo "doc-sync-gate: FAIL - code changed without documentation state update (AGENTS.md §4.6):"
    printf '%s\n' "${code}"
    exit 1
fi
echo "doc-sync-gate: PASS"
