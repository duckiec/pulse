package rag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"

	"pulse/internal/store"
)

// Watcher incrementally reindexes a workspace into the SQLite RAG store and
// reaps chunks whose source file disappeared or changed shape. It watches
// recursively; event handling is the same code path whether fsnotify drives it
// or a test calls HandleEvent directly, which is how the Reaper Gate runs
// without a live filesystem race.
type Watcher struct {
	Root     string
	Store    *store.Store
	Embedder Embedder
	Redact   func(string) string

	mu      sync.Mutex
	watcher *fsnotify.Watcher
	done    chan struct{}
}

// NewWatcher builds a watcher. It does not start watching until Start.
func NewWatcher(root string, st *store.Store, embedder Embedder, redact func(string) string) (*Watcher, error) {
	if root == "" {
		return nil, fmt.Errorf("rag: watcher root is required")
	}
	if st == nil {
		return nil, fmt.Errorf("rag: watcher store is required")
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("rag: create fsnotify watcher: %w", err)
	}
	return &Watcher{Root: root, Store: st, Embedder: embedder, Redact: redact, watcher: w}, nil
}

// Start begins background event processing. It is safe to call once.
func (w *Watcher) Start(ctx context.Context) error {
	if err := w.addRecursive(w.Root); err != nil {
		return err
	}
	w.done = make(chan struct{})
	go w.loop(ctx)
	return nil
}

// Stop releases the underlying watcher.
func (w *Watcher) Stop() error {
	if w.watcher != nil {
		return w.watcher.Close()
	}
	return nil
}

func (w *Watcher) addRecursive(root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if deniedDir(filepath.Base(path)) {
				return filepath.SkipDir
			}
			if err := w.watcher.Add(path); err != nil {
				return nil
			}
		}
		return nil
	})
}

func deniedDir(name string) bool {
	if _, ok := deniedDirs[name]; ok {
		return true
	}
	_, ok := deniedDirs[strings.ToLower(name)]
	return ok
}

func (w *Watcher) loop(ctx context.Context) {
	defer close(w.done)
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			_ = w.HandleEvent(ctx, ev)
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			_ = err
		}
	}
}

// HandleEvent processes a single fsnotify event. Exposed so tests and batch
// callers can drive reindexing without a live watcher. The Reaper Gate calls
// this directly.
func (w *Watcher) HandleEvent(ctx context.Context, ev fsnotify.Event) error {
	rel := w.relative(ev.Name)

	// Hard removals and renames reap the file's old chunks first. This is the
	// aggressive tombstone: nothing from the previous content survives.
	if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		_, err := w.Store.DeleteRAGChunksByFile(ctx, rel)
		return err
	}

	// For a modification or creation, reap the stale chunks BEFORE writing the
	// new ones. Reaping first means a heavily modified file cannot leave
	// phantom spans behind.
	if _, err := w.Store.DeleteRAGChunksByFile(ctx, rel); err != nil {
		return err
	}

	abs := ev.Name
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(w.Root, rel)
	}
	info, err := os.Stat(abs)
	if err != nil {
		// The path is gone; make sure nothing lingers.
		_, _ = w.Store.DeleteRAGChunksByFile(ctx, rel)
		return nil
	}
	if info.IsDir() {
		return nil
	}
	lang := LangOf(abs)
	if lang == "" {
		return nil
	}
	if info.Size() > 1<<20 {
		return nil
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Errorf("rag: read %s: %w", rel, err)
	}
	chunks, err := chunkFile(rel, lang, src)
	if err != nil {
		return fmt.Errorf("rag: chunk %s: %w", rel, err)
	}

	// Finalize each chunk: id, redaction, embedding. Mirrors finalize() in the
	// walker so watcher-written chunks agree byte-for-byte with index-written
	// ones.
	stored := make([]store.RagChunk, 0, len(chunks))
	for i := range chunks {
		c := &chunks[i]
		c.MakeID()
		if w.Redact != nil {
			c.Text = w.Redact(c.Text)
		}
		if w.Embedder != nil && w.Embedder.Available() {
			if vec, scale, err := w.Embedder.Embed(ctx, c.Symbol+" "+c.Path+"\n"+c.Text); err == nil && len(vec) > 0 {
				c.Vec, c.Scale = vec, scale
			}
		}
		stored = append(stored, store.RagChunk{
			ID:      c.ID,
			Path:    c.Path,
			Lang:    c.Lang,
			StartLn: c.StartLn,
			EndLn:   c.EndLn,
			Symbol:  c.Symbol,
			Text:    c.Text,
			Vec:     c.Vec,
			Scale:   c.Scale,
		})
	}
	if len(stored) == 0 {
		return nil
	}
	if _, err := w.Store.UpsertRAGChunks(ctx, stored); err != nil {
		return fmt.Errorf("rag: upsert %s: %w", rel, err)
	}
	return nil
}

// Trigger is a small convenience wrapper around HandleEvent for a path + op.
func (w *Watcher) Trigger(ctx context.Context, path string, op fsnotify.Op) error {
	return w.HandleEvent(ctx, fsnotify.Event{Name: path, Op: op})
}

func (w *Watcher) relative(path string) string {
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(w.Root, path)
		if err == nil {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(filepath.Clean(path))
}
