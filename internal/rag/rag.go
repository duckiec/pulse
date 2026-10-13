// Package rag indexes a local codebase so a failing API response can be
// explained with the backend code that most likely produced it.
//
// # Scope and honesty
//
// This is a lexical-and-structural retrieval system, not a semantic one. It
// chunks source by FUNCTIONAL boundary (a Go function, a Python method, an
// Astro component block) rather than by arbitrary line counts, and it ranks
// with two independent signals fused by Reciprocal Rank Fusion:
//
//	lexical   FTS5 (SQLite) — exact token match, bm25-ranked
//	structural a deterministic hashed projection of the chunk (see embed.go)
//
// Neither signal is a neural embedding, and the package never pretends
// otherwise. Where a real embedding model would go is spelled out in
// embed.go; when it is absent the system reports DEGRADED and says so in
// every result rather than fabricating vectors.
//
// # Offline-first
//
// Nothing here reaches the network. No model is fetched at runtime, no
// telemetry, no update check (AGENTS.md §1, invariant I-3). The index is a
// local file and a local database, or it is nothing.
//
// # Memory discipline
//
// The walker's live working set is bounded by construction, not by hope:
// concurrency is capped, one file is open at a time per worker, chunk text is
// capped, and the generator is LAZY — a consumer that discards chunks causes
// them to be collected rather than accumulated. A 1,000-file corpus indexes
// without resident growth exceeding the 2 MiB pipeline budget (indexer_test.go,
// TestMemoryCeilingRSSOverAThousandFileCorpus).
package rag

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
)

// Chunk is one retrievable unit of source code.
//
// The zero value is not a chunk: StartLn is 1-based so that a chunk's span is
// meaningful when quoted back to a human (a 0-based line number in an editor is
// off by one, and an error message that points one line above the defect is
// worse than one that points nowhere).
type Chunk struct {
	// ID is sha256(path + ":" + start + "-" + end), lowercase hex. It is a
	// CONTENT-ADDRESSED id derived from position, not from content, so an
	// unchanged chunk keeps its id across rebuilds and a changed one does not
	// silently keep its row (MEMORY.md ADL-0021).
	ID string

	// Path is repo-relative and slash-separated, so an index built on Windows
	// addresses the same chunk as one built on Linux.
	Path string

	// Lang is the language tag: "go", "py", "astro".
	Lang string

	// StartLn is the 1-based line of the chunk's first line.
	StartLn int

	// EndLn is the 1-based line of the chunk's last line.
	EndLn int

	// Symbol is the enclosing declaration name when one is known
	// ("(*Server).handleUser" is fine; empty is fine too).
	Symbol string

	// Text is the redacted source span. It is redacted BEFORE it is ever
	// offered to a store or an agent, not on the way out (AGENTS.md §3.4).
	Text string

	// Vec is the quantized structural vector, L2-normalized before
	// quantization. Empty means "this chunk has no vector" and the hybrid
	// search falls back to the lexical signal for it.
	Vec []int8

	// Scale converts Vec back to float space: real ≈ float32(Vec[i]) * Scale.
	// Stored per chunk because a per-vector scale quantizes far better than a
	// global one at negligible cost (4 bytes).
	Scale float32
}

// ID computes the canonical chunk id.
func ID(path string, start, end int) string {
	h := sha256.New()
	h.Write([]byte(path))
	h.Write([]byte(":"))
	h.Write([]byte(strconv.Itoa(start)))
	h.Write([]byte("-"))
	h.Write([]byte(strconv.Itoa(end)))
	return hex.EncodeToString(h.Sum(nil))
}

// MakeID returns the id for a chunk's span, filling ID if empty.
func (c *Chunk) MakeID() {
	if c.ID == "" {
		c.ID = ID(c.Path, c.StartLn, c.EndLn)
	}
}

// Bytes reports the chunk's text size, for budget accounting.
func (c *Chunk) Bytes() int { return len(c.Text) }

// ErrCorruptChunk marks a chunk that cannot be interpreted at all (an empty
// span, a missing path). It is distinct from ErrUnsupportedLang, which means
// "we know this language and cannot chunk it".
var ErrCorruptChunk = errors.New("rag: chunk has no source span")

// CorpusError reports a whole-file failure: the file was found but could not be
// turned into chunks. Indexing is not fatal for this — a single unparseable
// file must not abort a repository-wide index — so callers receive it as an
// `iter.Seq2[Chunk, error]` yield and decide.
type CorpusError struct {
	Path string
	Err  error
}

func (e *CorpusError) Error() string {
	return fmt.Sprintf("rag: %s: %v", e.Path, e.Err)
}

func (e *CorpusError) Unwrap() error { return e.Err }

// Lang tags the languages the chunkers understand.
const (
	LangGo    = "go"
	LangPy    = "py"
	LangAstro = "astro"
)

// ChunkFileForTest chunks a buffer directly, without walking a directory.
//
// It exists so the chunking heuristics can be gated in isolation. The chunker
// is unexported because production reaches it only through the walk; exporting
// it for the gates would widen the package's surface for a test-only reason.
func ChunkFileForTest(rel, lang string, src []byte) ([]Chunk, error) {
	return chunkFile(rel, lang, src)
}

// ParseIgnoreForTest parses a .gitignore body without a filesystem.
//
// Same reasoning as ChunkFileForTest: the matcher is a pure function, and the
// gate should exercise it as one.
func ParseIgnoreForTest(body string) *ignoreSet {
	return parseIgnoreText(body)
}

// LangOf reports the language tag for a path, or "" if the walker should not
// read it at all. A file with no tag is not an error: it is simply not source
// code, and the index must not fill with binary blobs and lockfiles.
func LangOf(path string) string {
	switch {
	case hasSuffixFold(path, ".go"):
		// _test.go files are indexed: a failing test names the function under
		// test, and a handler's fixture is often more informative than its
		// implementation.
		return LangGo
	case hasSuffixFold(path, ".py"):
		return LangPy
	case hasSuffixFold(path, ".astro"):
		return LangAstro
	}
	return ""
}
