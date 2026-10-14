package sync

import (
	"testing"
)

// TestSnapshotRoundTripPreservesLiveItemsAndTombstones pins the durable
// form: export → import into a FRESH store must reproduce the digest
// exactly, tombstones included. A snapshot that drops a tombstone
// resurrects the item on restart — the M91 split-brain in durable form.
func TestSnapshotRoundTripPreservesLiveItemsAndTombstones(t *testing.T) {
	a := NewStore(testNode(1), nil, allowAll)
	if err := a.PutField("r1", FieldName, "orders"); err != nil {
		t.Fatalf("PutField: %v", err)
	}
	if err := a.PutBody("r1", []byte("GET /v1/orders")); err != nil {
		t.Fatalf("PutBody: %v", err)
	}
	if err := a.PutField("r2", FieldName, "dead"); err != nil {
		t.Fatalf("PutField: %v", err)
	}
	a.Remove("r2")

	raw, err := a.ExportSnapshot()
	if err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}

	b := NewStore(testNode(2), nil, allowAll)
	if err := b.ImportSnapshot(raw); err != nil {
		t.Fatalf("ImportSnapshot: %v", err)
	}
	if got, want := b.DigestOf(), a.DigestOf(); !got.Equal(want) {
		t.Fatalf("restored digest %+v != original %+v", got, want)
	}
	if it := b.Get("r2"); it == nil || it.Alive() {
		t.Fatalf("tombstone for r2 did not survive the round trip: %+v", it)
	}
	if it := b.Get("r1"); it == nil || it.Fields[FieldName].Value != "orders" {
		t.Fatalf("live item r1 did not survive the round trip: %+v", it)
	}
}

// TestSnapshotImportIsFailClosed pins the refusal paths: empty input is
// a first boot (nil error, empty store), garbage is an error rather than
// a half-loaded store.
func TestSnapshotImportIsFailClosed(t *testing.T) {
	s := NewStore(testNode(3), nil, allowAll)
	if err := s.ImportSnapshot(nil); err != nil {
		t.Fatalf("empty snapshot must be a clean first boot: %v", err)
	}
	if len(s.IDs()) != 0 {
		t.Fatalf("empty import produced ids: %v", s.IDs())
	}
	if err := s.ImportSnapshot([]byte("this is not a snapshot")); err == nil {
		t.Fatal("garbage snapshot imported without error")
	}
	if len(s.IDs()) != 0 {
		t.Fatalf("refused import left residue: %v", s.IDs())
	}
}
