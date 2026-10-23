package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxMockReplayBytes bounds one cached replay body. It is larger than the
// 256 KiB the MCP collector and the test runner retain, so a body that reached
// this layer has been bounded already; this is the backstop that keeps a future
// caller from turning the cache into a file-copy destination.
const MaxMockReplayBytes = 1 << 20

// MockResponse is one replayable success: the latest 2xx a route produced.
//
// Header is the REDACTED response header set — the same copy engine.RunResult
// exposes, with sensitive values replaced before this type is constructed. It
// is JSON-encoded in the `meta` column as map[string][]string, and Replay is
// the redacted response text. Nothing else about the run is kept.
type MockResponse struct {
	// Method is the uppercase HTTP method.
	Method string
	// Route is the canonical path pattern shared by the .http file and the
	// incoming request, e.g. "/users/{*}".
	Route string
	// StatusCode is the original 2xx status.
	StatusCode int
	// Header holds the redacted response headers.
	Header map[string][]string
	// Replay is the response body, verbatim except for redaction.
	Replay string
	// RecordedAt is when the response arrived.
	RecordedAt time.Time
}

// ErrMockNotSuccess is returned when a caller tries to cache a non-2xx run.
// The replay server only ever promises successful traffic, so refusing at the
// write boundary is what makes that promise true rather than a convention.
var ErrMockNotSuccess = errors.New("store: the mock cache holds 2xx responses only")

// PutMockResponse inserts or replaces the cached response for one route.
//
// The cache is one row per (method, route): a replay server that accumulated
// every run would grow without bound, and "the latest success" is the documented
// meaning of --from-cache. Replacing rather than appending is therefore the
// whole retention policy.
func (s *Store) PutMockResponse(ctx context.Context, m MockResponse) error {
	method := strings.ToUpper(strings.TrimSpace(m.Method))
	route := strings.TrimSpace(m.Route)
	if method == "" || route == "" {
		return fmt.Errorf("store: put mock response: method and route are required")
	}
	if m.StatusCode < 200 || m.StatusCode > 299 {
		return fmt.Errorf("%w: %d %s %s", ErrMockNotSuccess, m.StatusCode, method, route)
	}
	if len(m.Replay) > MaxMockReplayBytes {
		return fmt.Errorf("store: put mock response for %s %s: %d bytes exceeds the %d-byte replay limit",
			method, route, len(m.Replay), MaxMockReplayBytes)
	}
	meta, err := json.Marshal(m.Header)
	if err != nil {
		return fmt.Errorf("store: encode mock headers for %s %s: %w", method, route, err)
	}
	recorded := m.RecordedAt
	if recorded.IsZero() {
		recorded = time.Now()
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO mock_responses (method, route, status_code, meta, replay, recorded_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(method, route) DO UPDATE SET
		   status_code=excluded.status_code, meta=excluded.meta,
		   replay=excluded.replay, recorded_at=excluded.recorded_at`,
		method, route, m.StatusCode, string(meta), m.Replay, recorded.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: put mock response for %s %s: %w", method, route, err)
	}
	return nil
}

// LatestMockResponse returns the cached response for method+route.
//
// A miss returns an error wrapping ErrNotFound, which is how the replay server
// distinguishes "not cached yet" from "the cache could not be read": only the
// former is a 404 with a hint, and collapsing them would hide corruption behind
// a message telling the user to run the request again.
func (s *Store) LatestMockResponse(ctx context.Context, method, route string) (MockResponse, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	route = strings.TrimSpace(route)
	if method == "" || route == "" {
		return MockResponse{}, fmt.Errorf("%w: mock response for an empty method or route", ErrNotFound)
	}
	var (
		m        MockResponse
		meta     string
		recorded int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT method, route, status_code, meta, replay, recorded_at
		 FROM mock_responses WHERE method = ? AND route = ?`, method, route).
		Scan(&m.Method, &m.Route, &m.StatusCode, &meta, &m.Replay, &recorded)
	if errors.Is(err, sql.ErrNoRows) {
		return MockResponse{}, fmt.Errorf("%w: mock response for %s %s", ErrNotFound, method, route)
	}
	if err != nil {
		return MockResponse{}, fmt.Errorf("store: read mock response for %s %s: %w", method, route, err)
	}
	if meta != "" && meta != "null" {
		if err := json.Unmarshal([]byte(meta), &m.Header); err != nil {
			return MockResponse{}, fmt.Errorf("store: decode mock headers for %s %s: %w", method, route, err)
		}
	}
	m.RecordedAt = time.UnixMilli(recorded)
	return m, nil
}
