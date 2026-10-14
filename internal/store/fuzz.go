package store

import (
	"context"
	"fmt"
	"time"

	"pulse/internal/fuzz"
)

// PersistFuzzFinding stores one deduplicated fuzz finding. A duplicate signature
// is an idempotent no-op, not an error.
func (s *Store) PersistFuzzFinding(ctx context.Context, f fuzz.PersistedFuzzFinding) error {
	created := f.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO fuzz_findings (signature, op_id, status_code, payload, created_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(signature) DO NOTHING`,
		f.Signature, f.OpID, f.Status, f.Payload, created.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: persist fuzz finding %s: %w", f.Signature, err)
	}
	return nil
}
