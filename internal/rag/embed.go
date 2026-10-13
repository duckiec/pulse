package rag

import (
	"context"
	"errors"
	"hash/fnv"
	"math"
	"strings"
	"sync"
)

// Embedder turns text into a quantized vector.
//
// # Why the interface exists
//
// ROADMAP.md §3.2.2 specifies a pinned MiniLM-class ONNX model at
// `$PULSE_HOME/models/embd-int8.onnx`, executed by a pure-Go ONNX runtime, and
// requires that a missing model report Unavailable so the system degrades to
// lexical-only search "honestly (never fake vectors)". That contract is the
// reason this is an INTERFACE and not a function: the model path is a
// deployment decision that needs a model artifact fetched out of band and
// verified by SHA-256, and Pulse must not download it at runtime (invariant
// I-3, offline-first).
//
// What ships today is HashingEmbedder: a deterministic, dependency-free
// structural projection. It is a real vector — not random noise, not a
// constant — and it captures identifier morphology (camelCase, snake_case,
// dotted paths) and token overlap, which is genuinely informative for code.
// It is NOT a semantic embedding, and every result it produces is labelled as
// structural so no consumer can mistake it for one.
type Embedder interface {
	// Embed returns the quantized vector and its scale. The scale converts
	// back to float space: real ≈ float32(v[i]) * scale.
	Embed(ctx context.Context, text string) (vec []int8, scale float32, err error)
	// Name identifies the embedding in provenance and DEGRADED reports.
	Name() string
	// Available reports whether Embed can succeed. A false here MUST be
	// surfaced by callers as DEGRADED, never swallowed.
	Available() bool
}

// ErrModelUnavailable is returned by an Embedder whose model artifact is
// absent or unreadable. It is the signal that drives the DEGRADED path.
var ErrModelUnavailable = errors.New("rag: embedding model unavailable")

// VectorDim is the width of every vector Pulse produces.
//
// 256 is chosen against the ROADMAP's retrieval budget: cosine over int8 at
// d=256 costs 256 multiply-adds per chunk, so a full scan of 50k chunks is
// ~12.8M operations — interactive without SIMD, and 4x cheaper than the
// 1024 a small transformer would emit.
const VectorDim = 256

// HashingEmbedder is the shipped pure-Go embedder: a signed feature-hashing
// projection over identifier-shaped tokens and character trigrams.
//
// Properties that matter and are tested:
//
//	deterministic  identical text → identical vector, always
//	normalized     L2-normalized before quantization, so cosine is a plain dot
//	cheap          no model load, no allocation per dimension, no I/O
type HashingEmbedder struct {
	dim int
}

// NewHashingEmbedder returns the structural embedder.
func NewHashingEmbedder() *HashingEmbedder { return &HashingEmbedder{dim: VectorDim} }

// Name implements Embedder.
func (h *HashingEmbedder) Name() string { return "structural-hash-v1" }

// Available implements Embedder. Always true: there is no artifact to be
// missing. The DEGRADED path exists for an Embedder that DOES need one.
func (h *HashingEmbedder) Available() bool { return true }

// Embed implements Embedder.
func (h *HashingEmbedder) Embed(ctx context.Context, text string) ([]int8, float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	// Whitespace-only input has no features and must produce NO vector rather
	// than a trigram-only one derived from indentation. An index embedding the
	// whitespace between two functions would make every pair of adjacent chunks
	// look similar for no reason.
	if strings.TrimSpace(text) == "" {
		return nil, 0, nil
	}
	dim := h.dim
	if dim <= 0 {
		dim = VectorDim
	}

	// Two passes over one reusable buffer: the vector is accumulated in
	// float32, then quantized. This is the ONLY per-embed allocation and it is
	// sized once, so a chunk costs dim*4 bytes transiently rather than a
	// map of counts.
	acc := make([]float32, dim)

	// The three feature families are accumulated into SEPARATE vectors and then
	// combined with fixed weights. Merging them into one accumulator lets
	// whichever family is longer dominate the norm: a 60-line function body
	// produces hundreds of trigrams and drowns out its own name — and the name
	// is precisely what a stack trace supplies. Each family is normalized before
	// it is combined, so no family's length can swamp the others.
	var subAcc, triAcc []float32

	for _, tok := range IdentifierTokens(text) {
		addFeature(acc, dim, "t:"+tok, 1.0+0.25*float32(mini(len(tok), 24)))
		// Identifier morphology matters for code: `parseHTTPResponse` must be
		// findable by a stack trace that says "parse http response".
		if parts := splitIdentifier(tok); len(parts) > 1 {
			if subAcc == nil {
				subAcc = make([]float32, dim)
			}
			for _, p := range parts {
				addFeature(subAcc, dim, "w:"+p, 1.0)
			}
		}
	}

	// Character trigrams make the vector robust to segmentation differences and
	// to error text that names a symbol with punctuation attached
	// (`undefined method 'foo'` vs `"foo"`).
	lower := strings.ToLower(text)
	if tri := trigrams(lower); len(tri) > 0 {
		triAcc = make([]float32, dim)
		for _, g := range tri {
			addFeature(triAcc, dim, "g:"+g, 0.35)
		}
	}

	combine(acc, subAcc, 0.60) // whole tokens dominate: they are the unit of meaning
	combine(acc, triAcc, 0.25) // trigrams are a robustness aid, not a signal

	return quantize(acc)
}

// combine adds src*weight into dst after normalizing src to unit length, so the
// weight means the same thing regardless of how many features src contributed.
func combine(dst, src []float32, weight float32) {
	if src == nil {
		return
	}
	var sum float64
	for _, v := range src {
		sum += float64(v) * float64(v)
	}
	n := math.Sqrt(sum)
	if n == 0 {
		return
	}
	k := weight / float32(n)
	for i := range src {
		dst[i] += src[i] * k
	}
}

// addFeature hashes a feature name to a dimension with a SIGN, which is the
// signed feature-hashing trick: the sign keeps unrelated features that collide
// from systematically inflating each other, so the collision noise cancels
// instead of accumulating.
func addFeature(acc []float32, dim int, name string, weight float32) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	sum := h.Sum64()
	idx := int(sum % uint64(dim))
	// The high bit decides the sign. Using a different bit than the index keeps
	// sign and position uncorrelated.
	if sum&(1<<63) != 0 {
		acc[idx] -= weight
	} else {
		acc[idx] += weight
	}
}

// quantize L2-normalizes then quantizes to int8 with a per-vector scale.
//
// Normalizing BEFORE quantizing is what makes a single scale safe: after L2
// normalization every component is in [-1,1], so one scale maps the whole
// vector without clipping the large components to 127 and destroying their
// relative weights.
func quantize(acc []float32) ([]int8, float32, error) {
	var sum float64
	for _, v := range acc {
		sum += float64(v) * float64(v)
	}
	norm := math.Sqrt(sum)
	if norm == 0 {
		// An empty or featureless text. Returning a ZERO vector would be a lie
		// that ranks as "maximally dissimilar"; returning nil says "no vector",
		// and the hybrid search falls back to the lexical signal honestly.
		return nil, 0, nil
	}

	// Two stages, and the order is load-bearing:
	//
	//  1. NORMALIZE in float space. After this every component is in [-1,1]
	//     and maxAbs is the largest of them.
	//  2. QUANTIZE with scale = maxAbs/127, so the largest component lands
	//     exactly on ±127 and nothing clips.
	//
	// Computing the scale from the PRE-normalization magnitudes (the obvious
	// first attempt) and then dividing by the norm as well divides twice, which
	// drives every component below 1 and loses most of the vector's dynamic
	// range to rounding. The result is still self-consistent, so it does not
	// look broken — it just ranks badly, which is harder to notice than an
	// error and worse to ship.
	invNorm := float32(1.0 / norm)
	normalized := make([]float32, len(acc))
	for i, v := range acc {
		normalized[i] = v * invNorm
	}

	var normMax float32
	for _, v := range normalized {
		if a := absf(v); a > normMax {
			normMax = a
		}
	}
	scale := normMax / 127
	if scale == 0 {
		return nil, 0, nil
	}
	invScale := float32(1.0 / scale)

	out := make([]int8, len(normalized))
	for i, v := range normalized {
		q := v * invScale
		if q > 127 {
			q = 127
		} else if q < -127 {
			q = -127
		}
		out[i] = int8(math.Round(float64(q)))
	}

	// Rescale so the STORED vector has L2 length exactly 127, which is the
	// largest length int8 can represent without clipping (127² = 16129 fits an
	// int32 accumulator).
	//
	// The target is 127 and NOT 1. After scaling the unit vector by
	// 127/maxAbs its length is 127/maxAbs, which is always GREATER than 127
	// (maxAbs < 1). Dividing by the vector's own length — the obvious reading
	// of "normalize" — therefore divides by something like 360, rounds every
	// component to zero, and returns an all-zero vector that still passes a
	// length check. Cosine of a zero vector is 0, so every chunk looks maximally
	// dissimilar from every other chunk and the structural leg silently ranks
	// nothing. It is not an error; it is a vector that is quietly useless, which
	// is the failure mode a gate has to be written against.
	var norm2 float64
	for _, v := range out {
		norm2 += float64(v) * float64(v)
	}
	if n := math.Sqrt(norm2); n > 0 {
		k := 127.0 / n
		for i, v := range out {
			q := int8(math.Round(float64(v) * k))
			if q > 127 {
				q = 127
			} else if q < -127 {
				q = -127
			}
			out[i] = q
		}
	}
	return out, scale, nil
}

// CosineInt8 returns the cosine similarity of two normalized int8 vectors.
//
// For L2-normalized vectors cosine IS the dot product, so this needs no
// division and no per-vector norm — the normalization happened once at
// quantize time and is the reason the stored form is int8 at all.
//
// Mismatched widths return 0, not a panic: a vector written by an older
// embedding dimension must degrade to "no structural signal", never take the
// process down on the retrieval path.
func CosineInt8(a, b []int8) float32 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot int32
	for i := range a {
		dot += int32(a[i]) * int32(b[i])
	}
	// The divisor is |a|·|b|, not 127². An int8 vector renormalized to L2
	// length 127 has components that do NOT reach 127 — they average about
	// 127/sqrt(d), so at d=256 a typical component is ~8. Dividing by the
	// maximum possible product therefore reports a self-similarity of roughly
	// 1/sqrt(d) instead of 1, and every genuinely-similar pair is squashed into
	// a narrow band near zero where ranking differences vanish.
	na := int32(0)
	for _, v := range a {
		na += int32(v) * int32(v)
	}
	nb := int32(0)
	for _, v := range b {
		nb += int32(v) * int32(v)
	}
	if na == 0 || nb == 0 {
		return 0
	}
	v := float32(float64(dot) / (math.Sqrt(float64(na)) * math.Sqrt(float64(nb))))
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// ---- unavailable embedder ---------------------------------------------------

// UnavailableEmbedder is the DEGRADED state: an Embedder that is present in
// the wiring but cannot embed, because its model artifact is missing.
//
// It exists so that "the model is not installed" is a FIRST CLASS value rather
// than a nil interface. A nil Embedder and an unavailable one are different
// situations: the first is a caller bug, the second is a deployment state that
// must be REPORTED to the user, not silently treated as "no results".
type UnavailableEmbedder struct {
	// Reason is machine-readable and safe to show: "model artifact not found
	// at <path>" rather than a wrapped filesystem error with a home directory
	// in it.
	Reason string
}

// NewUnavailable returns an UnavailableEmbedder with a reason.
func NewUnavailable(reason string) *UnavailableEmbedder {
	return &UnavailableEmbedder{Reason: reason}
}

// Name implements Embedder.
func (u *UnavailableEmbedder) Name() string { return "unavailable" }

// Available implements Embedder.
func (u *UnavailableEmbedder) Available() bool { return false }

// Embed implements Embedder. It fails closed: an absent model must never yield
// a zero vector, because a zero vector silently becomes "matches nothing" and
// the user is told their code has no similar chunks rather than that the
// feature is not installed.
func (u *UnavailableEmbedder) Embed(context.Context, string) ([]int8, float32, error) {
	return nil, 0, ErrModelUnavailable
}

// ---- tokenizer --------------------------------------------------------------

// IdentifierTokens splits text into the tokens an embedder should see.
//
// It splits on non-alphanumerics AND splits camelCase / PascalCase /
// snake_case / kebab-case, keeping both the whole token and its parts in
// `IdentifierTokens`' consumer. Language identifiers are the unit of meaning in
// a stack trace, so this is where most of the retrieval quality lives.
func IdentifierTokens(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	runes := []rune(text)
	for i, r := range runes {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			// Split on a case boundary, but not inside an ALLCAPS run (HTTPServer
			// is one word, not HTTPS + Server).
			if r >= 'A' && r <= 'Z' && i > 0 {
				p := runes[i-1]
				nxt := rune(0)
				if i+1 < len(runes) {
					nxt = runes[i+1]
				}
				if (p >= 'a' && p <= 'z') || (p >= '0' && p <= '9') ||
					(nxt >= 'a' && nxt <= 'z') {
					flush()
				}
			}
			cur.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// splitIdentifier breaks a token into its constituent words on case and
// underscore boundaries.
func splitIdentifier(tok string) []string {
	parts := strings.FieldsFunc(tok, func(r rune) bool { return r == '_' || r == '-' })
	var out []string
	for _, p := range parts {
		lower := strings.ToLower(p)
		// All-caps runs stay whole: SQL, HTTP, ID.
		if isAllUpper(p) && len(p) > 1 {
			out = append(out, lower)
			continue
		}
		var cur strings.Builder
		runes := []rune(p)
		for i, r := range runes {
			if r >= 'A' && r <= 'Z' && i > 0 {
				if cur.Len() > 0 {
					out = append(out, strings.ToLower(cur.String()))
					cur.Reset()
				}
			}
			cur.WriteRune(r)
		}
		if cur.Len() > 0 {
			out = append(out, strings.ToLower(cur.String()))
		}
	}
	return out
}

func isAllUpper(s string) bool {
	hasLetter := false
	for _, r := range s {
		if r >= 'a' && r <= 'z' {
			return false
		}
		if r >= 'A' && r <= 'Z' {
			hasLetter = true
		}
	}
	return hasLetter
}

// trigrams yields overlapping 3-rune windows. Short inputs yield nothing rather
// than a padded duplicate: a 2-character input has no trigram, and inventing
// one would put noise into every dimension of a very small vector.
func trigrams(s string) []string {
	r := []rune(s)
	if len(r) < 3 {
		return nil
	}
	out := make([]string, 0, len(r)-2)
	for i := 0; i+3 <= len(r); i++ {
		out = append(out, string(r[i:i+3]))
	}
	return out
}

func absf(v float32) float32 {
	if v < 0 {
		return -v
	}
	return v
}

func mini(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// embedOnce guards a shared embedder's warm-up, because the first Embed pays
// for allocator growth and a corpus that embeds concurrently would otherwise
// make that cost depend on scheduling.
type embedOnce struct {
	once sync.Once
	err  error
}
