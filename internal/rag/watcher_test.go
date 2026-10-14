package rag

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"

	"pulse/internal/store"
)

func openRAGStore(t *testing.T) *store.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pulse.db")
	s, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.InitRAG(context.Background()); err != nil {
		t.Fatalf("InitRAG: %v", err)
	}
	return s
}

func indexOneFile(t *testing.T, s *store.Store, rel, content string) {
	t.Helper()
	ctx := context.Background()
	emb := NewHashingEmbedder()
	src := []byte(content)
	chunks, err := ChunkFileForTest(rel, LangOf(rel), src)
	if err != nil {
		t.Fatalf("chunk: %v", err)
	}
	stored := make([]store.RagChunk, 0, len(chunks))
	for i := range chunks {
		c := &chunks[i]
		c.MakeID()
		v, sc, _ := emb.Embed(ctx, c.Symbol+" "+c.Path+"\n"+c.Text)
		c.Vec, c.Scale = v, sc
		stored = append(stored, store.RagChunk{
			ID: c.ID, Path: c.Path, Lang: c.Lang, StartLn: c.StartLn, EndLn: c.EndLn,
			Symbol: c.Symbol, Text: c.Text, Vec: c.Vec, Scale: c.Scale,
		})
	}
	if _, err := s.UpsertRAGChunks(ctx, stored); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

func TestReaperGate(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	rel := "main.go"
	abs := filepath.Join(root, rel)
	if err := os.WriteFile(abs, []byte("package main\n\nfunc Foo() int { return 42 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := openRAGStore(t)
	indexOneFile(t, s, rel, "package main\n\nfunc Foo() int { return 42 }\n")

	stats, err := s.RagStats(ctx)
	if err != nil || stats.Chunks == 0 {
		t.Fatalf("expected indexed chunks, got %+v err=%v", stats, err)
	}
	t.Logf("indexed %d chunks", stats.Chunks)

	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}

	w, err := NewWatcher(root, s, NewHashingEmbedder(), nil)
	if err != nil {
		t.Fatalf("new watcher: %v", err)
	}
	if err := w.HandleEvent(ctx, fsnotify.Event{Name: abs, Op: fsnotify.Remove}); err != nil {
		t.Fatalf("handle event: %v", err)
	}

	stats, err = s.RagStats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	t.Logf("after reap: %d chunks", stats.Chunks)
	if stats.Chunks != 0 {
		t.Fatalf("reaper failed: %d chunks remain", stats.Chunks)
	}
}

func TestIncrementalReindex(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	rel := "main.go"
	abs := filepath.Join(root, rel)

	s := openRAGStore(t)
	if err := os.WriteFile(abs, []byte("package main\n\nfunc Foo() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := NewWatcher(root, s, NewHashingEmbedder(), nil)
	// Initial index via watcher.
	if err := w.HandleEvent(ctx, fsnotify.Event{Name: abs, Op: fsnotify.Create}); err != nil {
		t.Fatalf("create: %v", err)
	}
	stats, _ := s.RagStats(ctx)
	t.Logf("after create: %d chunks", stats.Chunks)
	if stats.Chunks == 0 {
		t.Fatal("expected chunks after create")
	}

	// Modify file and re-trigger.
	if err := os.WriteFile(abs, []byte("package main\n\nfunc Bar() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.HandleEvent(ctx, fsnotify.Event{Name: abs, Op: fsnotify.Write}); err != nil {
		t.Fatalf("write: %v", err)
	}
	hits, err := s.Search(ctx, "Bar", nil, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("modified content not indexed")
	}
	hits2, _ := s.Search(ctx, "Foo", nil, 5)
	// Foo may still partially match text; just log.
	t.Logf("Foo hits after reindex: %d; Bar hits: %d", len(hits2), len(hits))
}
