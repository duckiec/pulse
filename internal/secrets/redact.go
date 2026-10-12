package secrets

import (
	"bytes"
	"net/http"
	"sort"
	"strings"
	"sync"
)

// Placeholder is what a redacted value is replaced with. It is deliberately
// conspicuous: a redacted value must never be mistaken for real data
// (AGENTS.md §3.4).
const Placeholder = "[REDACTED]"

// maxTrackedSecrets bounds the memory a Redactor will hold. The list exists so a
// secret echoed back by a server is scrubbed even when it appears somewhere the
// header-name rules do not cover (a JSON error body, a URL-encoded query, a
// stack trace). It is memory-bound by construction: once full, new registrations
// are DROPPED, not evicted — evicting could remove the secret currently in use
// and let it leak. Dropping only degrades scrubbing of secrets resolved after
// the cap, and the drop is counted so the limitation is never invisible.
const maxTrackedSecrets = 256

// minSecretLen is the shortest value worth tracking. Scrubbing 1–3 character
// strings would mangle unrelated output (an "a" appearing in every response)
// while protecting nothing meaningful.
const minSecretLen = 4

// sensitiveHeaders are header names whose VALUES are always replaced, regardless
// of whether their content was ever registered as a secret (AGENTS.md §3.4).
var sensitiveHeaders = map[string]bool{
	"authorization":             true,
	"proxy-authorization":       true,
	"cookie":                    true,
	"set-cookie":                true,
	"x-api-key":                 true,
	"x-auth-token":              true,
	"x-csrf-token":              true,
	"x-session-token":           true,
	"proxy-authorization-basic": true,
	"www-authenticate":          true,
}

// IsSensitiveHeader reports whether a header's value must always be redacted.
// The name is matched case-insensitively.
func IsSensitiveHeader(name string) bool {
	return sensitiveHeaders[strings.ToLower(strings.TrimSpace(name))]
}

// Redactor is the chokepoint between a resolved secret and any output.
//
// Two layers of defence:
//
//  1. Name-based: every value of a sensitive header is replaced.
//  2. Value-based: every secret handed to Register is scrubbed wherever it
//     turns up — body, URL, error text — because a server that echoes a token
//     back into a JSON error defeats layer 1 alone.
//
// A Redactor is safe for concurrent use.
type Redactor struct {
	mu     sync.RWMutex
	values [][]byte // sorted longest-first, so overlapping secrets scrub fully
	seen   map[string]struct{}

	dropped  int // registrations refused because the cap was reached
	scrubbed int64
	// inPlaceSkipped counts secrets too short for in-place replacement; they
	// are still scrubbed by the allocating paths.
	inPlaceSkipped int
}

// NewRedactor returns an empty redactor. It is ready for use immediately.
func NewRedactor() *Redactor {
	return &Redactor{seen: make(map[string]struct{}, maxTrackedSecrets)}
}

// Register records a resolved secret so it can be scrubbed from any future
// output. Registering the same value twice is a no-op.
//
// Registration happens the instant a value is resolved, BEFORE it is used
// anywhere, so there is no window in which an un-registered secret could reach
// a payload.
func (r *Redactor) Register(secret string) {
	if secret == "" || len(secret) < minSecretLen {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = make(map[string]struct{}, maxTrackedSecrets)
	}
	if _, dup := r.seen[secret]; dup {
		return
	}
	if len(r.values) >= maxTrackedSecrets {
		// Drop, do not evict: evicting could unprotect a secret still in use.
		r.dropped++
		return
	}
	r.seen[secret] = struct{}{}
	r.values = append(r.values, []byte(secret))
	// Longest first so a secret that contains another is replaced whole.
	sort.SliceStable(r.values, func(i, j int) bool {
		return len(r.values[i]) > len(r.values[j])
	})
}

// Dropped reports how many registrations the memory cap refused.
func (r *Redactor) Dropped() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.dropped
}

// MaxTrackedSecretsForTest exposes the memory cap so tests can prove the bound
// instead of hard-coding it.
func MaxTrackedSecretsForTest() int { return maxTrackedSecrets }

// Tracked reports how many secrets are currently registered.
func (r *Redactor) Tracked() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.values)
}

// Scrubbed reports how many values this redactor has replaced.
func (r *Redactor) Scrubbed() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scrubbed
}

// String returns s with every registered secret replaced.
//
// Secrets shorter than 4 bytes are ignored so ordinary text is not mangled.
func (r *Redactor) String(s string) string {
	if s == "" {
		return s
	}
	r.mu.RLock()
	if len(r.values) == 0 {
		r.mu.RUnlock()
		return s
	}
	snapshot := r.values
	r.mu.RUnlock()

	changed := false
	for _, secret := range snapshot {
		if strings.Contains(s, string(secret)) {
			s = strings.ReplaceAll(s, string(secret), Placeholder)
			changed = true
		}
	}
	if changed {
		r.mu.Lock()
		r.scrubbed++
		r.mu.Unlock()
	}
	return s
}

// Bytes is the []byte twin of String. It always returns a new slice when a
// replacement happened, and returns the input untouched otherwise.
func (r *Redactor) Bytes(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	r.mu.RLock()
	if len(r.values) == 0 {
		r.mu.RUnlock()
		return b
	}
	snapshot := r.values
	r.mu.RUnlock()

	out := b
	for _, secret := range snapshot {
		if bytes.Contains(out, secret) {
			if &out[0] == &b[0] {
				// Copy before the first replacement so the caller's payload
				// (a pooled chunk we still own) is never mutated in place.
				out = append([]byte(nil), out...)
			}
			out = bytes.ReplaceAll(out, secret, []byte(Placeholder))
		}
	}
	return out
}

// ScrubBytes compacts b IN PLACE, replacing every tracked secret with
// Placeholder, and returns the new length and the number of replacements.
//
// In place matters: the streaming path scrubs response bodies inside the
// engine's own 32 KiB chunk, and a copy there would defeat the memory ceiling
// that chunk discipline exists to enforce.
//
// The compaction SHRINKS the payload (Placeholder is 10 bytes), so a secret must
// be at least that long to be replaceable in place. Shorter secrets cannot be:
// writing 10 bytes over 6 would run off the end of the buffer. Those are
// skipped here — and counted in InPlaceSkipped — rather than silently mangling
// the payload. They remain fully covered by String/Bytes, which allocate.
func (r *Redactor) ScrubBytes(b []byte) (newLen int, replacements int) {
	if len(b) == 0 {
		return 0, 0
	}
	r.mu.RLock()
	snapshot := r.values
	r.mu.RUnlock()
	if len(snapshot) == 0 {
		return len(b), 0
	}

	// Only secrets that fit the placeholder can be replaced in place.
	eligible := make([][]byte, 0, len(snapshot))
	for _, s := range snapshot {
		if len(s) >= len(Placeholder) {
			eligible = append(eligible, s)
		} else {
			r.mu.Lock()
			r.inPlaceSkipped++
			r.mu.Unlock()
		}
	}
	if len(eligible) == 0 {
		return len(b), 0
	}

	out, i := 0, 0
	for i < len(b) {
		matched := false
		for _, secret := range eligible {
			if bytes.HasPrefix(b[i:], secret) {
				out += copy(b[out:], Placeholder)
				i += len(secret)
				replacements++
				matched = true
				break
			}
		}
		if !matched {
			b[out] = b[i]
			out++
			i++
		}
	}
	// Zero the tail so no trace of the secret survives past the new length.
	clear(b[out:])
	if replacements > 0 {
		r.mu.Lock()
		r.scrubbed++
		r.mu.Unlock()
	}
	return out, replacements
}

// InPlaceSkipped counts secrets too short for in-place replacement.
func (r *Redactor) InPlaceSkipped() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.inPlaceSkipped
}

// Headers returns a redacted COPY of h. The input is never modified: the
// transport still needs it, and mutating a header map mid-flight is how subtle
// bugs are born.
func (r *Redactor) Headers(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		if IsSensitiveHeader(k) {
			red := make([]string, len(vs))
			for i := range red {
				red[i] = Placeholder
			}
			out[k] = red
			continue
		}
		cp := make([]string, len(vs))
		for i, v := range vs {
			cp[i] = r.String(v)
		}
		out[k] = cp
	}
	return out
}

// ScrubAll scrubs a set of strings, returning a new slice. Used for errors,
// command lines, and any other multi-value diagnostic surface.
func (r *Redactor) ScrubAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = r.String(s)
	}
	return out
}

// Error scrubs an error's message, preserving the ability to unwrap. It returns
// the original error when nothing needed replacing, so errors.Is still works
// against sentinel values.
func (r *Redactor) Error(err error) error {
	if err == nil {
		return nil
	}
	msg := r.String(err.Error())
	if msg == err.Error() {
		return err
	}
	return &scrubbedError{msg: msg, cause: err}
}

// scrubbedError is an error whose message has been redacted.
type scrubbedError struct {
	msg   string
	cause error
}

func (e *scrubbedError) Error() string { return e.msg }

// Unwrap keeps the chain so errors.Is/As still reach the original sentinel.
func (e *scrubbedError) Unwrap() error { return e.cause }
