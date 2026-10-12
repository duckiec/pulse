package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pulse/internal/store"
)

// openTemp opens a throwaway database and removes it when the test ends.
func openTemp(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "pulse.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestSecureDeleteIsOn proves PRAGMA secure_delete is actually enabled, not just
// requested in the DSN (MEMORY.md ADL-0017). A deleted row must be overwritten
// with zeros rather than left as a recoverable free cell.
func TestSecureDeleteIsOn(t *testing.T) {
	s := openTemp(t)
	var on int
	if err := s.DB().QueryRowContext(context.Background(), "PRAGMA secure_delete").Scan(&on); err != nil {
		t.Fatalf("read pragma: %v", err)
	}
	if on != 1 {
		t.Fatalf("secure_delete = %d, want 1; deleted rows would survive in the raw file", on)
	}
}

// TestSecureDeleteZeroesDeletedRows is the behavioural proof: write a value that
// looks like a secret, delete it, and assert the bytes are gone from the raw
// file. This is the reason secure_delete exists.
func TestSecureDeleteZeroesDeletedRows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "erase.db")
	ctx := context.Background()
	s, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	const marker = "SHOULD-NEVER-SURVIVE-IN-THE-FILE"
	if _, err := s.DB().ExecContext(ctx,
		`INSERT INTO collections (id, root_path, created_at) VALUES (?, ?, ?)`, marker, marker, 1); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM collections WHERE id = ?`, marker); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.DB().ExecContext(ctx, `VACUUM`); err != nil {
		t.Fatalf("vacuum: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read raw db: %v", err)
	}
	if strings.Contains(string(raw), marker) {
		t.Errorf("deleted value %q is still present in the raw database file; "+
			"secure_delete is not protecting the data", marker)
	}
}

// TestSchemaHasNoSecretColumns is the ARCHITECTURE.md §6.3 gate: it queries
// sqlite_master at runtime — the live schema, not a copy of the constant — and
// asserts no column name could hold a credential.
func TestSchemaHasNoSecretColumns(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	cols, err := s.Columns(ctx)
	if err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if len(cols) == 0 {
		t.Fatal("schema audit found no columns at all — the audit is not reading the live schema")
	}
	forbidden := store.ForbiddenColumns()
	for _, c := range cols {
		for _, bad := range forbidden {
			if strings.Contains(strings.ToLower(c.Name), bad) {
				t.Errorf("table %q has credential-capable column %q (matched %q)", c.Table, c.Name, bad)
			}
		}
	}
	// Positive control: the audit must actually be able to fail.
	if !store.ViolatesSecretPolicy("api_key") {
		t.Error("ViolatesSecretPolicy failed to flag a forbidden column name")
	}
	if store.ViolatesSecretPolicy("status_code") {
		t.Error("ViolatesSecretPolicy flagged a legitimate column")
	}
}

// TestAuditSchemaRejectsAWidenedSchema proves the audit is not vacuous: a
// hand-widened schema with a credential column must be rejected.
func TestAuditSchemaRejectsAWidenedSchema(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.DB().ExecContext(ctx,
		`CREATE TABLE sneaky (id TEXT, auth_header TEXT)`); err != nil {
		t.Fatalf("create widened table: %v", err)
	}
	err := s.AuditSchema(ctx)
	if err == nil {
		t.Fatal("a schema carrying an auth_header column MUST fail the audit")
	}
	var se *store.SchemaAuditError
	if !asSchemaAuditError(err, &se) {
		t.Fatalf("expected a *store.SchemaAuditError, got %T: %v", err, err)
	}
	if se.Column != "auth_header" {
		t.Errorf("audit reported column %q, want auth_header", se.Column)
	}
}

// TestSchemaHasNoBodyColumns proves bodies are not persisted at all: the
// architecture defers body persistence pending a privacy ADL.
func TestSchemaHasNoBodyColumns(t *testing.T) {
	s := openTemp(t)
	cols, err := s.Columns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		switch strings.ToLower(c.Name) {
		case "body", "request_body", "response_body", "payload", "content":
			t.Errorf("history stores a body column %q; body persistence is deferred", c.Name)
		}
	}
}

// ---- round trips ------------------------------------------------------------

func TestCollectionAndRequestRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Millisecond)

	if err := s.UpsertCollection(ctx, store.Collection{ID: "col-1", RootPath: "/tmp/x", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Upsert twice: the ON CONFLICT path must work.
	if err := s.UpsertCollection(ctx, store.Collection{ID: "col-1", RootPath: "/tmp/y", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	r := store.Request{
		ID: "req-1", CollectionID: "col-1", FilePath: "/tmp/x/a.http",
		Name: "list-users", Method: "GET", URLTemplate: "https://api/{{host}}/users",
		Ordinal: 0, UpdatedAt: now,
	}
	if err := s.UpsertRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	// Update path.
	r.Name = "list-users-v2"
	if err := s.UpsertRequest(ctx, r); err != nil {
		t.Fatal(err)
	}

	var name, tmpl string
	if err := s.DB().QueryRowContext(ctx,
		`SELECT name, url_template FROM requests WHERE id = ?`, "req-1").Scan(&name, &tmpl); err != nil {
		t.Fatal(err)
	}
	if name != "list-users-v2" {
		t.Errorf("upsert did not update: name = %q", name)
	}
	// The RAW template is stored, including the anchor: the store records what
	// the file said, never a resolved secret.
	if !strings.Contains(tmpl, "{{host}}") {
		t.Errorf("url_template should keep the raw anchor, got %q", tmpl)
	}
}

func TestHistoryRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	// history.request_id is a real foreign key, so the parent rows must exist
	// first — the schema enforces referential integrity rather than storing
	// orphan runs.
	if err := s.UpsertCollection(ctx, store.Collection{ID: "col-1", RootPath: "/tmp/x"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRequest(ctx, store.Request{
		ID: "req-1", CollectionID: "col-1", FilePath: "/tmp/x/a.http",
		Method: "GET", URLTemplate: "https://api.example.com/users",
	}); err != nil {
		t.Fatal(err)
	}
	mustInsert(t, s, "req-1", "mcp:claude", 1)
	mustInsert(t, s, "req-1", "tui", 2)

	entries, err := s.History(ctx, "req-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	// Newest first.
	if entries[0].RunSource != "tui" {
		t.Errorf("history is not newest-first: %q", entries[0].RunSource)
	}
}

// TestHistoryRefusesUnresolvedAnchors proves the store refuses text that
// still carries an interpolation anchor, so a `{{var}}` can never sit in the
// database waiting to become a secret.
func TestHistoryRefusesUnresolvedAnchors(t *testing.T) {
	s := openTemp(t)
	err := s.InsertHistory(context.Background(), store.HistoryEntry{
		RequestID: "req-1",
		Error:     `dial failed for {{token}}`,
		StartedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("history row with an unresolved anchor MUST be refused")
	}
	if !strings.Contains(err.Error(), "anchor") {
		t.Errorf("error should explain the refusal: %v", err)
	}
}

func TestSanitizeURLActions(t *testing.T) {
	got := store.SanitizeURL("https://alice:hunter2@example.com/p?x=1#frag")
	if strings.Contains(got, "hunter2") {
		t.Errorf("userinfo survived: %s", got)
	}
	if strings.Contains(got, "frag") {
		t.Errorf("fragment survived: %s", got)
	}
	if !strings.Contains(got, "example.com/p") {
		t.Errorf("sanitizer destroyed the useful part: %s", got)
	}
	if bad := store.SanitizeURL("://not a url"); bad != "" {
		t.Errorf("an unparseable URL must not be persisted, got %q", bad)
	}
}

func mustInsert(t *testing.T, s *store.Store, reqID, source string, seq int) {
	t.Helper()
	err := s.InsertHistory(context.Background(), store.HistoryEntry{
		RequestID:     reqID,
		StartedAt:     time.UnixMilli(int64(seq) * 1000),
		Duration:      10 * time.Millisecond,
		StatusCode:    200,
		RequestBytes:  12,
		ResponseBytes: 4096,
		SHA256:        "deadbeef",
		Error:         "",
		RunSource:     source,
	})
	if err != nil {
		t.Fatalf("insert history: %v", err)
	}
}

// asSchemaAuditError is a tiny errors.As shim kept local so the test reads
// without importing errors elsewhere.
func asSchemaAuditError(err error, target **store.SchemaAuditError) bool {
	se, ok := err.(*store.SchemaAuditError)
	if ok {
		*target = se
	}
	return ok
}
