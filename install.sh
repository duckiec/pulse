#!/bin/sh
# Pulse one-shot installer for Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/duckiec/pulse/main/install.sh | sh
#
# Flags (also accepted as PULSE_VERSION / PULSE_PREFIX env):
#   --version vX.Y.Z   install a pinned release (default: latest)
#   --prefix DIR       install directory (default: ~/.local/bin)
#
# What it does: detect OS/arch, resolve the release tag, download the
# matching static binary plus checksums.txt, verify SHA-256 fail-closed,
# install, strip the macOS quarantine attribute, then self-check
# (`pulse --version` + `pulse doctor` in an isolated temp HOME). It never
# touches your workspace: the next commands are printed, not run.
set -eu

REPO="duckiec/pulse"
VERSION="${PULSE_VERSION:-}"
PREFIX="${PULSE_PREFIX:-$HOME/.local/bin}"

usage() {
    echo "usage: install.sh [--version vX.Y.Z] [--prefix DIR]" >&2
    exit 2
}

while [ $# -gt 0 ]; do
    case "$1" in
        --version) VERSION="${2:?--version needs a value like v1.0.0}"; shift 2 ;;
        --version=*) VERSION="${1#--version=}"; shift ;;
        --prefix) PREFIX="${2:?--prefix needs a directory}"; shift 2 ;;
        --prefix=*) PREFIX="${1#--prefix=}"; shift ;;
        -h|--help) usage ;;
        *) echo "install.sh: unknown argument '$1'" >&2; usage ;;
    esac
done

die() { echo "install.sh: $*" >&2; exit 1; }

# --- platform ---------------------------------------------------------------
os="$(uname -s)"
case "$os" in
    Linux) goos="linux" ;;
    Darwin) goos="darwin" ;;
    MINGW*|MSYS*|CYGWIN*) die "native Windows needs install.ps1 (see README.md); Git Bash can still run this script" ;;
    *) die "unsupported OS '$os' (Linux and macOS only)" ;;
esac

arch="$(uname -m)"
case "$arch" in
    x86_64|amd64) goarch="amd64" ;;
    arm64|aarch64) goarch="arm64" ;;
    *) die "unsupported architecture '$arch' (amd64 and arm64 only)" ;;
esac

# --- downloader + checksum tool ----------------------------------------------
if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL --proto '=https' --tlsv1.2 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -qO "$2" "$1"; }
else
    die "need curl or wget to download the release"
fi

if command -v sha256sum >/dev/null 2>&1; then
    sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
    sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
    die "need sha256sum (Linux) or shasum (macOS) to verify the download"
fi

# --- resolve version ----------------------------------------------------------
if [ -z "$VERSION" ]; then
    api_tmp="$(mktemp)"
    fetch "https://api.github.com/repos/$REPO/releases/latest" "$api_tmp"
    VERSION="$(grep -o '"tag_name": *"[^"]*"' "$api_tmp" | head -1 | cut -d'"' -f4)"
    rm -f "$api_tmp"
    [ -n "$VERSION" ] || die "could not resolve the latest release tag"
fi
case "$VERSION" in
    v*) ;;
    *) die "--version must look like v1.2.3 (got '$VERSION')" ;;
esac

# --- download -----------------------------------------------------------------
asset="pulse-${goos}-${goarch}"
base="https://github.com/$REPO/releases/download/$VERSION"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT INT TERM
fetch "$base/$asset" "$work/$asset"
fetch "$base/checksums.txt" "$work/checksums.txt"

# --- verify, fail closed -------------------------------------------------------
want="$(awk -v a="$asset" '$2 == a {print $1}' "$work/checksums.txt")"
[ -n "$want" ] || die "checksums.txt names no $asset entry"
got="$(sha256_of "$work/$asset")"
[ "$want" = "$got" ] || die "checksum mismatch for $asset (want $want, got $got)"

# --- install -------------------------------------------------------------------
mkdir -p "$PREFIX"
cp "$work/$asset" "$PREFIX/pulse"
chmod +x "$PREFIX/pulse"

# Unsigned binary: Gatekeeper quarantines the first launch. Stripping the
# attribute is documented in README.md; Apple signing is deferred.
if [ "$goos" = "darwin" ]; then
    xattr -d com.apple.quarantine "$PREFIX/pulse" 2>/dev/null || true
fi

# --- self-check in an isolated HOME (never touches your config) -----------------
check_home="$(mktemp -d)"
PULSE_HOME="$check_home" PULSE_SKIP_SYNC=1 "$PREFIX/pulse" --version || die "installed binary failed --version"
PULSE_HOME="$check_home" PULSE_SKIP_SYNC=1 "$PREFIX/pulse" doctor >/dev/null 2>&1 || {
    # doctor exits non-zero only on real findings in an empty dir, which
    # means the binary is broken, not the workspace (there is none).
    rm -rf "$check_home"
    die "installed binary failed its doctor self-check"
}
rm -rf "$check_home"

echo "pulse $VERSION installed to $PREFIX/pulse"
case ":$PATH:" in
    *":$PREFIX:"*) ;;
    *) echo "add it to your PATH: export PATH=\"$PREFIX:\$PATH\"" ;;
esac
echo "next: cd your-api && pulse init && pulse vault set DEMO_TOKEN && pulse doctor"
