package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SaveSyncSnapshot replaces the CRDT snapshot row. The write is an upsert
// of a single row: gossip persists after every round, and a read-modify
// cycle would only add contention on a writer SQLite already serializes.
func (s *Store) SaveSyncSnapshot(ctx context.Context, raw []byte) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sync_state (id, snapshot, updated_at) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET snapshot = excluded.snapshot, updated_at = excluded.updated_at`,
		raw, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("store: save sync snapshot: %w", err)
	}
	return nil
}

// LoadSyncSnapshot returns the stored snapshot, or nil when no peer has
// ever persisted one (first boot). A row holding zero bytes is also a
// first boot: the importer treats empty input as an empty store.
func (s *Store) LoadSyncSnapshot(ctx context.Context) ([]byte, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT snapshot FROM sync_state WHERE id = 1`).Scan(&raw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: load sync snapshot: %w", err)
	}
	return raw, nil
}
