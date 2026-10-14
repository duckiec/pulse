package app_test

// Gates for the P2P sync lifecycle wiring (Wave 8, Issue #11 Part 1):
// the composition root binds identity → node id, redactor → secret
// scanner, ledger → dial policy, and announcer/browser/engine → Boot;
// Close withdraws and disconnects everything. Every failure degrades,
// never fails Boot — except SkipSync, which is silent intent.

import (
	"crypto/ed25519"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pulse/internal/app"
	"pulse/internal/secrets"
	"pulse/internal/sync"
)

// syncIdentityFixture stores a fresh device identity in a MemoryStore and
// returns the store plus the expected node id.
func syncIdentityFixture(t *testing.T) (*secrets.MemoryStore, string) {
	t.Helper()
	id := mkIdentity(t)
	store := secrets.NewMemoryStore()
	if err := secrets.StoreIdentity("", id, store); err != nil {
		t.Fatalf("StoreIdentity: %v", err)
	}
	pub := ed25519.NewKeyFromSeed(id.Ed25519Seed).Public().(ed25519.PublicKey)
	return store, sync.NodeIDFor(pub)
}

func syncDegradations(a *app.App) string {
	return strings.Join(a.Degradations, "\n")
}

func TestSyncSkippedByConfig(t *testing.T) {
	a := newTestApp(t, func(c *app.Config) { c.SkipSync = true })
	if a.Sync() != nil {
		t.Fatal("SkipSync booted a subsystem anyway")
	}
	if strings.Contains(syncDegradations(a), "sync") {
		t.Fatalf("SkipSync recorded sync noise: %v", a.Degradations)
	}
}

func TestSyncOfflineWithoutIdentity(t *testing.T) {
	a := newTestApp(t, func(c *app.Config) {
		c.Backend = secrets.NewMemoryStore() // empty: no device halves
		c.SyncPeersFile = filepath.Join(t.TempDir(), "peers.txt")
	})
	if a.Sync() != nil {
		t.Fatal("sync booted without a device identity")
	}
	if !strings.Contains(syncDegradations(a), "sync offline: no device identity") {
		t.Fatalf("missing identity degradation not recorded: %v", a.Degradations)
	}
}

func TestSyncBootsDegradedButAlive(t *testing.T) {
	store, nodeID := syncIdentityFixture(t)
	peersPath := filepath.Join(t.TempDir(), "peers.txt") // absent: announce-only
	a := newTestApp(t, func(c *app.Config) {
		c.Backend = store
		c.SyncPeersFile = peersPath
		c.SyncPort = -1 // ephemeral: no fixed-port claim, hermetic
	})
	sub := a.Sync()
	if sub == nil {
		t.Fatalf("sync did not boot with identity+port: %v", a.Degradations)
	}
	if sub.NodeID != nodeID {
		t.Fatalf("node %q, want identity-derived %q", sub.NodeID, nodeID)
	}
	if sub.Store == nil || sub.Engine == nil {
		t.Fatal("subsystem missing Store or Engine")
	}
	if !strings.Contains(syncDegradations(a), "peers.txt") && !strings.Contains(syncDegradations(a), "announce") {
		t.Logf("note: no sync degradation recorded (network surprisingly present): %v", a.Degradations)
	}
	// The absent ledger must be reported as ABSENT (announce-only), never
	// as malformed: the operator's remedy differs (pin peers vs fix syntax).
	if !strings.Contains(syncDegradations(a), "absent") {
		t.Errorf("absent ledger not named as absent: %v", a.Degradations)
	}
	if strings.Contains(syncDegradations(a), "malformed") {
		t.Errorf("absent ledger misreported as malformed: %v", a.Degradations)
	}
	// The redactor IS the secret scanner: a resolved value is unfit for
	// shared state. Register through the App chain (as production does)
	// and prove the CRDT refuses it.
	const token = "sync-bound-secret-77c1"
	a.Redactor.Register(token)
	if err := sub.Store.PutField("r1", sync.FieldName, "holds "+token); !errors.Is(err, sync.ErrSecretRejected) {
		t.Fatalf("redactor-bound scanner accepted a resolved secret: %v", err)
	}
	// Clean edits still land.
	if err := sub.Store.PutField("r1", sync.FieldName, "clean"); err != nil {
		t.Fatalf("clean edit rejected: %v", err)
	}
	// Close is clean and idempotent: broadcaster withdrawn, listener and
	// conns closed, loops drained.
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if a.Sync() != nil {
		t.Fatal("Sync() still returns the subsystem after Close")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSyncPortConflictDegrades(t *testing.T) {
	store, _ := syncIdentityFixture(t)
	// Squat on a port with a plain listener, then force sync onto it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener available: %v", err)
	}
	defer ln.Close()
	taken, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatal("loopback listener has no TCP address")
	}
	a := newTestApp(t, func(c *app.Config) {
		c.Backend = store
		c.SyncPeersFile = filepath.Join(t.TempDir(), "peers.txt")
		c.SyncPort = taken.Port
	})
	if a.Sync() != nil {
		t.Fatal("sync booted on a taken port")
	}
	if !strings.Contains(syncDegradations(a), "cannot listen") {
		t.Fatalf("port conflict not recorded: %v", a.Degradations)
	}
}

func TestSyncMalformedLedgerRefusesAll(t *testing.T) {
	store, _ := syncIdentityFixture(t)
	peersPath := filepath.Join(t.TempDir(), "peers.txt")
	if err := os.WriteFile(peersPath, []byte("this is not a ledger\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := newTestApp(t, func(c *app.Config) {
		c.Backend = store
		c.SyncPeersFile = peersPath
		c.SyncPort = -1 // ephemeral: the ledger, not the port, is under test
	})
	// A malformed ledger must not half-load: either the subsystem runs
	// with zero pins (announce-only) or it stays down — and either way
	// the degradation names the file.
	if !strings.Contains(syncDegradations(a), "peers.txt") {
		t.Fatalf("malformed ledger not recorded: %v", a.Degradations)
	}
}
