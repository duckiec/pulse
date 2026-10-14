package rag

import (
	"bufio"
	"context"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Index walks root and yields chunks lazily.
//
// The laziness is the memory contract, not an API convenience. A generator that
// accumulated its output would hold every chunk of the repository live at once;
// a generator yields one chunk per pull, so a consumer that writes to a database
// and drops the chunk keeps the live set at "one file's chunks plus one worker"
// regardless of repository size. That is the difference between an indexer that
// scales and one that OOMs on a large monorepo.
//
//	seq := rag.Index(root, rag.WithContext(ctx),
//	    rag.WithEmbedder(rag.NewHashingEmbedder()))
//	for chunk := range seq {
//	    _ = store.UpsertChunks(ctx, chunk)
//	}
//
// File-level failures are yielded through `errors` rather than aborting the
// walk: one unparseable file must not cost the user the other 9,999.
func Index(root string, opts ...Option) iter.Seq2[Chunk, error] {
	o := Options{}
	for _, fn := range opts {
		if fn != nil {
			fn(&o)
		}
	}
	return func(yield func(Chunk, error) bool) {
		_, _ = IndexContext(ctxOrBackground(o.ctx), root, o, yield)
	}
}

// Option configures Index.
type Option func(*Options)

// WithContext binds the walk's context.
func WithContext(ctx context.Context) Option {
	return func(o *Options) { o.ctx = ctx }
}

// WithConcurrency caps walker workers.
func WithConcurrency(n int) Option {
	return func(o *Options) { o.Concurrency = n }
}

// WithMaxFileBytes overrides the per-file size cap.
func WithMaxFileBytes(n int64) Option {
	return func(o *Options) { o.MaxFileBytes = n }
}

// WithEmbedder installs the vector producer.
func WithEmbedder(e Embedder) Option {
	return func(o *Options) { o.Embedder = e }
}

// WithRedactor installs the chunk-time text scrubber.
func WithRedactor(fn func(string) string) Option {
	return func(o *Options) { o.Redact = fn }
}

// WithExtraDenied adds deny rules beyond the built-ins.
func WithExtraDenied(patterns ...string) Option {
	return func(o *Options) { o.ExtraDenied = append(o.ExtraDenied, patterns...) }
}

func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// IndexResult summarizes a completed indexing run.
type IndexResult struct {
	Stats    WalkStats
	Chunks   int  // chunks produced
	Vector   int  // chunks that carry a vector
	Failed   int  // files that could not be read or parsed at all
	Redacted bool // whether any text was scrubbed
}

// Degraded reports whether the run produced lexical-only chunks, which the
// caller MUST surface rather than present as a complete index.
func (r IndexResult) Degraded() bool { return r.Chunks > 0 && r.Vector == 0 }

// IndexContext walks root, yields chunks through yield, and returns what
// happened. It reports statistics that Index cannot, which is what the sync
// path and the gates need.
func IndexContext(ctx context.Context, root string, opts Options, yield func(Chunk, error) bool) (IndexResult, error) {
	parent := ctxOrBackground(ctx)
	o := opts.withDefaults()
	var res IndexResult

	w := newWalkCtx(root, o)

	// The consumer owns termination, and it is expressed by CANCELLING a derived
	// context rather than by a side channel.
	//
	// That choice is load-bearing for liveness, not style. A separate `stop`
	// channel has to be selected on at every point the producer can block, and
	// any point forgotten is a DEADLOCK: the walk channel fills, the consumer has
	// gone, and the enumerator blocks forever on a send nobody will receive. The
	// test suite then reports a ten-minute timeout instead of the assertion that
	// describes the defect.
	//
	// A context gives the escape for free, because the producer ALREADY selects
	// on `ctx.Done()` for the caller's own cancellation. Abandoning the generator
	// cancels the same channel, so there is exactly one termination mechanism
	// and no site where it can be forgotten.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	work := make(chan worker, o.Concurrency*2)
	done := make(chan struct{})

	go func() {
		defer close(done)
		for wk := range work {
			if ctx.Err() != nil {
				w.bump(func(s *WalkStats) { s.CancelledEarly = true })
				continue
			}
			chunks, err := w.process(ctx, wk, o)
			if err != nil {
				res.Failed++
				if !yield(Chunk{}, &CorpusError{Path: wk.rel, Err: err}) {
					return
				}
				continue
			}
			w.bump(func(s *WalkStats) { s.FilesIndexed++ })
			for i := range chunks {
				c := &chunks[i]
				hasVec := w.finalize(ctx, c, o)
				if !yield(*c, nil) {
					return
				}
				res.Chunks++
				if hasVec {
					res.Vector++
				}
			}
		}
	}()

	walkErr := w.enumerate(ctx, work, done)
	close(work)
	<-done

	res.Stats = w.snapshot()
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if walkErr != nil && !errorsIsBenign(walkErr) {
		return res, walkErr
	}
	return res, nil
}

// finalize fills the id, applies redaction, and embeds. It reports whether the
// chunk carries a vector.
//
// REDACTION ORDER MATTERS TWICE:
//
//  1. The id is computed from POSITION, not content, so scrubbing the text
//     cannot change the id and cause a rewrite storm of identical rows.
//  2. Redaction happens BEFORE embedding. An embedding of a secret is a secret
//     in a form nothing downstream can redact — it is already hashed into
//     numbers. Redacting after embedding would leave the secret in the vector.
func (w *walkCtx) finalize(ctx context.Context, c *Chunk, o Options) bool {
	// Thin adapter around finalizeOne (finalize.go): the ID, redaction, and
	// embedding rules live there alone, shared with the watcher path. This
	// wrapper only adds the walker's Redacted stat on top.
	hasVec, redacted := finalizeOne(ctx, c, o.Embedder, o.Redact)
	if redacted {
		w.bump(func(s *WalkStats) { s.Redacted++ })
	}
	return hasVec
}

// ErrFileTooLarge reports a file over the per-file cap.
var ErrFileTooLarge = errorsNew("rag: file exceeds the size limit")

// process reads and chunks one file, releasing its descriptor and buffer before
// returning.
//
// The deferred close IS the file-descriptor contract: every early return — too
// large, read error, context cancelled mid-read, parser fallback — unwinds
// through it. The FD leak gate cancels mid-walk and asserts the count returns
// to baseline, which holds only because there is exactly one close on the path
// every return shares.
func (w *walkCtx) process(ctx context.Context, wk worker, o Options) ([]Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(wk.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	want := int(wk.info.Size())
	sw := readBufPool.get(want)
	defer readBufPool.put(sw)

	// A pre-cancelled context must not read at all.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(f, scanBufferSize)
	// A private scratch for the read syscall, sized once per worker.
	scratch := make([]byte, scanBufferSize)
	if _, err := io.CopyBuffer(sw, br, scratch); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if int64(sw.n) > o.MaxFileBytes {
		return nil, ErrFileTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	src := sw.bytes()
	w.bumpBytes(int64(sw.n))
	w.bump(func(s *WalkStats) { s.PeakBuf = max(s.PeakBuf, sw.peak) })
	return chunkFile(wk.rel, wk.lang, src)
}

// readBufPool hands out reusable read buffers, grown to fit a file and returned
// on every path. A pool rather than a plain make() because a 1,000-file corpus
// would otherwise allocate 1,000 distinct buffers and hand the GC a thousand
// objects to chase mid-walk.
type readBufPoolT struct {
	p sync.Pool
}

var readBufPool = &readBufPoolT{}

// sliceWriter appends into its own backing array. io.CopyBuffer needs an
// io.Writer and []byte is not one; this is the minimum adapter that avoids
// copying the file's contents through an intermediate buffer.
type sliceWriter struct {
	buf  []byte // the backing array
	n    int    // valid prefix length
	peak int    // high-water mark, for the memory gate
}

func (s *sliceWriter) grow(n int) {
	if s.n+n <= cap(s.buf) {
		return
	}
	next := make([]byte, len(s.buf), max(2*cap(s.buf), s.n+n))
	copy(next, s.buf)
	s.buf = next
}

func (s *sliceWriter) Write(p []byte) (int, error) {
	s.grow(len(p))
	n := copy(s.buf[s.n:s.n+len(p)], p)
	s.n += n
	if s.n > s.peak {
		s.peak = s.n
	}
	return n, nil
}

func (s *sliceWriter) bytes() []byte { return s.buf[:s.n] }

// get returns a writer with an empty buffer of at least want bytes of capacity.
func (p *readBufPoolT) get(want int) *sliceWriter {
	if want < 32<<10 {
		want = 32 << 10
	}
	if v := p.p.Get(); v != nil {
		sw := v.(*sliceWriter)
		if cap(sw.buf) >= want {
			sw.n, sw.peak = 0, 0
			return sw
		}
		// Too small for this file; fall through and allocate a better one.
	}
	return &sliceWriter{buf: make([]byte, 0, want)}
}

func (p *readBufPoolT) put(sw *sliceWriter) {
	if sw == nil || cap(sw.buf) == 0 {
		return
	}
	sw.n, sw.peak = 0, 0
	p.p.Put(sw)
}

// ---- shared enumeration -----------------------------------------------------

// newWalkCtx builds the per-run state.
func newWalkCtx(root string, o Options) *walkCtx {
	w := &walkCtx{root: filepath.Clean(root), opts: o}
	if len(o.ExtraDenied) > 0 {
		w.extra = &ignoreSet{}
		for _, p := range o.ExtraDenied {
			w.extra.patterns = append(w.extra.patterns, normalizePattern(p))
		}
	}
	return w
}

// enumerate walks the tree and feeds work. It is shared by Walk and
// IndexContext so the two can never drift in what they consider indexable — a
// divergence there would mean `pulse reindex` and the library path disagreed
// about the repository, silently.
func (w *walkCtx) enumerate(ctx context.Context, work chan<- worker, done <-chan struct{}) error {
	root := w.root
	if _, err := os.Stat(root); err != nil {
		return &CorpusError{Path: root, Err: err}
	}

	last := time.Now()
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		// Cancellation is checked at directory boundaries, which is where
		// WalkDir spends its time in a large tree.
		if ctxErr := ctx.Err(); ctxErr != nil {
			w.bump(func(s *WalkStats) { s.CancelledEarly = true })
			return ctxErr
		}
		if time.Since(last) > w.opts.YieldInterval {
			last = time.Now()
		}

		if err != nil {
			w.bump(func(s *WalkStats) { s.FilesUnread++ })
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			w.bump(func(s *WalkStats) { s.FilesUnread++ })
			return nil
		}
		rel = filepath.ToSlash(rel)

		if d.IsDir() {
			w.bump(func(s *WalkStats) { s.DirsVisited++ })
			if p == root {
				w.loadRootIgnore(p)
				return nil
			}
			if w.skipDir(rel, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}

		w.bump(func(s *WalkStats) { s.FilesSeen++ })
		base := d.Name()
		if denied, _ := Denied(rel, base); denied {
			w.bump(func(s *WalkStats) { s.FilesDenied++ })
			return nil
		}
		lang := LangOf(base)
		if lang == "" {
			return nil // not source: not a denial, just not code
		}
		if w.extra.Match(rel) || w.ignore.get().Match(rel) {
			w.bump(func(s *WalkStats) { s.FilesIgnored++ })
			return nil
		}
		fi, statErr := d.Info()
		if statErr != nil {
			w.bump(func(s *WalkStats) { s.FilesUnread++ })
			return nil
		}
		if fi.Size() > w.opts.MaxFileBytes {
			w.bump(func(s *WalkStats) { s.FilesTooLarge++ })
			return nil
		}

		// The send selects on THREE ways out, and all three are needed:
		// `stop` for an orderly abandon, `ctx` for a cancellation, and `done`
		// for the case where the consumer has stopped and stopped is therefore
		// never signalled. Without the third, a full work channel plus a
		// consumer that has gone away is a DEADLOCK rather than a failure: the
		// enumerator blocks forever on a send nobody will receive, and the test
		// suite reports a ten-minute timeout instead of the assertion that
		// actually describes the defect.
		select {
		case work <- worker{path: p, rel: rel, lang: lang, info: fi}:
			return nil
		case <-done:
			return fs.SkipAll
		case <-ctx.Done():
			w.bump(func(s *WalkStats) { s.CancelledEarly = true })
			return ctx.Err()
		}
	})
}

// skipDir applies the three exclusion layers to a directory.
func (w *walkCtx) skipDir(rel, base string) bool {
	if denied, _ := Denied(rel, base); denied {
		return true
	}
	return w.extra.Match(rel) || w.ignore.get().Match(rel)
}
