package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a collection, request, or run ID names nothing.
var ErrNotFound = errors.New("store: not found")

// ListCollections returns every collection ordered by id, so two calls over an
// unchanged database produce byte-identical listings.
func (s *Store) ListCollections(ctx context.Context) ([]Collection, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, root_path, created_at FROM collections ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list collections: %w", err)
	}
	defer rows.Close()

	var out []Collection
	for rows.Next() {
		var c Collection
		var created int64
		if err := rows.Scan(&c.ID, &c.RootPath, &created); err != nil {
			return nil, fmt.Errorf("store: scan collection: %w", err)
		}
		c.CreatedAt = time.UnixMilli(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListRequests returns the requests of one collection in ordinal order. An
// empty collectionID lists every request, ordered by collection then ordinal.
func (s *Store) ListRequests(ctx context.Context, collectionID string) ([]Request, error) {
	q := `SELECT id, collection_id, file_path, name, method, url_template, ordinal, updated_at
	      FROM requests WHERE collection_id = ? ORDER BY ordinal, id`
	args := []any{collectionID}
	if collectionID == "" {
		q = `SELECT id, collection_id, file_path, name, method, url_template, ordinal, updated_at
		      FROM requests ORDER BY collection_id, ordinal, id`
		args = nil
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list requests: %w", err)
	}
	defer rows.Close()
	return scanRequests(rows)
}

// GetRequest returns one request definition by id.
func (s *Store) GetRequest(ctx context.Context, id string) (Request, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, collection_id, file_path, name, method, url_template, ordinal, updated_at
		 FROM requests WHERE id = ?`, id)
	if err != nil {
		return Request{}, fmt.Errorf("store: get request %s: %w", id, err)
	}
	defer rows.Close()
	got, err := scanRequests(rows)
	if err != nil {
		return Request{}, err
	}
	if len(got) == 0 {
		return Request{}, fmt.Errorf("%w: request %q", ErrNotFound, id)
	}
	return got[0], nil
}

func scanRequests(rows *sql.Rows) ([]Request, error) {
	var out []Request
	for rows.Next() {
		var r Request
		var updated int64
		if err := rows.Scan(&r.ID, &r.CollectionID, &r.FilePath, &r.Name,
			&r.Method, &r.URLTemplate, &r.Ordinal, &updated); err != nil {
			return nil, fmt.Errorf("store: scan request: %w", err)
		}
		r.UpdatedAt = time.UnixMilli(updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRun returns one history entry by its row id.
func (s *Store) GetRun(ctx context.Context, id int64) (HistoryEntry, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, request_id, started_at, duration_ms, status_code, request_bytes,
		        response_bytes, sha256, error, run_source
		 FROM history WHERE id = ?`, id)
	if err != nil {
		return HistoryEntry{}, fmt.Errorf("store: get run %d: %w", id, err)
	}
	defer rows.Close()
	got, err := scanRuns(rows)
	if err != nil {
		return HistoryEntry{}, err
	}
	if len(got) == 0 {
		return HistoryEntry{}, fmt.Errorf("%w: run %d", ErrNotFound, id)
	}
	return got[0], nil
}

// RecentHistory returns runs newest-first across all requests. Offset pages
// through them; limit <= 0 selects the 50-row default, matching History.
func (s *Store) RecentHistory(ctx context.Context, limit, offset int) ([]HistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, request_id, started_at, duration_ms, status_code, request_bytes,
		        response_bytes, sha256, error, run_source
		 FROM history ORDER BY started_at DESC, id DESC LIMIT ? OFFSET ?`,
		limit, offset)
	if err != nil {
		return nil, fmt.Errorf("store: recent history: %w", err)
	}
	defer rows.Close()
	return scanRuns(rows)
}

func scanRuns(rows *sql.Rows) ([]HistoryEntry, error) {
	var out []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		var started, durationMS int64
		if err := rows.Scan(&e.ID, &e.RequestID, &started, &durationMS, &e.StatusCode,
			&e.RequestBytes, &e.ResponseBytes, &e.SHA256, &e.Error, &e.RunSource); err != nil {
			return nil, fmt.Errorf("store: scan history: %w", err)
		}
		e.StartedAt = time.UnixMilli(started)
		e.Duration = time.Duration(durationMS) * time.Millisecond
		out = append(out, e)
	}
	return out, rows.Err()
}
