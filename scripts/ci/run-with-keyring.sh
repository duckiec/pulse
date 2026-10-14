#!/usr/bin/env bash
# Wave 6.5: isolated Secret Service session for the bare-metal keyring gate.
#
# Launches an ephemeral D-Bus session, starts an unlocked
# gnome-keyring-daemon secrets collection inside it, waits until
# org.freedesktop.Secret.Service answers on the bus, then execs "$@" there.
#
# Usage: ./scripts/ci/run-with-keyring.sh go test -v -tags=integration ./internal/secrets/...
#
# Offline, CGO-free, no network. The unlock password lives in a pipe and in
# daemon memory only; it is never written to disk.
set -euo pipefail

if [[ $# -eq 0 ]]; then
  echo "run-with-keyring.sh: no command given (usage: run-with-keyring.sh <cmd> [args...])" >&2
  exit 2
fi

for bin in dbus-run-session gnome-keyring-daemon dbus-send; do
  if ! command -v "$bin" >/dev/null 2>&1; then
    echo "run-with-keyring.sh: required binary $bin not in PATH" >&2
    exit 2
  fi
done

# Ephemeral unlock secret: random per invocation, pipe-only, never on disk.
UNLOCK="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
export GNOME_KEYRING_UNLOCK="$UNLOCK"

dbus-run-session -- bash -c '
  set -euo pipefail
  # Hermetic keyring dir: the daemon persists keyring FILES under
  # $XDG_DATA_HOME (or ~/.local/share), so a shared dir would resurrect
  # keyrings locked under earlier random passwords. A fresh temp dir makes
  # every run start with no keyrings; --unlock then CREATES login.keyring
  # under the piped secret and the default alias resolves to an unlocked
  # collection. Only XDG_DATA_HOME is overridden (never HOME itself) so
  # the Go build cache and module cache keep working. Torn down on exit.
  export KRING_XDG="$(mktemp -d)"
  export XDG_DATA_HOME="$KRING_XDG"
  trap "rm -rf $KRING_XDG" EXIT
  # Unlock (or create) the login collection with the piped secret.
  printf "%s" "$GNOME_KEYRING_UNLOCK" | gnome-keyring-daemon --unlock --components=secrets >/dev/null
  # Export the daemon runtime env (GNOME_KEYRING_CONTROL, SSH_AUTH_SOCK).
  eval "$(gnome-keyring-daemon --start --components=secrets)"
  export GNOME_KEYRING_UNLOCK
  # Poll until the Secret Service owns its well-known bus name (max ~10s).
  # NOTE: ListNames reports BUS NAMES (org.freedesktop.secrets), not the
  # INTERFACE name (org.freedesktop.Secret.Service) — grepping for the
  # interface here never matches even with a healthy daemon.
  for i in $(seq 1 100); do
    if dbus-send --session --dest=org.freedesktop.DBus --type=method_call --print-reply \
        /org/freedesktop/DBus org.freedesktop.DBus.ListNames 2>/dev/null \
        | grep -q org.freedesktop.secrets; then
      break
    fi
    if [[ "$i" == "100" ]]; then
      echo "run-with-keyring.sh: org.freedesktop.secrets never appeared on the session bus" >&2
      dbus-send --session --dest=org.freedesktop.DBus --type=method_call --print-reply \
        /org/freedesktop/DBus org.freedesktop.DBus.ListNames >&2 || true
      exit 1
    fi
    sleep 0.1
  done
  exec "$@"
' _ "$@"
