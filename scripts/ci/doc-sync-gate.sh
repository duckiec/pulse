#!/usr/bin/env bash
# Documentation-sync gate (AGENTS.md §4.6 / MEMORY.md ADL-0047):
# public-facing changes under cmd/, internal/, docs/, or assets/ must land in
# the same commit as a public-document state update. A README.md change always
# satisfies that requirement; a tracked internal governance file change also
# satisfies it. If the required public documents are not present in the checkout,
# the gate does not hard-fail for their absence.
#
# Internal governance surface (AGENTS.md, PRD.md, ROADMAP.md, ARCHITECTURE.md,
# DESIGN_SYSTEM.md, TASKS.md, MEMORY.md) is local-only.
# ADL-0050: docs/*.md is sanctioned public documentation — a docs/ update counts
# as the documentation state update itself, so code+docs ships without a README
# touch and docs-only commits pass.
#
# Scope selection:
#   GITHUB_BASE_REF set (CI pull_request): origin/$GITHUB_BASE_REF...HEAD
#   else (local): worktree vs HEAD
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

if [[ -n "${GITHUB_BASE_REF:-}" ]]; then
    changed="$(git diff --name-only "origin/$GITHUB_BASE_REF...HEAD")"
else
    changed="$(git status --porcelain | awk '{print $2}')"
fi

code_dirs="$(printf '%s\n' "${changed}" | grep -E '^(cmd|internal)/' || true)"
public_dirs="$(printf '%s\n' "${changed}" | grep -E '^(docs|assets)/' || true)"
readme_changed="$(printf '%s\n' "${changed}" | grep -E '^README\.md$' || true)"
docs_changed="$(printf '%s\n' "${changed}" | grep -E '^docs/' || true)"

has_public_touch=false
if [ -n "${code_dirs}" ] || [ -n "${public_dirs}" ]; then
    has_public_touch=true
fi

# A tracked internal governance file changing alongside public work still
# counts as a documentation state update. When no governance files are tracked,
# the gate does not invent them as an out.
gov_tracked_any="$(git ls-files | grep -E '^(AGENTS|PRD|ROADMAP|ARCHITECTURE|DESIGN_SYSTEM|TASKS|MEMORY)\.md$' | head -1 || true)"
if [ -n "${gov_tracked_any}" ]; then
    internal_gov="$(printf '%s\n' "${changed}" | grep -E '^(AGENTS|PRD|ROADMAP|ARCHITECTURE|DESIGN_SYSTEM|TASKS|MEMORY)\.md$' || true)"
else
    internal_gov=""
fi

if [ "${has_public_touch}" = true ] && [ -z "${internal_gov}" ] && [ -z "${readme_changed}" ] && [ -z "${docs_changed}" ]; then
    offending="${code_dirs}"
    if [ -n "${public_dirs}" ]; then
        offending="${offending}
${public_dirs}"
    fi
    echo "doc-sync-gate: FAIL - public-facing change without documentation state update (AGENTS.md §4.6 / MEMORY.md ADL-0047):"
    printf '%s\n' "${offending}"
    exit 1
fi
echo "doc-sync-gate: PASS"
