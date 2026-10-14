package rag

import (
	"context"

	"pulse/internal/store"
)

// finalizeOne is the single authoritative chunk-finalization step.
//
// ID generation, secret redaction, and vector embedding exist HERE and nowhere
// else: the bulk walker (walkCtx.finalize) and the incremental watcher
// (prepareChunks/HandleEvent) are thin adapters around it, so watcher-written
// chunks agree byte-for-byte with walker-written ones (Candidate C2).
//
// Rules, in order:
//  1. ID is computed from POSITION (path + span), never content, so scrubbing
//     the text cannot change the ID and cause a rewrite storm.
//  2. Redaction happens BEFORE embedding: an embedding of a secret is a secret
//     in a form nothing downstream can redact.
//  3. The embedding input is Symbol + " " + Path + "\n" + Text (redacted).
//
// It returns (hasVec, redacted).
func finalizeOne(ctx context.Context, c *Chunk, embedder Embedder, redact func(string) string) (bool, bool) {
	c.MakeID()
	redacted := false
	if redact != nil {
		before := c.Text
		c.Text = redact(c.Text)
		redacted = c.Text != before
	}
	if embedder == nil || !embedder.Available() {
		return false, redacted
	}
	if err := ctx.Err(); err != nil {
		return false, redacted
	}
	vec, scale, err := embedder.Embed(ctx, c.Symbol+" "+c.Path+"\n"+c.Text)
	if err != nil || len(vec) == 0 {
		c.Vec, c.Scale = nil, 0
		return false, redacted
	}
	c.Vec, c.Scale = vec, scale
	return true, redacted
}

// prepareChunks is the single authoritative chunk-to-row seam.
//
// It chunks src, finalizes every chunk through finalizeOne (ID, redaction,
// embedding), and maps to store rows. Both the bulk walker path and the
// incremental watcher path produce rows through this function, so the two can
// never drift in ID rules, redaction placement, or embedding-input shape.
// An empty src or an unsupported language yields an error; a ctx cancellation
// degrades embedding to lexical-only per chunk rather than failing the file.
func prepareChunks(ctx context.Context, path, lang string, src []byte, embedder Embedder, redact func(string) string) ([]store.RagChunk, error) {
	chunks, err := chunkFile(path, lang, src)
	if err != nil {
		return nil, err
	}
	stored := make([]store.RagChunk, 0, len(chunks))
	for i := range chunks {
		c := &chunks[i]
		finalizeOne(ctx, c, embedder, redact)
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
	return stored, nil
}
