// Snapshot persistence: the CRDT's durable form.
//
// Gossip state is in-memory, but a peer that forgets everything on every
// restart re-fetches the world on every boot — and worse, a restart that
// drops tombstones resurrects deleted items (the M91 failure). The snapshot
// is the whole store in one canonical JSON document: items with fields,
// bodies, add tags AND tombstones, so a reloaded peer resumes exactly where
// it stopped.
//
// The document holds only items that passed the secret filter: MergeItem
// refuses dirty items before they enter state, so there is nothing dirty
// to persist. A snapshot that fails to parse is refused whole (fail
// closed) rather than half-loaded — a partial ledger that silently
// un-merges someone is worse than an empty one.
package sync

import (
	"encoding/json"
	"fmt"
	"sort"
)

// maxSnapshotBytes bounds a snapshot import. The document comes from the
// peer's own disk, but an unbounded unmarshal is how a corrupt file
// becomes unbounded memory.
const maxSnapshotBytes = 64 << 20

// IDs returns every known item id, sorted. Tombstoned ids are included:
// they are replicated state, and forgetting one resurrects the item.
func (s *Store) IDs() []string {
	ids := make([]string, 0, len(s.items))
	for id := range s.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ExportSnapshot renders the whole store — live items and tombstones —
// as one JSON document. Callers must not mutate the store concurrently;
// the gossip round loop calls this synchronously after each Round, which
// is the only goroutine that mutates state outside tests.
func (s *Store) ExportSnapshot() ([]byte, error) {
	items := make([]*Item, 0, len(s.items))
	for _, id := range s.IDs() {
		items = append(items, s.items[id])
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("sync: export snapshot: %w", err)
	}
	return raw, nil
}

// ImportSnapshot folds a snapshot document back into the store. Every
// item passes through MergeItem, so the secret filter, the empty-id
// refusal, and the clock advance all apply exactly as they do to a
// gossip batch — a snapshot is a batch from disk, nothing more.
func (s *Store) ImportSnapshot(raw []byte) error {
	if len(raw) == 0 {
		return nil // no snapshot yet: first boot, empty store
	}
	if len(raw) > maxSnapshotBytes {
		return fmt.Errorf("sync: snapshot is %d bytes, cap %d", len(raw), maxSnapshotBytes)
	}
	var items []*Item
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("sync: decode snapshot: %w", err)
	}
	for _, it := range items {
		s.MergeItem(it)
	}
	return nil
}
