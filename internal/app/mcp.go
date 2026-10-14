package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"pulse/internal/mcp"
	"pulse/internal/rag"
	"pulse/internal/store"
)

// IndexCorpus walks a codebase and writes the chunks into the store.
//
// It is a plain method rather than something New does automatically, because
// walking a repository is a cost the user should ask for: a user who has never
// enabled Local RAG must not pay to have their tree read on every boot. An
// empty index degrades the 5xx explanation to "no context attached", which the
// agent is told — that is a correct answer, not a broken feature.
//
// The walk is bounded at every level (per-file size, worker count, chunk count
// per transaction) and honours ctx, because it runs against the user's own
// filesystem and the user is entitled to stop it.
func (a *App) IndexCorpus(ctx context.Context, root string) (rag.IndexResult, error) {
	if a.Store == nil {
		return rag.IndexResult{}, fmt.Errorf("app: the store is not open")
	}
	if root == "" {
		return rag.IndexResult{}, fmt.Errorf("app: no corpus root given")
	}

	opts := rag.Options{
		Embedder:    a.Embedder,
		Concurrency: a.cfg.IndexConcurrency,
		Redact:      a.Redactor.String,
	}
	// The redactor is bound to a METHOD VALUE, not to a closure over `a`. It
	// carries the redactor with it, so a chunk cannot be redacted by a
	// different instance than the one holding the values.
	if a.Redactor == nil {
		// Defensive: New always sets it, and an index written without
		// scrubbing is a secret on disk. Better to refuse than to index.
		return rag.IndexResult{}, fmt.Errorf("app: refusing to index without a redactor")
	}

	// Chunks are buffered and written in batches. Writing per chunk would put
	// a transaction on the hot path of a walk that may produce tens of
	// thousands of chunks, and store.UpsertRAGChunks already bounds itself at
	// 256 rows per transaction.
	const batchSize = 256
	var (
		batch    = make([]store.RagChunk, 0, batchSize)
		written  int
		skipped  int
		stopWalk bool
		writeErr error
	)

	// The counters live here rather than on res because the callback is an
	// ARGUMENT to IndexContext: res is not assigned until it returns, and a
	// closure reading it would read the zero value.
	res, err := rag.IndexContext(ctx, root, opts, func(c rag.Chunk, cErr error) bool {
		if cErr != nil {
			// A file that could not be read is counted and skipped; a corpus
			// with one unreadable file is still a corpus. The count is reported
			// so the user can see it rather than infer it from a smaller index.
			skipped++
			return true
		}
		batch = append(batch, store.RagChunk{
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
		if len(batch) < batchSize {
			return true
		}
		n, wErr := a.Store.UpsertRAGChunks(ctx, batch)
		if wErr != nil {
			stopWalk = true
			writeErr = wErr
			// Returning false stops the walk: the database is not accepting
			// writes, and continuing would discard every chunk after this one
			// silently — the exact failure an index is supposed to detect.
			return false
		}
		written += n
		batch = batch[:0]
		return true
	})
	if err != nil {
		return rag.IndexResult{}, fmt.Errorf("app: index %s: %w", root, err)
	}
	if writeErr != nil {
		return res, fmt.Errorf("app: write the index to the store: %w", writeErr)
	}
	res.Failed = skipped
	if len(batch) > 0 {
		n, err := a.Store.UpsertRAGChunks(ctx, batch)
		if err != nil {
			return res, fmt.Errorf("app: write the final index batch: %w", err)
		}
		written += n
	}
	res.Chunks = written
	_ = stopWalk
	return res, nil
}

// StartWatcherIfConfigured boots the production RAG lifecycle (Audit 1C,
// P4.1b.11): index the configured corpus, then start the recursive fsnotify
// watcher bound to the App lifecycle context. With no CorpusRoot it does
// nothing. The watcher is stopped deterministically by Close before the store
// closes.
func (a *App) StartWatcherIfConfigured(ctx context.Context) error {
	if a.cfg.CorpusRoot == "" {
		return nil
	}
	if _, err := a.IndexCorpusIfConfigured(ctx); err != nil {
		return err
	}
	if a.Redactor == nil {
		return fmt.Errorf("app: refusing to watch without a redactor")
	}
	w, err := rag.NewWatcher(a.cfg.CorpusRoot, a.Store, a.Embedder, a.Redactor.String)
	if err != nil {
		a.degrade(fmt.Sprintf("local RAG watcher could not start for %s: %v", a.cfg.CorpusRoot, err))
		return nil
	}
	if err := w.Start(a.ctx); err != nil {
		a.degrade(fmt.Sprintf("local RAG watcher could not start for %s: %v", a.cfg.CorpusRoot, err))
		return nil
	}
	a.Watcher = w
	return nil
}

// IndexCorpusIfConfigured indexes Config.CorpusRoot when one is set, and does
// nothing otherwise.
//
// It is called by New's callers (the CLI boot) rather than by New itself, so
// that a test can build an App without touching the filesystem outside its
// temporary directory.
func (a *App) IndexCorpusIfConfigured(ctx context.Context) (rag.IndexResult, error) {
	if a.cfg.CorpusRoot == "" {
		return rag.IndexResult{}, nil
	}
	// The walk gets its own deadline: an unbounded walk over a huge tree is a
	// hang with no diagnostic, and the index is an optimisation.
	walkCtx, cancel := context.WithTimeout(ctx, a.cfg.RunTimeout*4)
	defer cancel()
	res, err := a.IndexCorpus(walkCtx, a.cfg.CorpusRoot)
	if err != nil {
		a.degrade(fmt.Sprintf("local RAG could not index %s: %v", a.cfg.CorpusRoot, err))
		return rag.IndexResult{}, nil
	}
	return res, nil
}

// MCPServer builds the MCP server from the wired dependencies.
//
// Every field of mcp.Config is populated from the App. The nil-ness is the
// contract: a non-nil Executor, a non-nil FuzzRunner, and a real Redactor. That
// is what "wired" means, and it is asserted rather than assumed.
func (a *App) MCPServer() (*mcp.Server, error) {
	if a.Executor == nil {
		return nil, fmt.Errorf("app: refusing to serve MCP without an executor")
	}
	if a.FuzzRunner == nil {
		return nil, fmt.Errorf("app: refusing to serve MCP without a fuzz runner")
	}
	if a.Redactor == nil {
		// Refusing is the point. A server with no redactor still answers every
		// request; it just answers them with the user's tokens in the body.
		// Failing at construction makes that impossible to ship by accident.
		return nil, fmt.Errorf("app: refusing to serve MCP without a redactor")
	}
	return mcp.NewServerWithConfig(mcp.Config{
		Exec:         a.Executor,
		FuzzRunner:   a.FuzzRunner,
		Redactor:     a.Redactor,
		FuzzFindings: a.Store,
		Options:      []mcp.Option{mcp.WithFailureBinder(a.Binder)},
	})
}

// ServeMCP runs the MCP stdio session until stdin ends or ctx is cancelled.
//
// The stdout handle is a parameter and never os.Stdout, for the same reason
// Server.Serve takes one: the caller captured the real channel before
// mcp.Lock replaced it, and this is the only component permitted to hold it
// (AGENTS.md §3.3).
//
// A clean stdin EOF is SUCCESS — it is how an agent says goodbye — as is a
// cancelled context. Everything else is an error.
func (a *App) ServeMCP(ctx context.Context, stdin io.Reader, stdout io.Writer) error {
	srv, err := a.MCPServer()
	if err != nil {
		return err
	}
	err = srv.Serve(ctx, stdin, stdout)
	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, context.Canceled):
		// A clean end of input is how an agent says goodbye, and a cancelled
		// context is how it says stop. Neither is a failure, and reporting
		// either as one makes a well-behaved client look broken.
		return nil
	default:
		return err
	}
}

// Wait blocks until the app's context is done or d elapses.
//
// It exists so a shutdown path can give in-flight work a bounded moment to
// unwind before Close pulls its resources, rather than either racing or waiting
// forever.
func (a *App) Wait(d time.Duration) bool {
	if d <= 0 {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-a.ctx.Done():
		return true
	case <-t.C:
		return false
	}
}
