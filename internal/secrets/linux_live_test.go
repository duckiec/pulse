//go:build linux && integration

package secrets

// Wave 6.5: the bare-metal integration gate for the Linux Secret Service
// boundary.
//
// Zero mocks, zero MemoryStore, zero fakes. This test speaks to a live
// gnome-keyring-daemon over a live D-Bus session bus and MUST run inside
// scripts/ci/run-with-keyring.sh, which provides both:
//
//	./scripts/ci/run-with-keyring.sh go test -tags=integration ./internal/secrets/...
//
// Outside that session (no session bus, no daemon) it fails closed with the
// wrapper's name in the message — never skipped into a false green.
//
// The gate walks the exact production path a `vault init` run walks:
// GenerateIdentity → NewStore().Store (both private halves) →
// NewBackend().Resolve (both halves back) → byte-for-byte equality →
// out-of-band proof via the independent secret-tool client that the daemon
// actually committed the items → delete → reads fail with ErrNotFound.
//
// Secret hygiene: key material is compared, never logged. Failure messages
// name accounts and lengths, never values.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// requireLiveSession fails closed unless a D-Bus session bus is present and
// the Pulse backend reports it usable. Every failure names the wrapper, so
// a bare `go test -tags=integration` without it diagnoses itself.
func requireLiveSession(t *testing.T) {
	t.Helper()
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		t.Fatal("no D-Bus session bus: run under scripts/ci/run-with-keyring.sh " +
			"(./scripts/ci/run-with-keyring.sh go test -tags=integration ./internal/secrets/...)")
	}
	if err := NewBackend().Available(); err != nil {
		t.Fatalf("secret service not usable: %v (run under scripts/ci/run-with-keyring.sh)", err)
	}
	if _, err := exec.LookPath("secret-tool"); err != nil {
		t.Fatal("secret-tool not in PATH: the out-of-band leg needs it (run under scripts/ci/run-with-keyring.sh)")
	}
}

// secretTool runs the independent libsecret client against the same daemon.
// It shares no code with the backend under test: agreement between the two
// proves daemon commitment, not self-consistency.
func secretTool(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("secret-tool", args...)
	// secret-tool store reads the ITEM secret from stdin; lookup/clear take
	// none. No stdin here: a lookup that blocks on input would hang the
	// gate, so stdin is a closed empty pipe, never the terminal.
	cmd.Stdin = bytes.NewReader(nil)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("secret-tool %v: %v", args, err)
	}
	return out
}

func TestLinuxSecretServiceLive(t *testing.T) {
	requireLiveSession(t)

	// A unique key id per run: the hermetic session starts empty, but
	// uniqueness keeps reruns and parallel daemons from colliding.
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("nonce: %v", err)
	}
	keyID := "live-" + hex.EncodeToString(nonce)
	xAcct := IdentityAccount("x25519", keyID)
	sAcct := IdentityAccount("ed25519", keyID)

	// 1. Generate a random device identity (crypto/rand, both halves).
	id, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("GenerateIdentity: %v", err)
	}

	// 2. Write both private halves through the PRODUCTION write path.
	store := NewStore()
	if err := store.Available(); err != nil {
		t.Fatalf("store unavailable inside a live session: %v", err)
	}
	if err := StoreIdentity(keyID, id, store); err != nil {
		t.Fatalf("StoreIdentity: %v", err)
	}

	// 3. Read both halves back through the PRODUCTION read path and prove
	// byte-for-byte identity. Lengths only in the log — never values.
	backend := NewBackend()
	loaded, err := LoadIdentity(keyID, backend)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if !bytes.Equal(loaded.X25519Priv, id.X25519Priv) {
		t.Errorf("X25519 half round trip: %d bytes back, want %d identical bytes",
			len(loaded.X25519Priv), len(id.X25519Priv))
	}
	if !bytes.Equal(loaded.Ed25519Seed, id.Ed25519Seed) {
		t.Errorf("Ed25519 half round trip: %d bytes back, want %d identical bytes",
			len(loaded.Ed25519Seed), len(id.Ed25519Seed))
	}

	// 4. Out-of-band proof: the independent secret-tool client looks up the
	// same attributes and must return the same bytes. This is the step a
	// self-consistent mock could never provide — it proves the daemon
	// committed the items to its collection.
	if got := secretTool(t, "lookup", "service", ServiceName, "account", xAcct); !bytes.Equal(got, id.X25519Priv) {
		t.Errorf("out-of-band X25519 lookup: %d bytes, want the %d stored bytes verbatim",
			len(got), len(id.X25519Priv))
	}
	if got := secretTool(t, "lookup", "service", ServiceName, "account", sAcct); !bytes.Equal(got, id.Ed25519Seed) {
		t.Errorf("out-of-band Ed25519 lookup: %d bytes, want the %d stored bytes verbatim",
			len(got), len(id.Ed25519Seed))
	}

	// 5. Delete through the independent client (proving the items are
	// addressable by their attributes from outside Pulse), then prove
	// subsequent production reads fail with ErrNotFound — not a generic
	// error, and never a resurrected value.
	secretTool(t, "clear", "service", ServiceName, "account", xAcct)
	secretTool(t, "clear", "service", ServiceName, "account", sAcct)
	if _, err := backend.Resolve(xAcct); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-delete X25519 read: want ErrNotFound, got %v", err)
	}
	if _, err := backend.Resolve(sAcct); !errors.Is(err, ErrNotFound) {
		t.Errorf("post-delete Ed25519 read: want ErrNotFound, got %v", err)
	}
	if _, err := LoadIdentity(keyID, backend); err == nil {
		t.Error("post-delete LoadIdentity succeeded: the deleted halves are still readable")
	}
}
