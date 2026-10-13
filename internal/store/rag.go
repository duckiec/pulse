package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// This file is the CGO-free answer to "vector search in SQLite".
//
// # Why there is no vector extension
//
// sqlite-vec, vss, and every other SQLite vector extension are C libraries.
// AGENTS.md §2.1 makes CGO_ENABLED=0 mandatory for every platform and every CI
// job, and §2.4 names modernc.org/sqlite as the only acceptable driver. There
// is therefore no vector extension, no ANN index, and no HNSW — not as a
// performance trade-off, but as a hard constraint of the build.
//
// # What replaces it
//
// Two independent signals, fused:
//
//	lexical     FTS5 (compiled into modernc.org/sqlite) — bm25-ranked
//	structural  brute-force cosine over int8 vectors, computed in Go
//
// FTS5 finds exact tokens; cosine finds shape. Neither subsumes the other, so
// neither is trusted alone: results are combined by Reciprocal Rank Fusion,
// which merges two RANKINGS rather than two scores and therefore needs no
// calibration between an unbounded bm25 and a cosine in [-1,1]. RRF's k=60
// constant comes from Cormack et al. and flattens the head of either ranking so
// a document that both signals like does not monopolise the top.
//
// The cost is O(N·d) for the cosine leg with d=256 int8 — ~256 multiply-adds
// per candidate. At the ROADMAP's 50k-chunk ceiling that is ~12.8M operations,
// which is why `Search` bounds the candidate set with the FTS5 leg first
// (MEMORY.md ADL-0021).

// ragSchema is applied by InitRAG, not by Open.
//
// It is NOT in the main schema constant on purpose. The main schema is applied
// unconditionally when the history database opens, so putting FTS5 there would
// make the whole application unopenable on a driver build without FTS5 — for a
// feature that is optional and that reports DEGRADED honestly. A user who never
// indexes a codebase should not be unable to run Pulse at all.
const ragSchema = `
CREATE TABLE IF NOT EXISTS rag_chunks (
    id        TEXT PRIMARY KEY,
    path      TEXT NOT NULL,
    lang      TEXT NOT NULL,
    start_ln  INTEGER NOT NULL,
    end_ln    INTEGER NOT NULL,
    symbol    TEXT NOT NULL DEFAULT '',
    text      TEXT NOT NULL,
    vec       BLOB,
    scale     REAL NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_rag_chunks_path ON rag_chunks(path);

CREATE VIRTUAL TABLE IF NOT EXISTS rag_fts USING fts5(
    text,
    symbol,
    path,
    content='rag_chunks',
    content_rowid='rowid'
);

CREATE TABLE IF NOT EXISTS rag_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// ErrFTSUnavailable reports a driver build without FTS5. It is a distinct
// sentinel so the caller can report DEGRADED rather than crash.
var ErrFTSUnavailable = errors.New("store: this sqlite build has no FTS5 support")

// rrfK is the Reciprocal Rank Fusion damping constant. The standard value from
// Cormack et al. 2009; it controls how sharply rank 1 outweighs rank 10, and 60
// is the number that behaves well across heterogeneous rankers.
const rrfK = 60

// maxQueryTerms bounds how many OR'd terms reach FTS5. A 4 KB stack trace
// tokenizes to hundreds of terms; feeding them all to MATCH produces a query
// planner cost that dwarfs the work it saves, and the tail terms are the
// least informative ones anyway.
const maxQueryTerms = 32

// maxFTSCandidates is the lexical leg's depth before fusion.
const maxFTSCandidates = 200

// RagChunk is one indexed source span.
//
// The field names mirror internal/rag.Chunk deliberately: the store persists a
// value, it does not reinterpret it, and a mapping layer between two identical
// shapes is a place for a field to be quietly dropped.
type RagChunk struct {
	ID      string
	Path    string
	Lang    string
	StartLn int
	EndLn   int
	Symbol  string
	Text    string
	// Vec is the int8 vector, or nil when the chunk is lexical-only.
	Vec []int8
	// Scale converts Vec back to float space.
	Scale float32
}

// RagHit is one ranked result.
type RagHit struct {
	// ID is the chunk id. It is not serialized: an id is an internal handle
	// derived from path and span, and an agent gains nothing by seeing it.
	ID      string  `json:"-"`
	Path    string  `json:"path"`
	StartLn int     `json:"start_ln"`
	EndLn   int     `json:"end_ln"`
	Symbol  string  `json:"symbol"`
	Lang    string  `json:"lang"`
	Text    string  `json:"text"`
	Score   float64 `json:"score"`
	// LexicalRank and VectorRank are 1-based positions in their own ranking, or
	// 0 when that signal did not place the chunk. Surfacing them is what makes
	// a fused result auditable: a user can see whether the top hit was found by
	// exact-token match, by shape, or both.
	LexicalRank int `json:"lexical_rank,omitempty"`
	VectorRank  int `json:"vector_rank,omitempty"`
	// Sources lists which signals contributed: "fts", "vec", or both.
	Sources []string `json:"sources"`
}

// ErrEmptyQuery is returned when a search has nothing to search for.
var ErrEmptyQuery = errors.New("store: empty query")

// InitRAG creates the RAG tables and FTS5 index, verifying FTS5 is usable.
//
// The FTS5 probe is a real CREATE, not a compile-option lookup: a build can
// advertise the flag and still fail to instantiate the module, and discovering
// that at first query time would mean the user's first 500 explanation fails
// with a syntax error instead of at index time with a clear message.
func (s *Store) InitRAG(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, ragSchema); err != nil {
		if isNoSuchModule(err) {
			return fmt.Errorf("%w: %w", ErrFTSUnavailable, err)
		}
		return fmt.Errorf("store: apply RAG schema: %w", err)
	}
	// Prove bm25() is callable; it is part of the module surface we depend on.
	var score float64
	err := s.db.QueryRowContext(ctx,
		`SELECT bm25(rag_fts) FROM rag_fts WHERE rag_fts MATCH ? LIMIT 1`, "pulse").Scan(&score)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		if isNoSuchModule(err) || strings.Contains(err.Error(), "no such function") {
			return fmt.Errorf("%w: bm25() is unavailable: %w", ErrFTSUnavailable, err)
		}
		return fmt.Errorf("store: probe FTS5 bm25: %w", err)
	}
	// The RAG schema is subject to the same no-credential-columns audit as the
	// history schema. A code index contains source; a source file contains
	// whatever its author put in it.
	if err := s.AuditSchema(ctx); err != nil {
		return fmt.Errorf("store: RAG schema failed the secret audit: %w", err)
	}
	return nil
}

func isNoSuchModule(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "no such module") ||
		strings.Contains(msg, "fts5") && strings.Contains(msg, "not available")
}

// RagHasFTS5 reports whether this database supports RAG at all.
func (s *Store) RagHasFTS5(ctx context.Context) bool {
	_, err := s.db.ExecContext(ctx,
		`CREATE VIRTUAL TABLE IF NOT EXISTS rag_fts_probe USING fts5(x)`)
	if err != nil {
		return false
	}
	_, _ = s.db.ExecContext(ctx, `DROP TABLE IF EXISTS rag_fts_probe`)
	return true
}

// IndexBatch bounds one write transaction.
//
// A transaction is opened per batch rather than for the whole index, which is
// the entire reason indexing does not wedge a running Pulse: a reader on the
// same database (an MCP session mid-explanation, the TUI reading history) only
// ever waits for the CURRENT batch, and a batch is milliseconds of work. One
// transaction for 50k chunks would hold the write lock for minutes.
const IndexBatch = 256

// UpsertRAGChunks writes chunks in bounded transactions.
//
// Each chunk's write and its FTS5 row are in the SAME transaction. Splitting
// them would leave a window where a chunk is queryable by vector but invisible
// to lexical search, or the reverse — an index that silently disagrees with
// itself depending on which leg you ask.
func (s *Store) UpsertRAGChunks(ctx context.Context, chunks []RagChunk) (int, error) {
	if len(chunks) == 0 {
		return 0, nil
	}
	written := 0
	now := time.Now().UnixMilli()

	for start := 0; start < len(chunks); start += IndexBatch {
		end := min(start+IndexBatch, len(chunks))
		if err := ctx.Err(); err != nil {
			return written, err
		}
		if err := s.upsertRAGBatch(ctx, chunks[start:end], now); err != nil {
			return written, err
		}
		written += end - start
	}
	return written, nil
}

func (s *Store) upsertRAGBatch(ctx context.Context, batch []RagChunk, now int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin RAG batch: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// Delete the previous FTS row for each id before reinserting. With
	// content='rag_chunks' an external-content table is NOT auto-updated, and
	// an UPDATE on the content table leaves the index holding the OLD terms —
	// which would make a stale symbol match a query forever.
	del, err := tx.PrepareContext(ctx, `DELETE FROM rag_fts WHERE rowid = (SELECT rowid FROM rag_chunks WHERE id = ?)`)
	if err != nil {
		return fmt.Errorf("store: prepare RAG fts delete: %w", err)
	}
	defer del.Close()

	ins, err := tx.PrepareContext(ctx,
		`INSERT INTO rag_chunks (id, path, lang, start_ln, end_ln, symbol, text, vec, scale, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   path=excluded.path, lang=excluded.lang, start_ln=excluded.start_ln,
		   end_ln=excluded.end_ln, symbol=excluded.symbol, text=excluded.text,
		   vec=excluded.vec, scale=excluded.scale, updated_at=excluded.updated_at`)
	if err != nil {
		return fmt.Errorf("store: prepare RAG chunk insert: %w", err)
	}
	defer ins.Close()

	insFts, err := tx.PrepareContext(ctx,
		`INSERT INTO rag_fts (rowid, text, symbol, path) VALUES ((SELECT rowid FROM rag_chunks WHERE id = ?), ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("store: prepare RAG fts insert: %w", err)
	}
	defer insFts.Close()

	for _, c := range batch {
		if c.ID == "" {
			return fmt.Errorf("store: RAG chunk has no id (path %q)", c.Path)
		}
		if _, err := del.ExecContext(ctx, c.ID); err != nil {
			return fmt.Errorf("store: clear stale FTS row for %s: %w", c.ID, err)
		}
		var vecBlob any
		if len(c.Vec) > 0 {
			vecBlob = encodeInt8(c.Vec)
		}
		if _, err := ins.ExecContext(ctx, c.ID, c.Path, c.Lang, c.StartLn, c.EndLn,
			c.Symbol, c.Text, vecBlob, float64(c.Scale), now); err != nil {
			return fmt.Errorf("store: upsert RAG chunk %s: %w", c.ID, err)
		}
		if _, err := insFts.ExecContext(ctx, c.ID, c.Text, c.Symbol, c.Path); err != nil {
			return fmt.Errorf("store: index RAG chunk %s for FTS: %w", c.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit RAG batch: %w", err)
	}
	return nil
}

// encodeInt8 packs a vector for storage. A []byte of the raw values is used
// rather than base64 so a vector is 256 bytes on disk instead of 344, and so
// the blob can be read straight into a scan buffer with no decode step.
func encodeInt8(v []int8) []byte {
	b := make([]byte, len(v))
	for i, x := range v {
		b[i] = byte(x) // two's complement round-trips through int8(byte)
	}
	return b
}

func decodeInt8(b []byte) []int8 {
	if len(b) == 0 {
		return nil
	}
	v := make([]int8, len(b))
	for i, x := range b {
		v[i] = int8(x)
	}
	return v
}

// Search runs hybrid retrieval and returns up to limit fused hits.
//
// queryVec may be nil: the lexical leg then runs alone and the result says so
// via Sources. That is the DEGRADED path (no embedding model), and it is a
// first-class outcome rather than an error — a lexical explanation is a worse
// explanation, not a wrong one.
func (s *Store) Search(ctx context.Context, query string, queryVec []int8, limit int) ([]RagHit, error) {
	if limit <= 0 {
		limit = 10
	}
	terms := queryTerms(query)
	if len(terms) == 0 {
		return nil, ErrEmptyQuery
	}

	fused := make(map[string]*fusedScore)
	ids := make([]string, 0, maxFTSCandidates)

	// ---- leg 1: lexical (FTS5, bm25) ----
	ftsHits, ftsErr := s.searchFTS(ctx, terms, maxFTSCandidates)
	for i, h := range ftsHits {
		f := fused[h.ID]
		if f == nil {
			f = &fusedScore{hit: h}
			fused[h.ID] = f
			ids = append(ids, h.ID)
		}
		f.lexicalRank = i + 1
		f.lexicalScore = h.Score
	}
	if ftsErr != nil && !errors.Is(ftsErr, ErrEmptyQuery) {
		return nil, ftsErr
	}

	// ---- leg 2: structural (brute-force cosine) ----
	var vecHits []vectorHit
	if len(queryVec) > 0 {
		var err error
		vecHits, err = s.searchVector(ctx, queryVec, maxFTSCandidates)
		if err != nil {
			return nil, err
		}
		for i, vh := range vecHits {
			f := fused[vh.id]
			if f == nil {
				// Fill the hit from the chunk row so a vector-only result still
				// carries its text and location.
				f = &fusedScore{hit: RagHit{ID: vh.id}}
				fused[vh.id] = f
				ids = append(ids, vh.id)
			}
			f.vectorRank = i + 1
			f.vectorScore = float64(vh.score)
		}
	}

	if len(ids) == 0 {
		return nil, nil
	}

	// ---- fusion ----
	type scored struct {
		hit RagHit
	}
	out := make([]scored, 0, len(ids))
	for _, id := range ids {
		f := fused[id]
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// RRF: sum 1/(k+rank). Absent signal contributes 0, not a penalty —
		// a document found by only one leg should not be pushed below one
		// found by neither, because there is no "neither".
		var s float64
		if f.lexicalRank > 0 {
			s += 1.0 / float64(rrfK+f.lexicalRank)
		}
		if f.vectorRank > 0 {
			s += 1.0 / float64(rrfK+f.vectorRank)
		}
		h := f.hit
		h.Score = s
		h.LexicalRank = f.lexicalRank
		h.VectorRank = f.vectorRank
		h.Sources = sourcesFor(f)
		out = append(out, scored{hit: h})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].hit.Score != out[j].hit.Score {
			return out[i].hit.Score > out[j].hit.Score
		}
		// Deterministic tie-break on id. Without this, two equally-scored
		// chunks swap places between identical queries and the retrieval
		// benchmark reports nondeterminism that is not the index's fault but
		// is indistinguishable from it.
		return out[i].hit.ID < out[j].hit.ID
	})

	if len(out) > limit {
		out = out[:limit]
	}
	hits := make([]RagHit, 0, len(out))
	for _, o := range out {
		hits = append(hits, o.hit)
	}
	return hits, nil
}

func sourcesFor(f *fusedScore) []string {
	switch {
	case f.lexicalRank > 0 && f.vectorRank > 0:
		return []string{"fts", "vec"}
	case f.lexicalRank > 0:
		return []string{"fts"}
	case f.vectorRank > 0:
		return []string{"vec"}
	}
	return nil
}

type fusedScore struct {
	hit          RagHit
	lexicalRank  int
	vectorRank   int
	lexicalScore float64
	vectorScore  float64
}

type vectorHit struct {
	id    string
	score float32
}

// searchFTS runs the lexical leg.
//
// The MATCH expression is BUILT, never passed through: a stack trace contains
// `"`, `*`, `:`, `(`, `-` and the barewords AND/OR/NOT/NEAR, all of which are
// FTS5 syntax. Handing a user-supplied error message to MATCH verbatim turns
// every 500 whose body happens to contain a quote into a query syntax error —
// i.e. the explanation feature fails precisely when it is needed. Every term
// is therefore extracted as a bare identifier and re-emitted as a quoted
// phrase, which cannot contain syntax.
func (s *Store) searchFTS(ctx context.Context, terms []string, limit int) ([]RagHit, error) {
	expr := BuildFTSQuery(terms)
	if expr == "" {
		return nil, ErrEmptyQuery
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, c.path, c.lang, c.start_ln, c.end_ln, c.symbol, c.text,
		        bm25(rag_fts)
		 FROM rag_fts
		 JOIN rag_chunks c ON c.rowid = rag_fts.rowid
		 WHERE rag_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`, expr, limit)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "fts5: syntax error") {
			// Should be unreachable given BuildFTSQuery, but a query that
			// errors is better reported than silently returned empty.
			return nil, fmt.Errorf("store: FTS query rejected: %w", err)
		}
		return nil, fmt.Errorf("store: FTS search: %w", err)
	}
	defer rows.Close()

	var out []RagHit
	for rows.Next() {
		var h RagHit
		if err := rows.Scan(&h.ID, &h.Path, &h.Lang, &h.StartLn, &h.EndLn,
			&h.Symbol, &h.Text, &h.Score); err != nil {
			return nil, fmt.Errorf("store: scan FTS row: %w", err)
		}
		// bm25 returns a NEGATIVE number, lower (more negative) = better.
		// Negate it so "higher is better" holds for both legs and the fused
		// score is not the only place that direction matters.
		h.Score = -h.Score
		out = append(out, h)
	}
	return out, rows.Err()
}

// searchVector runs the structural leg: a bounded full scan with an int8 dot
// product per row.
//
// A full scan rather than an ANN index because there is no ANN index available
// (no CGO), and because at the ROADMAP's ceiling it is fast enough that the
// complexity of a bespoke approximate structure would not pay for itself.
func (s *Store) searchVector(ctx context.Context, q []int8, limit int) ([]vectorHit, error) {
	if len(q) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, vec FROM rag_chunks WHERE vec IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: vector scan: %w", err)
	}
	defer rows.Close()

	type acc struct {
		id    string
		score float32
	}
	var all []acc
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, fmt.Errorf("store: scan vector row: %w", err)
		}
		v := decodeInt8(blob)
		if len(v) == 0 {
			continue
		}
		all = append(all, acc{id: id, score: CosineInt8(q, v)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].id < all[j].id
	})
	if len(all) > limit {
		all = all[:limit]
	}
	out := make([]vectorHit, 0, len(all))
	for _, a := range all {
		out = append(out, vectorHit{id: a.id, score: a.score})
	}
	return out, nil
}

// CosineInt8 returns the cosine similarity of two L2-normalized int8 vectors.
//
// Duplicated here rather than imported from internal/rag because store is a
// LOWER layer: the dependency flow in ARCHITECTURE.md §3 is one-directional
// and rag depends on nothing in store, so the reverse edge would be a cycle.
// The two implementations are pinned to agree by
// TestCosineMatchesTheRAGPackage.
func CosineInt8(a, b []int8) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb int32
	for i := range a {
		av, bv := int32(a[i]), int32(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	// Divide by |a|·|b|, NOT by 127². An int8 vector renormalized to L2 length
	// 127 does not have components reaching 127 — they average about
	// 127/sqrt(d), so at d=256 a typical component is ~8. Dividing by the
	// maximum possible product reports self-similarity near 1/sqrt(d) instead
	// of 1 and squashes every genuine ranking difference into a band near zero.
	v := float32(float64(dot) / (math.Sqrt(float64(na)) * math.Sqrt(float64(nb))))
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// queryTerms extracts FTS5-safe terms from arbitrary text.
//
// Extraction, not escaping: only [A-Za-z0-9_] runs survive, FTS5's own bareword
// alphabet. That is strictly safer than escaping, because there is no character
// in the output that could be reinterpreted as syntax, whatever the input was.
func queryTerms(query string) []string {
	var out []string
	seen := make(map[string]bool, 64)
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() >= 2 && !seen[cur.String()] { // single chars match everything
			seen[cur.String()] = true
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	for _, r := range strings.ToLower(query) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	if len(out) > maxQueryTerms {
		// Keep the LONGEST terms: they are the most specific and the least
		// likely to be noise. Truncating the head would drop exactly the
		// identifier the stack trace was pointing at.
		sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
		out = out[:maxQueryTerms]
		sort.Strings(out)
	}
	return out
}

// BuildFTSQuery turns terms into a safe FTS5 MATCH expression.
//
// Terms are emitted as quoted phrases joined by OR. OR rather than AND because
// a stack trace's tokens are mostly absent from any one source file, and AND
// would return zero rows exactly when the identifier matters most. Ranking is
// bm25's job; the query's job is recall.
func BuildFTSQuery(terms []string) string {
	if len(terms) == 0 {
		return ""
	}
	var b strings.Builder
	for i, t := range terms {
		if i > 0 {
			b.WriteString(" OR ")
		}
		b.WriteByte('"')
		// Reject a term containing a quote outright rather than trying to
		// escape it. Extraction already guarantees this cannot happen; the
		// check makes the guarantee local instead of distributed.
		if strings.ContainsAny(t, "\"\x00") {
			continue
		}
		b.WriteString(t)
		b.WriteByte('"')
	}
	if b.Len() == 0 {
		return ""
	}
	return b.String()
}

// RagStats summarizes the index.
type RagStats struct {
	Chunks      int
	Vectorized  int
	LexicalOnly int
	Files       int
	Embedder    string
}

// RagStats reads the current index state. The embedder name is stored so a
// DEGRADED index built without a model is identifiable later, instead of
// looking like a complete one that simply happens to have no vectors.
func (s *Store) RagStats(ctx context.Context) (RagStats, error) {
	var out RagStats
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*),
		        COALESCE(SUM(CASE WHEN vec IS NOT NULL THEN 1 ELSE 0 END), 0),
		        COUNT(DISTINCT path)
		 FROM rag_chunks`).Scan(&out.Chunks, &out.Vectorized, &out.Files); err != nil {
		return out, fmt.Errorf("store: RAG stats: %w", err)
	}
	out.LexicalOnly = out.Chunks - out.Vectorized
	name, err := s.GetRAGMeta(ctx, "embedder")
	if err == nil {
		out.Embedder = name
	}
	return out, nil
}

// SetRAGMeta records an index provenance value.
func (s *Store) SetRAGMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rag_meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("store: set rag_meta %s: %w", key, err)
	}
	return nil
}

// GetRAGMeta reads an index provenance value, returning "" when absent.
func (s *Store) GetRAGMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM rag_meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get rag_meta %s: %w", key, err)
	}
	return v, nil
}

// DeleteRAGChunksByFile removes every chunk of a path. Used by the incremental
// reindex path when a file is deleted or shrinks.
func (s *Store) DeleteRAGChunksByFile(ctx context.Context, path string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("store: begin rag delete: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx, `SELECT rowid FROM rag_chunks WHERE path = ?`, path)
	if err != nil {
		return 0, fmt.Errorf("store: select rag rows for %s: %w", path, err)
	}
	var rowids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("store: scan rag rowid: %w", err)
		}
		rowids = append(rowids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	delFts, err := tx.PrepareContext(ctx, `DELETE FROM rag_fts WHERE rowid = ?`)
	if err != nil {
		return 0, fmt.Errorf("store: prepare rag fts delete: %w", err)
	}
	defer delFts.Close()
	for _, id := range rowids {
		if _, err := delFts.ExecContext(ctx, id); err != nil {
			return 0, fmt.Errorf("store: delete rag fts row %d: %w", id, err)
		}
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM rag_chunks WHERE path = ?`, path)
	if err != nil {
		return 0, fmt.Errorf("store: delete rag chunks for %s: %w", path, err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit rag delete: %w", err)
	}
	return int(n), nil
}

// RagQueryPlan reports what a query would cost, for the DEGRADED report.
type RagQueryPlan struct {
	Terms     int
	HasVector bool
	FTSOnly   bool
}

// PlanRAGQuery reports how a query will be executed, without executing it.
func PlanRAGQuery(query string, hasVector bool) RagQueryPlan {
	terms := len(queryTerms(query))
	return RagQueryPlan{
		Terms:     terms,
		HasVector: hasVector,
		FTSOnly:   !hasVector || terms == 0,
	}
}

// RRFForTest exposes the fused-score arithmetic to the gate.
//
// It is exported rather than duplicated in the test because a test that
// re-implements the formula tests its own copy, not the shipped one.
func RRFForTest(lexicalRank, vectorRank int) float64 {
	var s float64
	if lexicalRank > 0 {
		s += 1.0 / float64(rrfK+lexicalRank)
	}
	if vectorRank > 0 {
		s += 1.0 / float64(rrfK+vectorRank)
	}
	return s
}
