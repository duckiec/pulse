package rag

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Walker defaults. Each is a BOUND, not a guess: the memory gate asserts the
// live working set stays inside the pipeline budget, and these numbers are what
// make that true (indexer_test.go, TestMemoryCeilingRSSOverAThousandFileCorpus).
const (
	// DefaultConcurrency is deliberately far below runtime.NumCPU(). Indexing
	// is I/O-bound on cold page cache and allocation-bound on the parser, so
	// matching core count would raise the LIVE working set without raising
	// throughput — the exact trade the 2 MiB budget forbids.
	DefaultConcurrency = 4

	// maxFileBytes skips a file larger than this without reading it. A 40 MB
	// generated client would otherwise be read whole, parsed, and held live
	// by one worker — 40 MB against a 2 MiB budget, from a single file.
	maxFileBytes = 512 << 10 // 512 KiB

	// scanBufferSize is the read buffer used per file. Small on purpose: it is
	// one per worker and it is live for the whole read.
	scanBufferSize = 64 << 10 // 64 KiB
)

// deniedExact are file NAMES that are never opened, whatever the directory
// says. A deny-list that a `.gitignore` can talk its way out of is not a
// deny-list (AGENTS.md §4.3: a secret is never read, so it cannot be indexed).
var deniedExact = map[string]bool{
	".env":             true,
	".envrc":           true,
	".netrc":           true,
	".npmrc":           true,
	".pypirc":          true,
	".git-credentials": true,
	"credentials":      true,
	"id_rsa":           true,
	"id_ed25519":       true,
	"secrets.yaml":     true,
	"secrets.yml":      true,
	"secrets.json":     true,
	"pulse.toml":       true,
}

// deniedExt are file EXTENSIONS that are never opened.
var deniedExt = map[string]bool{
	".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true,
	".keystore": true, ".crt": true, ".cer": true, ".der": true,
	".env": true,
	// Binary and archive payloads: not source, and a wrong-extension zip can
	// be arbitrarily large.
	".so": true, ".dylib": true, ".dll": true, ".a": true, ".o": true,
	".class": true, ".jar": true, ".war": true, ".zip": true, ".gz": true,
	".bz2": true, ".xz": true, ".7z": true, ".tar": true, ".db": true,
	".sqlite": true, ".sqlite3": true, ".bin": true, ".exe": true, ".wasm": true,
}

// deniedDirs are directory NAMES skipped wherever they appear. `.git` and
// friends hold history, not working source, and walking them is the single
// easiest way to make an index explode in size and time.
var deniedDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, ".bzr": true,
	"node_modules": true, "vendor": true, "dist": true, "build": true,
	"target": true, "__pycache__": true, ".venv": true, "venv": true,
	"env": true, ".tox": true, ".mypy_cache": true, ".pytest_cache": true,
	".ruff_cache": true, ".next": true, ".nuxt": true, ".svelte-kit": true,
	".gradle": true, ".idea": true, ".vscode": true, ".cache": true,
	".terraform": true, "coverage": true, ".turbo": true, ".parcel-cache": true,
}

// Denied reports whether a path must never be opened, and why.
//
// The order matters: the directory check runs first because a directory name
// like `secrets` should stop the walk before it ever enumerates inside.
func Denied(rel, base string) (denied bool, reason string) {
	if deniedDirs[base] || deniedDirs[strings.ToLower(base)] {
		return true, "directory is excluded by name"
	}
	if deniedExact[strings.ToLower(base)] {
		return true, "filename is on the secret deny-list"
	}
	ext := strings.ToLower(filepath.Ext(base))
	if deniedExt[ext] {
		return true, "extension is on the secret/binary deny-list"
	}
	// `.env.local`, `.env.production`, `.envrc.bak` — any basename that starts
	// with `.env` is an environment file whatever follows it.
	if lower := strings.ToLower(base); strings.HasPrefix(lower, ".env") {
		return true, "environment file"
	}
	return false, ""
}

// ignoreSet holds parsed .gitignore patterns for one repository.
//
// This is a deliberate SUBSET of gitignore, and the subset is documented rather
// than implied: what it covers is sufficient to keep build output and vendored
// dependencies out of the index, and what it omits could produce a surprising
// exclusion if guessed at. An indexer that silently drops a user's source is
// worse than one that indexes a little too much, because the symptom is an
// explanation that silently cannot find the file you are staring at.
type ignoreSet struct {
	patterns []string // negations stripped, each a slash-anchored or basename glob
	negate   []string
}

// parseIgnore reads a .gitignore. Comments and blanks are dropped; a leading
// `!` is recorded as a negation. Patterns are kept in file order so later rules
// win, which is git's own precedence.
func parseIgnore(r *bufio.Reader) *ignoreSet {
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		return &ignoreSet{}
	}
	return parseIgnoreText(b.String())
}

// parseIgnoreText is the pure core of parseIgnore, separated so the gate can
// exercise the matcher without a file on disk.
func parseIgnoreText(body string) *ignoreSet {
	set := &ignoreSet{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "!") {
			set.negate = append(set.negate, normalizePattern(strings.TrimPrefix(line, "!")))
			continue
		}
		set.patterns = append(set.patterns, normalizePattern(line))
	}
	return set
}

// normalizePattern strips a trailing slash (a directory rule) and surrounding
// whitespace, leaving the pattern body comparable.
//
// It deliberately PRESERVES a leading slash, which is what records that the rule
// was anchored to the repository root. Stripping it (the obvious cleanup) would
// turn `/root-only.txt` into a rule that matches at any depth — over-excluding,
// and silently.
func normalizePattern(p string) string {
	p = strings.TrimSpace(p)
	p = strings.TrimSuffix(p, "/")
	return p
}

// Match reports whether a repo-relative path is excluded.
//
// A pattern containing a slash is matched against the whole relative path; a
// pattern without one matches any path COMPONENT, which is git's rule and the
// reason `node_modules` in a root .gitignore excludes nested copies too.
func (s *ignoreSet) Match(rel string) bool {
	if s == nil || (len(s.patterns) == 0 && len(s.negate) == 0) {
		return false
	}
	matched := false
	for _, p := range s.patterns {
		if matchAnchored(p, rel) {
			matched = true
		}
	}
	// Negations are applied after, so `!keep.log` re-includes a file an earlier
	// rule excluded — the one case where "later wins" is not enough.
	for _, p := range s.negate {
		if matchAnchored(p, rel) {
			matched = false
		}
	}
	return matched
}

// matchPattern applies one rule to a relative path.
func matchPattern(pattern, rel string) bool {
	return matchAnchored(pattern, rel)
}

// matchAnchored applies a rule that git anchors to the repository root because
// it contains a slash.
//
// The naive implementation — comparing the rule against the basename, or
// checking a "somewhere on this path" prefix — makes `/root-only.txt` exclude
// `sub/root-only.txt`. Git does the opposite: a leading slash anchors the rule
// so it matches only at the root. Getting this wrong over-excludes, and an
// indexer that silently drops source is worse than one that indexes a little
// too much, because the symptom is an explanation that cannot find the file the
// user is looking at.
func matchAnchored(pattern, rel string) bool {
	if pattern == "" {
		return false
	}
	// A LEADING slash anchors the rule to the repository root, independent of
	// whether the rule contains any other slash. That is the distinction the
	// naive implementation gets wrong: it checks for a slash anywhere, so
	// `/root-only.txt` is treated as a basename rule and excludes
	// `sub/root-only.txt`.
	if strings.HasPrefix(pattern, "/") {
		body := strings.TrimPrefix(pattern, "/")
		if ok, _ := filepath.Match(body, rel); ok {
			return true
		}
		return strings.HasPrefix(rel, body+"/")
	}
	if strings.Contains(pattern, "/") {
		// Anchored (or multi-segment): match the path from the root only.
		if ok, _ := filepath.Match(pattern, rel); ok {
			return true
		}
		if strings.HasPrefix(rel, pattern+"/") {
			return true
		}
		return false
	}
	// No slash: git matches any COMPONENT, which is why a root `node_modules`
	// rule excludes nested copies too.
	if ok, _ := filepath.Match(pattern, rel); ok {
		return true
	}
	if ok, _ := filepath.Match(pattern, filepath.Base(rel)); ok {
		return true
	}
	return strings.Contains("/"+rel, "/"+pattern+"/")
}

// WalkStats are the walker's observable behaviour, reported so a caller can
// state what happened rather than infer it. Every field exists because a
// surprising count in one of them is the difference between "the index is
// small because the repo is small" and "the index is small because the
// deny-list ate everything".
type WalkStats struct {
	DirsVisited    int
	FilesSeen      int
	FilesIndexed   int
	FilesTooLarge  int
	FilesDenied    int
	FilesIgnored   int
	FilesUnread    int
	Redacted       int // chunks whose text the scrubber changed
	BytesRead      int64
	PeakBuf        int // largest single-file read buffer ever held
	CancelledEarly bool
}

// Options configures a walk and a chunk run.
type Options struct {
	// Concurrency caps simultaneous workers. 0 selects DefaultConcurrency.
	// The cap is not advisory: more workers means more live file buffers, and
	// the memory budget is denominated in bytes, not in files.
	Concurrency int

	// MaxFileBytes overrides the per-file size cap. 0 selects maxFileBytes.
	MaxFileBytes int64

	// Embedder produces structural vectors. nil means vectors are not
	// produced at all and retrieval is lexical-only — which callers must
	// report as DEGRADED, not as "no matches".
	Embedder Embedder

	// Redact scrubs a chunk's text before it leaves this package. nil means no
	// value scrubbing. It is applied at CHUNK time, not at render time: a
	// secret that was written to the store is a secret already written.
	Redact func(string) string

	// ExtraDenied adds caller's deny rules on top of the built-ins.
	ExtraDenied []string

	// YieldInterval is how often a long walk checks for cancellation. A
	// directory with a million entries must still abort promptly on cancel.
	YieldInterval time.Duration

	// ctx is bound by WithContext, not exposed as a field, so a caller cannot
	// set it without going through the option that also records intent.
	ctx context.Context
}

// withDefaults fills unset fields.
func (o Options) withDefaults() Options {
	if o.Concurrency <= 0 {
		o.Concurrency = DefaultConcurrency
	}
	if o.Concurrency > DefaultConcurrency*4 {
		o.Concurrency = DefaultConcurrency * 4
	}
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = maxFileBytes
	}
	if o.YieldInterval <= 0 {
		o.YieldInterval = 25 * time.Millisecond
	}
	return o
}

// worker is one file's worth of work handed to a pool goroutine. It is the
// internal form of Worker; the split exists so the enumerate loop does not have
// to construct an exported type with fields a caller cannot use.
type worker struct {
	path string // absolute
	rel  string // repo-relative, slash-separated
	lang string
	info fs.FileInfo
}

// asWorker converts to the exported form.
func (w worker) asWorker() Worker {
	var size int64
	if w.info != nil {
		size = w.info.Size()
	}
	return Worker{Path: w.path, Rel: w.rel, Lang: w.lang, Size: size}
}

// WalkStats accumulates across workers.
func (st *WalkStats) add(other WalkStats) {
	st.DirsVisited += other.DirsVisited
	st.FilesSeen += other.FilesSeen
	st.FilesIndexed += other.FilesIndexed
	st.FilesTooLarge += other.FilesTooLarge
	st.FilesDenied += other.FilesDenied
	st.FilesIgnored += other.FilesIgnored
	st.FilesUnread += other.FilesUnread
	st.Redacted += other.Redacted
	st.BytesRead += other.BytesRead
	st.CancelledEarly = st.CancelledEarly || other.CancelledEarly
}

// walkCtx is the per-run mutable state shared by the enumerator and the pool.
type walkCtx struct {
	root    string
	opts    Options
	ignore  atomicIgnore // swapped once the root .gitignore is read
	extra   *ignoreSet   // caller deny rules, checked as negations are not
	stats   WalkStats
	statsMu sync.Mutex
}

// atomicIgnore is a mutex-guarded *ignoreSet: the root .gitignore is read
// lazily on the first directory and must be visible to workers already running.
type atomicIgnore struct {
	mu  sync.RWMutex
	set *ignoreSet
}

func (a *atomicIgnore) get() *ignoreSet {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.set
}

func (a *atomicIgnore) set_(s *ignoreSet) {
	a.mu.Lock()
	a.set = s
	a.mu.Unlock()
}

// Walk enumerates the indexable files under root, handing each to fn in an
// unspecified order.
//
// It exists for callers that want the FILE LIST without chunking (a CLI
// "what would be indexed" report, the corpus generator in the gates). Indexing
// itself goes through Index, which chunks as it goes and therefore never holds
// more than one file's chunks live.
//
// The concurrency is a FIXED POOL, not one goroutine per file: a goroutine per
// file is unbounded memory by construction, and an unbounded walk is exactly
// what the 2 MiB budget forbids.
func Walk(ctx context.Context, root string, opts Options, fn func(Worker) error) (WalkStats, error) {
	opts = opts.withDefaults()
	info, err := os.Stat(root)
	if err != nil {
		return WalkStats{}, &CorpusError{Path: root, Err: err}
	}
	if !info.IsDir() {
		return WalkStats{}, &CorpusError{Path: root, Err: errNotADirectory}
	}

	w := newWalkCtx(root, opts)
	work := make(chan worker, opts.Concurrency*2)
	done := make(chan struct{})
	var wg sync.WaitGroup
	go func() { wg.Wait(); close(done) }()

	var firstErr error
	var errOnce sync.Once
	for i := 0; i < opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for wk := range work {
				if ctx.Err() != nil {
					w.bump(func(s *WalkStats) { s.CancelledEarly = true })
					continue
				}
				if err := fn(wk.asWorker()); err != nil {
					errOnce.Do(func() { firstErr = err })
				}
			}
		}()
	}

	walkErr := w.enumerate(ctx, work, done)
	close(work)
	wg.Wait()

	st := w.snapshot()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return st, ctxErr
	}
	if firstErr != nil {
		return st, firstErr
	}
	if walkErr != nil && !errorsIsBenign(walkErr) {
		return st, walkErr
	}
	return st, nil
}

// Worker describes one file a walk produced. It is exported so callers of Walk
// can act on a file without importing unexported types.
type Worker struct {
	// Path is the absolute on-disk path.
	Path string
	// Rel is the repo-relative, slash-separated path. This is what a Chunk
	// records, so an index is portable across machines.
	Rel string
	// Lang is the language tag, never empty (the walker filters non-source).
	Lang string
	// Size is the file's byte size, already checked against the cap.
	Size int64
}

// errNotADirectory is reported when a walk root is a file.
var errNotADirectory = errors.New("rag: walk root is not a directory")

// errorsIsBenign reports whether a walk error is an ordinary termination
// signal rather than a failure worth reporting to the user.
func errorsIsBenign(err error) bool {
	return err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, fs.SkipAll) ||
		errors.Is(err, fs.SkipDir)
}

// errorsNew is a tiny indirection so the package's sentinels are declared in
// one style; it exists only to keep the Err* declarations terse.
func errorsNew(msg string) error { return errors.New(msg) }

// loadRootIgnore reads the root .gitignore once, best-effort.
func (w *walkCtx) loadRootIgnore(root string) {
	f, err := os.Open(filepath.Join(root, ".gitignore"))
	if err != nil {
		w.ignore.set_(&ignoreSet{})
		return
	}
	defer f.Close() // one descriptor, held for the life of the walk's start only
	set := parseIgnore(bufio.NewReaderSize(f, 8<<10))
	w.ignore.set_(set)
}

// bump mutates the shared stats under the lock.
func (w *walkCtx) bump(f func(*WalkStats)) {
	w.statsMu.Lock()
	f(&w.stats)
	w.statsMu.Unlock()
}

// bumpBytes adds to the read total, the hot counter during a large walk.
func (w *walkCtx) bumpBytes(n int64) {
	w.statsMu.Lock()
	w.stats.BytesRead += n
	w.statsMu.Unlock()
}

// snapshot returns a copy of the walk counters.
func (w *walkCtx) snapshot() WalkStats {
	w.statsMu.Lock()
	defer w.statsMu.Unlock()
	return w.stats
}

// GOMAXPROCSNote documents why DefaultConcurrency ignores NumCPU. Kept as a
// named reference so a future "optimization" to runtime.NumCPU() is greppable
// and has to argue with this comment.
const GOMAXPROCSNote = "indexing concurrency is I/O- and allocation-bound; " +
	"raising it above DefaultConcurrency raises the live working set against a " +
	"fixed byte budget (runtime.NumCPU is deliberately not used)"

// NumCPUForReport exposes the machine's parallelism for diagnostics only.
func NumCPUForReport() int { return runtime.NumCPU() }
