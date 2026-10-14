package rag

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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

// debounceWindow coalesces editor save bursts: one save often arrives as
// several events (create, write, chmod) within milliseconds, and every
// HandleEvent is a delete-plus-reindex transaction. Handling each raw event
// would reindex the same file several times for one save; holding the burst
// for a quiet window collapses it to a single handling of the latest state.
const debounceWindow = 100 * time.Millisecond

// Start begins background event processing. It is safe to call once.
func (w *Watcher) Start(ctx context.Context) error {
	if err := w.addRecursive(w.Root); err != nil {
		return err
	}
	w.done = make(chan struct{})
	go w.runLoop(ctx, w.watcher.Events, w.watcher.Errors)
	return nil
}

// Stop releases the underlying watcher.
func (w *Watcher) Stop() error {
	if w.watcher != nil {
		return w.watcher.Close()
	}
	return nil
}

// Wait blocks until the background event loop has exited. It must be called
// after Stop (or context cancellation) for a deterministic shutdown: Close
// without Wait races the loop's final upsert against the store close.
func (w *Watcher) Wait() {
	if w.done != nil {
		<-w.done
	}
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

// runLoop is the production event loop over injectable channels: Start wires
// the live fsnotify channels, and the synctest gate wires synthetic ones —
// the same function, the same debounce, the same cancellation. Events
// arriving within a debounceWindow collapse per path with the latest op
// winning; the quiet edge flushes through HandleEvent.
//
// Resetting a live timer with a possibly-undrained channel can only flush
// EARLY, never late or never: the worst case is one extra HandleEvent for a
// burst, which is still correct (reindexing is idempotent) and still
// bounded by the burst rate. The direction of the race is the safe one, so
// no drain dance is needed.
func (w *Watcher) runLoop(ctx context.Context, events <-chan fsnotify.Event, errs <-chan error) {
	defer close(w.done)
	var pending map[string]fsnotify.Event
	var timer *time.Timer
	var timerCh <-chan time.Time
	flush := func() {
		for _, ev := range pending {
			_ = w.HandleEvent(ctx, ev)
		}
		pending = nil
		timerCh = nil
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				// The watcher closed with events still held: flush them
				// rather than dropping the last save on the floor.
				flush()
				return
			}
			if pending == nil {
				pending = make(map[string]fsnotify.Event)
			}
			pending[ev.Name] = ev
			if timer == nil {
				timer = time.NewTimer(debounceWindow)
			} else {
				timer.Reset(debounceWindow)
			}
			timerCh = timer.C
		case <-timerCh:
			flush()
		case err, ok := <-errs:
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
	// Single seam (finalize.go): ID, redaction, and embedding live in
	// prepareChunks/finalizeOne alone, shared with the bulk walker path.
	stored, err := prepareChunks(ctx, rel, lang, src, w.Embedder, w.Redact)
	if err != nil {
		return fmt.Errorf("rag: chunk %s: %w", rel, err)
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
