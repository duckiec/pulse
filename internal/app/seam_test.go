package app_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"

	"pulse/internal/app"
	"pulse/internal/mcp"
)

// TestAppFuzzPersistsFindingsToStore is the M83 gate: the production fuzz path
// persists every 5xx finding to fuzz_findings. Removing the PersistFindings
// call leaves the table empty after a 500 run and fails this gate.
func TestAppFuzzPersistsFindingsToStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"kaboom"}`))
	}))
	defer srv.Close()

	a := newTestApp(t)
	s := startSession(t, a)
	s.callTool(mcp.ToolExecuteFuzz, map[string]any{
		"schema_path":     writeSpec(t),
		"target":          srv.URL,
		"op":              "POST /users",
		"seed":            11,
		"budget":          8,
		"rate_per_second": 500,
	})

	var n int
	if err := a.Store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM fuzz_findings`).Scan(&n); err != nil {
		t.Fatalf("count findings: %v", err)
	}
	t.Logf("fuzz_findings rows after a 500 run: %d", n)
	if n == 0 {
		t.Fatal("M83: the fuzz_findings table is empty after a 500 run; " +
			"PersistFindings is not wired into the production path")
	}
}

// TestAppWatcherReindexesOnEdit is the M84 gate: the production watcher
// lifecycle reindexes a modified file and reaps a deleted one. With the
// Watcher.Start hook disabled, the store keeps serving stale chunks and this
// gate fails.
func TestAppWatcherReindexesOnEdit(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	rel := "main.go"
	abs := filepath.Join(root, rel)
	if err := os.WriteFile(abs, []byte("package main\n\nfunc Foo() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Boot with a corpus root: newTestApp takes Config mutators.
	a2 := newTestApp(t, func(c *app.Config) { c.CorpusRoot = root })
	defer func() { _ = a2.Close() }()

	if err := a2.StartWatcherIfConfigured(ctx); err != nil {
		t.Fatalf("StartWatcherIfConfigured: %v", err)
	}
	if a2.Watcher == nil {
		t.Fatal("M84: no watcher started for a configured corpus; " +
			"the Start hook is not wired into boot")
	}
	// Determinism: halt background fsnotify delivery before driving
	// HandleEvent directly. The live loop would otherwise race this test's
	// own delete-then-upsert (loop reaps after our upsert → empty store →
	// flake under the full suite). The Start hook above is already proven
	// by the non-nil watcher; Close still drains via Stop+Wait idempotently.
	_ = a2.Watcher.Stop()
	a2.Watcher.Wait()

	// Modify and drive the production event path directly: deterministic,
	// no live-fsnotify race, same HandleEvent the loop calls.
	if err := os.WriteFile(abs, []byte("package main\n\nfunc Bar() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a2.Watcher.HandleEvent(ctx, fsnotify.Event{Name: abs, Op: fsnotify.Write}); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	hits, err := a2.Store.Search(ctx, "Bar", nil, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("M84: modified content not indexed; the store serves stale chunks")
	}

	// Delete and reap.
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	if err := a2.Watcher.HandleEvent(ctx, fsnotify.Event{Name: abs, Op: fsnotify.Remove}); err != nil {
		t.Fatalf("reap: %v", err)
	}
	stats, err := a2.Store.RagStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Chunks != 0 {
		t.Fatalf("M84: %d chunks remain after delete; the reaper did not run", stats.Chunks)
	}

	// Deterministic teardown: Close drains the watcher before the store.
	if err := a2.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if a2.Watcher != nil {
		t.Fatal("Close did not release the watcher")
	}
}
