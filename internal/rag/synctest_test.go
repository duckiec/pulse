package rag

// Synctest bubbles for the fsnotify watcher (ROADMAP P3.3.6, AGENTS.md §2.2).
//
// Debounce and cancellation are CONCURRENCY properties: wall-clock tests of
// them sleep real hundreds of milliseconds and still tolerate scheduling
// jitter. These bubbles drive the real runLoop over synthetic channels
// against the synthetic clock — the burst test's 5-event save storm costs
// zero wall time, and the wall-time fence proves no real sleep leaked in.
//
// The store underneath is a real on-disk SQLite index (os.MkdirTemp, never
// t.TempDir: testing helpers that register Cleanup must not run inside a
// bubble). Only runLoop joins the bubble; database/sql starts no background
// goroutines, so bubble exit additionally proves the loop left none behind.

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fsnotify/fsnotify"

	"pulse/internal/store"
)

const bubbleGoSrc = "package main\n\nfunc Foo() int { return 1 }\n"

// countingEmbedder wraps the shipped hash embedder and counts Embed calls:
// the count is the observable proof of how many times a burst was handled.
type countingEmbedder struct {
	inner Embedder
	calls atomic.Int64
}

func (c *countingEmbedder) Embed(ctx context.Context, text string) ([]int8, float32, error) {
	c.calls.Add(1)
	return c.inner.Embed(ctx, text)
}

func (c *countingEmbedder) Name() string    { return c.inner.Name() }
func (c *countingEmbedder) Available() bool { return true }

// openBubbleStore opens a real RAG index on a plain (non-testing) temp dir:
// t.TempDir registers a Cleanup, which a bubble must not trigger.
func openBubbleStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pulse-rag-bubble-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	dbPath := filepath.Join(dir, "rag.db")
	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.InitRAG(context.Background()); err != nil {
		st.Close()
		os.RemoveAll(dir)
		t.Fatalf("InitRAG: %v", err)
	}
	return st, dir
}

// TestBubbleWatcherDebouncesBursts proves the quiet-window coalescing: five
// write events for one file inside the debounce window must embed exactly
// what ONE direct handling embeds — no more — and the chunk must still land
// in the index.
func TestBubbleWatcherDebouncesBursts(t *testing.T) {
	wallStart := time.Now()
	synctest.Test(t, func(t *testing.T) {
		st, dir := openBubbleStore(t)
		defer st.Close()
		defer os.RemoveAll(dir)
		abs := filepath.Join(dir, "main.go")
		if err := os.WriteFile(abs, []byte(bubbleGoSrc), 0644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		ce := &countingEmbedder{inner: NewHashingEmbedder()}
		w := &Watcher{Root: dir, Store: st, Embedder: ce, Redact: func(s string) string { return s }, done: make(chan struct{})}

		// Measure what a single handling costs in Embed calls, so the burst
		// assertion is independent of how the chunker cuts this file.
		if err := w.HandleEvent(context.Background(), fsnotify.Event{Name: abs, Op: fsnotify.Create}); err != nil {
			t.Fatalf("HandleEvent: %v", err)
		}
		single := ce.calls.Load()
		if single == 0 {
			t.Fatal("direct handling embedded nothing: the fixture produces no chunks")
		}
		// Clear the index so the burst below is measured from zero.
		if err := w.HandleEvent(context.Background(), fsnotify.Event{Name: abs, Op: fsnotify.Remove}); err != nil {
			t.Fatalf("HandleEvent(remove): %v", err)
		}
		ce.calls.Store(0)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		events := make(chan fsnotify.Event)
		errs := make(chan error)
		go w.runLoop(ctx, events, errs)
		for i := 0; i < 5; i++ {
			events <- fsnotify.Event{Name: abs, Op: fsnotify.Write}
		}
		time.Sleep(3 * debounceWindow)
		time.Sleep(debounceWindow)
		if got := ce.calls.Load(); got != single {
			t.Errorf("5-event burst embedded %d times, want %d (one debounced handling)", got, single)
		}
		stats, err := st.RagStats(ctx)
		if err != nil {
			t.Fatalf("RagStats: %v", err)
		}
		if stats.Chunks == 0 {
			t.Error("debounced event never indexed: the quiet edge dropped the burst")
		}
		cancel()
		w.Wait()
	})
	if wall := time.Since(wallStart); wall > 10*time.Second {
		t.Errorf("bubble consumed %v wall time: a real sleep leaked into the watcher path", wall)
	}
}

// TestBubbleWatcherCancelDrainsLoop proves context cancellation stops the
// loop promptly and closes the done channel — the drain half of the RAG
// concurrency contract. No events are sent; the 10s virtual sleep would hang
// a wall-clock test for ten real seconds.
func TestBubbleWatcherCancelDrainsLoop(t *testing.T) {
	wallStart := time.Now()
	synctest.Test(t, func(t *testing.T) {
		st, dir := openBubbleStore(t)
		defer st.Close()
		defer os.RemoveAll(dir)
		w := &Watcher{Root: dir, Store: st, Embedder: NewHashingEmbedder(), Redact: func(s string) string { return s }, done: make(chan struct{})}
		ctx, cancel := context.WithCancel(context.Background())
		events := make(chan fsnotify.Event)
		errs := make(chan error)
		go w.runLoop(ctx, events, errs)
		time.Sleep(10 * time.Second)
		cancel()
		w.Wait()
	})
	if wall := time.Since(wallStart); wall > 10*time.Second {
		t.Errorf("bubble consumed %v wall time: cancellation waited on a real clock", wall)
	}
}
