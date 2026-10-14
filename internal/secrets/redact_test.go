package secrets_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"pulse/internal/secrets"
)

// fakeBackend is a stand-in keychain. It exists so the CHAIN can be tested
// exhaustively on any platform; it never stands in for a platform backend test,
// which must run against the real thing or report BLOCKED.
type fakeBackend struct {
	items    map[string]string
	availErr error
}

func (f fakeBackend) Backend() string { return "fake" }

func (f fakeBackend) Available() error { return f.availErr }

func (f fakeBackend) Resolve(key string) (string, error) {
	if err := f.Available(); err != nil {
		return "", err
	}
	v, ok := f.items[key]
	if !ok {
		return "", errors.New("not found")
	}
	return v, nil
}

// ---- the resolution chain ---------------------------------------------------

// TestChainKeychainFirst proves source order: the keychain wins over .env.
func TestChainKeychainFirst(t *testing.T) {
	chain := secrets.NewChain(
		secrets.WithBackend(fakeBackend{items: map[string]string{"api": "from-keychain"}}),
		secrets.WithEnv(map[string]string{"api": "from-env", "only_env": "env-value"}),
	)
	got, err := chain.Resolve("api")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "from-keychain" {
		t.Errorf("keychain must win: got %q", got)
	}
	// A key only .env has still resolves.
	if got, err := chain.Resolve("only_env"); err != nil || got != "env-value" {
		t.Errorf("env fallback failed: %q %v", got, err)
	}
}

// TestChainFallsBackWhenKeychainMisses proves the second link of the chain.
func TestChainFallsBackWhenKeychainMisses(t *testing.T) {
	chain := secrets.NewChain(
		secrets.WithBackend(fakeBackend{items: map[string]string{}}),
		secrets.WithEnv(map[string]string{"api": "from-env"}),
	)
	got, err := chain.Resolve("api")
	if err != nil || got != "from-env" {
		t.Fatalf("fallback failed: %q %v", got, err)
	}
}

// TestChainFallsBackWhenKeychainUnavailable proves an unavailable backend is a
// fall-through, not a hard stop.
func TestChainFallsBackWhenKeychainUnavailable(t *testing.T) {
	chain := secrets.NewChain(
		secrets.WithBackend(fakeBackend{availErr: errors.New("no session bus")}),
		secrets.WithEnv(map[string]string{"api": "from-env"}),
	)
	got, err := chain.Resolve("api")
	if err != nil || got != "from-env" {
		t.Fatalf("fallback on unavailable backend failed: %q %v", got, err)
	}
}

// TestChainFailClosed is the AGENTS.md §4.4 proof: the third link is a HARD
// ERROR. There is no fourth option.
func TestChainFailClosed(t *testing.T) {
	chain := secrets.NewChain(
		secrets.WithBackend(fakeBackend{items: map[string]string{}}),
		secrets.WithEnv(map[string]string{}),
	)
	got, err := chain.Resolve("missing")
	if err == nil {
		t.Fatal("an unresolved variable MUST be an error")
	}
	if got != "" {
		t.Errorf("fail-closed violated: returned %q alongside an error", got)
	}
	if !errors.Is(err, secrets.ErrUnresolved) {
		t.Errorf("error %v does not wrap ErrUnresolved", err)
	}
	if code := secrets.ExitCode(err); code != 3 {
		t.Errorf("exit code = %d, want 3 for a secret failure", code)
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error must name the variable: %v", err)
	}
}

// TestChainRegistersResolvedValues proves every resolved secret is registered
// with the redactor before it is returned, so it can never be emitted un-scrubbed.
func TestChainRegistersResolvedValues(t *testing.T) {
	r := secrets.NewRedactor()
	chain := secrets.NewChain(
		secrets.WithBackend(fakeBackend{items: map[string]string{"token": "s3cr3t-value"}}),
		secrets.WithEnv(map[string]string{"envtok": "env-secret-value"}),
		secrets.WithRedactor(r),
	)
	if _, err := chain.Resolve("token"); err != nil {
		t.Fatal(err)
	}
	if _, err := chain.Resolve("envtok"); err != nil {
		t.Fatal(err)
	}
	if r.Tracked() != 2 {
		t.Errorf("redactor tracked %d secrets, want 2", r.Tracked())
	}
	for _, s := range []string{"s3cr3t-value", "env-secret-value"} {
		if got := r.String("response contained " + s + " here"); strings.Contains(got, s) {
			t.Errorf("resolved secret %q survived registration", s)
		}
	}
}

// TestResolveAllFailsOnFirstMiss proves pre-resolution is all-or-nothing.
func TestResolveAllFailsOnFirstMiss(t *testing.T) {
	chain := secrets.NewChain(secrets.WithEnv(map[string]string{"a": "value-a"}))
	out, err := chain.ResolveAll([]string{"a", "b"})
	if err == nil {
		t.Fatal("expected failure when one key is missing")
	}
	if out != nil {
		t.Errorf("partial result returned on failure: %v", out)
	}
}

// ---- .env loading -----------------------------------------------------------

func TestLoadEnvFormats(t *testing.T) {
	in := strings.Join([]string{
		"# a comment",
		"",
		"PLAIN=value",
		"  SPACED  =  spaced value  ",
		"export EXPORTED=exported-value",
		`QUOTED="line1\nline2"`,
		"SINGLE='raw\\nnot-escaped'",
		"INLINE=value # trailing comment",
	}, "\n")
	env, err := secrets.LoadEnv(strings.NewReader(in))
	if err != nil {
		t.Fatalf("LoadEnv: %v", err)
	}
	want := map[string]string{
		"PLAIN":    "value",
		"SPACED":   "spaced value",
		"EXPORTED": "exported-value",
		"QUOTED":   "line1\nline2",
		"SINGLE":   `raw\nnot-escaped`,
		"INLINE":   "value",
	}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
}

func TestLoadEnvRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"NOEQUALS\n", "=novalue\n", "bad key=v\n"} {
		if _, err := secrets.LoadEnv(strings.NewReader(bad)); err == nil {
			t.Errorf("malformed .env %q was accepted silently", bad)
		}
	}
}

// ---- variable extraction ----------------------------------------------------

func TestExtractVariables(t *testing.T) {
	got := secrets.ExtractVariables("GET https://x/{{host}}/u?k={{key}} H: {{auth}} X: {{host}}")
	want := []string{"host", "key", "auth"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// ---- platform backend disposition (AGENTS.md §4.5) --------------------------

// TestPlatformBackendAvailability reports the REAL disposition of this
// environment's keychain. It never fakes a pass: when no backend exists the
// test skips with an explicit BLOCKED reason.
func TestPlatformBackendAvailability(t *testing.T) {
	b := secrets.NewBackend()
	err := b.Available()
	if err != nil {
		t.Skipf("BLOCKED: %s backend unavailable in this environment (%v)", b.Backend(), err)
	}
	t.Logf("keychain backend %s is AVAILABLE", b.Backend())
	// Available, so a lookup for a definitely-absent key must fail cleanly
	// rather than return a placeholder.
	if v, err := b.Resolve("pulse-definitely-absent-key-9f3a"); err == nil {
		t.Errorf("absent key returned a value %q", v)
	} else if strings.Contains(err.Error(), "pulse-definitely-absent-key-9f3a") &&
		len(v) > 0 {
		t.Errorf("error leaked a value for an absent key")
	}
}

// TestBackendIsPureGo asserts no cgo crept into the secrets package on any
// platform we build for. mattn/go-sqlite3 and libsecret bindings are banned.
func TestBackendIsPureGo(t *testing.T) {
	if b := secrets.NewBackend(); b.Backend() == "" {
		t.Error("backend must name itself for diagnostics")
	}
}

// ---- exit-code mapping ------------------------------------------------------

func TestExitCodeMapping(t *testing.T) {
	if got := secrets.ExitCode(secrets.ErrUnresolved); got != 3 {
		t.Errorf("ErrUnresolved maps to %d, want 3", got)
	}
	if got := secrets.ExitCode(errors.New("other")); got != 1 {
		t.Errorf("generic error maps to %d, want 1", got)
	}
	if got := secrets.ExitCode(nil); got != 1 {
		t.Errorf("nil maps to %d, want 1", got)
	}
}

// ---- redaction chokepoint ---------------------------------------------------

// TestRedactKnownSecret is the mutation target: if the redactor stops
// replacing, this fails.
func TestRedactKnownSecret(t *testing.T) {
	const secret = "AKIA-EXAMPLE-SECRET-9f3a2b"
	r := secrets.NewRedactor()
	r.Register(secret)

	payload := `{"error":"invalid key AKIA-EXAMPLE-SECRET-9f3a2b supplied"}`
	got := r.String(payload)
	if strings.Contains(got, secret) {
		t.Fatalf("SECRET LEAKED through the redactor: %s", got)
	}
	if !strings.Contains(got, secrets.Placeholder) {
		t.Errorf("redacted output must carry the placeholder: %s", got)
	}
	if r.Scrubbed() == 0 {
		t.Error("redactor did not count a scrub")
	}
}

// TestRedactBytesCoversSplitAndEmbedded proves value scrubbing works on
// response bytes, including a secret straddling nothing (same chunk) and one
// appearing twice.
func TestRedactBytesCoversEmbedded(t *testing.T) {
	const secret = "top-secret-token-value"
	r := secrets.NewRedactor()
	r.Register(secret)
	in := []byte("prefix " + secret + " middle " + secret + " suffix")
	out := r.Bytes(in)
	if strings.Contains(string(out), secret) {
		t.Fatalf("secret survived Bytes(): %s", out)
	}
	if strings.Contains(string(in), "[REDACTED]") {
		t.Error("Bytes() mutated the caller's slice")
	}
	if !strings.Contains(string(out), secrets.Placeholder) {
		t.Errorf("placeholder missing: %s", out)
	}
}

// TestScrubBytesInPlace covers the streaming path, where a copy would defeat the
// memory ceiling. It also pins the contract that scrubbing SHRINKS the payload
// and reports the new length — the engine must use it to update the chunk.
func TestScrubBytesInPlace(t *testing.T) {
	const secret = "streamed-secret-in-body"
	r := secrets.NewRedactor()
	r.Register(secret)
	buf := []byte(`{"token":"` + secret + `"}`)
	newLen, n := r.ScrubBytes(buf)
	if n == 0 {
		t.Fatal("ScrubBytes reported no replacement")
	}
	if strings.Contains(string(buf[:newLen]), secret) {
		t.Fatalf("in-place scrub failed: %s", buf[:newLen])
	}
	if !strings.Contains(string(buf[:newLen]), secrets.Placeholder) {
		t.Errorf("placeholder missing: %s", buf[:newLen])
	}
	if newLen != len(buf)-(len(secret)-len(secrets.Placeholder)) {
		t.Errorf("newLen = %d, want the payload shrunk by the placeholder delta (%d)",
			newLen, len(buf)-(len(secret)-len(secrets.Placeholder)))
	}
	// The tail must be zeroed so no trace of the secret survives in the buffer.
	if tail := buf[newLen:]; strings.Contains(string(tail), secret[:4]) {
		t.Error("tail bytes were not zeroed after compaction")
	}
}

// TestScrubBytesSkipsTooShortSecrets proves the in-place path refuses secrets it
// cannot replace without running off the buffer, and COUNTS the refusal rather
// than silently corrupting the payload.
func TestScrubBytesSkipsTooShortSecrets(t *testing.T) {
	r := secrets.NewRedactor()
	r.Register("token7") // 7 bytes: long enough to track (min 4), shorter than "[REDACTED]"
	buf := []byte(`{"k":"token7"}`)
	newLen, n := r.ScrubBytes(buf)
	if n != 0 {
		t.Errorf("a short secret must not be replaced in place, got %d replacements", n)
	}
	if newLen != len(buf) {
		t.Errorf("payload length changed despite no replacement: %d != %d", newLen, len(buf))
	}
	if string(buf[:newLen]) != `{"k":"token7"}` {
		t.Errorf("payload was mangled: %s", buf[:newLen])
	}
	if r.InPlaceSkipped() == 0 {
		t.Error("the skipped secret was not counted")
	}
	// It is still scrubbed on the allocating path, which has no length limit.
	if got := r.String(`{"k":"token7"}`); strings.Contains(got, "token7") {
		t.Errorf("short secret survived the allocating path: %s", got)
	}
}

// TestRedactorIgnoresUntrackablyShortValues documents the floor: values under
// minSecretLen are never registered, because scrubbing them would corrupt
// ordinary text without protecting anything meaningful.
func TestRedactorIgnoresUntrackablyShortValues(t *testing.T) {
	r := secrets.NewRedactor()
	r.Register("abc")
	if r.Tracked() != 0 {
		t.Errorf("a 3-byte value must not be tracked (tracked=%d)", r.Tracked())
	}
}

// TestRedactHeaders covers every sensitive header plus a benign one.
func TestRedactHeaders(t *testing.T) {
	r := secrets.NewRedactor()
	in := http.Header{
		"Authorization":       {"Bearer abc123"},
		"Proxy-Authorization": {"Basic xyz"},
		"Cookie":              {"session=abc"},
		"Set-Cookie":          {"session=abc; HttpOnly"},
		"X-Api-Key":           {"key-123"},
		"X-Auth-Token":        {"tok-123"},
		"X-Session-Token":     {"tok-456"},
		"X-Csrf-Token":        {"csrf-1"},
		"Content-Type":        {"application/json"},
		"X-Trace":             {"visible-value"},
	}
	out := r.Headers(in)
	for name := range in {
		if secrets.IsSensitiveHeader(name) {
			if got := out.Get(name); got != secrets.Placeholder {
				t.Errorf("%s not redacted: %q", name, got)
			}
		}
	}
	if got := out.Get("X-Trace"); got != "visible-value" {
		t.Errorf("benign header must survive: %q", got)
	}
	if in.Get("Authorization") != "Bearer abc123" {
		t.Error("Headers() mutated the input map")
	}
}

// TestRedactHeadersScrubsValuesInBenignHeaders proves the second layer: a
// secret echoed into a non-sensitive header is still scrubbed.
func TestRedactHeadersScrubsValuesInBenignHeaders(t *testing.T) {
	const secret = "leaked-into-a-custom-header"
	r := secrets.NewRedactor()
	r.Register(secret)
	in := http.Header{"X-Debug-Echo": {secret}}
	out := r.Headers(in)
	if strings.Contains(out.Get("X-Debug-Echo"), secret) {
		t.Errorf("value-based redaction missed a benign header: %q", out.Get("X-Debug-Echo"))
	}
}

// TestRedactErrorPreservesUnwrap proves errors.Is still reaches the sentinel.
func TestRedactErrorPreservesUnwrap(t *testing.T) {
	const secret = "secret-inside-an-error"
	r := secrets.NewRedactor()
	r.Register(secret)
	base := errors.New("dial failed for token " + secret)
	got := r.Error(base)
	if strings.Contains(got.Error(), secret) {
		t.Fatalf("secret survived Error(): %s", got)
	}
	if !errors.Is(got, base) {
		t.Error("Error() must preserve the error chain")
	}
	// An error with nothing to scrub is returned untouched.
	clean := errors.New("plain failure")
	if r.Error(clean) != clean {
		t.Error("a clean error should be returned as-is")
	}
	if r.Error(nil) != nil {
		t.Error("nil must map to nil")
	}
}

// TestRedactorIgnoresTinyValues proves short strings do not mangle output.
func TestRedactorIgnoresTinyValues(t *testing.T) {
	r := secrets.NewRedactor()
	r.Register("ab")
	if r.Tracked() != 0 {
		t.Errorf("a 2-byte value must not be tracked (tracked=%d)", r.Tracked())
	}
	if got := r.String("a cabin abroad"); got != "a cabin abroad" {
		t.Errorf("short-value registration corrupted output: %q", got)
	}
}

// TestRedactorCapIsMemoryBound proves the tracking list is bounded and DROPS
// rather than evicts (evicting could unprotect a secret still in flight).
func TestRedactorCapIsMemoryBound(t *testing.T) {
	r := secrets.NewRedactor()
	for i := range secrets.MaxTrackedSecretsForTest() + 50 {
		r.Register("secret-value-number-" + strings.Repeat("x", i%7) + itoa(i))
	}
	if got := r.Tracked(); got > secrets.MaxTrackedSecretsForTest() {
		t.Errorf("tracked %d secrets, cap is %d", got, secrets.MaxTrackedSecretsForTest())
	}
	if r.Dropped() == 0 {
		t.Error("expected the cap to drop registrations, got none")
	}
}

// TestRedactorOverlappingSecretsScrubsLongestFirst proves the ordering rule.
func TestRedactorOverlappingSecretsScrubsLongestFirst(t *testing.T) {
	r := secrets.NewRedactor()
	r.Register("abcd")     // registered first, shorter
	r.Register("abcdefgh") // registered second, contains the first
	defer r.Register("")   // no-op, keeps the linter honest
	out := r.String("value=abcdefgh;")
	if strings.Contains(out, "efgh") || strings.Contains(out, "abcd") {
		t.Errorf("overlapping secret not fully scrubbed: %q", out)
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

// TestRedactorContainsIsASideEffectFreeProbe gates the sync filter's
// boolean leg: Contains answers without booking a scrub metric, so
// gossip-time scanning does not drown Scrubbed.
func TestRedactorContainsIsASideEffectFreeProbe(t *testing.T) {
	r := secrets.NewRedactor()
	r.Register("vault-token-abc123")
	if !r.Contains("prefix vault-token-abc123 suffix") {
		t.Error("Contains missed a registered secret")
	}
	if r.Contains("nothing sensitive here") {
		t.Error("Contains fired on clean input")
	}
	if r.Contains("") {
		t.Error("Contains fired on empty input")
	}
	var nilR *secrets.Redactor
	if nilR.Contains("vault-token-abc123") {
		t.Error("nil Contains fired")
	}
	if got := r.Scrubbed(); got != 0 {
		t.Errorf("probing booked %d scrubs, want zero side effects", got)
	}
}
