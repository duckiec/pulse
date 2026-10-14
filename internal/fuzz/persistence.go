package fuzz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// Progress is one live fuzz progress notification. The Bubble Tea interface can
// consume these on a channel to render Pass/Fail/Skip counts without blocking
// the runner.
type Progress struct {
	Index   int
	Total   int
	Passed  bool
	Failed  bool
	Skipped bool
	Status  int
}

// FindingStore is the persistence surface a fuzz run writes through. Keeping it
// an interface (rather than *store.Store) is what lets the runner be tested
// without booting SQLite, and it mirrors the rest of the package's design.
type FindingStore interface {
	PersistFuzzFinding(ctx context.Context, f PersistedFuzzFinding) error
}

// PersistedFuzzFinding is the on-disk shape of one deduplicated finding. The
// signature is the dedup key: sha256(status + canonical response prefix), so two
// runs that trigger the same defect collapse into one row.
type PersistedFuzzFinding struct {
	OpID      string
	Signature string
	Status    int
	Payload   string
	CreatedAt time.Time
}

// FindingSignature computes the dedup key. The status contributes first so a
// 500 on one status cannot masquerade as a pass, and the canonical response
// prefix contributes the bytes the server actually echoed back.
func FindingSignature(status int, responsePrefix []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d", status)
	h.Write(responsePrefix)
	return hex.EncodeToString(h.Sum(nil))
}

// PersistFindings writes the failing payloads of a run through st, deduplicating
// by signature. A failure from the store propagates; individual duplicate keys
// are not errors because the store is responsible for idempotency.
func PersistFindings(ctx context.Context, st FindingStore, opID string, findings []Result) error {
	if st == nil {
		return nil
	}
	for _, r := range findings {
		status := 0
		if v, ok := r.Diagnostic.Fields["status"]; ok {
			fmt.Sscan(v, &status)
		}
		rec := PersistedFuzzFinding{
			OpID:      opID,
			Signature: FindingSignature(status, []byte(r.Diagnostic.Fields["payload"])),
			Status:    status,
			Payload:   r.Diagnostic.Fields["payload"],
			CreatedAt: time.Now(),
		}
		if err := st.PersistFuzzFinding(ctx, rec); err != nil {
			return err
		}
	}
	return nil
}
