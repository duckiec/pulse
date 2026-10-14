#!/usr/bin/env bash
# G-secret tripwire (Issue #11 release bar): fail closed if a credential
# shape appears in the working tree or in git history.
#
# What it catches: PEM private-key blocks, live-looking Stripe/AWS/Slack/
# GitHub tokens, and `sk_live_` canaries with a real suffix attached.
#
# What it deliberately does NOT catch, and why that is not a hole:
#   * `*_test.go` fixtures are exempt from the PEM rule only. Checked-in
#     fakes there (e.g. the indexer's key.pem, the redactor's
#     AKIA-EXAMPLE-SECRET) are deliberate fixtures with no live
#     counterpart; every other rule still applies to test files, and the
#     e2e crucible proves at runtime that no live value reaches disk.
#   * The e2e detector prefix `"sk_live_pulse_e2e_"` (quoted, concatenated
#     with a runtime-random suffix) carries no value; the rule requires an
#     8+ character suffix, so the detector never trips its own wire.
#
# Exit 0: clean. Exit 1: a hit, with file:line quoted. Never skips silently.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

hits=0
hit() { printf 'G-secret HIT: %s\n' "$1"; hits=$((hits + 1)); }

# High-confidence shapes. Each is anchored to what makes a leak real:
# a suffix (a bare prefix is a detector, not a value), a 16-char AWS id,
# a PEM block header.
patterns=(
    'sk_live_[A-Za-z0-9]{8,}'
    'AKIA[0-9A-Z]{16}'
    'ghp_[A-Za-z0-9]{36}'
    'gho_[A-Za-z0-9]{36}'
    'github_pat_[A-Za-z0-9_]{22,}'
    'xox[bpas]-[A-Za-z0-9-]+'
)

echo "== G-secret: tracked working tree"
while IFS= read -r f; do
    for pat in "${patterns[@]}"; do
        while IFS= read -r line; do
            hit "${f}: ${line}"
        done < <(grep -nEo "${pat}[^ \"']*" "$f" 2>/dev/null || true)
    done
    # PEM rule skips test fixtures (see header).
    case "$f" in
        *_test.go) ;;
        *)
            while IFS= read -r line; do
                hit "${f}: ${line}"
            done < <(grep -nEo -- '-----BEGIN (RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----' "$f" 2>/dev/null || true)
            ;;
    esac
done < <(git ls-files)

echo "== G-secret: untracked-but-not-ignored files"
while IFS= read -r f; do
    for pat in "${patterns[@]}" '-----BEGIN (RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----'; do
        while IFS= read -r line; do
            hit "untracked ${f}: ${line}"
        done < <(grep -nEo "${pat}[^ \"']*" "$f" 2>/dev/null || true)
    done
done < <(git status --porcelain | awk '$1 == "??" {print $2}')

echo "== G-secret: git history (all refs)"
hist=$(mktemp)
trap 'rm -f "$hist"' EXIT
git log -p --all -- . ':!*_test.go' > "$hist" 2>/dev/null || true
for pat in "${patterns[@]}" '-----BEGIN (RSA |EC |DSA |OPENSSH )?PRIVATE KEY-----'; do
    while IFS= read -r line; do
        hit "history: ${line:0:120}"
    done < <(grep -nEo "^\+[^+].*(${pat})[^ \"']*" "$hist" 2>/dev/null | head -20 || true)
done

if [ "$hits" -ne 0 ]; then
    echo "G-secret: FAIL ($hits hits)"
    exit 1
fi
echo "G-secret: PASS (tree, untracked, and history clean)"
