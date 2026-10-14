package rag

import (
	"context"
	"testing"
)

// TestPrepareChunksParity proves the C2 unification: rows produced through the
// single prepareChunks seam agree byte-for-byte with rows produced through the
// bulk walker's finalize path, for identical input.
func TestPrepareChunksParity(t *testing.T) {
	ctx := context.Background()
	src := []byte("package main\n\n// Foo adds one.\nfunc Foo(x int) int { return x + 1 }\n\nfunc Bar() int { return Foo(41) }\n")
	rel := "main.go"
	emb := NewHashingEmbedder()

	// Walker path: chunkFile + walkCtx.finalize.
	raw, err := chunkFile(rel, LangGo, src)
	if err != nil {
		t.Fatalf("chunkFile: %v", err)
	}
	w := &walkCtx{}
	o := Options{Embedder: emb}
	for i := range raw {
		w.finalize(ctx, &raw[i], o)
	}

	// Single-seam path.
	stored, err := prepareChunks(ctx, rel, LangGo, src, emb, nil)
	if err != nil {
		t.Fatalf("prepareChunks: %v", err)
	}
	if len(stored) != len(raw) {
		t.Fatalf("row count differs: seam=%d walker=%d", len(stored), len(raw))
	}
	for i := range stored {
		a, b := stored[i], raw[i]
		if a.ID != b.ID || a.Path != b.Path || a.Lang != b.Lang ||
			a.StartLn != b.StartLn || a.EndLn != b.EndLn ||
			a.Symbol != b.Symbol || a.Text != b.Text || a.Scale != b.Scale {
			t.Fatalf("row %d differs:\nseam:   %+v\nwalker: %+v", i, a, b)
		}
		if len(a.Vec) != len(b.Vec) {
			t.Fatalf("row %d vec len differs: %d vs %d", i, len(a.Vec), len(b.Vec))
		}
		for j := range a.Vec {
			if a.Vec[j] != b.Vec[j] {
				t.Fatalf("row %d vec[%d] differs", i, j)
			}
		}
	}
}

// TestPrepareChunksRedactsBeforeEmbed proves redaction placement: the secret
// is scrubbed from Text and never reaches the embedding input, and the ID is
// position-derived so scrubbing cannot change it.
func TestPrepareChunksRedactsBeforeEmbed(t *testing.T) {
	ctx := context.Background()
	src := []byte("package main\n\nfunc Leak() string { return \"token-SECRET-123\" }\n")
	redact := func(s string) string {
		out := s
		for _, secret := range []string{"token-SECRET-123"} {
			repl := "[REDACTED]"
			for i := 0; i+len(secret) <= len(out); i++ {
				if out[i:i+len(secret)] == secret {
					out = out[:i] + repl + out[i+len(secret):]
					break
				}
			}
		}
		return out
	}
	plain, err := prepareChunks(ctx, "leak.go", LangGo, src, NewHashingEmbedder(), nil)
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	scrubbed, err := prepareChunks(ctx, "leak.go", LangGo, src, NewHashingEmbedder(), redact)
	if err != nil {
		t.Fatalf("scrubbed: %v", err)
	}
	if len(plain) == 0 || len(scrubbed) == 0 {
		t.Fatal("expected chunks")
	}
	for _, c := range scrubbed {
		for _, needle := range []string{"SECRET"} {
			for i := 0; i+len(needle) <= len(c.Text); i++ {
				if c.Text[i:i+len(needle)] == needle {
					t.Fatalf("secret survived redaction in %q", c.Text)
				}
			}
		}
	}
	// IDs are position-derived: identical with and without redaction.
	for i := range plain {
		if plain[i].ID != scrubbed[i].ID {
			t.Fatalf("redaction changed ID on row %d", i)
		}
	}
	// Embedding consumed the redacted text: vectors must differ from the
	// unredacted run (the secret bits hashed into numbers would otherwise).
	same := true
	for i := range plain {
		if len(plain[i].Vec) != len(scrubbed[i].Vec) {
			same = false
			break
		}
		for j := range plain[i].Vec {
			if plain[i].Vec[j] != scrubbed[i].Vec[j] {
				same = false
				break
			}
		}
	}
	if same {
		t.Fatal("vectors identical with and without redaction: embedding may have run before redaction")
	}
}
