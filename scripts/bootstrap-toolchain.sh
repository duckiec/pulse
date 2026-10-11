#!/usr/bin/env bash
# Provision the mandated Go toolchain (AGENTS.md §2.1: Go 1.27+, CGO-free builds).
#
# The development box shipped Go 1.26.8, which violates the project's hard baseline.
# This script installs the pinned upstream release into ~/sdk so that every build,
# test, and benchmark runs on the sanctioned toolchain. Idempotent.
set -euo pipefail

VERSION=go1.27.2
SDK_DIR="${HOME}/sdk/${VERSION}"

if [ -x "${SDK_DIR}/bin/go" ]; then
    echo "bootstrap-toolchain: ${VERSION} already installed at ${SDK_DIR}"
    exit 0
fi

case "$(uname -s)-$(uname -m)" in
    Linux-aarch64)  FILE="${VERSION}.linux-arm64.tar.gz" ;;
    Linux-x86_64)   FILE="${VERSION}.linux-amd64.tar.gz" ;;
    Darwin-arm64)   FILE="${VERSION}.darwin-arm64.tar.gz" ;;
    Darwin-x86_64)  FILE="${VERSION}.darwin-amd64.tar.gz" ;;
    *) echo "bootstrap-toolchain: unsupported platform $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

echo "bootstrap-toolchain: resolving checksum for ${FILE} from go.dev"
WANT="$(curl -fsSL 'https://go.dev/dl/?mode=json' \
    | grep -A6 "\"filename\": \"${FILE}\"" \
    | grep '"sha256"' | head -1 | cut -d'"' -f4)"
if [ -z "${WANT}" ]; then
    echo "bootstrap-toolchain: could not resolve checksum for ${FILE}" >&2
    exit 1
fi

TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT
echo "bootstrap-toolchain: downloading ${FILE}"
curl -fsSL -o "${TMP}/${FILE}" "https://go.dev/dl/${FILE}"

GOT="$(sha256sum "${TMP}/${FILE}" | cut -d' ' -f1)"
if [ "${GOT}" != "${WANT}" ]; then
    echo "bootstrap-toolchain: SHA256 MISMATCH (want ${WANT}, got ${GOT})" >&2
    exit 1
fi
echo "bootstrap-toolchain: SHA256 verified (${GOT})"

mkdir -p "${HOME}/sdk"
tar -C "${HOME}/sdk" -xzf "${TMP}/${FILE}"
mv "${HOME}/sdk/go" "${SDK_DIR}"
"${SDK_DIR}/bin/go" version
echo "bootstrap-toolchain: installed ${VERSION} at ${SDK_DIR}"
