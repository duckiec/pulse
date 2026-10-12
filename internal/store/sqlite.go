// Package store is Pulse's local history database.
//
// It is written on one assumption: **a secret must never reach this file.**
// Not in a column, not in a free-text error, not in a URL, not in a body. The
// schema below is the enforcement mechanism — it has no column that could hold
// a header value or an authentication body, and TestSchemaHasNoSecretColumns
// reads sqlite_master at runtime to prove it (AGENTS.md §3.4, ARCHITECTURE.md
// §6.3).
//
// The driver is modernc.org/sqlite: a pure-Go SQLite with no cgo, satisfying the
// CGO_ENABLED=0 mandate. mattn/go-sqlite3 is permanently banned (AGENTS.md §2.4).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// DriverName is the database/sql driver Pulse uses.
const DriverName = "sqlite"

// Store is a handle to the history database.
type Store struct {
	db   *sql.DB
	path string
}

// schema is applied at open. It mirrors ARCHITECTURE.md §8 exactly.
//
// Note what is absent: no header column, no auth column, no token column, no
// cookie column, no request/response BODY column. Bodies are not persisted by
// default; history keeps metadata and the response digest only.
const schema = `
CREATE TABLE IF NOT EXISTS collections (
    id          TEXT PRIMARY KEY,
    root_path   TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS requests (
    id            TEXT PRIMARY KEY,
    collection_id TEXT NOT NULL REFERENCES collections(id),
    file_path     TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    method        TEXT NOT NULL,
    url_template  TEXT NOT NULL,
    ordinal       INTEGER NOT NULL DEFAULT 0,
    updated_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_requests_collection ON requests(collection_id, ordinal);

CREATE TABLE IF NOT EXISTS history (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id     TEXT NOT NULL REFERENCES requests(id),
    started_at     INTEGER NOT NULL,
    duration_ms    INTEGER NOT NULL,
    status_code    INTEGER NOT NULL,
    request_bytes  INTEGER NOT NULL,
    response_bytes INTEGER NOT NULL,
    sha256         TEXT    NOT NULL DEFAULT '',
    error          TEXT    NOT NULL DEFAULT '',
    run_source     TEXT    NOT NULL DEFAULT 'tui'
);

CREATE INDEX IF NOT EXISTS idx_history_request ON history(request_id, started_at DESC);
`

// forbiddenColumnSubstrings are the fragments that must never appear in any
// column name: they are what a credential column would be called. The check
// runs against sqlite_master at open time, not just in a test, so a future
// migration cannot quietly widen the schema (ARCHITECTURE.md §6.3).
//
// Matched case-insensitively and with separators normalized, so `api_key`,
// `api-key` and `ApiKey` all trip the `apikey` rule.
var forbiddenColumnSubstrings = []string{
	"header", "auth", "token", "cookie", "password", "secret", "apikey", "credential", "bearer",
}

// Open opens (creating if needed) the Pulse database at path.
//
// Two pragmas are set before any schema is applied, both part of the zero-trust
// contract (MEMORY.md ADL-0017):
//
//	secure_delete=ON   deleted rows are overwritten with zeros instead of being
//	                   left as free cells recoverable from the raw file;
//	journal_mode=WAL   keeps the write path from duplicating data into a
//	                   rollback journal, and is the mode Pulse reads back.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := path + "?_pragma=secure_delete(1)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite tolerates one writer; more connections only add lock contention.
	db.SetMaxOpenConns(1)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	s := &Store{db: db, path: path}
	if err := s.applySchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.verifySecureDelete(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.AuditSchema(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) applySchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("store: apply schema: %w", err)
	}
	return nil
}

// verifySecureDelete confirms PRAGMA secure_delete is actually ON rather than
// trusting that the DSN was honoured.
func (s *Store) verifySecureDelete(ctx context.Context) error {
	var on int
	if err := s.db.QueryRowContext(ctx, "PRAGMA secure_delete").Scan(&on); err != nil {
		return fmt.Errorf("store: read secure_delete pragma: %w", err)
	}
	if on != 1 {
		return fmt.Errorf("store: secure_delete is OFF; deleted rows would survive in the raw file")
	}
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// DB exposes the handle for callers that need their own queries.
func (s *Store) DB() *sql.DB { return s.db }

// ---- schema audit -----------------------------------------------------------

// Column describes one column as sqlite_master reports it.
type Column struct {
	Table string
	Name  string
	Type  string
	SQL   string // the original CREATE TABLE text
}

// SchemaAuditError reports a column that could hold a credential.
type SchemaAuditError struct {
	Table  string
	Column string
}

func (e *SchemaAuditError) Error() string {
	return fmt.Sprintf("store: schema violation: table %q has credential-capable column %q", e.Table, e.Column)
}

// ForbiddenColumns returns the fragments that must never appear in a column name.
func ForbiddenColumns() []string {
	out := make([]string, len(forbiddenColumnSubstrings))
	copy(out, forbiddenColumnSubstrings)
	return out
}

// Columns reads the live schema out of sqlite_master. It parses the stored
// CREATE TABLE text rather than trusting the constant, so the audit sees exactly
// what a future migration would leave on disk.
func (s *Store) Columns(ctx context.Context) ([]Column, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, fmt.Errorf("store: read sqlite_master: %w", err)
	}
	defer rows.Close()

	var out []Column
	for rows.Next() {
		var name, ddl sql.NullString
		if err := rows.Scan(&name, &ddl); err != nil {
			return nil, fmt.Errorf("store: scan sqlite_master: %w", err)
		}
		for _, col := range parseColumns(ddl.String) {
			out = append(out, Column{Table: name.String, Name: col.name, Type: col.typ, SQL: ddl.String})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate sqlite_master: %w", err)
	}
	return out, nil
}

// AuditSchema returns a SchemaAuditError for the first column whose name
// contains a credential fragment. A nil result is the proof the store is
// safe to write.
func (s *Store) AuditSchema(ctx context.Context) error {
	cols, err := s.Columns(ctx)
	if err != nil {
		return err
	}
	for _, c := range cols {
		if ViolatesSecretPolicy(c.Name) {
			return &SchemaAuditError{Table: c.Table, Column: c.Name}
		}
	}
	return nil
}

// ViolatesSecretPolicy reports whether a column name is forbidden.
//
// Matching is done on a NORMALIZED name — lowercased with `_`, `-`, and `.`
// removed — so a credential column cannot slip through by spelling itself
// `api_key`, `api-key`, or `ApiKey` when the rule only knows `apikey`.
func ViolatesSecretPolicy(name string) bool {
	lower := strings.ToLower(name)
	normalized := strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', '.', ' ':
			return -1
		}
		return r
	}, lower)
	for _, bad := range forbiddenColumnSubstrings {
		if strings.Contains(lower, bad) || strings.Contains(normalized, bad) {
			return true
		}
	}
	return false
}

// columnSpec is one parsed column definition.
type columnSpec struct {
	name string
	typ  string
}

// parseColumns extracts column names from a CREATE TABLE statement. It handles
// the shapes this schema uses (and rejects nothing loudly) by scanning
// parenthesised, comma-separated definitions at depth one and skipping table
// constraints, which start with a keyword rather than a bare identifier.
func parseColumns(ddl string) []columnSpec {
	open := strings.IndexByte(ddl, '(')
	if open < 0 {
		return nil
	}
	depth := 0
	var parts []string
	start := open + 1
	for i := open; i < len(ddl); i++ {
		switch ddl[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				parts = append(parts, ddl[start:i])
				return parseColumnParts(parts)
			}
		case ',':
			if depth == 1 {
				parts = append(parts, ddl[start:i])
				start = i + 1
			}
		}
	}
	return nil
}

func parseColumnParts(parts []string) []columnSpec {
	var out []columnSpec
	for _, raw := range parts {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		upper := strings.ToUpper(p)
		if strings.HasPrefix(upper, "PRIMARY") ||
			strings.HasPrefix(upper, "FOREIGN") ||
			strings.HasPrefix(upper, "UNIQUE") ||
			strings.HasPrefix(upper, "CONSTRAINT") ||
			strings.HasPrefix(upper, "CHECK") {
			continue // table constraint, not a column
		}
		name := p
		typ := ""
		if sp := strings.IndexAny(p, " \t\n("); sp > 0 {
			name = p[:sp]
			typ = strings.TrimSpace(p[sp:])
		}
		out = append(out, columnSpec{name: strings.Trim(name, `"`), typ: typ})
	}
	return out
}

// ---- records ----------------------------------------------------------------

// Collection is a directory of .http files.
type Collection struct {
	ID        string
	RootPath  string
	CreatedAt time.Time
}

// UpsertCollection inserts or refreshes a collection row.
func (s *Store) UpsertCollection(ctx context.Context, c Collection) error {
	created := c.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO collections (id, root_path, created_at) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET root_path=excluded.root_path`,
		c.ID, c.RootPath, created.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: upsert collection %s: %w", c.ID, err)
	}
	return nil
}

// UpsertRequest inserts or refreshes a request row. urlTemplate is the RAW
// template including any {{var}} anchors: the store records what the file said,
// not what was sent, so no resolved secret can enter this column.
func (s *Store) UpsertRequest(ctx context.Context, r Request) error {
	updated := r.UpdatedAt
	if updated.IsZero() {
		updated = time.Now()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO requests (id, collection_id, file_path, name, method, url_template, ordinal, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   file_path=excluded.file_path, name=excluded.name, method=excluded.method,
		   url_template=excluded.url_template, ordinal=excluded.ordinal, updated_at=excluded.updated_at`,
		r.ID, r.CollectionID, r.FilePath, r.Name, r.Method, r.URLTemplate, r.Ordinal, updated.UnixMilli())
	if err != nil {
		return fmt.Errorf("store: upsert request %s: %w", r.ID, err)
	}
	return nil
}

// Request is the stored definition of a request.
type Request struct {
	ID           string
	CollectionID string
	FilePath     string
	Name         string
	Method       string
	URLTemplate  string
	Ordinal      int
	UpdatedAt    time.Time
}

// HistoryEntry is one executed request's metadata.
//
// There is no Body field, no Header field, and no RequestBytes value that
// carries content: request_bytes is a COUNT, like response_bytes. The digest is
// a hash, which is safe to keep and useless for reconstruction.
type HistoryEntry struct {
	RequestID     string
	StartedAt     time.Time
	Duration      time.Duration
	StatusCode    int
	RequestBytes  int64
	ResponseBytes int64
	SHA256        string
	Error         string
	RunSource     string
}

// ErrSecretInPayload is returned when a caller tries to persist text that still
// carries an unresolved secret anchor.
var ErrSecretInPayload = errors.New("store: refusing to persist a payload containing a secret anchor")

// InsertHistory appends one run.
//
// The scrubber is applied to the error text and the URL-bearing fields by the
// caller; here we refuse outright if an interpolation anchor survives, because a
// `{{var}}` in the error column means an unresolved secret is one bug away from
// a secret.
func (s *Store) InsertHistory(ctx context.Context, h HistoryEntry) error {
	if strings.Contains(h.Error, "{{") || strings.Contains(h.Error, "}}") {
		return fmt.Errorf("%w: error text still contains an interpolation anchor", ErrSecretInPayload)
	}
	src := h.RunSource
	if src == "" {
		src = "tui"
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO history (request_id, started_at, duration_ms, status_code,
		                      request_bytes, response_bytes, sha256, error, run_source)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.RequestID, h.StartedAt.UnixMilli(), h.Duration.Milliseconds(), h.StatusCode,
		h.RequestBytes, h.ResponseBytes, h.SHA256, h.Error, src)
	if err != nil {
		return fmt.Errorf("store: insert history for %s: %w", h.RequestID, err)
	}
	return nil
}

// History returns the most recent runs for a request, newest first.
func (s *Store) History(ctx context.Context, requestID string, limit int) ([]HistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT request_id, started_at, duration_ms, status_code, request_bytes,
		        response_bytes, sha256, error, run_source
		 FROM history WHERE request_id = ? ORDER BY started_at DESC LIMIT ?`,
		requestID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: query history: %w", err)
	}
	defer rows.Close()

	var out []HistoryEntry
	for rows.Next() {
		var e HistoryEntry
		var started int64
		if err := rows.Scan(&e.RequestID, &started, &e.Duration, &e.StatusCode,
			&e.RequestBytes, &e.ResponseBytes, &e.SHA256, &e.Error, &e.RunSource); err != nil {
			return nil, fmt.Errorf("store: scan history: %w", err)
		}
		e.StartedAt = time.UnixMilli(started)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SanitizeURL strips credentials and fragment data before a URL is written
// anywhere persistent. It is the store-side half of the redaction chokepoint.
func SanitizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		// An unparseable URL is not something to persist verbatim.
		return ""
	}
	u.User = nil
	u.Fragment = ""
	return u.String()
}
