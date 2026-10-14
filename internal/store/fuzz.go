package store

import (
	"context"
	"fmt"
	"time"

	"pulse/internal/fuzz"
)

// FuzzFindingCount counts persisted findings. The bare-metal crucible
// reads it through the production reader to prove a campaign's findings
// survived the process, not just the session.
func (s *Store) FuzzFindingCount(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fuzz_findings`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count fuzz findings: %w", err)
	}
	return n, nil
}

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
