package rag_test

// The 50k-chunk gate (ROADMAP P3.2.9): a physical index at the roadmap's
// ceiling — 50,000 chunks in SQLite FTS5 plus the int8 vector table —
// ingested through the REAL pipeline (walker, chunker, hash embedder,
// batched upserts), then queried with hybrid top-K retrieval.
//
// Three assertions, each against a documented budget:
//
//  1. Ingest retention: the walker's PeakBuf high-water mark stays within
//     the 2 MiB pipeline budget across the whole 50k ingest.
//  2. Retrieval latency: nearest-rank p99 over 101 hybrid queries < 50ms.
//  3. Retrieval retention: post-GC heap growth across the query phase stays
//     within 2 MiB, proving the O(N·d) cosine scan is transient, not held.
//
// The corpus is fully deterministic (no randomness, no network), so the gate
// is reproducible machine to machine; only the latency leg depends on clock
// speed, and 50ms against a millisecond-scale scan leaves an order of
// magnitude of headroom. Under the race detector the clock measures
// instrumentation overhead instead of product latency, so that leg reports
// its p99 without enforcing the budget there; the native suite enforces it.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"pulse/internal/rag"
	"pulse/internal/store"
)

// Gate geometry: 1,100 files × 50 functions = 55,000 declarations, so the
// chunk count clears 50,000 even if the chunker merges some spans.
const (
	gateFiles        = 1100
	gateFuncsPerFile = 50
	gateMinChunks    = 50000
	gateP99Budget    = 50 * time.Millisecond
	gateMemBudget    = 2 << 20 // 2 MiB, the documented RAG pipeline budget
	gateQueries      = 101
)

// gatePoolWords gives every file a distinctive mid-selectivity token: file f
// carries pool[f%len(pool)], so a query naming it matches ~1/64 of the
// corpus. That is the realistic middle between a unique symbol (one match)
// and a universal word (every match) — the shape of a genuine retrieval
// query, which names something specific, not something ubiquitous.
//
// A corpus where EVERY token is universal would make every query score the
// whole index in FTS5, and no top-K FTS can rank without scoring its
// candidates: such queries measure full-index bm25, not retrieval. The gate
// queries the system the way the system is used.
var gatePoolWords = []string{
	"ledger", "quorum", "beacon", "harbor", "cipher", "meadow", "anvil", "comet",
	"drift", "ember", "fjord", "glacier", "helmet", "ivory", "jungle", "kelp",
	"lantern", "magnet", "needle", "onyx", "prism", "quartz", "raven", "saddle",
	"tundra", "umbra", "vortex", "willow", "xenon", "yarrow", "zephyr", "acorn",
	"basalt", "cinder", "dune", "elm", "flint", "grove", "heath", "inlet",
	"juniper", "krill", "lumen", "marble", "nomad", "opal", "pampas", "quill",
	"ridge", "sorrel", "thicket", "upland", "verge", "wren", "yew", "zinc",
	"amber", "birch", "coral", "dahlia", "egret", "fern", "garnet", "heron",
}

// gatePool returns file f's pool word.
func gatePool(f int) string { return gatePoolWords[f%len(gatePoolWords)] }

// gateCorpus writes the deterministic corpus and returns its root.
func gateCorpus(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	var sb strings.Builder
	for f := 0; f < gateFiles; f++ {
		sb.Reset()
		pool := gatePool(f)
		fmt.Fprintf(&sb, "package pkg%04d\n", f)
		for k := 0; k < gateFuncsPerFile; k++ {
			name := fmt.Sprintf("Alpha%04d%02d", f, k)
			// The name appears in the signature, the comment, and the body so
			// both the lexical leg (FTS over text/symbol) and the structural
			// leg (identifier morphology) can find it; the pool word gives
			// the file a mid-selectivity token.
			fmt.Fprintf(&sb, "\n// %s reconciles the %s ledger.\nfunc %s() int {\n\tsentinel%s := %d\n\treturn sentinel%s\n}\n",
				name, pool, name, name, f*100+k, name)
		}
		// Files fan out across package dirs to keep directory reads bounded.
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("pkg%04d", f)), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("pkg%04d/f%04d.go", f, f)),
			[]byte(sb.String()), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return root
}

// gateIndex builds the 50k index through the real pipeline and returns the
// open store, the chunk count, and the walker's peak buffer mark.
func gateIndex(t *testing.T, ctx context.Context, root string) (*store.Store, int, int) {
	t.Helper()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "gate50k.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.InitRAG(ctx); err != nil {
		t.Fatalf("InitRAG: %v", err)
	}
	var buf []store.RagChunk
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if _, err := st.UpsertRAGChunks(ctx, buf); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		buf = buf[:0]
	}
	res, err := rag.IndexContext(ctx, root, rag.Options{Embedder: rag.NewHashingEmbedder()},
		func(c rag.Chunk, err error) bool {
			if err != nil {
				t.Errorf("index yield: %v", err)
				return true
			}
			buf = append(buf, store.RagChunk{
				ID: c.ID, Path: c.Path, Lang: c.Lang,
				StartLn: c.StartLn, EndLn: c.EndLn,
				Symbol: c.Symbol, Text: c.Text,
				Vec: c.Vec, Scale: c.Scale,
			})
			if len(buf) >= store.IndexBatch {
				flush()
			}
			return true
		})
	if err != nil {
		t.Fatalf("IndexContext: %v", err)
	}
	flush()
	return st, res.Chunks, res.Stats.PeakBuf
}

// TestGate50kChunkLatencyAndMemory is the P3.2.9 gate.
func TestGate50kChunkLatencyAndMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("NOT RUN (reason: 50k gate needs the full corpus)")
	}
	ctx := context.Background()
	root := gateCorpus(t)

	ingestStart := time.Now()
	st, chunks, peak := gateIndex(t, ctx, root)
	ingestDur := time.Since(ingestStart)
	t.Logf("ingest: %d chunks in %v (%.0f chunks/s), walker PeakBuf %d bytes",
		chunks, ingestDur, float64(chunks)/ingestDur.Seconds(), peak)

	if chunks < gateMinChunks {
		t.Fatalf("index holds %d chunks, want >= %d: the corpus does not reach the gate floor", chunks, gateMinChunks)
	}
	if peak > gateMemBudget {
		t.Errorf("walker PeakBuf %d bytes over the %d-byte (2 MiB) pipeline budget during 50k ingest",
			peak, gateMemBudget)
	}

	emb := rag.NewHashingEmbedder()
	lats := make([]float64, 0, gateQueries)
	var ftsLeg, vecLeg int
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := 0; i < gateQueries; i++ {
		// Cycle deterministically across the corpus so every query names a
		// real symbol plus its file's pool word: a unique term joined with
		// a mid-selectivity one, the shape of a genuine retrieval query.
		f := (i * 37) % gateFiles
		q := fmt.Sprintf("Alpha%04d%02d %s", f, (i*13)%gateFuncsPerFile, gatePool(f))
		qvec, _, err := emb.Embed(ctx, q)
		if err != nil {
			t.Fatalf("embed query %d: %v", i, err)
		}
		qStart := time.Now()
		hits, err := st.Search(ctx, q, qvec, 10)
		lats = append(lats, float64(time.Since(qStart).Nanoseconds()))
		if err != nil {
			t.Fatalf("search %d: %v", i, err)
		}
		if len(hits) == 0 {
			t.Fatalf("query %d (%q) returned no hits at 50k scale", i, q)
		}
		for _, h := range hits {
			if h.LexicalRank > 0 {
				ftsLeg++
				break
			}
		}
		for _, h := range hits {
			if h.VectorRank > 0 {
				vecLeg++
				break
			}
		}
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	sort.Float64s(lats)
	// Nearest-rank p99: the smallest latency L such that 99% of samples are
	// <= L. Over 101 samples that is the 100th (index 99) — one cold sample
	// is tolerated, two is not.
	p99 := time.Duration(lats[(gateQueries*99+99)/100-1]) * time.Nanosecond
	p50 := time.Duration(lats[gateQueries/2]) * time.Nanosecond
	t.Logf("retrieval over %d chunks, %d queries: p50=%v p99=%v max=%v (budget %v)",
		chunks, gateQueries, p50, p99, time.Duration(lats[gateQueries-1])*time.Nanosecond, gateP99Budget)
	t.Logf("hybrid legs alive: fts=%d/%d queries, vec=%d/%d queries", ftsLeg, gateQueries, vecLeg, gateQueries)
	if underRaceDetector {
		t.Logf("latency leg NOT RUN (reason: race detector owns the clock; p99=%v measures instrumentation, the %v budget binds native runs)", p99, gateP99Budget)
	} else if p99 >= gateP99Budget {
		t.Errorf("p99 retrieval latency %v >= %v budget at %d chunks", p99, gateP99Budget, chunks)
	}
	if ftsLeg == 0 {
		t.Error("lexical leg placed zero hits across all queries: the hybrid is vector-only")
	}
	if vecLeg == 0 {
		t.Error("vector leg placed zero hits across all queries: the hybrid is lexical-only")
	}
	growth := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	t.Logf("retrieval retention: post-GC heap growth %d bytes across %d queries (budget %d)",
		growth, gateQueries, gateMemBudget)
	if growth > gateMemBudget {
		t.Errorf("query phase retained %d bytes, over the %d-byte budget: the scan holds, not streams", growth, gateMemBudget)
	}
}
