# Shared CI/dev environment (sourced by every script in this directory).
# Prefer the pinned toolchain installed by scripts/bootstrap-toolchain.sh
# (AGENTS.md §2.1: Go 1.27+ strictly). In CI, setup-go provides the toolchain
# and no ~/sdk entry exists, so the existing PATH is used unchanged.
for d in "$HOME"/sdk/go*/bin; do
    [ -x "$d/go" ] && PATH="$d:$PATH"
done
export PATH
