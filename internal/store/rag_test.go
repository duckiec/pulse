package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"pulse/internal/rag"
	"pulse/internal/store"
)

func openRAG(t *testing.T) *store.Store {
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

// TestRAGSchemaSurvivesTheSecretAudit proves the RAG tables are subject to the
// same no-credential-columns rule as the history schema. A code index holds
// source, and source holds whatever its author typed.
func TestRAGSchemaSurvivesTheSecretAudit(t *testing.T) {
	s := openRAG(t)
	if err := s.AuditSchema(context.Background()); err != nil {
		t.Fatalf("RAG schema failed the secret audit: %v", err)
	}
	cols, err := s.Columns(context.Background())
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	var sawChunks, sawFTS bool
	for _, c := range cols {
		if store.ViolatesSecretPolicy(c.Name) {
			t.Errorf("RAG column %q trips the secret policy", c.Name)
		}
		if c.Table == "rag_chunks" {
			sawChunks = true
		}
	}
	if !sawChunks {
		t.Fatal("rag_chunks did not appear in sqlite_master")
	}
	// The FTS table is a virtual table; its shadow tables must exist.
	names := map[string]bool{}
	for _, c := range cols {
		names[c.Table] = true
	}
	for _, want := range []string{"rag_fts_data", "rag_fts_idx", "rag_fts_docsize"} {
		if !names[want] {
			t.Errorf("FTS5 shadow table %q missing; FTS5 is not really active", want)
		}
	}
	_ = sawFTS
}

// TestFTSQueryCannotBeInjected is the gate for the single most likely way this
// feature breaks: a stack trace handed verbatim to FTS5's MATCH. FTS5 query
// syntax uses quotes, colons, parentheses, asterisks and barewords, and every
// one of them appears in real error text.
func TestFTSQueryCannotBeInjected(t *testing.T) {
	s := openRAG(t)
	ctx := context.Background()
	emb := rag.NewHashingEmbedder()

	chunks := []store.RagChunk{
		mkChunk("internal/handler.go", 10, 20, "ServeHTTP",
			"func ServeHTTP(w http.ResponseWriter, r *http.Request) {\n\thandleUsers(r)\n}"),
		mkChunk("internal/db.go", 5, 12, "queryOrders",
			"func queryOrders(db *sql.DB, uid int64) error { return db.Query(\"orders\") }"),
	}
	for i := range chunks {
		chunks[i].ID = rag.ID(chunks[i].Path, chunks[i].StartLn, chunks[i].EndLn)
		v, sc, err := emb.Embed(ctx, chunks[i].Symbol+" "+chunks[i].Text)
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		chunks[i].Vec, chunks[i].Scale = v, sc
	}
	if _, err := s.UpsertRAGChunks(ctx, chunks); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Every one of these is a realistic 500 body fragment.
	hostile := []string{
		`ERROR: undefined method 'foo' for nil:NilClass`,
		`column "user_id" does not exist`,
		`FTS5 syntax error near "NEAR"`,
		`unexpected token ) AND ( OR NOT`,
		`{"error": {"message": "boom"}}`,
		`panic: runtime error: index out of range [5] with length 3`,
		`"* wildcard prefix query`,
		`NEAR(a b, 5) OR "unbalanced`,
	}
	for _, q := range hostile {
		hits, err := s.Search(ctx, q, nil, 3)
		if err != nil {
			t.Errorf("query %q returned an error instead of results: %v", q, err)
			continue
		}
		if len(hits) == 0 {
			t.Logf("query %q matched nothing (acceptable: OR recall with no common tokens)", q)
		}
	}
}

// TestFTSQuerySyntaxErrorIsDetectable guards the guard: BuildFTSQuery must
// produce something FTS5 accepts. If it stopped quoting, the hostile-query test
// above would pass vacuously by every query erroring identically.
func TestFTSQuerySyntaxErrorIsDetectable(t *testing.T) {
	s := openRAG(t)
	ctx := context.Background()
	c := mkChunk("a.go", 1, 2, "f", "func f() { g() }")
	c.ID = rag.ID(c.Path, c.StartLn, c.EndLn)
	if _, err := s.UpsertRAGChunks(ctx, []store.RagChunk{c}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// A term that EXISTS must be findable; that proves the built expression
	// parsed rather than silently matching nothing.
	hits, err := s.Search(ctx, "func", nil, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("a term present in the corpus returned no hits; the MATCH expression is malformed")
	}
}

// TestHybridRetrievalFindsTheKnownChunk is the recall fixture: a query naming a
// symbol must rank that symbol's own chunk first.
func TestHybridRetrievalFindsTheKnownChunk(t *testing.T) {
	s := openRAG(t)
	ctx := context.Background()
	emb := rag.NewHashingEmbedder()

	corpus := []store.RagChunk{
		mkChunk("internal/user/list.go", 1, 10, "ListUsers",
			"func ListUsers(ctx context.Context, db *sql.DB) ([]User, error) { return nil, nil }"),
		mkChunk("internal/user/create.go", 1, 10, "CreateUser",
			"func CreateUser(ctx context.Context, db *sql.DB, u User) error { return nil }"),
		mkChunk("internal/order/refund.go", 1, 10, "RefundOrder",
			"func RefundOrder(ctx context.Context, db *sql.DB, id int64) error { return nil }"),
		mkChunk("internal/order/list.go", 1, 10, "ListOrders",
			"func ListOrders(ctx context.Context, db *sql.DB) ([]Order, error) { return nil, nil }"),
	}
	for i := range corpus {
		corpus[i].ID = rag.ID(corpus[i].Path, corpus[i].StartLn, corpus[i].EndLn)
		v, sc, _ := emb.Embed(ctx, corpus[i].Symbol+" "+corpus[i].Text)
		corpus[i].Vec, corpus[i].Scale = v, sc
	}
	if _, err := s.UpsertRAGChunks(ctx, corpus); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	q := "NoMethodError: undefined method 'ListUsers' for nil:NilClass"
	vec, _, err := emb.Embed(ctx, q)
	if err != nil {
		t.Fatalf("embed query: %v", err)
	}
	hits, err := s.Search(ctx, q, vec, 3)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits for a query naming a symbol that exists in the corpus")
	}
	if hits[0].Symbol != "ListUsers" {
		t.Errorf("top hit is %q, want ListUsers (rank 1 was %s:%d)",
			hits[0].Symbol, hits[0].Path, hits[0].StartLn)
	}
	if len(hits) > 1 && hits[1].Symbol == "ListUsers" {
		t.Error("ListUsers appeared twice; chunk ids are not unique per span")
	}
}

// TestVectorLegOrdersByCosine is the gate for the M30 boundary.
//
// The vector leg's RANK is what Reciprocal Rank Fusion consumes; the score's
// magnitude is not, because RRF uses 1/(k+rank) and never looks at the number
// behind the rank. That makes the vector leg's ORDERING the load-bearing
// behaviour, and ordering is what this gate pins.
//
// It also records the two mutations that turn out NOT to be defects here.
// Changing cosine's divisor, or clamping the divisor to 127*127, are both
// arithmetically no-ops: quantize rescales every vector to L2 length exactly
// 127, so |a|·|b| already equals 127². A gate that went red for either would be
// asserting an arithmetic identity rather than a behaviour, and would eventually
// fail when the quantizer's target length legitimately changed.
func TestVectorLegOrdersByCosine(t *testing.T) {
	s := openRAG(t)
	ctx := context.Background()

	// Two chunks whose ids order one way and whose similarity orders the other.
	// The id sort must NOT be what decides.
	near := store.RagChunk{
		ID: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", Path: "near.go",
		Lang: "go", StartLn: 1, EndLn: 2, Symbol: "Near",
		Text: "func Near() {}", Vec: []int8{100, 100, 0, 0},
	}
	far := store.RagChunk{
		ID: "0000000000000000000000000000000000000000000000000000000000000000", Path: "far.go",
		Lang: "go", StartLn: 1, EndLn: 2, Symbol: "Far",
		Text: "func Far() {}", Vec: []int8{-100, -100, 0, 0},
	}
	if _, err := s.UpsertRAGChunks(ctx, []store.RagChunk{near, far}); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	query := []int8{100, 100, 0, 0}
	hits, err := s.Search(ctx, "near far", query, 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) < 2 {
		t.Fatalf("got %d hits, want at least 2", len(hits))
	}
	// The query is identical to near's vector, so near must outrank far even
	// though its id sorts LAST.
	if hits[0].Symbol != "Near" {
		t.Errorf("top hit is %q (id %s…), want Near: the vector leg is ordering by id, not similarity",
			hits[0].Symbol, hits[0].ID[:8])
	}
	if hits[1].Symbol != "Far" {
		t.Errorf("second hit is %q, want Far", hits[1].Symbol)
	}
	// The scores must actually separate, or RRF is receiving a tie and the
	// ordering above is incidental.
	if hits[0].Score <= hits[1].Score {
		t.Errorf("scores did not separate: %v vs %v", hits[0].Score, hits[1].Score)
	}
}

// TestReciprocalRankFusionPrefersAgreement is the fusion gate. RRF must reward
// a document BOTH signals rank highly over one that only one signal loves.
func TestReciprocalRankFusionPrefersAgreement(t *testing.T) {
	both := store.RRFForTest(1, 1)
	lexicalOnly := store.RRFForTest(1, 0)
	vectorOnly := store.RRFForTest(0, 1)
	neither := store.RRFForTest(0, 0)

	if !(both > lexicalOnly) || !(both > vectorOnly) {
		t.Errorf("a chunk ranked by both signals must outrank one ranked by a single signal: both=%g lex=%g vec=%g",
			both, lexicalOnly, vectorOnly)
	}
	if lexicalOnly == neither {
		t.Error("a lexical hit scored the same as nothing; the lexical signal contributes zero")
	}
	if vectorOnly == neither {
		t.Error("a vector hit scored the same as nothing; the vector signal contributes zero")
	}
	// Rank 1 must beat rank 2, or the k constant is not doing its job.
	if store.RRFForTest(1, 0) <= store.RRFForTest(2, 0) {
		t.Error("RRF rank 1 does not outrank rank 2")
	}
}

// TestLexicalOnlySearchIsHonestAboutBeingDegraded proves the no-model path is a
// first-class outcome: results are returned, and the caller can tell they came
// from one signal.
func TestLexicalOnlySearchIsHonestAboutBeingDegraded(t *testing.T) {
	s := openRAG(t)
	ctx := context.Background()
	c := mkChunk("a.go", 1, 3, "handler", "func handler(w http.ResponseWriter) { panic(\"nope\") }")
	c.ID = rag.ID(c.Path, c.StartLn, c.EndLn)
	if _, err := s.UpsertRAGChunks(ctx, []store.RagChunk{c}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	hits, err := s.Search(ctx, "handler panic", nil, 3)
	if err != nil {
		t.Fatalf("lexical-only search must not error: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("lexical-only search returned nothing for a term in the corpus")
	}
	if len(hits[0].Sources) != 1 || hits[0].Sources[0] != "fts" {
		t.Errorf("sources = %v, want [fts]; a lexical-only result must not claim vector agreement",
			hits[0].Sources)
	}
	if hits[0].VectorRank != 0 {
		t.Errorf("VectorRank = %d, want 0 when no vector was supplied", hits[0].VectorRank)
	}
	plan := store.PlanRAGQuery("handler panic", false)
	if !plan.FTSOnly {
		t.Error("PlanRAGQuery reported a hybrid plan for a query with no vector")
	}
}

// TestReindexIsIdempotent proves the upsert path does not duplicate: the same
// corpus indexed twice must yield the same chunk count.
func TestReindexIsIdempotent(t *testing.T) {
	s := openRAG(t)
	ctx := context.Background()
	chunks := []store.RagChunk{
		mkChunk("a.go", 1, 5, "one", "func one() {}"),
		mkChunk("b.py", 1, 5, "two", "def two():\n    pass"),
	}
	for i := range chunks {
		chunks[i].ID = rag.ID(chunks[i].Path, chunks[i].StartLn, chunks[i].EndLn)
	}
	for pass := 0; pass < 3; pass++ {
		if _, err := s.UpsertRAGChunks(ctx, chunks); err != nil {
			t.Fatalf("pass %d upsert: %v", pass, err)
		}
	}
	st, err := s.RagStats(ctx)
	if err != nil {
		t.Fatalf("RagStats: %v", err)
	}
	if st.Chunks != 2 {
		t.Errorf("chunks = %d after three identical passes, want 2", st.Chunks)
	}
	if st.Files != 2 {
		t.Errorf("files = %d, want 2", st.Files)
	}
}

// TestStaleFTSRowsAreReplaced proves the external-content FTS table is kept in
// step with the content table. An UPDATE that left the old terms in the index
// would make a renamed symbol match forever.
func TestStaleFTSRowsAreReplaced(t *testing.T) {
	s := openRAG(t)
	ctx := context.Background()

	old := mkChunk("a.go", 1, 5, "oldName", "func oldName() { return 1 }")
	old.ID = rag.ID(old.Path, old.StartLn, old.EndLn)
	if _, err := s.UpsertRAGChunks(ctx, []store.RagChunk{old}); err != nil {
		t.Fatalf("upsert old: %v", err)
	}
	if hits, err := s.Search(ctx, "oldName", nil, 3); err != nil {
		t.Fatalf("search old: %v", err)
	} else if len(hits) == 0 {
		t.Fatal("the original symbol is not findable after indexing")
	}

	// Same id (same path and span), different text and symbol.
	updated := mkChunk("a.go", 1, 5, "newName", "func newName() { return 2 }")
	updated.ID = old.ID
	if _, err := s.UpsertRAGChunks(ctx, []store.RagChunk{updated}); err != nil {
		t.Fatalf("upsert updated: %v", err)
	}

	if hits, err := s.Search(ctx, "oldName", nil, 3); err != nil {
		t.Fatalf("search stale: %v", err)
	} else if len(hits) != 0 {
		t.Errorf("the replaced symbol still matches (%d hits); FTS holds a stale row", len(hits))
	}
	if hits, err := s.Search(ctx, "newName", nil, 3); err != nil {
		t.Fatalf("search new: %v", err)
	} else if len(hits) == 0 {
		t.Error("the new symbol is not findable after reindex")
	}
}

// TestUpsertIsBoundedPerTransaction proves the write path is batched. A single
// transaction for a whole index would hold the write lock for the entire run;
// the batch constant is what prevents that.
func TestUpsertIsBoundedPerTransaction(t *testing.T) {
	if store.IndexBatch <= 0 {
		t.Fatal("IndexBatch must be positive; an unbounded transaction wedges readers")
	}
	if store.IndexBatch > 4096 {
		t.Errorf("IndexBatch = %d is large enough to hold the write lock long enough to stall a running Pulse",
			store.IndexBatch)
	}
	s := openRAG(t)
	ctx := context.Background()

	// More chunks than one batch, so the loop actually iterates.
	var chunks []store.RagChunk
	for i := 0; i < store.IndexBatch+7; i++ {
		c := mkChunk(
			"gen/f"+itoa(i)+".go",
			1, 5, "sym"+itoa(i), "func sym"+itoa(i)+"() {}")
		c.ID = rag.ID(c.Path, c.StartLn, c.EndLn)
		chunks = append(chunks, c)
	}
	n, err := s.UpsertRAGChunks(ctx, chunks)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if n != len(chunks) {
		t.Errorf("wrote %d of %d chunks", n, len(chunks))
	}
}

// TestCosineMatchesTheRAGPackage pins the duplicated cosine implementations to
// agree. internal/store cannot import internal/rag (the dependency flow is
// one-directional), so the function is written twice and this gate is what
// stops the copies drifting into returning different rankings.
func TestCosineMatchesTheRAGPackage(t *testing.T) {
	emb := rag.NewHashingEmbedder()
	ctx := context.Background()

	texts := []string{
		"func ListUsers(ctx context.Context) error { return nil }",
		"func CreateUser(ctx context.Context) error { return nil }",
		"completely unrelated prose about gardening",
	}
	vecs := make([][]int8, len(texts))
	for i, text := range texts {
		v, _, err := emb.Embed(ctx, text)
		if err != nil {
			t.Fatalf("embed %d: %v", i, err)
		}
		vecs[i] = v
	}
	for i := range vecs {
		for j := range vecs {
			a := store.CosineInt8(vecs[i], vecs[j])
			b := rag.CosineInt8(vecs[i], vecs[j])
			if a != b {
				t.Errorf("cosine(%d,%d): store=%v rag=%v; the duplicated implementations disagree",
					i, j, a, b)
			}
		}
	}
	// Identical text must be its own nearest neighbour.
	if store.CosineInt8(vecs[0], vecs[0]) < 0.99 {
		t.Errorf("a vector is not self-similar: cos(v,v) = %v; normalization is broken",
			store.CosineInt8(vecs[0], vecs[0]))
	}
}

// TestEmptyQueryIsAnErrorNotAnEmptyResult prevents a silent no-op. A query with
// no usable terms must say so rather than return zero rows, which is
// indistinguishable from "the index is empty".
func TestEmptyQueryIsAnErrorNotAnEmptyResult(t *testing.T) {
	s := openRAG(t)
	if _, err := s.Search(context.Background(), "   ***   ", nil, 3); err == nil {
		t.Error("a query with no terms returned no error; that reads as an empty index")
	}
}

// TestDeleteByFileRemovesBothLegs proves deletion cleans the FTS index too.
func TestDeleteByFileRemovesBothLegs(t *testing.T) {
	s := openRAG(t)
	ctx := context.Background()
	c := mkChunk("gone.go", 1, 4, "vanishMe", "func vanishMe() {}")
	c.ID = rag.ID(c.Path, c.StartLn, c.EndLn)
	if _, err := s.UpsertRAGChunks(ctx, []store.RagChunk{c}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	n, err := s.DeleteRAGChunksByFile(ctx, "gone.go")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want 1", n)
	}
	if hits, err := s.Search(ctx, "vanishMe", nil, 3); err != nil {
		t.Fatalf("search after delete: %v", err)
	} else if len(hits) != 0 {
		t.Errorf("a deleted chunk is still findable (%d hits); the FTS row leaked", len(hits))
	}
}

// TestRAGMetaRecordsProvenance proves the embedder name is recorded, so an index
// built without a model is identifiable later instead of looking complete.
func TestRAGMetaRecordsProvenance(t *testing.T) {
	s := openRAG(t)
	ctx := context.Background()
	if err := s.SetRAGMeta(ctx, "embedder", "structural-hash-v1"); err != nil {
		t.Fatalf("SetRAGMeta: %v", err)
	}
	got, err := s.GetRAGMeta(ctx, "embedder")
	if err != nil {
		t.Fatalf("GetRAGMeta: %v", err)
	}
	if got != "structural-hash-v1" {
		t.Errorf("embedder = %q, want structural-hash-v1", got)
	}
	if v, err := s.GetRAGMeta(ctx, "absent"); err != nil || v != "" {
		t.Errorf("an absent key must return \"\" and no error; got %q, %v", v, err)
	}
	st, err := s.RagStats(ctx)
	if err != nil {
		t.Fatalf("RagStats: %v", err)
	}
	if st.Embedder != "structural-hash-v1" {
		t.Errorf("RagStats.Embedder = %q; provenance is not reaching the stats report", st.Embedder)
	}
}

// TestVectorizedStatsAreReportedSeparately proves a degraded index is
// distinguishable from a complete one.
func TestVectorizedStatsAreReportedSeparately(t *testing.T) {
	s := openRAG(t)
	ctx := context.Background()

	withVec := mkChunk("v.go", 1, 4, "withvec", "func withvec() {}")
	withVec.ID = rag.ID(withVec.Path, withVec.StartLn, withVec.EndLn)
	withVec.Vec = []int8{1, 2, 3}
	without := mkChunk("n.go", 1, 4, "novec", "func novec() {}")
	without.ID = rag.ID(without.Path, without.StartLn, without.EndLn)

	if _, err := s.UpsertRAGChunks(ctx, []store.RagChunk{withVec, without}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	st, err := s.RagStats(ctx)
	if err != nil {
		t.Fatalf("RagStats: %v", err)
	}
	if st.Chunks != 2 || st.Vectorized != 1 || st.LexicalOnly != 1 {
		t.Errorf("stats = %+v; want 2 chunks, 1 vectorized, 1 lexical-only", st)
	}
}

// helpers

// mkChunk builds a store-shaped chunk. The store persists a value and does not
// reinterpret it, so the test constructs exactly the type it persists rather
// than relying on a conversion the production path does not use.
func mkChunk(path string, start, end int, symbol, text string) store.RagChunk {
	return store.RagChunk{
		Path: path, Lang: "go", StartLn: start, EndLn: end,
		Symbol: symbol, Text: text,
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
